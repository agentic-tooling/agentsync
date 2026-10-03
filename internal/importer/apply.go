package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// Apply performs every write the plan selected, and nothing else.
//
// It returns the items actually written and one error per failure. Conflicts
// and skipped items are not touched: a conflict is a refusal, not a pending
// write, and the caller reports it separately.
//
// A failure does not stop the run. The items are independent files in
// different projects, so aborting on the first one would leave the rest
// uncaptured for a reason that has nothing to do with them — capturing nine of
// ten is strictly better than capturing none. Every failure is returned, and
// the caller must treat a non-empty result as a failed run rather than a
// partial success.
func (p Plan) Apply() ([]Item, []error) {
	written := make([]Item, 0, len(p.Items))
	var errs []error

	for _, item := range p.Items {
		if !item.Action.Writes() {
			continue
		}
		if err := importOne(item); err != nil {
			errs = append(errs, err)
			continue
		}
		written = append(written, item)
	}
	return written, errs
}

func importOne(item Item) error {
	satisfied, err := recheckDest(item)
	if err != nil {
		return err
	}
	if satisfied {
		// The destination already holds the bytes we would have written,
		// so there is nothing to do and the item counts as captured. This
		// has to short-circuit rather than fall through: on an import
		// placeCapture would meet its own destination and fail with
		// EEXIST, turning "someone captured this already" into a write
		// error. On an update it would rename identical bytes over
		// identical bytes for no reason.
		return nil
	}
	if err := copyAtomic(item.Entry.Path, item.Dest, item.SrcHash, item.Action); err != nil {
		return fmt.Errorf("importing %s/%s: %w", item.Entry.Project, item.Entry.Name, err)
	}
	return nil
}

// recheckDest re-examines the destination immediately before the write.
//
// The plan's verdict — nothing is there, or what is there is a stale capture —
// is what authorises the write, and it was formed during a scan that may have
// walked a hundred repositories. Re-asking replaces a minutes-old observation
// with a fresh one, which on this path is the difference between capturing a
// file and destroying one.
//
// It does not by itself close the window, and it never could: the gap between
// this check and the write spans the whole io.Copy and Sync below, so it is the
// duration of the copy rather than the microseconds the comment here used to
// claim. What closes it for an import is placeCapture, which puts the file down
// with os.Link — a create-if-absent, so the check and the write become one
// syscall. For an update the window stays open by necessity, because replacing
// is the point; what makes that survivable is commitGate, which requires the
// exact bytes about to be replaced to be recoverable from a commit.
//
// --update is not exempt from the content check either. The opt-in says
// "replace the stale capture I was shown", and the plan recorded which bytes
// those were in Item.DestHash. It is not a licence to replace whatever occupies
// the path later: the run that makes the destination worth preserving is
// precisely the one that put something else there.
//
// satisfied reports that the destination already holds the bytes this item
// would write, so the caller must skip the copy entirely rather than perform a
// write that is a no-op at best and an EEXIST at worst.
func recheckDest(item Item) (satisfied bool, err error) {
	fi, err := os.Lstat(item.Dest)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Absent is safe for both actions: an import expected this,
			// and an update loses nothing by creating the file instead.
			return false, nil
		}
		return false, fmt.Errorf("re-checking %q: %w", item.Dest, err)
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%q is not a regular file; refusing to replace it", item.Dest)
	}

	hash, err := scan.HashFile(item.Dest)
	if err != nil {
		return false, fmt.Errorf("re-hashing %q: %w", item.Dest, err)
	}
	switch {
	case hash == item.SrcHash:
		// Someone captured this exact content in the meantime. That is the
		// outcome we wanted, whichever action asked for it, so the item is
		// already satisfied and nothing is written.
		//
		// The commit gate does not apply here: it guards the destruction
		// of content, and nothing is destroyed when nothing is written.
		return true, nil
	case item.Action == ActionUpdate && hash == item.DestHash:
		// Byte-for-byte the stale capture the plan judged and the user
		// opted into replacing. This is the one branch where content is
		// about to be destroyed, so it is the one the commit gate guards.
		//
		// Re-asked here rather than trusted from the plan, for the same
		// reason the hash is: the plan's answer was formed before a scan
		// that may have walked a hundred repositories, and this is the
		// last point before the only copy of something is replaced. The
		// two branches above do not need it — an absent destination and a
		// destination already holding the source content both lose
		// nothing.
		if refusal, why := commitGate(item.Dest); refusal != RefusalNone {
			return false, fmt.Errorf("refusing to replace %q: %s", item.Dest, why)
		}
		return false, nil
	case item.Action == ActionUpdate:
		return false, fmt.Errorf("%q changed since the plan judged it stale; --update does not authorise replacing content this run never examined", item.Dest)
	default:
		return false, fmt.Errorf("%q gained different content since the plan was made; refusing to overwrite it", item.Dest)
	}
}

