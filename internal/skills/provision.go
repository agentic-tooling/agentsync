package skills

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/linker"
	"github.com/agentic-tooling/agentsync/internal/scan"
)

type status string

const (
	pending status = "pending"
	applied status = "applied"
	match   status = "matching"
	blocked status = "blocked"
	failed  status = "error"
)

type Change struct {
	Project     string `json:"project"`
	Name        string `json:"name"`
	Path        string `json:"path,omitempty"`
	Canonical   string `json:"canonical,omitempty"`
	Hash        string `json:"hash,omitempty"`
	Status      status `json:"status"`
	Reason      string `json:"reason,omitempty"`
	ExcludeFile string `json:"exclude_file,omitempty"`
	Pattern     string `json:"exclude_pattern,omitempty"`

	directory string
	mode      Mode
}

type Plan []Change

func (p Plan) Counts() (pendingCount, appliedCount, blockedCount, failedCount int) {
	for _, item := range p {
		switch item.Status {
		case pending:
			pendingCount++
		case applied:
			appliedCount++
		case blocked:
			blockedCount++
		case failed:
			failedCount++
		}
	}
	return
}

func (p Plan) Dirs() []string {
	var dirs []string
	for _, item := range p {
		if item.directory != "" {
			dir := filepath.Join(item.directory, projectDir)
			if !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

type Mode string

const (
	Link   Mode = "link"
	Import Mode = "import"
)

// Plan chooses project/name pairs before checking their current filesystem and
// Git state. A wide import selects only projects where the requested name exists.
func (c *Catalog) Plan(mode Mode, keys, names []string, report Report, exclude bool) Plan {
	if mode != Link && mode != Import {
		return Plan{{Status: failed, Reason: "invalid skill operation"}}
	}
	plan := make(Plan, 0)
	names = unique(names)
	if len(keys) != 0 || mode == Link {
		for _, key := range unique(keys) {
			for _, name := range names {
				plan = append(plan, c.check(mode, strings.TrimRight(key, "/"), name, exclude))
			}
		}
		return plan
	}
	for _, name := range names {
		found := false
		for _, dir := range report.Dirs {
			path := filepath.Join(dir, name)
			if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				continue
			}
			found = true
			key := scan.ProjectKey(c.repo.Home, dir)
			plan = append(plan, c.check(mode, key, name, exclude))
		}
		if !found {
			plan = append(plan, Change{Name: name, Status: blocked, Reason: "skill was not found in the selected projects"})
		}
	}
	return plan
}

func unique(values []string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}

func (c *Catalog) check(mode Mode, key, name string, exclude bool) Change {
	item := Change{Project: key, Name: name, mode: mode}
	dir, err := scan.ProjectDir(c.repo.Home, key)
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		return item.refuse(blocked, err.Error())
	}
	if _, inside := relative(c.repo.Root, dir); inside {
		return item.refuse(blocked, "the project is inside the canonical checkout")
	}
	if dir == c.repo.Home {
		return item.refuse(blocked, "the home directory holds user-level skills")
	}
	if scan.ProjectKey(c.repo.Home, filepath.Join(dir, projectDir)) != key {
		return item.refuse(blocked, "project path does not match the selected key")
	}
	item.directory = dir
	item.Path = filepath.Join(dir, projectDir, name)
	project, err := openAbsolute(dir)
	if err != nil {
		return item.refuse(failed, err.Error())
	}
	defer func() { _ = project.Close() }()
	if mode == Link {
		return c.checkLink(project, item, exclude)
	}
	item, _ = c.checkImport(project, item)
	return item
}

func (i Change) refuse(s status, reason string) Change {
	i.Status, i.Reason = s, reason
	return i
}

func (c *Catalog) checkLink(project *os.Root, item Change, exclude bool) Change {
	dir, err := openDir(project, projectDir, false)
	if err == nil {
		defer func() { _ = dir.Close() }()
		if info, err := dir.Lstat(item.Name); err == nil {
			if info.Mode()&fs.ModeSymlink != 0 {
				entry, err := c.classifyLink(dir, Entry{Name: item.Name, Project: item.Project})
				if err != nil {
					return item.refuse(failed, err.Error())
				}
				if entry.State == Linked {
					item.Canonical, item.Hash = entry.Counterpart, entry.Hash
					return item.refuse(match, "already linked to the canonical skill")
				}
			}
			return item.refuse(blocked, "a directory, file or link already occupies the project path")
		} else if !errors.Is(err, fs.ErrNotExist) {
			return item.refuse(failed, err.Error())
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return item.refuse(failed, err.Error())
	}
	item.Canonical, item.Hash, err = c.counterpart(item.Project, item.Name)
	if err != nil {
		return item.refuse(failed, err.Error())
	}
	if item.Canonical == "" {
		return item.refuse(blocked, "no canonical skill exists for this name")
	}
	return linkGate(item, exclude)
}

func linkGate(item Change, exclude bool) Change {
	item.ExcludeFile, item.Pattern = "", ""
	target := scan.InspectSkillPath(item.directory, filepath.Join(projectDir, item.Name))
	switch target.Tracking {
	case scan.DirUnknown:
		return item.refuse(failed, "git could not inspect the skill path")
	case scan.DirTracked:
		return item.refuse(blocked, "git has an index or HEAD entry at or under the skill path, or an ancestor file")
	case scan.DirNoRepo:
		return item.refuse(pending, "create a skill directory symlink")
	}

	parent := scan.InspectSkillPath(item.directory, projectDir)
	if parent.Tracking == scan.DirUnknown {
		return item.refuse(failed, "git could not inspect the project's skills directory")
	}
	if parent.Tracking == scan.DirTracked && !exclude {
		return item.refuse(blocked, "git tracks the skills directory; use --exclude-local for an untracked sibling")
	}
	if exclude && target.Tracking == scan.DirVisible {
		item.ExcludeFile = scan.SkillExcludePath(target.Root)
		if item.ExcludeFile == "" {
			return item.refuse(failed, "git did not resolve its local exclude file")
		}
		escaped := strings.NewReplacer("\\", "\\\\", "*", "\\*", "?", "\\?", "[", "\\[", "]", "\\]", " ", "\\ ")
		if strings.ContainsAny(target.Rel, "\n\r") {
			return item.refuse(failed, "git exclude rules cannot represent this path")
		}
		item.Pattern = "/" + escaped.Replace(target.Rel)
	}
	return item.refuse(pending, "create a skill directory symlink")
}

func (c *Catalog) checkImport(project *os.Root, item Change) (Change, tree) {
	dir, err := openDir(project, filepath.Join(projectDir, item.Name), false)
	if errors.Is(err, fs.ErrNotExist) {
		return item.refuse(blocked, "project skill directory is missing"), nil
	}
	if err != nil {
		return item.refuse(failed, err.Error()), nil
	}
	defer func() { _ = dir.Close() }()
	content, err := readTree(dir)
	if err != nil {
		return item.refuse(failed, err.Error()), nil
	}
	if scan.InspectSkillPath(item.directory, filepath.Join(projectDir, item.Name)).Tracking == scan.DirUnknown {
		return item.refuse(failed, "git could not inspect the project skill"), nil
	}
	item.Hash = content.digest()
	backup := filepath.Join(c.repo.Root, backupParent(item.Project), item.Name)
	target, hash, err := c.counterpart(item.Project, item.Name)
	if err != nil {
		return item.refuse(failed, err.Error()), nil
	}
	item.Canonical = backup
	if target != "" && item.Hash == hash {
		item.Canonical = target
		return item.refuse(match, "canonical skill already holds matching content"), nil
	}
	if target == backup {
		return item.refuse(blocked, "the project capture already exists with different content"), nil
	}
	switch scan.InspectSkillPath(c.repo.Root, filepath.Join(backupParent(item.Project), item.Name)).Tracking {
	case scan.DirUnknown:
		return item.refuse(failed, "git could not inspect the canonical capture path"), nil
	case scan.DirTracked:
		return item.refuse(blocked, "the missing capture still has entries in the canonical index or HEAD"), nil
	}
	return item.refuse(pending, "capture the complete skill directory"), content
}

func (c *Catalog) Apply(plan Plan, exclude bool) {
	for i, item := range plan {
		if item.Status != pending {
			continue
		}
		plan[i] = c.applyOne(item, exclude)
	}
}

func (c *Catalog) applyOne(item Change, exclude bool) Change {
	project, err := openAbsolute(item.directory)
	if err != nil {
		return item.refuse(failed, err.Error())
	}
	defer func() { _ = project.Close() }()
	var current Change
	var content tree
	switch item.mode {
	case Link:
		current = c.checkLink(project, item, exclude)
	case Import:
		current, content = c.checkImport(project, item)
	default:
		return item.refuse(failed, "invalid skill operation")
	}
	if current.Status != pending {
		return current
	}
	if item.Hash != current.Hash || item.Canonical != current.Canonical {
		return current.refuse(blocked, "skill changed since planning; run again to review the new content")
	}

	if item.mode == Link {
		err = applyLink(project, current, exclude)
	} else {
		err = c.applyCapture(current, content)
	}
	if errors.Is(err, fs.ErrExist) {
		return current.refuse(blocked, "the destination appeared while applying")
	}
	if err != nil {
		return current.refuse(failed, err.Error())
	}
	return current.refuse(applied, current.Reason)
}

func applyLink(project *os.Root, item Change, exclude bool) error {
	dir, err := openDir(project, projectDir, true)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if item.ExcludeFile != "" {
		if err := linker.WriteExclude(item.ExcludeFile, item.Pattern); err != nil {
			return err
		}
	}
	current := linkGate(item, exclude)
	if current.Status != pending {
		return errors.New(current.Reason)
	}
	if current.ExcludeFile != "" {
		return errors.New("git has not excluded the skill symlink")
	}
	return dir.Symlink(item.Canonical, item.Name)
}

func (c *Catalog) applyCapture(item Change, content tree) error {
	parent, err := openDir(c.root, backupParent(item.Project), true)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	return capture(parent, item.Name, content)
}
