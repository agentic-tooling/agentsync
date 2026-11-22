package scan

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// Tracked records whether a file is committed in the git repository that owns
// it — which is a different question from whether the agent repo has a copy.
//
// A file can be absent from the agent repo yet perfectly safe, because its own
// project has it committed. Only a file that is in neither place is genuinely
// unprotected.
type Tracked int

const (
	// TrackedUnknown means the check could not be performed, for example
	// because git is unavailable. Treated as untracked when judging risk,
	// so an unknown never downgrades a warning.
	TrackedUnknown Tracked = iota

	// TrackedYes means the file is present in the owning repository's HEAD
	// commit. That is the guarantee we need: the content is reachable from
	// a ref, so it survives `git reset --hard` and `git gc`.
	TrackedYes

	// TrackedNo means the file is not in HEAD — untracked, staged but never
	// committed, in a repository with no commits yet, or outside any git
	// repository at all. All of those lose the content if the working file
	// goes away.
	TrackedNo
)

func (t Tracked) String() string {
	switch t {
	case TrackedYes:
		return "tracked"
	case TrackedNo:
		return "untracked"
	default:
		return "unknown"
	}
}

// gitAnswer is the outcome of running one git command: git said yes, git said
// no, or git could not be run at all. The third case must never be folded into
// the second — "we could not ask" is not "the answer is no".
type gitAnswer int

const (
	gitUnavailable gitAnswer = iota
	gitYes
	gitNo
)

type gitResult struct {
	output     string
	answer     gitAnswer
	code       int
	diagnostic string
}

const maxGitDiagnosticBytes = 512

type gitStderr struct {
	prefix [maxGitDiagnosticBytes]byte
	used   int
}

func (s *gitStderr) Write(p []byte) (int, error) {
	s.used += copy(s.prefix[s.used:], p)
	return len(p), nil
}

func (s *gitStderr) diagnostic() string {
	line := string(s.prefix[:s.used])
	if end := strings.IndexAny(line, "\r\n\u0085\u2028\u2029"); end >= 0 {
		line = line[:end]
	}
	line = strings.ToValidUTF8(line, "")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, line))
}

// runGit trims stdout for probes that do not need exact pathname bytes.
func runGit(dir string, args ...string) gitResult {
	result := runGitRaw(dir, args...)
	result.output = strings.TrimSpace(result.output)
	return result
}

// runGitRaw preserves stdout on success and captures bounded stderr on a non-zero exit.
// Each probe interprets the exit code to distinguish a clean negative from a failure.
func runGitRaw(dir string, args ...string) gitResult {
	cmd := exec.Command("git", args...)
	// cmd.Dir rather than `git -C`: the working directory is the thing that
	// actually selects the repository, and it cannot be confused with a
	// pathspec.
	cmd.Dir = dir

	var out bytes.Buffer
	var stderr gitStderr
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return gitResult{output: out.String(), answer: gitYes}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// A non-zero exit is git answering the question — or reporting that
		// it could not. The caller decides which, from the code.
		return gitResult{answer: gitNo, code: exitErr.ExitCode(), diagnostic: stderr.diagnostic()}
	}

	// git is missing, not executable, or the directory is gone: we did not
	// get an answer.
	return gitResult{}
}

// gitSaidNo is the exit code git uses for a real negative answer. Anything
// else non-zero — 128 in practice — is git failing, not git answering.
const gitSaidNo = 1

// gitRoot is what we learned about the repository containing some directory.
type gitRoot struct {
	// top is the resolved repository root, set only when status is gitYes.
	top string
	// status is gitYes when the directory sits in a repository that has at
	// least one commit, gitNo when there is no such repository (including a
	// repository whose HEAD is unborn), and gitUnavailable when git could
	// not be run.
	status gitAnswer

	diagnostic string
}

// gitProber answers "is this committed?" and caches the per-directory repo
// lookup, which is the expensive part. The zero value is ready to use.
//
// Caching is safe within a scan: the scan is read-only and short-lived, so the
// repository layout cannot change underneath it.
type gitProber struct {
	mu    sync.Mutex
	cache map[string]gitRoot
}