// copyAtomic writes src to dest through a temporary file in dest's own
// directory, then puts that temporary in place — see placeCapture, which is
// where the action decides between creating and replacing.
//
// A reader of dest sees either the whole old file or the whole new one, and a
// crash mid-copy leaves a temporary behind rather than a truncated capture — a
// backup that exists but is half a file is worse than one that is missing,
// because it looks like protection.
//
// The durability claim stops there, deliberately. The bytes are fsynced before
// the file is put in place, but the directory entry naming it is not, so a
// power cut can lose the link or the rename itself and leave the capture
// missing rather than truncated. That is the safe direction — the project file
// it was copied from is still on disk, and the next run captures it again — and
// it is why there is no dir.Sync() here. The syscall would cost an open and an
// fsync of the destination directory on every capture to protect against an
// outcome whose remedy is already "re-run import". Stated rather than left
// implied, because the guarantee above is easy to read as covering more than it
// does.
//
// The temporary must live in dest's directory: both rename and link work only
// within a filesystem, and the system temp dir is routinely a different one.
//
// expect is the SHA-256 the plan classified src by. The bytes are hashed as
// they are copied and the result compared before anything is renamed into
// place, so a source that changed between the two phases is refused rather than
// captured. Nothing else re-reads src, and re-reading it would not help: a
// second read is a second chance to see different bytes. Hashing the copy is
// the only check that describes the file we are actually about to store.
//
// This also covers the source becoming a symlink after the scan classified it.
// os.Open follows one, and an Lstat before it would only move the race; what
// makes the capture safe is that the bytes arriving through that fd still hash
// to what was classified.
//
// An empty expect skips the comparison. It means the plan had no hash to
// classify by — the caller establishes it via sourceHash, so in practice this
// is only reachable for a hand-made item — and inventing a check against
// nothing would refuse every such write.
func copyAtomic(src, dest, expect string, action Action) (err error) {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".agentsync-import-*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Any failure past this point must not leave the temporary behind. Not
	// because it would be committed — .gitignore covers *.tmp — but because it
	// would accumulate silently beside the captures, one per failed run, with a
	// name that says nothing about when or why it was left. On the failure path
	// the Close error is moot — the file is about to be removed either way,
	// and the failure being reported is the one that matters.
	//
	// This keys on the named return, so it does not run during a panic unwind:
	// err is nil there. Knowingly left that way. Every error-return path below
	// was traced and is covered by it; a panic in this function would have to
	// come out of the runtime rather than out of anything written here; and the
	// residue would be a single dot-prefixed temporary that .gitignore already
	// covers, that cannot be committed as an agent, and that nothing reads.
	// Restructuring around recover() to catch it would put a recover on the
	// write path, which is the worse trade — it turns a crash that stops the
	// run into one that carries on past it.
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %q: %w", src, err)
	}
	// Read-only fd: nothing is buffered, so Close can only report a fault we
	// could not act on. Ignored explicitly rather than in .golangci.yml, so
	// errcheck keeps flagging the write-side Close below.
	defer func() { _ = in.Close() }()

	// Fstat on the open fd, not Lstat on the path: this describes the file we
	// are about to read, with no second lookup to race. An Lstat before the
	// open would only move the race; the content hash below is what actually
	// makes the capture safe, and it already covers a source that turned into
	// a symlink, since identical bytes mean the classification still holds.
	//
	// What this catches instead is a source that is not a regular file at all.
	// A device (/dev/zero) opens fine and would stream an unbounded number of
	// bytes into a temp file under project-agents/ before the hash could
	// reject any of it. A directory becomes a clear message rather than an
	// opaque read error.
	//
	// It does NOT rescue a FIFO. os.Open on a FIFO with no writer blocks in
	// the open, one line above, so this check is never reached — measured, not
	// assumed. A FIFO source hung the run before this check existed and still
	// does. Closing that would mean opening with O_RDONLY|O_NONBLOCK, which
	// costs a syscall import and Unix-only portability.
	fi, err := in.Stat()
	if err != nil {
		return fmt.Errorf("examining %q: %w", src, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%q is no longer a regular file; refusing to capture it", src)
	}

	sum := sha256.New()
	if _, err = io.Copy(io.MultiWriter(tmp, sum), in); err != nil {
		return fmt.Errorf("copying %q: %w", src, err)
	}
	// Verified before the rename, so a mismatch leaves the destination as it
	// was rather than holding content nothing ever classified. On --update
	// that is the whole point: replacing a capture with unexamined bytes
	// destroys the only copy of something and stores something else.
	if got := hex.EncodeToString(sum.Sum(nil)); expect != "" && got != expect {
		return fmt.Errorf("%q changed while being captured (%s, planned against %s); nothing was written",
			src, got, expect)
	}
	// Sync before the rename. Without it the rename can reach disk while the
	// bytes it points at have not, which is precisely the truncated capture
	// this function exists to prevent.
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %q: %w", tmpName, err)
	}
	// CreateTemp makes the file 0600. Captures are ordinary readable files in
	// a git repository like every other file here.
	//
	// This is fchmod on the open fd, which sets 0644 outright and does not
	// consult the umask. A capture is therefore 0644 even under `umask 077`,
	// where os.WriteFile(dest, b, 0o644) would have produced 0600. That is the
	// behaviour we want — the mode of a file in a shared repository should not
	// depend on which shell happened to run the import — but it is a real
	// difference from the obvious alternative, so it is written down rather
	// than left to be rediscovered by whoever changes this line.
	if err = tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("setting mode on %q: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", tmpName, err)
	}
	return placeCapture(tmpName, dest, action)
}

