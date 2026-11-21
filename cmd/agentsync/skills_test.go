package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/skills"
	"github.com/agentic-tooling/agentsync/internal/testfs"
)

func skillFixture(t *testing.T) (repo, project string) {
	t.Helper()
	requireGit(t)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, project = filepath.Join(home, "canonical"), filepath.Join(home, "project")
	testfs.GitRepo(t, repo, "README.md", "canonical")
	testfs.GitRepo(t, project, "README.md", "project")
	t.Setenv("HOME", home)
	t.Setenv(envRepo, repo)
	t.Setenv(envRoot, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Chdir(project)
	return repo, project
}

func runSkillCLI(t *testing.T, want int, contain string, args ...string) string {
	t.Helper()
	code, stdout, stderr := runCLI(t, args...)
	if code != want || !strings.Contains(stdout+stderr, contain) {
		t.Fatalf("%v exit=%d, want %d with %q\n%s\n%s", args, code, want, contain, stdout, stderr)
	}
	return stdout
}

func TestSkillsLinkApplyAndRepeat(t *testing.T) {
	repo, project := skillFixture(t)
	canonical := filepath.Join(repo, "catalog", "internal", "format")
	override := filepath.Join(repo, "project-skills", "project", "format")
	mustWriteT(t, filepath.Join(canonical, "SKILL.md"), "shared")
	mustWriteT(t, filepath.Join(repo, "catalog", "internal", "report", "SKILL.md"), "other")
	mustWriteT(t, filepath.Join(override, "SKILL.md"), "project-specific")
	cfg := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentsync", "config.toml")
	mustWriteT(t, cfg, "repo = '/different'\nskills_root = 'catalog/internal'\n")
	target := filepath.Join(project, ".claude", "skills", "format")

	runSkillCLI(t, exitOK, "DRY RUN", "link", "--skill", "format", "--exclude-local")
	if _, err := os.Lstat(filepath.Dir(target)); !os.IsNotExist(err) {
		t.Fatalf("dry run touched the project: %v", err)
	}
	runSkillCLI(t, exitOK, "Restart your Claude Code session", "link", "--all", "--project", "project/", "--skill", "report", "--skill", "format", "--apply", "--json", "--exclude-local")
	for name, want := range map[string]string{"format": override, "report": filepath.Join(repo, "catalog", "internal", "report")} {
		path := filepath.Join(filepath.Dir(target), name)
		if got, err := os.Readlink(path); err != nil || got != want {
			t.Fatalf("link %s = %q, %v; want %s", name, got, err, want)
		}
		testfs.Git(t, project, "check-ignore", "-q", "--", path)
	}
	runSkillCLI(t, exitOK, "already linked", "link", "--skill", "format", "--apply")
}

func TestSkillsLinkKeepsOccupiedPaths(t *testing.T) {
	repo, project := skillFixture(t)
	for _, name := range []string{"same", "damaged"} {
		mustWriteT(t, filepath.Join(repo, "skills", name, "SKILL.md"), "same")
	}
	dir := filepath.Join(project, ".claude", "skills")
	mustWriteT(t, filepath.Join(dir, "same", "SKILL.md"), "same")
	if err := os.Symlink(filepath.Join(repo, "missing"), filepath.Join(dir, "damaged")); err != nil {
		t.Fatal(err)
	}
	runSkillCLI(t, exitAtRisk, "already occupies", "link", "--skill", "same", "--skill", "damaged", "--apply")
	if info, err := os.Lstat(filepath.Join(dir, "same")); err != nil || !info.IsDir() {
		t.Fatalf("matching real directory replaced: %v, %v", info, err)
	}
	if got, err := os.Readlink(filepath.Join(dir, "damaged")); err != nil || got != filepath.Join(repo, "missing") {
		t.Fatalf("broken link replaced: %q, %v", got, err)
	}
}

func TestSkillsLinkChecksIndexAndHEAD(t *testing.T) {
	repo, project := skillFixture(t)
	for _, name := range []string{"committed", "staged", "sibling"} {
		mustWriteT(t, filepath.Join(repo, "skills", name, "SKILL.md"), name)
	}
	work := filepath.Join(project, ".claude", "skills")
	mustWriteT(t, filepath.Join(work, "committed", "SKILL.md"), "history")
	testfs.Git(t, project, "add", ".claude")
	testfs.Git(t, project, "commit", "-qm", "skill")
	testfs.Git(t, project, "rm", "-rf", "--", ".claude/skills/committed")
	mustWriteT(t, filepath.Join(work, "staged", "nested", "file"), "index")
	testfs.Git(t, project, "add", ".claude")
	if err := os.RemoveAll(filepath.Join(work, "staged")); err != nil {
		t.Fatal(err)
	}
	runSkillCLI(t, exitAtRisk, "index or HEAD", "link", "--skill", "committed", "--skill", "staged", "--exclude-local", "--apply")
	for _, name := range []string{"committed", "staged"} {
		if _, err := os.Lstat(filepath.Join(work, name)); !os.IsNotExist(err) {
			t.Fatalf("tracked path was shadowed: %s, %v", name, err)
		}
	}
	runSkillCLI(t, exitAtRisk, "git tracks the skills directory", "link", "--skill", "sibling", "--apply")
	runSkillCLI(t, exitOK, "applied", "link", "--skill", "sibling", "--exclude-local", "--apply")
	testfs.Git(t, project, "check-ignore", "-q", "--", ".claude/skills/sibling")
}

func TestSkillsImportCapturesFullTree(t *testing.T) {
	repo, project := skillFixture(t)
	source := filepath.Join(project, ".claude", "skills", "build")
	dest := filepath.Join(repo, "project-skills", "project", "build")
	mustWriteT(t, filepath.Join(source, "SKILL.md"), "local")
	mustWriteT(t, filepath.Join(source, "scripts", "go.sh"), "#!/bin/sh\n")
	mustMkdirT(t, filepath.Join(source, "empty"))
	if err := os.Chmod(filepath.Join(source, "scripts", "go.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	runSkillCLI(t, exitAtRisk, "pending", "import", "--skill", "build", "--fail-on-pending")
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("preview captured a skill: %v", err)
	}
	runSkillCLI(t, exitOK, "applied", "import", "--all", "--skill", "build", "--apply", "--json")
	for _, rel := range []string{"SKILL.md", "scripts/go.sh", "empty"} {
		original, err := os.Stat(filepath.Join(source, rel))
		if err != nil {
			t.Fatal(err)
		}
		saved, err := os.Stat(filepath.Join(dest, rel))
		if err != nil || original.Mode() != saved.Mode() {
			t.Fatalf("capture missed %s or its permissions: %v, %v", rel, saved, err)
		}
		if original.Mode().IsRegular() {
			first, errFirst := os.ReadFile(filepath.Join(source, rel))
			second, errSecond := os.ReadFile(filepath.Join(dest, rel))
			if errFirst != nil || errSecond != nil || string(first) != string(second) {
				t.Fatalf("capture changed %s: %q / %q", rel, first, second)
			}
		}
	}
	runSkillCLI(t, exitOK, "matching content", "import", "--skill", "build", "--apply")
	mustWriteT(t, filepath.Join(source, "scripts", "go.sh"), "changed")
	runSkillCLI(t, exitAtRisk, "different content", "import", "--skill", "build", "--apply")
	if data, err := os.ReadFile(filepath.Join(dest, "scripts", "go.sh")); err != nil || string(data) != "#!/bin/sh\n" {
		t.Fatalf("capture overwritten: %q, %v", data, err)
	}
}

func TestSkillsReportsDrift(t *testing.T) {
	repo, project := skillFixture(t)
	dir := filepath.Join(project, ".claude", "skills")
	for _, name := range []string{"same", "nested", "renamed", "linked"} {
		mustWriteT(t, filepath.Join(repo, "skills", name, "SKILL.md"), "test")
	}
	for _, name := range []string{"local", "same", "nested", "renamed"} {
		mustWriteT(t, filepath.Join(dir, name, "SKILL.md"), "test")
	}
	mustWriteT(t, filepath.Join(dir, "nested", "a"), "p")
	mustWriteT(t, filepath.Join(repo, "skills", "nested", "a"), "q")
	mustWriteT(t, filepath.Join(dir, "renamed", "a"), "p")
	mustWriteT(t, filepath.Join(repo, "skills", "renamed", "b"), "p")
	external, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"linked": filepath.Join(repo, "skills", "linked"), "broken": filepath.Join(repo, "missing"),
		"foreign": external, "misdirected": filepath.Join(repo, "skills", "same"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdirT(t, filepath.Join(dir, "no-manifest"))
	want := map[string]skills.State{
		"same": skills.Match, "nested": skills.Drift, "renamed": skills.Drift, "linked": skills.Linked,
		"local": skills.LocalOnly, "broken": skills.Broken, "foreign": skills.Foreign, "misdirected": skills.Drift,
	}
	for _, cmd := range []string{"scan", "doctor", "reconcile"} {
		args := []string{cmd, "--json"}
		code := exitAtRisk
		if cmd == "scan" {
			args = append(args, "--fail-on-risk")
		}
		if cmd == "reconcile" {
			args = append(args, "--apply", "--adopt", "--prune")
		}
		output := runSkillCLI(t, code, "skills", args...)
		var report skills.Report
		if err := json.Unmarshal([]byte(output), &report); err != nil {
			t.Fatal(err)
		}
		if len(report.Entries) != len(want) {
			t.Fatalf("%s: skills = %+v, want %v", cmd, report.Entries, want)
		}
		for _, row := range report.Entries {
			if row.State != want[row.Name] {
				t.Errorf("%s: %s = %s; want %s", cmd, row.Name, row.State, want[row.Name])
			}
		}
	}
	result := runSkillCLI(t, exitAtRisk, "manually", "reconcile")
	if !strings.Contains(result, "link points at a different canonical path") {
		t.Fatalf("text omitted mismatch reason: %s", result)
	}
	atRisk := runSkillCLI(t, exitOK, "local-only", "scan", "--at-risk")
	if strings.Contains(atRisk, "canonical-match") {
		t.Fatalf("at-risk report included canonical matches: %s", atRisk)
	}
	if _, err := os.Readlink(filepath.Join(dir, "broken")); err != nil {
		t.Fatalf("reconcile replaced or removed the broken link: %v", err)
	}
}

func TestSkillsRefuseGuards(t *testing.T) {
	t.Run("git error", func(t *testing.T) {
		repo, project := skillFixture(t)
		mustWriteT(t, filepath.Join(repo, "skills", "run", "SKILL.md"), "canonical")
		mustWriteT(t, filepath.Join(project, ".claude", "skills", "local", "SKILL.md"), "local")
		testfs.BreakGitfile(t, project)
		runSkillCLI(t, exitFailure, "git could not inspect", "link", "--skill", "run", "--apply", "--exclude-local")
		runSkillCLI(t, exitFailure, "git could not inspect", "import", "--skill", "local", "--apply")
	})
	t.Run("nested symlink", func(t *testing.T) {
		repo, project := skillFixture(t)
		source := filepath.Join(project, ".claude", "skills", "local")
		mustWriteT(t, filepath.Join(source, "SKILL.md"), "local")
		if err := os.Symlink(repo, filepath.Join(source, "external")); err != nil {
			t.Fatal(err)
		}
		runSkillCLI(t, exitFailure, "not a regular file or directory", "import", "--skill", "local", "--apply")
		runSkillCLI(t, exitFailure, "skill_problems", "doctor", "--json")
		runSkillCLI(t, exitAtRisk, "not a regular file or directory", "scan", "--fail-on-risk")
	})
	t.Run("outside canonical", func(t *testing.T) {
		repo, project := skillFixture(t)
		outside := filepath.Join(project, "catalog")
		mustWriteT(t, filepath.Join(outside, "shared", "run", "SKILL.md"), "outside")
		if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
			t.Fatal(err)
		}
		mustWriteT(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentsync", "config.toml"), "skills_root = 'escape/shared'\n")
		for _, cmd := range []string{"scan", "doctor", "reconcile"} {
			runSkillCLI(t, exitFailure, "must stay inside", cmd)
		}
		runSkillCLI(t, exitFailure, "must stay inside", "link", "--skill", "run", "--apply")
	})
	t.Run("project home gate", func(t *testing.T) {
		repo, _ := skillFixture(t)
		t.Setenv("HOME", t.TempDir())
		mustWriteT(t, filepath.Join(repo, "skills", "run", "SKILL.md"), "canonical")
		runSkillCLI(t, exitFailure, "refusing to write", "link", "--skill", "run", "--apply")
	})
	t.Run("cli selectors", func(t *testing.T) {
		skillFixture(t)
		runSkillCLI(t, exitFailure, "invalid --skill", "link", "--skill", "../outside")
		runSkillCLI(t, exitFailure, "invalid --skill", "import", "--skill", "../outside")
		runSkillCLI(t, exitFailure, "skill captures are write-once", "import", "--skill", "run", "--update")
		runSkillCLI(t, exitFailure, "requires a current project", "link", "--all", "--skill", "run")
		runSkillCLI(t, exitAtRisk, "was not found", "import", "--all", "--skill", "run")
		runSkillCLI(t, exitAtRisk, "no canonical skill", "link", "--skill", "run")
		mustWriteT(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentsync", "config.toml"), "skills_root = '../outside'\n")
		runSkillCLI(t, exitFailure, "skills_root must be a relative directory", "scan")
	})
}

func TestSkillsAndAgentsMayShareAName(t *testing.T) {
	repo, project := skillFixture(t)
	agent := filepath.Join(repo, "agents", "build.md")
	skill := filepath.Join(repo, "skills", "build")
	mustWriteT(t, agent, "agent")
	mustWriteT(t, filepath.Join(skill, "SKILL.md"), "skill")
	report := runSkillCLI(t, exitOK, "\"skill_actions\"", "link", "--agent", "build", "--skill", "build", "--exclude-local", "--apply", "--json")
	if !strings.Contains(report, "\"recorded\"") {
		t.Fatalf("combined link omitted agent report: %s", report)
	}
	for path, expected := range map[string]string{
		filepath.Join(project, ".claude", "agents", "build.md"): agent,
		filepath.Join(project, ".claude", "skills", "build"):    skill,
	} {
		if actual, err := os.Readlink(path); err != nil || actual != expected {
			t.Fatalf("link %s = %q, %v; want %s", path, actual, err, expected)
		}
	}
}

func TestSkillsExcludeTheUserLevelDirectory(t *testing.T) {
	repo, _ := skillFixture(t)
	home := os.Getenv("HOME")
	installed := filepath.Join(home, ".claude", "skills", "private")
	mustWriteT(t, filepath.Join(installed, "SKILL.md"), "installed")
	mustWriteT(t, filepath.Join(repo, "skills", "build", "SKILL.md"), "shared")
	output := runSkillCLI(t, exitOK, "entries", "scan", "--all", "--json", "--fail-on-risk")
	if strings.Contains(output, "private") {
		t.Fatalf("scan included the user-level skill: %s", output)
	}
	runSkillCLI(t, exitAtRisk, "was not found", "import", "--all", "--skill", "private", "--apply")
	runSkillCLI(t, exitAtRisk, "user-level skills", "link", "--all", "--project", "_home", "--skill", "build", "--apply")
	if _, err := os.Lstat(filepath.Join(filepath.Dir(installed), "build")); !os.IsNotExist(err) {
		t.Fatalf("link created a user-level skill: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(repo, "project-skills")); !os.IsNotExist(err) {
		t.Fatalf("import captured a user-level skill: %v", err)
	}
}

func TestSkillsKeepWhitespacePrefixedTrackedPaths(t *testing.T) {
	repo, monorepo := skillFixture(t)
	project := filepath.Join(monorepo, " package")
	for _, name := range []string{"staged", "committed"} {
		mustWriteT(t, filepath.Join(repo, "skills", name, "SKILL.md"), "canonical")
	}
	dir := filepath.Join(project, ".claude", "skills")
	mustWriteT(t, filepath.Join(dir, "committed", "SKILL.md"), "HEAD")
	testfs.Git(t, monorepo, "add", "--", " package")
	testfs.Git(t, monorepo, "commit", "-qm", "committed skill")
	testfs.Git(t, monorepo, "rm", "-rf", "--", " package/.claude/skills/committed")
	mustWriteT(t, filepath.Join(dir, "staged", "SKILL.md"), "index")
	testfs.Git(t, monorepo, "add", "--", " package")
	if err := os.RemoveAll(filepath.Join(dir, "staged")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	for _, name := range []string{"staged", "committed"} {
		runSkillCLI(t, exitAtRisk, "index or HEAD", "link", "--skill", name, "--exclude-local", "--apply")
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("link shadowed the tracked skill %s: %v", name, err)
		}
	}
}

func TestSkillsBrokenConfigCannotSelectDefaultCatalog(t *testing.T) {
	repo, project := skillFixture(t)
	mustWriteT(t, filepath.Join(repo, "skills", "build", "SKILL.md"), "wrong default")
	mustWriteT(t, filepath.Join(project, ".claude", "skills", "local", "SKILL.md"), "local")
	cfg := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agentsync", "config.toml")
	mustWriteT(t, cfg, "skills_root = 'custom'\nroot = = invalid\n")
	runSkillCLI(t, exitFailure, "config file", "link", "--all", "--project", "project", "--skill", "build", "--apply")
	runSkillCLI(t, exitFailure, "config file", "import", "--skill", "local", "--apply")
	runSkillCLI(t, exitFailure, "config file", "doctor", "--all")
	for _, path := range []string{filepath.Join(project, ".claude", "skills", "build"), filepath.Join(repo, "project-skills")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("broken config allowed a write at %s: %v", path, err)
		}
	}
}

func TestSkillsDanglingLinksFollowCanonicalOwnership(t *testing.T) {
	repo, project := skillFixture(t)
	dir := filepath.Join(project, ".claude", "skills")
	outside := t.TempDir()
	mustMkdirT(t, dir)
	mustMkdirT(t, filepath.Join(repo, "skills"))
	canonicalAlias := filepath.Join(project, "canonical-alias")
	escape := filepath.Join(repo, "escape")
	dangling := filepath.Join(repo, "dangling")
	for path, target := range map[string]string{
		canonicalAlias:                          filepath.Join(repo, "skills"),
		escape:                                  outside,
		dangling:                                filepath.Join(outside, "removed"),
		filepath.Join(dir, "canonical-missing"): filepath.Join(canonicalAlias, "missing", "nested"),
		filepath.Join(dir, "foreign-missing"):   filepath.Join(escape, "missing"),
		filepath.Join(dir, "foreign-dangling"):  dangling,
	} {
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	output := runSkillCLI(t, exitAtRisk, "canonical-missing", "doctor", "--json")
	var report skills.Report
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	want := map[string]skills.State{"canonical-missing": skills.Broken, "foreign-missing": skills.Foreign, "foreign-dangling": skills.Foreign}
	if len(report.Entries) != len(want) {
		t.Fatalf("wrong skills reported: %+v", report)
	}
	for _, entry := range report.Entries {
		if entry.State != want[entry.Name] {
			t.Errorf("%s = %s, want %s", entry.Name, entry.State, want[entry.Name])
		}
	}
}
