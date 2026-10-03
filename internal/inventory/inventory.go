// Package inventory renders a derived view of every agent on this machine:
// the global tier, every project's agents, and the plugin agents this tool does
// not manage.
//
// # Derived, never authoritative
//
// The filesystem is the source of truth and this file is a materialized view of
// it. That direction is the whole design, and reversing it turns the inventory
// from a convenience into the same class of hazard the classifier exists to
// prevent: the file says one thing, the disk says another, and a tool that
// trusts the file writes based on a picture that stopped being true.
//
// Concretely, and these are constraints on callers as much as on this package:
//
//   - Nothing may decide from the inventory. import and link re-scan before
//     writing, always, and neither imports this package.
//   - A reader may use it for orientation and reporting, and must re-scan
//     before acting.
//   - Regenerating is cheap and safe at any moment, and is the documented fix
//     for any inconsistency.
//
// Get that right and a stale inventory is merely inconvenient. So this package
// only reads, and the one thing it writes is its own output.
//
// # Distinct from a state file
//
// A future .agentsync-state.json records what the tool *did* — links created,
// content hash at link time — and is authoritative, because detecting tampering
// needs a trusted record of the last known-good state. This records what *is*.
// If the state file is lost, divergence detection degrades; if this is lost,
// regenerate it. They look similar and have opposite trust levels, which is why
// they are separate files and separate packages.
//
// # Why regeneration is byte-identical
//
// The output carries a schema number and nothing else volatile: no generation
// timestamp, no tool version. Both were in the original sketch and both are
// dropped deliberately.
//
// A timestamp makes every regeneration a diff, which destroys the property that
// makes this file worth committing — that `git diff --exit-code` after a
// regenerate is a true freshness check, and any diff at all is a real change in
// the tree. Git already records when the file was written, more trustworthily
// than a field the file asserts about itself. A tool version is worse still
// here, since an untagged build's version contains the commit and so changes on
// every commit, churning the file when nothing about the machine changed.
//
// Schema is hand-bumped, so it changes when the shape changes and never
// otherwise.
//
// # Machine portability
//
// Every path is a key relative to $HOME, or relative to the repo for something
// inside it — never absolute. That keeps the file meaningful on another machine
// and keeps diffs readable. Where a path genuinely cannot be expressed either
// way it is kept absolute and is plainly identifiable as such.
//
// # The one failure that is quiet
//
// Every unknown here resolves downward — a missing settings.json means nothing
// is enabled, a missing install registry means nothing is installed — so no
// absent file can ever inflate what the document claims loads. One of those two
// is quieter than the other and the asymmetry is worth naming.
//
// A missing settings.json still leaves plugin_agents_installed above zero
// against plugin_agents_enabled at zero, and that gap is visible to a reader. A
// missing installed_plugins.json empties plugins[] and zeroes both counters at
// once, which is indistinguishable from a machine with no plugins installed: the
// document is then internally consistent, silently low, and says nothing about
// why. On a Claude Code old enough to predate the install registry that is the
// normal state, and agents_loaded_globally would understate what every session
// costs with no sign in the file at all.
//
// RegistryGap detects exactly that combination — no registry, non-empty cache —
// so the command can say so on stderr. It is deliberately not a field in the
// document: a field describing the run rather than the machine is the kind of
// volatile content that breaks byte-identical regeneration.
package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// Schema is the shape of the emitted document. Bump it by hand when a field is
// added, removed, or changes meaning; never derive it from anything.
//
// "Changes" means changes to a *published* shape — anything from the first
// commit that carries inventory.json onward. Renaming a field in a working tree
// whose file has never been committed changes nothing anyone could have read,
// and bumping for it would assert a migration away from a shape that never
// existed outside that tree. This number is a promise to readers of past
// commits, so only a commit can put a shape under it.
const Schema = 1

// GlobalDirName is the agent directory Claude Code loads in every session,
// relative to the user's home. Scanning does not cover it — scan walks projects
// beneath a root, and this sits outside any project — which is the gap this
// package closes.
const GlobalDirName = ".claude/agents"

// pluginsDirName holds installed plugins and the marketplace catalogues they
// came from, relative to home.
const pluginsDirName = ".claude/plugins"

// settingsFileName is where Claude Code records which plugins are enabled,
// relative to home.
const settingsFileName = ".claude/settings.json"

// cacheDirName is the subtree of downloaded plugin versions. Its siblings —
// notably marketplaces/ — are catalogues of what is *available*, which is a
// different question and must never be counted as what is installed.
//
// It is a cache, and the name is meant literally: a directory here can outlive
// the install that put it there. See installedVersions.
const cacheDirName = "cache"

