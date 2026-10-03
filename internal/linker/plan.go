// Package linker replaces a project's real agent files with symlinks into the
// canonical agent repository, so one copy in this repo serves every project.
//
// It is the third command in the build order — scan classifies, import
// captures, link provisions — and the first one that writes into a directory
// this repo does not own. Everything below is shaped by that.
//
// Three rules do the work.
//
// Planning and applying are separate, exactly as in the importer. NewPlan
// reads and judges; only Plan.Apply writes, and only what the plan selected.
//
// The import-before-link invariant is enforced here, not documented here. A
// project holding a file whose content exists nowhere else — Entry.AtRisk —
// is refused as a whole: no file in it is linked, adopted, or created. That is
// what makes losing such a file structurally impossible rather than merely
// unlikely, and it is a per-project refusal, so one unprotected file does not
// stop an unrelated project from being provisioned.
//
// The symlink target is always Repo.Counterpart, never the global agent by
// name. Counterpart prefers project-agents/<key>/<name>.md over
// agents/<name>.md, which is the difference between pointing a captured
// override at its own captured copy and pointing it at a different agent that
// merely shares its name. A project copy of an agent that differs from the
// canonical one of the same name is the example: linking it to
// the canonical one would be exactly the silent destruction this tool exists to
// prevent.
//
// link both writes and reads internal/state: every symlink it creates is
// recorded there, in one save at the end of Apply, and a *declined* marker means
// a person deleted that link on purpose, so it is reported and not recreated.
// Nothing here writes a marker — concluding intent from an absence is
// reconcile's judgement — and clearing one is the only override link offers.
package linker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/state"
)

// Flag names this package's diagnostics refer to, kept here for the same
// reason scan.AllowOutsideHomeFlag is: a message telling a user to pass a flag
// that does not exist is worse than no message at all.
const (
	ProjectFlag      = "project"
	AgentFlag        = "agent"
	ExcludeLocalFlag = "exclude-local"
	UndeclineFlag    = "undecline"
)

// Action is what a link run would do, or did, with one agent in one project.
//
// Every scan.State maps onto exactly one action, plus two that no state
// produces: ActionCreate for an agent explicitly requested where no file
// exists, and ActionMissing for one requested that the repo cannot supply.
type Action int

const (
	// ActionInvalid is the zero value and never appears in a plan.
	ActionInvalid Action = iota

	// ActionCreate is state 1: nothing is at this path and the agent was
	// explicitly requested. Creates the symlink.
	ActionCreate

	// ActionAdopt is state 5: a real file byte-identical to its
	// counterpart. Replacing it with a symlink loses nothing.
	ActionAdopt

	// ActionLinked is state 2: a healthy link already pointing at this
	// project's counterpart. Nothing to do — this is what makes a second
	// run a no-op.
	ActionLinked

	// ActionMismatch is state 2 pointing at the wrong repo file: healthy,
	// ours, but not this project's counterpart. Reported and left alone;
	// see judgeLink for why link does not repoint it.
	ActionMismatch

	// ActionBroken is state 3: a link into the repo whose target is gone.
	// Reported, never unlinked.
	ActionBroken

	// ActionForeign is state 4: a link pointing outside the repo. Someone
	// else owns it; report only, and it does not fail the run.
	ActionForeign

	// ActionOverride is state 6: a real file that differs from its
	// counterpart. Never touched. Reachable only for a file that is
	// committed in its own project's repo — an uncaptured one blocks the
	// whole project instead.
	ActionOverride

	// ActionUnadopted is state 7: a real file with no counterpart at all.
	// Never touched, and reachable on the same terms as ActionOverride.
	ActionUnadopted

	// ActionMissing is an agent named by --agent that the repo cannot
	// supply: no project copy, no global agent. Nothing to link to.
	ActionMissing

	// ActionDeclined is an agent the state file records as deliberately
	// deleted: the link was there, a person removed it, nothing took its
	// place. Reported, never recreated. Only a *requested* agent can reach
	// it, since only a requested agent would otherwise be created.
	//
	// Appended rather than filed next to ActionCreate: Action marshals as a
	// number, so inserting an arm re-maps every action number in a report
	// somebody has already saved.
	ActionDeclined
)

