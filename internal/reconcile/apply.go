package reconcile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/agentic-tooling/agentsync/internal/linker"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
)

// Applied is what an Apply actually did.
//
// The five kinds are reported separately because they are five different
// promises: a removed link, a marker that changes what link will do in future,
// a record that makes a link removable at all, a moved directory, and a deleted
// file.
type Applied struct {
	Pruned    []Item
	Declined  []state.Declined
	Adopted   []state.Record
	Rekeyed   []Rekey
	Repointed []Repoint
	Dropped   []Item
}

// Apply performs everything the plan selected and nothing else.
//
// Every op re-examines its subject on disk immediately before acting: the plan
// was formed during a scan that may have walked a hundred repositories, and on
// a path that removes something, a stale observation is the difference between
// unlinking a dead link and destroying a live one.
//
// A failure does not stop the run, as in importer and linker: the subjects are
// independent, and a non-empty result is a failed run, not a partial success.
//
// Re-keys go first because a move can make a broken link healthy again, and a
// prune that ran first would destroy the link the move was about to fix. The
// prune re-check catches that anyway, so this is belt as well as braces.
func (p Plan) Apply(repo *scan.Repo, st *state.File) (Applied, []error) {
	done := Applied{
		Pruned:    make([]Item, 0),
		Declined:  make([]state.Declined, 0),
		Adopted:   make([]state.Record, 0),
		Rekeyed:   make([]Rekey, 0, len(p.Rekeys)),
		Repointed: make([]Repoint, 0),
		Dropped:   make([]Item, 0),
	}
	if repo == nil || st == nil {
		return done, []error{errors.New("reconcile: no repo or no state document; refusing to change anything that could not be recorded")}
	}

	// One timestamp for the run: stamping records microseconds apart would
	// suggest an ordering that means nothing.
	now := time.Now()
	var errs []error

	for _, rk := range p.Rekeys {
		errs = append(errs, applyRekey(st, rk, &done, now)...)
	}
	for _, item := range p.Items {
		var err error
		switch item.Op {
		case OpPrune:
			err = applyPrune(repo, st, item, &done)
		case OpDecline:
			err = applyDecline(st, item, &done, now)
		case OpAdopt:
			err = applyAdopt(st, item, &done, now)
		case OpDropCapture:
			err = applyDrop(item, &done)
		default:
			continue
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	// Once, and only if something changed. Dirty covers every record and
	// marker touched above, so a run that decided nothing does not conjure a
	// state file.
	if st.Dirty() {
		if err := st.Save(repo); err != nil {
			errs = append(errs, err)
		}
	}
	return done, errs
}

// applyPrune removes one link this tool created, then forgets it.
//
// The link goes first and the record second, and the order is the whole safety
// argument. A crash between them leaves a record naming a path that holds
// nothing, which the next run reads as row three of the table and turns into a
// declined marker — wrong, visible, and undone with `link --undecline`. The
// other order leaves a link on disk with no record, which is unowned forever:
// no later pass can tell it from a handmade link, so nothing may ever remove
// it. One failure is recoverable and the other is not.
func applyPrune(repo *scan.Repo, st *state.File, item Item, done *Applied) error {
	if err := recheckPrune(repo, item); err != nil {
		return err
	}
	if err := os.Remove(item.Path); err != nil {
		return fmt.Errorf("removing %s/%s: unlinking %q: %w", item.Project, item.Name, item.Path, err)
	}
	if !st.Remove(item.Path) {
		return fmt.Errorf("removed %q, but the state document had no record to drop; it is no longer accounted for", item.Path)
	}
	done.Pruned = append(done.Pruned, item)
	return nil
}

// recheckPrune re-asks every question the prune gate asked, against the disk as
// it is now rather than as the plan found it.
//
// All four must still hold: a symlink, aimed exactly where the record says,
// inside this repo, and pointing at nothing. The last is what makes the run
// order safe — a target that came back between plan and apply turns the link
// healthy, and a healthy link is never removed.
func recheckPrune(repo *scan.Repo, item Item) error {
	fi, err := os.Lstat(item.Path)
	if err != nil {
		return fmt.Errorf("re-checking %q: %w", item.Path, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%q is no longer a symlink (%v); refusing to remove it", item.Path, fi.Mode())
	}

	target, err := linkTarget(item.Path)
	if err != nil {
		return err
	}
	if target != item.Target {
		return fmt.Errorf("%q now points at %q, not the %q this run planned against; refusing to remove a link somebody re-pointed", item.Path, target, item.Target)
	}
	if !inside(repo.Root, target) {
		return fmt.Errorf("%q points outside this repo (%q); not ours to remove", item.Path, target)
	}

	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("%q is no longer broken — its target %q is back; refusing to remove a working link", item.Path, target)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("re-checking the target %q of %q: %w", target, item.Path, err)
	}
	return nil
}

// applyDecline records that a link was deleted on purpose, and drops the record
// that described it.
//
// Both halves are needed for a second run to be a no-op: a record left behind
// names a path holding nothing, so the next run would reach the same conclusion
// and rewrite the marker with a fresh timestamp forever.
func applyDecline(st *state.File, item Item, done *Applied, now time.Time) error {
	if _, err := os.Lstat(item.Path); err == nil {
		return fmt.Errorf("%q is no longer empty; refusing to record that it was deleted on purpose", item.Path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("re-checking %q: %w", item.Path, err)
	}

	key := state.Key{Project: item.Project, Name: item.Name}
	if err := st.Decline(key, "link gone, nothing in its place", now); err != nil {
		return fmt.Errorf("declining %s: %w", key, err)
	}
	st.Remove(item.Path)

	d, ok := st.LookupDeclined(key)
	if !ok {
		return fmt.Errorf("declining %s: the marker did not survive being written", key)
	}
	done.Declined = append(done.Declined, d)
	return nil
}

// applyAdopt records a link that was already there. Nothing on disk changes.
func applyAdopt(st *state.File, item Item, done *Applied, now time.Time) error {
	if err := recheckAdopt(item); err != nil {
		return err
	}
	rec, err := st.RecordAdopted(state.Link{
		Key:    state.Key{Project: item.Project, Name: item.Name},
		Path:   item.Path,
		Target: item.Target,
		Hash:   item.Hash,
	}, now)
	if err != nil {
		return fmt.Errorf("adopting %s/%s: %w", item.Project, item.Name, err)
	}
	done.Adopted = append(done.Adopted, rec)
	return nil
}

// recheckAdopt refuses to record a claim that has stopped being true.
//
// The hash matters as much as the path: the record's whole purpose is to be the
// merge base a later run reads, and one taken against content the target no
// longer holds would misjudge a dereferenced link as an edited one.
func recheckAdopt(item Item) error {
	fi, err := os.Lstat(item.Path)
	if err != nil {
		return fmt.Errorf("re-checking %q: %w", item.Path, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%q is no longer a symlink (%v); refusing to record it as one", item.Path, fi.Mode())
	}
	target, err := linkTarget(item.Path)
	if err != nil {
		return err
	}
	if target != item.Target {
		return fmt.Errorf("%q now points at %q, not the %q this run judged; refusing to record a link it did not examine", item.Path, target, item.Target)
	}

	tfi, err := os.Lstat(item.Target)
	if err != nil {
		return fmt.Errorf("re-checking the target %q of %q: %w", item.Target, item.Path, err)
	}
	if !tfi.Mode().IsRegular() {
		return fmt.Errorf("the target %q of %q is not a regular file (%v)", item.Target, item.Path, tfi.Mode())
	}
	hash, err := scan.HashFile(item.Target)
	if err != nil {
		return fmt.Errorf("re-hashing %q: %w", item.Target, err)
	}
	if hash != item.Hash {
		return fmt.Errorf("%q changed since this run hashed it (%s, planned against %s); the record would carry a merge base nothing holds", item.Target, short(hash), short(item.Hash))
	}
	return nil
}

// applyDrop deletes one capture, having re-asked the gate that authorised it.
//
// The commit gate is asked again rather than trusted from the plan. It is the
// entire justification for the delete — these bytes are in HEAD, so `git show`
// still has them — and a capture rewritten between the two phases would be
// destroyed on the strength of a fact about different content.
//
// The plan's in-use gate is NOT re-asked, and that is a known gap: a link
// created between the two phases points at this capture and is broken by the
// delete. It is left rather than closed because re-asking needs a whole scan
// this function does not have, and because the damage is bounded by the gate
// above — the bytes are provably in HEAD, so the link is restorable and nothing
// is lost. Give this a scan result and the check belongs here.
func applyDrop(item Item, done *Applied) error {
	fi, err := os.Lstat(item.Path)
	if err != nil {
		return fmt.Errorf("re-checking %q: %w", item.Path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%q is no longer a regular file (%v); refusing to delete it", item.Path, fi.Mode())
	}
	hash, err := scan.HashFile(item.Path)
	if err != nil {
		return fmt.Errorf("re-hashing %q: %w", item.Path, err)
	}
	if hash != item.Hash {
		return fmt.Errorf("%q changed since this run gated it (%s, planned against %s); the bytes now here are not the ones proved to be committed", item.Path, short(hash), short(item.Hash))
	}
	if c := scan.CommittedInHEAD(item.Path); c != scan.CommittedYes {
		return fmt.Errorf("%q is %s as of now; the gate that authorised deleting it no longer passes", item.Path, c)
	}

	if err := os.Remove(item.Path); err != nil {
		return fmt.Errorf("deleting %q: %w", item.Path, err)
	}
	done.Dropped = append(done.Dropped, item)
	return nil
}

// applyRekey moves one capture directory and makes every link and record that
// pointed into it follow.
//
// The move goes first. Repointing a link before its target has arrived would
// manufacture the broken link this tool refuses to create, and would do it in
// somebody else's project. A repoint that fails afterwards leaves a link
// pointing at the old path — broken, visible, reported, and fixable by hand —
// which is the survivable direction.
func applyRekey(st *state.File, rk Rekey, done *Applied, now time.Time) []error {
	if err := recheckRekey(rk); err != nil {
		return []error{err}
	}
	if err := os.MkdirAll(filepath.Dir(rk.NewDir), 0o755); err != nil {
		return []error{fmt.Errorf("re-keying %s: creating %q: %w", rk, filepath.Dir(rk.NewDir), err)}
	}
	// An empty destination directory is removed rather than renamed onto:
	// rename over an existing directory is permitted only when it is empty,
	// and only on some platforms, so this makes one behaviour out of two.
	if err := os.Remove(rk.NewDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return []error{fmt.Errorf("re-keying %s: clearing the empty %q: %w", rk, rk.NewDir, err)}
	}
	if err := os.Rename(rk.OldDir, rk.NewDir); err != nil {
		return []error{fmt.Errorf("re-keying %s: moving %q to %q: %w", rk, rk.OldDir, rk.NewDir, err)}
	}
	done.Rekeyed = append(done.Rekeyed, rk)

	var errs []error
	for _, rp := range rk.Repoint {
		if err := repointOne(st, rp, now); err != nil {
			errs = append(errs, err)
			continue
		}
		done.Repointed = append(done.Repointed, rp)
	}
	return errs
}

// recheckRekey re-asks the two gates that decide whether the move is safe: the
// source still holds only its own captures, and the destination still holds
// nothing.
func recheckRekey(rk Rekey) error {
	captures, dirs, err := captureContents(rk.OldDir)
	if err != nil {
		return fmt.Errorf("re-checking %q: %w", rk.OldDir, err)
	}
	if len(captures) == 0 {
		return fmt.Errorf("%q no longer holds any captures; nothing was moved", rk.OldDir)
	}
	if len(dirs) > 0 {
		return fmt.Errorf("%q gained a subdirectory since this run judged it; moving it would carry captures for a project nobody named", rk.OldDir)
	}

	destCaptures, destDirs, err := captureContents(rk.NewDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("re-checking %q: %w", rk.NewDir, err)
	}
	if len(destCaptures) > 0 || len(destDirs) > 0 {
		return fmt.Errorf("%q is no longer empty; refusing to merge two projects' captures", rk.NewDir)
	}
	return nil
}

// repointOne rewrites one link to follow the captures that moved, and rewrites
// the record behind it so the two never disagree about where the link points.
func repointOne(st *state.File, rp Repoint, now time.Time) error {
	target, err := linkTarget(rp.Path)
	if err != nil {
		return err
	}
	if target != rp.From {
		return fmt.Errorf("repointing %s/%s: %q points at %q, not the %q this run planned against", rp.Project, rp.Name, rp.Path, target, rp.From)
	}
	if fi, err := os.Lstat(rp.To); err != nil {
		return fmt.Errorf("repointing %s/%s: the moved capture is not at %q: %w", rp.Project, rp.Name, rp.To, err)
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("repointing %s/%s: %q is not a regular file (%v)", rp.Project, rp.Name, rp.To, fi.Mode())
	}
	if err := linker.ReplaceLink(rp.To, rp.Path); err != nil {
		return fmt.Errorf("repointing %s/%s: %w", rp.Project, rp.Name, err)
	}
	return rerecord(st, rp, now)
}

// rerecord moves a record's target along with the link it describes.
//
// The origin is carried across explicitly rather than defaulted, and an
// unrecognised one is an error: defaulting to created would widen what a later
// prune may remove, which is the one thing the two origins exist to keep apart.
// The hash is kept because the file moved unchanged.
func rerecord(st *state.File, rp Repoint, now time.Time) error {
	rec, ok := st.LookupLink(rp.Path)
	if !ok {
		return nil
	}
	link := state.Link{Key: rec.Key, Path: rec.Path, Target: rp.To, Hash: rec.Hash}

	var err error
	switch rec.Origin {
	case state.OriginCreated:
		_, err = st.RecordCreated(link, now)
	case state.OriginAdopted:
		_, err = st.RecordAdopted(link, now)
	default:
		return fmt.Errorf("repointing %s/%s: its record carries origin %q, which nothing here may re-file", rp.Project, rp.Name, rec.Origin)
	}
	if err != nil {
		return fmt.Errorf("repointing %s/%s: rewriting its record: %w", rp.Project, rp.Name, err)
	}
	return nil
}

// linkTarget reads a symlink the way scan.Classify does: the literal target,
// made absolute against the link's own directory and cleaned, with no chain
// followed. Comparing a resolved target against a recorded one would call a
// correct link diverged the moment anything above it became a symlink.
func linkTarget(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("reading the link %q: %w", path, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return filepath.Clean(target), nil
}