// installedPluginsFileName is Claude Code's own record of which plugins are
// installed and where, relative to home. It, not the cache directory listing,
// is what "installed" means.
const installedPluginsFileName = ".claude/plugins/installed_plugins.json"

// Inventory is the emitted document.
//
// Slice fields are always non-nil so the JSON carries empty arrays rather than
// nulls: a consumer distinguishing "no plugins" from "the field was missing"
// should not have to.
type Inventory struct {
	Schema   int       `json:"schema"`
	Global   []Agent   `json:"global"`
	Projects []Project `json:"projects"`
	Plugins  []Plugin  `json:"plugins"`
	Summary  Summary   `json:"summary"`
}

// Agent is one agent file, wherever it lives.
type Agent struct {
	Name string `json:"name"`

	// State is scan.State's string form for a project agent. It is stored as
	// text rather than as the integer the enum uses, because the integer is
	// an implementation detail that would silently re-map every record in
	// this file if a state were ever inserted in the middle of the list.
	//
	// A global-tier agent takes "healthy-link", "broken-link" or
	// "foreign-link" — spelled identically to scan's, and meaning the same
	// thing — or one of two values scan has no use for: "file" for a real
	// file, "irregular" for anything else. It cannot take "adoptable",
	// "override" or "unadopted", because all three are judgements about a
	// per-project counterpart and the global tier has none. See
	// globalLinkState.
	State string `json:"state"`

	// Target is where a symlink points, empty for a real file. Repo-relative
	// when it points into the agent repo, otherwise $HOME-relative with a
	// leading "~/", otherwise absolute.
	Target string `json:"target,omitempty"`
}

// Project is one project's agents, addressed by key.
type Project struct {
	// Key is scan.ProjectKey's output: a $HOME-relative path such as
	// Web/example. Never an absolute path and never a bare directory name —
	// two projects can share a leaf name, and only the key tells them apart.
	Key    string  `json:"key"`
	Agents []Agent `json:"agents"`
}

// Plugin is one installed plugin and the agents it ships.
type Plugin struct {
	Name        string `json:"name"`
	Marketplace string `json:"marketplace"`
	Version     string `json:"version,omitempty"`

	// Enabled is whether this plugin's agents actually load. Installed and
	// enabled are tracked separately and deliberately: the plugin tree holds
	// installed-but-disabled plugins alongside enabled ones, and counting
	// files on disk as "what loads" overstates the real cost several times
	// over. That is not a hypothetical — it is the error this field exists
	// to make impossible to repeat.
	//
	// Being present in this list already means the install registry vouches
	// for the directory, so Enabled is the second of two independent checks,
	// not the only one. See readPlugins for why one without the other leaves
	// a hole.
	Enabled bool     `json:"enabled"`
	Agents  []string `json:"agents"`
}

// key identifies a plugin the way Claude Code's own settings do:
// "<plugin>@<marketplace>". Several records can share one key, because the
// cache keeps superseded version directories beside the current one — and only
// one version of a plugin is ever loaded. The summary counts by this, never by
// record; see summarize.
func (p Plugin) key() string { return p.Name + "@" + p.Marketplace }

// Summary is the tally, so a reader does not have to compute the obvious.
type Summary struct {
	GlobalAgents  int `json:"global_agents"`
	Projects      int `json:"projects"`
	ProjectAgents int `json:"project_agents"`

	// ByState tallies project agents only. The global tier is excluded
	// because it is classified against a different question — there is no
	// per-project counterpart up there, so no adoptable/override/unadopted
	// distinction exists to tally — and mixing the two vocabularies into one
	// map would make every number in it ambiguous.
	ByState map[string]int `json:"by_state"`

	// AtRisk is the count of project agents whose content is protected
	// nowhere. It is the one number here that should always be zero.
	AtRisk int `json:"at_risk"`

	// PluginsShippingAgents and PluginsWithAgentsEnabled count distinct
	// plugins that ship at least one agent — NOT how many plugins are
	// installed or enabled on this machine, which is a much larger number
	// and not what this file is about. readPlugins drops a plugin shipping
	// no agents entirely, so quoting either of these as a plugin count is
	// wrong by several times over; the names say "agents" because that is
	// the only population they describe.
	//
	// Both count plugins rather than records: a key with two installed
	// version directories is one plugin.
	PluginsShippingAgents    int `json:"plugins_shipping_agents"`
	PluginsWithAgentsEnabled int `json:"plugins_with_agents_enabled"`

	// PluginAgentsInstalled counts agents shipped by plugins the install
	// registry actually records, which is narrower than what sits in the
	// plugin cache — a cache directory can outlive its install. See
	// installedVersions.
	PluginAgentsInstalled int `json:"plugin_agents_installed"`

	// PluginAgentsEnabled counts agents from installed plugins that are also
	// enabled: what actually loads. Compare against PluginAgentsInstalled
	// before quoting either as a cost.
	//
	// Exact whenever each plugin has one installed version, which is the
	// normal state and the only one observed. Where a registry lists several
	// installs of one plugin — different scopes — only one of them loads in
	// any given session and this becomes an upper bound, because the union of
	// their agent names is counted. See summarize.
	PluginAgentsEnabled int `json:"plugin_agents_enabled"`

	// AgentsLoadedGlobally is what every session pays regardless of which
	// project it runs in: the global tier plus enabled plugin agents. Project
	// agents are excluded because they load only in their own project.
	AgentsLoadedGlobally int `json:"agents_loaded_globally"`
}