func (a Action) String() string {
	switch a {
	case ActionCreate:
		return "create"
	case ActionAdopt:
		return "adopt"
	case ActionLinked:
		return "linked"
	case ActionMismatch:
		return "mismatch"
	case ActionBroken:
		return "broken"
	case ActionForeign:
		return "foreign"
	case ActionOverride:
		return "override"
	case ActionUnadopted:
		return "unadopted"
	case ActionMissing:
		return "missing"
	case ActionDeclined:
		return "declined"
	default:
		return "invalid"
	}
}

// Writes reports whether this action creates a symlink.
func (a Action) Writes() bool {
	return a == ActionCreate || a == ActionAdopt
}

// Unresolved reports whether this action means the run found something only a
// human can settle, which is what exit 1 asserts.
//
// A foreign link is deliberately not one of them. It is somebody else's link,
// correctly left alone, and a permanent feature of a machine that uses another
// tool alongside this one — failing on it forever would make the exit code
// useless. An override or an unadopted file that reaches this point is
// likewise permanent by design: it is committed in its own project's repo, so
// import skipped it on purpose and link must not spend the run's exit code
// complaining about a decision already made.
//
// ActionDeclined is not one either: it is a decision the user already made, so
// there is nothing left to settle, and failing on it would exit 1 forever in any
// project where somebody once deleted an agent. Changing it is --undecline.
func (a Action) Unresolved() bool {
	return a == ActionBroken || a == ActionMismatch || a == ActionMissing
}

// Item is one agent in one project and what the link run decided about it.
type Item struct {
	Project string `json:"project"`
	Name    string `json:"name"`

	// Path is where the symlink lives, or would live, inside the project.
	Path string `json:"path"`

	// Target is the repo file the link points at or would point at —
	// always Repo.Counterpart for this project and name. For a broken or
	// foreign link it is the target the existing link already has, which is
	// what a report has to name.
	Target string `json:"target,omitempty"`

	Action Action `json:"action"`

	// ActionName is Action in printable form. Action itself marshals as a
	// number, matching scan.State; the name is here so a jq pipeline can
	// read a report without a copy of this enum.
	ActionName string `json:"action_name"`

	// Reason explains the action in one clause, and is what the text report
	// prints.
	Reason string `json:"reason,omitempty"`

	// Hash is the SHA-256 the plan judged an adoption by: the project
	// file's content, which by definition is also the counterpart's. Apply
	// re-checks both sides against it, so a file that stops being adoptable
	// between the two phases is refused rather than replaced.
	Hash string `json:"hash,omitempty"`

	// Entry is the classification this item came from. It is nil for
	// ActionCreate and ActionMissing, which describe a path that has no
	// file to classify.
	Entry *scan.Entry `json:"entry,omitempty"`
}

func (i Item) String() string {
	return fmt.Sprintf("%s %s/%s -> %s", i.Action, i.Project, i.Name, i.Target)
}

func (i *Item) set(a Action, reason string) {
	i.Action = a
	i.ActionName = a.String()
	i.Reason = reason
}

// Blocked is a project this run refused to operate in.
//
// Two things cause it: the import-before-link invariant, and the project's own
// git tracking the directory link would write into. Both are per-project
// refusals, so one bad project never stops the rest.
//
// The offending entries are carried rather than counted: for an invariant
// refusal the user has to run import against these exact files, and a bare
// count would not tell them which.
type Blocked struct {
	Project string `json:"project"`
	Dir     string `json:"dir"`

	// Reason is the one-clause explanation the report prints.
	Reason string `json:"reason"`

	// AtRisk holds the entries whose content the repo does not have and
	// whose own project has not committed either — Entry.AtRisk. Empty for
	// a refusal that is not about the invariant.
	AtRisk []scan.Entry `json:"at_risk,omitempty"`
}

// ProjectGit is what the project's own git said about a directory this run
// would write into.
//
// It is recorded only for projects that have something to write. A project
// with nothing to do is never judged on the shape of its repository, because
// refusing it would make an idempotent re-run fail on a directory nothing was
// going to touch.
type ProjectGit struct {
	Project  string `json:"project"`
	Dir      string `json:"dir"`
	Tracking string `json:"tracking"`
	Note     string `json:"note"`
}

