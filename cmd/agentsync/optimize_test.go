package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/optimize"
)

// orTestdata is made absolute before any test changes directory.
var orTestdata, _ = filepath.Abs("../../internal/openrouter/testdata")

func orFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(orTestdata, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func agentFile(name, model, effort string) string {
	return "---\nname: " + name + "\ndescription: d\nmodel: " + model + "\neffort: " + effort + "\n---\nbody\n"
}

// fakeOpenRouter serves models (status 200 unless modelsStatus says
// otherwise) and the decisions in order, and points optimize at itself with
// testKey set. It counts both kinds of request, and calls onDecide, if set,
// with each decision request's number before answering it.
type fakeOpenRouter struct {
	models       string
	modelsStatus int
	decisions    []string
	onDecide     func(n int)
	mu           sync.Mutex
	listed, sent int
}

const testKey = "sk-test-never-printed"

func assertNoKey(t *testing.T, out ...string) {
	t.Helper()
	if s := strings.Join(out, ""); strings.Contains(s, testKey) {
		t.Errorf("output leaks the key:\n%s", s)
	}
}

func (f *fakeOpenRouter) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet {
			f.listed++
			if f.modelsStatus != 0 {
				w.WriteHeader(f.modelsStatus)
			}
			_, _ = io.WriteString(w, f.models)
			return
		}
		f.sent++
		if f.onDecide != nil {
			f.onDecide(f.sent)
		}
		if f.sent > len(f.decisions) {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		_, _ = io.WriteString(w, f.decisions[f.sent-1])
	}))
	t.Cleanup(srv.Close)
	old := openrouterURL
	openrouterURL = srv.URL
	t.Cleanup(func() { openrouterURL = old })
	t.Setenv(envOpenRouterKey, testKey)
}

// requests is how many decision requests arrived.
func (f *fakeOpenRouter) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent
}

func (f *fakeOpenRouter) lists() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listed
}

// optimizeProject makes a repo and a project whose agents dir holds files,
// and returns both paths.
func optimizeProject(t *testing.T, files map[string]string) (project, repoRoot string) {
	t.Helper()
	home := mustResolveDir(t, t.TempDir())
	t.Setenv("HOME", home)
	repoRoot = filepath.Join(home, "Web", "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "agents"))
	project = filepath.Join(home, "Web", "proj")
	mustMkdirT(t, filepath.Join(project, ".claude", "agents"))
	for name, content := range files {
		mustWriteT(t, filepath.Join(project, ".claude", "agents", name), content)
	}
	inProject(t, repoRoot, project)
	return project, repoRoot
}

func TestOptimize_Report(t *testing.T) {
	optimizeProject(t, map[string]string{
		"go-merge-reviewer.md": agentFile("go-merge-reviewer", "opus", "xhigh"),
		"README.md":            "not an agent\n",
	})
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{orFixture(t, "decision-jev.json")}}
	f.start(t)
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "low")

	code, stdout, stderr := runCLI(t, "optimize")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
	}
	for _, w := range []string{
		"Repo: ~/Web/agents-repo (from $AGENTSYNC_REPO).",
		"Root: ~/Web/proj (from the current directory, which is in a project: .claude/agents) — pass --all to widen.",
		"Note: $CLAUDE_CODE_EFFORT_LEVEL is set",
		"Asked ~typesafe/jev-latest about 1 agent (cap $0.50, min confidence 0.60).",
		"AGENT              NOW           SUGGESTED              CONFIDENCE",
		"go-merge-reviewer  opus / xhigh  keep (low confidence)  sonnet 0.57 / xhigh 0.89",
		"Spent $0.000082 (reported by OpenRouter). Dry run: nothing written.",
	} {
		if !strings.Contains(stdout, w) {
			t.Errorf("report is missing %q:\n%s", w, stdout)
		}
	}
	if strings.Contains(stdout, "SUBAGENT_MODEL") {
		t.Errorf("unexpected note:\n%s", stdout)
	}
	assertNoKey(t, stdout, stderr)
	if f.requests() != 1 {
		t.Errorf("%d decision requests, want 1 (README.md is not an agent)", f.requests())
	}
}

