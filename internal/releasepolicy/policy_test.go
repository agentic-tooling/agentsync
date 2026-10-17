package releasepolicy

import (
	"strings"
	"testing"
)

// wantBumps is the release contract written out by hand, so a change to the
// type list in policy.go has to be made here too.
var wantBumps = map[string]Bump{
	"feat": BumpMinor,
	"fix":  BumpPatch, "perf": BumpPatch, "refactor": BumpPatch, "revert": BumpPatch,
	"chore": BumpNone, "docs": BumpNone, "test": BumpNone, "style": BumpNone, "ci": BumpNone, "build": BumpNone,
}

func TestTypeList(t *testing.T) {
	if len(bumps) != len(wantBumps) {
		t.Errorf("policy has %d types, want %d", len(bumps), len(wantBumps))
	}
	for typ, want := range wantBumps {
		got, ok := bumps[typ]
		if !ok || got != want {
			t.Errorf("bumps[%q] = %v, %v; want %v", typ, got, ok, want)
		}
	}
}

func TestParseSubject_AllowedTypes(t *testing.T) {
	for typ := range wantBumps {
		for _, s := range []string{typ + ": do a thing", typ + "(cli): do a thing"} {
			got, err := ParseSubject(s)
			if err != nil {
				t.Errorf("ParseSubject(%q) error = %v", s, err)
				continue
			}
			if got.Type != typ || got.Description != "do a thing" {
				t.Errorf("ParseSubject(%q) = %+v", s, got)
			}
		}
	}
}

func TestParseSubject_Scope(t *testing.T) {
	got, err := ParseSubject("fix(internal/scan.v2): handle it")
	if err != nil || got.Scope != "internal/scan.v2" {
		t.Fatalf("ParseSubject() = %+v, %v", got, err)
	}
}

func TestParseSubject_SquashSuffix(t *testing.T) {
	got, err := ParseSubject("feat: add x (#12)")
	if err != nil || got.Type != "feat" || got.Description != "add x (#12)" {
		t.Fatalf("ParseSubject() = %+v, %v", got, err)
	}
}

func TestParseSubject_Rejects(t *testing.T) {
	tests := []struct {
		name, subject, wantErr string
	}{
		{"unknown type", "feature: add", "unknown commit type"},
		{"missing space after colon", "fix:broken", "not a Conventional Commit"},
		{"bang", "feat!: big change", "major releases are disabled"},
		{"bang with scope", "fix(cli)!: big change", "major releases are disabled"},
		{"bang with squash suffix", "feat!: z (#3)", "major releases are disabled"},
		{"uppercase type", "Fix: broken", "not a Conventional Commit"},
		{"empty description", "fix:  ", "description is empty"},
		{"no description", "fix: ", "not a Conventional Commit"},
		{"empty", "", "not a Conventional Commit"},
		{"merge subject", "Merge pull request #1 from a/b", "not a Conventional Commit"},
		{"empty scope", "fix(): broken", "not a Conventional Commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSubject(tt.subject)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseSubject(%q) error = %v, want %q", tt.subject, err, tt.wantErr)
			}
		})
	}
}