// Exclude is a line an apply would add to a project's .git/info/exclude so the
// links written there stay invisible to that project's git.
//
// .git/info/exclude, never .gitignore. .gitignore is a tracked, shared file:
// writing a rule into it commits a decision on behalf of everyone who ever
// clones that repo, and does it in the user's name. info/exclude is local to
// the clone, achieves exactly the same thing for the person running this
// command, and leaves .claude/ itself trackable — projects legitimately keep
// settings there and may want them committed. This is a standing rule, not a
// preference of this command.
type Exclude struct {
	Project string `json:"project"`

	// File is the .git/info/exclude path that would be appended to.
	File string `json:"file"`

	// Pattern is the line that would be added, anchored to the repository
	// root with a leading slash so it cannot also match a same-named
	// directory elsewhere in the tree.
	Pattern string `json:"pattern"`
}

func (e Exclude) String() string {
	return fmt.Sprintf("%s: %s in %s", e.Project, e.Pattern, e.File)
}

// Unresolved is a --project key that names no project.
//
// It is kept apart from Plan.Problems because it means something different: a
// problem is the tool failing to read something it needed, while this is the
// caller naming something that is not there. The first is exit 2, the second
// is exit 1.
type Unresolved struct {
	Project string `json:"project"`
	Reason  string `json:"reason"`
}

func (u Unresolved) String() string {
	return fmt.Sprintf("%s: %s", u.Project, u.Reason)
}

// Options controls which projects a plan covers and what it provisions into
// them.
//
// The zero value covers every project the scan found and provisions only what
// is already on disk there. Detection — deciding which agents a project
// *should* have — is deliberately not part of this package; link provisions
// what it is told to provision, and the manifest from issue #2 will feed the
// same Agents field when it exists.
type Options struct {
	// Projects restricts the run to these project keys, as produced by
	// scan.ProjectKey. Empty means every project found under the scan root.
	//
	// A key naming a project the scan did not find is still usable: it is
	// resolved through scan.ProjectDir, which is how an agent gets linked
	// into a project whose .claude/agents directory does not exist yet.
	Projects []string

	// Agents names agents to link into the selected projects even where no
	// file is present — state 1, the only state nothing on disk can
	// produce.
	//
	// It requires Projects. See Validate.
	Agents []string

	// ExcludeLocal opts into making the links invisible to a project's own
	// git, by adding the agents directory to its .git/info/exclude before
	// linking.
	//
	// Without it, a project whose git *tracks* that directory is refused
	// outright: a symlink into this repo carries an absolute path that
	// resolves on one machine, so committing one hands every other clone a
	// broken link. With it, a directory git can see at all — tracked, or
	// merely untracked-and-not-ignored — is excluded first and linked
	// second, which is what makes the flag's promise true rather than
	// partially true.
	//
	// It does not untrack anything. Files already committed under that
	// directory stay committed, and replacing one with a symlink still shows
	// up as a change in that project; excluding only stops the *new* links
	// from being added. Untracking is a rewrite of someone else's repository
	// and is not this command's to make.
	ExcludeLocal bool
}

// Validate rejects option combinations and values that would do something
// surprising, before anything is planned.
func (o Options) Validate() error {
	if len(o.Agents) > 0 && len(o.Projects) == 0 {
		return fmt.Errorf("--%s requires --%s: without it, naming an agent would link it into every project found under the scan root, which is never what anyone means",
			AgentFlag, ProjectFlag)
	}
	for _, name := range o.Agents {
		if err := validAgentName(name); err != nil {
			return err
		}
	}
	return nil
}

// validAgentName requires a bare agent name — the thing that goes before .md,
// with no directory part.
//
// An agent name is joined onto the project's agents directory to make the path
// a link is created at, so a name carrying a path escapes that directory: an
// "agent" of "../../../agents/shared" writes outside the project entirely,
// and MkdirAll obligingly builds the way there. The damage is not that the
// value is untrusted — a user typed it — but that both safety gates are
// computed for the project's agents directory. A write that lands somewhere
// else was checked against neither the import-before-link invariant nor the
// project's git tracking, so the checks pass and then do not apply to the file
// that actually gets created.
func validAgentName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("--%s: empty agent name", AgentFlag)
	case name == "." || name == "..":
		return fmt.Errorf("--%s %q: not an agent name", AgentFlag, name)
	case strings.ContainsRune(name, '/'), strings.ContainsRune(name, filepath.Separator):
		// Checked before Base, which would quietly reduce a path to its
		// last component and accept it as though it had been typed bare.
		return fmt.Errorf("--%s %q: an agent name is a bare name with no directory part; it names a file in the project's own agents directory and cannot point outside it",
			AgentFlag, name)
	case name != filepath.Base(name):
		return fmt.Errorf("--%s %q: not a bare agent name", AgentFlag, name)
	}
	return nil
}

