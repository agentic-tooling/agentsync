// Package reconcile acts on the drift doctor diagnoses, using the record
// internal/state keeps of what this tool created.
//
// doctor removes nothing because it cannot tell a link this tool made from one
// a person made by hand; the state document answers exactly that, and every
// removal here is gated on it. Planning and applying are separate, as in linker
// and importer: NewPlan reads and judges, only Plan.Apply writes.
package reconcile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/doctor"
	"github.com/agentic-tooling/agentsync/internal/linker"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
)

// Flag names this package's diagnostics refer to, kept here for the reason
// scan.AllowOutsideHomeFlag is: a message naming a flag that does not exist is
// worse than no message.
const (
	AdoptFlag        = "adopt"
	PruneFlag        = "prune"
	PruneAdoptedFlag = "prune-adopted"
	RekeyFlag        = "rekey"
	DropCaptureFlag  = "drop-capture"
)

// Op is what a reconcile run would do, or did, with one link or one capture.
//
// The report-only arms are as load-bearing as the writing ones: reconcile's
// whole claim is that it removes only what it can prove it owns, and what it
// declined to touch is the part a user cannot reconstruct from the filesystem.
type Op int

const (
	// OpInvalid is the zero value and never appears in a plan.
	OpInvalid Op = iota

	// OpUnrecorded is a healthy, correctly aimed link this tool has no
	// record of. --adopt records it; nothing removes it.
	OpUnrecorded

	// OpAdopt is OpUnrecorded with --adopt given: the link is recorded with
	// origin adopted, which is a weaker claim than created — see
	// findUnrecorded.
	OpAdopt

	// OpPrunable is a recorded link that is broken and still points where
	// the record says. --prune removes it, and an adopted record needs
	// --prune-adopted as well.
	OpPrunable

	// OpPrune is OpPrunable with the authorising flags given.
	OpPrune

	// OpDecline is a record whose path now holds nothing at all: the link
	// was deleted and nothing took its place. Apply writes a declined marker
	// so link never recreates it, and drops the record.
	OpDecline

	// OpDereferenced is a record whose link was replaced by a real file
	// holding the content the record hashed. Nothing was lost; re-linking is
	// safe. Reported, never done here.
	OpDereferenced

	// OpConflict is the same shape with a different hash: the file was
	// edited after it stopped being a link. Never resolved automatically.
	OpConflict

	// OpDiverged is a record whose path still holds a symlink, but not the
	// one recorded — it points elsewhere in the repo, or outside it
	// entirely. Never pruned: the record does not describe what is there.
	OpDiverged

	// OpUnchanged is a recorded link still present and still aimed where the
	// record says. Nothing to do — this is what makes a second run a no-op.
	OpUnchanged

	// OpRekeyCandidate is a proposal, never an action: an orphaned capture
	// directory beside a live project of the same name that has no captures
	// of its own. Applying it needs --rekey OLD=NEW naming both halves.
	OpRekeyCandidate

	// OpDropCapture is one capture named by --drop-capture whose current
	// bytes are in this repo's HEAD, so deleting the working copy destroys
	// nothing. Anything short of that is a Refused, not an item.
	OpDropCapture

	// OpProjectGone is a record whose agents directory is no longer at that
	// path. An ordinary `mv` of a project produces it, and it is evidence
	// about the project rather than about any agent in it — so nothing here
	// is declined and nothing is removed. See judgeAbsent.
	//
	// Appended rather than filed beside OpDecline: Op marshals as a number,
	// so inserting an arm re-maps every op in a report somebody has saved.
	OpProjectGone
)

func (o Op) String() string {
	switch o {
	case OpUnrecorded:
		return "unrecorded"
	case OpAdopt:
		return "adopt"
	case OpPrunable:
		return "prunable"
	case OpPrune:
		return "prune"
	case OpDecline:
		return "decline"
	case OpDereferenced:
		return "dereferenced"
	case OpConflict:
		return "conflict"
	case OpDiverged:
		return "diverged"
	case OpUnchanged:
		return "unchanged"
	case OpRekeyCandidate:
		return "rekey-candidate"
	case OpDropCapture:
		return "drop-capture"
	case OpProjectGone:
		return "project-gone"
	default:
		return "invalid"
	}
}

// Writes reports whether Apply acts on this op.
func (o Op) Writes() bool {
	return o == OpAdopt || o == OpPrune || o == OpDecline || o == OpDropCapture
}

// Unresolved reports whether this op means the run found something only a
// human can settle, which is what exit 1 asserts.
//
// Three are deliberately not. OpUnrecorded: this machine has dozens of links
// made before the state file existed, and failing forever on them trains a
// reader to ignore the code. OpRekeyCandidate: a proposal about an orphaned
// capture directory doctor already fails on, and reconcile must not charge for
// one finding twice. OpProjectGone: reconcile offers no operation for it and
// nothing here clears a record, so exit 1 would be permanent and unearned — the
// capture-side twin of that same fact is doctor's orphaned-captures, which does
// fail, so the signal lives in the command that owns it.
func (o Op) Unresolved() bool {
	return o == OpPrunable || o == OpDereferenced || o == OpConflict || o == OpDiverged
}