func (g *gitProber) root(dir string) gitRoot {
	g.mu.Lock()
	if r, ok := g.cache[dir]; ok {
		g.mu.Unlock()
		return r
	}
	g.mu.Unlock()

	r := probeGitRoot(dir)

	g.mu.Lock()
	if g.cache == nil {
		g.cache = make(map[string]gitRoot)
	}
	g.cache[dir] = r
	g.mu.Unlock()
	return r
}

func probeGitRoot(dir string) gitRoot {
	result := runGit(dir, "rev-parse", "--show-toplevel")
	switch result.answer {
	case gitUnavailable:
		return gitRoot{status: gitUnavailable}
	case gitNo:
		// The same 128-means-two-things problem InspectDir solves at the
		// bottom of this file, and it was solved there only. git exits 128
		// both for "this is not a repository" and for "I will not talk
		// about this repository" — safe.directory refusing a dubiously
		// owned checkout, a gitfile pointing at a missing gitdir, an
		// object store that is not there. Verified against git 2.50: a
		// plain non-repo directory, a broken gitfile, and a repo with
		// .git/objects removed all exit 128 from this one command, so the
		// code cannot separate them and the filesystem has to.
		//
		// Collapsing them made TrackedNo, which is a definite "nothing is
		// protecting this file". The wrong half of that reaches a user:
		// import --update's commit gate refuses on TrackedNo by telling
		// them to commit project-agents/ — i.e. to commit files in a
		// repository git has just declined to open. emitUpdateRefusals'
		// own comment says that is the one sentence it must never
		// produce.
		//
		// Nothing is destroyed either way; both arms refuse. It is the
		// remedy that is wrong, and a remedy that cannot work is worse
		// than "git could not say", because the user carries it out.
		if hasGitEntry(dir) {
			return gitRoot{status: gitUnavailable, diagnostic: result.diagnostic}
		}
		// Genuinely not inside a repository.
		return gitRoot{status: gitNo}
	}
	top := result.output
	if top == "" {
		// git succeeded but told us nothing usable (bare repo, odd
		// worktree). Do not guess.
		return gitRoot{status: gitUnavailable}
	}
	// Resolve the toplevel the same way the caller's path will be resolved:
	// on macOS /tmp and /var are symlinks, and git reports the physical
	// path, so an unresolved comparison would never line up.
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = resolved
	}

	// An unborn HEAD (freshly `git init`ed, nothing committed) means nothing
	// is protected yet. That is a genuine "no", not an error — and --quiet
	// makes it exit 1, which is the only exit that means it.
	//
	// This is the same query InspectDir asks below, and until now the two
	// read its exit differently: this one took every non-zero as "unborn",
	// InspectDir takes only 1. Two sites answering one question two ways is
	// how this bug class propagates, so they are now identical.
	//
	// The state that reaches this arm is a **corrupt .git/packed-refs**, and
	// it is the one corruption that splits the two probes in the same
	// directory — measured against git 2.50:
	//
	//	rev-parse --show-toplevel       -> 0
	//	rev-parse --verify --quiet HEAD -> 128
	//	  fatal: unexpected line in .git/packed-refs: ...
	//
	// --show-toplevel exits 0, so the hasGitEntry guard above never fires
	// and nothing upstream can catch it. Reading the 128 as "unborn" made
	// this gitNo, hence TrackedNo, hence CommittedNo, hence import --update
	// telling the user to commit project-agents/ in a repository whose refs
	// are unreadable, where committing cannot fix the failure.
	// `git pack-refs --all` is an ordinary
	// maintenance command and `git gc` runs it automatically, so a packed
	// refs file is the normal state of any repository that has been alive
	// for a while, not an exotic one.
	//
	// # A limit this exit code cannot express
	//
	// Exit 1 is necessary but NOT sufficient for "unborn", and no reading of
	// the code fixes that. git reports a *permission* failure with its
	// documented negative — measured, both for an unreadable
	// refs/heads/<branch> and for an unreadable refs/heads/ directory:
	//
	//	rev-parse --show-toplevel       -> 0
	//	rev-parse --verify --quiet HEAD -> 1
	//
	// So a repository whose refs cannot be read still lands on gitNo here
	// and reports as "nothing is committed". This remains a limitation here
	// and in InspectDir below: distinguishing unreadable refs from an unborn
	// HEAD requires filesystem corroboration in the style of hasGitEntry,
	// rather than relying on the exit code alone.
	result = runGit(top, "rev-parse", "--verify", "--quiet", "HEAD")
	switch {
	case result.answer == gitYes:
		// HEAD resolves; the repository has at least one commit.
	case result.answer == gitNo && result.code == gitSaidNo:
		// Unborn HEAD. A real "no".
		return gitRoot{status: gitNo}
	default:
		return gitRoot{status: gitUnavailable, diagnostic: result.diagnostic}
	}

	return gitRoot{top: top, status: gitYes}
}