// Plan is what a link run would do, before it does any of it.
//
// Every slice is non-nil after NewPlan, so the JSON form carries empty arrays
// rather than nulls.
type Plan struct {
	// Items is every agent considered, including the ones that will not be
	// touched. What was left alone and why is the part a user cannot
	// reconstruct from the filesystem.
	Items []Item `json:"items"`

	// Blocked lists the projects this run refused, with the reason for each.
	Blocked []Blocked `json:"blocked"`

	// Git records what each written-to project's own git says about its
	// agents directory, whether or not that led to a refusal. A user has to
	// be able to see that their links are local-only, not just that nothing
	// went wrong.
	Git []ProjectGit `json:"git"`

	// Excludes are the .git/info/exclude lines an apply would write, in the
	// order it would write them. Always empty without Options.ExcludeLocal,
	// and empty for a project whose exclude file already holds the line.
	Excludes []Exclude `json:"excludes"`

	// Unknown lists --project keys that named no project.
	Unknown []Unresolved `json:"unknown"`

	// Problems are paths this plan could not judge — a counterpart it could
	// not hash, an entry in a state it does not understand. The item is
	// dropped rather than acted on: not knowing what a file is, is not a
	// licence to replace it.
	Problems []scan.ScanProblem `json:"problems"`

	// Dirs are the .claude/agents directories this run is about, blocked
	// ones included. It is what the home guard must be given: the block
	// decision itself depends on the project key being right, so a wrong
	// $HOME misjudges a refused project just as badly as a provisioned one.
	Dirs []string `json:"dirs"`
}

// NewPlan decides what to do with every agent in every selected project.
//
// It reads and hashes, and writes nothing. The error is returned only for an
// input that must not be planned around at all; everything else that can go
// wrong is recorded in the plan, because one bad project must not blind the run
// to the rest.
//
// st is a parameter rather than something this function loads: a plan's
// verdicts depend on the repo, what is on disk, and what this tool recorded, and
// the third is as load-bearing as the second. A nil st is an error, not an empty
// document — see state.Load on why the empty document is never a safe default.
func NewPlan(repo *scan.Repo, res scan.Result, st *state.File, opts Options) (Plan, error) {
	if st == nil {
		return Plan{}, errors.New("no state document: link cannot tell whether an agent was declined without one")
	}
	if err := opts.Validate(); err != nil {
		return Plan{}, err
	}

	p := Plan{
		Items:    make([]Item, 0, len(res.Entries)),
		Blocked:  make([]Blocked, 0),
		Git:      make([]ProjectGit, 0),
		Excludes: make([]Exclude, 0),
		Unknown:  make([]Unresolved, 0),
		Problems: make([]scan.ScanProblem, 0),
		Dirs:     make([]string, 0, len(res.Dirs)),
	}

	selected, unknown := selectProjects(repo, res, opts.Projects)
	p.Unknown = append(p.Unknown, unknown...)

	byProject := make(map[string][]scan.Entry, len(selected))
	for _, e := range res.Entries {
		byProject[e.Project] = append(byProject[e.Project], e)
	}

	for _, key := range sortedKeys(selected) {
		dir := selected[key]
		entries := byProject[key]
		p.Dirs = append(p.Dirs, dir)

		// The invariant, before anything about this project is planned.
		// Entry.AtRisk rather than State.AtRisk: the state says only that
		// the agent repo has no copy, while the entry also accounts for
		// the file being committed in its own project's repository. A
		// file protected there is not the situation the invariant guards,
		// and refusing on it would block link in a project whose agents are
		// committed, where nothing is actually at risk — the case
		// project-agents/README.md left open. Nothing is lost by letting
		// it through: states 6 and 7 are never touched either way, so the
		// only thing the softer gate permits is linking *other* agents in
		// the same directory.
		if risky := atRisk(entries); len(risky) > 0 {
			p.Blocked = append(p.Blocked, Blocked{
				Project: key,
				Dir:     dir,
				Reason:  "holds files this repo has no copy of; capture them with `agentsync import --apply` first",
				AtRisk:  risky,
			})
			continue
		}

		items, problems := judgeProject(repo, st, key, dir, entries, opts.Agents)
		p.Problems = append(p.Problems, problems...)

		// The second gate: what the project's own git says about the
		// directory. It is asked only of a project this run would actually
		// write into. A project with nothing to do must not be refused for
		// the shape of its repository — that would turn every idempotent
		// re-run over a tracked directory into a failure about a directory
		// nothing was going to touch.
		if !anyWrites(items) {
			p.Items = append(p.Items, items...)
			continue
		}

		g := gitGate(key, dir, opts.ExcludeLocal)
		p.Git = append(p.Git, g.note)
		if g.blocked != nil {
			p.Blocked = append(p.Blocked, *g.blocked)
			continue
		}
		if g.exclude != nil {
			p.Excludes = append(p.Excludes, *g.exclude)
		}
		p.Items = append(p.Items, items...)
	}

	// Sorted so the report and the JSON are deterministic; the scan yields
	// entries in filesystem walk order.
	sort.Slice(p.Items, func(i, j int) bool {
		if p.Items[i].Project != p.Items[j].Project {
			return p.Items[i].Project < p.Items[j].Project
		}
		return p.Items[i].Name < p.Items[j].Name
	})
	return p, nil
}

