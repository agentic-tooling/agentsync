// Package releasepolicy decides what a Conventional Commit subject releases.
// Major releases are disabled: breaking markers are rejected, never downgraded.
package releasepolicy

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	subjectRE = regexp.MustCompile(`^([a-z]+)(?:\(([A-Za-z0-9][A-Za-z0-9._/-]{0,63})\))?(!)?: (.+)$`)
	tagRE     = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// Bump is how far a change moves the version. The zero value releases nothing.
type Bump int

const (
	BumpNone Bump = iota
	BumpPatch
	BumpMinor
)

var bumps = map[string]Bump{
	"build": BumpNone, "chore": BumpNone, "ci": BumpNone, "docs": BumpNone,
	"style": BumpNone, "test": BumpNone,
	"fix": BumpPatch, "perf": BumpPatch, "refactor": BumpPatch, "revert": BumpPatch,
	"feat": BumpMinor,
}

// Subject is a parsed Conventional Commit subject line.
type Subject struct {
	Type        string
	Scope       string
	Description string
}

// ParseSubject parses `type(scope): description`. Unknown types and `!` are errors.
func ParseSubject(s string) (Subject, error) {
	m := subjectRE.FindStringSubmatch(s)
	if m == nil {
		return Subject{}, fmt.Errorf("%q is not a Conventional Commit subject (type(scope): description)", s)
	}
	if _, ok := bumps[m[1]]; !ok {
		return Subject{}, fmt.Errorf("unknown commit type %q", m[1])
	}
	if m[3] != "" {
		return Subject{}, errors.New("major releases are disabled; drop the ! marker")
	}
	desc := strings.TrimSpace(m[4])
	if desc == "" {
		return Subject{}, errors.New("description is empty")
	}
	return Subject{Type: m[1], Scope: m[2], Description: desc}, nil
}

// CheckMessage validates a full commit message: its first line as the subject,
// and no breaking-change footer on any later line.
func CheckMessage(msg string) (Subject, error) {
	first, body, _ := strings.Cut(msg, "\n")
	sub, err := ParseSubject(strings.TrimRight(first, "\r"))
	if err != nil {
		return Subject{}, err
	}
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.ToUpper(strings.TrimSpace(line))
		if strings.HasPrefix(line, "BREAKING CHANGE:") || strings.HasPrefix(line, "BREAKING-CHANGE:") {
			return Subject{}, errors.New("major releases are disabled; remove the BREAKING CHANGE footer")
		}
	}
	return sub, nil
}

// BumpFor returns the bump for a commit type; unknown types release nothing.
func BumpFor(typ string) Bump {
	return bumps[typ]
}

// NextVersion returns the tag that the full commit messages since latest would
// release, if any. An empty latest means v0.0.0. Merge commits and release
// bookkeeping are skipped by first line; every other message must pass CheckMessage.
func NextVersion(latest string, messages []string) (string, bool, error) {
	minor, patch := 0, 0
	if latest != "" {
		var err error
		if minor, patch, err = parseTag(latest); err != nil {
			return "", false, err
		}
	}
	bump := BumpNone
	for _, msg := range messages {
		first, _, _ := strings.Cut(msg, "\n")
		first = strings.TrimRight(first, "\r")
		if strings.HasPrefix(first, "Merge pull request ") || strings.HasPrefix(first, "Merge branch ") ||
			strings.HasPrefix(first, "chore(release):") {
			continue
		}
		sub, err := CheckMessage(msg)
		if err != nil {
			return "", false, fmt.Errorf("commit %q: %w", first, err)
		}
		bump = max(bump, BumpFor(sub.Type))
	}
	switch bump {
	case BumpMinor:
		minor, patch = minor+1, 0
	case BumpPatch:
		patch++
	default:
		return "", false, nil
	}
	if minor < 0 || patch < 0 {
		return "", false, fmt.Errorf("tag %q: version component overflows", latest)
	}
	return fmt.Sprintf("v0.%d.%d", minor, patch), true, nil
}

// CheckTag accepts only v0.Y.Z release tags without leading zeros.
func CheckTag(tag string) error {
	_, _, err := parseTag(tag)
	return err
}

func parseTag(tag string) (minor, patch int, err error) {
	m := tagRE.FindStringSubmatch(tag)
	if m == nil {
		return 0, 0, fmt.Errorf("tag %q must be vX.Y.Z with no leading zeros", tag)
	}
	if m[1] != "0" {
		return 0, 0, fmt.Errorf("tag %q: major releases are disabled; only v0.Y.Z is allowed", tag)
	}
	if minor, err = strconv.Atoi(m[2]); err != nil {
		return 0, 0, fmt.Errorf("tag %q: %w", tag, err)
	}
	if patch, err = strconv.Atoi(m[3]); err != nil {
		return 0, 0, fmt.Errorf("tag %q: %w", tag, err)
	}
	return minor, patch, nil
}
