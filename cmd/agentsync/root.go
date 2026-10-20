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

// allFlag widens a run from the project you are in to the whole default tree.
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

	// from names where rootFromAll and rootFromWide found the default tree.
	from string
}

func registerAll(fs *flag.FlagSet) *bool {
	return fs.Bool(allFlag, false, "scan the whole default tree ($"+envRoot+" or root in the config file) instead of the project you are in")
}

// registerWideAll registers --all for inventory, where it means the default,
// so the flag reads the same on every command.
func registerWideAll(fs *flag.FlagSet) *bool {
	return fs.Bool(allFlag, false, "scan the whole default tree ($"+envRoot+" or root in the config file), which is already the default here")
}

// wideRoot is the whole default tree, from $AGENTSYNC_ROOT or else the config
// file, with where it came from. There is no machine-agnostic guess for it, so
// neither being set refuses rather than scanning somewhere unnamed.
func wideRoot() (path, from string, err error) {
	if p := strings.TrimSpace(os.Getenv(envRoot)); p != "" {
		// A relative value would name a different tree from every directory.
		if !filepath.IsAbs(p) {
			return "", "", fmt.Errorf("$%s must be an absolute path, got %q", envRoot, p)
		}
		return p, "$" + envRoot, nil
	}
	c, cfgPath, err := loadConfig()
	if err != nil {
		return "", "", err
	}
	if c.Root == "" {
		return "", "", notConfigured("default tree of projects", "root", envRoot, cfgPath)
	}
	return c.Root, "config file " + cfgPath, nil
}

// resolveAll is the whole default tree, as --all names it.
func resolveAll() (rootChoice, error) {
	p, from, err := wideRoot()
	if err != nil {
		return rootChoice{}, fmt.Errorf("--%s: %w", allFlag, err)
	}
	return rootChoice{Path: p, Origin: rootFromAll, from: from}, nil
}

// resolveWide decides the root for inventory, which never narrows. Its document
// is every agent on this machine, so a project-scoped one would be a different
// document under the same name, and regenerating it inside a project would
// overwrite the real file.
func resolveWide(all bool) (rootChoice, error) {
	if all {
		return resolveAll()
	}
	p, from, err := wideRoot()
	if err != nil {
		return rootChoice{}, err
	}
	return rootChoice{Path: p, Origin: rootFromWide, from: from}, nil
}

// resolveRoot decides the root: --all, else the project the cwd is in. Only
// the walk needs home; a wide run with none is left to the home warning and the
// write guards.
func resolveRoot(home string, all bool) (rootChoice, error) {
	if all {
		return resolveAll()
	}
	c, err := resolveProject(home)
	if err != nil {
		return rootChoice{}, fmt.Errorf("%w; or pass --%s to scan the whole default tree ($%s or root in the config file)", err, allFlag, envRoot)
	}
	return c, nil
}

// resolveProject is the project the cwd is in, found by a walk that stops at
// home. Its refusals name no flag, because override takes no --all.
func resolveProject(home string) (rootChoice, error) {
	if home == "" {
		return rootChoice{}, errors.New("no home directory, so the walk up from the working directory has nowhere to stop; set $HOME")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return rootChoice{}, fmt.Errorf("could not determine the working directory, so the project cannot be derived from it (%w)", err)
	}
	dir, marker, err := detectProject(home, cwd)
	if err != nil {
		return rootChoice{}, err
	}
	// A new project has no marker yet, and widening to the whole tree from there
	// would scan every project on the machine; --all is the explicit way to ask.
	if dir == "" {
		return rootChoice{}, errors.New("no project found above the working directory, so nothing was scanned; run inside a project or create its .claude/agents/")
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
		return "", "", fmt.Errorf("could not resolve the working directory %q (%w)", cwd, err)
	}
	for cur != home && filepath.Dir(cur) != cur {
		m, err := markerAt(cur)
		if err != nil {
			return "", "", fmt.Errorf("could not read %q while deriving the root from the working directory (%w)", cur, err)
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
	case rootFromProject:
		fmt.Fprintf(w, "Root: %s (from the current directory, which is in a project: %s) — pass --%s to widen.\n",
			shown, c.marker, allFlag)
	case rootFromAll:
		fmt.Fprintf(w, "Root: %s (from --%s: the whole default tree, %s).\n", shown, allFlag, c.from)
	case rootFromWide:
		fmt.Fprintf(w, "Root: %s (the default tree, %s: this command always describes the whole machine and never narrows to the project you are in).\n", shown, c.from)
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