// Item is one link or one capture and what the run decided about it.
type Item struct {
	Op Op `json:"op"`

	// OpName is Op in printable form. Op marshals as a number, matching
	// scan.State and linker.Action; the name is here so a jq pipeline can
	// read a report without a copy of this enum.
	OpName string `json:"op_name"`

	Project string `json:"project"`

	// Name is the agent name, empty for the capture-directory ops.
	Name string `json:"name,omitempty"`

	// Path is the thing acted on: the symlink, the capture file, or the
	// capture directory.
	Path string `json:"path"`

	// Target is the other end: the link's target, or the directory a re-key
	// would move captures to.
	Target string `json:"target,omitempty"`

	// Origin is what the state document says about this link, and
	// OriginUnknown for anything it has no record of. It is what separates a
	// prune from one needing --prune-adopted.
	Origin     state.Origin `json:"origin"`
	OriginName string       `json:"origin_name"`

	// Hash is the content this op was authorised against: the target's hash
	// for an adoption, the capture's own for a drop. Apply re-checks it, so
	// a file that changed between the phases is refused rather than acted on.
	Hash string `json:"hash,omitempty"`

	// Reason explains the op in one clause, and is what the text report
	// prints.
	Reason string `json:"reason,omitempty"`
}

func (i Item) String() string {
	if i.Name == "" {
		return fmt.Sprintf("%s %s: %s", i.Op, i.Project, i.Reason)
	}
	return fmt.Sprintf("%s %s/%s: %s", i.Op, i.Project, i.Name, i.Reason)
}

func (i *Item) set(o Op, reason string) {
	i.Op = o
	i.OpName = o.String()
	i.OriginName = i.Origin.String()
	i.Reason = reason
}

// Guard names the gate that refused an explicit selector.
//
// A string rather than an integer enum, following importer.Refusal: a report, a
// test, or a jq pipeline branches on which gate fired without matching on
// English, and the reasons below are written to be read and will be reworded.
type Guard string

const (
	// GuardNoCapture means the named capture is not there at all.
	GuardNoCapture Guard = "no-such-capture"

	// GuardNotRegular means the path exists but is a directory or a link, so
	// it is not the captured file it was named as.
	GuardNotRegular Guard = "not-a-regular-file"

	// GuardUncommitted means the capture's current bytes are in no commit in
	// this repo, so deleting the working copy destroys them.
	GuardUncommitted Guard = "uncommitted-capture"

	// GuardCommitUnknown means git could not be asked. Distinct from
	// GuardUncommitted because only one of them is fixable by committing.
	GuardCommitUnknown Guard = "commit-unknown"

	// GuardCaptureInUse means a live symlink points at the capture, so
	// removing it would manufacture the broken link this tool refuses to
	// create.
	GuardCaptureInUse Guard = "capture-in-use"

	// GuardNoSource means the re-key's source directory holds no captures to
	// move.
	GuardNoSource Guard = "no-captures-to-move"

	// GuardNestedCaptures means the source directory contains another
	// project's captures below it, which the caller did not name.
	GuardNestedCaptures Guard = "nested-captures"

	// GuardOccupied means the destination key already holds captures.
	// Merging two projects' captures is the failure this gate exists for.
	GuardOccupied Guard = "destination-occupied"

	// GuardOverlapping means one of the two keys is inside the other, so the
	// move has no meaning.
	GuardOverlapping Guard = "overlapping-keys"

	// GuardUnreadable means the gate could not be evaluated. It refuses like
	// every other, because a gate that could not run has not passed.
	GuardUnreadable Guard = "unreadable"
)

// Refused is an explicit selector this run would not act on.
//
// It is kept apart from Plan.Problems for the reason linker.Unresolved is: a
// problem is the tool failing to read something, while this is the tool
// declining to do something it was told to. Both are visible, and only the
// first is exit 2.
type Refused struct {
	Op       Op     `json:"op"`
	OpName   string `json:"op_name"`
	Selector string `json:"selector"`
	Path     string `json:"path,omitempty"`
	Guard    Guard  `json:"guard"`
	Reason   string `json:"reason"`
}

func (r Refused) String() string {
	return fmt.Sprintf("%s (%s): %s", r.Selector, r.Guard, r.Reason)
}

// Repoint is one symlink a re-key would rewrite, because the file it points at
// is moving.
type Repoint struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// String names the link itself first. It is a file in somebody else's project
// that this command is about to rewrite, so a line that showed only the two
// targets would hide the thing being changed.
func (r Repoint) String() string {
	return fmt.Sprintf("%s/%s %s: %s -> %s", r.Project, r.Name, r.Path, r.From, r.To)
}

