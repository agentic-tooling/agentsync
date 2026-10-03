package scan

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// repoWithHome builds a Repo whose Home is exactly what the test says, without
// consulting the real environment.
func repoWithHome(t *testing.T, home string) *Repo {
	t.Helper()
	repo, err := NewRepo(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	repo.Home = home
	return repo
}

// agentsDirIn returns the .claude/agents path for a project directory, which
// is the unit CheckHome judges.
func agentsDirIn(projectDir string) string {
	return filepath.Join(projectDir, ".claude", "agents")
}

// TestCheckHome_Statuses is the heart of the issue: /var/root is a valid,
// readable, resolvable directory, so a validity check passes it and the wrong
// keys go unreported. Only the keys the projects actually get separate a wrong
// home from a right one.
func TestCheckHome_Statuses(t *testing.T) {
	home := resolvePath(t.TempDir())
	outside := resolvePath(t.TempDir())

	// These fixture paths need not exist on disk: home is already resolved,
	// so ProjectKey's own resolution is a no-op on paths built from it and
	// the keys come out exactly as they would for real directories.
	// TestWarnHome in cmd/agentsync does need real ones, because the home it
	// sets is unresolved — do not copy that pattern down here, or this one
	// up there.
	inHome := agentsDirIn(filepath.Join(home, "Web", "proj"))
	outOfHome := agentsDirIn(filepath.Join(outside, "work"))

	cases := []struct {
		name string
		home string
		root string
		dirs []string
		want HomeStatus
	}{
		{
			name: "projects under home",
			home: home,
			root: filepath.Join(home, "Web"),
			dirs: []string{inHome},
			want: HomeOK,
		},
		{
			// The sudo -i case, modelled with a temp dir rather than
			// the real /var/root so it does not depend on that path
			// existing or being readable.
			name: "valid home, every project outside it",
			home: home,
			root: outside,
			dirs: []string{outOfHome},
			want: HomeImplausible,
		},
		{
			// Scanning / or /Users on a correct machine finds both
			// home-relative projects and genuinely outside ones.
			// Home is doing its job; warning would be crying wolf.
			name: "some projects outside home, some inside",
			home: home,
			root: "/",
			dirs: []string{inHome, outOfHome},
			want: HomeOK,
		},
		{
			// Nothing was keyed, so nothing was mis-keyed.
			name: "no projects at all",
			home: home,
			root: outside,
			dirs: nil,
			want: HomeOK,
		},
		{
			name: "empty home",
			home: "",
			root: outside,
			dirs: []string{outOfHome},
			want: HomeUnset,
		},
		{
			name: "home that is not a directory",
			home: filepath.Join(home, "no-such-dir"),
			root: home,
			dirs: []string{inHome},
			want: HomeUnresolvable,
		},
		{
			// Every path is under "/", so the keys come out looking
			// home-relative while being absolute paths in disguise.
			name: "home is the filesystem root",
			home: "/",
			root: home,
			dirs: []string{inHome},
			want: HomeIsRoot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := repoWithHome(t, tc.home)

			got := repo.CheckHome(tc.root, tc.dirs)
			if got.Status != tc.want {
				t.Errorf("CheckHome(%q, %v) with home %q = %v (%s), want %v",
					tc.root, tc.dirs, tc.home, got.Status, got.Summary(), tc.want)
			}
			if got.OK() != (tc.want == HomeOK) {
				t.Errorf("OK() = %v for status %v", got.OK(), got.Status)
			}
		})
	}
}

// TestCheckHome_RootIsNotTheSignal is the regression guard for the hole this
// check originally had: judging the *root* instead of the projects.
//
// Under `sudo -i` with --root / or --root /Users, home sits UNDER the root, so
// any root-relative test concludes "fine" — while every project key is
// silently re-based under _outside-home/. That is precisely the invocation
// shape the issue is about, and it must warn.
func TestCheckHome_RootIsNotTheSignal(t *testing.T) {
	base := resolvePath(t.TempDir())
	realHome := filepath.Join(base, "home")
	wrongHome := filepath.Join(base, "wronghome") // stands in for /var/root
	mustMkdir(t, realHome)
	mustMkdir(t, wrongHome)

	project := agentsDirIn(filepath.Join(realHome, "Web", "proj"))
	mustMkdir(t, project)

	// The root is an ancestor of the wrong home — the case a bidirectional
	// root test wrongly passes.
	repo := repoWithHome(t, wrongHome)
	got := repo.CheckHome(base, []string{project})
	if got.Status != HomeImplausible {
		t.Fatalf("status = %v (%s), want %v: a wrong $HOME above the root is still a wrong $HOME",
			got.Status, got.Summary(), HomeImplausible)
	}

	// And the verdict matches reality: the key really did get re-based.
	if key := ProjectKey(wrongHome, project); !outsideHomeKey(key) {
		t.Errorf("ProjectKey = %q, expected it under %q/", key, keyOutsideHome)
	}

	// The same projects under the right home are silent.
	repo.Home = realHome
	if got := repo.CheckHome(base, []string{project}); !got.OK() {
		t.Errorf("correct home reported %v (%s), want ok", got.Status, got.Summary())
	}
}

