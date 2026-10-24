package inventory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// --- fixtures ---------------------------------------------------------------

// fixture is a throwaway machine: a directory standing in for $HOME, holding
// the agent repo, the projects beside it, the global tier at .claude/agents,
// and the plugin tree at .claude/plugins.
//
// Everything below drives the real readers over real directories rather than
// hand-building structs, for the reason doctor's tests do the same: the states
// this package reports differ from each other only in what os.Lstat says about
// a path, so an Agent asserting a state the filesystem does not actually
// produce would test the wrong thing exactly where it matters.
type fixture struct {
	t    *testing.T
	base string
	repo *scan.Repo
}

// projectsSubdir is the fixture's stand-in for ~/Web: the tree the scan is
// pointed at. Every project key in these tests begins with it.
const projectsSubdir = "Web"

func newFixture(t *testing.T) *fixture {
	t.Helper()

	base := t.TempDir()
	repoRoot := filepath.Join(base, "agents-repo")
	mustMkdir(t, filepath.Join(repoRoot, "agents"))
	// Projects live under base/Web and the scan is pointed there, never at the
	// base itself. The base holds .claude/agents — the global tier — and that
	// is a .claude/agents directory like any other, so a scan rooted at the
	// base would find it and report the fake home as a project. The real tool
	// cannot hit this (the default tree is a projects directory, and the global tier sits at
	// ~/.claude) but a fixture that scans its own home would, and every project
	// count below would silently be one too many.
	mustMkdir(t, filepath.Join(base, projectsSubdir))

	repo, err := scan.NewRepo(repoRoot)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}

	// Resolved so paths built here compare equal to the ones NewRepo reports;
	// on macOS a temp dir is reached through /var, which is a symlink. Home is
	// pointed at the base so keys and rendered paths land in the home-relative
	// namespace these tests describe rather than under _outside-home/.
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("resolving %s: %v", base, err)
	}
	repo.Home = resolved

	return &fixture{t: t, base: resolved, repo: repo}
}

// globalDir is ~/.claude/agents in the fixture's fake home.
func (f *fixture) globalDir() string {
	return filepath.Join(f.base, GlobalDirName)
}

// repoAgent writes an agent into the repo's global pool and returns its path,
// which is what a healthy global link points at.
func (f *fixture) repoAgent(name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.repo.Root, "agents", name+".md")
	mustWrite(f.t, p, content)
	return p
}

// globalFile puts a real file in the global tier — an agent this tool does not
// manage.
func (f *fixture) globalFile(name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.globalDir(), name)
	mustWrite(f.t, p, content)
	return p
}

// globalLink puts a link in the global tier. The target need not exist — that
// is what a broken link is.
func (f *fixture) globalLink(name, target string) string {
	f.t.Helper()
	p := filepath.Join(f.globalDir(), name+".md")
	mustMkdir(f.t, filepath.Dir(p))
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatalf("symlink %s -> %s: %v", p, target, err)
	}
	return p
}

// projectAgent writes a real file into a project's .claude/agents directory,
// where the scan will find and classify it.
func (f *fixture) projectAgent(project, name, content string) {
	f.t.Helper()
	mustWrite(f.t, filepath.Join(f.base, filepath.FromSlash(project),
		".claude", "agents", name+".md"), content)
}

// projectLink links a project's agent at the repo, which is what `agentsync
// link --apply` leaves behind.
func (f *fixture) projectLink(project, name, target string) {
	f.t.Helper()
	p := filepath.Join(f.base, filepath.FromSlash(project), ".claude", "agents", name+".md")
	mustMkdir(f.t, filepath.Dir(p))
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatalf("symlink %s -> %s: %v", p, target, err)
	}
}

// pluginAgent writes an agent into a cached plugin, at the exact depth Claude
// Code uses: cache/<marketplace>/<plugin>/<version>/agents/<name>.md.
//
// It does NOT register the plugin as installed — see install. A cache directory
// with no install record is an orphan, and keeping the two operations separate
// is what lets a test build one.
func (f *fixture) pluginAgent(market, plugin, version, name string) {
	f.t.Helper()
	mustWrite(f.t, filepath.Join(f.base, pluginsDirName, cacheDirName,
		market, plugin, version, "agents", name+".md"), "plugin agent\n")
}

// install records plugins in the registry Claude Code writes on install, which
// is what "installed" means. Anything in the cache that this does not name is
// a leftover directory and must not be counted.
//
// keys are "<plugin>@<marketplace>/<version>".
func (f *fixture) install(keys ...string) {
	f.t.Helper()
	type record struct {
		InstallPath string `json:"installPath"`
		Version     string `json:"version"`
		Scope       string `json:"scope"`
	}
	plugins := map[string][]record{}
	for _, k := range keys {
		key, version, ok := strings.Cut(k, "/")
		if !ok {
			f.t.Fatalf("install key %q is not <plugin>@<marketplace>/<version>", k)
		}
		plugin, market, ok := strings.Cut(key, "@")
		if !ok {
			f.t.Fatalf("install key %q is not <plugin>@<marketplace>/<version>", k)
		}
		plugins[key] = append(plugins[key], record{
			InstallPath: filepath.Join(f.base, pluginsDirName, cacheDirName,
				market, plugin, version),
			Version: version,
			Scope:   "user",
		})
	}
	b, err := json.Marshal(map[string]any{"version": 2, "plugins": plugins})
	if err != nil {
		f.t.Fatalf("marshal install registry: %v", err)
	}
	mustWrite(f.t, filepath.Join(f.base, installedPluginsFileName), string(b))
}

