package scan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AllowOutsideHomeFlag is the name of the CLI flag that opts a caller out of
// the home-plausibility check, without the leading dashes.
//
// It lives here so the diagnostics below and the flag the CLI actually
// registers cannot drift apart: a message telling a user to pass a flag that
// does not exist is worse than no message at all.
const AllowOutsideHomeFlag = "allow-outside-home"

// HomeStatus classifies the home directory a scan keys projects by.
//
// Validity is not the interesting question. Under `sudo -i`, $HOME is
// /var/root: a real, readable, resolvable directory that is simply not the
// home the user's projects live in. Every ProjectKey then falls through to the
// _outside-home/ namespace, so the tool compares against the wrong
// counterparts and — once import exists — would write a duplicate parallel
// backup tree.
//
// The signal that actually separates a wrong home from a right one is the keys
// themselves: if every project a command is about to act on keys under
// _outside-home/, the home is not the one those projects live in. That is a
// statement about the projects, never about the root they were reached
// through. Judging the root instead looks equivalent and is not — scanning /
// or /Users puts home *under* the root, which says nothing about where the
// projects are, and is exactly the shape `sudo -i agentsync scan --all` with
// a default tree of / takes.
type HomeStatus int

const (
	// HomeOK means at least one project keys relative to home, or there are
	// no projects to key at all. Nothing to report.
	HomeOK HomeStatus = iota

	// HomeUnset means no home directory could be determined at all
	// ($HOME empty or unset, or os.UserHomeDir failing).
	HomeUnset

	// HomeUnresolvable means a home directory was named but is not an
	// existing directory.
	HomeUnresolvable

	// HomeIsRoot means $HOME is the filesystem root, which is never a real
	// home directory. It is called out separately because it is the one
	// wrong home that does NOT produce _outside-home/ keys: every path is
	// under "/", so keys come out looking home-relative while actually
	// being absolute paths with the leading separator shaved off. Silent
	// re-keying into a third namespace is worse than the loud kind.
	HomeIsRoot

	// HomeImplausible means home is a perfectly good directory that is not
	// the one these projects live in: every project examined keys under
	// _outside-home/.
	//
	// This is the only status that can legitimately occur on a correct
	// machine — projects on /opt or an external volume — which is why it is
	// the only one the opt-out flag suppresses.
	HomeImplausible
)

func (s HomeStatus) String() string {
	switch s {
	case HomeOK:
		return "ok"
	case HomeUnset:
		return "unset"
	case HomeUnresolvable:
		return "unresolvable"
	case HomeIsRoot:
		return "filesystem-root"
	case HomeImplausible:
		return "implausible"
	default:
		return "invalid"
	}
}

// HomeCheck is the outcome of judging a Repo's home against the projects a
// command is about to act on. It carries the values it judged so a caller can
// report them: a user cannot fix a wrong $HOME they were never shown.
type HomeCheck struct {
	Status HomeStatus

	// Home is the resolved home directory actually in use for keying, which
	// is what a diagnostic must print. Empty when Status is HomeUnset.
	Home string

	// Root is the root the projects were found under, for diagnostics only.
	// It takes no part in the decision.
	Root string

	// Projects is how many agent directories were examined, and OutsideHome
	// how many of those key under _outside-home/. For HomeImplausible the
	// two are equal and non-zero — that is the evidence for the verdict, and
	// printing it is what lets a user check the claim rather than trust it.
	Projects    int
	OutsideHome int
}

// CheckHome classifies r.Home against the agent directories a command is about
// to act on.
//
// dirs are .claude/agents directories — Result.Dirs from a completed scan, or
// the subset a write path is about to touch. They are what makes the answer
// trustworthy: the check asks the same question ProjectKey answers, of the same
// directories, so it cannot warn about a re-keying that will not happen or stay
// silent about one that will.
//
// Passing no directories is never implausible. Nothing was keyed, so nothing
// was mis-keyed, and a scan that found no projects has no business complaining
// about where they would have gone.
//
// It makes no judgement about what to do — see RequireHome for the write-path
// policy and HomeCheck.Explain for the read-path warning.
//
// One wrong home it cannot detect: a home that is a genuine *ancestor* of the
// real one, such as $HOME=/Users with projects in /Users/you/Web. Those keys
// come out home-relative (you/Web/proj) and merely wrong, and nothing on disk
// distinguishes them from correct ones without already knowing the real home.
// HomeIsRoot catches the one instance of that shape which is knowable a priori.
func (r *Repo) CheckHome(root string, dirs []string) HomeCheck {
	c := HomeCheck{Root: resolvePath(root), Projects: len(dirs)}

	if r.Home == "" {
		c.Status = HomeUnset
		return c
	}

	c.Home = resolvePath(r.Home)
	if fi, err := os.Stat(c.Home); err != nil || !fi.IsDir() {
		c.Status = HomeUnresolvable
		return c
	}
	if filepath.Dir(c.Home) == c.Home {
		c.Status = HomeIsRoot
		return c
	}

	for _, dir := range dirs {
		if outsideHomeKey(ProjectKey(c.Home, dir)) {
			c.OutsideHome++
		}
	}
	// Some projects keyed relative to home means home is doing its job, even
	// if others did not: a scan of / on a correct machine legitimately finds
	// both ~/Web and /opt. Only "not one of them" indicts the home.
	if c.Projects > 0 && c.OutsideHome == c.Projects {
		c.Status = HomeImplausible
		return c
	}
	c.Status = HomeOK
	return c
}

