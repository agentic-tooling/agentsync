// Package discover finds canonical agent repos on disk, so init can offer them
// instead of asking for a path from memory.
package discover

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Depth is how many levels below the search root are looked at, and MaxDirs
// how many directories a search visits before it stops.
const (
	Depth   = 4
	MaxDirs = 50000
)

// Candidate is a git repo whose agents/ holds at least one agent file.
type Candidate struct {
	Path   string
	Agents int
}

// Problem is a path the search could not read. What lies beneath it is
// unknown, not empty.
type Problem struct {
	Path string
	Err  error
}

// Result is what one search found, best candidate first.
type Result struct {
	Candidates []Candidate
	Unreadable []Problem
	// Truncated is true when the search stopped at its directory limit.
	Truncated bool
}

// skipped names directories never worth descending into: dependency trees and
// macOS's Library, which is large and holds no repos.
var skipped = map[string]bool{"node_modules": true, "vendor": true, "Library": true}

var errNotDir = errors.New("not a directory")

// Find searches root to Depth levels, visiting at most limit directories.
// Symlinks below root are never followed, and dot-directories are skipped.
// A root that is itself a symlink is resolved once, and paths are reported
// under root as given.
func Find(root string, limit int) Result {
	var res Result
	resolved, err := filepath.EvalSymlinks(root)
	var fi fs.FileInfo
	if err == nil {
		fi, err = os.Stat(resolved)
	}
	if err == nil && !fi.IsDir() {
		err = errNotDir
	}
	if err != nil {
		res.Unreadable = []Problem{{root, err}}
		return res
	}
	under := func(path string) string { return filepath.Join(root, path[len(resolved):]) }
	visited := 0
	_ = filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
		// A non-nil err is WalkDir failing to read path; it is reported like a
		// repo agentCount could not check.
		n := 0
		if err == nil {
			// WalkDir reports a symlink as a non-directory, so it is never entered.
			if !d.IsDir() {
				return nil
			}
			if path != resolved && (strings.HasPrefix(d.Name(), ".") || skipped[d.Name()]) {
				return fs.SkipDir
			}
			if visited++; visited > limit {
				res.Truncated = true
				return fs.SkipAll
			}
			n, err = agentCount(path)
		}
		if err != nil {
			res.Unreadable = append(res.Unreadable, Problem{under(path), err})
			return fs.SkipDir
		}
		if n > 0 {
			res.Candidates = append(res.Candidates, Candidate{under(path), n})
		}
		if rel, _ := filepath.Rel(resolved, path); rel != "." && strings.Count(rel, string(filepath.Separator)) >= Depth-1 {
			return fs.SkipDir
		}
		return nil
	})
	sort.Slice(res.Candidates, func(i, j int) bool {
		a, b := res.Candidates[i], res.Candidates[j]
		if a.Agents != b.Agents {
			return a.Agents > b.Agents
		}
		return a.Path < b.Path
	})
	return res
}

// agentCount returns how many agent files dir/agents holds when dir is a git
// repo, else 0. An error means dir could not be checked.
func agentCount(dir string) (int, error) {
	for _, sub := range []string{".git", "agents"} {
		fi, err := os.Lstat(filepath.Join(dir, sub))
		if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
	}
	// The first error stops the reading and is returned; callers discard n then.
	entries, err := os.ReadDir(filepath.Join(dir, "agents"))
	n := 0
	for _, e := range entries {
		if err == nil && e.Type().IsRegular() && filepath.Ext(e.Name()) == ".md" {
			var data []byte
			data, err = os.ReadFile(filepath.Join(dir, "agents", e.Name()))
			if err == nil && IsAgent(string(data)) {
				n++
			}
		}
	}
	return n, err
}

// IsAgent reports whether s opens with YAML frontmatter carrying top-level
// name and description keys, which is what Claude Code requires of an agent.
func IsAgent(s string) bool {
	fm, ok := Frontmatter(s)
	_, name := fm["name"]
	_, desc := fm["description"]
	return ok && name && desc
}

// Frontmatter returns the top-level keys of s's YAML frontmatter with their
// raw values trimmed, and false when s has none. Nested and list lines are
// skipped; the first occurrence of a key wins.
func Frontmatter(s string) (map[string]string, bool) {
	front, ok := strings.CutPrefix(strings.ReplaceAll(s, "\r\n", "\n"), "---\n")
	if !ok {
		return nil, false
	}
	front, _, ok = strings.Cut(front, "\n---")
	if !ok {
		return nil, false
	}
	fm := map[string]string{}
	for _, line := range strings.Split(front, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || key == "" || strings.ContainsAny(key[:1], " \t-#") {
			continue
		}
		if _, dup := fm[key]; !dup {
			fm[key] = strings.TrimSpace(value)
		}
	}
	return fm, true
}
