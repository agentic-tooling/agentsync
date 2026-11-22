package scan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/testfs"
)

func TestInspectSkillGit(t *testing.T) {
	requireGit(t)
	for _, test := range []struct {
		name string
		want DirTracking
		prep func(*testing.T, string)
	}{
		{"visible", DirVisible, nil},
		{"ignored", DirIgnored, func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, ".gitignore"), ".claude/skills/\n")
		}},
		{"unborn", DirVisible, func(t *testing.T, repo string) {
			if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
				t.Fatal(err)
			}
			initGitRepo(t, repo)
		}},
		{"index", DirTracked, func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, ".claude/skills/build/script"), "index")
			testfs.Git(t, repo, "add", "--", ".claude")
		}},
		{"HEAD only", DirTracked, func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, ".claude/skills/build/script"), "committed")
			testfs.Git(t, repo, "add", "--", ".claude")
			testfs.Git(t, repo, "commit", "-qm", "skill")
			testfs.Git(t, repo, "rm", "-rf", "--", ".claude")
		}},
		{"indexed ancestor", DirTracked, func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, ".claude"), "ancestor")
			testfs.Git(t, repo, "add", "--", ".claude")
			if err := os.Remove(filepath.Join(repo, ".claude")); err != nil {
				t.Fatal(err)
			}
		}},
		{"not a prefix match", DirVisible, func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, ".claude/skills/builder"), "sibling")
			testfs.Git(t, repo, "add", "--", ".claude")
		}},
		{"case alias", DirTracked, func(t *testing.T, repo string) {
			testfs.Git(t, repo, "config", "core.ignoreCase", "true")
			mustWrite(t, filepath.Join(repo, ".CLAUDE/Skills/Build/script"), "committed")
			testfs.Git(t, repo, "add", "--", ".CLAUDE")
			testfs.Git(t, repo, "commit", "-qm", "skill")
			testfs.Git(t, repo, "rm", "-rf", "--", ".CLAUDE")
		}},
		{"unreadable config", DirUnknown, func(t *testing.T, repo string) {
			testfs.Git(t, repo, "config", "core.ignoreCase", "invalid-boolean")
		}},
		{"unreadable repository", DirUnknown, func(t *testing.T, repo string) { testfs.BreakGitfile(t, repo) }},
		{"unreadable tree", DirUnknown, func(t *testing.T, repo string) { testfs.DeleteHeadCommit(t, repo) }},
		{"unreadable refs", DirUnknown, func(t *testing.T, repo string) { testfs.CorruptPackedRefs(t, repo) }},
		{"unreadable index", DirUnknown, func(t *testing.T, repo string) {
			mustWrite(t, filepath.Join(repo, ".git", "index"), "broken")
		}},
		{"no repository", DirNoRepo, func(t *testing.T, repo string) {
			if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			testfs.GitRepo(t, repo, "README", "fixture")
			if test.prep != nil {
				test.prep(t, repo)
			}
			if got := InspectSkillPath(repo, ".claude/skills/build"); got.Tracking != test.want {
				t.Fatalf("InspectSkillPath = %s; want %s", got.Tracking, test.want)
			}
		})
	}
}

func TestInspectSkillPathPreservesGitPathWhitespace(t *testing.T) {
	requireGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo ")
	mustMkdir(t, repo)
	testfs.GitRepo(t, repo, "README", "fixture")
	project := filepath.Join(repo, " project")
	skill := ".claude/skills/build"
	rel := filepath.Join(" project", skill)
	mustWrite(t, filepath.Join(project, skill, "SKILL.md"), "skill")
	testfs.Git(t, repo, "add", "--", rel)
	if got := InspectSkillPath(project, skill); got.Tracking != DirTracked {
		t.Fatalf("indexed skill: tracking = %s, want %s", got.Tracking, DirTracked)
	}
	testfs.Git(t, repo, "commit", "-qm", "skill")
	testfs.Git(t, repo, "rm", "-r", "--cached", "--", rel)
	if got := InspectSkillPath(project, skill); got.Tracking != DirTracked {
		t.Fatalf("HEAD-only skill: tracking = %s, want %s", got.Tracking, DirTracked)
	}
}

func TestSkillExcludePathUsesCommonGitDirectory(t *testing.T) {
	requireGit(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testfs.GitRepo(t, repo, "README", "fixture")
	worktree := filepath.Join(t.TempDir(), "worktree")
	testfs.Git(t, repo, "worktree", "add", "-qb", "fixture-worktree", worktree)
	want := filepath.Join(repo, ".git", "info", "exclude")
	if got := SkillExcludePath(worktree); got != want {
		t.Fatalf("SkillExcludePath = %q; want %q", got, want)
	}
	if got := SkillExcludePath(t.TempDir()); got != "" {
		t.Fatalf("SkillExcludePath named a path without a repository: %q", got)
	}
}

func TestFindSkillDirsStopsAtSkillRoots(t *testing.T) {
	root := t.TempDir()
	skills := filepath.Join(root, "a", ".claude", "skills")
	mustWrite(t, filepath.Join(skills, "name", "fixtures", ".claude", "agents", "test.md"), "nested")
	linked := filepath.Join(root, "b", ".claude", "skills")
	mustMkdir(t, filepath.Dir(linked))
	if err := os.Symlink(skills, linked); err != nil {
		t.Fatal(err)
	}
	got, _, err := FindSkillDirs(root)
	if err != nil || len(got) != 2 || got[0] != skills || got[1] != linked {
		t.Fatalf("FindSkillDirs = %v, %v", got, err)
	}
	if got, _, err := FindAgentDirs(root); err != nil || len(got) != 0 {
		t.Fatalf("agent finder walked a skill tree: %v, %v", got, err)
	}
}