// tracked reports whether path is present in the HEAD commit of whatever git
// repository contains it.
//
// This deliberately asks about HEAD rather than the index. `git ls-files` would
// answer "is it staged", and a staged-but-never-committed blob is unreachable
// after `git reset --hard` and collectable by `git gc` — treating it as safe is
// exactly the misreport this tool must never make.
//
// "Not a git repository", "no commits yet", "staged only", and "untracked" all
// collapse to TrackedNo: in every one of them, nothing is protecting the file.
// Every way of failing to get an answer yields TrackedUnknown.
func (g *gitProber) tracked(path string) Tracked {
	loc := g.locate(path)
	switch loc.status {
	case gitUnavailable:
		return TrackedUnknown
	case gitNo:
		return TrackedNo
	}
	answer, _ := trackedInTree(loc.root, loc.rel)
	return answer
}

// ls-tree returns empty stdout for an absent path. Every non-zero exit is a failure to read the tree.
func trackedInTree(root, rel string) (Tracked, string) {
	result := runGit(root, "ls-tree", "-r", "--name-only", "-z", "HEAD", "--", rel)
	if result.answer != gitYes {
		return TrackedUnknown, result.diagnostic
	}
	if result.output == "" {
		return TrackedNo, ""
	}
	return TrackedYes, ""
}

// located is one path expressed the way git wants to be asked about it: the
// root of the repository holding it, and its slash-separated path relative to
// that root. An absolute path would be matched against the repo root as a
// pathspec and silently find nothing.
//
// status is gitYes when root and rel are usable, gitNo when there is no
// repository (so there is a real negative answer without asking anything), and
// gitUnavailable when the question could not be formed at all.
type located struct {
	root       string
	rel        string
	status     gitAnswer
	diagnostic string
}

// locate resolves path into the repository-relative form every query below
// needs. It is separate from tracked because more than one question is asked
// about the same path, and each of them needs the same two strings.
func (g *gitProber) locate(path string) located {
	dir := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	root := g.root(dir)
	switch root.status {
	case gitUnavailable:
		return located{status: gitUnavailable, diagnostic: root.diagnostic}
	case gitNo:
		return located{status: gitNo}
	}

	rel, err := filepath.Rel(root.top, filepath.Join(dir, filepath.Base(path)))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// The file is not under the root git handed us. We cannot form a
		// meaningful question, so we do not answer one — gitUnavailable
		// rather than gitNo, because "I cannot ask" is not "the answer is
		// no".
		return located{status: gitUnavailable}
	}
	return located{root: root.top, rel: filepath.ToSlash(rel), status: gitYes}
}

// gitTracked is the uncached form of gitProber.tracked, for callers that check
// a single path.
func gitTracked(path string) Tracked {
	var g gitProber
	return g.tracked(path)
}

// Committed says whether the bytes a file holds right now can be recovered
// from the HEAD commit of the repository containing it.
//
// It is a stronger question than Tracked, and the difference is the whole
// reason this type exists. Tracked asks whether the *path* is in HEAD. That
// says nothing about the *bytes*: a file committed as v1 and then edited to v2
// is tracked, and v2 is in no commit at all. On a path that is about to
// overwrite those bytes, only the stronger question is worth asking.
type Committed int

