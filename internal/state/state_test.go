package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentic-tooling/agentsync/internal/scan"
)

// --- fixtures ---------------------------------------------------------------

// fixture is a throwaway checkout: a repo root with nothing in it but whatever
// the test puts there.
type fixture struct {
	t    *testing.T
	repo *scan.Repo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	root := filepath.Join(t.TempDir(), "agents-repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("creating %s: %v", root, err)
	}
	repo, err := scan.NewRepo(root)
	if err != nil {
		t.Fatalf("NewRepo: %v", err)
	}
	return &fixture{t: t, repo: repo}
}

// write puts a document on disk exactly as given, so a test can hand Load
// something no version of this package would ever have produced.
func (f *fixture) write(content string) {
	f.t.Helper()
	if err := os.WriteFile(Path(f.repo), []byte(content), 0o600); err != nil {
		f.t.Fatalf("writing the state file: %v", err)
	}
}

// link is a plausible record for a project agent, with everything valid.
func (f *fixture) link(project, name string) Link {
	return Link{
		Key:    Key{Project: project, Name: name},
		Path:   filepath.Join(f.repo.Root, "..", project, ".claude", "agents", name+".md"),
		Target: filepath.Join(f.repo.Root, "project-agents", project, name+".md"),
		Hash:   "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
}

var testTime = time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC)

// --- loading ----------------------------------------------------------------

