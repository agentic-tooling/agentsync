package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// version is the release tag, and the only piece of build metadata the Go
// toolchain cannot work out for itself.
//
// Everything else on the version report — commit, build time, dirty state — is
// read from the build info the toolchain embeds automatically since Go 1.18, so
// it is correct for `go build`, `go run`, and `go install` alike with nothing
// stamped and no build recipe involved. Hand-stamping those through -ldflags is
// the mistake this file exists to avoid: it duplicates what the toolchain
// already recorded, and a duplicate can disagree with the original.
//
// A tag is genuinely different. It lives in git refs rather than in the build,
// and Go only knows one when it can resolve HEAD to it. So this stays an
// -ldflags seam, for a release build that wants to state its tag outright:
//
//	-ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// It is empty by default, which is what lets buildDetails fall through to the
// module version and then to "dev". An unstamped binary must say "dev" rather
// than claim a release it is not.
var version = ""

// buildDetails is the version report, resolved once from the embedded build
// info. Fields are empty when the toolchain recorded nothing for them, which
// happens for a binary built with -buildvcs=false or outside a work tree; the
// report omits those lines rather than printing blanks.
type buildDetails struct {
	Version  string
	Commit   string
	Modified bool
	Time     string
	Go       string
	Platform string
}

// devVersion is what Go records for a build it cannot pin to a module version.
// It is not a version, so it must not be reported as one.
const devVersion = "(devel)"

// readBuildDetails resolves the report from build info.
//
// It takes the info as arguments rather than calling debug.ReadBuildInfo
// itself, because the interesting cases are the degraded ones — no VCS data, a
// dirty tree, no module version — and a test can only construct those by
// passing them in. The ok parameter mirrors ReadBuildInfo's second return.
//
// Version precedence is most-specific-first: an explicit -ldflags stamp beats
// what Go inferred, since it is the only one that can name a tag the build
// could not resolve; the module version beats nothing at all, including when it
// is a pseudo-version, because a pseudo-version still identifies the commit and
// time exactly and is therefore more useful than "dev".
func readBuildDetails(bi *debug.BuildInfo, ok bool) buildDetails {
	d := buildDetails{
		Version: version,
		// runtime, not the build info: these describe the binary that is
		// actually running. They agree in every ordinary build, and where they
		// could disagree the running values are the true ones.
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	if ok && bi != nil {
		if d.Version == "" && bi.Main.Version != "" && bi.Main.Version != devVersion {
			d.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				d.Commit = s.Value
			case "vcs.time":
				d.Time = s.Value
			case "vcs.modified":
				d.Modified = s.Value == "true"
			}
		}
	}

	if d.Version == "" {
		d.Version = "dev"
	}
	return d
}

// String renders the report.
//
// The first line stays exactly "agentsync <version>", unchanged from when that
// was the whole output, so anything parsing it with `head -1` keeps working.
// The detail lines are additions below it, and each is omitted when the
// toolchain recorded nothing for it.
func (d buildDetails) String() string {
	s := fmt.Sprintf("agentsync %s\n", d.Version)
	if d.Commit != "" {
		state := "clean"
		if d.Modified {
			// Worth naming loudly: a dirty binary was built from source that
			// exists in no commit, so its behaviour cannot be reproduced from
			// the revision printed beside it.
			state = "DIRTY — built from uncommitted changes"
		}
		s += fmt.Sprintf("  commit    %s (%s)\n", d.Commit, state)
	}
	if d.Time != "" {
		s += fmt.Sprintf("  built     %s\n", d.Time)
	}
	s += fmt.Sprintf("  go        %s %s\n", d.Go, d.Platform)
	return s
}

func runVersion(stdout io.Writer) int {
	fmt.Fprint(stdout, readBuildDetails(debug.ReadBuildInfo()).String())
	return exitOK
}
