package skills

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/testfs"
	"golang.org/x/sys/unix"
)

func skillRepo(t *testing.T, shared string) (*Catalog, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	canonical, project := filepath.Join(home, "catalog"), filepath.Join(home, "project")
	testfs.GitRepo(t, canonical, "README", "canonical")
	testfs.GitRepo(t, project, "README", "project")
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo, err := scan.NewRepo(canonical)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Open(repo, shared)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, canonical, project
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s was created or could not be checked: %v", path, err)
	}
}

func TestSkillApplyRevalidatesBytes(t *testing.T) {
	for _, mode := range []Mode{Link, Import} {
		t.Run(string(mode), func(t *testing.T) {
			c, repo, project := skillRepo(t, "")
			source, dest := filepath.Join(repo, "skills", "build"), filepath.Join(project, projectDir, "build")
			if mode == Import {
				source, dest = dest, filepath.Join(repo, "project-skills", "project", "build")
			}
			write(t, filepath.Join(source, "SKILL.md"), "skill")
			support := filepath.Join(source, "nested", "support")
			write(t, support, "before")
			plan := c.Plan(mode, []string{"project"}, []string{"build"}, Report{}, false)
			if plan[0].Status != pending {
				t.Fatalf("initial plan: %+v", plan)
			}
			write(t, support, "after")
			c.Apply(plan, false)
			if plan[0].Status != blocked || plan[0].Reason != "skill changed since planning; run again to review the new content" {
				t.Fatalf("changed tree was applied: %+v", plan)
			}
			absent(t, dest)
		})
	}
}

func TestSkillLinkChecksIndexAgainAtApply(t *testing.T) {
	c, repo, project := skillRepo(t, "")
	write(t, filepath.Join(repo, "skills", "build", "SKILL.md"), "skill")
	plan := c.Plan(Link, []string{"project"}, []string{"build"}, Report{}, true)
	if plan[0].Status != pending {
		t.Fatalf("initial plan: %+v", plan)
	}
	dest := filepath.Join(project, projectDir, "build")
	write(t, filepath.Join(dest, "tracked"), "stage after planning")
	testfs.Git(t, project, "add", "--", projectDir)
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	}
	c.Apply(plan, true)
	if plan[0].Status != blocked {
		t.Fatalf("staged path was allowed: %+v", plan)
	}
	absent(t, dest)
}

func TestSkillCapturePublicationIsExclusive(t *testing.T) {
	physical, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := openAbsolute(physical)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir("existing", 0o755); err != nil {
		t.Fatal(err)
	}
	contents := tree{{path: "SKILL.md", mode: 0o644, data: []byte("first")}}
	if err := capture(root, "existing", contents); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("empty existing destination was replaced: %v", err)
	}
	absent(t, filepath.Join(physical, "existing", "SKILL.md"))
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, text := range []string{"first", "second"} {
		wg.Go(func() { results <- capture(root, "contended", tree{{path: "SKILL.md", data: []byte(text)}}) })
	}
	wg.Wait()
	close(results)
	succeeded, occupied := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, fs.ErrExist):
			occupied++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || occupied != 1 {
		t.Fatalf("exclusive captures: successful=%d, occupied=%d", succeeded, occupied)
	}
	children, err := fs.ReadDir(root.FS(), ".")
	if err != nil || len(children) != 2 {
		t.Fatalf("temporary captures left behind: %v, %v", children, err)
	}
}