// marketplaceAgent writes an agent into the marketplace catalogue — a plugin
// that is merely *available*. Nothing here is installed and nothing here loads.
func (f *fixture) marketplaceAgent(market, plugin, version, name string) {
	f.t.Helper()
	mustWrite(f.t, filepath.Join(f.base, pluginsDirName, "marketplaces",
		market, plugin, version, "agents", name+".md"), "not installed\n")
}

func (f *fixture) settings(content string) {
	f.t.Helper()
	mustWrite(f.t, filepath.Join(f.base, settingsFileName), content)
}

// build runs the whole pipeline the command runs.
func (f *fixture) build() Inventory {
	f.t.Helper()
	return buildWith(f.t, f.repo, f.base)
}

// buildWith exists so a test can run the pipeline twice over one tree through
// two separately-constructed Repos, which is what proves regeneration is
// byte-identical rather than merely repeatable within a single process.
func buildWith(t *testing.T, repo *scan.Repo, base string) Inventory {
	t.Helper()
	res, err := repo.Scan(filepath.Join(base, projectsSubdir))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Problems) > 0 {
		t.Fatalf("fixture scan reported problems: %v", res.Problems)
	}
	inv, err := Build(repo, res)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return inv
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// agentNames is what most assertions below actually care about.
func agentNames(agents []Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.Name)
	}
	return out
}

