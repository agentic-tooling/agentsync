// Package doctor diagnoses the ways a project's agent links and this repo's
// captures drift apart, and does nothing whatsoever about any of it.
//
// It is the fourth command in the build order — scan classifies, import
// captures, link provisions, doctor reports — and the only one after scan that
// writes nothing at all. Not the projects, not this repo, not a
// .git/info/exclude, not a state file. There is no --apply because there is
// nothing to apply.
//
// That is a decision, not an unfinished feature. Every drift below has an
// obvious-looking repair — unlink the broken one, repoint the mismatched one,
// delete the stale capture — and every one of those repairs needs a fact this
// diagnosis alone cannot establish. The state file records
// every link the tool created and the content hash at link time, which is the
// only thing that separates "the user deleted this on purpose" from "this went
// missing", and "this capture is obsolete" from "this capture is the last copy
// of something". Repairing without that merge base means guessing, and the two
// guesses that look most reasonable — unlinking a broken link and deleting a
// stale capture — are exactly the two that destroy something unrecoverable.
//
// Reconciliation belongs to the separate reconcile command, gated on the state
// file. doctor's whole contract is: find it, name it, explain why it might be
// there, and leave the judgement to a human. Its exit code is the only thing it
// asserts.
//
// The link judgements are not made here. Broken, foreign, and mismatched are
// decided by linker.Judge, the same function link runs, because "is this link
// healthy and pointing at the right counterpart" is one question and two
// implementations of it would drift — silently, and in the direction of one
// command reporting a project clean while the other refuses it.
package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/linker"
	"github.com/agentic-tooling/agentsync/internal/scan"
)

// capturesDir is the repo subtree holding each project's captured agents. It
// is a tree, not one flat level — see walkCaptures.
const capturesDir = "project-agents"

// Kind is one category of drift.
type Kind int

const (
	// KindInvalid is the zero value and never appears in a report.
	KindInvalid Kind = iota

	// KindBrokenLink is state 3: a symlink into this repo whose target is
	// gone, usually because the agent was renamed or deleted here. Reported,
	// never unlinked.
	KindBrokenLink

	// KindMismatch is a healthy link into this repo pointing at the wrong
	// file: the project is served agents/<name>.md while its own
	// project-agents/<key>/<name>.md sits unused.
	KindMismatch

	// KindStaleCapture is a file in project-agents/<key>/ with no live
	// counterpart in the project. Reported, never deleted.
	KindStaleCapture

	// KindOrphanedCaptures is a whole project-agents/<key>/ directory whose
	// project no longer exists. Distinct from a stale file: the project is
	// gone, not one agent in it.
	KindOrphanedCaptures

	// KindForeignLink is state 4: a symlink pointing outside this repo.
	// Informational only — see NeedsHuman.
	KindForeignLink

	// KindOverrideCannotRun is a project override hook whose agentsync is
	// missing from PATH or cannot serve the hook, so the overrides do nothing.
	KindOverrideCannotRun
)

func (k Kind) String() string {
	switch k {
	case KindBrokenLink:
		return "broken-link"
	case KindMismatch:
		return "mismatch"
	case KindStaleCapture:
		return "stale-capture"
	case KindOrphanedCaptures:
		return "orphaned-captures"
	case KindForeignLink:
		return "foreign-link"
	case KindOverrideCannotRun:
		return "override-cannot-run"
	default:
		return "invalid"
	}
}

// NeedsHuman reports whether this kind is something only a person can settle,
// which is what exit 1 asserts.
//
// A foreign link is deliberately not one, exactly as in link: it is somebody
// else's link, correctly left alone, and a permanent feature of a machine
// running another tool alongside this one. A diagnosis that failed forever on
// it would train its reader to ignore the exit code, at which point the code
// protects nobody.
func (k Kind) NeedsHuman() bool {
	return k != KindForeignLink && k != KindInvalid
}