func TestOptimize_JSON(t *testing.T) {
	optimizeProject(t, map[string]string{"a.md": agentFile("a", "sonnet", "high")})
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{orFixture(t, "decision-clef.json")}}
	f.start(t)
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "haiku")

	code, stdout, stderr := runCLI(t, "optimize", "--json", "--advisor", "cloudflare/clef")
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	var rep optimize.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if rep.Advisor != "cloudflare/clef" || len(rep.Rows) != 1 || rep.Rows[0].SuggestedModel != "opus" || rep.Spent != 0.00045312 {
		t.Errorf("report = %+v", rep)
	}
	if !reflect.DeepEqual(rep.Overrides, []string{"CLAUDE_CODE_SUBAGENT_MODEL"}) {
		t.Errorf("overrides = %v", rep.Overrides)
	}
	if strings.Contains(stdout, `"appl`) {
		t.Errorf("a dry run's JSON carries apply fields:\n%s", stdout)
	}
	for _, w := range []string{"Repo: ", "Root: ", "Note: $CLAUDE_CODE_SUBAGENT_MODEL is set"} {
		if !strings.Contains(stderr, w) {
			t.Errorf("%q belongs on stderr with --json:\n%s", w, stderr)
		}
	}
}

func TestOptimize_JSONWriteFails(t *testing.T) {
	optimizeProject(t, map[string]string{"a.md": agentFile("a", "opus", "high")})
	(&fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{orFixture(t, "decision-clef.json")}}).start(t)
	var stderr bytes.Buffer
	if code := runOptimize([]string{"--json"}, failWriter{}, &stderr); code != exitFailure {
		t.Errorf("exit = %d, want %d\n%s", code, exitFailure, stderr.String())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestOptimize_UnresolvedAndSkippedExitOne(t *testing.T) {
	optimizeProject(t, map[string]string{
		"a.md": agentFile("a", "opus", "high"),
		"b.md": agentFile("b", "opus", "high"),
	})
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{`{"answers":{}}`}}
	f.start(t)
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "haiku")

	code, stdout, stderr := runCLI(t, "optimize")
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d", code, exitAtRisk)
	}
	for _, w := range []string{
		"Asked ~typesafe/jev-latest about 1 agent, 1 skipped (cap",
		"a: unresolved: response reported no valid usage.cost",
		"b: skipped: run stopped",
		"1 agent was skipped (reasons above).",
		"Note: $CLAUDE_CODE_SUBAGENT_MODEL is set",
	} {
		if !strings.Contains(stdout, w) {
			t.Errorf("report is missing %q:\n%s", w, stdout)
		}
	}
	assertNoKey(t, stdout, stderr)
}

func TestOptimize_AgentSelectionAndDedup(t *testing.T) {
	home := mustResolveDir(t, t.TempDir())
	t.Setenv("HOME", home)
	repoRoot := filepath.Join(home, "Web", "agents-repo")
	shared := filepath.Join(repoRoot, "agents", "shared.md")
	mustWriteT(t, shared, agentFile("shared", "opus", "high"))
	for _, p := range []string{"one", "two"} {
		dir := filepath.Join(home, "Web", p, ".claude", "agents")
		mustMkdirT(t, dir)
		if err := os.Symlink(shared, filepath.Join(dir, "shared.md")); err != nil {
			t.Fatal(err)
		}
		mustWriteT(t, filepath.Join(dir, "own.md"), agentFile("own", "opus", "high"))
	}
	clef := orFixture(t, "decision-clef.json")
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{clef, clef, clef, clef}}
	f.start(t)
	web := filepath.Join(home, "Web")
	inTree(t, filepath.Join(web, "agents-repo"), web)

	// The shared file is asked about once, under the first project linking it.
	code, stdout, stderr := runCLI(t, "optimize", "--all")
	if code != exitOK {
		t.Fatalf("exit = %d\n%s", code, stderr)
	}
	for _, w := range []string{"Web/one/own ", "Web/one/shared ", "Web/two/own "} {
		if !strings.Contains(stdout, w) {
			t.Errorf("report is missing %q:\n%s", w, stdout)
		}
	}
	if f.requests() != 3 || strings.Contains(stdout, "Web/two/shared") {
		t.Errorf("%d requests, want 3 with the shared file once:\n%s", f.requests(), stdout)
	}

	code, stdout, stderr = runCLI(t, "optimize", "--agent", "shared", "--all")
	if code != exitOK || f.requests() != 4 || strings.Contains(stdout, "own") {
		t.Errorf("--agent shared: exit %d, %d requests in all, want 4:\n%s%s", code, f.requests(), stdout, stderr)
	}

	code, _, stderr = runCLI(t, "optimize", "--agent", "shared", "--agent", "nope", "--all")
	if code != exitFailure || !strings.Contains(stderr, "--agent nope: no Claude agent by that name") {
		t.Errorf("exit = %d, stderr:\n%s", code, stderr)
	}
}

