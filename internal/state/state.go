// Package state records what agentsync did: every symlink it created, and every
// agent a person deleted on purpose.
//
// It answers the one question the filesystem cannot — did this tool make this
// link? — so ownership is recorded at the moment of acting and never inferred
// afterwards. Unlike inventory.json it is authoritative, cannot be regenerated,
// and is not committed: records are keyed by the link's absolute path (see
// canonical) into one particular checkout.
//
// The dangerous property: a document that fails to load must never degrade into
// an empty one. See Load.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// Schema is the shape of the document on disk. Bump it by hand when a field is
// added, removed, or changes meaning.
const Schema = 1

// FileName is the document's name, relative to the repo root. Gitignored.
const FileName = ".agentsync-state.json"

// Path is where the document lives for a given checkout.
func Path(repo *scan.Repo) string {
	return filepath.Join(repo.Root, FileName)
}

// Origin is how a link came to be recorded, and it decides what a later prune
// may touch. Never collapse the two arms.
type Origin int

const (
	// OriginUnknown is the zero value and the refusing arm: an unreadable or
	// absent origin must not be the one that authorises deletion.
	OriginUnknown Origin = iota

	// OriginCreated is a symlink this tool wrote. Note the collision with
	// linker.ActionAdopt, which replaces a project file with a link the tool
	// creates — that is created, not adopted.
	OriginCreated

	// OriginAdopted is a healthy link recorded after the fact. It cannot
	// distinguish an older agentsync's link from a handmade one, so it is a
	// weaker claim and anything acting on it needs extra authorisation.
	OriginAdopted
)

func (o Origin) String() string {
	switch o {
	case OriginCreated:
		return "created"
	case OriginAdopted:
		return "adopted"
	default:
		return "unknown"
	}
}

// Valid reports whether this is an origin a record may carry.
func (o Origin) Valid() bool { return o == OriginCreated || o == OriginAdopted }

// MarshalJSON writes the origin as text: the integer is an implementation
// detail, and inserting an arm would silently re-map every record already on
// disk. An unknown origin refuses rather than writing a value Load would reject.
func (o Origin) MarshalJSON() ([]byte, error) {
	if !o.Valid() {
		return nil, fmt.Errorf("state: refusing to write a record with origin %q", o)
	}
	return json.Marshal(o.String())
}

// UnmarshalJSON refuses an unrecognised origin rather than falling back to the
// zero value, which would drop the record on the next save.
func (o *Origin) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("state: origin is not a string: %w", err)
	}
	switch s {
	case "created":
		*o = OriginCreated
	case "adopted":
		*o = OriginAdopted
	default:
		return fmt.Errorf("state: unknown origin %q; this document was written by a different agentsync", s)
	}
	return nil
}

// Key is one agent in one project: the <project>/<name> pair the reports print.
// Project is a scan.ProjectKey, so it contains slashes of its own.
type Key struct {
	Project string `json:"project"`
	Name    string `json:"name"`
}

func (k Key) String() string { return k.Project + "/" + k.Name }

// Valid reports whether both halves are present.
func (k Key) Valid() bool { return k.Project != "" && k.Name != "" }

// ParseKey reads the "<project>/<name>" form a user types, mirroring
// importer.UpdateSet.Add: trailing ".md" dropped, split at the last slash.
//
// Rejecting a value with no interior slash is load-bearing: it is what turns
// `--undecline --apply` into a message instead of a silently swallowed --apply.
func ParseKey(s string) (Key, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Key{}, errors.New("empty selector; expected <project>/<name>")
	}
	s = strings.TrimSuffix(s, ".md")

	i := strings.LastIndex(s, "/")
	if i <= 0 || i == len(s)-1 {
		return Key{}, fmt.Errorf("%q: expected <project>/<name>, the two columns the report prints (for example Web/example/planner)", s)
	}
	return Key{Project: s[:i], Name: s[i+1:]}, nil
}

// Link is the identity of one symlink this tool is responsible for. Path and
// Target are both absolute.
type Link struct {
	Key

	// Path is the symlink itself, never what it resolves to. It is the
	// record's key, being the thing that is unique on disk.
	Path string `json:"path"`

	// Target is the repo file the link points at.
	Target string `json:"target"`

	// Hash is the SHA-256 of the target's content when the record was made.
	// It is the merge base: it is what tells a dereferenced link from one
	// edited after diverging.
	Hash string `json:"hash"`
}

// Record is a Link plus where it came from and when it was written down.
type Record struct {
	Link

	Origin Origin `json:"origin"`

	// RecordedAt is RFC3339 in UTC, held as the string that goes on the wire.
	// Nothing computes with it; records sort by path.
	RecordedAt string `json:"recorded_at"`
}

