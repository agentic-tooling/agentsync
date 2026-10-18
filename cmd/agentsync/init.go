package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/config"
	"github.com/agentic-tooling/agentsync/internal/discover"
)

// promptInput is where init reads answers, and whether it may prompt at all:
// only when stdin and stdout are both terminals. Tests replace it.
var promptInput = func() (io.Reader, bool) {
	return os.Stdin, terminal(os.Stdin) && terminal(os.Stdout)
}

// terminal reports whether f is a character device other than the null device,
// which is how `init </dev/null` stays a listing instead of a prompt.
func terminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err == nil && !os.SameFile(fi, null)
}

// initRun is one init invocation's context, shared by its two modes.
type initRun struct {
	stdout, stderr io.Writer
	home, cfgPath  string
	force          bool
}

func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	repoF := fs.String("repo", "", "canonical agent repo to write into the config file")
	rootF := fs.String("root", "", "default tree of projects to write into the config file")
	apply := fs.Bool("apply", false, "write the file; without it init is a dry run")
	force := fs.Bool("force", false, "replace an existing config file")
	search := fs.String("search", "~", "where to look for agent repos")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFailure
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "agentsync: init takes no arguments, got %q\n", fs.Arg(0))
		return exitFailure
	}
	repoArg, rootArg := strings.TrimSpace(*repoF), strings.TrimSpace(*rootF)
	// Nothing is written that the user did not name.
	if *apply && (repoArg == "" || rootArg == "") {
		fmt.Fprintln(stderr, "agentsync: init --apply needs both --repo and --root; run init without --apply to see candidates")
		return exitFailure
	}

	cfgPath, err := config.Path()
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}
	r := initRun{stdout: stdout, stderr: stderr, home: os.Getenv("HOME"), cfgPath: cfgPath, force: *force}
	if code, done := r.refuseExisting(); done {
		return code
	}

	var repo, root string
	if rootArg != "" {
		if root, err = checkRoot(rootArg); err != nil {
			fmt.Fprintf(stderr, "agentsync: %v\n", err)
			return exitFailure
		}
	}
	if repoArg != "" {
		if repo, err = checkRepo(repoArg); err != nil {
			fmt.Fprintf(stderr, "agentsync: %v\n", err)
			return exitFailure
		}
		if root == "" {
			root = filepath.Dir(repo)
		}
		return r.dryRunOrApply(repo, root, *apply)
	}

	in, tty := promptInput()
	interactive := tty && rootArg == ""
	searchRoot, err := config.ParsePath("--search", *search)
	if err != nil {
		fmt.Fprintf(stderr, "agentsync: %v\n", err)
		return exitFailure
	}
	found := r.listCandidates(searchRoot, interactive)
	if interactive {
		return r.interactive(bufio.NewScanner(in), found.Candidates)
	}
	if len(found.Candidates) == 0 {
		fmt.Fprintln(stdout, "Name yours: agentsync init --repo PATH --root PATH --apply")
		return exitAtRisk
	}
	best := found.Candidates[0].Path
	if root == "" {
		root = filepath.Dir(best)
	}
	fmt.Fprintf(stdout, "\nNothing written. To use the first, run:\n  %s\n", r.command(best, root))
	return exitOK
}

// refuseExisting stops the run when a config file is already there and --force
// was not given, showing what it holds so the user can decide.
func (r initRun) refuseExisting() (int, bool) {
	_, err := os.Lstat(r.cfgPath)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && r.force) {
		return 0, false
	}
	if err != nil {
		fmt.Fprintf(r.stderr, "agentsync: config file %s: %v\n", r.cfgPath, err)
		return exitFailure, true
	}
	fmt.Fprintf(r.stderr, "agentsync: config file %s already exists; pass --force to replace it.\n", r.show(r.cfgPath))
	if data, err := os.ReadFile(r.cfgPath); err != nil {
		fmt.Fprintf(r.stderr, "  (could not read it: %v)\n", err)
	} else {
		fmt.Fprint(r.stderr, indent(string(data)))
	}
	return exitAtRisk, true
}

func (r initRun) listCandidates(searchRoot string, numbered bool) discover.Result {
	fmt.Fprintf(r.stdout, "Looking for agent repos under %s (depth %d, at most %d directories)...\n",
		r.show(searchRoot), discover.Depth, discover.MaxDirs)
	found := discover.Find(searchRoot, discover.MaxDirs)
	if len(found.Candidates) == 0 {
		fmt.Fprintln(r.stdout, "No agent repos found.")
	}
	for i, c := range found.Candidates {
		marker := "-"
		if numbered {
			marker = strconv.Itoa(i+1) + ")"
		}
		fmt.Fprintf(r.stdout, "  %s %s  %d agent%s\n", marker, r.show(c.Path), c.Agents, plural(c.Agents, "", "s"))
	}
	if len(found.Unreadable) > 0 {
		fmt.Fprintln(r.stdout, "Could not read (anything under these was not searched):")
		for _, p := range found.Unreadable {
			fmt.Fprintf(r.stdout, "  %s: %v\n", r.show(p.Path), p.Err)
		}
	}
	if found.Truncated {
		fmt.Fprintf(r.stdout, "Stopped after %d directories; pass --search PATH to look somewhere narrower.\n", discover.MaxDirs)
	}
	return found
}