// Rekey is one authorised --rekey OLD=NEW: a capture directory to move, and
// every link that has to follow it.
//
// It is its own section rather than an Item because it is not one file. A
// directory move plus N link rewrites plus N record rewrites has to be
// reported as the compound thing it is.
type Rekey struct {
	Old string `json:"old"`
	New string `json:"new"`

	OldDir string `json:"old_dir"`
	NewDir string `json:"new_dir"`

	// Captures is how many .md files the move carries.
	Captures int `json:"captures"`

	// Repoint is every link that points into OldDir and must be rewritten,
	// along with the state record behind it.
	Repoint []Repoint `json:"repoint"`
}

func (r Rekey) String() string {
	return fmt.Sprintf("%s -> %s (%d capture%s, %d link%s)",
		r.Old, r.New, r.Captures, plural(r.Captures), len(r.Repoint), plural(len(r.Repoint)))
}

// Examined is what the plan was drawn from, reported because "nothing to do"
// and "nothing was looked at" produce the same empty list.
type Examined struct {
	// Records is how many links the state document claims.
	Records int `json:"records"`

	// Declined is how many markers it already holds.
	Declined int `json:"declined"`

	// Entries is how many project agent files the scan classified.
	Entries int `json:"entries"`
}

// Options is what this run was authorised to do. The zero value writes nothing
// but a declined marker, which is the only op no flag guards — see OpDecline.
type Options struct {
	// Adopt records currently-healthy links this tool has no record of.
	Adopt bool

	// Prune removes recorded links that are broken and still aimed where
	// the record says.
	Prune bool

	// PruneAdopted widens Prune to records made by an adopt pass. It is
	// separate because an adopted record cannot distinguish a link an older
	// agentsync made from one a person made by hand.
	PruneAdopted bool

	// Rekeys are the OLD=NEW moves named on the command line. Nothing is
	// ever inferred into this list.
	Rekeys []KeyMove

	// Drops are the captures named by --drop-capture, one at a time. There
	// is deliberately no blanket form.
	Drops []state.Key
}

// KeyMove is one --rekey OLD=NEW.
type KeyMove struct {
	Old string `json:"old"`
	New string `json:"new"`
}

func (k KeyMove) String() string { return k.Old + "=" + k.New }

// ParseKeyMove reads the OLD=NEW form --rekey takes.
func ParseKeyMove(s string) (KeyMove, error) {
	old, want, ok := strings.Cut(strings.TrimSpace(s), "=")
	if !ok {
		return KeyMove{}, fmt.Errorf("%q: expected OLD=NEW, two project keys (for example Web/old-name=Web/new-name)", s)
	}
	m := KeyMove{Old: strings.TrimRight(strings.TrimSpace(old), "/"), New: strings.TrimRight(strings.TrimSpace(want), "/")}
	if err := validKey(m.Old); err != nil {
		return KeyMove{}, fmt.Errorf("--%s: old key %w", RekeyFlag, err)
	}
	if err := validKey(m.New); err != nil {
		return KeyMove{}, fmt.Errorf("--%s: new key %w", RekeyFlag, err)
	}
	if m.Old == m.New {
		return KeyMove{}, fmt.Errorf("--%s %s: the two keys are the same", RekeyFlag, s)
	}
	return m, nil
}

// validKey refuses a project key that could name a path outside
// project-agents/.
//
// Load-bearing rather than tidy: both --rekey and --drop-capture take a key
// from the command line and hand it to BackupPath, which joins it onto the repo
// root — so a key carrying ".." names a directory this tool would then move or
// delete.
func validKey(key string) error {
	if key == "" {
		return errors.New("is empty")
	}
	if path.IsAbs(key) || filepath.IsAbs(key) {
		return fmt.Errorf("%q is absolute; a project key is relative, as the report prints it", key)
	}
	if path.Clean(key) != key {
		return fmt.Errorf("%q is not in the form the report prints (%q)", key, path.Clean(key))
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%q has a %q component; a project key names a directory under project-agents/ and cannot point outside it", key, part)
		}
	}
	return nil
}

// validName requires a bare agent name, for the reason linker requires one of
// --agent: the name is joined onto a capture directory to make a path this
// command deletes.
func validName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("%q is not an agent name", name)
	case strings.ContainsRune(name, '/'), strings.ContainsRune(name, filepath.Separator):
		return fmt.Errorf("%q is not a bare agent name", name)
	case name != filepath.Base(name):
		return fmt.Errorf("%q is not a bare agent name", name)
	}
	return nil
}