// TestCheckHome_UnresolvableBeatsImplausible pins the ordering: a home that
// does not exist is reported as unresolvable rather than implausible, because
// the two have different remedies and different suppressibility.
func TestCheckHome_UnresolvableBeatsImplausible(t *testing.T) {
	repo := repoWithHome(t, filepath.Join(t.TempDir(), "gone"))
	got := repo.CheckHome(t.TempDir(), []string{agentsDirIn(t.TempDir())})
	if got.Status != HomeUnresolvable {
		t.Errorf("status = %v, want %v", got.Status, HomeUnresolvable)
	}
}

// TestCheckHome_ReportsTheEvidence guards requirement 3: a user must be able to
// diagnose a wrong $HOME from the message alone, which means the check has to
// carry the value in use and the counts that justify its verdict.
func TestCheckHome_ReportsTheEvidence(t *testing.T) {
	home := resolvePath(t.TempDir())
	root := resolvePath(t.TempDir())
	dirs := []string{
		agentsDirIn(filepath.Join(root, "one")),
		agentsDirIn(filepath.Join(root, "two")),
	}
	repo := repoWithHome(t, home)

	got := repo.CheckHome(root, dirs)
	if got.Status != HomeImplausible {
		t.Fatalf("status = %v, want %v", got.Status, HomeImplausible)
	}
	if got.Home != home {
		t.Errorf("Home = %q, want %q", got.Home, home)
	}
	if got.Root != root {
		t.Errorf("Root = %q, want %q", got.Root, root)
	}
	if got.Projects != 2 || got.OutsideHome != 2 {
		t.Errorf("Projects/OutsideHome = %d/%d, want 2/2", got.Projects, got.OutsideHome)
	}

	summary := got.Summary()
	for _, want := range []string{"$HOME", home, root, keyOutsideHome, "2 of 2"} {
		if !strings.Contains(summary, want) {
			t.Errorf("Summary() = %q, missing %q", summary, want)
		}
	}
	if !strings.Contains(got.Advice(), "--"+AllowOutsideHomeFlag) {
		t.Errorf("Advice() = %q, does not name the opt-out flag", got.Advice())
	}
	if explain := got.Explain(); !strings.Contains(explain, summary) ||
		!strings.Contains(explain, got.Advice()) {
		t.Errorf("Explain() = %q, want it to carry both the summary and the advice", explain)
	}
}

// TestCheckHome_BrokenHomesNameHome covers the statuses that do not depend on
// any project: each must name $HOME, and each must print the offending value
// where there is one.
func TestCheckHome_BrokenHomesNameHome(t *testing.T) {
	root := t.TempDir()
	dirs := []string{agentsDirIn(t.TempDir())}

	unset := repoWithHome(t, "").CheckHome(root, dirs)
	if !strings.Contains(unset.Summary(), "$HOME") {
		t.Errorf("unset summary %q does not name $HOME", unset.Summary())
	}
	if unset.Home != "" {
		t.Errorf("unset Home = %q, want empty", unset.Home)
	}

	bad := filepath.Join(t.TempDir(), "gone")
	unresolvable := repoWithHome(t, bad).CheckHome(root, dirs)
	if s := unresolvable.Summary(); !strings.Contains(s, "$HOME") || !strings.Contains(s, bad) {
		t.Errorf("unresolvable summary %q must name $HOME and print %q", s, bad)
	}

	isRoot := repoWithHome(t, "/").CheckHome(root, dirs)
	if s := isRoot.Summary(); !strings.Contains(s, "$HOME") || !strings.Contains(s, `"/"`) {
		t.Errorf("filesystem-root summary %q must name $HOME and print %q", s, "/")
	}
}

