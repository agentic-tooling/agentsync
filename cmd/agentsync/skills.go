package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/agentic-tooling/agentsync/internal/config"
	"github.com/agentic-tooling/agentsync/internal/scan"
	"github.com/agentic-tooling/agentsync/internal/skills"
)

func openCatalog(repo *scan.Repo, strict bool) (*skills.Catalog, error) {
	shared, err := config.SkillsRoot(strict)
	if err != nil {
		return nil, err
	}
	return skills.Open(repo, shared)
}

func scanSkills(repo *scan.Repo, dirs []string) (skills.Report, error) {
	catalog, err := openCatalog(repo, len(dirs) > 0)
	if err != nil {
		return skills.Report{}, err
	}
	defer func() { _ = catalog.Close() }()
	return catalog.ScanDirs(dirs), nil
}

func emitSkills(w io.Writer, report skills.Report) {
	if len(report.Entries) == 0 && len(report.Problems) == 0 {
		return
	}
	fmt.Fprintln(w, "Project skills")
	for _, e := range report.Entries {
		fmt.Fprintf(w, "  %s %s/%s", e.State, e.Project, e.Name)
		if e.Reason != "" {
			fmt.Fprintf(w, " (%s)", e.Reason)
		}
		if e.LinkTarget != "" {
			fmt.Fprintf(w, " -> %s", e.LinkTarget)
		}
		fmt.Fprintln(w)
	}
	emitProblems(w, report.Problems)
}

type skillRun struct {
	mode             skills.Mode
	apply            bool
	excludeLocal     bool
	allowOutsideHome bool
	asJSON           bool
	failOnPending    bool
	keys             []string
	names            []string
}

func (r skillRun) run(repo *scan.Repo, repoFrom string, root rootChoice, stdout, stderr io.Writer) int {
	catalog, plan, scanned, err := r.prepare(repo, root)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}
	defer func() { _ = catalog.Close() }()
	if r.apply {
		catalog.Apply(plan, r.excludeLocal)
	}
	w := reportStream(r.asJSON, stdout, stderr)
	emitRepoChoice(w, repo, repoFrom)
	emitRootChoice(w, repo.Home, root)
	if r.asJSON {
		report := struct {
			Apply    bool               `json:"apply"`
			Repo     string             `json:"repo"`
			Root     string             `json:"root"`
			Actions  skills.Plan        `json:"skill_actions"`
			Problems []scan.ScanProblem `json:"skill_problems,omitempty"`
		}{r.apply, repo.Root, root.Path, plan, scanned.Problems}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "agentsync: %v\n", err)
			return exitFailure
		}
	} else {
		emitSkillActions(w, plan, r.apply)
		emitProblems(w, scanned.Problems)
	}
	pending, written, blocked, failed := plan.Counts()
	if r.mode == skills.Link {
		emitSkillRestart(w, written)
	}
	return skillExitCode(pending, blocked+len(scanned.Problems), failed, r.failOnPending)
}

func (r skillRun) prepare(repo *scan.Repo, root rootChoice) (*skills.Catalog, skills.Plan, skills.Report, error) {
	var scanned skills.Report
	if r.mode == skills.Link && len(r.keys) == 0 {
		return nil, nil, scanned, fmt.Errorf("--skill requires a current project or --project with --all")
	}
	catalog, err := openCatalog(repo, true)
	if err != nil {
		return nil, nil, scanned, err
	}
	if r.mode == skills.Import && len(r.keys) == 0 {
		scanned, err = catalog.Scan(root.Path)
		if err != nil {
			_ = catalog.Close()
			return nil, nil, scanned, err
		}
	}
	plan := catalog.Plan(r.mode, r.keys, r.names, scanned, r.excludeLocal)
	if err := repo.RequireHome(root.Path, plan.Dirs(), r.allowOutsideHome); err != nil {
		_ = catalog.Close()
		return nil, nil, scanned, err
	}
	return catalog, plan, scanned, nil
}

func emitSkillRestart(w io.Writer, written int) {
	if written > 0 {
		fmt.Fprintln(w, "Restart your Claude Code session to load the linked project skills.")
	}
}

func skillExitCode(pending, blocked, failed int, failOnPending bool) int {
	if failed > 0 {
		return exitFailure
	}
	if blocked > 0 || (failOnPending && pending > 0) {
		return exitAtRisk
	}
	return exitOK
}

func emitSkillActions(w io.Writer, plan skills.Plan, apply bool) {
	if !apply {
		fmt.Fprintln(w, "DRY RUN — pass --apply to carry out the pending skill changes.")
	}
	for _, item := range plan {
		fmt.Fprintf(w, "  %s %s/%s: %s\n", item.Status, item.Project, item.Name, item.Reason)
		if item.Canonical != "" {
			fmt.Fprintf(w, "    %s <-> %s\n", item.Path, item.Canonical)
		}
		if item.ExcludeFile != "" {
			fmt.Fprintf(w, "    %s in %s\n", item.Pattern, item.ExcludeFile)
		}
	}
}