// Validate rejects option combinations that would do nothing, or something
// surprising, before anything is planned.
func (o Options) Validate() error {
	if o.PruneAdopted && !o.Prune {
		return fmt.Errorf("--%s widens --%s and authorises nothing on its own; pass both", PruneAdoptedFlag, PruneFlag)
	}
	seen := make(map[string]bool, len(o.Rekeys))
	for _, m := range o.Rekeys {
		// Two moves naming one source would have the second act on a
		// directory the first already moved.
		if seen[m.Old] {
			return fmt.Errorf("--%s names %s as the source twice", RekeyFlag, m.Old)
		}
		seen[m.Old] = true
	}
	for _, k := range o.Drops {
		if err := validKey(k.Project); err != nil {
			return fmt.Errorf("--%s: project %w", DropCaptureFlag, err)
		}
		if err := validName(k.Name); err != nil {
			return fmt.Errorf("--%s: agent %w", DropCaptureFlag, err)
		}
	}
	return nil
}

// Plan is what a reconcile run would do, before it does any of it.
//
// Every slice is non-nil after NewPlan, so the JSON form carries empty arrays
// rather than nulls.
type Plan struct {
	// Items is every link and capture considered, the untouched ones
	// included.
	Items []Item `json:"items"`

	// Rekeys are the authorised OLD=NEW moves, in the order Apply performs
	// them.
	Rekeys []Rekey `json:"rekeys"`

	// Refused are the explicit selectors a gate turned down.
	Refused []Refused `json:"refused"`

	// Problems are paths this plan could not judge. The item is dropped
	// rather than acted on: not knowing what a file is, is not a licence to
	// remove it.
	Problems []scan.ScanProblem `json:"problems"`

	// Dirs are the agent directories in scope, which is what the home guard
	// must be given.
	Dirs []string `json:"dirs"`

	Examined Examined `json:"examined"`
}

// NewPlan decides what to do with every record in the state document, every
// healthy link the scan found, and every selector the caller named.
//
// It reads, stats, and hashes; it writes nothing. diag is doctor's diagnosis of
// the same tree, and it is a parameter rather than something this function
// computes so that reconcile can only ever act on a finding doctor also
// reports.
//
// st is a parameter for the reason it is one in linker.NewPlan: a nil document
// is an error, never an empty one, because the empty document is the claim
// "this tool created nothing".
func NewPlan(repo *scan.Repo, res scan.Result, diag doctor.Report, st *state.File, opts Options) (Plan, error) {
	if repo == nil {
		return Plan{}, errors.New("no repo")
	}
	if st == nil {
		return Plan{}, errors.New("no state document: reconcile cannot tell this tool's links from handmade ones without one")
	}
	if err := opts.Validate(); err != nil {
		return Plan{}, err
	}

	records := st.Records()
	p := Plan{
		Items:    make([]Item, 0, len(records)+len(res.Entries)),
		Rekeys:   make([]Rekey, 0, len(opts.Rekeys)),
		Refused:  make([]Refused, 0),
		Problems: make([]scan.ScanProblem, 0),
		Dirs:     append(make([]string, 0, len(res.Dirs)), res.Dirs...),
		Examined: Examined{Records: len(records), Declined: len(st.AllDeclined()), Entries: len(res.Entries)},
	}

	p.judgeRecords(repo, records, opts)
	p.findUnrecorded(repo, res, st, opts)
	p.proposeRekeys(repo, res, diag)
	p.planRekeys(repo, res, opts.Rekeys)
	p.planDrops(repo, res, opts.Drops)

	sort.SliceStable(p.Items, func(i, j int) bool {
		a, b := p.Items[i], p.Items[j]
		if a.Op != b.Op {
			return a.Op < b.Op
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Name < b.Name
	})
	return p, nil
}

func (p *Plan) add(i Item) { p.Items = append(p.Items, i) }

func (p *Plan) problem(path string, format string, args ...any) {
	p.Problems = append(p.Problems, scan.ScanProblem{Path: path, Err: fmt.Sprintf(format, args...)})
}

func (p *Plan) refuse(op Op, selector, path string, guard Guard, format string, args ...any) {
	p.Refused = append(p.Refused, Refused{
		Op: op, OpName: op.String(), Selector: selector, Path: path,
		Guard: guard, Reason: fmt.Sprintf(format, args...),
	})
}

// judgeRecords walks what this tool says it created and compares each record
// against what is on disk now. It is where the three-row table from issue #13
// is decided, and where the prune gate is evaluated.
func (p *Plan) judgeRecords(repo *scan.Repo, records []state.Record, opts Options) {
	for _, rec := range records {
		item := Item{
			Project: rec.Project, Name: rec.Name,
			Path: rec.Path, Target: rec.Target, Origin: rec.Origin,
		}

		fi, err := os.Lstat(rec.Path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				p.problem(rec.Path, "could not tell what is at this recorded link: %v", err)
				continue
			}
			p.judgeAbsent(item, rec)
			continue
		}

		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			p.judgeRecordedLink(repo, item, rec, opts)
		case fi.Mode().IsRegular():
			p.judgeRecordedFile(item, rec)
		default:
			p.problem(rec.Path, "recorded as a link, but is neither a link nor a regular file (%v); refusing to judge it", fi.Mode())
		}
	}
}