// judgeProject decides what to do with everything in one project: each file
// already there, plus each agent the caller asked for that is not.
//
// It is a separate step from committing the items to the plan because the git
// gate below has to know whether this project would be written to at all, and
// that is only knowable once every item is judged.
func judgeProject(repo *scan.Repo, st *state.File, project, dir string, entries []scan.Entry, agents []string) ([]Item, []scan.ScanProblem) {
	items := make([]Item, 0, len(entries)+len(agents))
	var problems []scan.ScanProblem

	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.Name] = true
		item, prob := Judge(repo, e)
		if prob != nil {
			problems = append(problems, *prob)
			continue
		}
		items = append(items, item)
	}

	// Requested agents that have no file on disk. A requested agent that does
	// have one is already covered by its own state — asking for it again must
	// not produce a second item claiming the path is empty.
	//
	// The declined marker is consulted only here. It records one observation
	// — the link was gone and nothing stood in its place — so it speaks only
	// to the absent case; adopting a real file destroys nothing and must not
	// be suppressed by it.
	for _, name := range agents {
		if present[name] {
			continue
		}
		items = append(items, requested(repo, st, project, dir, name))
	}
	return items, problems
}

func anyWrites(items []Item) bool {
	for _, i := range items {
		if i.Action.Writes() {
			return true
		}
	}
	return false
}

// gate is one project's outcome from the git check: what to report, whether to
// refuse, and what to exclude first if not.
type gate struct {
	note    ProjectGit
	blocked *Blocked
	exclude *Exclude
}

