// Package git wraps the system git executable for local operations (clone,
// fetch, checkout, remotes, branch, status) and parses/detects remote URLs.
//
// Trove never reimplements the Git protocol. Credentials for HTTPS remotes
// are supplied through git's credential-helper protocol (see CredentialHelper),
// never embedded in URLs or command-line arguments.
package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/redact"
)

// Git runs git commands.
type Git struct {
	// Path to the git binary; defaults to "git" on $PATH.
	Path string
	// Env is appended to the process environment.
	Env []string
	// CredentialHelper, when set, is configured as the only credential helper
	// for commands that talk to remotes (clone/fetch), e.g.
	// "!'/usr/local/bin/trove' auth git-credential --provider gh".
	CredentialHelper string
}

// New returns a Git using the binary on $PATH.
func New() *Git { return &Git{Path: "git"} }

// WithCredentialHelper returns a copy of g that uses helper for remotes.
func (g *Git) WithCredentialHelper(helper string) *Git {
	cp := *g
	cp.CredentialHelper = helper
	return &cp
}

func (g *Git) bin() string {
	if g.Path == "" {
		return "git"
	}
	return g.Path
}

// Available reports whether git can be executed.
func (g *Git) Available() error {
	if _, err := exec.LookPath(g.bin()); err != nil {
		return errs.Wrap(errs.ErrGit, err, "git executable not found; install Git from https://git-scm.com")
	}
	return nil
}

// Version returns the git version string.
func (g *Git) Version(ctx context.Context) (string, error) {
	out, err := g.run(ctx, "", false, "version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "git version")), nil
}

// CloneOptions tune a clone.
type CloneOptions struct {
	Branch string
	Depth  int
	// Origin names the remote (default "origin").
	Origin string
}

// Clone clones url into dir.
func (g *Git) Clone(ctx context.Context, url, dir string, o CloneOptions) error {
	args := []string{"clone", "--quiet"}
	if o.Branch != "" {
		args = append(args, "--branch", o.Branch)
	}
	if o.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(o.Depth))
	}
	if o.Origin != "" {
		args = append(args, "--origin", o.Origin)
	}
	args = append(args, "--", url, dir)
	_, err := g.run(ctx, "", true, args...)
	return err
}

// Fetch fetches refspecs from remote in the repository at dir.
func (g *Git) Fetch(ctx context.Context, dir, remote string, refspecs ...string) error {
	args := append([]string{"fetch", "--quiet", remote}, refspecs...)
	_, err := g.run(ctx, dir, true, args...)
	return err
}

// Checkout switches to branch, creating it from startPoint when non-empty.
func (g *Git) Checkout(ctx context.Context, dir, branch, startPoint string) error {
	args := []string{"checkout", "--quiet"}
	if startPoint != "" {
		args = append(args, "-B", branch, startPoint)
	} else {
		args = append(args, branch)
	}
	_, err := g.run(ctx, dir, false, args...)
	return err
}

// Remote is a configured git remote.
type Remote struct {
	Name     string `json:"name"`
	FetchURL string `json:"fetch_url"`
	PushURL  string `json:"push_url,omitempty"`
}

// Remotes lists remotes of the repository at dir, in `git remote` order.
func (g *Git) Remotes(ctx context.Context, dir string) ([]Remote, error) {
	out, err := g.run(ctx, dir, false, "remote", "-v")
	if err != nil {
		return nil, err
	}
	return ParseRemotes(out), nil
}

// ParseRemotes parses `git remote -v` output.
func ParseRemotes(out string) []Remote {
	var order []string
	byName := map[string]*Remote{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		r, ok := byName[f[0]]
		if !ok {
			r = &Remote{Name: f[0]}
			byName[f[0]] = r
			order = append(order, f[0])
		}
		kind := ""
		if len(f) >= 3 {
			kind = f[2]
		}
		switch kind {
		case "(push)":
			r.PushURL = sanitize(f[1])
		default:
			r.FetchURL = sanitize(f[1])
		}
	}
	out2 := make([]Remote, 0, len(order))
	for _, n := range order {
		r := byName[n]
		if r.PushURL == r.FetchURL {
			r.PushURL = ""
		}
		out2 = append(out2, *r)
	}
	return out2
}

// CurrentBranch returns the checked-out branch ("" when detached).
func (g *Git) CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := g.run(ctx, dir, false, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) && e.Status == 1 {
			return "", nil // detached HEAD
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// TopLevel returns the repository root containing dir.
func (g *Git) TopLevel(ctx context.Context, dir string) (string, error) {
	out, err := g.run(ctx, dir, false, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errs.Wrap(errs.ErrNotFound, err, "not inside a git repository")
	}
	return strings.TrimSpace(out), nil
}

// Status summarizes the working tree.
type Status struct {
	Branch    string `json:"branch"`
	Upstream  string `json:"upstream,omitempty"`
	Ahead     int    `json:"ahead"`
	Behind    int    `json:"behind"`
	Changed   int    `json:"changed"`
	Untracked int    `json:"untracked"`
}

// Clean reports whether there are no local changes.
func (s Status) Clean() bool { return s.Changed == 0 && s.Untracked == 0 }

// Status returns the working tree status of dir.
func (g *Git) Status(ctx context.Context, dir string) (*Status, error) {
	out, err := g.run(ctx, dir, false, "status", "--porcelain=v2", "--branch")
	if err != nil {
		return nil, err
	}
	return ParseStatus(out), nil
}

// ParseStatus parses `git status --porcelain=v2 --branch` output.
func ParseStatus(out string) *Status {
	s := &Status{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			s.Branch = strings.TrimPrefix(line, "# branch.head ")
			if s.Branch == "(detached)" {
				s.Branch = ""
			}
		case strings.HasPrefix(line, "# branch.upstream "):
			s.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			f := strings.Fields(strings.TrimPrefix(line, "# branch.ab "))
			if len(f) == 2 {
				s.Ahead, _ = strconv.Atoi(strings.TrimPrefix(f[0], "+"))
				s.Behind, _ = strconv.Atoi(strings.TrimPrefix(f[1], "-"))
			}
		case strings.HasPrefix(line, "? "):
			s.Untracked++
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "), strings.HasPrefix(line, "u "):
			s.Changed++
		}
	}
	return s
}

// IsRepository reports whether dir is (inside) a git work tree.
func (g *Git) IsRepository(ctx context.Context, dir string) bool {
	out, err := g.run(ctx, dir, false, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// run executes git. When remote is true, the configured credential helper
// is installed for this invocation only (after clearing inherited helpers so
// that trove's credentials are used for trove-initiated operations).
func (g *Git) run(ctx context.Context, dir string, remote bool, args ...string) (string, error) {
	full := []string{}
	if remote && g.CredentialHelper != "" {
		full = append(full, "-c", "credential.helper=", "-c", "credential.helper="+g.CredentialHelper)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, g.bin(), full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), g.Env...)
	// Never let git prompt on the terminal: prompts would corrupt TUIs and
	// hang non-interactive runs.
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		code := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		msg := strings.TrimSpace(redact.String(lastLines(stderr.String(), 4)))
		if msg == "" {
			msg = err.Error()
		}
		return "", &errs.Error{Kind: errs.ErrGit, Op: "git " + args[0], Message: msg, Status: code}
	}
	return stdout.String(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