// Declined marks an agent whose link a person deleted on purpose. link honours
// it and never recreates the link; clearing it is a write like any other.
type Declined struct {
	Key

	// DeclinedAt is RFC3339 in UTC.
	DeclinedAt string `json:"declined_at"`

	// Observation is what was seen — "link gone, nothing in its place" —
	// rather than the conclusion drawn from it. The marker was written by a
	// heuristic, so whoever meets it later has to be able to argue with it.
	Observation string `json:"observation"`
}

// document is the wire shape: sorted arrays, where File keeps maps.
type document struct {
	Schema   int        `json:"schema"`
	Links    []Record   `json:"links"`
	Declined []Declined `json:"declined"`
}

// File is the loaded document. It carries maps, so it is handled by pointer and
// never copied — as scan.Repo is.
type File struct {
	// links is keyed by the link's absolute path.
	links map[string]Record

	// declined is keyed by Key.String(), so link can ask about every agent it
	// considers without a linear scan per question.
	declined map[string]Declined

	// dirty says whether anything changed since loading, so a run that
	// decided nothing writes nothing.
	dirty bool
}

// New returns an empty document: what a checkout that has recorded nothing has.
func New() *File {
	return &File{
		links:    make(map[string]Record),
		declined: make(map[string]Declined),
	}
}

// Load reads the state document for this checkout.
//
// A missing file is not an error — a repo that has never run `link --apply` has
// nothing recorded. Every other failure is an error and must never degrade into
// the empty document, because the empty document is not a neutral answer: it is
// the claim "nothing was created and nothing was declined". The declined half of
// that claim degrades permissively — link would recreate every link the user
// deliberately deleted — and the ownership half makes a prune that refused
// everything look like one that found nothing to do.
func Load(repo *scan.Repo) (*File, error) {
	if repo == nil {
		return nil, errors.New("state: nil repo")
	}
	path := Path(repo)

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return New(), nil
		}
		return nil, fmt.Errorf("reading the state file %q: %w", path, err)
	}
	f, err := parse(b)
	if err != nil {
		return nil, fmt.Errorf("reading the state file %q: %w", path, err)
	}
	return f, nil
}

// parse turns the wire form into a File, refusing anything it cannot vouch for.
//
// Unknown fields refuse rather than being ignored: Save would re-render the
// document without them, silently destroying whatever they held.
func parse(b []byte) (*File, error) {
	// The schema is read on its own first, and deliberately without
	// DisallowUnknownFields: a document written by a newer agentsync carries
	// fields this build has never heard of, so the strict decode below fails
	// on one of them and never reaches the version check. That put the
	// "upgrade rather than letting an older build rewrite it" message out of
	// reach in exactly the case it was written for.
	var head struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	switch {
	case head.Schema == Schema:
	case head.Schema > Schema:
		return nil, fmt.Errorf("schema %d was written by a newer agentsync than this one (which understands %d); upgrade rather than letting an older build rewrite it",
			head.Schema, Schema)
	default:
		return nil, fmt.Errorf("schema %d is not one this build understands (it understands %d); refusing to guess at the shape of an authoritative document",
			head.Schema, Schema)
	}

	var doc document
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}

	f := New()
	for _, r := range doc.Links {
		// Validated before cleaning, not after: validate refuses a relative
		// path, while canonical would resolve one against this process's
		// cwd, which is no evidence about a link written months ago. add
		// takes the opposite view because there the path is live.
		if err := r.validate(); err != nil {
			return nil, err
		}
		r.Path, r.Target = filepath.Clean(r.Path), filepath.Clean(r.Target)
		// Refused rather than de-duplicated: two records for one path can
		// disagree about origin, and picking either silently widens or
		// narrows what a prune may delete.
		if _, dup := f.links[r.Path]; dup {
			return nil, fmt.Errorf("two records for the link %q; this document is corrupt and cannot say which one is true", r.Path)
		}
		f.links[r.Path] = r
	}
	for _, d := range doc.Declined {
		if err := d.validate(); err != nil {
			return nil, err
		}
		if _, dup := f.declined[d.Key.String()]; dup {
			return nil, fmt.Errorf("two declined markers for %s; this document is corrupt", d.Key)
		}
		f.declined[d.Key.String()] = d
	}
	return f, nil
}

