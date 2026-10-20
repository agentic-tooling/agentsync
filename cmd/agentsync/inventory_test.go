package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- fixtures ---------------------------------------------------------------

// inventoryFixture builds a fake home holding the agent repo, a project beside
// it, and the global tier at .claude/agents, and points $HOME at it.
//
// Pointing $HOME is not decoration: the global tier and the plugin cache are
// both found through it, so a fixture leaving the developer's real $HOME in
// place would report that machine's agents and give a different answer on every
// machine the suite runs on.
//
// The scan is rooted at base/Web rather than at base, because the global tier
// is a .claude/agents directory like any other and a scan rooted at the fake
// home would find it and report the home itself as a project.
func inventoryFixture(t *testing.T) (base, repoRoot, root string) {
	t.Helper()

	base = t.TempDir()
	repoRoot = filepath.Join(base, "agents-repo")
	root = filepath.Join(base, "Web")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	mustMkdirT(t, filepath.Join(repoRoot, "project-agents"))
	mustMkdirT(t, root)
	t.Setenv("HOME", base)
	return base, repoRoot, root
}

func inventoryArgs(t *testing.T, repoRoot, root string, extra ...string) []string {
	t.Helper()
	inTree(t, repoRoot, root)
	return append([]string{"inventory"}, extra...)
}

func inventoryPath(repoRoot string) string {
	return filepath.Join(repoRoot, inventoryFileName)
}

// --- writing ----------------------------------------------------------------

// TestRunInventory_EmptyHomeWarnsAndCompletes: inventory needs no home to find
// its tree, so an empty $HOME is a warning, as in scan, on the default path and
// with --all.
func TestRunInventory_EmptyHomeWarnsAndCompletes(t *testing.T) {
	base, repoRoot, root := inventoryFixture(t)
	mustWriteT(t, projectAgentPath(base, "Web/one", "solo"), "only here\n")

	for _, extra := range [][]string{nil, {"--" + allFlag}} {
		args := inventoryArgs(t, repoRoot, root, extra...)
		t.Setenv("HOME", "")
		mustRemoveAll(t, inventoryPath(repoRoot))
		code, stdout, stderr := runCLI(t, args...)
		if code != exitOK {
			t.Errorf("%v: exit = %d, want %d\n%s", extra, code, exitOK, stderr)
		}
		if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "$HOME") {
			t.Errorf("%v: an empty $HOME produced no warning naming it:\n%s", extra, stderr)
		}
		if !strings.Contains(stdout, "Root: ") || !strings.Contains(readFileT(t, inventoryPath(repoRoot)), "solo") {
			t.Errorf("%v: the run did not complete:\n%s", extra, stdout)
		}
	}
}

func TestRunInventory_WritesTheFileAndSummarizes(t *testing.T) {
	base, repoRoot, root := inventoryFixture(t)
	mustWriteT(t, projectAgentPath(base, "Web/one", "solo"), "only here\n")

	code, stdout, stderr := runCLI(t, inventoryArgs(t, repoRoot, root)...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, exitOK, stdout, stderr)
	}
	if !strings.Contains(stdout, inventoryPath(repoRoot)) {
		t.Errorf("the run does not say where it wrote:\n%s", stdout)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFileT(t, inventoryPath(repoRoot))), &doc); err != nil {
		t.Fatalf("the written file is not valid JSON: %v", err)
	}
	if _, ok := doc["schema"]; !ok {
		t.Error("the written document has no schema header")
	}
}

// The idempotence claim, verified rather than assumed. Two runs over an
// unchanged tree must leave byte-identical files — that is what makes the
// committed inventory's git history a record of real changes, and what makes
// --check a freshness test rather than a clock reading.
func TestRunInventory_RegeneratingAnUnchangedTreeChangesNothing(t *testing.T) {
	base, repoRoot, root := inventoryFixture(t)
	mustWriteT(t, projectAgentPath(base, "Web/one", "solo"), "only here\n")
	mustWriteT(t, filepath.Join(base, ".claude", "agents", "global.md"), "global\n")

	if code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root)...); code != exitOK {
		t.Fatalf("first run exit = %d: %s", code, stderr)
	}
	first := readFileT(t, inventoryPath(repoRoot))

	if code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root)...); code != exitOK {
		t.Fatalf("second run exit = %d: %s", code, stderr)
	}
	if second := readFileT(t, inventoryPath(repoRoot)); second != first {
		t.Errorf("regeneration changed the file on an unchanged tree\nfirst:\n%s\nsecond:\n%s",
			first, second)
	}
}

