// Package importer captures project-local agent files into the canonical agent
// repository.
//
// It is the write counterpart to scan's classification. Scan says which files
// exist only inside a project — override (state 6) and unadopted (state 7),
// the two states whose content the repo holds no copy of. This package copies
// those into project-agents/<key>/<name>.md, which is what makes the
// import-before-link invariant enforceable: once a file is captured here, a
// later link cannot destroy content that exists nowhere else.
//
// Two rules shape everything below.
//
// Planning and applying are separate. NewPlan touches no file except to read
// it, so the CLI can default to showing what it would do; only Plan.Apply
// writes.
//
// A destination whose content differs from the source is never replaced
// without an explicit opt-in, and not even then unless the exact bytes about to
// be destroyed are recoverable from a commit in this repo. That destination is
// itself an earlier capture of some project's file, and it may be the only copy
// of it — overwriting it silently would lose exactly what this command exists
// to preserve. The opt-in is Options.Update; the recoverability check is
// commitGate, and it is about the bytes rather than the path, because a capture
// committed once and edited since is a path in HEAD holding content that is in
// no commit.
package importer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// Action is what an import would do, or did, with one project file.
type Action int

const (
	// ActionInvalid is the zero value and never appears in a plan.
	ActionInvalid Action = iota

	// ActionImport captures a file the repo has no copy of at all.
	ActionImport

	// ActionUpdate replaces a captured copy whose content differs from the
	// project file. It requires both that Options.Update authorises this
	// file and that commitGate finds the capture committed; failing either,
	// the same situation is ActionConflict.
	ActionUpdate

	// ActionCaptured means the destination already holds this exact
	// content. Nothing to do — this is what makes a second run a no-op.
	ActionCaptured

	// ActionConflict means the destination cannot be written safely: it
	// holds different content, or it is not a regular file. Never written.
	ActionConflict

	// ActionSkipTracked means the file lacks a repo copy but is committed
	// in its own project's git repository, so it is not at risk. Skipped
	// unless Options.IncludeTracked says otherwise.
	ActionSkipTracked
)

func (a Action) String() string {
	switch a {
	case ActionImport:
		return "import"
	case ActionUpdate:
		return "update"
	case ActionCaptured:
		return "captured"
	case ActionConflict:
		return "conflict"
	case ActionSkipTracked:
		return "skipped"
	default:
		return "invalid"
	}
}

// Writes reports whether this action modifies the destination.
func (a Action) Writes() bool {
	return a == ActionImport || a == ActionUpdate
}

// Refusal names the specific guard that turned an authorised update into a
// conflict. It is empty on every other item.
//
// Reason says the same thing in prose, for a person. This is here so a report,
// a test, or a jq pipeline can branch on which guard fired without matching on
// English — the reasons are written to be read and will be reworded.
type Refusal string

const (
	// RefusalNone is the zero value: no guard refused this item.
	RefusalNone Refusal = ""

	// RefusalUncommitted means the bytes the capture holds right now are in
	// no commit in the agent repo — the path is untracked, or it is tracked
	// and has been edited since. Either way those bytes exist nowhere else.
	RefusalUncommitted Refusal = "uncommitted-capture"

	// RefusalCommitUnknown means git could not be asked whether the
	// capture's bytes are committed. Distinct from RefusalUncommitted on
	// purpose: "not committed" and "could not ask" are different facts, and
	// only one of them is something the user can fix by committing.
	RefusalCommitUnknown Refusal = "commit-unknown"
)

// UpdateSet is the set of captures --update authorises replacing.
//
// The zero value authorises nothing, which is both the default and the safe
// answer: the capture being replaced is an earlier copy of some project's file
// and may be the only one, so permission has to be given rather than assumed.
//
// It replaced a bool because a bool is the wrong shape for this decision. The
// blanket form covers every differing capture in the run, and the decision is
// actually made one file at a time after looking at a diff — so the blanket
// form asks for authorisation over replacements the user has not seen. Both
// forms are kept: the set is a narrowing of the flag, not a different one.
type UpdateSet struct {
	// blanket is bare --update: every differing capture in the run.
	blanket bool

	// items holds "<project>/<name>" keys from --update=<project>/<name>.
	items map[string]bool
}