// Build assembles the inventory from a completed scan plus the two trees the
// scan does not cover: the global tier and the plugin cache.
//
// Everything portable is expressed relative to repo.Home and repo.Root rather
// than to the environment, so a key here means the same thing as a key
// everywhere else in this tool — and so a test can point a fixture at a temp
// directory and get the same answers the real tree would give.
func Build(repo *scan.Repo, res scan.Result) (Inventory, error) {
	inv := Inventory{
		Schema:   Schema,
		Global:   make([]Agent, 0),
		Projects: make([]Project, 0),
		Plugins:  make([]Plugin, 0),
		Summary:  Summary{ByState: map[string]int{}},
	}
	if repo == nil {
		return inv, errors.New("inventory: nil repo")
	}

	global, err := readGlobal(repo)
	if err != nil {
		return inv, err
	}
	inv.Global = global

	inv.Projects = groupByProject(repo, res)
	plugins, err := readPlugins(repo.Home)
	if err != nil {
		return inv, err
	}
	inv.Plugins = plugins

	inv.Summary = summarize(inv, res)
	return inv, nil
}

// readGlobal lists ~/.claude/agents.
//
// A missing directory is not an error: the migration that created it may not
// have run, and reporting an empty global tier is the honest answer rather than
// refusing to produce an inventory at all.
func readGlobal(repo *scan.Repo) ([]Agent, error) {
	out := make([]Agent, 0)
	if repo.Home == "" {
		return out, nil
	}
	dir := filepath.Join(repo.Home, GlobalDirName)

	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("reading the global agent directory %q: %w", dir, err)
	}

	for _, ent := range ents {
		name := ent.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(dir, name)

		// Lstat, never Stat: Stat follows the link and would describe the
		// target, so a broken link would look like a missing file and a link
		// into the repo would look like a regular file. The distinction
		// between a link and the thing it points at is the entire subject
		// here.
		fi, err := os.Lstat(path)
		if err != nil {
			// Skipped, not fatal. ReadDir returned a name and the stat then
			// failed, which in practice means the entry went away between the
			// two — an agent deleted while the walk was running. Aborting the
			// whole document over one vanished file would make regeneration
			// fail exactly when it is least able to be retried, and this is a
			// derived view whose documented fix is to run it again.
			continue
		}

		a := Agent{Name: strings.TrimSuffix(name, ".md")}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, rerr := os.Readlink(path)
			if rerr != nil {
				return out, fmt.Errorf("reading the link %q: %w", path, rerr)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			target = filepath.Clean(target)
			a.Target = renderPath(repo, target)
			a.State = globalLinkState(repo, target)
		case fi.Mode().IsRegular():
			a.State = "file"
		default:
			a.State = "irregular"
		}
		out = append(out, a)
	}

	// Stable: a directory cannot hold two entries of the same name, so the
	// comparison is total in practice — but see readPlugins on why that is an
	// invariant held elsewhere rather than a property of this code.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// globalLinkState classifies a link in the global tier.