func (r Record) validate() error {
	if !r.Key.Valid() {
		return fmt.Errorf("a record for %q names no project and agent", r.Path)
	}
	if !r.Origin.Valid() {
		return fmt.Errorf("the record for %s carries no usable origin", r.Key)
	}
	if !filepath.IsAbs(r.Path) {
		return fmt.Errorf("the record for %s has a relative link path %q; a record is keyed by the path that is unique on disk, and a relative one is not", r.Key, r.Path)
	}
	if !filepath.IsAbs(r.Target) {
		return fmt.Errorf("the record for %s has a relative target %q", r.Key, r.Target)
	}
	if r.Hash == "" {
		return fmt.Errorf("the record for %s carries no hash; without it a later run cannot tell a dereferenced link from an edited one", r.Key)
	}
	if err := validTime(r.RecordedAt); err != nil {
		return fmt.Errorf("the record for %s: recorded_at %w", r.Key, err)
	}
	return nil
}

func (d Declined) validate() error {
	if !d.Key.Valid() {
		return errors.New("a declined marker names no project and agent")
	}
	if err := validTime(d.DeclinedAt); err != nil {
		return fmt.Errorf("the declined marker for %s: declined_at %w", d.Key, err)
	}
	if d.Observation == "" {
		return fmt.Errorf("the declined marker for %s carries no observation; a marker nobody can argue with is one nobody can safely clear", d.Key)
	}
	return nil
}

// validTime insists the field really is RFC3339, on the way in as well as out: a
// timestamp that cannot be parsed is not evidence.
func validTime(s string) error {
	if s == "" {
		return errors.New("is missing")
	}
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return fmt.Errorf("%q is not RFC3339: %w", s, err)
	}
	return nil
}

// Dirty reports whether anything changed since this document was loaded.
func (f *File) Dirty() bool { return f.dirty }

// LookupLink answers "is this link ours, and what did we record about it?" in
// one call: the origin decides whether it may be touched and the hash decides
// what the touch should be. False for a link this tool has no record of.
func (f *File) LookupLink(path string) (Record, bool) {
	key, err := canonical(path)
	if err != nil {
		// Reported as "not ours", the refusing arm: it can only make a
		// caller decline to act.
		return Record{}, false
	}
	r, ok := f.links[key]
	return r, ok
}

// Origin reports how this link came to be recorded, and OriginUnknown for a link
// with no record. The two collapse deliberately: to a caller deciding whether it
// may remove something, both mean no.
func (f *File) Origin(path string) Origin {
	r, ok := f.LookupLink(path)
	if !ok {
		return OriginUnknown
	}
	return r.Origin
}

// Hash returns the SHA-256 recorded for this link's target, or "" if there is no
// record.
func (f *File) Hash(path string) string {
	r, ok := f.LookupLink(path)
	if !ok {
		return ""
	}
	return r.Hash
}