func TestOptimize_RefusesBeforeAnyRequest(t *testing.T) {
	good := map[string]string{"a.md": agentFile("a", "opus", "high")}
	for _, tc := range []struct {
		name   string
		files  map[string]string
		setup  func(t *testing.T, project string)
		fake   func(f *fakeOpenRouter)
		args   []string
		code   int
		stderr string
		lists  int // models-list fetches; zero for every refusal before it
	}{
		{name: "help", args: []string{"-h"}, code: exitOK},
		{name: "unknown flag", args: []string{"--nope"}, code: exitFailure, stderr: "flag provided but not defined"},
		{name: "positional", args: []string{"extra"}, code: exitFailure, stderr: `unexpected argument "extra"; optimize takes flags only.`},
		{name: "empty advisor", args: []string{"--advisor", " "}, code: exitFailure, stderr: "--advisor is empty"},
		{name: "negative cap", args: []string{"--max-cost", "-1"}, code: exitFailure, stderr: "--max-cost must be"},
		{name: "infinite cap", args: []string{"--max-cost", "Inf"}, code: exitFailure, stderr: "--max-cost must be"},
		{name: "confidence above 1", args: []string{"--min-confidence", "1.5"}, code: exitFailure, stderr: "--min-confidence must be between 0 and 1"},
		{name: "no key", setup: func(t *testing.T, _ string) { t.Setenv(envOpenRouterKey, "") },
			code: exitFailure, stderr: "$OPENROUTER_API_KEY"},
		{name: "no repo", setup: func(t *testing.T, _ string) { t.Setenv(envRepo, "") },
			code: exitFailure, stderr: "no canonical agent repo configured"},
		{name: "all without a default tree", setup: func(t *testing.T, _ string) { t.Setenv(envRoot, "") },
			args: []string{"--all"}, code: exitFailure, stderr: "--all: no default tree of projects configured"},
		{name: "root missing", setup: func(t *testing.T, project string) { mustRemoveAll(t, project) },
			code: exitFailure, stderr: "could not resolve the working directory"},
		{name: "scan problem", files: map[string]string{"dir.md/x": "x"}, code: exitFailure, stderr: "could not read every agent under the root"},
		{name: "broken link", setup: func(t *testing.T, project string) {
			if err := os.Symlink(filepath.Join(project, "gone.md"), filepath.Join(project, ".claude", "agents", "b.md")); err != nil {
				t.Fatal(err)
			}
		}, code: exitFailure, stderr: "could not read agent"},
		{name: "models list down", fake: func(f *fakeOpenRouter) { f.modelsStatus = 500 },
			code: exitFailure, stderr: "OpenRouter models list: OpenRouter HTTP 500", lists: 1},
		{name: "advisor unpriced", args: []string{"--advisor", "typesafe/jev-router"}, code: exitFailure, stderr: "no fixed input price", lists: 1},
		{name: "no claude family", fake: func(f *fakeOpenRouter) {
			f.models = `{"data":[{"id":"~typesafe/jev-latest","pricing":{"prompt":"0.000000042"}}]}`
		}, code: exitFailure, stderr: "has no anthropic/claude-<family>-<version> model", lists: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := tc.files
			if files == nil {
				files = good
			}
			project, _ := optimizeProject(t, files)
			f := &fakeOpenRouter{models: orFixture(t, "models.json")}
			if tc.fake != nil {
				tc.fake(f)
			}
			f.start(t)
			if tc.setup != nil {
				tc.setup(t, project)
			}
			args := append([]string{"optimize"}, tc.args...)
			code, stdout, stderr := runCLI(t, args...)
			if code != tc.code || !strings.Contains(stderr, tc.stderr) {
				t.Errorf("exit = %d, want %d; stderr missing %q:\n%s", code, tc.code, tc.stderr, stderr)
			}
			if code != exitOK && stdout != "" {
				t.Errorf("a refused run printed a report:\n%s", stdout)
			}
			if f.requests() != 0 || f.lists() != tc.lists {
				t.Errorf("%d decision requests and %d models fetches; want 0 and %d", f.requests(), f.lists(), tc.lists)
			}
			assertNoKey(t, stdout, stderr)
		})
	}
}