func findAgent(t *testing.T, agents []Agent, name string) Agent {
	t.Helper()
	for _, a := range agents {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("no agent named %q in %v", name, agentNames(agents))
	return Agent{}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- byte-identical regeneration --------------------------------------------

// The property the whole design rests on, asserted directly rather than
// inferred from the absence of a timestamp field.
//
// Everything else here — the sorts, the schema-only header, the refusal to
// record a generation time or a tool version — exists to make this true, and
// `agentsync inventory --check` is only a freshness test because it is. Two
// full builds are used, over two independently-constructed Repos, so nothing
// cached inside one run can carry the answer into the next.
func TestMarshal_IsByteIdenticalAcrossTwoBuildsOfTheSameTree(t *testing.T) {
	f := newFixture(t)
	f.repoAgent("shared", "canonical\n")
	f.globalLink("shared", filepath.Join(f.repo.Root, "agents", "shared.md"))
	f.globalFile("loose.md", "unmanaged\n")
	f.projectAgent("Web/one", "alpha", "alpha\n")
	f.projectAgent("Web/two", "beta", "beta\n")
	f.pluginAgent("market", "plug", "1.0.0", "helper")
	f.install("plug@market/1.0.0")
	f.settings(`{"enabledPlugins": {"plug@market": true}}`)

	first, err := f.build().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// A second Repo over the same tree, not a reuse of the first: a Repo caches
	// per-directory git lookups, and reusing it would let the second run answer
	// from the first's memory instead of from the disk.
	got, err := buildWith(t, mustRepo(t, f.repo.Root, f.base), f.base).Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if string(got) != string(first) {
		t.Errorf("regeneration is not byte-identical; --check would report a\n"+
			"stale file on an unchanged tree.\nfirst:\n%s\nsecond:\n%s", first, got)
	}
}

func mustRepo(t *testing.T, root, home string) *scan.Repo {
	t.Helper()
	repo, err := scan.NewRepo(root)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	repo.Home = home
	return repo
}

// Marshal must end with exactly one newline and must not HTML-escape. The
// newline keeps git from reporting a missing one on a committed file; the
// escaping would rewrite & < > inside an agent name into & and friends,
// which serves a browser and nothing that reads this file.
func TestMarshal_EndsWithOneNewlineAndDoesNotEscapeHTML(t *testing.T) {
	f := newFixture(t)
	f.globalFile("a&b.md", "ampersand in the name\n")

	out, err := f.build().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.HasSuffix(string(out), "}\n") {
		t.Errorf("document does not end with exactly one newline after the closing brace:\n%q",
			lastBytes(out, 16))
	}
	if !strings.Contains(string(out), `"a&b"`) {
		t.Errorf("the & in an agent name was escaped:\n%s", out)
	}
}

func lastBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

// --- deterministic ordering -------------------------------------------------

// groupByProject folds through a map, so without the sorts the output order is
// Go's map iteration order — different on every run, and a whole-file diff on
// every regeneration. Feeding the entries in an order that is wrong twice over
// (projects descending, agents descending) is what makes the sorts load-bearing
// rather than incidentally satisfied by the input.
func TestGroupByProject_OrdersProjectsByKeyAndAgentsByName(t *testing.T) {
	f := newFixture(t)
	res := scan.Result{Entries: []scan.Entry{
		{Project: "Web/zeta", Name: "zulu", State: scan.StateAdoptable},
		{Project: "Web/zeta", Name: "alpha", State: scan.StateAdoptable},
		{Project: "Web/alpha", Name: "zulu", State: scan.StateAdoptable},
		{Project: "Web/alpha", Name: "alpha", State: scan.StateAdoptable},
		{Project: "Web/mid", Name: "mike", State: scan.StateAdoptable},
	}}

	// Repeated because a single pass can agree with map order by luck; the
	// point of the assertion is that every pass agrees with the same order.
	var want []string
	for i := 0; i < 20; i++ {
		got := []string{}
		for _, p := range groupByProject(f.repo, res) {
			got = append(got, p.Key+":"+strings.Join(agentNames(p.Agents), ","))
		}
		if want == nil {
			want = got
		}
		if !equalStrings(got, want) {
			t.Fatalf("ordering is not stable across runs:\n%v\nvs\n%v", got, want)
		}
	}

	expect := []string{"Web/alpha:alpha,zulu", "Web/mid:mike", "Web/zeta:alpha,zulu"}
	if !equalStrings(want, expect) {
		t.Errorf("order = %v, want %v", want, expect)
	}
}

// A linked project agent carries its target, rendered as a key like every other
// path in the document. An absolute target here would name a path that resolves
// on one machine only, which is exactly what the key form exists to avoid — and
// this is the field most likely to leak one, since it is read straight off the
// symlink.
func TestGroupByProject_RendersLinkTargetsAsKeys(t *testing.T) {
	f := newFixture(t)
	target := f.repoAgent("shared", "canonical\n")
	f.projectLink("Web/one", "shared", target)

	inv := f.build()
	if len(inv.Projects) != 1 {
		t.Fatalf("got %d projects, want 1: %+v", len(inv.Projects), inv.Projects)
	}
	a := findAgent(t, inv.Projects[0].Agents, "shared")
	if a.State != "healthy-link" {
		t.Errorf("state = %q, want healthy-link", a.State)
	}
	if a.Target != "agents/shared.md" {
		t.Errorf("target = %q, want agents/shared.md", a.Target)
	}
}

// readPlugins walks marketplace-major and must emit plugin-name-major. The
// fixture makes those two orders disagree — marketplace "aaa" ships "zeta",
// marketplace "zzz" ships "alpha" — so the sort cannot be satisfied by accident
// through ReadDir's own ordering, which is what a same-marketplace fixture
// would have let it do.
func TestReadPlugins_OrdersByNameThenMarketplaceThenVersion(t *testing.T) {
	f := newFixture(t)
	f.pluginAgent("aaa", "zeta", "1.0.0", "one")
	f.pluginAgent("zzz", "alpha", "1.0.0", "one")
	f.pluginAgent("mmm", "alpha", "2.0.0", "one")
	f.pluginAgent("mmm", "alpha", "1.0.0", "one")
	f.install("zeta@aaa/1.0.0", "alpha@zzz/1.0.0",
		"alpha@mmm/2.0.0", "alpha@mmm/1.0.0")

	plugins, err := readPlugins(f.base)
	if err != nil {
		t.Fatalf("readPlugins: %v", err)
	}
	got := make([]string, 0, len(plugins))
	for _, p := range plugins {
		got = append(got, p.Name+"@"+p.Marketplace+"/"+p.Version)
	}
	want := []string{
		"alpha@mmm/1.0.0",
		"alpha@mmm/2.0.0",
		"alpha@zzz/1.0.0",
		"zeta@aaa/1.0.0",
	}
	if !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// --- the global tier --------------------------------------------------------

func TestReadGlobal(t *testing.T) {
	t.Run("a link into the repo is healthy and its target is repo-relative", func(t *testing.T) {
		f := newFixture(t)
		f.repoAgent("shared", "canonical\n")
		f.globalLink("shared", filepath.Join(f.repo.Root, "agents", "shared.md"))

		agents := mustReadGlobal(t, f)
		a := findAgent(t, agents, "shared")
		if a.State != "healthy-link" {
			t.Errorf("state = %q, want healthy-link", a.State)
		}
		// Repo-relative, not "~/agents-repo/agents/shared.md": the repo form
		// survives the whole checkout moving, which the home form does not.
		if a.Target != "agents/shared.md" {
			t.Errorf("target = %q, want agents/shared.md", a.Target)
		}
	})

	t.Run("a link outside the repo is foreign and its target is home-relative", func(t *testing.T) {
		f := newFixture(t)
		elsewhere := filepath.Join(f.base, "elsewhere", "theirs.md")
		mustWrite(t, elsewhere, "not ours\n")
		f.globalLink("theirs", elsewhere)

		a := findAgent(t, mustReadGlobal(t, f), "theirs")
		if a.State != "foreign-link" {
			t.Errorf("state = %q, want foreign-link", a.State)
		}
		if a.Target != "~/elsewhere/theirs.md" {
			t.Errorf("target = %q, want ~/elsewhere/theirs.md", a.Target)
		}
	})

	t.Run("a broken link is reported, not an error", func(t *testing.T) {
		f := newFixture(t)
		f.globalLink("gone", filepath.Join(f.repo.Root, "agents", "gone.md"))

		a := findAgent(t, mustReadGlobal(t, f), "gone")
		if a.State != "broken-link" {
			t.Errorf("state = %q, want broken-link", a.State)
		}
		// The target is still rendered. A broken link whose destination is not
		// named cannot be acted on, and naming it is the whole report.
		if a.Target != "agents/gone.md" {
			t.Errorf("target = %q, want agents/gone.md", a.Target)
		}
	})

	t.Run("a real file has no target", func(t *testing.T) {
		f := newFixture(t)
		f.globalFile("loose.md", "unmanaged\n")

		a := findAgent(t, mustReadGlobal(t, f), "loose")
		if a.State != "file" {
			t.Errorf("state = %q, want file", a.State)
		}
		if a.Target != "" {
			t.Errorf("target = %q, want empty: a real file points at nothing", a.Target)
		}
	})

	t.Run("a missing directory is empty and not an error", func(t *testing.T) {
		f := newFixture(t)

		agents, err := readGlobal(f.repo)
		if err != nil {
			t.Fatalf("readGlobal on a missing directory: %v", err)
		}
		if len(agents) != 0 {
			t.Errorf("got %v, want none", agentNames(agents))
		}
		// Non-nil so the JSON carries [] rather than null; a consumer should
		// not have to tell "no global agents" from "the field was missing".
		if agents == nil {
			t.Error("the slice is nil, so the document would carry a JSON null")
		}
	})

	t.Run("something that is neither a file nor a link is irregular", func(t *testing.T) {
		f := newFixture(t)
		mustMkdir(t, filepath.Join(f.globalDir(), "adirectory.md"))

		// Reported rather than skipped: something named like an agent that is
		// not one is exactly what a reader needs to be told about, and dropping
		// it would leave the tier looking clean.
		a := findAgent(t, mustReadGlobal(t, f), "adirectory")
		if a.State != "irregular" {
			t.Errorf("state = %q, want irregular", a.State)
		}
	})

	t.Run("an unknown home yields nothing rather than reading the real one", func(t *testing.T) {
		// An unknown home is not an error anywhere in this tool. It must also
		// never fall through to the developer's actual ~/.claude/agents, which
		// would make a test's answer depend on the machine running it.
		agents, err := readGlobal(&scan.Repo{Root: t.TempDir()})
		if err != nil {
			t.Fatalf("readGlobal: %v", err)
		}
		if len(agents) != 0 {
			t.Errorf("got %v, want none", agentNames(agents))
		}
	})

	t.Run("non-.md entries are ignored", func(t *testing.T) {
		f := newFixture(t)
		f.globalFile("README", "not an agent\n")
		f.globalFile("notes.txt", "not an agent\n")
		f.globalFile("real.md", "an agent\n")
		mustMkdir(t, filepath.Join(f.globalDir(), "subdir"))

		agents := mustReadGlobal(t, f)
		if !equalStrings(agentNames(agents), []string{"real"}) {
			t.Errorf("got %v, want [real]", agentNames(agents))
		}
	})
}

// The Lstat/Stat distinction, pinned on its own so a regression names itself.
//
// Stat follows the link and describes the target, which breaks the global tier
// in two directions at once: a healthy link into the repo reports as a plain
// "file" with no target, and a broken link fails to stat at all — turning the
// single most important thing to report into a hard error that aborts the whole
// inventory. Both are asserted here, because a change to Stat that someone
// "fixed" by ignoring the error would still silently pass the first.
func TestReadGlobal_UsesLstatSoLinksAreNotMistakenForTheirTargets(t *testing.T) {
	f := newFixture(t)
	f.repoAgent("healthy", "canonical\n")
	f.globalLink("healthy", filepath.Join(f.repo.Root, "agents", "healthy.md"))
	f.globalLink("broken", filepath.Join(f.repo.Root, "agents", "never-existed.md"))

	agents, err := readGlobal(f.repo)
	if err != nil {
		t.Fatalf("readGlobal: %v — a broken link must be reported, never fatal", err)
	}

	healthy := findAgent(t, agents, "healthy")
	if healthy.State == "file" || healthy.Target == "" {
		t.Errorf("a symlink was described as its target (state %q, target %q); "+
			"this is os.Stat behaviour and readGlobal must use os.Lstat",
			healthy.State, healthy.Target)
	}
	broken := findAgent(t, agents, "broken")
	if broken.State != "broken-link" {
		t.Errorf("broken link state = %q, want broken-link", broken.State)
	}
}

func mustReadGlobal(t *testing.T, f *fixture) []Agent {
	t.Helper()
	agents, err := readGlobal(f.repo)
	if err != nil {
		t.Fatalf("readGlobal: %v", err)
	}
	return agents
}

// --- enabled plugins --------------------------------------------------------

func TestEnabledPlugins(t *testing.T) {
	t.Run("a missing settings file is empty and not an error", func(t *testing.T) {
		f := newFixture(t)

		got, err := enabledPlugins(f.base)
		if err != nil {
			t.Fatalf("enabledPlugins: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})

	t.Run("malformed JSON is an error", func(t *testing.T) {
		f := newFixture(t)
		f.settings(`{"enabledPlugins": {`)

		// Never a silent empty map. An unparseable settings file means the
		// enabled set is unknown, and returning "nothing is enabled" would let
		// a broken file quietly zero out the one number here that claims to
		// say what loads.
		if _, err := enabledPlugins(f.base); err == nil {
			t.Fatal("malformed settings parsed without error")
		}
	})

	t.Run("a plugin absent from the map counts as disabled", func(t *testing.T) {
		f := newFixture(t)
		f.settings(`{"enabledPlugins": {"on@market": true, "off@market": false}}`)

		got, err := enabledPlugins(f.base)
		if err != nil {
			t.Fatalf("enabledPlugins: %v", err)
		}
		if !got["on@market"] {
			t.Error(`"on@market" is not enabled`)
		}
		if got["off@market"] {
			t.Error(`"off@market" is enabled`)
		}
		// The file lists what has been decided about; no decision means not on.
		// Erring this way keeps the count of what loads from drifting upward.
		if got["never-mentioned@market"] {
			t.Error(`a plugin absent from the file counts as enabled`)
		}
	})
}

// --- the plugin cache -------------------------------------------------------

// The single most important assertion in this file.
//
// ~/.claude/plugins holds three different things: marketplaces/ is every plugin
// *available* from every registered marketplace, cache/ is what is actually
// installed, and within cache/ sit plugins that are installed but disabled.
// Only enabled plugins load. Globbing every agents/*.md under the whole tree
// once produced a count roughly six times the true figure, which was published
// and had to be retracted. This test exists so that cannot happen twice.
func TestReadPlugins_WalksOnlyTheCacheAndIgnoresMarketplaces(t *testing.T) {
	f := newFixture(t)
	f.pluginAgent("market", "installed", "1.0.0", "real")
	f.marketplaceAgent("market", "merely-available", "1.0.0", "phantom")
	// Same plugin name in both trees: the catalogue entry must not become a
	// second record of the installed one either.
	f.marketplaceAgent("market", "installed", "9.9.9", "phantom")
	f.install("installed@market/1.0.0")
	f.settings(`{"enabledPlugins": {"installed@market": true}}`)

	plugins, err := readPlugins(f.base)
	if err != nil {
		t.Fatalf("readPlugins: %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("got %d plugin records, want 1: %+v", len(plugins), plugins)
	}
	if plugins[0].Name != "installed" || plugins[0].Version != "1.0.0" {
		t.Errorf("got %s@%s/%s, want installed@market/1.0.0",
			plugins[0].Name, plugins[0].Marketplace, plugins[0].Version)
	}
	if !equalStrings(plugins[0].Agents, []string{"real"}) {
		t.Errorf("agents = %v, want [real]: an agent from marketplaces/ was counted",
			plugins[0].Agents)
	}
}

// The second half of the same measurement error, and the one that was missed
// the first time.
//
// A cache directory can outlive its install. On the machine this was written
// against, cache/claude-code-plugins/pr-review-toolkit/1.0.0 still holds six
// agent files while the install registry lists that plugin only under
// claude-plugins-official — a leftover from before it moved marketplaces.
// Nothing in it loads, and a directory listing cannot tell it apart from a real
// install. Counting it inflated plugin_agents_installed from 9 to 15 and made
// the command print a false sentence to the user.
func TestReadPlugins_DropsCacheDirectoriesWithNoInstallRecord(t *testing.T) {
	f := newFixture(t)
	f.pluginAgent("official", "plug", "2.0.0", "real")
	f.pluginAgent("oldmarket", "plug", "1.0.0", "orphan")
	// The registry knows only about the one under the new marketplace.
	f.install("plug@official/2.0.0")
	f.settings(`{"enabledPlugins": {"plug@official": true}}`)

	plugins, err := readPlugins(f.base)
	if err != nil {
		t.Fatalf("readPlugins: %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("got %d records, want 1 — the orphaned cache directory was counted: %+v",
			len(plugins), plugins)
	}
	if plugins[0].Marketplace != "official" {
		t.Errorf("kept %s, want the registered install", plugins[0].Marketplace)
	}
}

// The hole the enabled flag alone cannot close.
//
// "Enabled" comes from settings.json and, before the registry check, "installed"
// came from a directory listing, with nothing reconciling the two. An orphaned
// cache directory whose key still carried a stale true in settings.json would
// have put phantom agents straight into plugin_agents_enabled — the one number
// that claims to say what actually loads. That the real orphan happens to be
// absent from settings.json was luck, not design; this pins the design.
func TestReadPlugins_AnOrphanedDirectoryWithAStaleEnabledFlagIsStillDropped(t *testing.T) {
	f := newFixture(t)
	f.pluginAgent("oldmarket", "plug", "1.0.0", "phantom")
	f.install() // nothing installed at all
	f.settings(`{"enabledPlugins": {"plug@oldmarket": true}}`)

	inv := f.build()
	if len(inv.Plugins) != 0 {
		t.Fatalf("an uninstalled plugin was listed: %+v", inv.Plugins)
	}
	if inv.Summary.PluginAgentsEnabled != 0 {
		t.Errorf("plugin_agents_enabled = %d, want 0: a stale enabled flag on an "+
			"orphaned cache directory produced phantom agents",
			inv.Summary.PluginAgentsEnabled)
	}
}

func TestReadPlugins(t *testing.T) {
	t.Run("a missing install registry means nothing is installed", func(t *testing.T) {
		f := newFixture(t)
		f.pluginAgent("market", "plug", "1.0.0", "a")

		// Absent evidence of an install is not evidence of one, and this is the
		// direction that cannot inflate what the document claims loads — the
		// same reasoning as a missing settings.json meaning nothing is enabled.
		plugins, err := readPlugins(f.base)
		if err != nil {
			t.Fatalf("readPlugins: %v", err)
		}
		if len(plugins) != 0 {
			t.Errorf("got %+v, want none", plugins)
		}
	})

	t.Run("a malformed install registry fails the whole read", func(t *testing.T) {
		f := newFixture(t)
		f.pluginAgent("market", "plug", "1.0.0", "a")
		mustWrite(t, filepath.Join(f.base, installedPluginsFileName), `{"plugins": [`)

		if _, err := readPlugins(f.base); err == nil {
			t.Fatal("readPlugins succeeded with an unparseable install registry")
		}
	})

	t.Run("a symlinked plugin directory is followed", func(t *testing.T) {
		f := newFixture(t)
		// Deliberately the opposite of readGlobal's rule. There a symlink is
		// the subject of the report; here a symlinked plugin directory is just
		// a plugin directory, and DirEntry.IsDir would report false for it and
		// silently drop a real install.
		real := filepath.Join(f.base, "elsewhere", "plug-1.0.0")
		mustWrite(t, filepath.Join(real, "agents", "a.md"), "plugin agent\n")
		linked := filepath.Join(f.base, pluginsDirName, cacheDirName,
			"market", "plug", "1.0.0")
		mustMkdir(t, filepath.Dir(linked))
		if err := os.Symlink(real, linked); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		f.install("plug@market/1.0.0")

		plugins, err := readPlugins(f.base)
		if err != nil {
			t.Fatalf("readPlugins: %v", err)
		}
		if len(plugins) != 1 {
			t.Fatalf("a symlinked plugin directory was skipped: %+v", plugins)
		}
	})

	t.Run("a missing cache is empty and not an error", func(t *testing.T) {
		f := newFixture(t)

		plugins, err := readPlugins(f.base)
		if err != nil {
			t.Fatalf("readPlugins: %v", err)
		}
		if len(plugins) != 0 {
			t.Errorf("got %+v, want none", plugins)
		}
		if plugins == nil {
			t.Error("the slice is nil, so the document would carry a JSON null")
		}
	})

	t.Run("a plugin shipping no agents is omitted", func(t *testing.T) {
		f := newFixture(t)
		mustMkdir(t, filepath.Join(f.base, pluginsDirName, cacheDirName,
			"market", "toolsonly", "1.0.0", "commands"))
		f.pluginAgent("market", "hasagents", "1.0.0", "one")
		f.install("toolsonly@market/1.0.0", "hasagents@market/1.0.0")

		plugins, err := readPlugins(f.base)
		if err != nil {
			t.Fatalf("readPlugins: %v", err)
		}
		// Nothing this file is about, and listing every such plugin would bury
		// the handful that actually cost something.
		if len(plugins) != 1 || plugins[0].Name != "hasagents" {
			t.Errorf("got %+v, want only hasagents", plugins)
		}
	})

	t.Run("enabled comes from settings, keyed plugin@marketplace", func(t *testing.T) {
		f := newFixture(t)
		f.pluginAgent("market", "on", "1.0.0", "a")
		f.pluginAgent("market", "off", "1.0.0", "b")
		f.pluginAgent("market", "unmentioned", "1.0.0", "c")
		f.install("on@market/1.0.0", "off@market/1.0.0", "unmentioned@market/1.0.0")
		// Deliberately the wrong way round, to catch a key built as
		// marketplace@plugin: it would match nothing and disable everything.
		f.settings(`{"enabledPlugins": {"on@market": true, "off@market": false}}`)

		plugins, err := readPlugins(f.base)
		if err != nil {
			t.Fatalf("readPlugins: %v", err)
		}
		want := map[string]bool{"on": true, "off": false, "unmentioned": false}
		for _, p := range plugins {
			if p.Enabled != want[p.Name] {
				t.Errorf("%s enabled = %v, want %v", p.Name, p.Enabled, want[p.Name])
			}
		}
	})

	t.Run("an unknown home yields nothing rather than reading the real one", func(t *testing.T) {
		plugins, err := readPlugins("")
		if err != nil {
			t.Fatalf("readPlugins: %v", err)
		}
		if len(plugins) != 0 {
			t.Errorf("got %+v, want none", plugins)
		}
		enabled, err := enabledPlugins("")
		if err != nil {
			t.Fatalf("enabledPlugins: %v", err)
		}
		if len(enabled) != 0 {
			t.Errorf("got %v, want none", enabled)
		}
	})

	t.Run("malformed settings fail the whole read", func(t *testing.T) {
		f := newFixture(t)
		f.pluginAgent("market", "plug", "1.0.0", "a")
		f.install("plug@market/1.0.0")
		f.settings(`not json`)

		if _, err := readPlugins(f.base); err == nil {
			t.Fatal("readPlugins succeeded with unparseable settings")
		}
	})
}

// --- the summary ------------------------------------------------------------

// Installed and enabled are separate numbers and must never collapse into one.
// Quoting the installed figure as the cost of a session is the exact error this
// package exists to prevent.
func TestSummarize_KeepsInstalledAndEnabledPluginAgentsApart(t *testing.T) {
	f := newFixture(t)
	f.pluginAgent("market", "on", "1.0.0", "a")
	f.pluginAgent("market", "on", "1.0.0", "b")
	f.pluginAgent("market", "off", "1.0.0", "c")
	f.pluginAgent("market", "off", "1.0.0", "d")
	f.pluginAgent("market", "off", "1.0.0", "e")
	f.install("on@market/1.0.0", "off@market/1.0.0")
	f.settings(`{"enabledPlugins": {"on@market": true, "off@market": false}}`)
	f.globalFile("global-one.md", "x\n")

	s := f.build().Summary
	if s.PluginsShippingAgents != 2 || s.PluginsWithAgentsEnabled != 1 {
		t.Errorf("plugins installed/enabled = %d/%d, want 2/1",
			s.PluginsShippingAgents, s.PluginsWithAgentsEnabled)
	}
	if s.PluginAgentsInstalled != 5 || s.PluginAgentsEnabled != 2 {
		t.Errorf("plugin agents installed/enabled = %d/%d, want 5/2",
			s.PluginAgentsInstalled, s.PluginAgentsEnabled)
	}
	// What every session pays: the global tier plus enabled plugin agents.
	// Project agents load only in their own project and are excluded.
	if s.AgentsLoadedGlobally != 3 {
		t.Errorf("agents loaded globally = %d, want 3 (1 global + 2 enabled plugin)",
			s.AgentsLoadedGlobally)
	}
}

// The cache keeps superseded version directories beside the current one, and
// only one version of a plugin is ever loaded. Counting records rather than
// plugins would report an enabled plugin's agents twice — the same over-count
// that walking marketplaces/ produced, in a shape small enough to go unnoticed.
func TestSummarize_ASecondCachedVersionDoesNotInflateWhatLoads(t *testing.T) {
	f := newFixture(t)
	f.pluginAgent("market", "plug", "1.0.0", "a")
	f.pluginAgent("market", "plug", "1.0.0", "b")
	f.pluginAgent("market", "plug", "2.0.0", "a")
	f.pluginAgent("market", "plug", "2.0.0", "b")
	f.install("plug@market/1.0.0", "plug@market/2.0.0")
	f.settings(`{"enabledPlugins": {"plug@market": true}}`)

	inv := f.build()

	// Both version directories are still listed: they are both on disk, and
	// the document reports what is there.
	if len(inv.Plugins) != 2 {
		t.Fatalf("got %d plugin records, want both cached versions listed", len(inv.Plugins))
	}
	s := inv.Summary
	if s.PluginsShippingAgents != 1 || s.PluginsWithAgentsEnabled != 1 {
		t.Errorf("plugins installed/enabled = %d/%d, want 1/1: two cached versions are one plugin",
			s.PluginsShippingAgents, s.PluginsWithAgentsEnabled)
	}
	if s.PluginAgentsInstalled != 2 || s.PluginAgentsEnabled != 2 {
		t.Errorf("plugin agents installed/enabled = %d/%d, want 2/2: a superseded "+
			"version's agents were counted a second time",
			s.PluginAgentsInstalled, s.PluginAgentsEnabled)
	}
}

// The differing-set case, which the identical-set test above cannot see.
//
// Two installs of one plugin shipping overlapping-but-different agent sets is
// the only case where the count is not exact: {a,b} and {b,c} union to 3 while
// exactly one install loads. PluginAgentsEnabled's doc says so rather than
// claiming an exactness the union cannot deliver, and this pins the arithmetic
// so the doc and the code cannot drift apart.
//
// Picking one install's list instead would be a guess dressed up as a
// measurement. An upper bound that announces itself beats a point estimate that
// does not.
func TestSummarize_TwoInstallsWithDifferentAgentSetsGiveAnUpperBound(t *testing.T) {
	f := newFixture(t)
	f.pluginAgent("market", "plug", "1.0.0", "a")
	f.pluginAgent("market", "plug", "1.0.0", "b")
	f.pluginAgent("market", "plug", "2.0.0", "b")
	f.pluginAgent("market", "plug", "2.0.0", "c")
	f.install("plug@market/1.0.0", "plug@market/2.0.0")
	f.settings(`{"enabledPlugins": {"plug@market": true}}`)

	s := f.build().Summary
	// Union of {a,b} and {b,c}: the shared "b" is counted once, not twice.
	if s.PluginAgentsEnabled != 3 {
		t.Errorf("plugin_agents_enabled = %d, want 3 (union of {a,b} and {b,c})",
			s.PluginAgentsEnabled)
	}
	if s.PluginsShippingAgents != 1 {
		t.Errorf("plugins_shipping_agents = %d, want 1: two installs are one plugin",
			s.PluginsShippingAgents)
	}
}

// Two marketplaces shipping a plugin of the same name are two plugins, because
// they are separately installable and separately enabled. This is the case the
// de-duplication above must not swallow, which is why the key carries the
// marketplace.
func TestSummarize_SameNameFromTwoMarketplacesCountsTwice(t *testing.T) {
	f := newFixture(t)
	f.pluginAgent("alpha", "plug", "1.0.0", "a")
	f.pluginAgent("beta", "plug", "1.0.0", "a")
	f.install("plug@alpha/1.0.0", "plug@beta/1.0.0")
	f.settings(`{"enabledPlugins": {"plug@alpha": true, "plug@beta": true}}`)

	s := f.build().Summary
	if s.PluginsWithAgentsEnabled != 2 || s.PluginAgentsEnabled != 2 {
		t.Errorf("plugins/agents enabled = %d/%d, want 2/2",
			s.PluginsWithAgentsEnabled, s.PluginAgentsEnabled)
	}
}

func TestSummarize_CountsProjectAgentsByStateAndTalliesAtRisk(t *testing.T) {
	f := newFixture(t)
	f.repoAgent("shared", "canonical\n")
	f.projectAgent("Web/one", "shared", "canonical\n") // adoptable: identical
	f.projectAgent("Web/one", "shared-custom", "different\n")
	f.projectAgent("Web/two", "solo", "only here\n") // unadopted: at risk
	f.globalFile("g.md", "x\n")

	inv := f.build()
	s := inv.Summary
	if s.GlobalAgents != 1 {
		t.Errorf("global agents = %d, want 1", s.GlobalAgents)
	}
	if s.Projects != 2 || s.ProjectAgents != 3 {
		t.Errorf("projects/agents = %d/%d, want 2/3", s.Projects, s.ProjectAgents)
	}
	if s.ByState["adoptable"] != 1 {
		t.Errorf("by_state[adoptable] = %d, want 1: %v", s.ByState["adoptable"], s.ByState)
	}
	if s.ByState["unadopted"] != 2 {
		t.Errorf("by_state[unadopted] = %d, want 2: %v", s.ByState["unadopted"], s.ByState)
	}
	// ByState tallies project agents only — the global tier is classified
	// against a different question and has no adoptable/unadopted answer.
	if got := s.ByState["file"]; got != 0 {
		t.Errorf("by_state carries a global-tier state (file: %d); the two "+
			"vocabularies must not be mixed into one map", got)
	}
	if s.AtRisk != 2 {
		t.Errorf("at risk = %d, want 2", s.AtRisk)
	}
}

// --- portable paths ---------------------------------------------------------

// renderPath and within are pure, so these use synthetic paths rather than a
// fixture: the filesystem has nothing to say about either.
func TestRenderPath(t *testing.T) {
	repo := &scan.Repo{Root: "/home/u/Web/agents-repo", Home: "/home/u"}

	cases := []struct {
		name string
		path string
		want string
	}{
		{
			// The repo form is preferred where a path is both, because it
			// survives the whole checkout moving and the home form does not.
			name: "inside the repo renders repo-relative, not home-relative",
			path: "/home/u/Web/agents-repo/agents/x.md",
			want: "agents/x.md",
		},
		{
			name: "inside home but outside the repo renders home-relative",
			path: "/home/u/elsewhere/x.md",
			want: "~/elsewhere/x.md",
		},
		{
			// Absolute is the last resort, and is plainly identifiable as
			// such — a leading slash, where every key form has none or a "~/".
			name: "outside both stays absolute",
			path: "/opt/shared/x.md",
			want: "/opt/shared/x.md",
		},
		{
			// The string-prefix trap: "/home/u/Web/agents-repo-old" has the
			// repo root as a string prefix. Rendering it repo-relative would
			// produce "../agents-repo-old/x.md", a key naming a directory
			// that does not contain it.
			name: "a sibling whose name extends the repo root is not inside it",
			path: "/home/u/Web/agents-repo-old/x.md",
			want: "~/Web/agents-repo-old/x.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderPath(repo, tc.path); got != tc.want {
				t.Errorf("renderPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestRenderPath_WithNoHomeFallsBackToAbsolute(t *testing.T) {
	// An unknown home is not an error anywhere else in this tool, and it must
	// not become one here: an absolute path is still correct, just not
	// portable.
	repo := &scan.Repo{Root: "", Home: ""}
	if got := renderPath(repo, "/anywhere/x.md"); got != "/anywhere/x.md" {
		t.Errorf("got %q, want the path unchanged", got)
	}
}

func TestWithin(t *testing.T) {
	cases := []struct {
		dir, path string
		want      bool
	}{
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b", true},
		// The bug this function exists to avoid: /a/bc has /a/b as a string
		// prefix and is not inside it.
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a/bc/d", false},
		{"/a/b", "/a", false},
		{"/a/b", "/x/y", false},
		// An empty dir is never a container; it is what an unknown home looks
		// like, and treating "" as a prefix would make everything relative to
		// nothing.
		{"", "/a/b", false},
	}
	for _, tc := range cases {
		if got := within(tc.dir, tc.path); got != tc.want {
			t.Errorf("within(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
		}
	}
}

// --- Build ------------------------------------------------------------------

// A nil repo is a programming error, not a machine state, and must not be
// papered over: every path in the document is rendered relative to the repo, so
// without one there is nothing to render against.
func TestBuild_NilRepoIsAnError(t *testing.T) {
	if _, err := Build(nil, scan.Result{}); err == nil {
		t.Fatal("Build(nil) succeeded")
	}
}

// Every slice is non-nil even on an empty machine, so the document carries []
// rather than null and a consumer never has to tell "none" from "missing".
func TestBuild_EmptyMachineStillEmitsArrays(t *testing.T) {
	f := newFixture(t)
	out, err := f.build().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "null") {
		t.Errorf("the document carries a JSON null:\n%s", out)
	}
	if !strings.Contains(string(out), `"schema": 1`) {
		t.Errorf("the schema header is missing:\n%s", out)
	}
}

// The top level is exactly these five keys, asserted as a closed set rather
// than by looking for the fields we do not want.
//
// A generated-at timestamp or tool version would change the output even when
// the inventory content is unchanged, undermining --check and creating noise
// in a committed inventory file.
// Their absence is a contract, so adding a sixth key has to fail here and be
// argued for, not slip in.
func TestMarshal_TopLevelIsSchemaAndContentOnly(t *testing.T) {
	f := newFixture(t)
	out, err := f.build().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	want := map[string]bool{
		"schema": true, "global": true, "projects": true,
		"plugins": true, "summary": true,
	}
	for key := range doc {
		if !want[key] {
			t.Errorf("unexpected top-level key %q; nothing volatile may be in the document", key)
		}
	}
	for key := range want {
		if _, ok := doc[key]; !ok {
			t.Errorf("missing top-level key %q", key)
		}
	}
}

// --- the quiet failure ------------------------------------------------------

// Every unknown in this package resolves downward, so nothing absent can
// inflate what the document claims loads. A missing install registry is the one
// that resolves downward *invisibly*: it zeroes plugins[] and both counters at
// once, leaving a document indistinguishable from a machine with no plugins.
// RegistryGap is what lets the command say so out loud.
func TestRegistryGap(t *testing.T) {
	t.Run("absent registry over a non-empty cache is reported", func(t *testing.T) {
		f := newFixture(t)
		f.pluginAgent("market", "plug", "1.0.0", "a")

		gap := RegistryGap(f.base)
		if gap == "" {
			t.Fatal("no warning for an absent registry over a populated cache")
		}
		// The sentence has to name both paths and say what it costs, since it
		// is the only signal a reader gets — the document itself looks fine.
		if !strings.Contains(gap, installedPluginsFileName) {
			t.Errorf("the warning does not name the missing registry: %q", gap)
		}
		if !strings.Contains(gap, cacheDirName) {
			t.Errorf("the warning does not name the cache: %q", gap)
		}
	})

	t.Run("absent registry over an absent cache is not a gap", func(t *testing.T) {
		f := newFixture(t)

		// A machine with no plugins at all. Nothing is being under-reported,
		// so warning here would be noise on every clean machine.
		if gap := RegistryGap(f.base); gap != "" {
			t.Errorf("warned with no plugin tree at all: %q", gap)
		}
	})

	t.Run("a present registry is never a gap", func(t *testing.T) {
		f := newFixture(t)
		f.pluginAgent("market", "plug", "1.0.0", "a")
		f.install("plug@market/1.0.0")

		if gap := RegistryGap(f.base); gap != "" {
			t.Errorf("warned with a registry present: %q", gap)
		}
	})

	t.Run("an unreadable registry is left to readPlugins", func(t *testing.T) {
		f := newFixture(t)
		f.pluginAgent("market", "plug", "1.0.0", "a")
		// A directory where the file should be: present, unreadable, and not
		// ErrNotExist. readPlugins fails hard on it, which is the louder
		// signal, so this must not also warn.
		mustMkdir(t, filepath.Join(f.base, installedPluginsFileName))

		if gap := RegistryGap(f.base); gap != "" {
			t.Errorf("warned about a registry that exists but cannot be read: %q", gap)
		}
		if _, err := readPlugins(f.base); err == nil {
			t.Error("readPlugins did not fail on an unreadable registry")
		}
	})

	t.Run("an unknown home is not a gap", func(t *testing.T) {
		if gap := RegistryGap(""); gap != "" {
			t.Errorf("warned with no home: %q", gap)
		}
	})
}