// TestLoad_MissingFileIsAnEmptyDocument: the one absence this package treats
// as an answer rather than a failure.
func TestLoad_MissingFileIsAnEmptyDocument(t *testing.T) {
	f := newFixture(t)

	got, err := Load(f.repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(got.Records()); n != 0 {
		t.Errorf("records = %d, want 0", n)
	}
	if n := len(got.AllDeclined()); n != 0 {
		t.Errorf("declined = %d, want 0", n)
	}
	if got.Dirty() {
		t.Error("a freshly loaded document is dirty; a run that changes nothing would rewrite the file")
	}
}

// TestLoad_UnreadableFileIsAnErrorNeverAnEmptyDocument is the failure direction
// the package turns on: reaching the empty document through a read error would
// silently recreate every link a user deliberately deleted.
//
// A directory stands where the file belongs, not chmod 000: permission bits are
// inert under the root CI runs as, so such a test would skip and cover nothing.
func TestLoad_UnreadableFileIsAnErrorNeverAnEmptyDocument(t *testing.T) {
	f := newFixture(t)
	if err := os.Mkdir(Path(f.repo), 0o755); err != nil {
		t.Fatalf("planting an unreadable state file: %v", err)
	}

	got, err := Load(f.repo)
	if err == nil {
		t.Fatalf("Load returned %+v and no error; an unreadable document must never load as an empty one", got)
	}
	if got != nil {
		t.Errorf("Load returned a document alongside its error (%+v); a caller ignoring the error would act on it", got)
	}
	if !strings.Contains(err.Error(), FileName) {
		t.Errorf("the error does not name the file: %v", err)
	}
}

func TestLoad_NilRepo(t *testing.T) {
	if _, err := Load(nil); err == nil {
		t.Error("Load(nil) returned no error")
	}
}

// TestLoad_RefusesDocumentsItCannotVouchFor: each refuses rather than loading
// the part it could read. A record silently dropped from an authoritative
// document is a link nothing can ever prune.
func TestLoad_RefusesDocumentsItCannotVouchFor(t *testing.T) {
	const rec = `{"project":"Web/example","name":"planner","path":"/p/.claude/agents/planner.md",` +
		`"target":"/repo/agents/planner.md","hash":"abc","origin":"created","recorded_at":"2026-08-24T10:30:00Z"}`

	tests := []struct {
		name     string
		document string
		wantErr  string
	}{{
		name:     "not json at all",
		document: "this is not a document\n",
		wantErr:  "parsing",
	}, {
		name:     "no schema",
		document: `{"links":[],"declined":[]}`,
		wantErr:  "schema 0",
	}, {
		// Carries a field this build has never heard of, because that is what
		// a document from a newer agentsync actually looks like. Without it
		// the row passes whether or not the schema is read before the strict
		// decode, and the "upgrade rather than letting an older build rewrite
		// it" message it exists to pin stays unreachable.
		name:     "a newer schema",
		document: `{"schema":2,"links":[],"declined":[],"future_field":{"added":"later"}}`,
		wantErr:  "newer agentsync",
	}, {
		name:     "a field this build does not know",
		document: `{"schema":1,"links":[],"declined":[],"pruned":["x"]}`,
		wantErr:  "pruned",
	}, {
		name:     "two records for one link",
		document: `{"schema":1,"links":[` + rec + `,` + rec + `],"declined":[]}`,
		wantErr:  "two records",
	}, {
		name: "an origin this build does not know",
		document: `{"schema":1,"links":[{"project":"a","name":"b","path":"/p/b.md","target":"/t/b.md",` +
			`"hash":"abc","origin":"inferred","recorded_at":"2026-08-24T10:30:00Z"}],"declined":[]}`,
		wantErr: "unknown origin",
	}, {
		name: "no origin at all",
		document: `{"schema":1,"links":[{"project":"a","name":"b","path":"/p/b.md","target":"/t/b.md",` +
			`"hash":"abc","recorded_at":"2026-08-24T10:30:00Z"}],"declined":[]}`,
		wantErr: "no usable origin",
	}, {
		name: "a relative link path",
		document: `{"schema":1,"links":[{"project":"a","name":"b","path":"b.md","target":"/t/b.md",` +
			`"hash":"abc","origin":"created","recorded_at":"2026-08-24T10:30:00Z"}],"declined":[]}`,
		wantErr: "relative link path",
	}, {
		name: "no hash",
		document: `{"schema":1,"links":[{"project":"a","name":"b","path":"/p/b.md","target":"/t/b.md",` +
			`"hash":"","origin":"created","recorded_at":"2026-08-24T10:30:00Z"}],"declined":[]}`,
		wantErr: "no hash",
	}, {
		name: "a timestamp that is not RFC3339",
		document: `{"schema":1,"links":[{"project":"a","name":"b","path":"/p/b.md","target":"/t/b.md",` +
			`"hash":"abc","origin":"created","recorded_at":"last tuesday"}],"declined":[]}`,
		wantErr: "not RFC3339",
	}, {
		name: "a declined marker with no observation",
		document: `{"schema":1,"links":[],"declined":[{"project":"a","name":"b",` +
			`"declined_at":"2026-08-24T10:30:00Z","observation":""}]}`,
		wantErr: "no observation",
	}, {
		name: "two declined markers for one agent",
		document: `{"schema":1,"links":[],"declined":[` +
			`{"project":"a","name":"b","declined_at":"2026-08-24T10:30:00Z","observation":"gone"},` +
			`{"project":"a","name":"b","declined_at":"2026-08-24T10:30:00Z","observation":"gone"}]}`,
		wantErr: "two declined markers",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.write(tc.document)

			got, err := Load(f.repo)
			if err == nil {
				t.Fatalf("Load accepted the document and returned %+v", got)
			}
			if got != nil {
				t.Errorf("Load returned a document alongside its error: %+v", got)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// --- writing and reading back -----------------------------------------------

// TestSaveLoad_RoundTrip: every field survives, origin included — the one field
// a later prune is gated on.
func TestSaveLoad_RoundTrip(t *testing.T) {
	f := newFixture(t)
	doc := New()

	made, err := doc.RecordCreated(f.link("Web/example", "planner"), testTime)
	if err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}
	if _, err := doc.RecordAdopted(f.link("Web/api", "reviewer"), testTime); err != nil {
		t.Fatalf("RecordAdopted: %v", err)
	}
	if err := doc.Decline(Key{Project: "Web/example", Name: "scribe"}, "link gone, nothing in its place", testTime); err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if err := doc.Save(f.repo); err != nil {
		t.Fatalf("Save: %v", err)
	}

	back, err := Load(f.repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, ok := back.LookupLink(made.Path)
	if !ok {
		t.Fatalf("the record for %s did not survive the round trip", made.Path)
	}
	if got != made {
		t.Errorf("record = %+v, want %+v", got, made)
	}
	if got := back.Origin(made.Path); got != OriginCreated {
		t.Errorf("origin = %v, want created", got)
	}
	if got := back.Hash(made.Path); got != made.Hash {
		t.Errorf("hash = %q, want %q", got, made.Hash)
	}

	adopted := back.Records()[0]
	for _, r := range back.Records() {
		if r.Name == "reviewer" {
			adopted = r
		}
	}
	if adopted.Origin != OriginAdopted {
		t.Errorf("the adopted record came back as %v; the two origins authorise different things and must never merge", adopted.Origin)
	}

	if !back.IsDeclined(Key{Project: "Web/example", Name: "scribe"}) {
		t.Error("the declined marker did not survive the round trip")
	}
	d, _ := back.LookupDeclined(Key{Project: "Web/example", Name: "scribe"})
	if d.Observation != "link gone, nothing in its place" {
		t.Errorf("observation = %q; a marker nobody can argue with is one nobody can safely clear", d.Observation)
	}
	if d.DeclinedAt != testTime.Format(time.RFC3339) {
		t.Errorf("declined_at = %q, want %q", d.DeclinedAt, testTime.Format(time.RFC3339))
	}
}

// TestSave_IsOwnerOnlyAndLeavesNoTemporary: the mode is chosen, not inherited
// from CreateTemp, and a completed save leaves nothing behind.
func TestSave_IsOwnerOnlyAndLeavesNoTemporary(t *testing.T) {
	f := newFixture(t)
	doc := New()
	if _, err := doc.RecordCreated(f.link("Web/example", "planner"), testTime); err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}
	if err := doc.Save(f.repo); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Lstat(Path(f.repo))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %v, want 0600: this document is machine-private and authoritative, unlike the committed inventory", got)
	}

	ents, err := os.ReadDir(f.repo.Root)
	if err != nil {
		t.Fatalf("reading the repo root: %v", err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary survived the save: %s", e.Name())
		}
	}
}

// TestSave_ReplacesTheWholeDocument: a second save is not an append, so a
// removed record leaves the file and not merely memory.
func TestSave_ReplacesTheWholeDocument(t *testing.T) {
	f := newFixture(t)
	doc := New()
	rec, err := doc.RecordCreated(f.link("Web/example", "planner"), testTime)
	if err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}
	if err := doc.Save(f.repo); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if !doc.Remove(rec.Path) {
		t.Fatal("Remove reported no record to remove")
	}
	if err := doc.Save(f.repo); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	back, err := Load(f.repo)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(back.Records()); n != 0 {
		t.Errorf("records = %d after a save that removed the only one, want 0", n)
	}
}

// TestMarshal_IsOrdered: byte-identity is not a contract, but two runs holding
// the same records must still diff readably. The two documents are built in
// opposite orders, since map iteration is randomized.
func TestMarshal_IsOrdered(t *testing.T) {
	f := newFixture(t)
	names := []string{"zeta", "alpha", "middle"}

	first := New()
	for _, n := range names {
		if _, err := first.RecordCreated(f.link("Web/example", n), testTime); err != nil {
			t.Fatalf("RecordCreated: %v", err)
		}
		if err := first.Decline(Key{Project: "Web/example", Name: n}, "gone", testTime); err != nil {
			t.Fatalf("Decline: %v", err)
		}
	}
	second := New()
	for i := len(names) - 1; i >= 0; i-- {
		if _, err := second.RecordCreated(f.link("Web/example", names[i]), testTime); err != nil {
			t.Fatalf("RecordCreated: %v", err)
		}
		if err := second.Decline(Key{Project: "Web/example", Name: names[i]}, "gone", testTime); err != nil {
			t.Fatalf("Decline: %v", err)
		}
	}

	a, err := first.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	b, err := second.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("the same records rendered differently depending on insertion order:\n%s\n---\n%s", a, b)
	}
	if !strings.HasSuffix(string(a), "}\n") {
		t.Errorf("the document does not end in exactly one newline:\n%q", string(a)[len(a)-8:])
	}
}

// TestMarshal_EmptyDocumentCarriesArraysNotNulls: a consumer should not have to
// tell "no records" from "the field was missing".
func TestMarshal_EmptyDocumentCarriesArraysNotNulls(t *testing.T) {
	b, err := New().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshalling what we just rendered: %v", err)
	}
	for _, field := range []string{"links", "declined"} {
		if got := string(raw[field]); got != "[]" {
			t.Errorf("%s = %s, want []", field, got)
		}
	}
	if got := string(raw["schema"]); got != "1" {
		t.Errorf("schema = %s, want 1", got)
	}
}