// Finding is one piece of drift, and why it might be there.
//
// It always carries enough to act on without re-deriving anything: which
// project, which agent, the path of the thing found, and the path of the thing
// that is missing or wrong.
type Finding struct {
	Kind Kind `json:"kind"`

	// KindName is Kind in printable form. Kind itself marshals as a number,
	// matching scan.State and linker.Action; the name is here so a jq
	// pipeline can read a report without a copy of this enum.
	KindName string `json:"kind_name"`

	// Project is the project key, as produced by scan.ProjectKey.
	Project string `json:"project"`

	// Name is the agent name, empty for KindOrphanedCaptures, which is about
	// a whole directory rather than one agent.
	Name string `json:"name,omitempty"`

	// Path is the thing that was found: the link, the capture file, or the
	// capture directory.
	Path string `json:"path"`

	// Target is the other end of the finding: the missing link target, the
	// counterpart a mismatched link should point at, the live file a stale
	// capture has none of, or the project directory an orphaned capture
	// directory names.
	Target string `json:"target,omitempty"`

	// Detail explains the finding in one clause, and is what the text report
	// prints.
	Detail string `json:"detail"`

	// Entry is the classification a link finding came from, nil for the
	// capture-side kinds, which have no file in a project to classify.
	Entry *scan.Entry `json:"entry,omitempty"`
}

func (f Finding) String() string {
	if f.Name == "" {
		return fmt.Sprintf("%s %s: %s", f.Kind, f.Project, f.Detail)
	}
	return fmt.Sprintf("%s %s/%s: %s", f.Kind, f.Project, f.Name, f.Detail)
}

// Examined is what the diagnosis was drawn from.
//
// It is reported alongside the findings because "nothing is wrong" and "nothing
// was looked at" produce the same empty list, and only these numbers tell them
// apart.
type Examined struct {
	// Projects is how many .claude/agents directories the scan found.
	Projects int `json:"projects"`

	// Entries is how many agent files were classified across them.
	Entries int `json:"entries"`

	// CaptureDirs is how many project-agents/<key>/ directories hold at
	// least one capture, and Captures how many capture files they hold.
	CaptureDirs int `json:"capture_dirs"`
	Captures    int `json:"captures"`

	// HomeKeyedCaptureDirs is how many of CaptureDirs have a key the home
	// directory actually resolves — everything except the _outside-home/
	// namespace, whose keys are absolute paths that scan.ProjectDir locates
	// without reading home at all.
	//
	// It is the denominator HomeSuspect judges by, and it is reported
	// separately because it is the only one of these numbers that says
	// anything about the home: an _outside-home/ capture is orphaned or not
	// for reasons the home took no part in.
	HomeKeyedCaptureDirs int `json:"home_keyed_capture_dirs"`
}

// Report is one doctor run.
//
// Findings and Problems are always non-nil, so the JSON form carries empty
// arrays rather than nulls.
type Report struct {
	// Findings is every piece of drift found, sorted by kind then project
	// then name so the report and the JSON are deterministic.
	Findings []Finding `json:"findings"`

	Examined Examined `json:"examined"`

	// Problems are paths this run could not judge: a subtree of
	// project-agents/ it could not read, a capture whose key names nothing
	// resolvable, an entry in a state it does not understand.
	//
	// A problem is not a finding. A finding is drift doctor identified; a
	// problem is doctor failing to look, which means the diagnosis is
	// incomplete and "nothing is wrong" cannot be said.
	Problems []scan.ScanProblem `json:"problems"`
}

// Diagnose examines the links a scan classified and the captures this repo
// holds, and reports what does not line up.
//
// It reads and it stats. It creates, modifies, and removes nothing — see the
// package doc for why that is the whole design rather than a first version.
func Diagnose(repo *scan.Repo, res scan.Result) Report {
	r := Report{
		Findings: make([]Finding, 0),
		Problems: make([]scan.ScanProblem, 0),
		Examined: Examined{Projects: len(res.Dirs), Entries: len(res.Entries)},
	}

	r.diagnoseLinks(repo, res.Entries)
	r.diagnoseCaptures(repo)

	sort.Slice(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Name < b.Name
	})
	return r
}