func (r initRun) interactive(sc *bufio.Scanner, cands []discover.Candidate) int {
	// ask's false means stop: end of input, or q at any prompt.
	ask := func(q string) (string, bool) {
		fmt.Fprint(r.stdout, q)
		if !sc.Scan() {
			fmt.Fprintln(r.stdout)
			return "", false
		}
		ans := strings.TrimSpace(sc.Text())
		return ans, ans != "q"
	}
	choices := "a path"
	if len(cands) == 1 {
		choices = "1, or a path"
	} else if len(cands) > 1 {
		choices = fmt.Sprintf("1-%d, or a path", len(cands))
	}

	var repo string
	for repo == "" {
		ans, ok := ask("Repo [" + choices + "; q to quit]: ")
		if !ok {
			return r.aborted()
		}
		if n, err := strconv.Atoi(ans); err == nil {
			if n >= 1 && n <= len(cands) {
				repo = cands[n-1].Path
			} else {
				fmt.Fprintf(r.stdout, "There is no candidate %d.\n", n)
			}
		} else if ans != "" {
			if p, err := checkRepo(ans); err != nil {
				fmt.Fprintf(r.stdout, "%v\n", err)
			} else {
				repo = p
			}
		}
	}

	suggested := filepath.Dir(repo)
	var root string
	for root == "" {
		ans, ok := ask("Default tree for --all and inventory [" + r.show(suggested) + "; q to quit]: ")
		if !ok {
			return r.aborted()
		}
		if ans == "" {
			ans = suggested
		}
		if p, err := checkRoot(ans); err != nil {
			fmt.Fprintf(r.stdout, "%v\n", err)
		} else {
			root = p
		}
	}

	r.preview("Will write", repo, root)
	ans, ok := ask("Write it? [y/N]: ")
	if ans = strings.ToLower(ans); !ok || (ans != "y" && ans != "yes") {
		return r.aborted()
	}
	return r.write(repo, root)
}

func (r initRun) dryRunOrApply(repo, root string, apply bool) int {
	if apply {
		return r.write(repo, root)
	}
	r.preview("Would write", repo, root)
	fmt.Fprintf(r.stdout, "Dry run: nothing written. To write it:\n  %s\n", r.command(repo, root))
	return exitOK
}

// preview prints the exact file a write would produce. Render cannot refuse
// here: repo and root have already passed ParsePath, the only check it makes.
func (r initRun) preview(verb, repo, root string) {
	data, _ := config.Render(config.Config{Repo: repo, Root: root})
	fmt.Fprintf(r.stdout, "%s %s:\n%s", verb, r.show(r.cfgPath), indent(string(data)))
}

func (r initRun) write(repo, root string) int {
	if err := config.Write(r.cfgPath, config.Config{Repo: repo, Root: root}, r.force); err != nil {
		fmt.Fprintf(r.stderr, "agentsync: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(r.stdout, "Wrote %s\nNext, inside a project:\n  agentsync scan --at-risk\n  agentsync link --agent <name> --apply\n", r.show(r.cfgPath))
	return exitOK
}

func (r initRun) aborted() int {
	fmt.Fprintln(r.stderr, "agentsync: init aborted; nothing written.")
	return exitAtRisk
}

// command is the init invocation that writes repo and root, ready to paste.
func (r initRun) command(repo, root string) string {
	cmd := "agentsync init --repo " + r.shellPath(repo) + " --root " + r.shellPath(root) + " --apply"
	if r.force {
		cmd += " --force"
	}
	return cmd
}

// shellPath is show(p) quoted for a POSIX shell, keeping a leading ~/ outside
// the quotes so the shell still expands it.
func (r initRun) shellPath(p string) string {
	s := r.show(p)
	prefix, rest := "", s
	if after, ok := strings.CutPrefix(s, "~/"); ok {
		prefix, rest = "~/", after
	}
	if rest == "" || strings.Trim(rest, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+~@") == "" {
		return s
	}
	return prefix + "'" + strings.ReplaceAll(rest, "'", `'\''`) + "'"
}

func (r initRun) show(p string) string { return displayPath(r.home, p) }

// checkRepo returns the expanded repo path if it names a directory holding
// agents/, the layout every other command expects.
func checkRepo(raw string) (string, error) {
	p, err := checkDir("repo", raw)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(filepath.Join(p, "agents")); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("repo %s has no agents/ directory, so it is not an agent repo", p)
	}
	return p, nil
}

func checkRoot(raw string) (string, error) { return checkDir("root", raw) }

func checkDir(key, raw string) (string, error) {
	p, err := config.ParsePath(key, raw)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("%s: %w", key, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s %s is not a directory", key, p)
	}
	return p, nil
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n  ") + "\n"
}
