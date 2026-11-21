package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

const (
	Flag       = "skill"
	projectDir = ".claude/skills"
	backupsDir = "project-skills"
)

// State describes the relationship of a project skill to the canonical repo.
type State string

const (
	LocalOnly State = "local-only"
	Match     State = "canonical-match"
	Drift     State = "content-drift"
	Linked    State = "linked"
	Broken    State = "broken"
	Foreign   State = "foreign"
)

// Entry is a skill directory or directory symlink found in a project.
type Entry struct {
	Project     string `json:"project"`
	Name        string `json:"name"`
	Path        string `json:"path"`
	State       State  `json:"state"`
	Hash        string `json:"hash,omitempty"`
	Counterpart string `json:"counterpart,omitempty"`
	LinkTarget  string `json:"link_target,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func (e Entry) NeedsHuman() bool {
	return e.State == LocalOnly || e.State == Drift || e.State == Broken
}

type Report struct {
	Entries  []Entry            `json:"skills,omitempty"`
	Problems []scan.ScanProblem `json:"skill_problems,omitempty"`
	Dirs     []string           `json:"-"`
}

func (r Report) AtRisk() Report {
	out := Report{Problems: r.Problems}
	for _, entry := range r.Entries {
		if entry.NeedsHuman() {
			out.Entries = append(out.Entries, entry)
		}
	}
	return out
}

// Catalog locates the shared and per-project trees beneath one checkout.
type Catalog struct {
	repo   *scan.Repo
	root   *os.Root
	shared string
}

func Open(repo *scan.Repo, shared string) (*Catalog, error) {
	if shared == "" {
		shared = "skills"
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Join(repo.Root, shared)); err == nil {
		var inside bool
		shared, inside = relative(repo.Root, resolved)
		if !inside || shared == "." {
			return nil, fmt.Errorf("skills root %s must stay inside %s", resolved, repo.Root)
		}
	}
	root, err := openAbsolute(repo.Root)
	if err != nil {
		return nil, err
	}
	dir, err := openDir(root, shared, false)
	if err == nil {
		_ = dir.Close()
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = root.Close()
		return nil, fmt.Errorf("skills root: %w", err)
	}
	return &Catalog{repo: repo, root: root, shared: shared}, nil
}

func (c *Catalog) Close() error { return c.root.Close() }

func relative(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	return rel, err == nil && filepath.IsLocal(rel)
}

func backupParent(key string) string {
	return filepath.Join(backupsDir, filepath.FromSlash(key))
}

func (c *Catalog) counterpart(key, name string) (string, string, error) {
	for _, parent := range []string{backupParent(key), c.shared} {
		path := filepath.Join(parent, name)
		dir, err := openDir(c.root, path, false)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		content, err := readTree(dir)
		_ = dir.Close()
		if err != nil {
			return "", "", fmt.Errorf("canonical skill %s: %w", path, err)
		}
		return filepath.Join(c.repo.Root, path), content.digest(), nil
	}
	return "", "", nil
}

func (c *Catalog) Scan(root string) (Report, error) {
	dirs, problems, err := scan.FindSkillDirs(root)
	if err != nil {
		return Report{}, err
	}
	report := c.ScanDirs(dirs)
	report.Problems = append(report.Problems, problems...)
	return report, nil
}

func (c *Catalog) ScanDirs(dirs []string) Report {
	var report Report
	for _, path := range dirs {
		project, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(path)))
		if err != nil {
			report.addProblem(path, err)
			continue
		}
		if _, inside := relative(c.repo.Root, project); inside || project == c.repo.Home {
			continue
		}
		path = filepath.Join(project, projectDir)
		report.Dirs = append(report.Dirs, path)
		report.scanDir(c, path)
	}
	return report
}

func (r *Report) scanDir(c *Catalog, path string) {
	project := filepath.Dir(filepath.Dir(path))
	anchor, err := openAbsolute(project)
	if err != nil {
		r.addProblem(path, err)
		return
	}
	defer func() { _ = anchor.Close() }()
	dir, err := openDir(anchor, projectDir, false)
	if err != nil {
		r.addProblem(path, err)
		return
	}
	defer func() { _ = dir.Close() }()
	children, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		r.addProblem(path, err)
		return
	}
	key := scan.ProjectKey(c.repo.Home, path)
	for _, child := range children {
		if !child.IsDir() && child.Type()&fs.ModeSymlink == 0 {
			continue
		}
		entry := Entry{Project: key, Name: child.Name(), Path: filepath.Join(path, child.Name())}
		item, err := c.classify(dir, entry)
		switch {
		case err == errNoManifest:
		case err != nil:
			r.addProblem(entry.Path, err)
		default:
			r.Entries = append(r.Entries, item)
		}
	}
}

func (r *Report) addProblem(path string, err error) {
	r.Problems = append(r.Problems, scan.ScanProblem{Path: path, Err: err.Error()})
}

func (c *Catalog) classify(dir *os.Root, e Entry) (Entry, error) {
	info, err := dir.Lstat(e.Name)
	if err != nil {
		return e, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return c.classifyLink(dir, e)
	}
	child, err := openChild(dir, e.Name, false)
	if err != nil {
		return e, err
	}
	defer func() { _ = child.Close() }()
	content, err := readTree(child)
	if err != nil {
		return e, err
	}
	e.Hash = content.digest()
	var hash string
	e.Counterpart, hash, err = c.counterpart(e.Project, e.Name)
	switch {
	case e.Counterpart == "":
		e.State = LocalOnly
	case e.Hash == hash:
		e.State = Match
	default:
		e.State = Drift
	}
	return e, err
}

func (c *Catalog) classifyLink(dir *os.Root, e Entry) (Entry, error) {
	target, err := dir.Readlink(e.Name)
	if err != nil {
		return e, err
	}
	if !filepath.IsAbs(target) {
		target = dir.Name() + string(filepath.Separator) + target
	}
	e.LinkTarget = target
	resolved, missing, err := resolveTarget(target)
	if err != nil {
		return e, err
	}
	if missing {
		e.State = Foreign
		if _, inside := relative(c.repo.Root, resolved); inside {
			e.State = Broken
		}
		return e, nil
	}
	if _, inside := relative(c.repo.Root, resolved); !inside {
		e.State = Foreign
		return e, nil
	}
	e.Counterpart, e.Hash, err = c.counterpart(e.Project, e.Name)
	if resolved == e.Counterpart {
		e.State = Linked
	} else {
		e.State = Drift
		e.Reason = "link points at a different canonical path"
	}
	return e, err
}

// resolveTarget retains the missing suffix while resolving existing ancestors
// and dangling intermediate links so ownership follows the intended path.
func resolveTarget(path string) (string, bool, error) {
	suffix := ""
	dangling := 0
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, suffix), suffix != "" || dangling > 0, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		probe := strings.TrimRight(path, string(filepath.Separator))
		parent, base := filepath.Split(probe)
		if parent == "" {
			return "", false, err
		}
		info, err := os.Lstat(probe)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false, err
		}
		if info != nil && info.Mode()&fs.ModeSymlink != 0 {
			ref, err := os.Readlink(probe)
			if err != nil {
				return "", false, err
			}
			if !filepath.IsAbs(ref) {
				ref = parent + ref
			}
			path = ref
			dangling++
			if dangling > 255 {
				return "", false, errors.New("too many dangling symlink hops")
			}
			continue
		}
		suffix = filepath.Join(base, suffix)
		path = parent
	}
}

// ValidateNames accepts directory basenames without normalizing path input.
func ValidateNames(names []string) error {
	for _, name := range names {
		if !filepath.IsLocal(name) || name == "." || strings.ContainsAny(name, "/\\\x00\n\r") || strings.HasPrefix(name, "-") {
			return fmt.Errorf("invalid --skill name %q: expected a directory name", name)
		}
	}
	return nil
}
