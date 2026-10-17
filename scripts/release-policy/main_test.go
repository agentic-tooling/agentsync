package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"no args", nil, "", exitUsage, "", "usage:"},
		{"unknown command", []string{"bogus"}, "", exitUsage, "", "usage:"},
		{"unknown flag", []string{"check-tag", "--latest", "v0.1.0", "v0.1.0"}, "", exitUsage, "", "not defined"},

		{"subject valid", []string{"validate-subject"}, "feat(cli): add x\n", exitOK, "", ""},
		{"subject bang", []string{"validate-subject"}, "feat!: add x\n", exitPolicy, "", "invalid commit message: major releases are disabled"},
		{"pr title wording", []string{"validate-subject", "--pr-title"}, "Add x\n", exitPolicy, "", "invalid PR title"},
		{"subject footer", []string{"validate-subject"}, "fix: x\n\nBREAKING CHANGE: y\n", exitPolicy, "", "BREAKING CHANGE"},
		{"subject extra arg", []string{"validate-subject", "fix: x"}, "", exitUsage, "", "usage:"},

		{"next minor", []string{"next-version", "--latest", "v0.0.2"}, "fix: a\r\n\r\nbody\r\n\x00feat: b\n\x00", exitOK, "v0.1.0\n", ""},
		{"next none", []string{"next-version", "--latest", "v0.0.2"}, "docs: a\n\x00Merge pull request #6 from a/b\n\nfix!: x\n\x00", exitOK, "", ""},
		{"next empty stdin", []string{"next-version", "--latest", "v0.0.2"}, "", exitOK, "", ""},
		{"next footer in body", []string{"next-version", "--latest", "v0.0.2"}, "feat: a\n\x00fix: x\n\nBREAKING CHANGE: y\n\x00", exitPolicy, "", "BREAKING CHANGE footer"},
		{"next bad subject", []string{"next-version", "--latest", "v0.0.2"}, "fix: a\x00wip\x00", exitPolicy, "", `"wip"`},
		{"next empty message", []string{"next-version", "--latest", "v0.0.2"}, "fix: a\x00\x00", exitPolicy, "", `commit ""`},
		{"next lone NUL", []string{"next-version", "--latest", "v0.0.2"}, "\x00", exitPolicy, "", `commit ""`},
		{"next not NUL-terminated", []string{"next-version", "--latest", "v0.0.2"}, "fix: a\nfeat: b\n", exitUsage, "", "NUL-terminated"},
		{"next major latest", []string{"next-version", "--latest", "v1.2.3"}, "fix: a\x00", exitPolicy, "", "major releases are disabled"},
		{"next missing latest", []string{"next-version"}, "fix: a\x00", exitUsage, "", "usage:"},

		{"tag ok", []string{"check-tag", "v0.0.3"}, "", exitOK, "", ""},
		{"tag major", []string{"check-tag", "v1.0.0"}, "", exitPolicy, "", "major releases are disabled"},
		{"tag missing", []string{"check-tag"}, "", exitUsage, "", "usage:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, strings.NewReader(tt.stdin), &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit = %d, want %d; stderr=%q", code, tt.wantCode, stderr.String())
			}
			if stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr == "" && stderr.Len() != 0 || !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}