// --stdout leaves the file alone. Someone piping the document somewhere must
// not also silently rewrite the committed one.
func TestRunInventory_StdoutLeavesTheFileAlone(t *testing.T) {
	_, repoRoot, root := inventoryFixture(t)

	code, stdout, stderr := runCLI(t, inventoryArgs(t, repoRoot, root, "--stdout")...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d: %s", code, exitOK, stderr)
	}
	if !strings.HasPrefix(stdout, "{") {
		t.Errorf("stdout is not the bare document:\n%s", stdout)
	}
	if _, err := os.Stat(inventoryPath(repoRoot)); err == nil {
		t.Error("--stdout wrote the file as well")
	}
}

// --out redirects the write, which is what lets --check run against a candidate
// file without touching the committed one.
func TestRunInventory_OutRedirectsTheWrite(t *testing.T) {
	_, repoRoot, root := inventoryFixture(t)
	dest := filepath.Join(t.TempDir(), "nested", "elsewhere.json")

	code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root, "--out", dest)...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d: %s", code, exitOK, stderr)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("nothing at %s: %v", dest, err)
	}
	if _, err := os.Stat(inventoryPath(repoRoot)); err == nil {
		t.Error("--out wrote the default path too")
	}
}

// The command is derived and reads only. Its one write is its own file, and a
// project's agents directory must come out of a run exactly as it went in —
// this is the constraint that keeps a stale inventory merely inconvenient.
func TestRunInventory_NeverTouchesAProject(t *testing.T) {
	base, repoRoot, root := inventoryFixture(t)
	agent := projectAgentPath(base, "Web/one", "solo")
	mustWriteT(t, agent, "only here\n")
	before, err := os.Lstat(agent)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}

	if code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root)...); code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}

	if got := readFileT(t, agent); got != "only here\n" {
		t.Errorf("the project file was rewritten: %q", got)
	}
	after, err := os.Lstat(agent)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the project file was touched")
	}
}

// --- --check ----------------------------------------------------------------

// The exit codes are the CLI contract CI keys off: 0 the committed file still
// describes reality, 1 it does not (or is not there), 2 the tool could not tell.
// A stale file and a missing one deliberately share a code — both mean the same
// thing to a caller, that the committed inventory cannot be trusted and the fix
// is to regenerate.
func TestRunInventory_Check(t *testing.T) {
	t.Run("a current file exits 0", func(t *testing.T) {
		_, repoRoot, root := inventoryFixture(t)
		if code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root)...); code != exitOK {
			t.Fatalf("write exit = %d: %s", code, stderr)
		}

		code, stdout, stderr := runCLI(t, inventoryArgs(t, repoRoot, root, "--check")...)
		if code != exitOK {
			t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s",
				code, exitOK, stdout, stderr)
		}
		if !strings.Contains(stdout, "up to date") {
			t.Errorf("the run does not say the file is current:\n%s", stdout)
		}
	})

	t.Run("a stale file exits 1 and is left alone", func(t *testing.T) {
		base, repoRoot, root := inventoryFixture(t)
		if code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root)...); code != exitOK {
			t.Fatalf("write exit = %d: %s", code, stderr)
		}
		// A real change in the tree, not a hand-edit of the file: this is the
		// case --check exists for, and it must be the drift that is detected
		// rather than any tampering with the document.
		mustWriteT(t, projectAgentPath(base, "Web/new", "appeared"), "new agent\n")
		before := readFileT(t, inventoryPath(repoRoot))

		code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root, "--check")...)
		if code != exitAtRisk {
			t.Fatalf("exit = %d, want %d: %s", code, exitAtRisk, stderr)
		}
		if !strings.Contains(stderr, "out of date") {
			t.Errorf("stderr does not say why:\n%s", stderr)
		}
		// --check writes nothing. A check that repaired what it found would
		// make every CI run pass and report nothing.
		if after := readFileT(t, inventoryPath(repoRoot)); after != before {
			t.Error("--check rewrote the file it was asked only to compare")
		}
	})

	t.Run("an absent file exits 1 and says how to create it", func(t *testing.T) {
		_, repoRoot, root := inventoryFixture(t)

		code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root, "--check")...)
		if code != exitAtRisk {
			t.Fatalf("exit = %d, want %d: %s", code, exitAtRisk, stderr)
		}
		if !strings.Contains(stderr, "agentsync inventory") {
			t.Errorf("stderr does not name the command that would fix it:\n%s", stderr)
		}
	})

	t.Run("a file that cannot be read exits 2", func(t *testing.T) {
		_, repoRoot, root := inventoryFixture(t)
		// A directory where the file should be. Unreadable-by-mode would do the
		// same job but not when the suite runs as root, and this way the failure
		// is the same on every platform.
		dest := filepath.Join(t.TempDir(), "in-the-way")
		mustMkdirT(t, dest)

		code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root, "--check", "--out", dest)...)
		// 2, not 1: the tool could not tell whether the file is current, which
		// is a different answer from knowing it is stale, and CI must be able
		// to distinguish "regenerate this" from "something is wrong here".
		if code != exitFailure {
			t.Fatalf("exit = %d, want %d: %s", code, exitFailure, stderr)
		}
	})

	t.Run("--check with --stdout is refused", func(t *testing.T) {
		_, repoRoot, root := inventoryFixture(t)

		// Refused rather than resolved by precedence: silently ignoring
		// --check would make `--check --stdout` exit 0 on every run and read
		// as a passing freshness gate while checking nothing.
		code, stdout, stderr := runCLI(t, inventoryArgs(t, repoRoot, root, "--check", "--stdout")...)
		if code != exitFailure {
			t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s",
				code, exitFailure, stdout, stderr)
		}
		if !strings.Contains(stderr, "mutually exclusive") {
			t.Errorf("stderr does not explain the refusal:\n%s", stderr)
		}
	})
}