// outsideHomeKey reports whether a ProjectKey landed in the reserved
// absolute-path namespace rather than being expressed relative to home.
func outsideHomeKey(key string) bool {
	head, _, _ := strings.Cut(key, "/")
	return head == keyOutsideHome
}

// OK reports whether there is nothing to say about this home.
func (c HomeCheck) OK() bool { return c.Status == HomeOK }

// SuppressedBy reports whether an explicit opt-out covers this status.
//
// Only HomeImplausible is suppressible. A user whose projects genuinely live
// on /opt or an external volume produces that status on every correct run, so
// it must be silenceable or the warning becomes noise that gets ignored — and
// a warning that is ignored protects nobody. The other statuses are never
// suppressed: no correct configuration has no home, a home that does not
// exist, or a home of "/", and the flag says "my projects are outside $HOME",
// which is not a claim about any of them.
func (c HomeCheck) SuppressedBy(allowOutsideHome bool) bool {
	return allowOutsideHome && c.Status == HomeImplausible
}

// Summary is a single-line description of what is wrong, naming $HOME as the
// cause and printing the value in use. It is empty when the check is OK.
func (c HomeCheck) Summary() string {
	outside := keyOutsideHome + "/"
	switch c.Status {
	case HomeUnset:
		return fmt.Sprintf("$HOME is empty or unset, so every project is keyed by absolute path under %q instead of relative to your home directory", outside)
	case HomeUnresolvable:
		return fmt.Sprintf("$HOME is %q, which is not an existing directory, so every project is keyed by absolute path under %q", c.Home, outside)
	case HomeIsRoot:
		return fmt.Sprintf("$HOME is %q, the filesystem root, which is never a real home directory: every project is keyed by a path that only looks home-relative and will not match the keys a real home produces", c.Home)
	case HomeImplausible:
		return fmt.Sprintf("every project found under %q keys under %q (%d of %d) — $HOME (%q) is not the home they live in, so these keys will not match the ones a correct $HOME produces",
			c.Root, outside, c.OutsideHome, c.Projects, c.Home)
	default:
		return ""
	}
}

// Advice is a single-line remedy for the status. Empty when the check is OK.
func (c HomeCheck) Advice() string {
	switch c.Status {
	case HomeUnset, HomeUnresolvable, HomeIsRoot:
		return "set $HOME to the home directory whose projects you mean to scan"
	case HomeImplausible:
		return fmt.Sprintf("if $HOME is wrong (sudo, cron, a container), fix it; if your projects genuinely live outside $HOME, pass --%s", AllowOutsideHomeFlag)
	default:
		return ""
	}
}

// Explain is the multi-line diagnostic a read-only command writes to stderr:
// what is wrong on the first line, what to do about it on the second. It is
// empty when the check is OK, so a caller can test it for emptiness rather
// than re-examining the status.
func (c HomeCheck) Explain() string {
	if c.OK() {
		return ""
	}
	return c.Summary() + "\n  " + c.Advice()
}

// HomeError is what RequireHome returns when a write must not proceed. It
// carries the check so a caller can inspect the status — for example to offer
// the opt-out only for HomeImplausible, the one status where it applies.
type HomeError struct {
	Check HomeCheck
}

func (e *HomeError) Error() string {
	return "refusing to write: " + e.Check.Summary() + "; " + e.Check.Advice()
}

// RequireHome is the guard every write path must call before touching disk,
// and the reason this package's home check exists at all.
//
// It is the same classification a read-only scan warns about, at error
// severity instead. The asymmetry is deliberate. A scan with a wrong $HOME
// over-reports — it claims files are unprotected when they are protected under
// the real home — which is annoying but fails safe, and hard-failing would
// break the legitimate outside-$HOME case and change existing exit codes. A
// write with a wrong $HOME silently splits the backup namespace: import writes
// a second, parallel tree under _outside-home/ instead of updating the real
// backups, and a later correct-$HOME link will not see any of it. Nothing is
// destroyed, but the guarantee that the repo holds every project's agents is
// quietly false. Relying on a human noticing odd keys is not adequate for that.
//
// import and link do not exist yet. Both must call this with the agent
// directories they are about to write for — which they necessarily know, since
// both have to scan before they can act — and abort on a non-nil error, before
// they create, modify, or remove anything:
//
//	res, err := repo.Scan(root)
//	if err != nil {
//	    return err
//	}
//	if err := repo.RequireHome(root, res.Dirs, *allowOutsideHome); err != nil {
//	    return err // write nothing
//	}
//
// Passing the directories rather than the root is what makes the guard sound:
// the root a command was invoked with says nothing about where the projects
// under it actually live. See CheckHome.
//
// The returned error is always a *HomeError; nil means the write may proceed.
func (r *Repo) RequireHome(root string, dirs []string, allowOutsideHome bool) error {
	c := r.CheckHome(root, dirs)
	if c.OK() || c.SuppressedBy(allowOutsideHome) {
		return nil
	}
	return &HomeError{Check: c}
}