// --- origins ----------------------------------------------------------------

// TestOrigin_ZeroValueIsTheRefusingArm: here it decides whether something may
// be deleted.
func TestOrigin_ZeroValueIsTheRefusingArm(t *testing.T) {
	var zero Origin
	if zero != OriginUnknown {
		t.Errorf("the zero value is %v, want OriginUnknown", zero)
	}
	if zero.Valid() {
		t.Error("OriginUnknown reports itself valid; a record with no origin would authorise a removal")
	}
	if _, err := zero.MarshalJSON(); err == nil {
		t.Error("OriginUnknown marshalled; the document would then refuse to load")
	}
	if got := New().Origin("/nowhere/agents/nothing.md"); got != OriginUnknown {
		t.Errorf("origin of an unrecorded link = %v, want OriginUnknown", got)
	}
}

func TestOrigin_MarshalsAsTextNotAsAnInteger(t *testing.T) {
	b, err := json.Marshal(OriginCreated)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `"created"` {
		t.Errorf("origin marshalled as %s; an integer would silently re-map every record on disk if an arm were ever inserted", b)
	}
}

// --- records ----------------------------------------------------------------

// TestRecord_RefusesWhatItCannotVouchFor: the loader.s validation applied on
// the way in, so a bad record never reaches the file.
func TestRecord_RefusesWhatItCannotVouchFor(t *testing.T) {
	f := newFixture(t)
	good := f.link("Web/example", "planner")

	tests := []struct {
		name    string
		link    Link
		at      time.Time
		wantErr string
	}{{
		name:    "no project",
		link:    Link{Key: Key{Name: "planner"}, Path: good.Path, Target: good.Target, Hash: good.Hash},
		at:      testTime,
		wantErr: "no project",
	}, {
		name:    "no agent name",
		link:    Link{Key: Key{Project: "Web/example"}, Path: good.Path, Target: good.Target, Hash: good.Hash},
		at:      testTime,
		wantErr: "no project",
	}, {
		name:    "no hash",
		link:    Link{Key: good.Key, Path: good.Path, Target: good.Target},
		at:      testTime,
		wantErr: "no hash",
	}, {
		name:    "no timestamp",
		link:    good,
		at:      time.Time{},
		wantErr: "no timestamp",
	}, {
		name:    "empty path",
		link:    Link{Key: good.Key, Target: good.Target, Hash: good.Hash},
		at:      testTime,
		wantErr: "empty path",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := New()
			if _, err := doc.RecordCreated(tc.link, tc.at); err == nil {
				t.Fatal("RecordCreated accepted it")
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if doc.Dirty() {
				t.Error("a refused record left the document dirty; the next save would write something nobody recorded")
			}
		})
	}
}