// judgeAbsent separates row three of the table from a project that is no longer
// at this path at all.
//
// Row three is one link deleted out of a directory that is still there, which
// is evidence about that agent. A missing agents directory is evidence about
// the project — an ordinary `mv` produces it — and reading it as row three
// writes a declined marker for every agent in the project, suppressing agents
// nobody declined. That is the false-positive direction #13 names, and the
// decline is the one write no flag guards.
func (p *Plan) judgeAbsent(item Item, rec state.Record) {
	dir := filepath.Dir(rec.Path)

	// os.Stat, not Lstat. A symlinked .claude/agents is a supported layout —
	// ProjectKey deliberately does not resolve it — so an Lstat-only check
	// would call every project using one "gone". The Lstat rule governs agent
	// *files*; doctor.dirPresent and linker.deriveDir ask this one the same way.
	fi, err := os.Stat(dir)
	switch {
	case err == nil && fi.IsDir():
		item.set(OpDecline, "the link is gone and nothing stands in its place; recording that it was deleted on purpose, so link never recreates it")
	case errors.Is(err, fs.ErrNotExist):
		item.set(OpProjectGone, fmt.Sprintf("%s is not there any more, so this record is evidence about the project rather than about this agent — it moved (a move re-keys it) or it was deleted; nothing is declined and nothing is removed", dir))
	case err != nil:
		p.problem(dir, "could not tell whether this record's agents directory is still there: %v", err)
		return
	default:
		p.problem(dir, "the agents directory a record names is not a directory (%v); refusing to conclude anything about the agents under it", fi.Mode())
		return
	}
	p.add(item)
}

// judgeRecordedFile decides rows one and two: the link is gone and a real file
// stands where it was. The recorded hash is the merge base, and it is the only
// thing that separates a dereferenced link from an edit made after diverging.
func (p *Plan) judgeRecordedFile(item Item, rec state.Record) {
	hash, err := scan.HashFile(rec.Path)
	if err != nil {
		p.problem(rec.Path, "hashing the file that replaced this link: %v", err)
		return
	}
	if hash == rec.Hash {
		item.Hash = hash
		item.set(OpDereferenced, fmt.Sprintf("the link was replaced by a real file holding exactly the content recorded for %s; nothing was lost and it is safe to link again", rec.Target))
		p.add(item)
		return
	}
	item.Hash = hash
	item.set(OpConflict, fmt.Sprintf("the link was replaced by a real file that has since been edited (%s, recorded against %s); capture it with `agentsync import --apply` before anything links over it — reconcile never resolves this",
		short(hash), short(rec.Hash)))
	p.add(item)
}

// judgeRecordedLink decides what to do with a recorded path that still holds a
// symlink. Every verdict about the link itself comes from linker.Judge, so
// reconcile cannot call a link broken that link would call healthy.
func (p *Plan) judgeRecordedLink(repo *scan.Repo, item Item, rec state.Record, opts Options) {
	e, err := repo.Classify(rec.Project, rec.Path)
	if err != nil {
		p.problem(rec.Path, "classifying this recorded link: %v", err)
		return
	}
	judged, prob := linker.Judge(repo, e)
	if prob != nil {
		p.Problems = append(p.Problems, *prob)
		return
	}

	// Checked before the verdict is used: a link pointing somewhere other
	// than the record describes something this run did not create, whatever
	// its health. It is the gate that keeps a prune from removing a link
	// somebody re-pointed by hand.
	if e.LinkTarget != rec.Target {
		item.Target = e.LinkTarget
		item.set(OpDiverged, fmt.Sprintf("points at %s, not the %s this tool recorded; the record does not describe what is here, so nothing is removed", e.LinkTarget, rec.Target))
		p.add(item)
		return
	}

	switch judged.Action {
	case linker.ActionLinked, linker.ActionMismatch:
		item.set(OpUnchanged, "still the link this tool recorded, and its target is still there")
		p.add(item)

	case linker.ActionForeign:
		item.set(OpDiverged, fmt.Sprintf("now points outside this repo (%s); not ours to remove, whatever the record says", e.LinkTarget))
		p.add(item)

	case linker.ActionBroken:
		p.judgeBroken(item, opts)

	default:
		p.problem(rec.Path, "link judged as %v, which reconcile does not know how to act on", judged.Action)
	}
}

