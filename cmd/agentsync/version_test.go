package main

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// setVersion stamps the -ldflags seam for one test and restores it after.
// These tests must not run in parallel with each other for that reason.
func setVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

func vcsInfo(mainVersion string, settings map[string]string) *debug.BuildInfo {
	bi := &debug.BuildInfo{GoVersion: "go1.25.14"}
	bi.Main.Version = mainVersion
	for k, v := range settings {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: k, Value: v})
	}
	return bi
}

func TestReadBuildDetailsVersionPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		stamp       string
		mainVersion string
		want        string
	}{{
		// The case that matters most: nothing identifies the build, so it must
		// not imply that something does.
		name:        "nothing recorded falls back to dev",
		mainVersion: "",
		want:        "dev",
	}, {
		name:        "(devel) is not a version",
		mainVersion: devVersion,
		want:        "dev",
	}, {
		name:        "module version is used when Go resolved a tag",
		mainVersion: "v0.1.0",
		want:        "v0.1.0",
	}, {
		// A pseudo-version is ugly but exact — it names the commit and the
		// time — so it beats reporting "dev" for a build that is identifiable.
		name:        "pseudo-version beats dev",
		mainVersion: "v0.0.0-20260824131222-a51b5ed4ade6",
		want:        "v0.0.0-20260824131222-a51b5ed4ade6",
	}, {
		// The whole reason the -ldflags seam survives: a local build from a
		// tagged commit can state the tag, which Go alone may not resolve.
		name:        "an explicit stamp wins over the module version",
		stamp:       "v0.2.0",
		mainVersion: "v0.0.0-20260824131222-a51b5ed4ade6",
		want:        "v0.2.0",
	}, {
		name:        "a dirty stamp is reported verbatim",
		stamp:       "v0.1.0-3-ga51b5ed-dirty",
		mainVersion: devVersion,
		want:        "v0.1.0-3-ga51b5ed-dirty",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setVersion(t, tc.stamp)
			got := readBuildDetails(vcsInfo(tc.mainVersion, nil), true)
			if got.Version != tc.want {
				t.Errorf("Version = %q, want %q", got.Version, tc.want)
			}
		})
	}
}

func TestReadBuildDetailsWithoutBuildInfo(t *testing.T) {
	setVersion(t, "")

	// ReadBuildInfo reports ok=false for a binary the toolchain recorded
	// nothing for. Every VCS field must then be empty rather than guessed, and
	// the two runtime fields must still be populated, since they do not depend
	// on the build having been recorded at all.
	got := readBuildDetails(nil, false)

	if got.Version != "dev" {
		t.Errorf("Version = %q, want %q", got.Version, "dev")
	}
	if got.Commit != "" || got.Time != "" || got.Modified {
		t.Errorf("VCS fields = %q/%q/%v, want all empty", got.Commit, got.Time, got.Modified)
	}
	if got.Go != runtime.Version() {
		t.Errorf("Go = %q, want %q", got.Go, runtime.Version())
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; got.Platform != want {
		t.Errorf("Platform = %q, want %q", got.Platform, want)
	}
}

// ok=true with a nil pointer is not something ReadBuildInfo produces, but
// readBuildDetails takes both as parameters and must not panic on the
// combination.
func TestReadBuildDetailsNilInfoWithOK(t *testing.T) {
	setVersion(t, "")
	if got := readBuildDetails(nil, true); got.Version != "dev" {
		t.Errorf("Version = %q, want %q", got.Version, "dev")
	}
}

func TestReadBuildDetailsVCSSettings(t *testing.T) {
	setVersion(t, "")
	got := readBuildDetails(vcsInfo(devVersion, map[string]string{
		"vcs":          "git",
		"vcs.revision": "a51b5ed4ade603c5968b1c73a004ebcaba4b6844",
		"vcs.time":     "2026-08-24T13:12:22Z",
		"vcs.modified": "true",
		// An unrelated setting must be ignored rather than mistaken for one of
		// the three that are read.
		"-ldflags": "-s -w",
	}), true)

	if want := "a51b5ed4ade603c5968b1c73a004ebcaba4b6844"; got.Commit != want {
		t.Errorf("Commit = %q, want %q", got.Commit, want)
	}
	if want := "2026-08-24T13:12:22Z"; got.Time != want {
		t.Errorf("Time = %q, want %q", got.Time, want)
	}
	if !got.Modified {
		t.Error("Modified = false, want true")
	}
}

func TestReadBuildDetailsModifiedIsOnlyTrueForTrue(t *testing.T) {
	setVersion(t, "")
	// vcs.modified is recorded as the strings "true" and "false". Anything
	// else means the toolchain did not say the tree was dirty, and the report
	// must not claim it was.
	for _, v := range []string{"false", "", "TRUE", "1"} {
		got := readBuildDetails(vcsInfo(devVersion, map[string]string{"vcs.modified": v}), true)
		if got.Modified {
			t.Errorf("vcs.modified=%q gave Modified = true, want false", v)
		}
	}
}

func TestBuildDetailsString(t *testing.T) {
	t.Run("first line keeps the original one-line contract", func(t *testing.T) {
		// Anything parsing the old single-line output with `head -1` must keep
		// working, so this prefix is a contract and not a formatting choice.
		d := buildDetails{Version: "v0.1.0", Go: "go1.25.14", Platform: "darwin/arm64"}
		first, _, _ := strings.Cut(d.String(), "\n")
		if want := "agentsync v0.1.0"; first != want {
			t.Errorf("first line = %q, want %q", first, want)
		}
	})

	t.Run("a clean build says so", func(t *testing.T) {
		d := buildDetails{Version: "v0.1.0", Commit: "a51b5ed", Go: "go1.25.14", Platform: "darwin/arm64"}
		if got := d.String(); !strings.Contains(got, "a51b5ed (clean)") {
			t.Errorf("String() = %q, want a clean commit line", got)
		}
	})

	t.Run("a dirty build is called out loudly", func(t *testing.T) {
		// A dirty binary was built from source that exists in no commit, so
		// the revision printed beside it does not reproduce it. That has to be
		// obvious in the output, not inferred from a subtle marker.
		d := buildDetails{Version: "v0.1.0", Commit: "a51b5ed", Modified: true, Go: "go1.25.14", Platform: "darwin/arm64"}
		got := d.String()
		if !strings.Contains(got, "DIRTY") {
			t.Errorf("String() = %q, want it to name the dirty state", got)
		}
		if strings.Contains(got, "(clean)") {
			t.Errorf("String() = %q, must not also claim clean", got)
		}
	})

	t.Run("absent fields are omitted, not blank", func(t *testing.T) {
		d := buildDetails{Version: "dev", Go: "go1.25.14", Platform: "darwin/arm64"}
		got := d.String()
		if strings.Contains(got, "commit") {
			t.Errorf("String() = %q, want no commit line when none was recorded", got)
		}
		if strings.Contains(got, "built") {
			t.Errorf("String() = %q, want no built line when none was recorded", got)
		}
		if !strings.Contains(got, "go        go1.25.14 darwin/arm64") {
			t.Errorf("String() = %q, want the go line", got)
		}
	})
}
