// Package testfs builds filesystem fixtures the test suites cannot build with
// permission bits.
//
// It exists for one reason, recorded in issue #11: the CI gate runs as root,
// and root's directory walk is not restricted by permission bits. Five tests
// covering the same property — an unreadable subtree must never read as a
// clean result — built their fixture with chmod 000, so all five skipped in CI
// on `os.Geteuid() == 0`. `go test` without -v prints nothing for a skip, so a
// green pipeline said "everything passed" when it meant "everything that ran
// passed", and the property those five guard is the exact defect that failed
// import in review: exit 0 while paths went unread.
//
// internal/linker never had that problem, and not by luck. Its equivalent
// tests break their fixtures structurally — a .git *file* pointing at a
// missing gitdir, a repo with its HEAD commit object deleted — and both fail
// identically for root. That package reports the same coverage in CI as it
// does locally; the three that used chmod did not. This package gives the walk
// tests the same property. A test that runs everywhere beats a test that fails
// loudly about not running.
//
// This package imports testing and so must never be imported by non-test code;
// linking testing into a real binary registers the test flags. It lives under
// internal/ so that cannot happen outside this module, and nothing but _test.go
// files imports it. (The same arrangement the standard library uses for
// internal/testenv.)
package testfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// One path component. Comfortably under NAME_MAX (255 on Linux and macOS) so
// the individual mkdir always succeeds, and long enough that a couple of dozen
// of them clear the largest PATH_MAX in practice.
const segmentLen = 200

// Enough levels to clear PATH_MAX on Linux (4096) from a deep temp-dir prefix,
// with slack. macOS (1024) gets there in five. The loop is bounded rather than
// unbounded so that a platform where the limit never bites fails the test with
// a description of what happened, instead of filling the disk.
const maxDepth = 64

// UnreadableSubtree creates a directory beneath parent that exists, is reported
// as a directory by its parent's readdir, and cannot be opened by path by any
// process — root included. filepath.WalkDir descends to it, os.ReadDir fails,
// and the walk's error branch runs exactly as it did under chmod 000.
//
// It returns that directory's path, and an *os.Root through which the caller
// can still plant whatever the walk must fail to find. The root is closed at
// the end of the test.
//
// The mechanism is path length, not permission. open() rejects a path longer
// than PATH_MAX with ENAMETOOLONG in the kernel's path resolver, before any
// inode or credential is consulted, so there is no privilege that bypasses it
// — unlike the mode bits, which root's CAP_DAC_OVERRIDE ignores, which is the
// whole bug. The directory stays reachable through an openat chain, which is
// how it is created and how the returned root reaches inside it, so this is
// genuinely "cannot be read through its path" and not "is not there".
//
// Rejected alternatives, each tried against the walk rather than assumed:
//
//   - chmod 000: the original fixture. Inert under root.
//   - a symlink loop: filepath.WalkDir does not follow symlinks, so it reports
//     the entry as a link and never attempts to read through it — the walk
//     records no problem at all. A loop is a fine fixture for scan.Classify,
//     which does resolve links, and no fixture whatsoever for the walk.
//   - a dangling symlink, or a plain file where a directory is expected: same
//     reason. The walk sees a non-directory and moves on without an error.
//   - unlinking the directory mid-walk: ENOENT does reach the error branch,
//     but only deterministically from inside a walk callback, and these walks
//     are internal to the package under test with no hook to reach them.
func UnreadableSubtree(t testing.TB, parent string) (string, *os.Root) {
	t.Helper()

	seg := strings.Repeat("u", segmentLen)

	cur, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatalf("opening %s: %v", parent, err)
	}
	curPath := parent

	for depth := 0; depth < maxDepth; depth++ {
		// Created through the parent's descriptor, so the syscall only
		// ever sees a 200-byte relative name however long curPath has
		// grown. Creating it by absolute path would fail for the same
		// reason reading it is about to.
		if err := cur.Mkdir(seg, 0o755); err != nil {
			_ = cur.Close()
			t.Fatalf("creating level %d of the fixture: %v", depth, err)
		}
		nextPath := filepath.Join(curPath, seg)

		next, err := os.OpenRoot(nextPath)
		if err == nil {
			_ = cur.Close()
			cur = next
			curPath = nextPath
			continue
		}

		// The directory was created a line ago, so the only reason its
		// path can fail to open is the length of that path. Anything
		// else means the fixture is not what it claims to be, and a
		// test asserting on it would be asserting on the wrong thing.
		if !errors.Is(err, syscall.ENAMETOOLONG) {
			_ = cur.Close()
			t.Fatalf("level %d exists but did not fail to open for the expected reason: %v", depth, err)
		}

		inside, ierr := cur.OpenRoot(seg)
		_ = cur.Close()
		if ierr != nil {
			t.Fatalf("opening the unreadable directory through its parent's descriptor: %v", ierr)
		}
		t.Cleanup(func() { _ = inside.Close() })
		return nextPath, inside
	}

	_ = cur.Close()
	t.Fatalf("a path of %d bytes still opened after %d levels: this platform's PATH_MAX is larger than the fixture assumes",
		len(curPath), maxDepth)
	return "", nil
}