// gitGate decides whether a project's own git permits writing symlinks into
// its agents directory.
//
// The hazard is specific to symlinks and does not exist for the file copies
// import makes. A link into this repo names an absolute path — on this machine
// /Users/<user>/Web/agents-repo/agents/foo.md — so committing one hands every
// other clone of that project a link into a directory that is not there. A
// directory the project's git tracks is therefore refused by default rather
// than linked into; ExcludeLocal is the explicit opt-in that makes the links
// local first and writes them second.
//
// "git could not be asked" is a refusal, never a pass. Treating it as "not
// tracked" would mean the one condition that disables the check — git missing,
// a directory that vanished — is also the one that lets the damaging write
// through.
func gitGate(project, dir string, excludeLocal bool) gate {
	g := scan.InspectDir(dir)
	note := ProjectGit{Project: project, Dir: dir, Tracking: g.Tracking.String()}

	switch g.Tracking {
	case scan.DirNoRepo:
		note.Note = "not a git repository; nothing to reconcile"
		return gate{note: note}

	case scan.DirIgnored:
		note.Note = "ignored by its own git; these links stay local to this machine"
		return gate{note: note}

	case scan.DirTracked, scan.DirVisible:
		// Handled below: both are directories git can see.

	default:
		note.Note = "git could not be asked about this directory"
		return gate{note: note, blocked: &Blocked{
			Project: project,
			Dir:     dir,
			Reason:  "git could not say whether this directory is tracked, and a symlink committed into one breaks every other clone; refusing rather than assuming it is not",
		}}
	}

	if !excludeLocal {
		if g.Tracking == scan.DirTracked {
			note.Note = "tracked by its own git; linking would commit machine-specific absolute paths"
			return gate{note: note, blocked: &Blocked{
				Project: project,
				Dir:     dir,
				Reason: fmt.Sprintf("its own git tracks this directory, and a symlink into this repo names an absolute path that resolves only on this machine; pass --%s to add it to .git/info/exclude first",
					ExcludeLocalFlag),
			}}
		}
		// Untracked and not ignored: the links will show up in git status,
		// which is noise a user can see and act on rather than damage a
		// clone inherits. Reported, not refused.
		note.Note = fmt.Sprintf("its own git can see this directory; the links will show as untracked changes (--%s hides them)", ExcludeLocalFlag)
		return gate{note: note}
	}

	if g.GitDir == "" || g.Rel == "" {
		note.Note = "git did not name this repository's git directory, so no local exclusion could be planned"
		return gate{note: note, blocked: &Blocked{
			Project: project,
			Dir:     dir,
			Reason: fmt.Sprintf("--%s was given but git did not name this repository's git directory, so the links could not be made local; refusing rather than linking into a directory git can see",
				ExcludeLocalFlag),
		}}
	}

	// Anchored to the repository root. Without the leading slash the pattern
	// would also match a same-named directory anywhere else in the tree, which
	// in a monorepo silently hides another package's files.
	pattern := "/" + g.Rel + "/"
	file := filepath.Join(g.GitDir, "info", "exclude")

	present, err := excludePresent(file, pattern)
	if err != nil {
		note.Note = "this repository's exclude file could not be read"
		return gate{note: note, blocked: &Blocked{
			Project: project,
			Dir:     dir,
			Reason:  fmt.Sprintf("reading %s failed (%v), so the links could not be made local; refusing", file, err),
		}}
	}
	if present {
		note.Note = "already excluded locally; these links stay on this machine"
		return gate{note: note}
	}

	note.Note = fmt.Sprintf("git can see this directory; %s will be added to %s first", pattern, file)
	return gate{note: note, exclude: &Exclude{Project: project, File: file, Pattern: pattern}}
}

// excludePresent reports whether an exclude file already carries this pattern,
// which is what makes a second --exclude-local run write nothing.
//
// A missing file is not an error: a repository that has never had a local
// exclusion simply has no info/exclude, and the write path creates it.
func excludePresent(file, pattern string) (bool, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.TrimSpace(line) == pattern {
			return true, nil
		}
	}
	return false, nil
}

// atRisk returns the entries in a project whose content is protected nowhere.
func atRisk(entries []scan.Entry) []scan.Entry {
	var risky []scan.Entry
	for _, e := range entries {
		if e.AtRisk() {
			risky = append(risky, e)
		}
	}
	return risky
}

