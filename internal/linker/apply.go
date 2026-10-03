package linker

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
)

// Applied is what an Apply actually did. link has three kinds of write — the
// symlinks themselves, the local exclusions that permit some of them, and the
// state records that make the symlinks removable later — and a report that
// showed only the first would claim a project was linked without saying that its
// repository was edited to allow it.
type Applied struct {
	Links    []Item
	Excludes []Exclude

	// Recorded is what went into the state document, one entry per link
	// actually created — the document's own words, so a report cannot
	// describe a record that was never written.
	Recorded []state.Record
}

// Apply creates every symlink the plan selected, records each one, and does
// nothing else.
//
// It returns what was written and one error per failure. Everything the plan
// only reported — a broken link, a foreign link, an override, a declined agent —
// is untouched: those are refusals, not pending writes.
//
// Exclusions go first, and a project whose exclusion could not be written is
// not linked. The exclusion is the condition under which linking that project
// was permitted at all, so writing the links anyway would be doing the thing
// the plan refused to do by default.
//
// A failure does not stop the run, for the same reason the importer's does
// not. The items are independent files in independent projects, so aborting on
// the first would leave the rest unprovisioned for a reason that has nothing to
// do with them. Every failure is returned, and the caller must treat a
// non-empty result as a failed run rather than a partial success.
//
// Apply saves the state document itself, so there is no supported way to create
// symlinks without recording them. The record follows the write and never
// precedes it: a crash then leaves unowned links, which an adopt pass can
// recover, rather than records claiming links this tool never made.
func (p Plan) Apply(repo *scan.Repo, st *state.File) (Applied, []error) {
	done := Applied{
		Links:    make([]Item, 0, len(p.Items)),
		Excludes: make([]Exclude, 0, len(p.Excludes)),
		Recorded: make([]state.Record, 0, len(p.Items)),
	}
	// Refused before anything is written: creating links and only then
	// finding there is nowhere to record them is the unowned-link state
	// above, reached deliberately rather than by a crash.
	if repo == nil || st == nil {
		return done, []error{errors.New("link: no repo or no state document; refusing to create links that could not be recorded")}
	}

	var errs []error

	unexcluded := make(map[string]bool, len(p.Excludes))
	for _, ex := range p.Excludes {
		if err := writeExclude(ex); err != nil {
			errs = append(errs, err)
			unexcluded[ex.Project] = true
			continue
		}
		done.Excludes = append(done.Excludes, ex)
	}

	for _, item := range p.Items {
		if !item.Action.Writes() {
			continue
		}
		if unexcluded[item.Project] {
			continue
		}
		if err := linkOne(item); err != nil {
			errs = append(errs, err)
			continue
		}
		done.Links = append(done.Links, item)
	}

	errs = append(errs, record(st, done.Links, &done.Recorded)...)

	// Once, and only if there is something to save. Dirty covers this run's
	// records and any marker the caller cleared before planning, so a run that
	// did neither does not conjure a state file.
	if st.Dirty() {
		if err := st.Save(repo); err != nil {
			errs = append(errs, err)
		}
	}
	return done, errs
}

// record writes one state entry per link that was actually created.
//
// Every entry is OriginCreated, ActionAdopt included: that action replaces a
// project file with a link this tool makes, where state.OriginAdopted means a
// link that was already there.
//
// The target is re-hashed here rather than reused from item.Hash, which judges
// the project file and is empty for a create. A hash that cannot be taken
// produces an error and no record, leaving the link unowned rather than storing
// an empty hash that would match nothing.
func record(st *state.File, links []Item, into *[]state.Record) []error {
	// One timestamp for the run: stamping links microseconds apart would
	// suggest an ordering that means nothing.
	now := time.Now()

	var errs []error
	for _, item := range links {
		hash, err := scan.HashFile(item.Target)
		if err != nil {
			errs = append(errs, fmt.Errorf("recording %s/%s: hashing %q: %w", item.Project, item.Name, item.Target, err))
			continue
		}
		rec, err := st.RecordCreated(state.Link{
			Key:    state.Key{Project: item.Project, Name: item.Name},
			Path:   item.Path,
			Target: item.Target,
			Hash:   hash,
		}, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("recording %s/%s: %w", item.Project, item.Name, err))
			continue
		}
		*into = append(*into, rec)
	}
	return errs
}