// TestRecordCreated_ARelativePathIsResolvedAgainstTheProcess: a relative --root
// produces relative item paths, and a record must still be keyed by something
// unique on disk.
func TestRecordCreated_ARelativePathIsResolvedAgainstTheProcess(t *testing.T) {
	f := newFixture(t)
	l := f.link("Web/example", "planner")
	l.Path = filepath.Join("relative", "agents", "planner.md")

	rec, err := f.recordable(l)
	if err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}
	if !filepath.IsAbs(rec.Path) {
		t.Errorf("the stored path %q is still relative", rec.Path)
	}
}

// recordable records a link into a throwaway document and returns the record.
func (f *fixture) recordable(l Link) (Record, error) {
	f.t.Helper()
	return New().RecordCreated(l, testTime)
}

// TestLookupLink_FindsARecordThroughAnotherSpellingOfTheSamePath: every entry
// point cleans its argument, or a prune would refuse to touch its own work.
func TestLookupLink_FindsARecordThroughAnotherSpellingOfTheSamePath(t *testing.T) {
	f := newFixture(t)
	doc := New()
	rec, err := doc.RecordCreated(f.link("Web/example", "planner"), testTime)
	if err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}

	// Built by concatenation, not filepath.Join, which would clean it here and
	// leave the test asserting nothing.
	messy := filepath.Dir(rec.Path) + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(rec.Path)
	if messy == rec.Path {
		t.Fatalf("the fixture path %q is not actually spelled differently", messy)
	}
	if _, ok := doc.LookupLink(messy); !ok {
		t.Errorf("%q did not find the record stored for %q", messy, rec.Path)
	}
}

