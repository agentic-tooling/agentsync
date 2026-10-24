package testfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestUnreadableSubtree_IsUnreadableAndStopsTheWalk pins the two claims the
// fixture makes, because five tests in three packages now rest on them and a
// fixture that quietly stopped being unreadable would take their assertions
// with it, allowing those tests to pass without exercising an unreadable path.
func TestUnreadableSubtree_IsUnreadableAndStopsTheWalk(t *testing.T) {
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	mustMkdir(t, locked)
	mustMkdir(t, filepath.Join(root, "readable"))

	unreadable, inside := UnreadableSubtree(t, locked)
	PlantAgent(t, inside, "proj", "hidden", "unreachable\n")

	// Claim one: the path cannot be read. Not by this process, and — since
	// the kernel rejects it during path resolution, before it looks at any
	// inode — not by a privileged one either.
	_, err := os.ReadDir(unreadable)
	if err == nil {
		t.Fatalf("a %d-byte path opened; the fixture is not unreadable", len(unreadable))
	}
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("unreadable for the wrong reason, so the fixture proves nothing about root: %v", err)
	}

	// Claim two: a walk reaches it, fails on it, and reports it as a
	// directory — which is what puts the walk in the fs.SkipDir branch the
	// five tests exercise. A fixture the walk never attempts to read (a
	// symlink loop, a dangling link) would record no problem at all.
	var (
		problems []string
		sawDir   bool
	)
	werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			problems = append(problems, path)
			if d != nil && d.IsDir() {
				sawDir = true
				return fs.SkipDir
			}
			return nil
		}
		return nil
	})
	if werr != nil {
		t.Fatalf("the walk aborted instead of recording a problem: %v", werr)
	}
	if len(problems) != 1 || problems[0] != unreadable {
		t.Fatalf("problems = %v, want exactly one naming %q", problems, unreadable)
	}
	if !sawDir {
		t.Error("the walk did not report the failing entry as a directory, so fs.SkipDir would not be reached")
	}

	// And what was planted really is out of the walk's reach: reachable
	// through the descriptor, invisible by path.
	if _, err := inside.Stat(filepath.Join("proj", ".claude", "agents", "hidden.md")); err != nil {
		t.Errorf("the planted file is not there even through the descriptor: %v", err)
	}
}

