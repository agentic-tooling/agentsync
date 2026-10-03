package scan

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/testfs"
)

// fixture builds a throwaway agent repo plus a project tree and returns the
// resolved repo, the repo root, and the project's .claude/agents directory.
func fixture(t *testing.T) (*Repo, string, string) {
	t.Helper()

	base := t.TempDir()
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdir(t, filepath.Join(repoRoot, "agents"))
	mustMkdir(t, filepath.Join(repoRoot, "project-agents"))

	agentsDir := filepath.Join(base, "myproject", ".claude", "agents")
	mustMkdir(t, agentsDir)

	repo, err := NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	return repo, repo.Root, agentsDir
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func classify(t *testing.T, repo *Repo, path string) Entry {
	t.Helper()
	e, err := repo.Classify("myproject", path)
	if err != nil {
		t.Fatalf("Classify(%s): %v", path, err)
	}
	return e
}

// TestClassify_AllStates covers every state the scanner can produce.
func TestClassify_AllStates(t *testing.T) {
	t.Run("healthy link", func(t *testing.T) {
		repo, root, agentsDir := fixture(t)
		target := filepath.Join(root, "agents", "golang.md")
		mustWrite(t, target, "global golang agent")
		link := filepath.Join(agentsDir, "golang.md")
		mustSymlink(t, target, link)

		e := classify(t, repo, link)
		if e.State != StateHealthyLink {
			t.Errorf("got %v, want %v", e.State, StateHealthyLink)
		}
		if e.Hash != "" {
			t.Errorf("symlink entries must not carry a content hash, got %q", e.Hash)
		}
	})

	t.Run("broken link into repo", func(t *testing.T) {
		repo, root, agentsDir := fixture(t)
		// Target inside the repo that was never created — the agent was
		// renamed or deleted upstream.
		target := filepath.Join(root, "agents", "removed.md")
		link := filepath.Join(agentsDir, "removed.md")
		mustSymlink(t, target, link)

		e := classify(t, repo, link)
		if e.State != StateBrokenLink {
			t.Errorf("got %v, want %v", e.State, StateBrokenLink)
		}
	})

	t.Run("foreign link", func(t *testing.T) {
		repo, _, agentsDir := fixture(t)
		outside := filepath.Join(t.TempDir(), "somewhere.md")
		mustWrite(t, outside, "not ours")
		link := filepath.Join(agentsDir, "somewhere.md")
		mustSymlink(t, outside, link)

		e := classify(t, repo, link)
		if e.State != StateForeignLink {
			t.Errorf("got %v, want %v", e.State, StateForeignLink)
		}
	})

	t.Run("dangling link outside repo is foreign not broken", func(t *testing.T) {
		repo, _, agentsDir := fixture(t)
		link := filepath.Join(agentsDir, "ghost.md")
		mustSymlink(t, filepath.Join(t.TempDir(), "never-created.md"), link)

		e := classify(t, repo, link)
		if e.State != StateForeignLink {
			t.Errorf("got %v, want %v", e.State, StateForeignLink)
		}
	})

	t.Run("adoptable: identical content", func(t *testing.T) {
		repo, root, agentsDir := fixture(t)
		const body = "identical agent body\n"
		mustWrite(t, filepath.Join(root, "agents", "golang.md"), body)
		local := filepath.Join(agentsDir, "golang.md")
		mustWrite(t, local, body)

		e := classify(t, repo, local)
		if e.State != StateAdoptable {
			t.Errorf("got %v, want %v", e.State, StateAdoptable)
		}
		if e.Counterpart == "" {
			t.Error("adoptable entry must record its counterpart")
		}
	})

	t.Run("override: differing content", func(t *testing.T) {
		repo, root, agentsDir := fixture(t)
		mustWrite(t, filepath.Join(root, "agents", "researcher.md"), "global version\n")
		local := filepath.Join(agentsDir, "researcher.md")
		mustWrite(t, local, "heavily customized local version\n")

		e := classify(t, repo, local)
		if e.State != StateOverride {
			t.Errorf("got %v, want %v", e.State, StateOverride)
		}
		if !e.State.AtRisk() {
			t.Error("an override holds unique content and must count as at-risk")
		}
	})

	t.Run("unadopted: no counterpart", func(t *testing.T) {
		repo, _, agentsDir := fixture(t)
		local := filepath.Join(agentsDir, "bespoke.md")
		mustWrite(t, local, "exists only here\n")

		e := classify(t, repo, local)
		if e.State != StateUnadopted {
			t.Errorf("got %v, want %v", e.State, StateUnadopted)
		}
		if !e.State.AtRisk() {
			t.Error("an unadopted file exists nowhere else and must count as at-risk")
		}
		if e.Counterpart != "" {
			t.Errorf("unadopted entry must have no counterpart, got %q", e.Counterpart)
		}
	})
}

// TestClassify_LstatNotStat is the regression guard for the single most
// dangerous mistake in this package.
//
// os.Stat follows symlinks. If the classifier used Stat, a healthy symlink
// would be reported as a regular file, its content would hash equal to the
// target's, and it would be classified adoptable — telling a caller it is safe
// to "replace" a link that is already correct. Worse, if the link pointed at a
// project-agents override, a naive caller could relink it to the global agent
// and destroy the customization.
func TestClassify_LstatNotStat(t *testing.T) {
	repo, root, agentsDir := fixture(t)
	const body = "same bytes on both sides\n"

	target := filepath.Join(root, "agents", "golang.md")
	mustWrite(t, target, body)
	link := filepath.Join(agentsDir, "golang.md")
	mustSymlink(t, target, link)

	// Sanity: Stat and Lstat genuinely disagree about this path, so the
	// test is exercising the distinction rather than a tautology.
	st, err := os.Stat(link)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !st.Mode().IsRegular() {
		t.Fatal("precondition failed: Stat should report the symlink as a regular file")
	}
	lst, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if lst.Mode()&os.ModeSymlink == 0 {
		t.Fatal("precondition failed: Lstat should report the path as a symlink")
	}

	e := classify(t, repo, link)
	if e.State == StateAdoptable {
		t.Fatal("classifier followed the symlink: a healthy link was reported adoptable")
	}
	if e.State != StateHealthyLink {
		t.Errorf("got %v, want %v", e.State, StateHealthyLink)
	}
}

// TestCounterpart_ProjectAgentsWinsOverGlobal locks in the precedence rule.
// A project-specific copy represents the adopted version of that project's
// file, so it must be compared against before the global pool.
func TestCounterpart_ProjectAgentsWinsOverGlobal(t *testing.T) {
	repo, root, agentsDir := fixture(t)

	mustWrite(t, filepath.Join(root, "agents", "researcher.md"), "global version\n")
	mustWrite(t, filepath.Join(root, "project-agents", "myproject", "researcher.md"), "project version\n")

	local := filepath.Join(agentsDir, "researcher.md")
	mustWrite(t, local, "project version\n")

	e := classify(t, repo, local)
	if e.State != StateAdoptable {
		t.Errorf("got %v, want %v (should match the project-agents copy, not global)", e.State, StateAdoptable)
	}
	wantCounterpart := filepath.Join(root, "project-agents", "myproject", "researcher.md")
	if e.Counterpart != wantCounterpart {
		t.Errorf("counterpart = %q, want %q", e.Counterpart, wantCounterpart)
	}
}

// TestClassify_RelativeSymlink ensures links written relative to their own
// directory resolve correctly.
func TestClassify_RelativeSymlink(t *testing.T) {
	repo, root, agentsDir := fixture(t)
	target := filepath.Join(root, "agents", "golang.md")
	mustWrite(t, target, "body\n")

	rel, err := filepath.Rel(agentsDir, target)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	link := filepath.Join(agentsDir, "golang.md")
	mustSymlink(t, rel, link)

	e := classify(t, repo, link)
	if e.State != StateHealthyLink {
		t.Errorf("got %v, want %v (relative link into repo)", e.State, StateHealthyLink)
	}
}

func TestFindAgentDirs(t *testing.T) {
	root := t.TempDir()

	want := []string{
		filepath.Join(root, "shallow", ".claude", "agents"),
		filepath.Join(root, "a", "b", "c", "deep", ".claude", "agents"),
	}
	for _, d := range want {
		mustMkdir(t, d)
	}

	// A directory merely named "agents" without a .claude parent.
	mustMkdir(t, filepath.Join(root, "app", "agents"))

	// Every skipDirs entry must be pruned, not just node_modules. A vendored
	// or built copy of someone else's agent dir is not this user's project
	// and must never be reported as one.
	for name := range skipDirs {
		mustMkdir(t, filepath.Join(root, "app", name, "pkg", ".claude", "agents"))
	}

	got, problems, err := FindAgentDirs(root)
	if err != nil {
		t.Fatalf("FindAgentDirs: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("unexpected problems on a clean tree: %v", problems)
	}
	if len(got) != len(want) {
		t.Fatalf("found %d dirs %v, want %d %v", len(got), got, len(want), want)
	}

	found := make(map[string]bool, len(got))
	for _, g := range got {
		found[g] = true
	}
	for _, w := range want {
		if !found[w] {
			t.Errorf("missing expected dir %s", w)
		}
	}
}

// TestFindAgentDirs_UnreadableRootIsFatal locks in the one condition that is
// still a hard error: if the walk root cannot be read, nothing was scanned and
// reporting "no at-risk files" would be a lie.
func TestFindAgentDirs_UnreadableRootIsFatal(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, _, err := FindAgentDirs(missing); err == nil {
		t.Fatal("FindAgentDirs on a missing root must return an error")
	}
}

// TestFindAgentDirs_UnreadableSubtreeIsAProblem guards the distinction between
// "this subtree is empty" and "this subtree could not be read".
//
// The unreadable subtree is built structurally rather than with chmod 000
// (issue #11). Permission bits do not restrict root's walk, so the chmod form
// of this test skipped on the CI gate, which runs as root — silently, since
// `go test` prints nothing for a skip. testfs.UnreadableSubtree fails for root
// exactly as it does for anyone else; see that package for why, and for the
// alternatives that do not.
func TestFindAgentDirs_UnreadableSubtreeIsAProblem(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "ok", ".claude", "agents"))

	locked := filepath.Join(root, "locked")
	mustMkdir(t, locked)
	unreadable, inside := testfs.UnreadableSubtree(t, locked)
	testfs.PlantAgentsDir(t, inside, "proj")

	got, problems, err := FindAgentDirs(root)
	if err != nil {
		t.Fatalf("an unreadable subtree must not abort the walk: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("dirs = %v, want the one readable agents dir", got)
	}
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one: an unreadable subtree must be reported, not silently skipped", problems)
	}
	if problems[0].Path != unreadable {
		t.Errorf("problem %v does not name the unreadable subtree", problems[0])
	}
}