// writeExclude appends the plan's pattern to a project's .git/info/exclude.
//
// info/exclude, not .gitignore: .gitignore is tracked and shared, so a rule
// written there is a decision committed on behalf of everyone who clones the
// project. info/exclude is local to this clone and does the same job for the
// person running this command. See the Exclude doc comment.
//
// The file is rewritten whole through a temporary and a rename rather than
// opened for append. An append that fails halfway leaves a partial line, and a
// partial line in an exclude file is a pattern nobody wrote — which could match
// paths nobody meant to hide.
func writeExclude(ex Exclude) error {
	dir := filepath.Dir(ex.File)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %q: %w", dir, err)
	}

	existing, err := os.ReadFile(ex.File)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading %q: %w", ex.File, err)
	}
	// Re-checked here rather than trusted from the plan, so a concurrent run
	// or a hand edit between the two phases cannot produce a duplicate line.
	for line := range strings.SplitSeq(string(existing), "\n") {
		if strings.TrimSpace(line) == ex.Pattern {
			return nil
		}
	}

	var buf strings.Builder
	buf.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		buf.WriteString("\n")
	}
	// The comment survives in someone else's repository, so it says who wrote
	// the line and why rather than leaving an unexplained pattern behind.
	buf.WriteString("# agentsync: symlinks here point at an absolute path on this machine\n")
	buf.WriteString(ex.Pattern + "\n")

	if err := writeAtomic(ex.File, buf.String()); err != nil {
		return fmt.Errorf("excluding %s in %q: %w", ex.Pattern, ex.File, err)
	}
	return nil
}

// writeAtomic replaces a file's contents through a temporary in its own
// directory. A reader sees either the whole old file or the whole new one, and
// a crash leaves a stray temporary rather than a truncated exclude file.
func writeAtomic(path, content string) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agentsync-exclude-*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.WriteString(content); err != nil {
		return fmt.Errorf("writing %q: %w", tmpName, err)
	}
	// Synced before the rename: without it the rename can reach disk while
	// the bytes it points at have not.
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %q: %w", tmpName, err)
	}
	// CreateTemp makes the file 0600; an exclude file is an ordinary readable
	// file like everything else in .git/info.
	if err = tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("setting mode on %q: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", tmpName, err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %q to %q: %w", tmpName, path, err)
	}
	return nil
}

func linkOne(item Item) error {
	if err := recheck(item); err != nil {
		return err
	}

	// Create and adopt need different writes, and using one for both makes the
	// safer of the two worse.
	//
	// Adopt is replacing a file that is there, so it needs the rename: there
	// is no way to swap a file for a link in one step otherwise, and the
	// alternative leaves a window with no agent file at all.
	//
	// Create is putting a link where nothing is, and os.Symlink already fails
	// with EEXIST rather than clobbering — an exclusive create, atomic by
	// itself. Routing it through recheck-then-rename would replace that with a
	// check and a later unconditional overwrite, which is a race the direct
	// call does not have. recheck still runs first because it produces a far
	// better message for the ordinary case; EEXIST is the guarantee.
	var err error
	if item.Action == ActionCreate {
		err = symlinkExclusive(item.Target, item.Path)
	} else {
		err = symlinkAtomic(item.Target, item.Path)
	}
	if err != nil {
		return fmt.Errorf("linking %s/%s: %w", item.Project, item.Name, err)
	}
	return nil
}

// symlinkExclusive creates a link where nothing is, and fails if anything is.
//
// No temporary and no rename: os.Symlink is itself the exclusive create, so
// there is nothing to leak and no window in which a file that appeared could
// be overwritten.
func symlinkExclusive(target, path string) error {
	dir := filepath.Dir(path)
	// The directory may not exist at all: --agent can provision a project that
	// never had a .claude/agents, which is the whole point of being able to
	// address a project by key rather than by discovery.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %q: %w", dir, err)
	}
	if err := os.Symlink(target, path); err != nil {
		return fmt.Errorf("creating %q: %w", path, err)
	}
	return nil
}