// add stamps the printable kind name so no caller can set one that disagrees
// with the kind beside it.
func (r *Report) add(f Finding) {
	f.KindName = f.Kind.String()
	r.Findings = append(r.Findings, f)
}

// diagnoseLinks reports the three link-side kinds, deferring every judgement to
// linker.Judge.
//
// Only the three symlink states are put to it. The rest — adoptable, override,
// unadopted — are real files, and what to do about those is scan's and import's
// report to make; asking Judge about them would also make it hash files doctor
// has no use for.
func (r *Report) diagnoseLinks(repo *scan.Repo, entries []scan.Entry) {
	for _, e := range entries {
		switch e.State {
		case scan.StateHealthyLink, scan.StateBrokenLink, scan.StateForeignLink:
		default:
			continue
		}

		item, prob := linker.Judge(repo, e)
		if prob != nil {
			r.Problems = append(r.Problems, *prob)
			continue
		}

		// The verdict is linker's; only the sentence below is doctor's. The
		// mismatch case keeps linker's reason verbatim because it carries a
		// runnable remedy naming link's own flags, and a second copy of that
		// sentence would go stale the moment a flag is renamed. The other
		// two say what link would do rather than what is wrong, which is the
		// wrong thing to print in a report about diagnosis.
		switch item.Action {
		case linker.ActionLinked:
			// Healthy and pointing at its counterpart. Nothing to say.

		case linker.ActionBroken:
			r.add(Finding{
				Kind: KindBrokenLink, Project: e.Project, Name: e.Name,
				Path: e.Path, Target: item.Target, Entry: entryOf(e),
				Detail: fmt.Sprintf("links to %s, which no longer exists — the agent was renamed or deleted in this repo; restore it from git history or remove the link by hand", item.Target),
			})

		case linker.ActionMismatch:
			r.add(Finding{
				Kind: KindMismatch, Project: e.Project, Name: e.Name,
				Path: e.Path, Target: item.Target, Entry: entryOf(e),
				Detail: item.Reason,
			})

		case linker.ActionForeign:
			r.add(Finding{
				Kind: KindForeignLink, Project: e.Project, Name: e.Name,
				Path: e.Path, Target: item.Target, Entry: entryOf(e),
				Detail: fmt.Sprintf("points at %s, outside this repo — not ours, and not a fault", item.Target),
			})

		default:
			r.Problems = append(r.Problems, scan.ScanProblem{
				Path: e.Path,
				Err:  fmt.Sprintf("link judged as %v, which doctor does not know how to report", item.Action),
			})
		}
	}
}

// entryOf gives the finding its own copy of the entry rather than a pointer
// into the caller's Result.
//
// Not an aliasing fix: Go 1.22 gave range variables per-iteration scope, and
// this one arrives as a by-value parameter regardless, so &e would be safe.
// It is about ownership. A Report is passed to emitters and, in tests,
// modified, and none of that should be able to reach back into the scan result
// it was derived from.
func entryOf(e scan.Entry) *scan.Entry { return &e }

// capture is one file found under project-agents/.
type capture struct {
	// key is the project key the file is filed under, derived from its path
	// relative to project-agents/ — which is the inverse of what an import
	// did when it wrote the file there.
	key  string
	name string
	path string
}