// Longest pad segment the tuning loop below may use. NAME_MAX is 255 on both
// Linux and macOS, so 250 is the largest safe single component — and it has to
// exceed segmentLen, or the reachable pad lengths at two adjacent levels would
// leave a gap the window could fall into.
const padMax = 250

// UnreadableAgentsDir plants a project directory whose .claude/agents the walk
// can *find* but not *read*, and returns that agents directory's path.
//
// This is a different fixture from UnreadableSubtree and it is not
// interchangeable with it. UnreadableSubtree makes a whole subtree unopenable,
// so filepath.WalkDir fails on its way in and the error reaches the walk's own
// error branch, which is already handled and already tested. The gap this one
// reproduces is narrower and was invisible because of it:
//
//	WalkDir identifies ".claude/agents" from its PARENT's readdir — the dirent
//	says "directory", and nothing has opened it — and the callback returns
//	fs.SkipDir immediately. So the walk never opens the agents directory, never
//	fails, and hands back a directory it has not looked inside. Whatever reads
//	it next is the first thing to meet the failure, and if that reader has no
//	way to report one, the scan says "this project has no agent files".
//
// It is the exact fixture needed to distinguish an empty agents directory from
// an unreadable one, which is issue #12's shape with no git in it.
//
// The mechanism is the same as UnreadableSubtree's — path length, not
// permission, so root is not exempt — but it has to be tuned rather than
// merely exceeded. The window is seven bytes wide: len(".claude") is 7 and
// len("/agents") is 7, so a prefix has to land where the .claude path is still
// within PATH_MAX and the agents path beneath it is not. A 200-byte segment
// steps straight over that, which is why this cannot just call
// UnreadableSubtree and append.
//
// Tuning needs no trial directories. open() rejects an over-long path in the
// kernel's resolver before any inode is consulted, so ENAMETOOLONG comes back
// for a path that does not exist just as readily as for one that does — the
// error is a pure function of the length. The search below therefore stats
// paths that were never created, and only the single winning layout is built.
func UnreadableAgentsDir(t testing.TB, parent string) string {
	t.Helper()

	seg := strings.Repeat("u", segmentLen)

	// Descend in coarse segments, then stop as soon as the *pad* can reach
	// the window — not as soon as another coarse segment would overflow.
	//
	// The difference is the whole fix, and getting it wrong is what broke
	// this on Linux. Stopping when the next 200-byte segment no longer fits
	// leaves however many bytes happen to remain, which is a property of
	// PATH_MAX modulo 201 and nothing else. On macOS (PATH_MAX 1024) that
	// happened to leave enough room; on Linux (4096) the descent stopped at
	// 4093 bytes with three bytes left, and ".claude" needs eight. There was
	// no pad length that could recover, because the budget was already spent
	// before the pad was applied.
	//
	// The condition below asks the question that actually matters: can the
	// longest permitted pad push ".claude" past the limit from here? While
	// the answer is no there is plenty of room and a coarse segment is safe.
	// The moment it becomes yes, the window is within the pad's reach and
	// the descent must stop so the pad can be tuned to byte granularity.
	//
	// That the *shortest* pad still fits at that point is guaranteed rather
	// than hoped for. The previous iteration had curPath+1+padMax+8 <= limit,
	// and a coarse step adds segmentLen+1 = 201, so the shortest pad needs
	// curPath+211 — comfortably inside, because padMax (250) exceeds the step
	// (201). The two constants are related for this reason and not by
	// coincidence; padMax must stay larger than segmentLen or consecutive
	// steps leave a gap the window can fall into.
	//
	// No PATH_MAX value is assumed anywhere. tooLong probes it, and probing
	// costs nothing and creates nothing because ENAMETOOLONG is decided in
	// the kernel's path resolver before any inode is consulted.
	cur, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatalf("opening %s: %v", parent, err)
	}
	defer func() { _ = cur.Close() }()
	curPath := parent

	maxPad := strings.Repeat("p", padMax)
	for depth := 0; !tooLong(filepath.Join(curPath, maxPad, ".claude")); depth++ {
		if depth >= maxDepth {
			t.Fatalf("a path of %d bytes plus a %d-byte pad still leaves .claude reachable after %d levels: this platform's path limit is larger than the fixture assumes",
				len(curPath), padMax, maxDepth)
		}
		if err := cur.Mkdir(seg, 0o755); err != nil {
			t.Fatalf("creating level %d of the fixture: %v", depth, err)
		}
		nr, err := cur.OpenRoot(seg)
		if err != nil {
			t.Fatalf("opening level %d through its parent's descriptor: %v", depth, err)
		}
		_ = cur.Close()
		cur, curPath = nr, filepath.Join(curPath, seg)
	}

	// The largest pad that still leaves .claude openable. One byte more and
	// .claude itself would be out of reach, which would make this
	// UnreadableSubtree again; "/agents" is seven bytes more than that, so
	// it lands past the limit. Both halves are asserted below rather than
	// reasoned about.
	pad, ok := longestPad(curPath)
	if !ok {
		// Unreachable given the descent above, which stops precisely when
		// the window comes into range — so if it ever fires, the descent's
		// invariant is what broke, and that is what the message has to say.
		// Lengths rather than paths: these are thousands of bytes each, and
		// a failure that has to be scrolled past is a failure people learn
		// to ignore.
		t.Fatalf("no pad length in 1..%d puts <base>/<pad>/.claude inside the path limit while leaving .claude/agents outside it.\n"+
			"  base length:      %d bytes\n"+
			"  probed limit:     %d bytes\n"+
			"  window needed:    base+pad+1 in %d..%d bytes\n"+
			"The descent is supposed to stop exactly when this window is reachable, so this means its stopping condition no longer holds on this platform.",
			padMax, len(curPath), pathLimit(parent), pathLimit(parent)-len(".claude/agents"), pathLimit(parent)-len(".claude"))
	}

	project := filepath.Join(curPath, pad)
	claude := filepath.Join(project, ".claude")
	agents := filepath.Join(claude, "agents")

	if err := cur.MkdirAll(filepath.Join(pad, ".claude", "agents"), 0o755); err != nil {
		t.Fatalf("planting the agents directory: %v", err)
	}
	// An override sitting where the scan cannot see it. The point of the
	// fixture is that a scan which cannot read this directory must not
	// report the project as holding nothing at risk, so there has to be
	// something at risk in it.
	inside, err := cur.OpenRoot(filepath.Join(pad, ".claude", "agents"))
	if err != nil {
		t.Fatalf("opening the planted agents directory through its parent's descriptor: %v", err)
	}
	defer func() { _ = inside.Close() }()
	if err := inside.WriteFile("hidden.md", []byte("an unprotected override\n"), 0o644); err != nil {
		t.Fatalf("planting the hidden agent: %v", err)
	}

	// The three properties every caller depends on, checked rather than
	// assumed. If any stops holding, a test built on this would pass
	// because nothing happened — the exact failure this package exists to
	// prevent.
	ents, err := os.ReadDir(claude)
	if err != nil {
		t.Fatalf("the .claude directory must stay readable or the walk never reaches agents at all: %v", err)
	}
	var sawAgents bool
	for _, e := range ents {
		if e.Name() == "agents" && e.IsDir() {
			sawAgents = true
		}
	}
	if !sawAgents {
		t.Fatalf("%s does not report an \"agents\" directory; the walk would never identify it", claude)
	}
	if _, err := os.ReadDir(agents); err == nil {
		t.Fatalf("%s is readable, so the fixture reproduces nothing: a test using it would be asserting against an ordinary directory", agents)
	}

	return agents
}