// TestStrings pins the labels down. They are what a human reads in the report,
// so a mislabelled state is a mis-told story about what is safe.
func TestStrings(t *testing.T) {
	states := map[State]string{
		StateHealthyLink: "healthy-link",
		StateBrokenLink:  "broken-link",
		StateForeignLink: "foreign-link",
		StateAdoptable:   "adoptable",
		StateOverride:    "override",
		StateUnadopted:   "unadopted",
		StateInvalid:     "invalid",
		State(99):        "invalid",
	}
	for st, want := range states {
		if got := st.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", int(st), got, want)
		}
	}

	tracked := map[Tracked]string{
		TrackedYes:     "tracked",
		TrackedNo:      "untracked",
		TrackedUnknown: "unknown",
		Tracked(99):    "unknown",
	}
	for tr, want := range tracked {
		if got := tr.String(); got != want {
			t.Errorf("Tracked(%d).String() = %q, want %q", int(tr), got, want)
		}
	}

	e := Entry{Project: "proj", Name: "agent", State: StateOverride}
	if got, want := e.String(), "proj/agent: override"; got != want {
		t.Errorf("Entry.String() = %q, want %q", got, want)
	}

	p := ScanProblem{Path: "/x/y.md", Err: "symlink loop"}
	if got, want := p.String(), "/x/y.md: symlink loop"; got != want {
		t.Errorf("ScanProblem.String() = %q, want %q", got, want)
	}
}

// --- project identity -------------------------------------------------------

// homedFixture builds a throwaway agent repo inside a throwaway home, so that
// project keys are predictable without touching the real $HOME.
//
// The returned home is already resolved, matching what NewRepo would have
// stored: on macOS t.TempDir() sits under /var, which is itself a symlink, so
// an unresolved home would never line up with a resolved project path.
func homedFixture(t *testing.T) (repo *Repo, root, home string) {
	t.Helper()

	home = resolvePath(t.TempDir())
	repoRoot := filepath.Join(home, "agents-repo")
	mustMkdir(t, filepath.Join(repoRoot, "agents"))
	mustMkdir(t, filepath.Join(repoRoot, "project-agents"))

	repo, err := NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	repo.Home = home
	return repo, repo.Root, home
}

