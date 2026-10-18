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

const orTestdata = "../../internal/openrouter/testdata/"

func orFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(orTestdata + name)
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
// testKey set. It counts both kinds of request.
type fakeOpenRouter struct {
	models       string
	modelsStatus int
	decisions    []string
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
	return project, repoRoot
}

func TestOptimize_Report(t *testing.T) {
	project, repoRoot := optimizeProject(t, map[string]string{
		"go-merge-reviewer.md": agentFile("go-merge-reviewer", "opus", "xhigh"),
		"README.md":            "not an agent\n",
	})
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{orFixture(t, "decision-jev.json")}}
	f.start(t)
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "low")

	code, stdout, stderr := runCLI(t, "optimize", "--root", project, "--repo", repoRoot)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, exitOK, stderr)
	}
	for _, w := range []string{
		"Repo: ~/Web/agents-repo (from --repo).",
		"Root: ~/Web/proj (from --root).",
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
	project, repoRoot := optimizeProject(t, map[string]string{"a.md": agentFile("a", "sonnet", "high")})
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{orFixture(t, "decision-clef.json")}}
	f.start(t)
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "haiku")

	code, stdout, stderr := runCLI(t, "optimize", "--json", "--advisor", "cloudflare/clef", "--root", project, "--repo", repoRoot)
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
	for _, w := range []string{"Repo: ", "Root: ", "Note: $CLAUDE_CODE_SUBAGENT_MODEL is set"} {
		if !strings.Contains(stderr, w) {
			t.Errorf("%q belongs on stderr with --json:\n%s", w, stderr)
		}
	}
}

func TestOptimize_JSONWriteFails(t *testing.T) {
	project, repoRoot := optimizeProject(t, map[string]string{"a.md": agentFile("a", "opus", "high")})
	(&fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{orFixture(t, "decision-clef.json")}}).start(t)
	var stderr bytes.Buffer
	if code := runOptimize([]string{"--json", "--root", project, "--repo", repoRoot}, failWriter{}, &stderr); code != exitFailure {
		t.Errorf("exit = %d, want %d\n%s", code, exitFailure, stderr.String())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestOptimize_UnresolvedAndSkippedExitOne(t *testing.T) {
	project, repoRoot := optimizeProject(t, map[string]string{
		"a.md": agentFile("a", "opus", "high"),
		"b.md": agentFile("b", "opus", "high"),
	})
	f := &fakeOpenRouter{models: orFixture(t, "models.json"), decisions: []string{`{"answers":{}}`}}
	f.start(t)
	t.Setenv("CLAUDE_CODE_SUBAGENT_MODEL", "haiku")

	code, stdout, stderr := runCLI(t, "optimize", "--root", project, "--repo", repoRoot)
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

	// The shared file is asked about once, under the first project linking it.
	code, stdout, stderr := runCLI(t, "optimize", "--root", web, "--repo", repoRoot)
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

	code, stdout, stderr = runCLI(t, "optimize", "--agent", "shared", "--root", web, "--repo", repoRoot)
	if code != exitOK || f.requests() != 4 || strings.Contains(stdout, "own") {
		t.Errorf("--agent shared: exit %d, %d requests in all, want 4:\n%s%s", code, f.requests(), stdout, stderr)
	}

	code, _, stderr = runCLI(t, "optimize", "--agent", "shared", "--agent", "nope", "--root", web, "--repo", repoRoot)
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
		{name: "positional", args: []string{"extra"}, code: exitFailure, stderr: `optimize takes no arguments, got "extra"`},
		{name: "empty advisor", args: []string{"--advisor", " "}, code: exitFailure, stderr: "--advisor is empty"},
		{name: "negative cap", args: []string{"--max-cost", "-1"}, code: exitFailure, stderr: "--max-cost must be"},
		{name: "infinite cap", args: []string{"--max-cost", "Inf"}, code: exitFailure, stderr: "--max-cost must be"},
		{name: "confidence above 1", args: []string{"--min-confidence", "1.5"}, code: exitFailure, stderr: "--min-confidence must be between 0 and 1"},
		{name: "no key", setup: func(t *testing.T, _ string) { t.Setenv(envOpenRouterKey, "") },
			code: exitFailure, stderr: "$OPENROUTER_API_KEY"},
		{name: "no repo", setup: func(t *testing.T, _ string) { t.Setenv(envRepo, "") },
			args: []string{"--repo", ""}, code: exitFailure, stderr: "no canonical agent repo configured"},
		{name: "root and all", args: []string{"--all"}, code: exitFailure, stderr: "mutually exclusive"},
		{name: "root missing", setup: func(t *testing.T, project string) { mustRemoveAll(t, project) },
			code: exitFailure, stderr: "walking"},
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
			project, repoRoot := optimizeProject(t, files)
			f := &fakeOpenRouter{models: orFixture(t, "models.json")}
			if tc.fake != nil {
				tc.fake(f)
			}
			f.start(t)
			if tc.setup != nil {
				tc.setup(t, project)
			}
			args := append([]string{"optimize", "--root", project, "--repo", repoRoot}, tc.args...)
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