//
// This intentionally mirrors the shape of scan's own classifyLink, which is
// unexported and takes a project counterpart this tier does not have. The three
// state names are spelled identically so "healthy-link" means the same thing in
// both places; the judgement is simpler here because the global tier has no
// per-project counterpart and so no mismatch state to be in. A link either
// resolves into this repo, resolves somewhere else, or does not resolve.
//
// The duplication is deliberate and small. Exporting scan's version to share it
// would mean exporting a signature built around a counterpart argument that is
// meaningless here, which would make the shared function harder to read than
// the two separate ones.
func globalLinkState(repo *scan.Repo, target string) string {
	if _, err := os.Stat(target); err != nil {
		return "broken-link"
	}
	if within(repo.Root, target) {
		return "healthy-link"
	}
	return "foreign-link"
}

// groupByProject folds the scan's flat entry list into per-project records.
func groupByProject(repo *scan.Repo, res scan.Result) []Project {
	byKey := map[string][]Agent{}
	for _, e := range res.Entries {
		a := Agent{Name: e.Name, State: e.State.String()}
		if e.LinkTarget != "" {
			a.Target = renderPath(repo, e.LinkTarget)
		}
		byKey[e.Project] = append(byKey[e.Project], a)
	}

	// Both sorts are stable. The map fold above means the input order here is
	// Go's map iteration order — deliberately randomized — so an unstable sort
	// over a non-total comparison would be the one place in this package where
	// byte-identical output could actually break. Neither duplicate can occur
	// today (a directory holds one file per name, and a key names one project),
	// which is exactly the kind of invariant that should not be load-bearing.
	out := make([]Project, 0, len(byKey))
	for key, agents := range byKey {
		sort.SliceStable(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
		out = append(out, Project{Key: key, Agents: agents})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// enabledPlugins reads which plugins are switched on.
//
// The key is "<plugin>@<marketplace>". A plugin absent from the map is
// disabled: the file lists what has been decided about, and no decision means
// not on. Erring that way keeps the count of what loads from drifting upward on
// a parse surprise.
func enabledPlugins(home string) (map[string]bool, error) {
	out := map[string]bool{}
	if home == "" {
		return out, nil
	}
	path := filepath.Join(home, settingsFileName)

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("reading %q: %w", path, err)
	}

	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		return out, fmt.Errorf("parsing %q: %w", path, err)
	}
	for k, v := range settings.EnabledPlugins {
		out[k] = v
	}
	return out, nil
}

// installedVersions reads Claude Code's registry of installed plugins, keyed
// "<plugin>@<marketplace>" and holding the version directory names it records
// as installed.
//
// This exists because a cache directory can outlive its install. On the machine
// this was written against, cache/claude-code-plugins/pr-review-toolkit/1.0.0
// still holds six agent files while the registry lists that plugin only under
// claude-plugins-official — the directory is a leftover from before the plugin
// moved marketplaces, and nothing in it loads. A directory listing cannot tell
// that apart from an install; the registry states it outright.
//
// Treating the cache as the definition of "installed" is the same error as
// treating marketplaces/ as the definition of "installed", differing only in
// magnitude, so the same answer applies: ask the file whose job it is.
//
// The version is taken from the install path's last element rather than from
// the version field beside it, because the directory name is the thing being
// matched and the two can differ. The path is used for its final element only —
// never compared whole — since it is absolute and recorded against whatever the
// home directory looked like at install time, which need not match the resolved
// home this runs with.
//
// A missing file means nothing is installed, exactly as a missing settings.json
// means nothing is enabled: absent evidence of an install is not evidence of
// one, and this is the direction that cannot inflate what the document claims
// loads.
func installedVersions(home string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	if home == "" {
		return out, nil
	}
	path := filepath.Join(home, installedPluginsFileName)

	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("reading %q: %w", path, err)
	}

	var registry struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(b, &registry); err != nil {
		return out, fmt.Errorf("parsing %q: %w", path, err)
	}

	for key, installs := range registry.Plugins {
		for _, in := range installs {
			version := in.Version
			if in.InstallPath != "" {
				version = filepath.Base(in.InstallPath)
			}
			if version == "" {
				continue
			}
			if out[key] == nil {
				out[key] = map[string]bool{}
			}
			out[key][version] = true
		}
	}
	return out, nil
}