func mustRemoveAll(t *testing.T, p string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
}

func TestOptimize_DryRunWritesNothing(t *testing.T) {
	project, _ := optimizeProject(t, map[string]string{"a.md": agentFile("a", "sonnet", "high")})
	(&fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{orFixture(t, "decision-clef.json")}}).start(t)

	code, stdout, stderr := runCLI(t, "optimize")
	if code != exitOK || !strings.Contains(stdout, "Dry run: nothing written. Run with --apply to write 1 change.\n") {
		t.Errorf("exit %d:\n%s%s", code, stdout, stderr)
	}
	if got := readFileT(t, filepath.Join(project, ".claude", "agents", "a.md")); got != agentFile("a", "sonnet", "high") {
		t.Errorf("a dry run wrote the file:\n%s", got)
	}
}

// The canonical file is written through both links, which stay links, and
// doctor and reconcile report the same tree before and after.
func TestOptimize_ApplyWritesTheCanonicalTarget(t *testing.T) {
	home := mustResolveDir(t, t.TempDir())
	t.Setenv("HOME", home)
	web := filepath.Join(home, "Web")
	inTree(t, filepath.Join(web, "agents-repo"), web)
	repoRoot := filepath.Join(web, "agents-repo")
	mustMkdirT(t, filepath.Join(repoRoot, "project-agents"))
	target := filepath.Join(repoRoot, "agents", "shared.md")
	const content = "---\nname: shared\ndescription: d\nmodel: sonnet # pinned\neffort: high\n---\nbody says model: sonnet\n"
	mustWriteT(t, target, content)
	for _, p := range []string{"one", "two"} {
		mustMkdirT(t, filepath.Join(web, p, ".claude"))
		if code, out, errOut := runCLI(t, "link", "--all", "--project", "Web/"+p, "--agent", "shared", "--apply"); code != exitOK {
			t.Fatalf("link %s: exit %d\n%s%s", p, code, out, errOut)
		}
	}
	before := map[string]string{}
	for _, cmd := range []string{"doctor", "reconcile"} {
		code, out, errOut := runCLI(t, cmd, "--all")
		if code != exitOK {
			t.Fatalf("%s before the apply: exit %d\n%s%s", cmd, code, out, errOut)
		}
		before[cmd] = out
	}
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{orFixture(t, "decision-clef.json"), orFixture(t, "decision-jev.json"), orFixture(t, "decision-clef.json")}}
	f.start(t)

	code, stdout, stderr := runCLI(t, "optimize", "--apply", "--all")
	if code != exitOK {
		t.Fatalf("exit = %d\n%s%s", code, stdout, stderr)
	}
	want := strings.Replace(content, "model: sonnet # pinned", "model: opus # pinned", 1)
	if got := readFileT(t, target); got != want {
		t.Errorf("canonical file =\n%s\nwant\n%s", got, want)
	}
	for _, p := range []string{"one", "two"} {
		assertIsLink(t, filepath.Join(web, p, ".claude", "agents", "shared.md"), target)
	}
	for _, w := range []string{
		"wrote ~/Web/agents-repo/agents/shared.md (canonical; linked into 2 projects)\n",
		"Changed files in ~/Web/agents-repo are not committed; review and commit them there.\n",
		"Wrote 1 change.\n",
	} {
		if !strings.Contains(stdout, w) {
			t.Errorf("report is missing %q:\n%s", w, stdout)
		}
	}
	for cmd, was := range before {
		if code, out, _ := runCLI(t, cmd, "--all"); code != exitOK || out != was {
			t.Errorf("%s changed after the apply: exit %d\n%s\n->\n%s", cmd, code, was, out)
		}
	}

	// Without a state file there is no link count to give.
	mustRemoveAll(t, filepath.Join(repoRoot, ".agentsync-state.json"))
	code, stdout, stderr = runCLI(t, "optimize", "--apply", "--all")
	if code != exitOK || !strings.Contains(stdout, "wrote ~/Web/agents-repo/agents/shared.md (canonical file)\n") {
		t.Errorf("exit %d:\n%s%s", code, stdout, stderr)
	}
	if got := readFileT(t, target); got != strings.Replace(want, "effort: high", "effort: xhigh", 1) {
		t.Errorf("canonical file =\n%s", got)
	}

	// An unreadable state file is warned about, not trusted for a count.
	mustWriteT(t, filepath.Join(repoRoot, ".agentsync-state.json"), "{")
	code, stdout, stderr = runCLI(t, "optimize", "--apply", "--min-confidence", "0.5", "--all")
	if code != exitOK || !strings.Contains(stdout, "(canonical file)\n") || !strings.Contains(stderr, "warning: link counts not shown") {
		t.Errorf("exit %d:\n%s%s", code, stdout, stderr)
	}
}