// diagnoseCaptures reports the two capture-side kinds: a captured file with no
// live counterpart, and a whole capture directory whose project is gone.
//
// Liveness is asked of the filesystem rather than of the scan's entries. The
// scan only saw what was under its root, so a capture belonging to a project
// outside that root would look stale on every run — a false report about the
// one thing doctor must never be casually wrong about, since the suggested
// remedy a reader will imagine is deletion.
func (r *Report) diagnoseCaptures(repo *scan.Repo) {
	root := filepath.Join(repo.Root, capturesDir)
	captures, problems := walkCaptures(root)
	r.Problems = append(r.Problems, problems...)
	r.Examined.Captures = len(captures)

	byKey := make(map[string][]capture)
	keys := make([]string, 0)
	for _, c := range captures {
		if _, seen := byKey[c.key]; !seen {
			keys = append(keys, c.key)
		}
		byKey[c.key] = append(byKey[c.key], c)
	}
	sort.Strings(keys)
	r.Examined.CaptureDirs = len(keys)
	for _, key := range keys {
		if scan.KeyDependsOnHome(key) {
			r.Examined.HomeKeyedCaptureDirs++
		}
	}

	for _, key := range keys {
		group := byKey[key]
		dir := filepath.Join(root, filepath.FromSlash(key))

		projectDir, err := scan.ProjectDir(repo.Home, key)
		if err != nil {
			// The key names nothing this scheme could have produced, so
			// there is no project to compare against and no way to say
			// whether the files here are stale. Never guessed at: a wrong
			// guess here reads as "the project is gone".
			r.Problems = append(r.Problems, scan.ScanProblem{Path: dir, Err: err.Error()})
			continue
		}

		isDir, err := dirPresent(projectDir)
		if err != nil {
			r.Problems = append(r.Problems, scan.ScanProblem{
				Path: dir,
				Err:  fmt.Sprintf("could not tell whether the project at %q still exists: %v", projectDir, err),
			})
			continue
		}
		if !isDir {
			// The whole project is gone, so every file here is stale for
			// one reason. Reported once, as a directory: a per-file
			// report would be the same fact repeated N times and would
			// hide the fact that actually matters.
			r.add(Finding{
				Kind: KindOrphanedCaptures, Project: key, Path: dir, Target: projectDir,
				Detail: fmt.Sprintf("%d captured file%s, but no project at %s — it was deleted, or moved (the key is derived from the path, so a move re-keys the project and this directory keeps the old name); nothing here is deleted, and these may be the only surviving copies",
					len(group), plural(len(group)), projectDir),
			})
			continue
		}

		agentsDir := filepath.Join(projectDir, ".claude", "agents")
		liveDir, err := dirPresent(agentsDir)
		if err != nil {
			r.Problems = append(r.Problems, scan.ScanProblem{
				Path: agentsDir,
				Err:  fmt.Sprintf("could not tell whether this project still has an agents directory: %v", err),
			})
			continue
		}

		for _, c := range group {
			live := filepath.Join(agentsDir, c.name+".md")
			// Lstat semantics, and they are the right ones: a symlink at
			// the live path counts as present even when its target is
			// gone. The project still has that agent by name, so its
			// capture is not stale — whether the link works is state 3's
			// question, reported separately above.
			if liveDir {
				ok, err := present(live)
				if err != nil {
					r.Problems = append(r.Problems, scan.ScanProblem{Path: live, Err: err.Error()})
					continue
				}
				if ok {
					continue
				}
			}

			why := "the project deleted it, or was moved (the key is derived from the path, so a move re-keys the project)"
			if !liveDir {
				why = "the project has no .claude/agents directory at all any more"
			}
			r.add(Finding{
				Kind: KindStaleCapture, Project: key, Name: c.name,
				Path: c.path, Target: live,
				Detail: fmt.Sprintf("no live file at %s — %s; nothing is deleted here, and this may be the only surviving copy", live, why),
			})
		}
	}
}