// TestCheckHome_FilesystemRootIsNotOutsideHome documents why HomeIsRoot needs
// its own status: with $HOME=/ the keys do NOT land under _outside-home/, so
// the key-counting rule alone would call this home perfectly fine.
func TestCheckHome_FilesystemRootIsNotOutsideHome(t *testing.T) {
	project := agentsDirIn(filepath.Join(resolvePath(t.TempDir()), "proj"))
	if key := ProjectKey("/", project); outsideHomeKey(key) {
		t.Fatalf("premise changed: ProjectKey with home / = %q, now under %q",
			key, keyOutsideHome)
	}
	got := repoWithHome(t, "/").CheckHome("/", []string{project})
	if got.Status != HomeIsRoot {
		t.Errorf("status = %v, want %v — a home of / must not pass as ok",
			got.Status, HomeIsRoot)
	}
}

func TestHomeCheck_OKSaysNothing(t *testing.T) {
	home := resolvePath(t.TempDir())
	got := repoWithHome(t, home).CheckHome(home, []string{agentsDirIn(filepath.Join(home, "p"))})
	if !got.OK() {
		t.Fatalf("status = %v, want %v", got.Status, HomeOK)
	}
	for name, s := range map[string]string{
		"Summary": got.Summary(),
		"Advice":  got.Advice(),
		"Explain": got.Explain(),
	} {
		if s != "" {
			t.Errorf("%s() = %q for a good home, want empty", name, s)
		}
	}
}

// TestRequireHome is the write-path guard: import and link do not exist yet,
// but the primitive they must call does, and it has to error rather than warn
// on every failing condition.
func TestRequireHome(t *testing.T) {
	home := resolvePath(t.TempDir())
	outside := resolvePath(t.TempDir())
	missing := filepath.Join(t.TempDir(), "gone")

	inHome := []string{agentsDirIn(filepath.Join(home, "Web", "proj"))}
	outOfHome := []string{agentsDirIn(filepath.Join(outside, "work"))}

	cases := []struct {
		name       string
		home       string
		dirs       []string
		allow      bool
		wantErr    bool
		wantStatus HomeStatus
	}{
		{
			name: "good home permits the write",
			home: home,
			dirs: inHome,
		},
		{
			name: "nothing to write is permitted",
			home: home,
			dirs: nil,
		},
		{
			name:       "valid home that owns none of the projects blocks the write",
			home:       home,
			dirs:       outOfHome,
			wantErr:    true,
			wantStatus: HomeImplausible,
		},
		{
			name:  "explicit opt-out permits the outside-home write",
			home:  home,
			dirs:  outOfHome,
			allow: true,
		},
		{
			name:       "unset home blocks the write",
			home:       "",
			dirs:       outOfHome,
			wantErr:    true,
			wantStatus: HomeUnset,
		},
		{
			// The opt-out means "my projects live outside $HOME",
			// not "ignore my broken environment". No correct
			// configuration has no home at all.
			name:       "opt-out does not excuse an unset home",
			home:       "",
			dirs:       outOfHome,
			allow:      true,
			wantErr:    true,
			wantStatus: HomeUnset,
		},
		{
			name:       "unresolvable home blocks the write",
			home:       missing,
			dirs:       inHome,
			wantErr:    true,
			wantStatus: HomeUnresolvable,
		},
		{
			name:       "opt-out does not excuse an unresolvable home",
			home:       missing,
			dirs:       inHome,
			allow:      true,
			wantErr:    true,
			wantStatus: HomeUnresolvable,
		},
		{
			name:       "filesystem-root home blocks the write",
			home:       "/",
			dirs:       inHome,
			wantErr:    true,
			wantStatus: HomeIsRoot,
		},
		{
			name:       "opt-out does not excuse a filesystem-root home",
			home:       "/",
			dirs:       inHome,
			allow:      true,
			wantErr:    true,
			wantStatus: HomeIsRoot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := repoWithHome(t, tc.home)

			err := repo.RequireHome(home, tc.dirs, tc.allow)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("RequireHome = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("RequireHome = nil, want an error: a write must not proceed")
			}

			var homeErr *HomeError
			if !errors.As(err, &homeErr) {
				t.Fatalf("error %v is not a *HomeError; callers cannot inspect it", err)
			}
			if homeErr.Check.Status != tc.wantStatus {
				t.Errorf("status = %v, want %v", homeErr.Check.Status, tc.wantStatus)
			}
			if !strings.Contains(err.Error(), "$HOME") {
				t.Errorf("error %q does not name $HOME as the cause", err)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("error %q spans lines; an error string should not", err)
			}
		})
	}
}