// judgeBroken applies the origin half of the prune gate to a link that is
// broken and still aimed where the record says.
func (p *Plan) judgeBroken(item Item, opts Options) {
	switch {
	case !item.Origin.Valid():
		// Unreachable from a document state.parse accepted, which validates
		// every origin. It is a hand-edited file, and the refusing arm.
		item.set(OpDiverged, "the record carries no usable origin, so nothing here authorises removing anything")

	case opts.Prune && (item.Origin == state.OriginCreated || opts.PruneAdopted):
		item.set(OpPrune, fmt.Sprintf("recorded as %s and its target %s is gone; removing the link", item.Origin, item.Target))

	case opts.Prune && item.Origin == state.OriginAdopted:
		item.set(OpPrunable, fmt.Sprintf("recorded by an adopt pass, which cannot tell an older agentsync's link from a handmade one; pass --%s as well to remove it", PruneAdoptedFlag))

	case item.Origin == state.OriginAdopted:
		item.set(OpPrunable, fmt.Sprintf("recorded by an adopt pass and its target %s is gone; pass --%s and --%s to remove it", item.Target, PruneFlag, PruneAdoptedFlag))

	default:
		item.set(OpPrunable, fmt.Sprintf("this tool created this link and its target %s is gone; pass --%s to remove it", item.Target, PruneFlag))
	}
	p.add(item)
}

// findUnrecorded is the adopt pass: every healthy, correctly aimed link the
// state document has never heard of.
//
// It cannot tell a link an older agentsync made from one a person made by hand.
// They are byte-identical on disk, and that is why an adopted record is a
// weaker claim than a created one and why removing one needs --prune-adopted.
// The reason on every item says so, because a caveat only in a doc comment is a
// caveat nobody reads.
func (p *Plan) findUnrecorded(repo *scan.Repo, res scan.Result, st *state.File, opts Options) {
	for _, e := range res.Entries {
		if e.State != scan.StateHealthyLink {
			continue
		}
		if _, recorded := st.LookupLink(e.Path); recorded {
			continue
		}
		judged, prob := linker.Judge(repo, e)
		if prob != nil {
			p.Problems = append(p.Problems, *prob)
			continue
		}
		// Only a link pointing at this project's own counterpart. A
		// mismatch is a link somebody aimed deliberately at another file,
		// and recording it would claim ownership of that decision.
		if judged.Action != linker.ActionLinked {
			continue
		}

		hash, err := scan.HashFile(judged.Target)
		if err != nil {
			p.problem(judged.Target, "hashing the target of an unrecorded link: %v", err)
			continue
		}
		item := Item{
			Project: e.Project, Name: e.Name, Path: e.Path,
			Target: judged.Target, Origin: state.OriginAdopted, Hash: hash,
		}
		// Short, because these lines come in dozens: this machine has 62.
		// The caveat the reader actually has to weigh is in the group note
		// and in the summary, which are printed once.
		if opts.Adopt {
			item.set(OpAdopt, "recording as adopted — this pass CANNOT tell an older agentsync's link from a handmade one; removing it later needs --"+PruneAdoptedFlag)
		} else {
			item.set(OpUnrecorded, "healthy and aimed at its counterpart, but unrecorded; --"+AdoptFlag+" records it")
		}
		p.add(item)
	}
}

// proposeRekeys pairs each orphaned capture directory doctor found with a live
// project of the same name that has no captures of its own.
//
// A proposal and nothing more. Correlating an orphaned key with a new one is a
// heuristic, and a wrong one here files one project's captures under another
// project's key — so the pairing is printed with the exact command that would
// apply it and never applied on its own.
func (p *Plan) proposeRekeys(repo *scan.Repo, res scan.Result, diag doctor.Report) {
	live := make([]string, 0, len(res.Dirs))
	for _, dir := range res.Dirs {
		live = append(live, scan.ProjectKey(repo.Home, dir))
	}
	sort.Strings(live)

	for _, f := range diag.OfKind(doctor.KindOrphanedCaptures) {
		for _, key := range live {
			if key == f.Project || path.Base(key) != path.Base(f.Project) {
				continue
			}
			has, err := hasCaptures(repo, key)
			if err != nil {
				p.problem(captureDir(repo, key), "could not tell whether this project has captures of its own: %v", err)
				continue
			}
			if has {
				continue
			}
			item := Item{Project: f.Project, Path: f.Path, Target: captureDir(repo, key)}
			item.set(OpRekeyCandidate, fmt.Sprintf("captures under a key no project has, beside %s which has none of its own — a move would explain both; nothing is applied unless you name it: --%s %s=%s",
				key, RekeyFlag, f.Project, key))
			p.add(item)
		}
	}
}

// planRekeys turns each explicit OLD=NEW into a checked move, or into a
// refusal naming the gate that stopped it.
func (p *Plan) planRekeys(repo *scan.Repo, res scan.Result, moves []KeyMove) {
	for _, m := range moves {
		rk, ref := p.planRekey(repo, res, m)
		if ref != nil {
			p.Refused = append(p.Refused, *ref)
			continue
		}
		p.Rekeys = append(p.Rekeys, *rk)
	}
}