func agentsDirFor(home string, parts ...string) string {
	return filepath.Join(append(append([]string{home}, parts...), ".claude", "agents")...)
}

// byProject indexes a scan result by project key. Two entries sharing a key is
// itself the bug under test, so it fails rather than overwriting.
func byProject(t *testing.T, res Result) map[string]Entry {
	t.Helper()
	out := make(map[string]Entry, len(res.Entries))
	for _, e := range res.Entries {
		if prev, dup := out[e.Project]; dup {
			t.Fatalf("two projects share the key %q: %s and %s", e.Project, prev.Path, e.Path)
		}
		out[e.Project] = e
	}
	return out
}

func TestProjectKey(t *testing.T) {
	const home = "/home/u"

	cases := []struct {
		name      string
		agentsDir string
		home      string
		want      string
	}{
		{
			name:      "project directly under home",
			agentsDir: "/home/u/Web/example/.claude/agents",
			home:      home,
			want:      "Web/example",
		},
		{
			name:      "project nested one level deeper",
			agentsDir: "/home/u/Web/monorepo/nested-fixture/.claude/agents",
			home:      home,
			want:      "Web/monorepo/nested-fixture",
		},
		{
			name:      "outside home",
			agentsDir: "/opt/thing/.claude/agents",
			home:      home,
			want:      "_outside-home/opt/thing",
		},
		{
			name:      "sibling of home is outside home, not a parent escape",
			agentsDir: "/home/other/proj/.claude/agents",
			home:      home,
			want:      "_outside-home/home/other/proj",
		},
		{
			name:      "home itself",
			agentsDir: "/home/u/.claude/agents",
			home:      home,
			want:      "_home",
		},
		{
			name:      "unknown home falls back to the absolute form",
			agentsDir: "/home/u/Web/example/.claude/agents",
			home:      "",
			want:      "_outside-home/home/u/Web/example",
		},
		{
			// A project sitting where the reserved namespace lives is
			// pushed into the absolute form, so it can never be
			// confused with a genuine outside-home project.
			name:      "home-relative path in the reserved namespace is escaped",
			agentsDir: "/home/u/_outside-home/proj/.claude/agents",
			home:      home,
			want:      "_outside-home/home/u/_outside-home/proj",
		},
		{
			name:      "home-relative path named _home is escaped",
			agentsDir: "/home/u/_home/.claude/agents",
			home:      home,
			want:      "_outside-home/home/u/_home",
		},
		{
			name:      "trailing separators and dot segments are cleaned away",
			agentsDir: "/home/u/Web/./example//.claude/agents/",
			home:      home,
			want:      "Web/example",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProjectKey(tc.home, tc.agentsDir)
			if got != tc.want {
				t.Errorf("ProjectKey(%q, %q) = %q, want %q", tc.home, tc.agentsDir, got, tc.want)
			}
			// Deterministic: the same inputs must key the same way
			// every time, or a counterpart written on one run is
			// invisible on the next.
			if again := ProjectKey(tc.home, tc.agentsDir); again != got {
				t.Errorf("ProjectKey is not deterministic: %q then %q", got, again)
			}
		})
	}
}

// TestProjectKey_SameLeafDifferentParents is the P1 regression guard.
//
// The old scheme took the bare basename of the project directory, so every
// project whose leaf directory shares a name collapsed to one key — one
// counterpart directory, and under import one backup file that the second
// project would overwrite.
func TestProjectKey_SameLeafDifferentParents(t *testing.T) {
	const home = "/home/u"

	a := ProjectKey(home, "/home/u/Web/mono-a/packages/api/.claude/agents")
	b := ProjectKey(home, "/home/u/Web/mono-b/packages/api/.claude/agents")

	if a == b {
		t.Fatalf("two distinct projects share the key %q; one would overwrite the other's backup", a)
	}
	if filepath.Base(a) != "api" || filepath.Base(b) != "api" {
		t.Errorf("keys should still end in the readable project name: %q, %q", a, b)
	}
}

// TestScan_SameLeafNameProjectsDoNotCrossContaminate proves the consequence of
// P1 end to end: with a bare-basename key, mono-b's agent is compared against
// mono-a's backup. Both failure modes it produces are checked here.
func TestScan_SameLeafNameProjectsDoNotCrossContaminate(t *testing.T) {
	t.Run("differing content is not an override against another project's baseline", func(t *testing.T) {
		repo, root, home := homedFixture(t)

		aAgents := agentsDirFor(home, "Web", "mono-a", "packages", "api")
		bAgents := agentsDirFor(home, "Web", "mono-b", "packages", "api")
		mustWrite(t, filepath.Join(aAgents, "reviewer.md"), "mono-a reviewer\n")
		mustWrite(t, filepath.Join(bAgents, "reviewer.md"), "mono-b reviewer\n")

		// Only mono-a has been captured in the repo.
		aKey := "Web/mono-a/packages/api"
		aBackup := filepath.Join(root, "project-agents", aKey, "reviewer.md")
		mustWrite(t, aBackup, "mono-a reviewer\n")

		res, err := repo.Scan(home)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}

		entries := byProject(t, res)
		a, ok := entries[aKey]
		if !ok {
			t.Fatalf("no entry keyed %q; got %v", aKey, keysOf(entries))
		}
		if a.State != StateAdoptable {
			t.Errorf("mono-a: got %v, want %v (its own backup is byte-identical)", a.State, StateAdoptable)
		}
		if a.Counterpart != aBackup {
			t.Errorf("mono-a counterpart = %q, want its own backup %q", a.Counterpart, aBackup)
		}

		b, ok := entries["Web/mono-b/packages/api"]
		if !ok {
			t.Fatalf("no entry keyed %q; got %v", "Web/mono-b/packages/api", keysOf(entries))
		}
		if b.State != StateUnadopted {
			t.Errorf("mono-b: got %v, want %v — it has no backup of its own, and mono-a's is not its baseline",
				b.State, StateUnadopted)
		}
		if b.Counterpart != "" {
			t.Errorf("mono-b was compared against %q, which belongs to a different project", b.Counterpart)
		}
		if !b.AtRisk() {
			t.Error("mono-b's agent is captured nowhere and must be reported at risk")
		}
	})

	t.Run("identical content is not adoptable against another project's backup", func(t *testing.T) {
		// The more dangerous half: if the two projects' files happen to
		// match, the bare-basename scheme called mono-b adoptable —
		// claiming it is safe to replace with a symlink pointing at a
		// different project's agent.
		repo, root, home := homedFixture(t)

		const body = "coincidentally identical\n"
		mustWrite(t, filepath.Join(agentsDirFor(home, "Web", "mono-a", "packages", "api"), "reviewer.md"), body)
		mustWrite(t, filepath.Join(agentsDirFor(home, "Web", "mono-b", "packages", "api"), "reviewer.md"), body)
		mustWrite(t, filepath.Join(root, "project-agents", "Web/mono-a/packages/api", "reviewer.md"), body)

		res, err := repo.Scan(home)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}

		b, ok := byProject(t, res)["Web/mono-b/packages/api"]
		if !ok {
			t.Fatal("mono-b produced no entry")
		}
		if b.State == StateAdoptable {
			t.Fatalf("mono-b reported adoptable against %q — a different project's agent", b.Counterpart)
		}
		if b.State != StateUnadopted {
			t.Errorf("mono-b: got %v, want %v", b.State, StateUnadopted)
		}
	})
}