// TestNoTestGuardsOnRoot is the recurrence guard.
//
// Permission-based unreadability fixtures do not work under root. Five tests
// reached for chmod 000 to make a directory unreadable, and
// every one of them then had to skip under root, invisibly, because `go test`
// prints nothing for a skip. The considered alternative — an env var CI sets
// that turns a privilege-guarded skip into a failure — makes the gap visible
// but still does not run the test, and it only works if CI remembers to set
// it.
//
// This catches it a step earlier and everywhere: at authoring time, on every
// machine, in the ordinary `go test ./...`. A privilege guard is the signal
// that a fixture depends on permission bits, and permission bits are exactly
// what root ignores.
//
// # The escape hatch, and why it is per-site
//
// Not every use of the effective uid is a privilege guard. A test asserting
// chown semantics has a legitimate reason to ask, and the check cannot tell
// the two apart — it is a text search, and it fires on a comment or a string
// literal just as readily as on a call.
//
// So an exemption is granted one line at a time, by marking it:
//
//	//testfs:allow-geteuid asserts chown semantics; does not skip
//	if os.Geteuid() == 0 { ... }
//
// The marker goes on the offending line or the line above it, and the
// justification after it is required — a bare marker is reported the same as
// no marker, because "someone decided this was fine" is the only thing that
// makes an exemption worth more than deleting the check.
//
// Per-site rather than per-file, and certainly rather than all-or-nothing: the
// realistic way a guard like this dies is that one legitimate use appears and
// the whole check gets deleted to accommodate it. A marker costs a line and
// leaves the other 40 files still guarded.
func TestNoTestGuardsOnRoot(t *testing.T) {
	// Two levels up from internal/testfs is the module root. Anchored to
	// go.mod rather than assumed, so moving this package fails loudly here
	// instead of quietly scanning nothing and passing.
	repoRoot := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("expected the module root two levels up: %v", err)
	}

	// Split so this file does not report itself. Writing the call spelled
	// out here would be the one true positive the check must not have.
	// The marker below needs no such treatment: it does not contain the
	// needle, which is what lets it be quoted in the doc comment above.
	needle := "Gete" + "uid()"
	const marker = "//testfs:allow-geteuid"

	var offenders []string

	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}

		offenders = append(offenders, geteuidOffences(path, string(b), needle, marker)...)
		return nil
	})
	if err != nil {
		t.Fatalf("scanning the tree: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("a test that decides what to do from the effective uid skips under root, and CI runs as root — see issue #11, and testfs.UnreadableSubtree for the structural fixture that replaced the five that did:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// geteuidOffences reports every line of src that asks the effective uid
// without a justified exemption. It is separated from the walk so the
// exemption rules can be pinned directly, on strings, by the test below.
//
// Worth pinning rather than eyeballing: every mistake in here fails open. A
// marker match that is too loose exempts lines nobody exempted, and the guard
// goes on passing while it stops guarding — the same shape of silence as the
// skip it was written to prevent.
func geteuidOffences(path, src, needle, marker string) []string {
	var out []string
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if !strings.Contains(line, needle) {
			continue
		}
		// The marker is accepted on the offending line or the one above
		// it, because which reads better depends on how long the line
		// already is.
		exemption := ""
		switch {
		case strings.Contains(line, marker):
			exemption = line
		case i > 0 && strings.Contains(lines[i-1], marker):
			exemption = lines[i-1]
		default:
			out = append(out, fmt.Sprintf("%s:%d: asks the effective uid with no %s marker", path, i+1, marker))
			continue
		}
		if _, why, _ := strings.Cut(exemption, marker); strings.TrimSpace(why) == "" {
			out = append(out, fmt.Sprintf("%s:%d: %s with no justification after it", path, i+1, marker))
		}
	}
	return out
}

func TestGeteuidOffences_ExemptionRules(t *testing.T) {
	const marker = "//testfs:allow-geteuid"

	// Assembled from parts, for the same reason the walk assembles it: spelled
	// out, these fixtures would be reported by the very check they exist to
	// test, and the obvious fix would be to exempt them — which would leave
	// the exemption rules pinned by nothing.
	needle := "Gete" + "uid()"
	use := "if os." + needle + " == 0 {\n"

	cases := []struct {
		name string
		src  string
		want int
	}{
		{"no mention at all", "func f() {}\n", 0},
		{"bare use", use, 1},
		{"marked on the line above", marker + " asserts chown\n" + use, 0},
		{"marked on the same line", "if os." + needle + " == 0 { " + marker + " asserts chown\n", 0},
		{"marker with no justification", marker + "\n" + use, 1},
		{"marker with only whitespace after it", marker + "   \n" + use, 1},
		// One exemption covers one line, never the file. This is the
		// property that makes the hatch narrow, and the one a
		// file-scoped implementation would quietly lose.
		{"a marked use does not cover a later bare one",
			marker + " justified\n" + use + "}\n" + use, 1},
		// The check is a text search, so it fires on comments and
		// strings too. That is deliberate — it cannot tell them apart,
		// and the marker is how a reader says which it is.
		{"a mention in a comment still needs marking", "// os." + needle + " is interesting\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := geteuidOffences("x_test.go", tc.src, needle, marker)
			if len(got) != tc.want {
				t.Errorf("offences = %v, want %d", got, tc.want)
			}
		})
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// TestPathLimit_IsDerivedAndExact covers the probe that explains a failure of
// UnreadableAgentsDir's tuning.
//
// It is diagnostic-only code, which is exactly why it is worth a test: a wrong
// number here would not fail anything, it would just send whoever hits the
// t.Fatalf looking in the wrong place.
//
// The number it reports is the *effective* limit for paths under this base,
// not the platform constant. On macOS a temp dir reached through /var resolves
// to /private/var, eight bytes longer, so the probe reports 1016 where PATH_MAX
// is 1024 — and that is the useful answer, because the fixture's own paths go
// through the same symlink.
func TestPathLimit_IsDerivedAndExact(t *testing.T) {
	base := t.TempDir()
	limit := pathLimit(base)

	if limit <= len(base) {
		t.Fatalf("pathLimit returned %d for a %d-byte base, which leaves no room for anything", limit, len(base))
	}
	if tooLong(probePath(base, limit)) {
		t.Errorf("a path of the probed limit (%d bytes) is itself too long, so the limit is overstated", limit)
	}
	if !tooLong(probePath(base, limit+1)) {
		t.Errorf("a path one byte over the probed limit (%d bytes) still opens, so %d is not the boundary", limit+1, limit)
	}

	// probePath must hit its target length exactly, or the binary search
	// above is measuring something other than what it reports. The one
	// exception is len(base)+1, which no component can express.
	for _, n := range []int{len(base) + 2, len(base) + 201, len(base) + 202, len(base) + 500} {
		if got := len(probePath(base, n)); got != n {
			t.Errorf("probePath(base, %d) produced %d bytes; the length search would be off by %d", n, got, got-n)
		}
	}

	// Spread across components rather than concentrated in one. A single
	// component over NAME_MAX (255) fails for its own reason, and a probe
	// built that way would measure the per-component limit instead of the
	// whole-path one.
	for _, part := range strings.Split(probePath(base, len(base)+1000), "/") {
		if len(part) > 255 {
			t.Fatalf("probePath emitted a %d-byte component; that hits NAME_MAX, not PATH_MAX", len(part))
		}
	}
}

func TestDirWithRoom_LeavesExactlyThatRoom(t *testing.T) {
	const room = 22
	dir := DirWithRoom(t, t.TempDir(), room)

	fits := filepath.Join(dir, strings.Repeat("f", room-1))
	if err := os.WriteFile(fits, nil, 0o644); err != nil {
		t.Fatalf("a %d-byte name should fit: %v", room-1, err)
	}
	err := os.WriteFile(filepath.Join(dir, strings.Repeat("f", room)), nil, 0o644)
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("a %d-byte name should fail with ENAMETOOLONG, got %v", room, err)
	}

	// The self-check DirWithRoom relies on must reject a room off by one either
	// way, and an ordinary directory.
	for _, r := range []int{room - 1, room + 1} {
		if hasRoom(dir, r) {
			t.Errorf("hasRoom accepted %d bytes for a directory with %d", r, room)
		}
	}
	if hasRoom(t.TempDir(), room) {
		t.Error("hasRoom accepted a short directory")
	}
}