func (p *Plan) planRekey(repo *scan.Repo, res scan.Result, m KeyMove) (*Rekey, *Refused) {
	oldDir, newDir := captureDir(repo, m.Old), captureDir(repo, m.New)
	refuse := func(g Guard, format string, args ...any) *Refused {
		return &Refused{
			Op: OpRekeyCandidate, OpName: OpRekeyCandidate.String(), Selector: m.String(),
			Path: oldDir, Guard: g, Reason: fmt.Sprintf(format, args...),
		}
	}

	if inside(oldDir, newDir) || inside(newDir, oldDir) {
		return nil, refuse(GuardOverlapping, "%s and %s are nested in one another, so moving one into the other has no meaning", m.Old, m.New)
	}

	captures, dirs, err := captureContents(oldDir)
	if err != nil {
		return nil, refuse(GuardUnreadable, "reading %s: %v", oldDir, err)
	}
	if len(captures) == 0 {
		return nil, refuse(GuardNoSource, "%s holds no captured agents, so there is nothing to move", oldDir)
	}
	if len(dirs) > 0 {
		return nil, refuse(GuardNestedCaptures, "%s contains %d subdirector%s (%s), which hold captures for projects nested under %s and which you did not name; move those keys explicitly instead",
			oldDir, len(dirs), plural2(len(dirs), "y", "ies"), strings.Join(dirs, ", "), m.Old)
	}

	destCaptures, destDirs, err := captureContents(newDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The ordinary case: the destination has never existed.
	case err != nil:
		return nil, refuse(GuardUnreadable, "reading %s: %v", newDir, err)
	case len(destCaptures) > 0 || len(destDirs) > 0:
		return nil, refuse(GuardOccupied, "%s already holds %d captured agent%s and %d subdirector%s; merging two projects' captures is exactly what this gate exists to prevent",
			newDir, len(destCaptures), plural(len(destCaptures)), len(destDirs), plural2(len(destDirs), "y", "ies"))
	}

	repoint, ref := rekeyRepoints(res, m, oldDir, newDir, refuse)
	if ref != nil {
		return nil, ref
	}
	return &Rekey{
		Old: m.Old, New: m.New, OldDir: oldDir, NewDir: newDir,
		Captures: len(captures), Repoint: repoint,
	}, nil
}

// rekeyRepoints collects the links that must follow the captures, and refuses
// the whole move if any link outside the destination project points into the
// directory being moved.
//
// Those links belong to a project this move does not re-key, so following them
// would rewrite a decision nobody made, and leaving them would break them.
// Neither is this command's to choose.
//
// The state records are not collected here: Apply looks each one up by the
// link's path, so a link with no record simply gains none, and one with a
// record has its target rewritten alongside the link itself.
func rekeyRepoints(res scan.Result, m KeyMove, oldDir, newDir string,
	refuse func(Guard, string, ...any) *Refused) ([]Repoint, *Refused) {

	repoint := make([]Repoint, 0)
	for _, e := range res.Entries {
		rel, ok := relUnder(oldDir, e.LinkTarget, e.LinkResolved)
		if !ok {
			continue
		}
		if e.Project != m.New {
			return nil, refuse(GuardCaptureInUse, "%s links into %s, and that project is not %s; moving the captures would break it",
				e.Path, oldDir, m.New)
		}
		// From is the literal target even when the resolved spelling is what
		// matched: repointOne re-reads the link and compares, and reading a
		// resolved path back off disk would report a correct link as one
		// somebody moved.
		repoint = append(repoint, Repoint{
			Project: e.Project, Name: e.Name, Path: e.Path,
			From: e.LinkTarget, To: filepath.Join(newDir, rel),
		})
	}
	sort.Slice(repoint, func(i, j int) bool { return repoint[i].Path < repoint[j].Path })
	return repoint, nil
}