// Only the confident differing part of a change row is written, in place;
// keep and unresolved rows are left alone.
func TestOptimize_ApplyEditsAProjectFileInPlace(t *testing.T) {
	project, _ := optimizeProject(t, map[string]string{
		"b.md": agentFile("b", "opus", "high"),
		"c.md": agentFile("c", "opus", "low"),
	})
	dir := filepath.Join(project, ".claude", "agents")
	// Not the 0600 a temp file is created with, so a lost mode shows.
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte(agentFile("a", "sonnet", "low")), 0o750); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(dir, "a.md"))
	if err != nil || before.Mode().Perm() == 0o600 {
		t.Fatalf("a.md mode %v, %v", before.Mode(), err)
	}
	clef := orFixture(t, "decision-clef.json")
	(&fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{clef, clef}}).start(t)

	code, stdout, stderr := runCLI(t, "optimize", "--apply")
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d (c is unresolved)\n%s", code, exitAtRisk, stderr)
	}
	for name, want := range map[string]string{
		"a": agentFile("a", "opus", "low"),
		"b": agentFile("b", "opus", "high"),
		"c": agentFile("c", "opus", "low"),
	} {
		path := filepath.Join(dir, name+".md")
		if got := readFileT(t, path); got != want {
			t.Errorf("%s =\n%s\nwant\n%s", name, got, want)
		}
		assertIsRegular(t, path, "an in-place edit keeps a regular file")
	}
	if fi, err := os.Stat(filepath.Join(dir, "a.md")); err != nil || fi.Mode().Perm() != before.Mode().Perm() {
		t.Errorf("a.md mode = %v, %v; want %v kept", fi.Mode(), err, before.Mode())
	}
	if !strings.Contains(stdout, "wrote ~/Web/proj/.claude/agents/a.md\n") || !strings.Contains(stdout, "Wrote 1 change.\n") || strings.Contains(stdout, "not committed") {
		t.Errorf("report:\n%s", stdout)
	}
}