func keysOf(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestProjectKey_IndependentOfScanRoot is the subtle constraint. A key derived
// relative to --root would rename the same project depending on where the scan
// started — ~/Web gives "example", ~/Web/monorepo gives "nested-fixture" —
// and counterpart lookup would find nothing on the second invocation.
func TestProjectKey_IndependentOfScanRoot(t *testing.T) {
	repo, root, home := homedFixture(t)

	project := filepath.Join(home, "Web", "monorepo", "nested-fixture")
	mustWrite(t, filepath.Join(project, ".claude", "agents", "senior-gin-dev.md"), "gin agent\n")

	const wantKey = "Web/monorepo/nested-fixture"
	backup := filepath.Join(root, "project-agents", wantKey, "senior-gin-dev.md")
	mustWrite(t, backup, "gin agent\n")

	roots := []string{
		home,
		filepath.Join(home, "Web"),
		filepath.Join(home, "Web", "monorepo"),
		project,
	}
	for _, scanRoot := range roots {
		t.Run(scanRoot, func(t *testing.T) {
			res, err := repo.Scan(scanRoot)
			if err != nil {
				t.Fatalf("Scan(%s): %v", scanRoot, err)
			}
			if len(res.Entries) != 1 {
				t.Fatalf("entries = %v, want exactly the one agent", res.Entries)
			}
			e := res.Entries[0]
			if e.Project != wantKey {
				t.Errorf("scanning from %s keyed the project %q, want %q", scanRoot, e.Project, wantKey)
			}
			if e.State != StateAdoptable || e.Counterpart != backup {
				t.Errorf("counterpart lookup broke when scanning from %s: state=%v counterpart=%q",
					scanRoot, e.State, e.Counterpart)
			}
		})
	}
}

// TestProjectKey_OutsideHome covers projects on an external volume or under
// /opt, which have no home-relative form at all.
func TestProjectKey_OutsideHome(t *testing.T) {
	repo, root, _ := homedFixture(t)

	// A tree that is deliberately NOT under the fixture's home.
	outside := resolvePath(t.TempDir())
	project := filepath.Join(outside, "thing")
	mustWrite(t, filepath.Join(project, ".claude", "agents", "bespoke.md"), "outside agent\n")

	wantKey := path.Join(keyOutsideHome, strings.TrimPrefix(filepath.ToSlash(project), "/"))
	if got := ProjectKey(repo.Home, filepath.Join(project, ".claude", "agents")); got != wantKey {
		t.Fatalf("ProjectKey = %q, want %q", got, wantKey)
	}
	if strings.HasPrefix(wantKey, "/") || !fs.ValidPath(wantKey) {
		t.Errorf("key %q is not usable as a relative path under project-agents/", wantKey)
	}

	// It is a usable directory name: a counterpart stored under it is found.
	backup := filepath.Join(root, "project-agents", wantKey, "bespoke.md")
	mustWrite(t, backup, "outside agent\n")

	res, err := repo.Scan(outside)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("entries = %v, want one", res.Entries)
	}
	if got := res.Entries[0]; got.Project != wantKey || got.State != StateAdoptable {
		t.Errorf("entry = %+v, want project %q and state %v", got, wantKey, StateAdoptable)
	}

	// Two distinct outside projects must not collide, including the
	// same-leaf-name case that P1 was about.
	a := ProjectKey(repo.Home, "/opt/one/api/.claude/agents")
	b := ProjectKey(repo.Home, "/opt/two/api/.claude/agents")
	if a == b {
		t.Errorf("distinct outside-home projects share the key %q", a)
	}
}

// TestProjectKey_AwkwardDirectoryNames checks that keys stay usable as real
// directories. Every component comes from a name the filesystem already
// accepted, so the only character the key itself introduces is "/".
func TestProjectKey_AwkwardDirectoryNames(t *testing.T) {
	names := []string{
		"my project",           // spaces
		"v1.2.3",               // dots
		".hidden-project",      // leading dot
		"projet-café",          // unicode
		"проект",               // non-latin
		"emoji 🚀 dir",          // multi-byte, wide
		"name-with-дash–types", // en dash next to a hyphen
		"trailing.space ",      // trailing space
		"under_score",
	}

	repo, root, home := homedFixture(t)
	seen := make(map[string]string, len(names))

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			agentsDir := agentsDirFor(home, "Web", name)
			key := ProjectKey(home, agentsDir)

			if want := "Web/" + name; key != want {
				t.Fatalf("ProjectKey = %q, want %q", key, want)
			}
			if again := ProjectKey(home, agentsDir); again != key {
				t.Errorf("not deterministic: %q then %q", key, again)
			}
			if prev, dup := seen[key]; dup {
				t.Fatalf("key %q already produced by %q", key, prev)
			}
			seen[key] = name
			if !fs.ValidPath(key) {
				t.Errorf("key %q is not a valid slash-separated path", key)
			}

			// The real proof of filesystem-safety: the key round
			// trips through an actual directory and a counterpart
			// stored under it is found again.
			mustWrite(t, filepath.Join(agentsDir, "agent.md"), "body\n")
			backup := filepath.Join(root, "project-agents", key, "agent.md")
			mustWrite(t, backup, "body\n")

			if got := repo.Counterpart(key, "agent"); got != backup {
				t.Errorf("Counterpart(%q) = %q, want %q", key, got, backup)
			}
			e := classifyAs(t, repo, key, filepath.Join(agentsDir, "agent.md"))
			if e.State != StateAdoptable {
				t.Errorf("got %v, want %v for a project named %q", e.State, StateAdoptable, name)
			}
		})
	}
}