const (
	// CommittedUnknown means git could not be asked — it is missing,
	// safe.directory refuses the checkout, the object store is damaged. It
	// is the zero value on purpose: a caller that forgets a case gets the
	// answer that refuses.
	CommittedUnknown Committed = iota

	// CommittedYes means the path is in HEAD and the file on disk is
	// byte-identical to what HEAD holds. Only this answer means the current
	// content survives the file being replaced.
	CommittedYes

	// CommittedNo means the current bytes are in no commit: the path is
	// absent from HEAD, or present in HEAD and modified since. Both lose the
	// content on disk if it is overwritten, so they are one answer.
	CommittedNo
)

func (c Committed) String() string {
	switch c {
	case CommittedYes:
		return "committed"
	case CommittedNo:
		return "uncommitted"
	default:
		return "unknown"
	}
}

// CommittedInHEAD reports whether the bytes at path are recoverable from the
// HEAD commit of the repository containing it.
//
// This is the write-path question, and it is exported for callers in other
// packages that must not replace a file whose content exists nowhere else —
// importer's --update is the caller it was written for.
//
// It asks git three times because no one question covers it, and no two do
// either.
//
//  1. `ls-tree HEAD` proves the path is in HEAD, and says nothing about the
//     working file's bytes.
//  2. `diff --quiet HEAD` proves the working file matches HEAD — but it exits 0
//     for an untracked path too, since an untracked file is in no diff, so on
//     its own it would call a file that has never been committed "unchanged".
//  3. `cat-file -e HEAD:<rel>` proves the blob those two agree on is actually
//     in the object store. See below for why the first two do not.
//
// The exit codes are read the way the rest of this file reads them: 0 is a yes,
// a documented 1 is a no, and anything else is git failing rather than
// answering. Only a clean yes to all three questions returns CommittedYes;
// every other outcome refuses. Making CommittedUnknown the zero value keeps
// a forgotten case on the refusing side.
//
// # Why the blob has to be asked for separately
//
// Without question 3 this returns CommittedYes for a path whose blob is missing
// from the object store — a yes in the one direction the gate exists to
// prevent, since it authorises overwriting content that cannot in fact be
// recovered.
//
// The cause is the index stat cache. `git diff` is *defined* to use it: when a
// working file's stat data still matches the index entry, git answers
// "unchanged" from the recorded object id without ever opening the object, so a
// deleted blob goes unnoticed. Invalidating the stat data does not help either
// — git then re-hashes the working file, gets the same id, and again concludes
// "unchanged" without a read. The blob is only read when the content genuinely
// differs, which is the case where the answer is CommittedNo anyway. So no
// rephrasing of the diff closes this; a separate question is the only way.
//
// `cat-file -e` is asked last, on the sole branch that would otherwise return
// yes, which holds the cost to one extra fork per authorised update rather than
// one per gated file. Its non-zero exits are all CommittedUnknown, not
// CommittedNo: ls-tree has already said the path is in HEAD, so a blob that is
// missing at this point means a damaged repository, never an ordinary "not
// committed".
//
// This is existence-and-header checking, not verification: `cat-file -e` reads
// the object's header, so a truncated or corrupt object is caught, while a
// blob whose payload was altered in place is not. Catching that needs
// fsck-grade hashing of every object and is out of scope for a gate that runs
// on a write path.
//
// It would also refuse a gitlink — a submodule directory, whose commit object
// lives in the submodule's own store rather than this one — as CommittedUnknown.
// No caller can reach that today: this is only ever asked about a regular .md
// capture file, and the answer fails safe regardless. It is recorded because a
// future caller that hands this a directory would meet it, and the refusal
// would look like a bug rather than a limit.
//
// One known false negative, in the safe direction: `diff --quiet` can report a
// difference from stale stat information in an unrefreshed index. That refuses
// an update that could have proceeded, and the remedy — any ordinary git
// command in the repo — is one the user is being told to run anyway.
func CommittedInHEAD(path string) Committed {
	answer, _ := CommittedInHEADWithDiagnostic(path)
	return answer
}

