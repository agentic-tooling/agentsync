// Package scan classifies agent files found in projects' .claude/agents/
// directories against the canonical agent repository.
//
// The classification is deliberately read-only. Nothing in this package
// creates, modifies, or removes a file; callers decide what to do with the
// results.
package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// State is the classification of a single path inside a project's
// .claude/agents/ directory.
//
// State1Absent from the design is not produced here: deciding that an agent
// *should* be present requires the detection manifest, which belongs to the
// link command. Scan only classifies what exists on disk.
type State int

const (
	// StateInvalid is the zero value and never returned for a real entry.
	StateInvalid State = iota

	// StateHealthyLink is a symlink pointing into the agent repo whose
	// target exists. Nothing to do.
	StateHealthyLink

	// StateBrokenLink is a symlink pointing into the agent repo whose
	// target is gone — the agent was renamed or deleted upstream. Report
	// it; never silently unlink.
	StateBrokenLink

	// StateForeignLink is a symlink pointing somewhere outside the agent
	// repo. Someone else owns it. Never touch it.
	StateForeignLink

	// StateAdoptable is a real file whose content is byte-identical to its
	// counterpart in the repo. Replacing it with a symlink loses nothing.
	StateAdoptable

	// StateOverride is a real file that shares a name with a counterpart in
	// the repo but differs in content. It is a deliberate customization and
	// must never be overwritten.
	StateOverride

	// StateUnadopted is a real file with no counterpart in the repo at all.
	// It exists only on disk and must be imported before anything else
	// touches the directory.
	StateUnadopted
)

func (s State) String() string {
	switch s {
	case StateHealthyLink:
		return "healthy-link"
	case StateBrokenLink:
		return "broken-link"
	case StateForeignLink:
		return "foreign-link"
	case StateAdoptable:
		return "adoptable"
	case StateOverride:
		return "override"
	case StateUnadopted:
		return "unadopted"
	default:
		return "invalid"
	}
}

// AtRisk reports whether this state means the agent repo holds no copy of the
// content. It is a judgement about the agent repo alone — see Entry.AtRisk for
// the question that actually decides whether a file is unprotected.
func (s State) AtRisk() bool {
	return s == StateOverride || s == StateUnadopted
}