func classifyAs(t *testing.T, repo *Repo, project, path string) Entry {
	t.Helper()
	e, err := repo.Classify(project, path)
	if err != nil {
		t.Fatalf("Classify(%s): %v", path, err)
	}
	return e
}

// TestNewRepo_SetsHome guards the wiring: a Repo built the normal way must be
// able to key projects, or every scan would silently fall into the
// absolute-path namespace and match no existing counterpart.
func TestNewRepo_SetsHome(t *testing.T) {
	repo, err := NewRepo(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory in this environment")
	}
	if repo.Home != resolvePath(home) {
		t.Errorf("Repo.Home = %q, want the resolved home %q", repo.Home, resolvePath(home))
	}
}

// TestScan_ExcludesRepoOwnDirs guards against the scanner treating the agent
// repo's own trees as a project to classify.
func TestScan_ExcludesRepoOwnDirs(t *testing.T) {
	base := t.TempDir()
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdir(t, filepath.Join(repoRoot, "agents"))

	// An agents dir belonging to the repo itself.
	mustMkdir(t, filepath.Join(repoRoot, ".claude", "agents"))
	mustWrite(t, filepath.Join(repoRoot, ".claude", "agents", "self.md"), "repo's own\n")

	// A genuine project.
	mustWrite(t, filepath.Join(base, "proj", ".claude", "agents", "real.md"), "a project agent\n")

	repo, err := NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	res, err := repo.Scan(base)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for _, e := range res.Entries {
		if e.Name == "self" {
			t.Error("scan classified the repo's own .claude/agents directory")
		}
	}
	if len(res.Entries) != 1 || res.Entries[0].Name != "real" {
		t.Errorf("entries = %v, want exactly the project agent", res.Entries)
	}
}

func TestResult_CountsAndAtRisk(t *testing.T) {
	repo, root, agentsDir := fixture(t)

	mustWrite(t, filepath.Join(root, "agents", "same.md"), "x\n")
	mustWrite(t, filepath.Join(agentsDir, "same.md"), "x\n") // adoptable

	mustWrite(t, filepath.Join(root, "agents", "diff.md"), "a\n")
	mustWrite(t, filepath.Join(agentsDir, "diff.md"), "b\n") // override

	mustWrite(t, filepath.Join(agentsDir, "only-here.md"), "z\n") // unadopted

	res, err := repo.Scan(filepath.Dir(filepath.Dir(filepath.Dir(agentsDir))))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	counts := res.Counts()
	if counts[StateAdoptable] != 1 || counts[StateOverride] != 1 || counts[StateUnadopted] != 1 {
		t.Errorf("counts = %v, want one of each", counts)
	}
	if got := len(res.AtRisk()); got != 2 {
		t.Errorf("AtRisk = %d, want 2 (override + unadopted)", got)
	}
}

// TestEntry_AtRisk_ConsidersGitTracking locks in the distinction between "the
// agent repo has no copy" and "nothing anywhere protects this file".
func TestEntry_AtRisk_ConsidersGitTracking(t *testing.T) {
	cases := []struct {
		name    string
		state   State
		tracked Tracked
		want    bool
	}{
		{"unadopted and untracked is at risk", StateUnadopted, TrackedNo, true},
		{"unadopted but tracked by its own repo is safe", StateUnadopted, TrackedYes, false},
		{"override and untracked is at risk", StateOverride, TrackedNo, true},
		{"override but tracked by its own repo is safe", StateOverride, TrackedYes, false},
		{"unknown tracking never downgrades a warning", StateUnadopted, TrackedUnknown, true},
		{"adoptable is never at risk", StateAdoptable, TrackedNo, false},
		{"healthy link is never at risk", StateHealthyLink, TrackedNo, false},
		{"foreign link is never at risk", StateForeignLink, TrackedNo, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Entry{State: tc.state, Tracked: tc.tracked}
			if got := e.AtRisk(); got != tc.want {
				t.Errorf("AtRisk() = %v, want %v (state=%v tracked=%v)",
					got, tc.want, tc.state, tc.tracked)
			}
		})
	}
}

// TestCounterpart covers the lookup in isolation, including the case where only
// the global pool has the name — the path that decides whether a file counts as
// unadopted (unique, must never be overwritten) or merely an override.
func TestCounterpart(t *testing.T) {
	t.Run("global only", func(t *testing.T) {
		repo, root, _ := fixture(t)
		global := filepath.Join(root, "agents", "solo.md")
		mustWrite(t, global, "global\n")

		if got := repo.Counterpart("myproject", "solo"); got != global {
			t.Errorf("Counterpart = %q, want the global agent %q", got, global)
		}
	})

	t.Run("project copy wins", func(t *testing.T) {
		repo, root, _ := fixture(t)
		mustWrite(t, filepath.Join(root, "agents", "dual.md"), "global\n")
		projectCopy := filepath.Join(root, "project-agents", "myproject", "dual.md")
		mustWrite(t, projectCopy, "project\n")

		if got := repo.Counterpart("myproject", "dual"); got != projectCopy {
			t.Errorf("Counterpart = %q, want the project copy %q", got, projectCopy)
		}
	})

	t.Run("another project's copy does not count", func(t *testing.T) {
		repo, root, _ := fixture(t)
		mustWrite(t, filepath.Join(root, "project-agents", "otherproject", "theirs.md"), "theirs\n")

		if got := repo.Counterpart("myproject", "theirs"); got != "" {
			t.Errorf("Counterpart = %q, want \"\" (belongs to a different project)", got)
		}
	})

	t.Run("nothing anywhere", func(t *testing.T) {
		repo, _, _ := fixture(t)
		if got := repo.Counterpart("myproject", "nobody"); got != "" {
			t.Errorf("Counterpart = %q, want \"\"", got)
		}
	})

	t.Run("a directory is not a counterpart", func(t *testing.T) {
		repo, root, _ := fixture(t)
		// A directory named like an agent must not be treated as one: it
		// cannot be hashed, and calling it a counterpart would turn a
		// unique local file into an "override" that looks comparable.
		mustMkdir(t, filepath.Join(root, "agents", "dirlike.md"))

		if got := repo.Counterpart("myproject", "dirlike"); got != "" {
			t.Errorf("Counterpart = %q, want \"\" for a directory", got)
		}
	})
}