// walkCaptures collects every capture file under project-agents/.
//
// A tree walk, not one flat level, and project-agents/README.md explains why:
// a project key is a nested path, so a directory here holds .md files if it is
// a project, subdirectories if it contains projects, and both when one project
// nests inside another.
//
// A subtree that cannot be read is recorded and skipped rather than aborting,
// for the same reason it is in scan.FindAgentDirs: one unreadable directory
// must not blind the run to the rest, and must not be silently
// indistinguishable from an empty one either.
func walkCaptures(root string) ([]capture, []scan.ScanProblem) {
	var (
		captures []capture
		problems []scan.ScanProblem
	)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			problems = append(problems, scan.ScanProblem{
				Path: path,
				Err:  fmt.Sprintf("subtree skipped, not readable: %v", err),
			})
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			problems = append(problems, scan.ScanProblem{Path: path, Err: err.Error()})
			return nil
		}
		key := filepath.ToSlash(filepath.Dir(rel))
		if key == "." {
			// A .md sitting directly under project-agents/ has no project
			// key above it, so it is a capture of nothing.
			// project-agents/README.md is the live instance, and this is
			// why it is excluded by shape rather than by name: any other
			// documentation file put beside it is excluded on the same
			// principled grounds, and a real capture can never land here.
			return nil
		}
		if !d.Type().IsRegular() {
			problems = append(problems, scan.ScanProblem{
				Path: path,
				Err:  "not a regular file; cannot be judged as a capture",
			})
			return nil
		}

		captures = append(captures, capture{
			key:  key,
			name: strings.TrimSuffix(d.Name(), ".md"),
			path: path,
		})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A repo that has captured nothing yet simply has no
		// project-agents/ directory, which is not a problem: there is
		// nothing to diagnose. Anything else is.
		problems = append(problems, scan.ScanProblem{
			Path: root,
			Err:  fmt.Sprintf("walking the capture tree: %v", err),
		})
	}
	return captures, problems
}

// present reports whether anything at all sits at path.
//
// os.Lstat, never os.Stat, which is the standing rule for every agent path in
// this tool: Stat follows symlinks, so a broken link would report as absent and
// its capture would be called stale when the project plainly still has the
// agent.
//
// An error that is not "does not exist" is returned rather than folded into
// false. A permission-denied path answering "absent" would produce exactly the
// wrong finding.
func present(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// dirPresent reports whether path is a directory, or a symlink leading to one.
//
// Lstat first, as everywhere else here, and then — only for a path that is
// itself a symlink — the link is resolved.
//
// The case that makes the exception necessary is the agents directory, not the
// project directory. ProjectKey deliberately does NOT resolve
// <project>/.claude/agents (see the comment on it in scan/state.go: resolving
// it would give every project pointing at a shared location the same key), so
// a symlinked agents directory is a supported layout — and an Lstat-only check
// would call it absent and report every capture in that project stale. The
// project directory above it is the weaker case: ProjectKey resolves *that*,
// so a project whose own directory is a symlink is keyed by its resolved path
// and rarely arrives here as a link at all.
//
// This does not weaken the Lstat rule, which is about agent *files*: following
// a link there turns a healthy link into a false adoptable that link then
// overwrites. Whether a directory exists is a different question, and present()
// below keeps Lstat for the file check where the rule does apply.
func dirPresent(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return fi.IsDir(), nil
	}

	resolved, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return resolved.IsDir(), nil
}