// placeCapture puts the finished temporary at dest, replacing what is there
// only when replacing is what was asked for.
//
// ActionImport uses os.Link, which is the portable create-if-absent this path
// needs: it fails with EEXIST rather than replacing, so a destination that
// appeared at any point since the plan was made is refused instead of
// destroyed. That closes the window recheckDest can only narrow — the gap
// between the recheck and the write is the whole copy, not microseconds — by
// making the check and the write the same syscall. os.Rename cannot do this at
// all: it replaces unconditionally, and there is no portable rename-if-absent.
// Link is the tool that exists instead.
//
// The extra Remove is what it costs. Link leaves two names for one inode, so
// the temporary has to be unlinked afterwards. If that fails the capture is
// nonetheless in place and correct, and the error says so — a caller told only
// "failed" would re-run believing nothing had been written. err is non-nil at
// that point, so copyAtomic's cleanup retries the Remove, which is what we
// want; it cannot touch dest, since unlinking one name of a hard-linked pair
// leaves the other.
//
// ActionUpdate keeps os.Rename, because replacing is the point there and a
// rename is the only way to do it with no moment where dest does not exist.
// What authorises that replacement is settled before this function: recheckDest
// compares the destination against the bytes the plan judged, and commitGate
// requires those exact bytes — not merely the path — to be recoverable from a
// commit.
//
// A filesystem without hard links (some FUSE mounts, exFAT) fails an import
// here rather than falling back to Rename. The fallback would silently restore
// the window this closes, on exactly the filesystems where nobody would think
// to look for it.
func placeCapture(tmpName, dest string, action Action) error {
	if action == ActionUpdate {
		if err := os.Rename(tmpName, dest); err != nil {
			return fmt.Errorf("renaming %q to %q: %w", tmpName, dest, err)
		}
		return nil
	}

	if err := os.Link(tmpName, dest); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Only reachable for content this run genuinely has not seen.
			// A destination already holding the source bytes is caught
			// upstream by recheckDest, which reports the item satisfied so
			// this function is never called — without that, this message
			// would be flatly wrong in the one case where the run had in
			// fact examined the file and found it identical.
			return fmt.Errorf("%q appeared between the check and the write; refusing to replace content this run never examined", dest)
		}
		return fmt.Errorf("linking %q into place at %q: %w", tmpName, dest, err)
	}
	if err := os.Remove(tmpName); err != nil {
		return fmt.Errorf("%q was captured, but its temporary %q could not be removed: %w", dest, tmpName, err)
	}
	return nil
}