// tooLong reports whether a path is over the platform's limit, without needing
// it to exist. ENAMETOOLONG is decided by the kernel's path resolver before any
// inode is looked at, so a nonexistent short path answers ENOENT and a
// nonexistent long one answers ENAMETOOLONG.
func tooLong(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, syscall.ENAMETOOLONG)
}

// pathLimit returns the longest path length under base that does not fail with
// ENAMETOOLONG — this platform's effective PATH_MAX as the kernel applies it.
//
// It is derived, never assumed: the two values in play differ by 4x (1024 on
// macOS, 4096 on Linux) and hardcoding either is what made the first version of
// this fixture pass on one and fail on the other. Nothing is created; the probe
// is a pure length test.
//
// Used only to explain a failure. The fixture itself never needs the number —
// it asks tooLong the specific questions it cares about — but a message saying
// "the limit is 4096 and the window is 4081..4088" is actionable, where one
// quoting two four-kilobyte paths is not.
func pathLimit(base string) int {
	if tooLong(base) {
		return len(base)
	}
	// An upper bound comfortably past any real PATH_MAX. The search is over
	// lengths, so it costs a handful of Lstat calls and creates nothing.
	lo, hi := len(base), 1<<16
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if tooLong(probePath(base, mid)) {
			hi = mid - 1
		} else {
			lo = mid
		}
	}
	return lo
}