// CommittedInHEADWithDiagnostic also returns the failed Git probe's diagnostic, when available.
func CommittedInHEADWithDiagnostic(path string) (Committed, string) {
	var g gitProber

	loc := g.locate(path)
	switch loc.status {
	case gitUnavailable:
		return CommittedUnknown, loc.diagnostic
	case gitNo:
		return CommittedNo, ""
	}

	tracked, diagnostic := trackedInTree(loc.root, loc.rel)
	switch tracked {
	case TrackedUnknown:
		return CommittedUnknown, diagnostic
	case TrackedNo:
		return CommittedNo, ""
	}

	diff := runGit(loc.root, "diff", "--quiet", "HEAD", "--", loc.rel)
	switch {
	case diff.answer == gitYes:
		// A missing blob may exit 1 even though the tree still references it.
		blob := runGit(loc.root, "cat-file", "-e", "HEAD:"+loc.rel)
		if blob.answer != gitYes {
			return CommittedUnknown, blob.diagnostic
		}
		return CommittedYes, ""
	case diff.answer == gitNo && diff.code == gitSaidNo:
		return CommittedNo, ""
	default:
		return CommittedUnknown, diff.diagnostic
	}
}

// DirTracking is what a project's own git says about a directory this tool is
// about to write into.
//
// It answers a different question from Tracked, which asks whether a file's
// content is protected. This asks whether a *write* would be visible to the
// project's repository — and it exists because a symlink is not portable
// content. A link into this repo names an absolute path on one machine, so
// committing one hands every other clone of that project a broken link.
type DirTracking int

const (
	// DirUnknown means git could not be asked. It must never be treated as
	// "not tracked": on a write path that would link into a tracked
	// directory precisely because the check failed.
	DirUnknown DirTracking = iota

	// DirNoRepo means the directory is not inside a git repository at all.
	// Nothing can be committed, so there is nothing to reconcile.
	DirNoRepo

	// DirTracked means the repository has files committed under this
	// directory. A symlink written here is a change to a tracked path.
	DirTracked

	// DirIgnored means the repository ignores this directory. Links written
	// here stay local to this machine, which is what we want.
	DirIgnored

	// DirVisible means the directory is inside a repository, holds nothing
	// committed, and is not ignored. A link written here shows up as an
	// untracked file — noise rather than damage, but git can see it.
	DirVisible
)

func (d DirTracking) String() string {
	switch d {
	case DirNoRepo:
		return "no-repo"
	case DirTracked:
		return "tracked"
	case DirIgnored:
		return "ignored"
	case DirVisible:
		return "visible"
	default:
		return "unknown"
	}
}

// DirGit is everything a write path needs to know about the git repository
// owning a directory: how the directory stands in it, and where to write a
// local exclusion if the caller wants one.
type DirGit struct {
	// Dir is the directory that was asked about. It need not exist.
	Dir string

	Tracking DirTracking

	// Diagnostic describes the Git failure behind DirUnknown, when available.
	Diagnostic string

	// Root is the repository's worktree root, resolved. Empty when there is
	// no repository or the question could not be asked.
	Root string

	// GitDir is the absolute path of the repository's git directory, which
	// is where info/exclude lives. Empty on the same terms as Root, and also
	// when git declined to name it.
	GitDir string

	// GitDirDiagnostic describes a failed GitDir lookup for callers that need a local exclusion.
	GitDirDiagnostic string

	// Rel is Dir relative to Root, slash-separated — the form a git
	// pathspec and a .git/info/exclude pattern both want.
	Rel string
}

// InspectDir classifies a directory against the git repository that owns it.
//
// dir need not exist. That is the point: link can be asked to provision a
// project whose .claude/agents directory has never been created, and the
// question "would writing here be visible to git" has an answer before the
// directory does. git is therefore run from the nearest existing ancestor.
//
// The three-way distinction from runGit is preserved end to end. Every path
// that could not get an answer out of git returns DirUnknown rather than
// falling through to a "no", because a caller about to write must be able to
// tell "git says this is clean" from "git did not say".
func InspectDir(dir string) DirGit {
	return inspectPath(dir, existingAncestor(dir), false)
}

