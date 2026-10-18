package discover

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/testfs"
)

const agentMD = "---\nname: x\ndescription: does x\n---\nbody\n"

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repo makes dir a git repo whose agents/ holds n agent files.
func repo(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		mustWrite(t, filepath.Join(dir, "agents", string(rune('a'+i))+".md"), agentMD)
	}
}

// mustSwap replaces link with a symlink to target.
func mustSwap(t *testing.T, target, link string) {
	t.Helper()
	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// resolvedTempDir is t.TempDir without symlinks in its path. Find walks the
// resolved root, so a PATH_MAX fixture must be measured against that path.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func tooLong(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, syscall.ENAMETOOLONG)
}

// unreadableAgentFile builds a repo under parent whose agents/ opens but whose
// one agent file's path is past PATH_MAX, so reading it fails even as root.
// Each level is made through an os.Root, the only way to create past the limit.
func unreadableAgentFile(t *testing.T, parent string) string {
	t.Helper()
	seg := strings.Repeat("d", 200)
	name := strings.Repeat("a", 250) + ".md"
	cur, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	dir := parent
	for depth := 0; !tooLong(filepath.Join(dir, seg, "agents")); depth++ {
		if depth > 64 {
			t.Fatalf("a %d-byte path is still under the path limit", len(dir))
		}
		if err := cur.Mkdir(seg, 0o755); err != nil {
			t.Fatal(err)
		}
		next, err := cur.OpenRoot(seg)
		if err != nil {
			t.Fatal(err)
		}
		_ = cur.Close()
		cur, dir = next, filepath.Join(dir, seg)
	}
	for _, d := range []string{".git", "agents"} {
		if err := cur.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := cur.WriteFile(filepath.Join("agents", name), []byte(agentMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if ents, err := os.ReadDir(filepath.Join(dir, "agents")); err != nil || len(ents) != 1 || !tooLong(filepath.Join(dir, "agents", name)) {
		t.Fatalf("fixture: agents/ must list one entry (%d, %v) whose path is too long", len(ents), err)
	}
	return dir
}

func paths(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Path)
	}
	return out
}

func TestFind_WhatCountsAsACandidate(t *testing.T) {
	root := t.TempDir()
	repo(t, filepath.Join(root, "Web", "few"), 1)
	repo(t, filepath.Join(root, "Web", "many"), 3)
	repo(t, filepath.Join(root, "b-tie"), 1)
	repo(t, filepath.Join(root, "a", "b", "c", "depth4"), 1)
	repo(t, filepath.Join(root, "a", "b", "c", "d", "depth5"), 1)
	repo(t, filepath.Join(root, ".hidden", "repo"), 1)
	repo(t, filepath.Join(root, "node_modules", "repo"), 1)
	repo(t, filepath.Join(root, "x", "vendor", "repo"), 1)
	repo(t, filepath.Join(root, "Library", "repo"), 1)
	repo(t, filepath.Join(root, "empty"), 0)
	mustWrite(t, filepath.Join(root, "nogit", "agents", "a.md"), agentMD)
	// A worktree's gitfile is not the .git/ directory a candidate needs.
	mustWrite(t, filepath.Join(root, "gitfile", ".git"), "gitdir: /elsewhere\n")
	mustWrite(t, filepath.Join(root, "gitfile", "agents", "a.md"), agentMD)
	mustWrite(t, filepath.Join(root, "agentsfile", "agents"), "")
	if err := os.MkdirAll(filepath.Join(root, "agentsfile", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlinked .git or agents/ is not the directory a candidate needs.
	elsewhere := t.TempDir()
	repo(t, elsewhere, 1)
	repo(t, filepath.Join(root, "linkedgit"), 1)
	mustSwap(t, filepath.Join(elsewhere, ".git"), filepath.Join(root, "linkedgit", ".git"))
	repo(t, filepath.Join(root, "linkedagents"), 1)
	mustSwap(t, filepath.Join(elsewhere, "agents"), filepath.Join(root, "linkedagents", "agents"))

	res := Find(root, MaxDirs)
	want := []Candidate{
		{filepath.Join(root, "Web", "many"), 3},
		{filepath.Join(root, "Web", "few"), 1},
		{filepath.Join(root, "a", "b", "c", "depth4"), 1},
		{filepath.Join(root, "b-tie"), 1},
	}
	// Sorted by count, then path: "Web" sorts before "a" in byte order.
	if !reflect.DeepEqual(res.Candidates, want) {
		t.Errorf("candidates = %v, want %v", res.Candidates, want)
	}
	if len(res.Unreadable) != 0 || res.Truncated {
		t.Errorf("unreadable = %v, truncated = %v; want neither", res.Unreadable, res.Truncated)
	}
}

func TestFind_CountsOnlyAgentFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "r")
	repo(t, dir, 1)
	agents := filepath.Join(dir, "agents")
	mustWrite(t, filepath.Join(agents, "crlf.md"), "---\r\nname: y\r\ndescription: y\r\n---\r\n")
	mustWrite(t, filepath.Join(agents, "noname.md"), "---\ndescription: y\n---\n")
	mustWrite(t, filepath.Join(agents, "nodesc.md"), "---\nname: y\n---\n")
	mustWrite(t, filepath.Join(agents, "nested.md"), "---\nmeta:\n  name: y\n  description: y\n---\n")
	mustWrite(t, filepath.Join(agents, "nofront.md"), "name: y\ndescription: y\n")
	mustWrite(t, filepath.Join(agents, "unclosed.md"), "---\nname: y\ndescription: y\n")
	mustWrite(t, filepath.Join(agents, "notes.txt"), agentMD)
	if err := os.Symlink(filepath.Join(agents, "a.md"), filepath.Join(agents, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(agents, "sub.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	res := Find(root, MaxDirs)
	if want := []Candidate{{dir, 2}}; !reflect.DeepEqual(res.Candidates, want) {
		t.Errorf("candidates = %v, want %v (a.md and crlf.md only)", res.Candidates, want)
	}
}

func TestFind_NeverFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	repo(t, filepath.Join(elsewhere, "real"), 1)
	for link, target := range map[string]string{
		"loop":    root,
		"tolink":  filepath.Join(elsewhere, "real"),
		"towalk":  elsewhere,
		"dangles": filepath.Join(root, "gone"),
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	res := Find(root, MaxDirs)
	if len(res.Candidates) != 0 || len(res.Unreadable) != 0 {
		t.Errorf("followed a symlink: candidates = %v, unreadable = %v", res.Candidates, res.Unreadable)
	}
}

func TestFind_ReportsWhatItCouldNotRead(t *testing.T) {
	base := resolvedTempDir(t)
	unreadable, inside := testfs.UnreadableSubtree(t, base)
	// A repo the search would count if it could get there.
	if err := inside.MkdirAll(filepath.Join("r", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := inside.MkdirAll(filepath.Join("r", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := inside.WriteFile(filepath.Join("r", "agents", "a.md"), []byte(agentMD), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(unreadable)
	repo(t, filepath.Join(root, "ok"), 1)

	res := Find(root, MaxDirs)
	if got := paths(res.Candidates); !reflect.DeepEqual(got, []string{filepath.Join(root, "ok")}) {
		t.Errorf("candidates = %v, want only the readable repo", got)
	}
	if len(res.Unreadable) != 1 || res.Unreadable[0].Path != unreadable || res.Unreadable[0].Err == nil {
		t.Errorf("unreadable = %v, want exactly %s with its error", res.Unreadable, unreadable)
	}
}

func TestFind_ReportsAnAgentFileItCouldNotRead(t *testing.T) {
	dir := unreadableAgentFile(t, resolvedTempDir(t))
	res := Find(dir, MaxDirs)
	if len(res.Candidates) != 0 || len(res.Unreadable) != 1 || res.Unreadable[0].Path != dir ||
		!errors.Is(res.Unreadable[0].Err, syscall.ENAMETOOLONG) {
		t.Errorf("%d candidates, %d unreadable; want the repo reported unreadable with ENAMETOOLONG",
			len(res.Candidates), len(res.Unreadable))
	}
}

func TestFind_RootThatIsNotADirectoryIsUnreadable(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	mustWrite(t, file, "")
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(base, "gone"), dangling, file} {
		res := Find(root, MaxDirs)
		if len(res.Candidates) != 0 || len(res.Unreadable) != 1 || res.Unreadable[0].Path != root || res.Unreadable[0].Err == nil {
			t.Errorf("Find(%s): candidates = %v, unreadable = %v; want only %[1]s with its error", root, res.Candidates, res.Unreadable)
		}
	}
	if res := Find(file, MaxDirs); len(res.Unreadable) == 1 && !errors.Is(res.Unreadable[0].Err, errNotDir) {
		t.Errorf("a file root: err = %v, want %v", res.Unreadable[0].Err, errNotDir)
	}
}

func TestFind_ResolvesASymlinkedRoot(t *testing.T) {
	target := t.TempDir()
	repo(t, filepath.Join(target, "Web", "agents-repo"), 1)
	root := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	res := Find(root, MaxDirs)
	if want := []Candidate{{filepath.Join(root, "Web", "agents-repo"), 1}}; !reflect.DeepEqual(res.Candidates, want) || len(res.Unreadable) != 0 {
		t.Errorf("candidates = %v, unreadable = %v; want %v, reported under the root as given", res.Candidates, res.Unreadable, want)
	}
}

func TestFind_StopsAtTheLimit(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a", "b", "c", "d"} {
		repo(t, filepath.Join(root, d), 1)
	}
	res := Find(root, 3)
	// The root, a and a/agents use up the limit, so b, c and d go unchecked.
	if got := paths(res.Candidates); !res.Truncated || !reflect.DeepEqual(got, []string{filepath.Join(root, "a")}) {
		t.Errorf("truncated = %v, candidates = %v; want true and only a", res.Truncated, got)
	}
	if res := Find(root, 9); res.Truncated || len(res.Candidates) != 4 {
		t.Errorf("a limit the tree fits in: truncated = %v, %d candidates; want false, 4", res.Truncated, len(res.Candidates))
	}
}

func TestIsAgent(t *testing.T) {
	for _, s := range []string{agentMD, "---\ndescription: d\nname: n\n---"} {
		if !IsAgent(s) {
			t.Errorf("IsAgent(%q) = false", s)
		}
	}
	for _, s := range []string{"", "---\n", "---\nname: n\n---\n", " ---\nname: n\ndescription: d\n---\n"} {
		if IsAgent(s) {
			t.Errorf("IsAgent(%q) = true", s)
		}
	}
}

func TestFrontmatter(t *testing.T) {
	s := "---\r\nname: n\r\nmodel:  opus \r\nmodel: sonnet\r\n  effort: max\r\n- effort: low\r\n# effort: low\r\n: x\r\ncolor\r\neffort:\r\ntools: Read # and Grep\r\n---\r\nmodel: haiku\r\n"
	fm, ok := Frontmatter(s)
	want := map[string]string{"name": "n", "model": "opus", "effort": "", "tools": "Read # and Grep"}
	if !ok || !reflect.DeepEqual(fm, want) {
		t.Errorf("Frontmatter = %v, %v; want %v, true", fm, ok, want)
	}
	if _, ok := Frontmatter("model: opus\n"); ok {
		t.Error("Frontmatter found frontmatter in a file without any")
	}
}