// TestRequireHome_RejectsTheSudoShape is the write-path half of the hole the
// root-based test left open. An import run as `sudo -i agentsync import
// --root /` must not be permitted just because $HOME happens to sit under /.
func TestRequireHome_RejectsTheSudoShape(t *testing.T) {
	base := resolvePath(t.TempDir())
	wrongHome := filepath.Join(base, "wronghome")
	mustMkdir(t, wrongHome)
	project := agentsDirIn(filepath.Join(base, "home", "Web", "proj"))
	mustMkdir(t, project)

	repo := repoWithHome(t, wrongHome)
	if err := repo.RequireHome(base, []string{project}, false); err == nil {
		t.Fatal("RequireHome permitted a write under a wrong $HOME that sits below the root")
	}
}

func TestHomeCheck_SuppressedBy(t *testing.T) {
	cases := []struct {
		status HomeStatus
		allow  bool
		want   bool
	}{
		{HomeImplausible, true, true},
		{HomeImplausible, false, false},
		{HomeUnset, true, false},
		{HomeUnresolvable, true, false},
		{HomeIsRoot, true, false},
		{HomeOK, true, false},
	}
	for _, tc := range cases {
		c := HomeCheck{Status: tc.status}
		if got := c.SuppressedBy(tc.allow); got != tc.want {
			t.Errorf("HomeCheck{%v}.SuppressedBy(%v) = %v, want %v",
				tc.status, tc.allow, got, tc.want)
		}
	}
}

func TestHomeStatus_String(t *testing.T) {
	cases := map[HomeStatus]string{
		HomeOK:           "ok",
		HomeUnset:        "unset",
		HomeUnresolvable: "unresolvable",
		HomeIsRoot:       "filesystem-root",
		HomeImplausible:  "implausible",
		HomeStatus(99):   "invalid",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("HomeStatus(%d).String() = %q, want %q", int(st), got, want)
		}
	}
}

// TestCheckHome_AgreesWithProjectKey ties the verdict to the thing it claims.
// The warning says keys will fall under _outside-home/; these assert that they
// do exactly when the check says so, in both directions, so the two can never
// drift into warning about a condition that does not happen — or staying quiet
// about one that does.
func TestCheckHome_AgreesWithProjectKey(t *testing.T) {
	home := resolvePath(t.TempDir())
	mustMkdir(t, home)

	cases := []struct {
		name        string
		root        string
		project     string
		wantOK      bool
		wantOutside bool
	}{
		{
			name:        "project under home",
			root:        filepath.Join(home, "Web"),
			project:     filepath.Join(home, "Web", "proj"),
			wantOK:      true,
			wantOutside: false,
		},
		{
			name:        "project outside home",
			root:        resolvePath(t.TempDir()),
			project:     filepath.Join(resolvePath(t.TempDir()), "proj"),
			wantOK:      false,
			wantOutside: true,
		},
		{
			// The case a root-based check gets wrong: home is under
			// the root, but the project is not under home.
			name:        "home under root, project outside home",
			root:        filepath.Dir(home),
			project:     filepath.Join(resolvePath(t.TempDir()), "proj"),
			wantOK:      false,
			wantOutside: true,
		},
		{
			// And its mirror: home under the root, project inside
			// home. Must stay silent.
			name:        "home under root, project inside home",
			root:        filepath.Dir(home),
			project:     filepath.Join(home, "Web", "proj"),
			wantOK:      true,
			wantOutside: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := repoWithHome(t, home)
			dir := agentsDirIn(tc.project)

			key := ProjectKey(repo.Home, dir)
			if got := outsideHomeKey(key); got != tc.wantOutside {
				t.Fatalf("ProjectKey = %q, outside-home = %v, want %v",
					key, got, tc.wantOutside)
			}
			if got := repo.CheckHome(tc.root, []string{dir}); got.OK() != tc.wantOK {
				t.Errorf("CheckHome = %v (%s), OK = %v, want OK = %v — the check disagrees with the key it predicts",
					got.Status, got.Summary(), got.OK(), tc.wantOK)
			}
		})
	}
}