// A file changed while the advisor answered, or one Edit refuses, is not
// written; the other rows still are.
func TestOptimize_ApplyRefusals(t *testing.T) {
	block := "---\nname: b\ndescription: d\nmodel: |\n  sonnet\n---\n"
	project, _ := optimizeProject(t, map[string]string{
		"a.md": agentFile("a", "sonnet", "high"),
		"b.md": block,
		"c.md": agentFile("c", "sonnet", "high"),
	})
	dir := filepath.Join(project, ".claude", "agents")
	edited := agentFile("a", "sonnet", "high") + "edited\n"
	clef := orFixture(t, "decision-clef.json")
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{clef, clef, clef}}
	f.onDecide = func(n int) {
		if n == 1 {
			mustWriteT(t, filepath.Join(dir, "a.md"), edited)
		}
	}
	f.start(t)

	code, stdout, stderr := runCLI(t, "optimize", "--apply", "--json")
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\n%s", code, exitAtRisk, stderr)
	}
	var rep optimize.Report
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil || len(rep.Rows) != 3 {
		t.Fatalf("stdout: %v\n%s", err, stdout)
	}
	for i, w := range []string{"changed since it was read; run optimize again", "model: is not a simple one-line value", ""} {
		r := rep.Rows[i]
		if !strings.Contains(r.ApplyError, w) || (w == "") != r.Applied || (w == "") != (r.ApplyError == "") {
			t.Errorf("row %s: applied %v, apply_error %q; want error %q", r.Agent, r.Applied, r.ApplyError, w)
		}
	}
	for name, want := range map[string]string{"a": edited, "b": block, "c": agentFile("c", "opus", "high")} {
		if got := readFileT(t, filepath.Join(dir, name+".md")); got != want {
			t.Errorf("%s =\n%s\nwant\n%s", name, got, want)
		}
	}
	if !strings.Contains(stderr, "a: not written: the file changed") || !strings.Contains(stderr, "wrote ~/Web/proj/.claude/agents/c.md\n") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

func TestOptimize_ApplyWriteFailureExitsTwo(t *testing.T) {
	project, _ := optimizeProject(t, map[string]string{
		"a.md": agentFile("a", "sonnet", "high"),
		"b.md": agentFile("b", "sonnet", "high"),
	})
	dir := filepath.Join(project, ".claude", "agents")
	clef := orFixture(t, "decision-clef.json")
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{clef, clef}}
	f.onDecide = func(n int) {
		if n == 1 {
			mustRemoveAll(t, filepath.Join(dir, "a.md"))
		}
	}
	f.start(t)

	code, stdout, stderr := runCLI(t, "optimize", "--apply")
	if code != exitFailure {
		t.Errorf("exit = %d, want %d\n%s", code, exitFailure, stderr)
	}
	if got := readFileT(t, filepath.Join(dir, "b.md")); got != agentFile("b", "opus", "high") {
		t.Errorf("b was not written after a failed:\n%s", got)
	}
	if !strings.Contains(stdout, "a: not written: ") || !strings.Contains(stdout, "Wrote 1 change.\n") {
		t.Errorf("report:\n%s", stdout)
	}
}