// InspectSkillPath checks a project-relative path without following its leaf.
// Both indexed and committed paths can be shadowed by a symlink.
func InspectSkillPath(project, rel string) DirGit {
	return inspectPath(filepath.Join(project, rel), project, true)
}

func inspectPath(dir, probe string, skill bool) DirGit {
	d := DirGit{Dir: dir, Tracking: DirUnknown}
	if probe == "" {
		return d
	}

	result := runGitRaw(probe, "rev-parse", "--show-toplevel")
	switch result.answer {
	case gitUnavailable:
		return d
	case gitNo:
		// "not a git repository" and "I could not tell you" are the same
		// non-zero exit here — git reports both as 128 — so the exit code
		// cannot separate them and the filesystem has to. A .git in any
		// ancestor means there IS a repository and git declined to say so:
		// safe.directory refusing a dubiously-owned checkout (sudo, cron,
		// containers, bind mounts), an unreadable .git, a broken gitfile.
		//
		// Getting this wrong is silent and total. DirNoRepo is the most
		// permissive verdict in the switch, so treating a failed probe as
		// "no repository" turns the gate off for every project in exactly
		// the environments that need it most.
		if hasGitEntry(probe) {
			d.Diagnostic = result.diagnostic
			return d
		}
		d.Tracking = DirNoRepo
		return d
	}
	top := result.output
	if skill {
		top = strings.TrimSuffix(top, "\n")
	} else {
		top = strings.TrimSpace(top)
	}
	if top == "" {
		// git succeeded and told us nothing usable (a bare repo, an odd
		// worktree). Do not guess.
		return d
	}
	// Resolved the same way Repo.Root is, so the relative path below is
	// computed between two paths of the same kind; on macOS /tmp and /var
	// are themselves symlinks and git reports the physical path.
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = resolved
	}
	d.Root = top

	// --absolute-git-dir rather than --git-dir: the latter answers relative
	// to the working directory, which is the probe, not the caller's.
	//
	// This discards the exit code and it is safe to: GitDir has no
	// three-way to lose, because "git said no" and "git could not answer"
	// both leave it empty and empty is already the refusing value. The one
	// caller that needs it — linker's --exclude-local — blocks on
	// `GitDir == ""` rather than proceeding without an exclusion, so the
	// ambiguity never reaches a decision that could go the permissive way.
	result = runGit(probe, "rev-parse", "--absolute-git-dir")
	d.GitDir = result.output
	d.GitDirDiagnostic = result.diagnostic

	resolved := dir
	if !skill {
		resolved = resolveMissing(dir)
	}
	rel, err := filepath.Rel(top, resolved)
	if err != nil || !contains(rel) {
		// The directory is not under the root git handed us. We cannot form
		// a meaningful question, so we do not answer one.
		return d
	}
	d.Rel = filepath.ToSlash(rel)
	if skill {
		return inspectSkillGit(d)
	}

	// An unborn HEAD is a repository that has committed nothing, so nothing
	// under this directory is tracked. That is a real answer — --quiet makes
	// it exit 1 — and skipping straight to the ignore check is what makes a
	// freshly-initialised project usable rather than DirUnknown. Any other
	// failure is not an unborn HEAD and must not be read as one.
	//
	// Carries the same recorded limit as probeGitRoot's copy of this query,
	// stated in full there: exit 1 is necessary but not sufficient, because
	// git reports an unreadable refs/heads/ with its documented negative
	// rather than with 128. A repository whose refs cannot be read therefore
	// reaches the ignore check as though it had no commits, and lands on
	// DirVisible or DirIgnored — the permissive side. Distinguishing these
	// cases remains an unresolved limitation of this exit-code check.
	result = runGit(top, "rev-parse", "--verify", "--quiet", "HEAD")
	switch {
	case result.answer == gitYes:
		tracked, diagnostic := trackedInTree(top, d.Rel)
		switch tracked {
		case TrackedUnknown:
			d.Diagnostic = diagnostic
			return d
		case TrackedYes:
			d.Tracking = DirTracked
			return d
		}
	case result.answer == gitNo && result.code == gitSaidNo:
		// Unborn HEAD. Nothing is committed; carry on to the ignore check.
	default:
		d.Diagnostic = result.diagnostic
		return d
	}

	// check-ignore exits 0 for ignored and 1 for not ignored. Anything else
	// non-zero is git failing rather than answering, and a failure here must
	// not read as "not ignored".
	result = runGit(top, "check-ignore", "-q", "--", d.Rel)
	switch {
	case result.answer == gitYes:
		d.Tracking = DirIgnored
	case result.answer == gitNo && result.code == gitSaidNo:
		d.Tracking = DirVisible
	default:
		d.Diagnostic = result.diagnostic
	}
	return d
}