// UpdateAll returns the blanket set — what bare --update means.
func UpdateAll() UpdateSet { return UpdateSet{blanket: true} }

// UpdateOnly returns a set authorising exactly the given keys.
func UpdateOnly(keys ...string) (UpdateSet, error) {
	var u UpdateSet
	for _, k := range keys {
		if err := u.Add(k); err != nil {
			return UpdateSet{}, err
		}
	}
	return u, nil
}

// Add records one "<project>/<name>" key, the form the report prints.
//
// A trailing ".md" is accepted and dropped. The report prints names without
// it, but the file on disk has it and people copy from whichever they are
// looking at; refusing one of the two spellings would only ever produce a
// selector that silently matches nothing.
//
// The project key itself contains slashes (Web/example), so the split is at the
// last one rather than the first.
func (u *UpdateSet) Add(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("--update: empty selector; expected --update=<project>/<name>")
	}
	key = strings.TrimSuffix(key, ".md")

	i := strings.LastIndex(key, "/")
	if i <= 0 || i == len(key)-1 {
		return fmt.Errorf("--update=%q: expected <project>/<name>, the two columns the report prints (for example --update=Web/example/planner)", key)
	}
	if u.items == nil {
		u.items = make(map[string]bool)
	}
	u.items[key] = true
	return nil
}

// SetBlanket makes this set cover every differing capture — bare --update.
func (u *UpdateSet) SetBlanket() { u.blanket = true }

// Allows reports whether replacing this project's capture was authorised.
func (u UpdateSet) Allows(project, name string) bool {
	return u.blanket || u.items[project+"/"+name]
}

// Enabled reports whether --update was given in any form. It is what the
// report echoes back; it is never what authorises a write, since a set holding
// three keys authorises three files and not a fourth.
func (u UpdateSet) Enabled() bool { return u.blanket || len(u.items) > 0 }

// Blanket reports whether this is the bare --update rather than a selection.
func (u UpdateSet) Blanket() bool { return u.blanket }