// RegistryGap explains the one state in which this package's plugin figures are
// silently and unrecoverably low — the install registry absent while the plugin
// cache is not — and returns "" when that is not the case.
//
// readPlugins then finds no install to vouch for anything, drops every cached
// directory, and produces a document that reads exactly like a machine with no
// plugins: internally consistent, and wrong with nothing to show for it. See the
// package doc.
//
// It returns the sentence rather than a bool, following HomeCheck.Explain and
// Report.HomeWarning, so the paths it names stay beside the constants that
// define them instead of being spelled a second time at the print site.
//
// It answers only about that combination. An unreadable registry is a different
// problem, and readPlugins reports it as an error rather than a gap, so this
// stays silent there and the louder signal wins.
func RegistryGap(home string) string {
	if home == "" {
		return ""
	}
	_, err := os.Lstat(filepath.Join(home, installedPluginsFileName))
	if !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	cache := filepath.Join(home, pluginsDirName, cacheDirName)
	ents, err := os.ReadDir(cache)
	if err != nil || len(ents) == 0 {
		return ""
	}
	return fmt.Sprintf("no plugin install registry at ~/%s, but ~/%s is not empty. "+
		"Every plugin count here is reported as zero, which understates what each "+
		"session loads.",
		installedPluginsFileName, filepath.Join(pluginsDirName, cacheDirName))
}

// readPlugins lists installed plugins and the agents they ship.
//
// Only the cache/ subtree is walked. Its sibling marketplaces/ is a catalogue
// of everything *available* from every registered marketplace, most of which is
// not installed and none of which loads. Counting that tree is how a plugin
// agent total comes out several times too large.
//
// The walk is then narrowed by the install registry, which is the same
// intent applied one level in: marketplaces/ holds plugins that were never
// installed, and cache/ holds directories whose install is gone. Both are files
// on disk that do not load, and neither belongs in a count of what does.
//
// The narrowing also closes a hole the enabled flag alone cannot. "Enabled"
// comes from settings.json and "installed" would otherwise come from a
// directory listing, with nothing checking either against the registry — so an
// orphaned cache directory whose key still carried a stale true in settings.json
// would put phantom agents straight into PluginAgentsEnabled, the one number
// here that claims to say what actually loads. That the current orphan happens
// to be absent from settings.json is luck, not design.
func readPlugins(home string) ([]Plugin, error) {
	out := make([]Plugin, 0)
	if home == "" {
		return out, nil
	}

	enabled, err := enabledPlugins(home)
	if err != nil {
		return out, err
	}
	installed, err := installedVersions(home)
	if err != nil {
		return out, err
	}

	cache := filepath.Join(home, pluginsDirName, cacheDirName)
	markets, err := os.ReadDir(cache)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("reading the plugin cache %q: %w", cache, err)
	}

	for _, market := range markets {
		if !isDir(filepath.Join(cache, market.Name())) {
			continue
		}
		plugins, err := os.ReadDir(filepath.Join(cache, market.Name()))
		if err != nil {
			return out, fmt.Errorf("reading plugins of %q: %w", market.Name(), err)
		}
		for _, plugin := range plugins {
			if !isDir(filepath.Join(cache, market.Name(), plugin.Name())) {
				continue
			}
			versions, err := os.ReadDir(filepath.Join(cache, market.Name(), plugin.Name()))
			if err != nil {
				return out, fmt.Errorf("reading versions of %q: %w", plugin.Name(), err)
			}
			for _, version := range versions {
				dir := filepath.Join(cache, market.Name(), plugin.Name(), version.Name())
				if !isDir(dir) {
					continue
				}
				p := Plugin{
					Name:        plugin.Name(),
					Marketplace: market.Name(),
					Version:     version.Name(),
				}
				// The key is built once, by the same method the summary counts
				// by and the settings file is read with. Its exact form is
				// load-bearing and a second hand-written copy of it is a second
				// place for it to be wrong.
				if !installed[p.key()][p.Version] {
					continue
				}
				p.Enabled = enabled[p.key()]

				agents, err := pluginAgents(filepath.Join(dir, "agents"))
				if err != nil {
					return out, err
				}
				if len(agents) == 0 {
					// A plugin shipping no agents costs nothing in the terms
					// this file is about, and listing every one would bury the
					// handful that do. This is why the plugin counts in Summary
					// are named for agents rather than for plugins — they are
					// not a count of what is installed on this machine.
					continue
				}
				p.Agents = agents
				out = append(out, p)
			}
		}
	}

	// Stable, and with a total order over the three fields that identify a
	// record. Stability is belt to the order's braces: the filesystem cannot
	// currently produce two records sharing all three, so the comparison is
	// total in practice — but byte-identical output should be a property of
	// this code rather than of an invariant held somewhere else.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Marketplace != out[j].Marketplace {
			return out[i].Marketplace < out[j].Marketplace
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// isDir follows symlinks, unlike DirEntry.IsDir.
//
// Deliberately the opposite choice from readGlobal, which is scrupulous about
// never following one. The two are asking different questions: up there a
// symlink *is* the subject, since the whole report is about what links to what,
// while here a symlinked plugin directory is simply a plugin directory and
// skipping it would silently drop a real install. A path that cannot be stat'ed
// is not a directory, which also skips a symlink whose target is gone.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func pluginAgents(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %q: %w", dir, err)
	}
	var out []string
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".md") {
			continue
		}
		out = append(out, strings.TrimSuffix(ent.Name(), ".md"))
	}
	sort.Strings(out)
	return out, nil
}