// TestClassify_LinkTargetIsAlwaysUnresolved locks in the field's meaning. It
// used to hold the resolved path for healthy links and the unresolved one for
// foreign links, so no caller could rely on it.
func TestClassify_LinkTargetIsAlwaysUnresolved(t *testing.T) {
	repo, root, agentsDir := fixture(t)

	// real.md is the actual file; alias.md is a link to it inside the repo.
	// The project links to the alias, so resolved and unresolved differ.
	real := filepath.Join(root, "agents", "real.md")
	mustWrite(t, real, "body\n")
	alias := filepath.Join(root, "agents", "alias.md")
	mustSymlink(t, real, alias)

	link := filepath.Join(agentsDir, "alias.md")
	mustSymlink(t, alias, link)

	e := classify(t, repo, link)
	if e.State != StateHealthyLink {
		t.Fatalf("got %v, want %v", e.State, StateHealthyLink)
	}
	if e.LinkTarget != alias {
		t.Errorf("LinkTarget = %q, want the unresolved target %q", e.LinkTarget, alias)
	}
	if e.LinkResolved != real {
		t.Errorf("LinkResolved = %q, want the fully resolved path %q", e.LinkResolved, real)
	}
}

func TestClassify_BrokenLinkKeepsUnresolvedTargetOnly(t *testing.T) {
	repo, root, agentsDir := fixture(t)
	target := filepath.Join(root, "agents", "gone.md")
	link := filepath.Join(agentsDir, "gone.md")
	mustSymlink(t, target, link)

	e := classify(t, repo, link)
	if e.State != StateBrokenLink {
		t.Fatalf("got %v, want %v", e.State, StateBrokenLink)
	}
	if e.LinkTarget != target {
		t.Errorf("LinkTarget = %q, want %q", e.LinkTarget, target)
	}
	if e.LinkResolved != "" {
		t.Errorf("LinkResolved = %q, want \"\" for a link that does not resolve", e.LinkResolved)
	}
}

// TestClassify_DanglingLinkWithMissingParent covers the case where not just the
// target but the directory holding it is gone — the parent cannot be resolved
// either, so ownership has to fall back to the raw path.
func TestClassify_DanglingLinkWithMissingParent(t *testing.T) {
	t.Run("into the repo is broken", func(t *testing.T) {
		repo, root, agentsDir := fixture(t)
		target := filepath.Join(root, "agents", "never-made", "deeper", "ghost.md")
		link := filepath.Join(agentsDir, "ghost.md")
		mustSymlink(t, target, link)

		e := classify(t, repo, link)
		if e.State != StateBrokenLink {
			t.Errorf("got %v, want %v (target is inside the repo)", e.State, StateBrokenLink)
		}
	})

	t.Run("outside the repo is foreign", func(t *testing.T) {
		repo, _, agentsDir := fixture(t)
		target := filepath.Join(t.TempDir(), "never-made", "deeper", "ghost.md")
		link := filepath.Join(agentsDir, "ghost.md")
		mustSymlink(t, target, link)

		e := classify(t, repo, link)
		if e.State != StateForeignLink {
			t.Errorf("got %v, want %v (target is outside the repo)", e.State, StateForeignLink)
		}
	})
}

// TestClassify_SymlinkLoop must report an error rather than hanging or panicking.
func TestClassify_SymlinkLoop(t *testing.T) {
	repo, _, agentsDir := fixture(t)
	a := filepath.Join(agentsDir, "a.md")
	b := filepath.Join(agentsDir, "b.md")
	mustSymlink(t, b, a)
	mustSymlink(t, a, b)

	if _, err := repo.Classify("myproject", a); err == nil {
		t.Fatal("a symlink loop must produce an error, not a classification")
	}
}

// TestClassify_NonRegularEntry rejects anything that is neither a regular file
// nor a symlink. Hashing a directory is impossible, and guessing a state for it
// would be worse than saying so.
func TestClassify_NonRegularEntry(t *testing.T) {
	repo, _, agentsDir := fixture(t)
	dir := filepath.Join(agentsDir, "notafile.md")
	mustMkdir(t, dir)

	if _, err := repo.Classify("myproject", dir); err == nil {
		t.Fatal("a directory named *.md must produce an error, not a classification")
	}
}

// TestScan_OneBadEntryDoesNotBlindTheRest is the regression guard for the abort
// bug: Scan used to return on the first Classify failure, so a single weird
// file in one project hid every unprotected override in every other project.
func TestScan_OneBadEntryDoesNotBlindTheRest(t *testing.T) {
	base := t.TempDir()
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdir(t, filepath.Join(repoRoot, "agents"))

	// Project A sorts first and contains the unclassifiable entries.
	aAgents := filepath.Join(base, "aaa-broken", ".claude", "agents")
	mustMkdir(t, filepath.Join(aAgents, "adirectory.md"))
	loopA := filepath.Join(aAgents, "loop-a.md")
	loopB := filepath.Join(aAgents, "loop-b.md")
	mustSymlink(t, loopB, loopA)
	mustSymlink(t, loopA, loopB)
	mustWrite(t, filepath.Join(aAgents, "fine.md"), "also fine\n")

	// Project B sorts later and holds the file we must not lose sight of.
	bAgents := filepath.Join(base, "zzz-victim", ".claude", "agents")
	mustWrite(t, filepath.Join(bAgents, "precious.md"), "exists only here\n")

	repo, err := NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	res, err := repo.Scan(base)
	if err != nil {
		t.Fatalf("Scan must not fail because of unclassifiable entries: %v", err)
	}

	names := make(map[string]State)
	for _, e := range res.Entries {
		names[e.Name] = e.State
	}
	if _, ok := names["fine"]; !ok {
		t.Error("a good file in the same directory as a bad one was dropped")
	}
	if got, ok := names["precious"]; !ok || got != StateUnadopted {
		t.Errorf("the unprotected file in a later project was not classified: %v (present=%v)", got, ok)
	}
	if len(res.AtRisk()) == 0 {
		t.Error("Scan reported no at-risk files while an unadopted file exists")
	}

	// The bad entries must be surfaced, never silently swallowed.
	if len(res.Problems) < 3 {
		t.Errorf("problems = %v, want the directory plus both loop links", res.Problems)
	}
	var sawDir, sawLoop bool
	for _, p := range res.Problems {
		if strings.HasSuffix(p.Path, "adirectory.md") {
			sawDir = true
		}
		if strings.Contains(p.Path, "loop-") {
			sawLoop = true
		}
	}
	if !sawDir {
		t.Error("the non-regular entry was not reported as a problem")
	}
	if !sawLoop {
		t.Error("the symlink loop was not reported as a problem")
	}
}