// Keys returns the explicit selectors, sorted. Empty for the blanket form.
func (u UpdateSet) Keys() []string {
	keys := make([]string, 0, len(u.items))
	for k := range u.items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Unmatched returns the selectors that named nothing in these items, sorted.
//
// A selector matches an item with that project and name whatever the plan
// decided about it, not only one that became an update. Matching on the action
// would make the successful command fail on its own second run: once the
// capture has been replaced it is identical to the project file, so the item
// comes back as "captured" and the selector that just worked would read as a
// typo.
//
// Callers report this and nothing more: it does not move the exit code. The
// load-bearing reason is not that a selector is merely a fact about the command
// line — it is that a typo which matters cannot reach exit 0 anyway. If the file
// the user meant to authorise really did need replacing, it is still differing
// and still unauthorised, so judgeDest makes it ActionConflict, and conflicts
// exit 1 unconditionally. The only typo that reaches exit 0 is one where nothing
// needed doing, and failing that run would make the exit code disagree with the
// state of the repo.
func (u UpdateSet) Unmatched(items []Item) []string {
	if len(u.items) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(items))
	for _, i := range items {
		seen[i.Entry.Project+"/"+i.Entry.Name] = true
	}
	var out []string
	for _, k := range u.Keys() {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

// Item is one project file and what the import decided about it.
type Item struct {
	Entry scan.Entry `json:"entry"`

	// Dest is the repo path this file would be captured at, always
	// scan.Repo.BackupPath for the entry's project and name.
	Dest string `json:"dest"`

	Action Action `json:"action"`

	// ActionName is Action in printable form. Action itself marshals as a
	// number, matching scan.State; the name is here so a jq pipeline can
	// read a report without a copy of this enum.
	ActionName string `json:"action_name"`

	// Reason explains the action in one clause, and is what the text report
	// prints. Empty only for a plain import, which needs no explaining.
	Reason string `json:"reason,omitempty"`

	// Refusal names the guard that turned an authorised update into a
	// conflict, empty when none did. See Refusal.
	Refusal Refusal `json:"refusal,omitempty"`

	// DestHash is the SHA-256 of the destination as it stood when the plan
	// was made, empty when nothing was there. It is the evidence for a
	// conflict verdict, and on an update it is what the write re-checks the
	// destination against: replacing a stale capture is authorised for the
	// bytes the plan actually judged, not for whatever is there later.
	DestHash string `json:"dest_hash,omitempty"`

	// SrcHash is the SHA-256 of the project file as the plan saw it, via
	// sourceHash — the classifier's hash when it has one, recomputed when it
	// does not.
	//
	// It is recorded rather than re-derived at write time so that planning
	// and writing ask the same question of the same bytes. Apply compares it
	// both against the destination and against what it actually copied, so a
	// source that changes between the two phases is refused instead of
	// captured unclassified.
	SrcHash string `json:"src_hash,omitempty"`
}

func (i Item) String() string {
	return fmt.Sprintf("%s %s/%s -> %s", i.Action, i.Entry.Project, i.Entry.Name, i.Dest)
}

func (i *Item) set(a Action, reason string) {
	i.Action = a
	i.ActionName = a.String()
	i.Reason = reason
}

// Options controls what a plan selects and what it permits.
//
// Both default to false, and both defaults are the conservative one: capture
// only what is genuinely unprotected, and never replace a capture that differs.
type Options struct {
	// IncludeTracked also captures state-6/7 files that are committed in
	// their own project's git repository.
	//
	// Off by default because those files are not at risk — a project's own
	// committed agents. Capturing them anyway would give the
	// content two owners: the project's repo remains the place it is edited,
	// so the copy here starts diverging the moment anyone touches it, and
	// this repo would be claiming custody of files it does not maintain. The
	// flag exists because "protected by another repo" is a judgement about
	// someone else's repo, and a user may reasonably want a copy regardless.
	IncludeTracked bool

	// Update is the set of captures the user opted into replacing, either
	// as a blanket authorisation or one "<project>/<name>" at a time. The
	// zero value authorises nothing.
	//
	// It never covers a destination that is not a regular file: "the capture
	// is stale, replace it" is not "delete whatever is in the way". Nor does
	// it cover a capture that is not committed in the agent repo — see
	// commitGate, which is the guard the opt-in was always assuming and
	// never had.
	Update UpdateSet
}

// Plan is what an import would do, before it does any of it.
//
// Items and Problems are always non-nil, so the JSON form carries empty arrays
// rather than nulls.
type Plan struct {
	// Items is every state-6/7 entry considered, including the ones that
	// will not be written. A user has to be able to see what was skipped and
	// why, not just what was chosen.
	Items []Item `json:"items"`

	// Problems are paths this plan could not judge: a destination inside the
	// repo whose safety could not be determined — an unreadable file there,
	// a failed hash — or a source file that could not be hashed at all.
	//
	// Distinct from the scan's own problems, which are paths out in the
	// projects that were never classified. A problem here is about a file
	// this plan meant to act on, and means we could not tell whether acting
	// would destroy something or what we would be copying. The item is
	// dropped rather than written.
	Problems []scan.ScanProblem `json:"problems"`
}

// NewPlan decides what to do with every entry whose content the repo does not
// already hold.
//
// It reads destinations but writes nothing. Entries in states 1-5 are ignored
// entirely: a healthy link, a broken link, a foreign link, and an adoptable
// file all have their content in the repo already (or belong to someone else),
// so there is nothing to capture. In particular state 5 — byte-identical to
// its counterpart — is already captured by definition, and re-importing it
// would write a file identical to the one being compared against.
func NewPlan(repo *scan.Repo, entries []scan.Entry, opts Options) Plan {
	p := Plan{
		Items:    make([]Item, 0, len(entries)),
		Problems: make([]scan.ScanProblem, 0),
	}

	for _, e := range entries {
		if !e.State.AtRisk() {
			continue
		}

		item := Item{Entry: e, Dest: repo.BackupPath(e.Project, e.Name)}

		// Entry.AtRisk, not State.AtRisk: the state says only that the
		// repo has no copy, while the entry also accounts for the file
		// being committed in its own project.
		if !e.AtRisk() && !opts.IncludeTracked {
			item.set(ActionSkipTracked, "committed in its own repo; --include-tracked captures it anyway")
			p.Items = append(p.Items, item)
			continue
		}

		// Hashed here rather than inside judgeDest, which used to skip it
		// whenever the destination was absent. Every item needs the value:
		// it is what Apply verifies the copied bytes against, so an item
		// planned without one could only be written unverified.
		srcHash, err := sourceHash(e)
		if err != nil {
			p.Problems = append(p.Problems, scan.ScanProblem{Path: e.Path, Err: err.Error()})
			continue
		}
		item.SrcHash = srcHash

		verdict, err := judgeDest(item, opts)
		if err != nil {
			p.Problems = append(p.Problems, scan.ScanProblem{Path: item.Dest, Err: err.Error()})
			continue
		}
		if verdict.action == ActionImport && verdict.reason == "" {
			// The destination check cannot say why this file needs
			// capturing — only the state can.
			verdict.reason = importReason(e.State)
		}
		item.DestHash = verdict.destHash
		item.Refusal = verdict.refusal
		item.set(verdict.action, verdict.reason)
		p.Items = append(p.Items, item)
	}

	// Sorted so the report and the JSON are deterministic; the scan yields
	// entries in filesystem walk order.
	sort.Slice(p.Items, func(i, j int) bool {
		if p.Items[i].Entry.Project != p.Items[j].Entry.Project {
			return p.Items[i].Entry.Project < p.Items[j].Entry.Project
		}
		return p.Items[i].Entry.Name < p.Items[j].Entry.Name
	})
	return p
}

// importReason distinguishes the two states worth capturing: an unadopted file
// has no counterpart in the repo at all, while an override has one and
// deliberately differs from it. Both are content the repo does not hold, and a
// report that called them the same thing would hide which is which.
func importReason(s scan.State) string {
	switch s {
	case scan.StateOverride:
		return "customized; differs from its repo counterpart"
	case scan.StateUnadopted:
		return "exists only in the project; no repo copy"
	default:
		return s.String()
	}
}

// verdict is judgeDest's answer: what to do, why, what was there, and which
// guard refused if one did. It is a struct rather than four returns because
// the fourth was one positional string too many to read at the call site.
type verdict struct {
	action   Action
	reason   string
	destHash string
	refusal  Refusal
}

// judgeDest decides what the destination allows.
//
// item carries the project file's hash as NewPlan established it — always via
// sourceHash, never Entry.Hash directly, so that this comparison and the one
// Apply makes are answering the same question about the same bytes.
func judgeDest(item Item, opts Options) (verdict, error) {
	dest := item.Dest

	// Lstat, never Stat, for the same reason the classifier uses it: Stat
	// follows symlinks, so a symlinked destination would present as a
	// regular file and the write would silently replace something we never
	// looked at.
	fi, err := os.Lstat(dest)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return verdict{action: ActionImport}, nil
		}
		return verdict{action: ActionInvalid}, fmt.Errorf("lstat %q: %w", dest, err)
	}
	if !fi.Mode().IsRegular() {
		return verdict{action: ActionConflict, reason: "destination exists and is not a regular file"}, nil
	}

	destHash, err := scan.HashFile(dest)
	if err != nil {
		return verdict{action: ActionInvalid}, fmt.Errorf("hashing %q: %w", dest, err)
	}

	if destHash == item.SrcHash {
		return verdict{ActionCaptured, "already captured, identical content", destHash, RefusalNone}, nil
	}
	if !opts.Update.Allows(item.Entry.Project, item.Entry.Name) {
		return verdict{ActionConflict, "captured copy differs from the project file", destHash, RefusalNone}, nil
	}

	// The user authorised replacing this capture. Whether the replacement is
	// survivable is a separate question, and it is the one nothing used to
	// ask.
	if refusal, why := commitGate(dest); refusal != RefusalNone {
		return verdict{ActionConflict, "--update refused: " + why, destHash, refusal}, nil
	}
	return verdict{ActionUpdate, "captured copy differs; replacing it (--update)", destHash, RefusalNone}, nil
}