// Records returns a copy of every record, sorted by link path.
func (f *File) Records() []Record {
	out := make([]Record, 0, len(f.links))
	for _, r := range f.links {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// LookupDeclined returns the marker for this agent, if there is one.
func (f *File) LookupDeclined(k Key) (Declined, bool) {
	d, ok := f.declined[k.String()]
	return d, ok
}

// IsDeclined reports whether this agent has been declined, in one map lookup.
func (f *File) IsDeclined(k Key) bool {
	_, ok := f.declined[k.String()]
	return ok
}

// AllDeclined returns a copy of every marker, sorted by project then name.
func (f *File) AllDeclined() []Declined {
	out := make([]Declined, 0, len(f.declined))
	for _, d := range f.declined {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// RecordCreated records a symlink this tool just made, and returns the stored
// record.
//
// The two origins are separate functions rather than one taking an Origin, so no
// call site can pass the wrong one by miscounting arguments.
func (f *File) RecordCreated(l Link, at time.Time) (Record, error) {
	return f.add(l, OriginCreated, at)
}

// RecordAdopted records a healthy link that was already there. Nothing in link
// calls it; it is for the adopt pass, and it lives beside its counterpart so the
// two origins are not conflated later.
func (f *File) RecordAdopted(l Link, at time.Time) (Record, error) {
	return f.add(l, OriginAdopted, at)
}

func (f *File) add(l Link, origin Origin, at time.Time) (Record, error) {
	if at.IsZero() {
		return Record{}, fmt.Errorf("recording %s: no timestamp", l.Key)
	}
	path, err := canonical(l.Path)
	if err != nil {
		return Record{}, fmt.Errorf("recording %s: %w", l.Key, err)
	}
	target, err := canonical(l.Target)
	if err != nil {
		return Record{}, fmt.Errorf("recording %s: %w", l.Key, err)
	}
	l.Path, l.Target = path, target

	r := Record{
		Link:   l,
		Origin: origin,
		// UTC, so records made either side of a clock change stay
		// comparable by eye.
		RecordedAt: at.UTC().Format(time.RFC3339),
	}
	if err := r.validate(); err != nil {
		return Record{}, err
	}

	// Last write wins: a link recorded twice is the same link re-made, and
	// the newer record carries the newer target hash. Unlike parse's
	// duplicate, this one knows which came second.
	f.links[r.Path] = r
	f.dirty = true
	return r, nil
}

// Remove drops the record for a link, reporting whether there was one. It
// removes nothing on disk, and forgetting a link that still exists makes it
// unowned forever — so callers remove the link first.
func (f *File) Remove(path string) bool {
	key, err := canonical(path)
	if err != nil {
		return false
	}
	if _, ok := f.links[key]; !ok {
		return false
	}
	delete(f.links, key)
	f.dirty = true
	return true
}

// Decline records that this agent's link was deleted on purpose.
//
// Nothing in this build calls it: writing a marker means concluding intent from
// an absence, which is reconcile's judgement, not link's. link only honours what
// it finds.
func (f *File) Decline(k Key, observation string, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("declining %s: no timestamp", k)
	}
	d := Declined{Key: k, DeclinedAt: at.UTC().Format(time.RFC3339), Observation: observation}
	if err := d.validate(); err != nil {
		return err
	}
	f.declined[k.String()] = d
	f.dirty = true
	return nil
}

// Undecline clears a marker, reporting whether there was one to clear. False is
// not a failure: a marker that is not there is the state the caller asked for,
// so an identical second run says "no change" rather than "error".
func (f *File) Undecline(k Key) bool {
	if _, ok := f.declined[k.String()]; !ok {
		return false
	}
	delete(f.declined, k.String())
	f.dirty = true
	return true
}

// canonical is how a path becomes a record key: absolute, cleaned, and with the
// parent directory's symlinks resolved.
//
// The parent, never the final component: resolving that would key the record by
// the repo file the link points at. Resolving the parent gives one link one key
// however the caller spelled it — /var/... and /private/var/... on macOS name
// the same directory, and without this a link records under one spelling and
// looks unowned under the other. A parent that cannot be resolved falls back to
// the cleaned absolute path, which only arises once the link is gone anyway.
func canonical(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", path, err)
	}
	abs = filepath.Clean(abs)
	dir, base := filepath.Split(abs)
	resolved, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return abs, nil
	}
	return filepath.Join(resolved, base), nil
}

// Marshal renders the document, sorted so a diff between two runs is readable.
// Byte-identity is not a contract here — every record carries a timestamp — so
// this file must never grow an inventory-style --check.
func (f *File) Marshal() ([]byte, error) {
	doc := document{
		Schema: Schema,
		// Non-nil, so the JSON carries empty arrays rather than nulls.
		Links:    f.Records(),
		Declined: f.AllDeclined(),
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Off, or agent names and paths containing &, < or > come back escaped
	// for a browser that is never going to read this.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("rendering the state document: %w", err)
	}
	return buf.Bytes(), nil
}

// Save replaces the document through a temporary in the repo root, then renames:
// a reader sees the whole old document or the whole new one, and a crash leaves
// a stray temporary rather than half a document that still parses.
//
// Mode 0600, where inventory.json is 0644. That file is a committed, shared
// artifact; this one is machine-private and authoritative — anything that can
// rewrite it can make the tool believe it created a link it did not. Set
// explicitly rather than inherited from CreateTemp's default.
//
// It always writes: an explicit Save is an explicit instruction. Callers wanting
// "only if something changed" ask Dirty first.
func (f *File) Save(repo *scan.Repo) (err error) {
	if repo == nil {
		return errors.New("state: nil repo")
	}
	content, err := f.Marshal()
	if err != nil {
		return err
	}
	dest := Path(repo)
	dir := filepath.Dir(dest)

	// No MkdirAll: the destination is the repo root, which scan.NewRepo
	// resolved and which therefore exists.
	tmp, err := os.CreateTemp(dir, ".agentsync-state-*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %q: %w", dir, err)
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()

	if _, err = tmp.Write(content); err != nil {
		return fmt.Errorf("writing %q: %w", name, err)
	}
	// Synced before the rename, or the rename can reach disk while the bytes
	// it points at have not — which is how a crash produces an empty state
	// file, the one shape that reads as "this tool created nothing".
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %q: %w", name, err)
	}
	if err = tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("setting mode on %q: %w", name, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", name, err)
	}
	if err = os.Rename(name, dest); err != nil {
		return fmt.Errorf("renaming %q to %q: %w", name, dest, err)
	}
	f.dirty = false
	return nil
}