// SkillExcludePath asks Git for the shared info/exclude, including in worktrees.
func SkillExcludePath(root string) string {
	result := runGit(root, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if result.answer != gitYes || !filepath.IsAbs(result.output) {
		return ""
	}
	return result.output
}

func inspectSkillGit(d DirGit) DirGit {
	// Query the first component as well so an indexed ancestor file or
	// gitlink is found even when it disappeared from the working tree.
	prefix, _, _ := strings.Cut(d.Rel, "/")
	pathspec := []string{"--", prefix}
	result := runGit(d.Root, "config", "--type=bool", "--get", "core.ignoreCase")
	if result.answer != gitYes && !(result.answer == gitNo && result.code == gitSaidNo) {
		return d
	}
	ignoreCase := result.output == "true"
	if ignoreCase {
		// ls-tree has no case-insensitive pathspec. Filter its output below
		// so case aliases cannot shadow an indexed or committed path.
		pathspec = []string{"--"}
	}
	result = runGitRaw(d.Root, append([]string{"--literal-pathspecs", "ls-files", "-z"}, pathspec...)...)
	if result.answer != gitYes {
		return d
	}
	paths := result.output
	result = runGit(d.Root, "rev-parse", "--verify", "--quiet", "HEAD")
	switch {
	case result.answer == gitYes:
		tree := runGitRaw(d.Root, append([]string{"--literal-pathspecs", "ls-tree", "-r", "--name-only", "-z", "HEAD"}, pathspec...)...)
		if tree.answer != gitYes {
			return d
		}
		paths += tree.output
	case result.answer == gitNo && result.code == gitSaidNo:
	default:
		return d
	}
	rel := d.Rel
	if ignoreCase {
		rel = strings.ToLower(rel)
	}
	for name := range strings.SplitSeq(paths, "\x00") {
		if ignoreCase {
			name = strings.ToLower(name)
		}
		if name != "" && (name == rel || strings.HasPrefix(name, rel+"/") || strings.HasPrefix(rel, name+"/")) {
			d.Tracking = DirTracked
			return d
		}
	}
	result = runGit(d.Root, "check-ignore", "-q", "--", d.Rel)
	switch {
	case result.answer == gitYes:
		d.Tracking = DirIgnored
	case result.answer == gitNo && result.code == gitSaidNo:
		d.Tracking = DirVisible
	}
	return d
}

// hasGitEntry reports whether dir or any ancestor holds a .git entry — a
// directory in an ordinary checkout, a file in a worktree or submodule.
//
// It is only ever consulted when git has already declined to name a
// repository, so finding one is a contradiction: the repository exists and git
// would not talk about it. That is the signal to refuse.
func hasGitEntry(dir string) bool {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// existingAncestor returns dir, or the nearest ancestor of it that exists, or
// "" if none does. It is what lets git be asked about a directory that has not
// been created yet.
func existingAncestor(dir string) string {
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// resolveMissing resolves a path that may not exist, by resolving the part
// that does and re-appending the rest.
//
// resolvePath alone would give up and return the cleaned absolute path, which
// on macOS leaves a /var prefix where git reports /private/var — and a
// relative path computed between the two forms escapes the repository and
// reads as "not under this root".
func resolveMissing(dir string) string {
	dir = filepath.Clean(dir)
	anc := existingAncestor(dir)
	if anc == "" {
		return dir
	}
	rest, err := filepath.Rel(anc, dir)
	if err != nil {
		return dir
	}
	return filepath.Join(resolvePath(anc), rest)
}