// commitGate decides whether a capture may be replaced at all, whatever the
// user opted into. It returns RefusalNone and an empty string when it may.
//
// --update's entire safety argument is that project-agents/ is version
// controlled: replacing a capture is survivable because the bytes being
// replaced stay reachable from a commit. Nothing verified that. A capture
// written by one run and replaced by the next --update before anyone committed
// is gone permanently, with no copy anywhere — which is precisely the loss this
// package exists to prevent, performed by the command meant to prevent it.
//
// What it checks is the bytes, not the path. A path-only check (is this file in
// HEAD) passes a capture that was committed as v1 and has since been edited to
// v2, and replacing it destroys v2, which is in no commit. That case needs no
// hand-editing to reach: --update itself leaves the capture modified against
// HEAD, so a second --update before anyone commits destroys the first one's
// result. scan.CommittedInHEAD asks both halves — the path is in HEAD, and the
// working file matches what HEAD holds — and only a yes to both opens the gate.
//
// scan.CommittedNo and scan.CommittedUnknown are different facts, and are
// reported as different refusals because only one of them has a fix the user
// can carry out, but both refuse. Making the permissive answer the single named
// case rather than the default arm is the point: every way the probe can fail
// to say yes — git missing, a repo git will not talk about under safe.directory,
// a corrupt object store, no repository at all — then lands on the refusing side
// without any call site remembering to handle it.
func commitGate(dest string) (Refusal, string) {
	switch scan.CommittedInHEAD(dest) {
	case scan.CommittedYes:
		return RefusalNone, ""
	case scan.CommittedNo:
		return RefusalUncommitted, "the captured copy's current bytes are in no commit — it is untracked, or it has been edited since it was last committed — so replacing it would leave them nowhere at all; commit project-agents/ first, then re-run"
	default:
		return RefusalCommitUnknown, "git could not say whether the captured copy's current bytes are committed, and an unanswered question is not permission to replace what may be the only copy"
	}
}