func summarize(inv Inventory, res scan.Result) Summary {
	s := Summary{
		GlobalAgents: len(inv.Global),
		Projects:     len(inv.Projects),
		ByState:      map[string]int{},
	}
	for _, p := range inv.Projects {
		s.ProjectAgents += len(p.Agents)
		for _, a := range p.Agents {
			s.ByState[a.State]++
		}
	}
	s.AtRisk = len(res.AtRisk())

	// Tallied per plugin, never per record.
	//
	// readPlugins has already dropped every cache directory the install
	// registry does not vouch for, so in the normal case — one install per
	// plugin, which is every plugin on the machine this was written against —
	// there is exactly one record per key and this loop is a straight sum.
	//
	// The de-duplication is for the case the registry permits and this machine
	// does not exhibit: one plugin installed at more than one scope, listed
	// with more than one version. Those are one plugin, and adding up records
	// would report it twice.
	//
	// Where that happens the union of agent names is counted, which makes
	// PluginAgentsEnabled an upper bound rather than an exact figure, since
	// only one of those installs loads in any given session. The alternative
	// — picking one install's list — would be a guess dressed up as a
	// measurement, and an upper bound that says so beats a point estimate that
	// does not. PluginAgentsEnabled's doc states this rather than claiming an
	// exactness the union cannot deliver.
	//
	// Two marketplaces shipping a plugin of the same name are two plugins and
	// are counted twice, which is why the key carries the marketplace. They
	// are separately installable and separately enabled.
	installed := map[string]map[string]bool{}
	enabled := map[string]map[string]bool{}
	for _, p := range inv.Plugins {
		collect(installed, p.key(), p.Agents)
		if p.Enabled {
			collect(enabled, p.key(), p.Agents)
		}
	}
	s.PluginsShippingAgents = len(installed)
	s.PluginsWithAgentsEnabled = len(enabled)
	s.PluginAgentsInstalled = countAgents(installed)
	s.PluginAgentsEnabled = countAgents(enabled)

	s.AgentsLoadedGlobally = s.GlobalAgents + s.PluginAgentsEnabled
	return s
}

// collect unions a plugin's agent names into the set held under its key.
func collect(into map[string]map[string]bool, key string, agents []string) {
	set, ok := into[key]
	if !ok {
		set = map[string]bool{}
		into[key] = set
	}
	for _, a := range agents {
		set[a] = true
	}
}

func countAgents(sets map[string]map[string]bool) int {
	n := 0
	for _, set := range sets {
		n += len(set)
	}
	return n
}

// renderPath expresses an absolute path portably: repo-relative for something
// inside the agent repo, then $HOME-relative with a leading "~/", and only
// absolute when it is neither.
//
// Repo-relative is tried first because a path can be both, and the repo form is
// the more stable of the two — it survives the whole checkout moving, which the
// home form does not.
func renderPath(repo *scan.Repo, path string) string {
	if repo.Root != "" && within(repo.Root, path) {
		if rel, err := filepath.Rel(repo.Root, path); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	if repo.Home != "" && within(repo.Home, path) {
		if rel, err := filepath.Rel(repo.Home, path); err == nil {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}

// within reports whether path is dir itself or sits beneath it.
//
// It compares path components rather than string prefixes: "/a/bc" has "/a/b"
// as a string prefix but is not inside it, and treating it as inside would
// render a path relative to a directory that does not contain it.
func within(dir, path string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Marshal renders the document.
//
// Byte-identical for an identical tree, which is the property that makes
// `agentsync inventory --check` a meaningful freshness test. Three things
// deliver it: every slice is sorted by a total order above, encoding/json emits
// map keys sorted, and nothing volatile is in the document at all.
//
// HTML escaping is off because it would rewrite &, < and > inside agent names
// and paths into escapes, which serves a browser and nothing else here.
func (inv Inventory) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(inv); err != nil {
		return nil, fmt.Errorf("rendering the inventory: %w", err)
	}
	// Encode already terminates with a newline, so the file ends with exactly
	// one and git does not report a missing final newline.
	return buf.Bytes(), nil
}