// planDrops evaluates the commit gate for each --drop-capture selector.
//
// Only a clean CommittedYes authorises anything. "Could not ask" refuses with
// the rest, because a capture whose bytes are in no commit may be the last copy
// of something, and a gate that could not run has not passed.
func (p *Plan) planDrops(repo *scan.Repo, res scan.Result, drops []state.Key) {
	for _, k := range drops {
		selector := k.String()
		capture := repo.BackupPath(k.Project, k.Name)

		fi, err := os.Lstat(capture)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				p.refuse(OpDropCapture, selector, capture, GuardNoCapture, "no capture at %s; nothing was deleted", capture)
				continue
			}
			p.refuse(OpDropCapture, selector, capture, GuardUnreadable, "could not tell what is at %s: %v", capture, err)
			continue
		}
		if !fi.Mode().IsRegular() {
			p.refuse(OpDropCapture, selector, capture, GuardNotRegular, "%s is not a regular file (%v); reconcile deletes captured agent files and nothing else", capture, fi.Mode())
			continue
		}
		if user := linksInto(res, capture); user != "" {
			p.refuse(OpDropCapture, selector, capture, GuardCaptureInUse, "%s links to this capture; deleting it would manufacture a broken link, which this tool refuses to do", user)
			continue
		}

		switch c := scan.CommittedInHEAD(capture); c {
		case scan.CommittedYes:
		case scan.CommittedNo:
			p.refuse(OpDropCapture, selector, capture, GuardUncommitted,
				"the bytes in %s are in no commit in this repo — untracked, or edited since it was last committed — so they exist nowhere else; commit them first if you still want them gone", capture)
			continue
		default:
			p.refuse(OpDropCapture, selector, capture, GuardCommitUnknown,
				"git could not say whether %s is committed (%s), and a gate that could not run has not passed", capture, c)
			continue
		}

		hash, err := scan.HashFile(capture)
		if err != nil {
			p.refuse(OpDropCapture, selector, capture, GuardUnreadable, "hashing %s: %v", capture, err)
			continue
		}
		item := Item{Project: k.Project, Name: k.Name, Path: capture, Hash: hash}
		item.set(OpDropCapture, "committed in this repo's HEAD, so `git show` still has these exact bytes; deleting the working copy destroys nothing")
		p.add(item)
	}
}

// linksInto returns the first live symlink pointing at path, or "".
func linksInto(res scan.Result, path string) string {
	for _, e := range res.Entries {
		if e.LinkTarget == path || e.LinkResolved == path {
			return e.Path
		}
	}
	return ""
}

// captureDir is project-agents/<key>/, derived from BackupPath rather than
// rebuilt from a second copy of the layout, so the two cannot name different
// directories.
func captureDir(repo *scan.Repo, key string) string {
	return filepath.Dir(repo.BackupPath(key, ""))
}

// captureContents lists the .md files and the subdirectories directly under a
// capture directory. The error is returned rather than folded into empty
// results: "there is nothing here" and "I could not look" decide opposite
// things about a move.
//
// A symlink counts as neither, deliberately narrowly: an import writes regular
// files, so a link here was put there by hand. It therefore leaves a directory
// holding only symlinks looking empty to both callers, which today still
// refuses the move — but by os.Rename returning ENOTEMPTY, not by a gate. If a
// caller ever needs the refusal to be its own, this is the function to widen.
func captureContents(dir string) (captures, dirs []string, err error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents {
		switch {
		case e.IsDir():
			dirs = append(dirs, e.Name())
		case strings.HasSuffix(e.Name(), ".md") && e.Type().IsRegular():
			captures = append(captures, e.Name())
		}
	}
	return captures, dirs, nil
}

func hasCaptures(repo *scan.Repo, key string) (bool, error) {
	captures, dirs, err := captureContents(captureDir(repo, key))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(captures) > 0 || len(dirs) > 0, nil
}

// relUnder places a link's target inside dir, trying both spellings the
// classifier recorded, and returns the path relative to dir.
//
// The literal target alone is not enough. A link naming the same capture
// through a symlinked path component fails the containment test, and the caller
// would then neither repoint it nor refuse the move — the one outcome that
// leaves a dangling link behind a report claiming the gate held. linksInto and
// linker.pointsAt compare both forms for the same reason.
func relUnder(dir string, spellings ...string) (string, bool) {
	for _, candidate := range spellings {
		if candidate == "" {
			continue
		}
		rel, err := filepath.Rel(dir, candidate)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return rel, true
	}
	return "", false
}

// inside reports whether path lies within dir.
func inside(dir, path string) bool {
	_, ok := relUnder(dir, path)
	return ok
}

// short trims a hash for a report line. The full value stays in the JSON.
func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func plural(n int) string { return plural2(n, "", "s") }

func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Counts tallies items by op.
func (p Plan) Counts() map[Op]int {
	counts := make(map[Op]int)
	for _, i := range p.Items {
		counts[i.Op]++
	}
	return counts
}

// Writes returns the items an Apply would act on, in plan order.
func (p Plan) Writes() []Item {
	return p.filter(func(i Item) bool { return i.Op.Writes() })
}

// Unresolved returns the items that need a human, in plan order.
func (p Plan) Unresolved() []Item {
	return p.filter(func(i Item) bool { return i.Op.Unresolved() })
}

// OfOp returns the items of one op, in plan order.
func (p Plan) OfOp(o Op) []Item {
	return p.filter(func(i Item) bool { return i.Op == o })
}

func (p Plan) filter(keep func(Item) bool) []Item {
	out := make([]Item, 0, len(p.Items))
	for _, i := range p.Items {
		if keep(i) {
			out = append(out, i)
		}
	}
	return out
}

// Pending reports whether this plan has anything for Apply to do at all.
func (p Plan) Pending() bool {
	return len(p.Writes()) > 0 || len(p.Rekeys) > 0
}