// probePath returns a path under base of total length exactly n.
//
// It spreads the length over components of at most segmentLen bytes rather than
// one long one, which is the difference between measuring PATH_MAX and
// measuring NAME_MAX. A single component over 255 bytes fails with
// ENAMETOOLONG for its own reason, so a probe built that way would report the
// per-component limit and the caller would never learn the total-path one.
//
// The one length it cannot represent is len(base)+1: a component costs at least
// two bytes, the separator and one character. That n overshoots by exactly one,
// which is the smallest possible miss and harmless to a binary search over a
// monotone predicate.
func probePath(base string, n int) string {
	b := base
	for len(b)+1 < n {
		chunk := n - len(b) - 1
		if chunk > segmentLen {
			chunk = segmentLen
		}
		b += "/" + strings.Repeat("p", chunk)
	}
	if len(b) < n {
		b += "/p"
	}
	return b
}

// longestPad finds the longest single component that can be added to base while
// leaving base/<pad>/.claude reachable and base/<pad>/.claude/agents not.
//
// The predicate is monotone — every extra byte can only take a path further
// past the limit, never back inside it — so a binary search is exact rather
// than approximate.
func longestPad(base string) (string, bool) {
	fits := func(n int) bool {
		return !tooLong(filepath.Join(base, strings.Repeat("p", n), ".claude"))
	}
	if !fits(1) {
		// Even the shortest pad puts .claude out of reach. The caller
		// descended one segment too far to tune from here.
		return "", false
	}
	lo, hi := 1, padMax
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	// lo is now the longest pad that keeps .claude reachable. "/agents" is
	// seven bytes more, so it should be past the limit — but that is only
	// guaranteed when the search actually found a boundary. If the whole pad
	// range stayed inside PATH_MAX the caller descended too shallow, lo is
	// simply padMax, and agents fits too. One check covers both: either the
	// window was hit or it was not.
	pad := strings.Repeat("p", lo)
	if !tooLong(filepath.Join(base, pad, ".claude", "agents")) {
		return "", false
	}
	return pad, true
}

// PlantAgentsDir creates project/.claude/agents inside an unreadable subtree.
//
// It is a project the walk cannot reach. Whether it holds anything is beside
// the point: a scan that cannot enter it cannot say it is empty, which is the
// whole property the callers assert.
func PlantAgentsDir(t testing.TB, inside *os.Root, project string) string {
	t.Helper()

	dir := filepath.Join(project, ".claude", "agents")
	if err := inside.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("planting %s: %v", dir, err)
	}
	return dir
}

// PlantAgent is PlantAgentsDir plus an agent file in it, for the callers whose
// point is that the unreachable project may hold an unprotected override — the
// file that would be lost, sitting where nothing can see it.
func PlantAgent(t testing.TB, inside *os.Root, project, name, content string) {
	t.Helper()

	dir := PlantAgentsDir(t, inside, project)
	if err := inside.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatalf("planting %s: %v", name, err)
	}
}
