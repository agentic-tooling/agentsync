package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// allFlag widens a run back to the whole default tree ($AGENTSYNC_ROOT), so the machine-wide view
// is discoverable from --help rather than folklore about --root.
const allFlag = "all"

// The entries that identify a project directory, in the order the walk tests
// them: an agents directory, then the project that has no agents yet, then a
// project root with neither.
const (
	markerAgents = ".claude/agents"
	markerClaude = ".claude"
	markerGit    = ".git"
)

// rootOrigin records how a run's scan root was decided.
type rootOrigin int

const (
	// The zero value: a choice never resolved. It names no root and implies no
	// project, so nothing acts on it.
	rootOriginInvalid rootOrigin = iota

	rootFromFlag    // an explicit --root
	rootFromAll     // an explicit --all
	rootFromProject // the project the cwd is in; the one origin implying a project
	rootFromWide    // the default of a command that never derives one; see resolveWide
)

// rootChoice is the tree one run will scan, and why.
type rootChoice struct {
	Path   string
	Origin rootOrigin

	// Project is the key rootFromProject implies, empty for every other origin.
	// A --project flag is filled in from here rather than relaxed.
	Project string

	// marker is the entry that identified the project, named in the report.
	marker string
}

// rootFlags are the two ways a run may name its root, registered together
// because they are refused together.
type rootFlags struct {
	root *string
	all  *bool
}

func registerRoot(fs *flag.FlagSet) rootFlags {
	return rootFlags{
		root: fs.String("root", "",
			"directory tree to scan (default: the project the working directory is in)"),
		all: fs.Bool(allFlag, false, "scan the whole default tree ($"+envRoot+") instead of the project you are in"),
	}
}

// registerWideRoot registers the same pair for a command that never narrows.
// --all is accepted, meaning what the default already means, so the flag reads
// the same on every command rather than being missing from one. It differs from
// registerRoot in help text only: resolveWide enforces the exemption, and this
// is not a safety boundary.
func registerWideRoot(fs *flag.FlagSet) rootFlags {
	return rootFlags{
		root: fs.String("root", "", "directory tree to scan (default: the whole tree, $"+envRoot+")"),
		all:  fs.Bool(allFlag, false, "scan the whole default tree ($"+envRoot+"), which is already the default here"),
	}
}

// wideRoot is the whole default tree. There is no machine-agnostic guess for it,
// so an unset $AGENTSYNC_ROOT refuses rather than scanning somewhere unnamed.
func wideRoot() (string, error) {
	if p := strings.TrimSpace(os.Getenv(envRoot)); p != "" {
		// A relative value would name a different tree from every directory.
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("$%s must be an absolute path, got %q", envRoot, p)
		}
		return p, nil
	}
	return "", fmt.Errorf("no default tree: pass --root PATH or set $%s to the directory holding your projects", envRoot)
}

// settled returns the choice the flags decide with no derivation of any kind.
// Origin is rootOriginInvalid when neither settled it, leaving the meaning of
// their absence to the caller — all the two resolvers below disagree about.
func (f rootFlags) settled(home string) (rootChoice, error) {
	// Trimmed to empty counts as absent: filepath.Abs("") is the working
	// directory, so honouring it would scan somewhere nobody named.
	explicit := strings.TrimSpace(*f.root)
	if explicit != "" && *f.all {
		return rootChoice{}, fmt.Errorf("--root and --%s are mutually exclusive: --%s is the whole default tree, so naming a root as well says two different things",
			allFlag, allFlag)
	}
	if explicit != "" {
		return rootChoice{Path: explicit, Origin: rootFromFlag}, nil
	}
	if home == "" {
		return rootChoice{}, fmt.Errorf("no home directory, so project keys and the walk up from the working directory cannot be worked out; pass --root PATH")
	}
	if *f.all {
		p, err := wideRoot()
		if err != nil {
			return rootChoice{}, fmt.Errorf("--%s: %w", allFlag, err)
		}
		return rootChoice{Path: p, Origin: rootFromAll}, nil
	}
	return rootChoice{}, nil
}

// resolveWide decides the root for a command that must never narrow: --root if
// given, the machine-wide tree otherwise, with no walk up from the cwd.
//
// inventory is the only caller, and the exemption is deliberate. Its document
// is every agent on this machine by definition, so a project-scoped one is not
// a narrower view of it but a different document under the same name, and
// regenerating that from inside a project overwrites the real file. --check
// failing safe does not save it: its message names the overwriting command.
func (f rootFlags) resolveWide(home string) (rootChoice, error) {
	c, err := f.settled(home)
	if err != nil || c.Origin != rootOriginInvalid {
		return c, err
	}
	p, err := wideRoot()
	if err != nil {
		return rootChoice{}, err
	}
	return rootChoice{Path: p, Origin: rootFromWide}, nil
}

// resolve decides the root, given the resolved home directory project keys are
// expressed against. Absent both flags, it derives one from the cwd.
func (f rootFlags) resolve(home string) (rootChoice, error) {
	c, err := f.settled(home)
	if err != nil || c.Origin != rootOriginInvalid {
		return c, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return rootChoice{}, fmt.Errorf("could not determine the working directory, so the root cannot be derived from it (%w); pass --root PATH or --%s", err, allFlag)
	}
	dir, marker, err := detectProject(home, cwd)
	if err != nil {
		return rootChoice{}, err
	}
	// A new project has no marker yet, and widening to the whole tree from there
	// would scan every project on the machine; --all is the explicit way to ask.
	if dir == "" {
		return rootChoice{}, fmt.Errorf("no project found above the working directory, so nothing was scanned; run inside a project or create its .claude/agents/, or pass --%s to scan the whole tree ($%s) or --root PATH", allFlag, envRoot)
	}
	return rootChoice{
		Path:    dir,
		Origin:  rootFromProject,
		Project: scan.ProjectKey(home, filepath.Join(dir, ".claude", "agents")),
		marker:  marker,
	}, nil
}