// TestScan_ResultSlicesAreNonNil keeps the JSON form emitting [] rather than
// null, which a consumer would otherwise have to special-case.
func TestScan_ResultSlicesAreNonNil(t *testing.T) {
	base := t.TempDir()
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdir(t, filepath.Join(repoRoot, "agents"))

	repo, err := NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	res, err := repo.Scan(base)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Entries == nil || res.Dirs == nil || res.Problems == nil {
		t.Errorf("Result slices must be non-nil: entries=%v dirs=%v problems=%v",
			res.Entries, res.Dirs, res.Problems)
	}
	if res.AtRisk() == nil {
		t.Error("AtRisk must return a non-nil slice")
	}
}

// TestProjectDir_IsTheInverseOfProjectKey. The two must not drift: a key that
// ProjectDir sends somewhere ProjectKey would not name is a key pointing at the
// wrong project's files, which is what the whole keying scheme exists to
// prevent.
func TestProjectDir_IsTheInverseOfProjectKey(t *testing.T) {
	home := t.TempDir()

	for _, dir := range []string{
		filepath.Join(home, "Web", "example"),
		filepath.Join(home, "Web", "monorepo", "nested-fixture"),
		home,
	} {
		mustMkdir(t, dir)
		key := ProjectKey(home, filepath.Join(dir, ".claude", "agents"))
		got, err := ProjectDir(home, key)
		if err != nil {
			t.Fatalf("ProjectDir(%q): %v", key, err)
		}
		if got != resolvePath(dir) {
			t.Errorf("%q -> %q, want %q", key, got, resolvePath(dir))
		}
	}

	// The outside-home namespace round-trips through the absolute path it was
	// built from, which is the one form that is not relative to home.
	t.Run("outside home", func(t *testing.T) {
		got, err := ProjectDir(home, keyOutsideHome+"/opt/thing")
		if err != nil {
			t.Fatalf("ProjectDir: %v", err)
		}
		if want := filepath.Join(string(filepath.Separator), "opt", "thing"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestProjectDir_RejectsKeysThatAreNotKeys. A key is always relative and always
// stays inside project-agents/. Turning anything else into a path would let a
// caller address a directory the keying scheme never named — and link writes
// symlinks into whatever it is handed.
func TestProjectDir_RejectsKeysThatAreNotKeys(t *testing.T) {
	home := t.TempDir()

	for _, key := range []string{
		"",
		".",
		"..",
		"../escape",
		"a/../../escape",
		string(filepath.Separator) + "absolute",
		keyOutsideHome,
		keyHomeItself + "/with-a-suffix",
	} {
		if got, err := ProjectDir(home, key); err == nil {
			t.Errorf("ProjectDir(%q) = %q, want an error", key, got)
		}
	}

	// A home-relative key with no home to resolve against is unanswerable
	// rather than a guess at the filesystem root.
	if got, err := ProjectDir("", "Web/example"); err == nil {
		t.Errorf("ProjectDir with no home = %q, want an error", got)
	}
}

// TestKeyDependsOnHome: which keys a wrong home can misplace, and which it
// provably cannot. doctor draws an inference about the home from keys that
// resolve to nothing, and that inference is only sound for the first group.
func TestKeyDependsOnHome(t *testing.T) {
	cases := map[string]bool{
		"Web/example":                 true,
		"Web/monorepo/nested-fixture": true,
		"_home":                       true,
		"_outside-home/opt/thing":     false,
		"_outside-home":               false,

		// A project directory literally named like the reserved prefix is
		// still keyed relative to home — ProjectKey pushes a real
		// $HOME/_outside-home/... project into the absolute-path form
		// precisely so the two namespaces cannot overlap.
		"_outside-home-ish/thing": true,
	}
	for key, want := range cases {
		if got := KeyDependsOnHome(key); got != want {
			t.Errorf("KeyDependsOnHome(%q) = %v, want %v", key, got, want)
		}
	}
}

// The property that makes the function worth having: for every key it reports
// as home-independent, ProjectDir returns the same directory whatever home it
// is given.
func TestKeyDependsOnHome_MatchesProjectDir(t *testing.T) {
	for _, key := range []string{"Web/example", "_home", "_outside-home/opt/thing"} {
		a, errA := ProjectDir("/home/one", key)
		b, errB := ProjectDir("/home/two", key)
		if errA != nil || errB != nil {
			t.Fatalf("ProjectDir(%q): %v / %v", key, errA, errB)
		}
		sameEverywhere := a == b
		if dependsOnHome := KeyDependsOnHome(key); dependsOnHome == sameEverywhere {
			t.Errorf("KeyDependsOnHome(%q) = %v, but ProjectDir gave %q and %q",
				key, dependsOnHome, a, b)
		}
	}
}

// TestScan_UnreadableAgentsDirIsNotAnEmptyOne is issue #12's shape with no git
// in it, and the reason the audit was told to look past git at all.
//
// Scan asked filepath.Glob for the *.md files in an agents directory. Glob's
// contract is explicit that it ignores filesystem errors and can return only
// ErrBadPattern, so a directory it cannot read produces (nil, nil) — the same
// value, to the byte, that an empty directory produces. One return value
// carrying both "there is nothing here" and "I could not look", and a caller
// that has to pick a branch picking the definite one.
//
// The walk cannot cover for it. FindAgentDirs identifies .claude/agents from
// its parent's readdir and returns fs.SkipDir, so it never opens the directory
// and never sees the failure — which is exactly why the existing unreadable-
// subtree tests, which fail higher up the walk, all passed while this hole was
// open.
//
// The result was a scan reporting the project in Result.Dirs, with zero
// entries and zero problems: a clean, uncaveated "nothing here is at risk"
// about a directory it never opened. That is the one claim this tool must
// never make, and it is the claim FindAgentDirs' own doc comment promises it
// does not make.
func TestScan_UnreadableAgentsDirIsNotAnEmptyOne(t *testing.T) {
	root := t.TempDir()
	agents := testfs.UnreadableAgentsDir(t, root)

	repo, err := NewRepo(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}

	res, err := repo.Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// The premise: the walk did reach the directory and did identify it.
	// If it stopped earlier, this test is covering the walk's error branch
	// instead of the one it aims at, and would pass for the wrong reason.
	var found bool
	for _, d := range res.Dirs {
		if d == agents {
			found = true
		}
	}
	if !found {
		t.Fatalf("the walk did not identify %s as an agents directory, so this test is not exercising the read that follows it; Dirs=%v Problems=%v",
			agents, res.Dirs, res.Problems)
	}

	// The directory holds an unprotected override. A scan that cannot open
	// it must say so.
	if len(res.Problems) == 0 {
		t.Fatalf("Scan reported no problems over an agents directory it could not read; an unreadable directory that reports as empty is a silent \"nothing is at risk\" about a project holding an unprotected override")
	}

	var named bool
	for _, p := range res.Problems {
		if p.Path == agents {
			named = true
			if !strings.Contains(p.Err, "agents directory") {
				t.Errorf("problem for %s reads %q; it should say the directory could not be read, not something else", agents, p.Err)
			}
		}
	}
	if !named {
		t.Errorf("problems do not name %s: %v", agents, res.Problems)
	}
}

// TestScan_ReadableAgentsDirStillListsEverything is the other half of the
// guard. Replacing Glob with os.ReadDir changes how the listing is produced,
// and a fix that reports unreadable directories correctly but stops finding
// files in readable ones would trade one silent miss for another.
//
// It pins the three properties the switch had to preserve: the same *.md
// filter, the same lexical order, and the same single non-recursive level.
func TestScan_ReadableAgentsDirStillListsEverything(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agents")
	mustMkdir(t, dir)

	// Deliberately created out of order, so passing means the listing is
	// sorted rather than merely happening to match.
	for _, n := range []string{"zeta.md", "alpha.md", ".hidden.md", "middle.md"} {
		mustWrite(t, filepath.Join(dir, n), n)
	}
	// Must not appear: wrong extension, and a nested directory's contents.
	mustWrite(t, filepath.Join(dir, "notes.txt"), "x")
	mustWrite(t, filepath.Join(dir, "nested", "deep.md"), "x")

	got, err := agentFiles(dir)
	if err != nil {
		t.Fatalf("agentFiles: %v", err)
	}

	want := []string{".hidden.md", "alpha.md", "middle.md", "zeta.md"}
	if len(got) != len(want) {
		t.Fatalf("agentFiles returned %v, want %v", got, want)
	}
	for i := range want {
		if filepath.Base(got[i]) != want[i] {
			t.Errorf("agentFiles[%d] = %s, want %s (listing must stay sorted, as Glob's was)", i, filepath.Base(got[i]), want[i])
		}
	}
}

// TestAgentFiles_MissingDirectoryIsAnError pins the difference from Glob at
// the smallest possible scale: the same input Glob answered (nil, nil) for now
// produces an error.
func TestAgentFiles_MissingDirectoryIsAnError(t *testing.T) {
	if _, err := agentFiles(filepath.Join(t.TempDir(), "not-there")); err == nil {
		t.Error("agentFiles on a directory that does not exist returned no error; that is the Glob behaviour this replaced")
	}
}

// TestScan_ProjectPathIsDataNotAGlobPattern is the second instance the Glob
// call site carried, and the one that needs no unusual permissions at all —
// only a project directory whose name contains a glob metacharacter.
//
// filepath.Glob was handed filepath.Join(dir, "*.md") and interpreted the
// whole string, so metacharacters in the *project path* were expanded rather
// than matched literally. The bracket forms lost the project silently. The
// star and question forms were worse: they matched sibling directories, and
// Scan then filed another project's file under this project's key — the exact
// cross-contamination ProjectKey exists to prevent and documents as data loss,
// since the wrongly-keyed entry draws its counterpart from the wrong project's
// project-agents/ directory and an import would write it to the wrong backup
// path.
//
// Measured on the pre-fix code, this same fixture produced five entries, two
// of which attributed projA.md and projB.md to project "proj*".
//
// "[" and "]" in a project path are not hypothetical: Next.js and SvelteKit
// name route directories that way.
func TestScan_ProjectPathIsDataNotAGlobPattern(t *testing.T) {
	root := t.TempDir()

	// Two ordinary projects to be stolen from, and the metacharacter
	// projects that would do the stealing. "proj*" and "proj?" both match
	// "projA" and "projB" as patterns.
	for _, p := range []string{"projA", "projB", "proj*", "proj?", "proj[1]", "pkg[a-z]"} {
		mustWrite(t, filepath.Join(root, p, ".claude", "agents", "a.md"), "content of "+p+"\n")
	}

	repo, err := NewRepo(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	res, err := repo.Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Every project contributes exactly its own one file, and no project
	// contributes a file that lives somewhere else.
	seen := make(map[string]int)
	for _, e := range res.Entries {
		// The key ends in the project's directory name; compare on that
		// rather than the whole key, which carries the temp-dir path.
		key := path.Base(e.Project)
		seen[key]++

		wantDir := filepath.Join(root, key, ".claude", "agents")
		if gotDir := filepath.Dir(e.Path); gotDir != wantDir {
			t.Errorf("entry keyed %q has path %s, which lives under %s: a file has been filed under another project, so its counterpart and its import destination both belong to the wrong project",
				key, e.Path, filepath.Base(filepath.Dir(filepath.Dir(gotDir))))
		}
	}

	for _, p := range []string{"projA", "projB", "proj*", "proj?", "proj[1]", "pkg[a-z]"} {
		switch seen[p] {
		case 1:
			// Exactly right.
		case 0:
			t.Errorf("project %q contributed no entries; a metacharacter in its name made it invisible", p)
		default:
			t.Errorf("project %q contributed %d entries, want 1; it absorbed other projects' files", p, seen[p])
		}
	}
	if len(res.Problems) != 0 {
		t.Errorf("unexpected problems: %v", res.Problems)
	}
}
