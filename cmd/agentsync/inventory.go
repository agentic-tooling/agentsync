package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/agentic-tooling/agentsync/internal/inventory"
	"github.com/agentic-tooling/agentsync/internal/scan"
)

// inventoryFileName is where the inventory lives, relative to the repo root.
// It is committed, so `git log` answers "when did this project gain that
// agent?" — the history is the point, and it comes free.
const inventoryFileName = "inventory.json"

// runInventory regenerates the derived view of every agent on this machine.
//
// This writes without an --apply, which every other writing command requires,
// and the difference is deliberate rather than an oversight. --apply guards
// writes that can destroy something: import can overwrite a capture, link
// replaces a project's file. This command's only output is a file it alone
// owns, computed entirely from the filesystem, and regenerating it is the
// documented fix for any inconsistency rather than a risk to be gated. A tool
// that is unsafe to re-run is not a materialized view.
//
// The home check is a warning rather than a refusal, as in scan and doctor: a
// read-only command with a wrong $HOME produces a visibly wrong report, which
// fails safe. RequireHome exists to stop a write landing in the wrong
// namespace, and the only write here is this command's own derived file.
//
// It is also the one command that does not derive its root from the working
// directory. That document describes the whole machine by definition, so
// narrowing it would not produce a smaller inventory but a different one under
// the same name — see resolveWide, which carries the argument in full.
func runInventory(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inventory", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	all := registerWideAll(fs)
	out := fs.String("out", "", "write here instead of <repo>/"+inventoryFileName)
	toStdout := fs.Bool("stdout", false, "write to stdout and leave the file alone")
	check := fs.Bool("check", false,
		"write nothing; exit 1 if the file on disk is not what a regeneration would produce")
	allowOutsideHome := fs.Bool(scan.AllowOutsideHomeFlag, false,
		"projects legitimately live outside $HOME; suppress the home warning")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		// The flag package has already written the message and the usage
		// block to stderr.
		return exitFailure
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentsync: %s\n", strayArg("inventory", fs.Arg(0), nil))
		return exitFailure
	}

	// Refused rather than resolved by precedence. --stdout says "write the
	// document here", --check says "write nothing and compare" — whichever
	// won, the other would be silently ignored, and the silent outcome in CI
	// is the dangerous one: `--check --stdout` would exit 0 on every run and
	// read as a passing freshness gate while checking nothing at all.
	if *toStdout && *check {
		fmt.Fprintln(stderr, "agentsync: --stdout and --check are mutually exclusive: "+
			"--check writes nothing, so there is nothing to send to stdout")
		return exitFailure
	}

	repo, repoFrom, err := openRepo()
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	root, err := resolveWide(*all)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	res, err := repo.Scan(root.Path)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}
	warnHome(stderr, repo, root.Path, res.Dirs, *allowOutsideHome)

	inv, err := inventory.Build(repo, res)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	// Before the document is written or compared, and on every path including
	// --stdout and --check, because it qualifies the numbers all three produce.
	//
	// Every unknown in this package resolves downward, so nothing absent can
	// inflate what the document claims loads — but a missing install registry
	// zeroes the plugin counts in a way that reads exactly like a machine with
	// no plugins. Silently low and internally consistent is the one shape a
	// reader cannot catch, so it is said out loud here rather than left in the
	// file. stderr, not stdout: --stdout must stay machine-parseable.
	if gap := inventory.RegistryGap(repo.Home); gap != "" {
		fmt.Fprintf(stderr, "agentsync: warning: %s\n", gap)
	}

	want, err := inv.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}

	// --stdout is this command's machine-readable form, as --json is elsewhere.
	emitRepoChoice(reportStream(*toStdout, stdout, stderr), repo, repoFrom)
	emitRootChoice(reportStream(*toStdout, stdout, stderr), repo.Home, root)

	if *toStdout {
		if _, err := stdout.Write(want); err != nil {
			fmt.Fprintf(stderr, "agentsync: writing to stdout: %v\n", err)
			return exitFailure
		}
		return exitOK
	}

	dest := *out
	if dest == "" {
		dest = filepath.Join(repo.Root, inventoryFileName)
	}

	if *check {
		return checkInventory(dest, want, stdout, stderr)
	}

	if err := writeInventory(dest, want); err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "Wrote %s\n", dest)
	emitInventorySummary(stdout, inv)
	return exitOK
}