// Judge maps one classified entry onto the action link takes for it.
//
// The mapping is the state table from issue #2 and nothing else: states 3, 4,
// 6 and 7 are reported and left alone, state 2 is a no-op, and state 5 is the
// only on-disk state link writes for.
//
// Exported for doctor, which reports on the same link states. "Is this link
// healthy, and is it pointing at this project's counterpart?" is one question,
// and two implementations of it would drift — silently, and in the direction of
// doctor calling a project clean while link refuses it, or the reverse. A caller
// that only wants the link verdict passes only the three symlink states; the
// rest of the table costs a hash it has no use for.
func Judge(repo *scan.Repo, e scan.Entry) (Item, *scan.ScanProblem) {
	item := Item{Project: e.Project, Name: e.Name, Path: e.Path, Entry: &e}

	switch e.State {
	case scan.StateHealthyLink:
		judgeLink(repo, &item, e)

	case scan.StateBrokenLink:
		item.Target = e.LinkTarget
		item.set(ActionBroken, "link into the repo whose target is gone; restore the agent or remove the link by hand — link never unlinks")

	case scan.StateForeignLink:
		item.Target = e.LinkTarget
		item.set(ActionForeign, "points outside the repo; not ours to touch")

	case scan.StateAdoptable:
		item.Target = e.Counterpart
		hash, err := sourceHash(e)
		if err != nil {
			prob := scan.ScanProblem{Path: e.Path, Err: err.Error()}
			return item, &prob
		}
		item.Hash = hash
		item.set(ActionAdopt, "identical to its counterpart; replacing it with a link loses nothing")

	case scan.StateOverride:
		item.Target = e.Counterpart
		// Only reachable for a file committed in its own project's repo;
		// an uncaptured one blocked the project above.
		item.set(ActionOverride, "customized and not captured here, but committed in its own repo; left alone")

	case scan.StateUnadopted:
		item.set(ActionUnadopted, "no copy in this repo, but committed in its own repo; left alone")

	default:
		prob := scan.ScanProblem{Path: e.Path, Err: fmt.Sprintf("unhandled state %v; refusing to act on it", e.State)}
		return item, &prob
	}
	return item, nil
}

// judgeLink separates a healthy link that is already correct from one that
// points at the wrong file in the right repo.
//
// The distinction is not cosmetic. Counterpart prefers a project's captured
// copy over the global agent of the same name, so a link pointing at
// agents/<name>.md in a project that has project-agents/<key>/<name>.md is
// serving that project a different agent than the one this repo records as
// its own — the override case, in its residual form: the file is not being
// destroyed, it is being ignored.
//
// link reports it and does not repoint it. Repointing is a rewrite of a link
// somebody made deliberately, and choosing between the two repo files is
// exactly the judgement the state table reserves for a human. It costs an
// exit 1 rather than an edit.
func judgeLink(repo *scan.Repo, item *Item, e scan.Entry) {
	want := repo.Counterpart(e.Project, e.Name)
	if want == "" {
		// A link into the repo pointing at something Counterpart does not
		// offer: the agent was moved, or the link was hand-made against
		// another project's capture.
		item.Target = e.LinkTarget
		item.set(ActionMismatch, "links into the repo, but at no file this project has as a counterpart")
		return
	}
	item.Target = want
	if pointsAt(e, want) {
		item.set(ActionLinked, "already linked to its counterpart")
		return
	}
	// The remedy is spelled out because refusing to act is only reasonable if
	// the user is told what acting looks like. Removing the link and asking
	// for it again by name recreates it at the counterpart, which is the same
	// two steps link would have taken, done deliberately.
	item.set(ActionMismatch, fmt.Sprintf("links to %s, but this project's counterpart is %s; remove the link and re-run with --%s %s --%s %s to recreate it at the counterpart",
		e.LinkTarget, want, ProjectFlag, e.Project, AgentFlag, e.Name))
}

// pointsAt reports whether a link already leads to target.
//
// Both the literal destination and the resolved one are accepted: the target
// is built from Repo.Root, which NewRepo resolved, but a link may name the
// same file through a path that only matches after resolution — or before it,
// when the final component is itself a link. Comparing one form only would
// report a correct link as a mismatch.
func pointsAt(e scan.Entry, target string) bool {
	if target == "" {
		return false
	}
	if e.LinkTarget == target || e.LinkResolved == target {
		return true
	}
	resolved, err := filepath.EvalSymlinks(target)
	return err == nil && (e.LinkResolved == resolved || e.LinkTarget == resolved)
}

// requested builds the item for an agent named on the command line that has no
// file in the project — state 1, the only state the filesystem cannot produce
// on its own.
func requested(repo *scan.Repo, st *state.File, project, dir, name string) Item {
	item := Item{
		Project: project,
		Name:    name,
		Path:    filepath.Join(dir, name+".md"),
	}

	// Checked before the counterpart, so an agent that is both declined and
	// absent from this repo reports as declined: "no such agent" would send
	// someone looking for a missing file when the real reason is a decision
	// recorded against this exact pair.
	if d, ok := st.LookupDeclined(state.Key{Project: project, Name: name}); ok {
		item.set(ActionDeclined, fmt.Sprintf("declined on %s (%s); link does not recreate a link that was deleted on purpose — pass --%s=%s/%s with --apply to clear the marker and link it again",
			d.DeclinedAt, d.Observation, UndeclineFlag, project, name))
		return item
	}

	target := repo.Counterpart(project, name)
	if target == "" {
		item.set(ActionMissing, "requested, but this repo holds no agent by that name for this project")
		return item
	}
	item.Target = target
	item.set(ActionCreate, "requested and absent; create the link")
	return item
}