// --- wiring -----------------------------------------------------------------

// The subcommand is reachable and documented. A command that exists in the
// package but not in the switch is invisible, and one missing from the usage
// text is undiscoverable.
func TestRunInventory_IsWiredIntoTheCLI(t *testing.T) {
	_, stdout, _ := runCLI(t, "help")
	if !strings.Contains(stdout, "agentsync inventory") {
		t.Errorf("usage does not list the inventory subcommand:\n%s", stdout)
	}
	if !strings.Contains(stdout, "inventory flags:") {
		t.Errorf("usage has no inventory flags block:\n%s", stdout)
	}
	// The closing paragraph tells a reader which commands write what. inventory
	// writes a file, so a paragraph that did not mention it would be untrue.
	if !strings.Contains(stdout, "inventory also writes only into this repo") {
		t.Errorf("usage does not say what inventory writes:\n%s", stdout)
	}

	_, repoRoot, root := inventoryFixture(t)
	if code, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root, "--stdout")...); code != exitOK {
		t.Errorf("the subcommand is not reachable: exit %d: %s", code, stderr)
	}
}

func TestRunInventory_UnknownFlagExitsTwo(t *testing.T) {
	_, repoRoot, root := inventoryFixture(t)
	code, _, _ := runCLI(t, inventoryArgs(t, repoRoot, root, "--nope")...)
	if code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
}

// The warning fires on every path, including --check and --stdout, because it
// qualifies the numbers all three produce — and it goes to stderr so --stdout
// stays machine-parseable.
func TestRunInventory_WarnsWhenTheInstallRegistryIsMissing(t *testing.T) {
	base, repoRoot, root := inventoryFixture(t)
	// A populated plugin cache with no installed_plugins.json beside it: what
	// a Claude Code predating the registry looks like. Every plugin count comes
	// out zero and the document alone gives no sign of it.
	mustWriteT(t, filepath.Join(base, ".claude", "plugins", "cache",
		"market", "plug", "1.0.0", "agents", "a.md"), "plugin agent\n")

	for _, mode := range []string{"", "--stdout"} {
		args := inventoryArgs(t, repoRoot, root)
		if mode != "" {
			args = append(args, mode)
		}
		_, stdout, stderr := runCLI(t, args...)
		if !strings.Contains(stderr, "no plugin install registry") {
			t.Errorf("%q: no warning on stderr:\n%s", mode, stderr)
		}
		if strings.Contains(stdout, "no plugin install registry") {
			t.Errorf("%q: the warning leaked into stdout, which --stdout must keep parseable:\n%s",
				mode, stdout)
		}
	}
}

// And stays silent on a machine with no plugin tree at all, which is every
// other fixture in this file — a warning printed on every clean run is noise
// that trains people to ignore it.
func TestRunInventory_DoesNotWarnWithNoPluginTree(t *testing.T) {
	_, repoRoot, root := inventoryFixture(t)

	_, _, stderr := runCLI(t, inventoryArgs(t, repoRoot, root)...)
	if strings.Contains(stderr, "no plugin install registry") {
		t.Errorf("warned with no plugin tree at all:\n%s", stderr)
	}
}