// checkInventory compares the file on disk against a fresh render.
//
// This is the point of having no timestamp and no tool version in the document:
// with nothing volatile in it, a byte difference is a real difference in the
// tree, so this is a meaningful freshness test rather than a clock reading. It
// is what lets CI assert the committed inventory still describes reality.
func checkInventory(dest string, want []byte, stdout, stderr io.Writer) int {
	got, err := os.ReadFile(dest)
	if err != nil {
		// os.ErrNotExist rather than the fs.ErrNotExist the rest of this repo
		// spells it as. They are the same sentinel value; importing io/fs here
		// would shadow it behind the flag set every command in this package
		// names fs, and a shadowed sentinel is the kind of thing that compiles
		// happily right up until someone moves a line between functions.
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "agentsync: %s does not exist; run `agentsync inventory` to create it\n", dest)
			return exitAtRisk
		}
		fmt.Fprintf(stderr, "agentsync: reading %q: %v\n", dest, err)
		return exitFailure
	}
	if !bytes.Equal(got, want) {
		fmt.Fprintf(stderr,
			"agentsync: %s is out of date; run `agentsync inventory` to regenerate it\n", dest)
		return exitAtRisk
	}
	fmt.Fprintf(stdout, "%s is up to date.\n", dest)
	return exitOK
}

// writeInventory replaces the file through a temporary in the same directory.
//
// The same reasoning as import's copyAtomic, for a much less precious file: a
// reader sees the whole old document or the whole new one, and a crash leaves a
// temporary rather than a half-written inventory. A truncated JSON document is
// worse than a missing one, because it still parses as a file and fails only
// when something tries to read it.
func writeInventory(dest string, content []byte) (err error) {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".agentsync-inventory-*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %q: %w", dir, err)
	}
	name := tmp.Name()
	// Any failure past this point must not leave the temporary behind. Not
	// because it would be committed — .gitignore covers *.tmp — but because it
	// would accumulate silently in the repo root, one per failed run, with a
	// name that says nothing about when or why it was left.
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()

	if _, err = tmp.Write(content); err != nil {
		return fmt.Errorf("writing %q: %w", name, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %q: %w", name, err)
	}
	// CreateTemp makes the file 0600; this is an ordinary readable file in a
	// git repository like every other file here.
	if err = tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("setting mode on %q: %w", name, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", name, err)
	}
	if err = os.Rename(name, dest); err != nil {
		return fmt.Errorf("renaming %q to %q: %w", name, dest, err)
	}
	return nil
}

// emitInventorySummary prints the tally, leading with what every session pays.
//
// Installed and enabled plugin agents are both printed, and never collapsed
// into one number. The gap between them is large and counting the wrong one
// overstates what actually loads several times over.
func emitInventorySummary(w io.Writer, inv inventory.Inventory) {
	s := inv.Summary
	fmt.Fprintf(w, "\n%d agents load in every session: %d global",
		s.AgentsLoadedGlobally, s.GlobalAgents)
	if s.PluginAgentsEnabled > 0 {
		fmt.Fprintf(w, " + %d from %d enabled plugin(s)",
			s.PluginAgentsEnabled, s.PluginsWithAgentsEnabled)
	}
	fmt.Fprintln(w)

	// "installed" here means what the install registry records, not what sits
	// in the plugin cache — a cache directory can outlive its install, and
	// reporting those files as installed is how this line would go back to
	// overstating what a session costs.
	if s.PluginAgentsInstalled != s.PluginAgentsEnabled {
		fmt.Fprintf(w, "%d plugin agents are installed but not all enabled; %d actually load.\n",
			s.PluginAgentsInstalled, s.PluginAgentsEnabled)
	}
	fmt.Fprintf(w, "%d project agents across %d projects, loading only in their own project.\n",
		s.ProjectAgents, s.Projects)
	if s.AtRisk > 0 {
		fmt.Fprintf(w, "%d project agents are at risk — run `agentsync scan --at-risk`.\n", s.AtRisk)
	}
}