// recheck re-examines both ends immediately before the write.
//
// The plan's verdict is what authorises replacing a real file with a link, and
// it was formed during a scan that may have walked a hundred repositories.
// Re-asking narrows that window to microseconds. It cannot close it — os.Rename
// replaces its destination unconditionally and there is no portable
// rename-if-absent — but on this path the difference between a stale
// observation and a fresh one is the difference between adopting a file and
// destroying an edit somebody made while the scan was running.
//
// Both sides are re-hashed, not just the project file. "Adoptable" is a claim
// about a pair, and it stops being true if either half moves: a counterpart
// rewritten between plan and apply would leave the project pointing at content
// it never had.
func recheck(item Item) error {
	// The target must be a real file in the repo at the moment we point at
	// it. Linking to something absent manufactures precisely the broken link
	// (state 3) this command refuses to clean up afterwards.
	tfi, err := os.Lstat(item.Target)
	if err != nil {
		return fmt.Errorf("re-checking target %q: %w", item.Target, err)
	}
	if !tfi.Mode().IsRegular() {
		return fmt.Errorf("target %q is not a regular file; refusing to link to it", item.Target)
	}

	fi, err := os.Lstat(item.Path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("re-checking %q: %w", item.Path, err)
		}
		if item.Action == ActionCreate {
			// Absent is what a create expects.
			return nil
		}
		return fmt.Errorf("%q vanished since the plan judged it adoptable; nothing was written", item.Path)
	}

	if item.Action == ActionCreate {
		return fmt.Errorf("%q appeared since the plan judged it absent; refusing to replace a file this run never examined", item.Path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%q is no longer a regular file; refusing to replace it", item.Path)
	}
	if item.Hash == "" {
		// Only reachable for a hand-made item. An adoption authorised by
		// no hash is an adoption nothing verified.
		return fmt.Errorf("%q was planned for adoption without a content hash; refusing to replace an unverified file", item.Path)
	}

	hash, err := scan.HashFile(item.Path)
	if err != nil {
		return fmt.Errorf("re-hashing %q: %w", item.Path, err)
	}
	if hash != item.Hash {
		return fmt.Errorf("%q changed since the plan judged it adoptable (%s, planned against %s); replacing it with a link would discard that change",
			item.Path, hash, item.Hash)
	}

	targetHash, err := scan.HashFile(item.Target)
	if err != nil {
		return fmt.Errorf("re-hashing target %q: %w", item.Target, err)
	}
	if targetHash != item.Hash {
		return fmt.Errorf("%q no longer matches %q (%s against %s); it is not adoptable and was left alone",
			item.Path, item.Target, item.Hash, targetHash)
	}
	return nil
}

// symlinkAtomic replaces whatever is at path with a link to target, by
// creating the link under a temporary name in path's own directory and
// renaming it into place. It is the adopt path; see symlinkExclusive for
// create, which needs none of this.
//
// Never unlink-then-symlink. That sequence leaves a window in which the agent
// file does not exist at all, and a session starting inside it sees a project
// that has lost an agent. Rename has no such window: a reader sees either the
// old file or the new link, and a crash between the two leaves a stray
// temporary rather than a gap.
//
// The temporary must live in path's directory, because rename is atomic only
// within a filesystem and a project may well sit on a different one from the
// system temp dir.
func symlinkAtomic(target, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %q: %w", dir, err)
	}

	tmp, err := tempSymlink(dir, target)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		// The temporary is inside a project's agent directory, where a
		// stray file would be picked up as an agent by the next scan — and
		// by Claude Code. Removing it matters more than reporting that the
		// removal failed, which the rename error already implies.
		_ = os.Remove(tmp)
		return fmt.Errorf("renaming %q to %q: %w", tmp, path, err)
	}
	return nil
}

// ReplaceLink replaces whatever is at path with a symlink to target, with no
// window in which the agent file does not exist.
//
// Exported for reconcile, which repoints links when a project is re-keyed. The
// no-gap guarantee is the whole content of symlinkAtomic below, and a second
// implementation of it would lose that quietly. It performs no re-check of its
// own: the caller decides what it is entitled to replace.
func ReplaceLink(target, path string) error { return symlinkAtomic(target, path) }

// tempSymlink creates a symlink to target under an unused name in dir and
// returns that name.
//
// os.CreateTemp cannot be used: it creates a regular file, and creating one
// only to unlink it before symlinking over the name reopens the race the
// temporary exists to avoid. os.Symlink already fails with EEXIST rather than
// clobbering, so retrying on a taken name is itself the exclusion.
//
// The name starts with a dot and does not end in .md, so a scan racing this
// window neither globs it (*.md) nor mistakes it for an agent.
func tempSymlink(dir, target string) (string, error) {
	const attempts = 10
	for range attempts {
		name := filepath.Join(dir, fmt.Sprintf(".agentsync-link-%d-%d.tmp", os.Getpid(), rand.Uint32()))
		err := os.Symlink(target, name)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("creating a temporary link in %q: %w", dir, err)
		}
	}
	return "", fmt.Errorf("creating a temporary link in %q: %d names were already taken", dir, attempts)
}