// detectProject walks up from cwd and returns the first directory carrying a
// marker, with the marker that matched. It returns "" when it runs out of
// levels having found none.
//
// $HOME is never a candidate and neither is the filesystem root: scanning either
// is the wide run, which --all names. A cwd outside $HOME still gets a walk —
// "never above $HOME" forbids escaping home, not a tree never inside it.
//
// Every error is a refusal: reading an unreadable level as "nothing here" would
// ascend past the project to whatever encloses it.
func detectProject(home, cwd string) (dir, marker string, err error) {
	// One of them: os.Getwd can hand back a path that is no longer there, and
	// a walk from it finds nothing at every level and calls that a clean result.
	cur, err := resolveDir(cwd)
	if err != nil {
		return "", "", fmt.Errorf("could not resolve the working directory %q (%w); pass --root PATH or --%s", cwd, err, allFlag)
	}
	for cur != home && filepath.Dir(cur) != cur {
		m, err := markerAt(cur)
		if err != nil {
			return "", "", fmt.Errorf("could not read %q while deriving the root from the working directory (%w); pass --root PATH or --%s", cur, err, allFlag)
		}
		if m != "" {
			return cur, m, nil
		}
		cur = filepath.Dir(cur)
	}
	return "", "", nil
}

// markerAt reports which marker, if any, makes dir a project.
//
// .claude/agents is asked about only when .claude is a directory: a regular
// file by that name would otherwise produce an ENOTDIR, which does not satisfy
// fs.ErrNotExist and would refuse the whole run.
func markerAt(dir string) (string, error) {
	claude, err := statMarker(filepath.Join(dir, markerClaude))
	if err != nil {
		return "", err
	}
	if claude != nil && claude.IsDir() {
		agents, err := statMarker(filepath.Join(dir, markerClaude, "agents"))
		if err != nil {
			return "", err
		}
		if agents != nil && agents.IsDir() {
			return markerAgents, nil
		}
		return markerClaude, nil
	}

	// Anything at .git counts, directory or file: a worktree and a submodule
	// both leave a gitfile there, and both are project roots.
	git, err := statMarker(filepath.Join(dir, markerGit))
	if err != nil {
		return "", err
	}
	if git != nil {
		return markerGit, nil
	}
	return "", nil
}

// statMarker keeps "not there" and "could not look" apart. The second is not an
// answer, and reading it as the first would ascend past a project.
func statMarker(path string) (os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return fi, nil
}

// resolveDir makes a path absolute and follows symlinks, so the walk's stop
// test compares like with like against an already-resolved home.
//
// It returns the error where scan.resolvePath degrades to the unresolved path,
// because keying must always produce an answer while deciding a scan root may
// decline. Degrading
// here would read a deleted working directory, whose stats all answer ENOENT,
// as having no markers and climb past it.
func resolveDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// emitRootChoice prints the one line saying which tree this run is about, and
// why. rootOriginInvalid prints nothing, having named nothing.
//
// Callers emit it where they emit the report rather than where they resolve, so
// a run refused before reporting prints nothing at all.
func emitRootChoice(w io.Writer, home string, c rootChoice) {
	shown := displayPath(home, c.Path)
	switch c.Origin {
	case rootOriginInvalid:
		// Nothing was resolved, so there is nothing to name.
	case rootFromFlag:
		fmt.Fprintf(w, "Root: %s (from --root).\n", shown)
	case rootFromProject:
		fmt.Fprintf(w, "Root: %s (from the current directory, which is in a project: %s) — pass --root PATH or --%s to widen.\n",
			shown, c.marker, allFlag)
	case rootFromAll:
		fmt.Fprintf(w, "Root: %s (from --%s: the whole default tree, $%s).\n", shown, allFlag, envRoot)
	case rootFromWide:
		fmt.Fprintf(w, "Root: %s (the default tree, $%s: this command always describes the whole machine and never narrows to the project you are in).\n", shown, envRoot)
	}
}

// displayPath shortens a path under home to ~/…, as this tool writes
// home-relative paths everywhere else.
func displayPath(home, p string) string {
	if home == "" {
		return p
	}
	rel, err := filepath.Rel(home, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	if rel == "." {
		return "~"
	}
	return filepath.Join("~", rel)
}

// reportStream is where a run's human-readable asides go: stdout beside the
// text report, stderr when stdout carries a document a machine has to parse.
func reportStream(machine bool, stdout, stderr io.Writer) io.Writer {
	if machine {
		return stderr
	}
	return stdout
}

// restartNotice is the last line a run that changed a symlink prints — last,
// because a notice above a wall of per-file output is a notice nobody reads.
const restartNotice = "Restart your Claude Code session for this to take effect: agents are loaded when a session starts, so the session you are in still has the old set."

// emitRestartNotice prints that line only when a symlink was created or
// removed. Callers decide what qualifies; see each call site.
func emitRestartNotice(w io.Writer, changed bool) {
	if !changed {
		return
	}
	fmt.Fprintln(w, restartNotice)
}