func TestSkillTreesRejectNestedSpecialFiles(t *testing.T) {
	c, repo, project := skillRepo(t, "")
	source := filepath.Join(project, projectDir, "build")
	write(t, filepath.Join(source, "SKILL.md"), "skill")
	if err := unix.Mkfifo(filepath.Join(source, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := c.Plan(Import, []string{"project"}, []string{"build"}, Report{}, false)
	c.Apply(plan, false)
	if plan[0].Status != failed {
		t.Fatalf("special file accepted: %+v", plan)
	}
	absent(t, filepath.Join(repo, "project-skills"))
}

func TestSkillPathsRefuseAncestorSymlinks(t *testing.T) {
	c, repo, project := skillRepo(t, "")
	outside := t.TempDir()
	write(t, filepath.Join(repo, "skills", "build", "SKILL.md"), "canonical")
	if err := os.Symlink(outside, filepath.Join(project, ".claude")); err != nil {
		t.Fatal(err)
	}
	plan := c.Plan(Link, []string{"project"}, []string{"build"}, Report{}, false)
	c.Apply(plan, false)
	if plan[0].Status != failed || !strings.Contains(plan[0].Reason, ".claude is not a real directory") {
		t.Fatalf("escaped project path accepted: %+v", plan)
	}
	absent(t, filepath.Join(outside, "skills"))
	if err := os.Remove(filepath.Join(project, ".claude")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(project, projectDir, "build", "SKILL.md"), "custom")
	if err := os.Symlink(outside, filepath.Join(repo, "project-skills")); err != nil {
		t.Fatal(err)
	}
	plan = c.Plan(Import, []string{"project"}, []string{"build"}, Report{}, false)
	c.Apply(plan, false)
	if plan[0].Status != failed || !strings.Contains(plan[0].Reason, "project-skills is not a real directory") {
		t.Fatalf("escaped capture path accepted: %+v", plan)
	}
	absent(t, filepath.Join(outside, "project"))
}

func TestSkillLinkConfirmsIgnoreRule(t *testing.T) {
	c, repo, project := skillRepo(t, "")
	write(t, filepath.Join(repo, "skills", "build", "SKILL.md"), "skill")
	write(t, filepath.Join(project, ".gitignore"), "!/.claude/skills/build\n")
	plan := c.Plan(Link, []string{"project"}, []string{"build"}, Report{}, true)
	if plan[0].Status != pending {
		t.Fatalf("initial plan: %+v", plan)
	}
	c.Apply(plan, true)
	if plan[0].Status != failed || plan[0].Reason != "git has not excluded the skill symlink" {
		t.Fatalf("negated ignore rule authorized link: %+v", plan)
	}
	absent(t, filepath.Join(project, projectDir, "build"))
}

func TestSkillImportKeepsMissingTrackedCanonicalCapture(t *testing.T) {
	c, repo, project := skillRepo(t, "")
	write(t, filepath.Join(project, projectDir, "build", "SKILL.md"), "custom")
	dest := filepath.Join(repo, "project-skills", "project", "build")
	write(t, filepath.Join(dest, "SKILL.md"), "captured")
	testfs.Git(t, repo, "add", "--", "project-skills")
	testfs.Git(t, repo, "commit", "-qm", "capture")
	testfs.Git(t, repo, "rm", "-rf", "--", "project-skills")
	plan := c.Plan(Import, []string{"project"}, []string{"build"}, Report{}, false)
	c.Apply(plan, false)
	if plan[0].Status != blocked {
		t.Fatalf("tracked canonical capture was replaced: %+v", plan)
	}
	absent(t, dest)
}

func TestSkillScanClassifiesProjectDirectories(t *testing.T) {
	c, repo, project := skillRepo(t, "")
	base := filepath.Join(project, projectDir)
	canonical := filepath.Join(repo, "skills", "matching")
	write(t, filepath.Join(canonical, "SKILL.md"), "skill")
	write(t, filepath.Join(base, "matching", "SKILL.md"), "skill")
	write(t, filepath.Join(base, "standalone", "SKILL.md"), "local")
	write(t, filepath.Join(base, "notes"), "ordinary file")
	write(t, filepath.Join(base, "missing-manifest", "notes"), "ordinary directory")
	write(t, filepath.Join(base, "invalid-manifest", "SKILL.md", "notes"), "directory manifest")
	for name, target := range map[string]string{
		"broken": filepath.Join(repo, "absent"), "other": t.TempDir(), "wrong-name": canonical,
	} {
		if err := os.Symlink(target, filepath.Join(base, name)); err != nil {
			t.Fatal(err)
		}
	}
	foreign := filepath.Join(filepath.Dir(project), "foreign", projectDir)
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, foreign); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, projectDir, "own-project", "SKILL.md"), "ignore canonical")
	report, err := c.Scan(filepath.Dir(project))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]State{"matching": Match, "standalone": LocalOnly, "broken": Broken, "other": Foreign, "wrong-name": Drift}
	if len(report.Entries) != len(want) || len(report.Problems) != 2 || len(report.AtRisk().Entries) != 3 {
		t.Fatalf("unexpected report: %+v", report)
	}
	for _, row := range report.Entries {
		if row.State != want[row.Name] {
			t.Errorf("%s: got %s, want %s", row.Name, row.State, want[row.Name])
		}
	}
}