// Entry is one classified file.
type Entry struct {
	// Project is the owning project's key as produced by ProjectKey: a
	// slash-separated path, not a bare directory name. It selects the
	// project's counterpart under project-agents/<Project>/ and is where an
	// import would write this file's backup.
	Project string `json:"project"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	State   State  `json:"state"`

	// Counterpart is the repo file this entry was compared against, empty
	// when no counterpart exists.
	Counterpart string `json:"counterpart,omitempty"`

	// Hash is the SHA-256 of the on-disk content. Empty for symlinks, whose
	// content belongs to the target rather than the link.
	Hash string `json:"hash,omitempty"`

	// LinkTarget is what the symlink itself points at — the literal target
	// made absolute and cleaned, with no symlink chain followed. It is set
	// for every symlink state, including broken ones, and is always the
	// unresolved destination so that the same field means the same thing in
	// every state.
	LinkTarget string `json:"link_target,omitempty"`

	// LinkResolved is LinkTarget with every intermediate symlink followed,
	// set only when the chain actually resolves. It is empty for a broken
	// link, and for a foreign link whose target does not exist.
	LinkResolved string `json:"link_resolved,omitempty"`

	// Tracked records whether the owning project has this file present in
	// its HEAD commit. A file absent from the agent repo but committed in
	// its own project is not at risk.
	Tracked Tracked `json:"tracked"`
}

func (e Entry) String() string {
	return fmt.Sprintf("%s/%s: %s", e.Project, e.Name, e.State)
}

// AtRisk reports whether this file is unprotected everywhere: the agent repo
// has no copy of its content, and its own project does not have it committed
// either. Losing the file would lose it for good.
//
// TrackedUnknown counts as untracked so a failed check never silently
// downgrades a warning.
func (e Entry) AtRisk() bool {
	return e.State.AtRisk() && e.Tracked != TrackedYes
}

// Repo is the canonical agent repository being compared against.
//
// A Repo carries a per-scan git lookup cache, so it must not be copied after
// first use; pass it by pointer.
type Repo struct {
	// Root is the absolute path to the canonical agents repo checkout.
	Root string

	// Home is the resolved home directory that project keys are expressed
	// relative to — see ProjectKey. NewRepo fills it in; a caller with an
	// unusual layout (and every test) may override it. An empty Home is
	// valid and keys every project by absolute path instead.
	//
	// It is deliberately not derived from the scan root: a key must name
	// the same project no matter which root the scan was invoked with.
	Home string

	// git answers "is this file committed in its own project?" and caches
	// the per-directory repository lookup.
	git gitProber
}

// NewRepo resolves root to an absolute, symlink-free path.
func NewRepo(root string) (*Repo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving repo root: %w", err)
	}
	// EvalSymlinks matters here: on macOS /tmp is itself a symlink, and
	// without this every link into a temp-dir repo would look foreign.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolving repo root %q: %w", abs, err)
	}
	return &Repo{Root: resolved, Home: homeDir()}, nil
}

// homeDir returns the resolved home directory, or "" when it cannot be
// determined. An unknown home is not an error: ProjectKey falls back to keying
// by absolute path, which is still unique and still stable.
func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return ""
	}
	return resolvePath(h)
}

// resolvePath makes p absolute and follows symlinks, degrading to the cleaned
// absolute path when either step fails. It never returns an error: a path that
// cannot be resolved still has to produce a deterministic key.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// Counterpart returns the repo file an agent of this name in this project
// should be compared against, or "" if there is none.
//
// A project-specific copy wins over the global pool: if project-agents/<key>/
// holds the name, that is the counterpart, because it represents the adopted
// version of this project's file. Only when no project-specific copy exists
// does the global agent apply.
//
// project is a key from ProjectKey — a slash-separated path such as
// "Web/monorepo/nested-fixture", so project-agents/ is a tree. Keying by
// bare basename would let two same-named projects in different parents share
// one counterpart directory and be judged against each other's files.
func (r *Repo) Counterpart(project, name string) string {
	candidates := []string{
		r.BackupPath(project, name),
		filepath.Join(r.Root, "agents", name+".md"),
	}
	// This conflates "no counterpart exists" with "the counterpart could not
	// be stat-ed", and it is left that way deliberately — audited under
	// issue #12 and recorded here so the next reader does not have to
	// re-derive it.
	//
	// Both collapse to "", which routes the entry to StateUnadopted. That
	// is the loud direction, not the quiet one: StateUnadopted.AtRisk() is
	// true, so a counterpart hidden by a permission error makes the file
	// report as unprotected rather than as safely adopted. The dangerous
	// mistake would be the reverse — naming a counterpart that is not
	// really there, which would let a real file be judged StateAdoptable
	// and replaced with a symlink. An error can never produce that.
	//
	// The write paths do not take this word for it either. The first
	// candidate is BackupPath, which is also import's destination, and
	// judgeDest re-stats it independently and fails hard on any error that
	// is not fs.ErrNotExist. So a stat that fails here fails there too, as
	// a Problem rather than as a silent capture.
	//
	// Returning an error instead was considered and rejected: Counterpart
	// answers "which file, if any" and has no third value to carry one, so
	// every caller would gain an error path whose only correct handling is
	// the behaviour it already has.
	for _, c := range candidates {
		if fi, err := os.Lstat(c); err == nil && fi.Mode().IsRegular() {
			return c
		}
	}
	return ""
}

// BackupPath is where this project's copy of an agent lives in the repo:
// project-agents/<key>/<name>.md.
//
// It is both the first counterpart candidate and the destination an import
// writes to, and it exists so those two can never drift. They are the same
// path by definition: importing a file is what makes it that project's
// counterpart, and a captured file that no longer answers Counterpart would be
// re-imported on every run.
//
// project is a key from ProjectKey, which is slash-separated regardless of
// platform, so it is converted before being joined.
func (r *Repo) BackupPath(project, name string) string {
	return filepath.Join(r.Root, "project-agents", filepath.FromSlash(project), name+".md")
}

// owns reports whether path lies inside the repo.
func (r *Repo) owns(path string) bool {
	rel, err := filepath.Rel(r.Root, path)
	if err != nil {
		return false
	}
	return contains(rel)
}

// contains reports whether a relative path stays inside the base it was
// computed against, i.e. whether it needs no ".." to get there.
func contains(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// HashFile returns the hex SHA-256 of a file's contents.
//
// Exported because identity in this tool is content, never name, size, or
// mtime: any command that has to decide whether two files are the same file
// must ask this exact question, of the same implementation the classifier used.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	// Read-only fd (os.Open is O_RDONLY): there is nothing buffered to flush,
	// so Close can only report a fd-level fault we could not act on anyway.
	// Ignored explicitly rather than via .golangci.yml, so that errcheck keeps
	// flagging a Close() on a file opened for writing -- where a dropped error
	// does mean lost data.
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Classify determines the state of a single path.
//
// path must be a file inside some project's .claude/agents/ directory.
// project is the key from ProjectKey, used to look up project-specific
// counterparts.
func (r *Repo) Classify(project, path string) (Entry, error) {
	name := strings.TrimSuffix(filepath.Base(path), ".md")
	e := Entry{Project: project, Name: name, Path: path}

	// Lstat, never Stat. Stat follows symlinks, which would report a
	// healthy link as a regular file and make it look adoptable — the
	// single most likely cause of data loss in this tool.
	fi, err := os.Lstat(path)
	if err != nil {
		return e, fmt.Errorf("lstat %q: %w", path, err)
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		return r.classifyLink(e, path)
	}

	if !fi.Mode().IsRegular() {
		return e, fmt.Errorf("%q is neither a regular file nor a symlink", path)
	}

	return r.classifyFile(e, project, name, path)
}

func (r *Repo) classifyLink(e Entry, path string) (Entry, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return e, fmt.Errorf("readlink %q: %w", path, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	target = filepath.Clean(target)
	// LinkTarget always holds the unresolved destination; the resolved form
	// goes in its own field so the two are never confused.
	e.LinkTarget = target

	// Resolve through any intermediate links so a symlink chain that ends
	// inside the repo is still recognised as ours.
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		// Target does not exist. Whether it is ours is still decided by
		// path: a dangling link into the repo is a broken link we must
		// report, not a foreign one we may ignore.
		//
		// The target itself cannot be resolved, but its parent usually
		// can. Resolving the parent normalises platform-level symlinks
		// (on macOS /tmp and /var are themselves links) so ownership is
		// compared against the same real path as the repo root.
		if errors.Is(err, fs.ErrNotExist) {
			probe := target
			if parent, perr := filepath.EvalSymlinks(filepath.Dir(target)); perr == nil {
				probe = filepath.Join(parent, filepath.Base(target))
			}
			if r.owns(probe) {
				e.State = StateBrokenLink
			} else {
				e.State = StateForeignLink
			}
			return e, nil
		}
		return e, fmt.Errorf("resolving %q: %w", target, err)
	}

	e.LinkResolved = resolved
	if r.owns(resolved) {
		e.State = StateHealthyLink
	} else {
		e.State = StateForeignLink
	}
	return e, nil
}

func (r *Repo) classifyFile(e Entry, project, name, path string) (Entry, error) {
	hash, err := HashFile(path)
	if err != nil {
		return e, fmt.Errorf("hashing %q: %w", path, err)
	}
	e.Hash = hash
	e.Tracked = r.git.tracked(path)

	counterpart := r.Counterpart(project, name)
	if counterpart == "" {
		e.State = StateUnadopted
		return e, nil
	}
	e.Counterpart = counterpart

	counterHash, err := HashFile(counterpart)
	if err != nil {
		return e, fmt.Errorf("hashing counterpart %q: %w", counterpart, err)
	}

	if hash == counterHash {
		e.State = StateAdoptable
	} else {
		e.State = StateOverride
	}
	return e, nil
}

// skipDirs are never descended into when searching for agent directories.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".venv":        true,
	"venv":         true,
	"target":       true,
	".next":        true,
	".svelte-kit":  true,
}

// FindAgentDirs walks root and returns every .claude/agents directory found,
// at any depth.
//
// A subtree that cannot be read is skipped rather than aborting the walk, but
// it is reported as a problem: a permission-denied directory must never be
// indistinguishable from an empty one, or the scan would quietly claim that
// nothing is at risk in a tree it never looked at.
//
// A hard error is returned only when the walk root itself cannot be read,
// which makes the whole result meaningless.
func FindAgentDirs(root string) ([]string, []ScanProblem, error) {
	var (
		dirs     []string
		problems []ScanProblem
	)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				// Nothing was scanned at all. Fatal.
				return err
			}
			problems = append(problems, problem(path, fmt.Errorf("subtree skipped, not readable: %w", err)))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		base := d.Name()
		if skipDirs[base] {
			return fs.SkipDir
		}
		if base == "agents" && filepath.Base(filepath.Dir(path)) == ".claude" {
			dirs = append(dirs, path)
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walking %q: %w", root, err)
	}
	return dirs, problems, nil
}

// agentFiles lists the .md files directly inside one agents directory.
//
// It exists instead of filepath.Glob, which got two different questions wrong
// for the same underlying reason: **the directory path is being used as a
// pattern, and a pattern has no way to report that it matched nothing because
// something went wrong.**
//
// # The path is data, not a pattern
//
// This is the stronger of the two, because it needs no unusual permissions —
// only a directory whose name contains a glob metacharacter. Glob is handed
// filepath.Join(dir, "*.md") and interprets the *whole* string, dir included,
// so any `*`, `?`, or `[` anywhere in the project's path is expanded rather
// than matched literally. Measured, with three sibling projects:
//
//	project directory   Glob     os.ReadDir
//	proj[1]             0 files  1 file
//	pkg[a-z]            0 files  1 file
//	proj*               2 files  1 file      <- and they are OTHER projects' files
//	proj?               2 files  1 file
//
// The bracket forms silently lose the project. The star and question forms are
// worse: they match sibling directories, and Scan then classifies another
// project's file under *this* project's key. Confirmed end to end on the
// pre-fix code — a tree of projA, projB, and proj* scans as five entries, of
// which two attribute projA.md and projB.md to project "proj*".
//
// That is precisely the cross-contamination ProjectKey exists to prevent, and
// which it documents as data loss: the wrongly-keyed entry gets its
// counterpart from the wrong project's project-agents/ directory, and an
// import would write it into the wrong project's backup path. `[` and `]` are
// not hypothetical in a projects tree either — Next.js and SvelteKit route
// directories are named exactly that way.
//
// # The directory could not be read
//
// Glob's contract is explicit — "Glob ignores file system errors such as I/O
// errors reading directories. The only possible returned error is
// ErrBadPattern" — so an unreadable agents directory produces `nil, nil`,
// byte-for-byte the answer an empty one produces. That is issue #12's shape
// with no git in it: one return value carrying both "there is nothing here"
// and "I could not look".
//
// It defeated the guarantee FindAgentDirs is written to provide a few lines up
// — "a permission-denied directory must never be indistinguishable from an
// empty one, or the scan would quietly claim that nothing is at risk in a tree
// it never looked at." FindAgentDirs keeps that promise for the walk and then
// handed the directory to a function that broke it. The walk cannot cover for
// it either: it identifies `.claude/agents` from its *parent's* readdir and
// returns fs.SkipDir immediately, so it never opens the directory and never
// sees the failure. The directory was then reported in Result.Dirs with zero
// entries and zero problems.
//
// # What os.ReadDir changes, and what it does not
//
// It takes a directory rather than a pattern, so the path is data; and it
// returns the error. Everything else is preserved, verified against the
// previous behaviour: same *.md set, same lexical order (ReadDir sorts by
// filename, as Glob does), same single non-recursive level, dotfiles included
// by both (Go's path.Match does not special-case a leading dot the way a shell
// does), symlinks and dangling symlinks listed by both.
//
// Rejected: keeping Glob and escaping the directory prefix with
// filepath.QuoteMeta. It fixes only the first problem and leaves the silent
// unreadable-directory case exactly as it was — and it leaves the next reader
// with a pattern built from data, which is the thing that was wrong.
//
// Rejected: keeping Glob and stat-ing the directory first to prove it is
// readable. That is a TOCTOU with a second syscall's worth of window, and it
// still asks a different question than the one being answered — "was this
// readable a moment ago" rather than "did the read succeed".
func agentFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading agents directory: %w", err)
	}
	var out []string
	for _, e := range ents {
		// No IsDir filter here on purpose. A directory named foo.md is a
		// thing Classify is required to reject loudly, and the existing
		// test for it depends on this function handing it over rather
		// than quietly dropping it.
		if strings.HasSuffix(e.Name(), ".md") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// Reserved first components of a project key. Only ProjectKey itself emits
// them, and a $HOME-relative key that would start with one is pushed into the
// absolute-path form instead — so the two namespaces stay disjoint and no two
// distinct project directories can ever land on the same key.
const (
	keyOutsideHome = "_outside-home"
	keyHomeItself  = "_home"
)

var reservedKeyRoots = map[string]bool{
	keyOutsideHome: true,
	keyHomeItself:  true,
}

// ProjectKey derives the identity key of the project owning agentsDir — the
// directory two levels above it, i.e. the parent of .claude.
//
// The key does two jobs: it selects the counterpart at
// project-agents/<key>/<name>.md, and it is where an import writes that
// project's backup. Both make uniqueness a data-safety property, not a
// cosmetic one. The original scheme — the bare basename of the project
// directory — is not unique: mono-a/packages/api and mono-b/packages/api both
// reduce to "api", so one project's file is compared against the other's
// baseline today, and would overwrite it once import exists.
//
// The key is the project's path relative to home, kept as a nested path:
//
//	/Users/x/Web/example                 -> Web/example
//	/Users/x/Web/monorepo/nested-fixture -> Web/monorepo/nested-fixture
//	/opt/thing                           -> _outside-home/opt/thing
//	the home directory itself            -> _home
//
// What that buys:
//
//   - Unique. Two distinct directories differ in some path component, and the
//     key preserves every component, so their keys differ too.
//   - Independent of the scan root. It is computed from the project's absolute
//     path and home, never from the --root the caller passed, so ~/Web/example
//     keys as Web/example whether the scan started at ~/Web, at ~, or at the
//     project itself. A root-relative key would rename the project depending on
//     how it was found, and counterpart lookup would miss.
//   - Readable. project-agents/Web/example/ is recognisable at a glance, which
//     an opaque hash of the path would not be.
//   - Filesystem-safe. Every component is an existing directory name on the
//     same filesystem, so spaces, dots, and unicode are already legal; "/" is
//     the one byte a component cannot contain, and it is the only character the
//     key adds.
//
// Nesting rather than flattening is deliberate. Flattening the path into a
// single component needs a separator, and every candidate separator is a
// character a real directory name may contain ("my-cli" already
// rules out "-"), so a flattened key is injective only if the separator is
// escaped inside components too — at which point it stops being readable,
// which was the point of not hashing. A nested path needs no escaping at all.
// The cost is that project-agents/ is a tree: enumerating projects means
// walking it rather than listing one level, and a directory there may hold
// both .md files (it is a project) and subdirectories (projects nested below
// it). That is a mild inconvenience for tooling; a wrong-project counterpart
// is data loss, so injectivity wins.
//
// Paths are resolved — made absolute, symlinks followed — before the key is
// computed, so reaching one project by two different routes still yields one
// key.
func ProjectKey(home, agentsDir string) string {
	// Clean before walking up: filepath.Dir on a path with a trailing
	// separator consumes nothing, so an uncleaned ".../agents/" would climb
	// only one level and key the project as "<proj>/.claude".
	//
	// The agents directory itself is not resolved, only the project
	// directory above it. If <proj>/.claude/agents were a symlink into a
	// shared location, resolving it would give every project pointing there
	// the same key — reintroducing the collision this function exists to
	// remove.
	projectDir := resolvePath(filepath.Dir(filepath.Dir(filepath.Clean(agentsDir))))

	if home != "" {
		if rel, err := filepath.Rel(resolvePath(home), projectDir); err == nil && contains(rel) {
			if rel == "." {
				return keyHomeItself
			}
			key := filepath.ToSlash(rel)
			// A project literally at $HOME/_outside-home/... would
			// otherwise be able to collide with the namespace below.
			// Falling through keys it by absolute path instead.
			if head, _, _ := strings.Cut(key, "/"); !reservedKeyRoots[head] {
				return key
			}
		}
	}

	// Outside home, or home unknown: key by absolute path under a reserved
	// prefix. The leading separator is dropped so the key stays relative and
	// cannot escape project-agents/.
	abs := filepath.ToSlash(projectDir)
	return path.Join(keyOutsideHome, strings.TrimPrefix(abs, "/"))
}

// ProjectDir is the inverse of ProjectKey: the directory a key names.
//
// It exists because link can be told to provision a project by key
// (--project) that the scan never found — a project whose .claude/agents
// directory does not exist yet has nothing for the walk to discover, and that
// is precisely the state-1 case link is asked to fix. Turning the key back
// into a path is the only way to address it.
//
// It lives beside ProjectKey rather than in the caller so the two cannot
// drift. A key that ProjectDir sends somewhere ProjectKey would not name is a
// key pointing at the wrong project's files, which is the failure the whole
// keying scheme exists to prevent — so callers are expected to re-key the
// result and refuse on a mismatch, which is also what catches the one case
// this direction cannot reproduce: ProjectKey resolves symlinks, and a
// component that is a symlink resolves to a path whose key differs from the
// one asked for.
//
// An error means the key names nothing usable. It is never a silent fallback:
// guessing a directory here provisions the wrong project.
func ProjectDir(home, key string) (string, error) {
	clean := path.Clean(key)
	if key == "" || clean == "." {
		return "", fmt.Errorf("empty project key")
	}
	// A key is always relative and always stays inside project-agents/. One
	// that climbs out of it, or that arrives absolute, is not a key this
	// scheme ever produced and must not be turned into a path.
	if path.IsAbs(clean) || !contains(filepath.FromSlash(clean)) {
		return "", fmt.Errorf("project key %q is not relative to the agent repo's key namespace", key)
	}

	head, rest, _ := strings.Cut(clean, "/")
	switch head {
	case keyHomeItself:
		if rest != "" {
			return "", fmt.Errorf("project key %q: %q names the home directory itself and takes no suffix", key, keyHomeItself)
		}
		if home == "" {
			return "", fmt.Errorf("project key %q names the home directory, but no home directory is known", key)
		}
		return resolvePath(home), nil
	case keyOutsideHome:
		if rest == "" {
			return "", fmt.Errorf("project key %q: %q is a namespace prefix, not a project", key, keyOutsideHome)
		}
		// ProjectKey built this by shaving the leading separator off an
		// absolute path, so putting it back is the whole inverse.
		return filepath.Join(string(filepath.Separator), filepath.FromSlash(rest)), nil
	}

	if home == "" {
		return "", fmt.Errorf("project key %q is relative to the home directory, but no home directory is known", key)
	}
	return filepath.Join(resolvePath(home), filepath.FromSlash(clean)), nil
}

// KeyDependsOnHome reports whether ProjectDir consults the home directory to
// resolve this key.
//
// It is false for exactly one namespace: _outside-home/, whose keys carry an
// absolute path that ProjectDir turns back into one by joining it onto "/",
// without reading home at all. Every other key — including _home, which *is*
// home — is located relative to it, so a wrong home sends it somewhere else.
//
// It exists so a caller can tell which of its results are evidence about the
// home and which cannot be. doctor is the case: it treats "every capture
// directory names a project that is not there" as a sign that the home is
// wrong, and that inference is only valid for the keys home actually resolved.
// An _outside-home/ capture whose project was genuinely deleted is orphaned for
// reasons that have nothing to do with home, and counting it would make the
// warning name a directory that took no part in the lookup.
//
// It lives beside ProjectDir for the reason ProjectDir lives beside ProjectKey:
// a caller inspecting the key prefix itself would be restating a rule that is
// ProjectDir's to state, and the two would drift the moment a namespace is
// added.
func KeyDependsOnHome(key string) bool {
	return !outsideHomeKey(key)
}

// ScanProblem is a path the scan could not account for: a file it could not
// classify, or a subtree it could not read.
//
// Problems are collected rather than fatal. One malformed entry in one project
// must not blind the scan to an unprotected override in a different project —
// but a problem is also never silently dropped, because the path it names might
// be exactly the file that would be lost.
type ScanProblem struct {
	Path string `json:"path"`
	Err  string `json:"error"`
}

func (p ScanProblem) String() string {
	return fmt.Sprintf("%s: %s", p.Path, p.Err)
}

func problem(path string, err error) ScanProblem {
	return ScanProblem{Path: path, Err: err.Error()}
}

// Result is a full scan across one or more roots.
//
// Entries, Dirs, and Problems are always non-nil after a Scan, so the JSON form
// carries empty arrays rather than nulls.
type Result struct {
	Entries []Entry  `json:"entries"`
	Dirs    []string `json:"dirs"`

	// Problems lists paths the scan could not account for. A non-empty
	// Problems means the report is incomplete: "no at-risk files" is only a
	// statement about what could be read.
	Problems []ScanProblem `json:"problems"`
}

// Counts tallies entries by state.
func (res Result) Counts() map[State]int {
	counts := make(map[State]int)
	for _, e := range res.Entries {
		counts[e.State]++
	}
	return counts
}

// AtRisk returns entries that are unprotected everywhere — no copy in the
// agent repo, and not committed in their own project. The result is always
// non-nil so callers can marshal it without producing a JSON null.
func (res Result) AtRisk() []Entry {
	out := make([]Entry, 0, len(res.Entries))
	for _, e := range res.Entries {
		if e.AtRisk() {
			out = append(out, e)
		}
	}
	return out
}

// Scan classifies every agent file under every .claude/agents directory
// beneath root. The repo's own project-agents/ and agents/ trees are excluded:
// they are the source being compared against, not projects to scan.
//
// An entry that cannot be classified — a directory named *.md, a symlink loop,
// an unreadable file — is recorded in Result.Problems and the scan continues.
// A hard error is returned only when the scan as a whole could not run, since
// aborting on one odd file would hide unprotected content everywhere else.
func (r *Repo) Scan(root string) (Result, error) {
	res := Result{
		Entries:  make([]Entry, 0),
		Dirs:     make([]string, 0),
		Problems: make([]ScanProblem, 0),
	}

	dirs, problems, err := FindAgentDirs(root)
	if err != nil {
		return res, err
	}
	res.Problems = append(res.Problems, problems...)

	for _, dir := range dirs {
		// Compare ownership on the resolved path. The walk yields paths
		// built from the caller's root, which may reach the repo through
		// a different symlink prefix than Root was resolved to; without
		// this the repo's own trees would be scanned as if they were a
		// project.
		probe := dir
		if resolved, rerr := filepath.EvalSymlinks(dir); rerr == nil {
			probe = resolved
		}
		if r.owns(probe) {
			continue
		}
		res.Dirs = append(res.Dirs, dir)

		project := ProjectKey(r.Home, dir)
		matches, err := agentFiles(dir)
		if err != nil {
			res.Problems = append(res.Problems, problem(dir, err))
			continue
		}
		for _, m := range matches {
			e, err := r.Classify(project, m)
			if err != nil {
				res.Problems = append(res.Problems, problem(m, err))
				continue
			}
			res.Entries = append(res.Entries, e)
		}
	}
	return res, nil
}