// sourceHash returns the hash of the project file, preferring the one the
// classifier already computed, exactly as the importer does. An entry with no
// hash compared against a freshly-read one would never be equal, so a plan
// built from a hand-made entry would refuse every adoption it planned.
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

// selectProjects resolves the run's project selection to key -> agents
// directory, along with the keys that named nothing.
func selectProjects(repo *scan.Repo, res scan.Result, want []string) (map[string]string, []Unresolved) {
	scanned := make(map[string]string, len(res.Dirs))
	for _, dir := range res.Dirs {
		key := scan.ProjectKey(repo.Home, dir)
		// First wins. Two directories keying the same is only possible
		// through a symlink arrangement the key scheme already treats as
		// one project, and picking either is equivalent.
		if _, ok := scanned[key]; !ok {
			scanned[key] = dir
		}
	}
	if len(want) == 0 {
		return scanned, nil
	}

	selected := make(map[string]string, len(want))
	var unknown []Unresolved
	for _, key := range want {
		// A key from ProjectKey never carries a trailing separator, so a
		// typed "Web/example/" would match nothing. Normalised here rather
		// than at the flag, where the same tidying would also launder an
		// --agent value past the check that it is a bare name.
		key = strings.TrimRight(key, "/")
		if dir, ok := scanned[key]; ok {
			selected[key] = dir
			continue
		}
		dir, err := deriveDir(repo, key)
		if err != nil {
			unknown = append(unknown, Unresolved{Project: key, Reason: err.Error()})
			continue
		}
		selected[key] = dir
	}
	return selected, unknown
}

// deriveDir turns a project key the scan did not find into the agents
// directory it names.
//
// The scan can only find a .claude/agents directory that already exists, so a
// project that has never had one is invisible to it — and that project is
// precisely the state-1 case --agent exists to serve. The key is the only
// handle on it.
//
// Two checks keep that from provisioning nonsense. The project directory
// itself must exist, so a typo creates no tree; and the derived directory must
// key back to the key that was asked for, which catches the one thing the
// inverse cannot reproduce — a path component that is a symlink resolves to a
// different key, and linking there would provision a different project than
// the one named.
func deriveDir(repo *scan.Repo, key string) (string, error) {
	projectDir, err := scan.ProjectDir(repo.Home, key)
	if err != nil {
		return "", err
	}
	// os.Stat, not Lstat, and deliberately so: the question here is whether
	// a project exists, and a project directory reached through a symlink is
	// an ordinary layout. The Lstat rule governs agent *files*, where
	// following a link is what turns a healthy link into a false adoptable.
	//
	// This does fold "not there" together with "could not look" — audited
	// under issue #12 and kept, because here both answers are the same
	// answer. Every non-nil error returns, so an unreadable path refuses to
	// provision exactly as a missing one does, and refusing is the safe arm
	// on a write path. The wording is the only casualty: "no project at X"
	// is imprecise for a permission error. It is not misleading in the way
	// the import gate's was, because it prescribes nothing — the user is
	// told which path failed and goes and looks at it, rather than being
	// sent to run a command that cannot work.
	fi, err := os.Stat(projectDir)
	if err != nil {
		return "", fmt.Errorf("no project at %q", projectDir)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%q is not a directory", projectDir)
	}

	agentsDir := filepath.Join(projectDir, ".claude", "agents")
	if got := scan.ProjectKey(repo.Home, agentsDir); got != key {
		return "", fmt.Errorf("%q keys as %q, not %q; use the key the scan reports", projectDir, got, key)
	}
	return agentsDir, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

// Unresolved returns the items that need a human, in plan order.
func (p Plan) Unresolved() []Item {
	return p.filter(func(i Item) bool { return i.Action.Unresolved() })
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