func TestOptimize_ApplyNotesOverrideStillWins(t *testing.T) {
	_, _, settings := overrideFixture(t)
	mustWriteT(t, settings, oneOverride)
	clef := orFixture(t, "decision-clef.json")
	(&fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{clef, clef, clef, clef}}).start(t)

	code, stdout, stderr := runCLI(t, "optimize", "--apply", "--agent", "a")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "wrote ~/Web/proj/.claude/agents/a.md\nNote: Web/proj/a's project override (sonnet / -) still wins over the written frontmatter there.\n") {
		t.Errorf("no override note after the write:\n%s", stdout)
	}
	if strings.Contains(stdout, "b's project override") {
		t.Errorf("note for b, which was not written:\n%s", stdout)
	}

	// b's model is written, but its override sets only effort, so nothing is masked.
	code, stdout, stderr = runCLI(t, "optimize", "--apply")
	if code != exitOK || !strings.Contains(stdout, "wrote ~/Web/proj/.claude/agents/b.md\n") || strings.Contains(stdout, "still wins") {
		t.Errorf("exit %d:\n%s%s", code, stdout, stderr)
	}

	// a's effort is written, but its override sets only model.
	code, stdout, stderr = runCLI(t, "optimize", "--apply", "--min-confidence", "0.5", "--agent", "a")
	if code != exitOK || !strings.Contains(stdout, "wrote ~/Web/proj/.claude/agents/a.md\n") || strings.Contains(stdout, "still wins") {
		t.Errorf("exit %d:\n%s%s", code, stdout, stderr)
	}
}

// Foreign links, read-only files and dereferenced copies of agentsync links
// are refused. A project-local file is not written while the state file is
// unreadable, and that is exit 2: the question could not be answered.
func TestOptimize_ApplyRefusesWhatAgentsyncLeavesAlone(t *testing.T) {
	project, repoRoot := optimizeProject(t, nil)
	home := filepath.Dir(filepath.Dir(project))
	dir := filepath.Join(project, ".claude", "agents")
	// Linked first: link refuses a project holding files the repo has no copy of.
	mustWriteT(t, filepath.Join(repoRoot, "agents", "s.md"), agentFile("s", "sonnet", "high"))
	// A sibling whose path merely starts with the repo's is still outside it.
	sibling := filepath.Join(repoRoot+"2", "agents", "e.md")
	mustWriteT(t, sibling, agentFile("e", "sonnet", "high"))
	if code, out, errOut := runCLI(t, "link", "--project", "Web/proj", "--agent", "s", "--apply"); code != exitOK {
		t.Fatalf("link: exit %d\n%s%s", code, out, errOut)
	}
	foreign := filepath.Join(home, "dotfiles", "d.md")
	mustWriteT(t, foreign, agentFile("d", "sonnet", "high"))
	for name, target := range map[string]string{"d.md": foreign, "e.md": sibling} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	// The write bit is what is checked, so this holds whatever user runs it.
	if err := os.WriteFile(filepath.Join(dir, "r.md"), []byte(agentFile("r", "sonnet", "high")), 0o444); err != nil {
		t.Fatal(err)
	}
	mustRemoveAll(t, filepath.Join(dir, "s.md"))
	mustWriteT(t, filepath.Join(dir, "s.md"), agentFile("s", "sonnet", "high"))
	clef := orFixture(t, "decision-clef.json")
	(&fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{clef, clef, clef, clef, clef, clef, clef, clef}}).start(t)

	code, stdout, stderr := runCLI(t, "optimize", "--apply")
	if code != exitAtRisk {
		t.Errorf("exit = %d, want %d\n%s", code, exitAtRisk, stderr)
	}
	for _, w := range []string{
		"d: not written: it links outside the canonical repo",
		"e: not written: it links outside the canonical repo",
		"r: not written: the file is read-only",
		"s: not written: agentsync recorded a link here that is now a copy; run agentsync link --apply first",
		"Wrote 0 changes.",
	} {
		if !strings.Contains(stdout, w) {
			t.Errorf("report is missing %q:\n%s", w, stdout)
		}
	}
	for _, p := range []string{foreign, sibling, filepath.Join(dir, "r.md"), filepath.Join(dir, "s.md")} {
		if got := readFileT(t, p); !strings.Contains(got, "model: sonnet") {
			t.Errorf("%s was written:\n%s", p, got)
		}
	}

	mustWriteT(t, filepath.Join(repoRoot, ".agentsync-state.json"), "{")
	code, stdout, stderr = runCLI(t, "optimize", "--apply")
	if code != exitFailure || !strings.Contains(stdout, "s: not written: the state file could not be read") || strings.Contains(stderr, "warning") {
		t.Errorf("exit %d:\n%s%s", code, stdout, stderr)
	}
}