// TestRecordCreated_TheSameLinkTwiceKeepsTheNewer: re-linking must leave the
// record describing the link that is there now, hash included.
func TestRecordCreated_TheSameLinkTwiceKeepsTheNewer(t *testing.T) {
	f := newFixture(t)
	doc := New()
	l := f.link("Web/example", "planner")
	if _, err := doc.RecordCreated(l, testTime); err != nil {
		t.Fatalf("first RecordCreated: %v", err)
	}

	l.Hash = "0000000000000000000000000000000000000000000000000000000000000000"
	later := testTime.Add(time.Hour)
	if _, err := doc.RecordCreated(l, later); err != nil {
		t.Fatalf("second RecordCreated: %v", err)
	}

	if n := len(doc.Records()); n != 1 {
		t.Fatalf("records = %d, want 1", n)
	}
	got := doc.Records()[0]
	if got.Hash != l.Hash {
		t.Errorf("hash = %q, want the newer %q", got.Hash, l.Hash)
	}
	if got.RecordedAt != later.Format(time.RFC3339) {
		t.Errorf("recorded_at = %q, want the newer %q", got.RecordedAt, later.Format(time.RFC3339))
	}
}

func TestRemove_ReportsWhetherThereWasAnythingToRemove(t *testing.T) {
	f := newFixture(t)
	doc := New()
	rec, err := doc.RecordCreated(f.link("Web/example", "planner"), testTime)
	if err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}
	if !doc.Remove(rec.Path) {
		t.Error("Remove returned false for a record that was there")
	}
	if doc.Remove(rec.Path) {
		t.Error("Remove returned true for a record that was already gone")
	}
	if _, ok := doc.LookupLink(rec.Path); ok {
		t.Error("the record survived Remove")
	}
}

// --- declined markers -------------------------------------------------------

func TestDecline_AndUndecline(t *testing.T) {
	k := Key{Project: "Web/example", Name: "planner"}
	doc := New()

	if doc.IsDeclined(k) {
		t.Error("an empty document declines something")
	}
	if doc.Undecline(k) {
		t.Error("Undecline returned true with no marker to clear")
	}
	if err := doc.Decline(k, "link gone, nothing in its place", testTime); err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if !doc.IsDeclined(k) {
		t.Error("the marker was not recorded")
	}
	if !doc.Undecline(k) {
		t.Error("Undecline returned false for a marker that was there")
	}
	if doc.IsDeclined(k) {
		t.Error("the marker survived Undecline")
	}
	// The second identical run: a command that fails the second time is a
	// command people stop trusting.
	if doc.Undecline(k) {
		t.Error("a second Undecline reported a clear that did not happen")
	}
}