func TestCheckMessage(t *testing.T) {
	tests := []struct {
		name, msg string
		wantErr   bool
	}{
		{"subject only", "fix: x", false},
		{"prose mentions breaking", "fix: x\n\nThis is not a breaking change.\nBreaking things is bad.", false},
		{"footer space", "fix: x\n\nBREAKING CHANGE: gone", true},
		{"footer hyphen", "fix: x\n\nBREAKING-CHANGE: gone", true},
		{"footer lowercase", "fix: x\n\nbreaking change: gone", true},
		{"footer without blank line", "fix: x\nBREAKING CHANGE: gone", true},
		{"footer after blank lines", "feat: x\n\nbody\n\n\n\nRefs: 1\nBREAKING CHANGE: gone", true},
		{"footer with CRLF", "fix: x\r\n\r\nBREAKING CHANGE: gone\r\n", true},
		{"bad subject", "fix!: x\n\nbody", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := CheckMessage(tt.msg); (err != nil) != tt.wantErr {
				t.Fatalf("CheckMessage() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBumpFor(t *testing.T) {
	for typ, want := range wantBumps {
		if got := BumpFor(typ); got != want {
			t.Errorf("BumpFor(%q) = %v, want %v", typ, got, want)
		}
	}
	if got := BumpFor("unknown"); got != BumpNone {
		t.Errorf("BumpFor(unknown) = %v, want BumpNone", got)
	}
}

func TestNextVersion(t *testing.T) {
	tests := []struct {
		name        string
		latest      string
		messages    []string
		want        string
		wantRelease bool
		wantErr     string
	}{
		{"squash suffixes", "v0.0.2", []string{"ci: enforce (#12)", "fix: a (#9)"}, "v0.0.3", true, ""},
		{"footer in body", "v0.0.2", []string{"fix: x\n\nBREAKING CHANGE: y\n"}, "", false, "BREAKING CHANGE footer"},
		{"footer without blank line", "v0.0.2", []string{"fix: x\nBREAKING CHANGE: y"}, "", false, "BREAKING CHANGE footer"},
		{"hyphenated footer", "v0.0.2", []string{"docs: x\n\nBREAKING-CHANGE: y"}, "", false, "BREAKING CHANGE footer"},
		{"prose mentions breaking", "v0.0.2", []string{"fix: x\n\nNot a breaking change.\n"}, "v0.0.3", true, ""},
		{"empty message", "v0.0.2", []string{"fix: a", ""}, "", false, "not a Conventional Commit"},
		{"merge body not checked", "v0.0.2", []string{"Merge pull request #6 from a/b\n\nfeat!: wip", "docs: a"}, "", false, ""},
		{"feat wins over fix", "v0.0.2", []string{"fix: a", "feat: b", "fix: c"}, "v0.1.0", true, ""},
		{"fix bumps patch", "v0.3.4", []string{"docs: a", "refactor(x): b"}, "v0.3.5", true, ""},
		{"chore and docs release nothing", "v0.0.2", []string{"chore: a", "docs: b", "ci: c"}, "", false, ""},
		{"no messages", "v0.0.2", nil, "", false, ""},
		{"merge subjects skipped", "v0.0.2", []string{"Merge pull request #6 from a/b", "Merge branch 'main' into x", "docs: a"}, "", false, ""},
		{"release bookkeeping skipped", "v0.0.2", []string{"chore(release): v0.0.3"}, "", false, ""},
		{"bad subject stops release", "v0.0.2", []string{"feat: a", "updated stuff"}, "", false, "updated stuff"},
		{"bang stops release", "v0.0.2", []string{"fix!: a"}, "", false, "major releases are disabled"},
		{"major latest refused", "v1.0.0", []string{"fix: a"}, "", false, "major releases are disabled"},
		{"malformed latest refused", "0.1.0", []string{"fix: a"}, "", false, "must be vX.Y.Z"},
		{"empty latest patch", "", []string{"fix: a"}, "v0.0.1", true, ""},
		{"empty latest minor", "", []string{"feat: a"}, "v0.1.0", true, ""},
		{"overflow refused", "v0.0.9223372036854775807", []string{"fix: a"}, "", false, "overflows"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, release, err := NextVersion(tt.latest, tt.messages)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NextVersion() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want || release != tt.wantRelease {
				t.Fatalf("NextVersion() = %q, %v, %v; want %q, %v", got, release, err, tt.want, tt.wantRelease)
			}
		})
	}
}

func TestCheckTag(t *testing.T) {
	tests := []struct {
		tag     string
		wantErr bool
	}{
		{"v0.0.3", false},
		{"v0.10.0", false},
		{"v0.0.0", false},
		{"v1.0.0", true},
		{"v2.3.4", true},
		{"v01.0.0", true},
		{"v0.01.0", true},
		{"v0.0.01", true},
		{"0.1.0", true},
		{"v0.1", true},
		{"v0.1.0-rc1", true},
		{"", true},
	}
	for _, tt := range tests {
		if err := CheckTag(tt.tag); (err != nil) != tt.wantErr {
			t.Errorf("CheckTag(%q) error = %v, wantErr %v", tt.tag, err, tt.wantErr)
		}
	}
}
