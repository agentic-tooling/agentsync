// Command release-policy applies internal/releasepolicy in CI. It is not part
// of the agentsync binary. Exit 0 is clean, 1 a policy violation, 2 a usage error.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/releasepolicy"
)

const (
	exitOK     = 0
	exitPolicy = 1
	exitUsage  = 2
)

const usage = `usage: release-policy validate-subject [--pr-title] < message
       git log -z --format=%B RANGE | release-policy next-version --latest vX.Y.Z
       release-policy check-tag vX.Y.Z`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var prTitle bool
	var latest string
	switch args[0] {
	case "validate-subject":
		fs.BoolVar(&prTitle, "pr-title", false, "word errors for a pull request title")
	case "next-version":
		fs.StringVar(&latest, "latest", "", "latest release tag (required)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}

	switch args[0] {
	case "validate-subject":
		if fs.NArg() != 0 {
			break
		}
		msg, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "read stdin: %v\n", err)
			return exitUsage
		}
		if _, err := releasepolicy.CheckMessage(strings.TrimRight(string(msg), "\r\n")); err != nil {
			what := "commit message"
			if prTitle {
				what = "PR title (it becomes the squash commit subject)"
			}
			fmt.Fprintf(stderr, "invalid %s: %v\n", what, err)
			return exitPolicy
		}
		return exitOK

	case "next-version":
		// Required so a dropped flag cannot silently release from v0.0.0.
		if fs.NArg() != 0 || latest == "" {
			break
		}
		in, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "read stdin: %v\n", err)
			return exitUsage
		}
		// `git log -z` terminates every message with NUL, so non-empty input
		// without one is newline-joined subjects whose bodies were never seen.
		var messages []string
		if len(in) > 0 {
			if in[len(in)-1] != 0 {
				fmt.Fprintln(stderr, "stdin must be NUL-terminated commit messages (git log -z --format=%B)")
				return exitUsage
			}
			messages = strings.Split(string(in[:len(in)-1]), "\x00")
		}
		next, release, err := releasepolicy.NextVersion(latest, messages)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitPolicy
		}
		if release {
			fmt.Fprintln(stdout, next)
		}
		return exitOK

	case "check-tag":
		if fs.NArg() != 1 {
			break
		}
		if err := releasepolicy.CheckTag(fs.Arg(0)); err != nil {
			fmt.Fprintln(stderr, err)
			return exitPolicy
		}
		return exitOK
	}
	fmt.Fprintln(stderr, usage)
	return exitUsage
}