func TestSkillLinkAndCaptureReportRepeatedRuns(t *testing.T) {
	c, repo, project := skillRepo(t, "")
	write(t, filepath.Join(repo, "skills", "shared", "SKILL.md"), "canonical")
	link := c.Plan(Link, []string{"project"}, []string{"shared", "shared"}, Report{}, false)
	c.Apply(link, false)
	if pending, written, blocked, failed := link.Counts(); pending != 0 || written != 1 || blocked != 0 || failed != 0 {
		t.Fatalf("link counts: %+v", link)
	}
	dest := filepath.Join(project, projectDir, "shared")
	if target, err := os.Readlink(dest); err != nil || target != link[0].Canonical {
		t.Fatalf("link = %q, %v; want %s", target, err, link[0].Canonical)
	}
	if repeat := c.Plan(Link, []string{"project"}, []string{"shared"}, Report{}, false); repeat[0].Status != match {
		t.Fatalf("repeated link: %+v", repeat)
	}
	write(t, filepath.Join(project, projectDir, "local", "SKILL.md"), "local")
	write(t, filepath.Join(project, projectDir, "local", "nested", "script"), "support")
	report, err := c.Scan(filepath.Dir(project))
	if err != nil {
		t.Fatal(err)
	}
	plan := c.Plan(Import, nil, []string{"local", "absent"}, report, false)
	if pending, _, blocked, _ := plan.Counts(); pending != 1 || blocked != 1 || len(plan.Dirs()) != 1 {
		t.Fatalf("wide import counts: %+v", plan)
	}
	c.Apply(plan, false)
	if data, err := os.ReadFile(filepath.Join(repo, "project-skills", "project", "local", "nested", "script")); err != nil || string(data) != "support" {
		t.Fatalf("captured script = %q, %v", data, err)
	}
	if repeat := c.Plan(Import, []string{"project"}, []string{"local"}, Report{}, false); repeat[0].Status != match {
		t.Fatalf("repeated import: %+v", repeat)
	}
}

func TestSkillModeAndNamesRefuseInvalidInputs(t *testing.T) {
	c, _, project := skillRepo(t, "")
	plan := c.Plan("", []string{"project"}, []string{"bad"}, Report{}, false)
	if len(plan) != 1 || plan[0].Status != failed {
		t.Fatalf("invalid mode planned an operation: %+v", plan)
	}
	plan = Plan{{directory: project, Status: pending}}
	c.Apply(plan, false)
	if plan[0].Status != failed {
		t.Fatalf("invalid mode authorized a write: %+v", plan)
	}
	for _, name := range []string{".", "../foreign", "some/name", "a\\b", "-apply"} {
		if err := ValidateNames([]string{name}); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
	if err := ValidateNames([]string{"a name", "normal"}); err != nil {
		t.Fatal(err)
	}
}

func TestSkillCapturePreparationFailureLeavesNoDestination(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := openAbsolute(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	files := tree{{path: "SKILL.md"}, {path: "absent/support"}}
	if err := capture(root, "build", files); err == nil {
		t.Fatal("failed capture published a partial directory")
	}
	if children, err := fs.ReadDir(root.FS(), "."); err != nil || len(children) != 0 {
		t.Fatalf("failed capture left files visible: %v, %v", children, err)
	}
}

func TestResolveSkillLinkTargets(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(base, "outside", "nested")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"alias": physical, "relative-dangling": "outside/removed", "loop": "loop",
	} {
		if err := os.Symlink(target, filepath.Join(base, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		target, want string
		missing      bool
	}{
		{"alias", physical, false},
		{"alias/../removed", filepath.Join(base, "outside", "removed"), true},
		{"relative-dangling/support", filepath.Join(base, "outside", "removed", "support"), true},
	} {
		got, missing, err := resolveTarget(base + "/" + test.target)
		if err != nil || got != test.want || missing != test.missing {
			t.Fatalf("%s resolved to %q, missing=%v, %v; want %q, missing=%v", test.target, got, missing, err, test.want, test.missing)
		}
	}
	if _, _, err := resolveTarget(filepath.Join(base, "loop")); err == nil {
		t.Fatal("cyclic link resolved")
	}
}