// HomeSuspect reports whether this diagnosis is better explained by a wrong
// home directory than by the drift it found.
//
// doctor is the first command whose output depends on repo.Home for paths
// nothing walked. scan, import, and link only ever key directories they
// actually found, so scan.CheckHome's "no projects examined, nothing to say"
// is honest for them. Here it is not: a run that found zero projects still
// resolves every capture key through scan.ProjectDir, and a home that is not
// the one these projects live under sends every one of those lookups somewhere
// empty. The result is every capture in the repo reported as belonging to a
// deleted project — the loudest thing doctor can say, about the files
// project-agents/ exists to protect, with the true remedy nowhere in sight.
//
// The evidence is the captures themselves, which is why the judgement lives
// here rather than in scan.CheckHome: that function counts how many *scanned*
// project directories key under _outside-home/, and it cannot see a capture.
// Handing it doctor's derived project directories instead would prove nothing
// either — they are built from the same home by scan.ProjectDir, so re-keying
// them just returns the key we started with.
//
// Deliberately not gated on having scanned no projects. The wrong home this
// catches includes the one scan.CheckHome documents as undetectable — a home
// that is a genuine ancestor of the real one, $HOME=/Users with projects in
// /Users/you/Web — whose keys come out home-relative and merely wrong. Those
// keys resolve to directories that are not there, so the captures see it even
// though nothing about the scanned projects does.
//
// Only keys the home actually resolves are counted, on both sides of the
// comparison — scan.KeyDependsOnHome. An _outside-home/ key carries an absolute
// path that ProjectDir locates without reading home at all, so its project
// being gone is, by construction, zero evidence about the home; counting it
// would let the warning blame a directory that took no part in the lookup. It
// also makes the printed "n of n" honest, and it is what makes the
// --allow-outside-home reasoning below true rather than merely true-so-far: the
// captures that flag describes are not evidence here at all, whether or not
// their projects still exist.
//
// It is a suspicion, not a verdict, and the sentence in HomeWarning says so: a
// fresh clone of this repo on a machine where none of the projects are checked
// out produces exactly this shape, honestly. That is also the shape where a
// reader most needs telling not to act, so it is not silenced.
//
// There is deliberately no minimum. The false positive it would suppress —
// a small repo where the one home-keyed project really was deleted — costs a
// reader one sentence saying "check your home directory first", while the
// finding itself stands unchanged; the false negative it would buy costs a
// silent green light over files that may exist nowhere else. The error is
// asymmetric and already points the safe way, so a threshold would only trade
// a cheap wrong caveat for an expensive missing one.
func (r Report) HomeSuspect() bool {
	n := r.Examined.HomeKeyedCaptureDirs
	if n == 0 {
		return false
	}
	orphaned := 0
	for _, f := range r.Findings {
		if f.Kind == KindOrphanedCaptures && scan.KeyDependsOnHome(f.Project) {
			orphaned++
		}
	}
	return orphaned == n
}

// HomeWarning is the diagnostic for HomeSuspect: what is doubtful on the first
// line, what to do about it on the second. Empty when there is nothing to
// suspect, so a caller can test it rather than re-asking.
//
// home is the resolved home directory in use for keying — scan.Repo.Home —
// because a user cannot fix a home directory they were never shown.
//
// --allow-outside-home deliberately does not suppress it, and after the
// home-keyed filter in HomeSuspect the reason is structural rather than
// circumstantial: captures in the _outside-home/ namespace — the layout that
// flag exists to describe — are excluded from this evidence entirely, gone
// project or not. The flag's claim and this warning are about disjoint sets of
// captures, so honouring it here would silence a warning it says nothing about.
//
// The wording avoids "below" and "above": this sentence is printed on stderr
// ahead of the report and again inside the summary, where the findings it
// redirects the reader to sit above rather than below. A sentence whose whole
// job is pointing somewhere must not point the wrong way in either place.
func (r Report) HomeWarning(home string) string {
	if !r.HomeSuspect() {
		return ""
	}
	shown := fmt.Sprintf("%q", home)
	if home == "" {
		shown = "empty or unset"
	}
	n := r.Examined.HomeKeyedCaptureDirs
	return fmt.Sprintf("every capture directory located relative to the home directory in use (%d of %d; %s) names a project that is not there, so either that home is not the one these projects live in — sudo, cron, and containers all do this — or none of these projects is checked out on this machine\n  check the home directory before acting on any orphaned-captures finding in this report: on this evidence the captures are not proof that anything was deleted",
		n, n, shown)
}

// Counts tallies findings by kind.
func (r Report) Counts() map[Kind]int {
	counts := make(map[Kind]int)
	for _, f := range r.Findings {
		counts[f.Kind]++
	}
	return counts
}

// OfKind returns the findings of one kind, in report order.
func (r Report) OfKind(k Kind) []Finding {
	out := make([]Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if f.Kind == k {
			out = append(out, f)
		}
	}
	return out
}

// NeedsHuman returns the findings only a person can settle, which is what the
// exit code is computed from.
func (r Report) NeedsHuman() []Finding {
	out := make([]Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if f.Kind.NeedsHuman() {
			out = append(out, f)
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