// sourceHash returns the hash of the project file, preferring the one the
// classifier already computed.
//
// The fallback matters for correctness, not convenience: an entry with no hash
// compared against a destination hash would never be equal, so a plan built
// from a hand-made entry would report a conflict on a file it had already
// captured. Recomputing is cheap and cannot be wrong.
//
// This is the only place a project file's hash is derived. Every comparison —
// the planner's against the destination, and the writer's against the bytes it
// copied — reads Item.SrcHash, which is this function's answer. Two call sites
// deriving it two ways would disagree exactly where it matters: an entry with
// an empty Hash would be planned as an import and then refused at write time by
// a check comparing against nothing.
func sourceHash(e scan.Entry) (string, error) {
	if e.Hash != "" {
		return e.Hash, nil
	}
	h, err := scan.HashFile(e.Path)
	if err != nil {
		return "", fmt.Errorf("hashing %q: %w", e.Path, err)
	}
	return h, nil
}

// Counts tallies items by action.
func (p Plan) Counts() map[Action]int {
	counts := make(map[Action]int)
	for _, i := range p.Items {
		counts[i.Action]++
	}
	return counts
}

// Writes returns the items an Apply would write, in plan order.
func (p Plan) Writes() []Item {
	return p.filter(func(i Item) bool { return i.Action.Writes() })
}

// Conflicts returns the items that cannot be written safely.
func (p Plan) Conflicts() []Item {
	return p.filter(func(i Item) bool { return i.Action == ActionConflict })
}

// Refused returns the items one named guard turned down, in plan order. It is
// what lets a report offer the remedy for a particular refusal without matching
// on the wording of Reason.
func (p Plan) Refused(r Refusal) []Item {
	if r == RefusalNone {
		return nil
	}
	return p.filter(func(i Item) bool { return i.Refusal == r })
}

func (p Plan) filter(keep func(Item) bool) []Item {
	out := make([]Item, 0, len(p.Items))
	for _, i := range p.Items {
		if keep(i) {
			out = append(out, i)
		}
	}
	return out
}