func TestDecline_RefusesAMarkerNobodyCouldArgueWith(t *testing.T) {
	doc := New()
	k := Key{Project: "Web/example", Name: "planner"}

	if err := doc.Decline(k, "", testTime); err == nil {
		t.Error("Decline accepted a marker with no observation")
	}
	if err := doc.Decline(k, "gone", time.Time{}); err == nil {
		t.Error("Decline accepted a marker with no timestamp")
	}
	if err := doc.Decline(Key{Project: "Web/example"}, "gone", testTime); err == nil {
		t.Error("Decline accepted a marker naming no agent")
	}
	if doc.Dirty() {
		t.Error("a refused marker left the document dirty")
	}
}

// --- dirty tracking ---------------------------------------------------------

// TestDirty_TracksWhetherThereIsAnythingToSave, or every link --apply would
// rewrite the file — conjuring one for a repo that has recorded nothing.
func TestDirty_TracksWhetherThereIsAnythingToSave(t *testing.T) {
	f := newFixture(t)
	doc := New()
	if doc.Dirty() {
		t.Error("a new document is dirty")
	}

	if _, err := doc.RecordCreated(f.link("Web/example", "planner"), testTime); err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}
	if !doc.Dirty() {
		t.Error("recording a link left the document clean; the record would never be saved")
	}

	if err := doc.Save(f.repo); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if doc.Dirty() {
		t.Error("the document is still dirty after a successful save")
	}
}

// --- key parsing ------------------------------------------------------------

func TestParseKey(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Key
		wantErr bool
	}{
		{name: "a nested project key", in: "Web/monorepo/fixture/planner",
			want: Key{Project: "Web/monorepo/fixture", Name: "planner"}},
		{name: "the .md the file on disk carries", in: "Web/example/planner.md",
			want: Key{Project: "Web/example", Name: "planner"}},
		{name: "surrounding space", in: "  Web/example/planner  ",
			want: Key{Project: "Web/example", Name: "planner"}},
		{name: "empty", in: "", wantErr: true},
		{name: "no slash at all", in: "planner", wantErr: true},
		// The one that matters: `--undecline --apply` would otherwise
		// swallow the next flag and silently downgrade the run.
		{name: "a flag mistaken for a value", in: "--apply", wantErr: true},
		{name: "nothing before the slash", in: "/planner", wantErr: true},
		{name: "nothing after the slash", in: "Web/example/", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseKey(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseKey(%q) = %+v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKey(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseKey(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			if got.String() != tc.want.Project+"/"+tc.want.Name {
				t.Errorf("String() = %q, which is not the form the flag takes back", got.String())
			}
		})
	}
}

// TestLookupLink_ResolvesTheParentSoOneLinkHasOneKey. A scan rooted at one
// spelling of a directory and a project derived from a resolved $HOME name the
// same link two ways; without this the run records under one and the next run
// finds the link unowned.
func TestLookupLink_ResolvesTheParentSoOneLinkHasOneKey(t *testing.T) {
	f := newFixture(t)
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real", ".claude", "agents")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", realDir, err)
	}
	alias := filepath.Join(tmp, "alias")
	if err := os.Symlink(filepath.Join(tmp, "real"), alias); err != nil {
		t.Fatalf("symlinking %s: %v", alias, err)
	}

	l := f.link("proj", "planner")
	l.Path = filepath.Join(alias, ".claude", "agents", "planner.md")
	doc := New()
	rec, err := doc.RecordCreated(l, testTime)
	if err != nil {
		t.Fatalf("RecordCreated: %v", err)
	}

	// Resolved, because a temp dir is itself reached through a symlink on
	// macOS — which is the same aliasing this is about.
	resolvedDir, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("resolving %s: %v", realDir, err)
	}
	viaReal := filepath.Join(resolvedDir, "planner.md")
	if rec.Path != viaReal {
		t.Errorf("stored path = %q, want the resolved %q", rec.Path, viaReal)
	}
	if _, ok := doc.LookupLink(filepath.Join(realDir, "planner.md")); !ok {
		t.Errorf("%q did not find the record made through %q", viaReal, l.Path)
	}
	if _, ok := doc.LookupLink(l.Path); !ok {
		t.Errorf("%q no longer finds its own record", l.Path)
	}
}
