package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/git"
	"github.com/SurajMazar/trove-cli/internal/tui"
)

// --- SSH key selection ------------------------------------------------------------

// homeRelative renders a path under $HOME as ~/..., keeping configs portable.
func homeRelative(p string) string {
	if p == "" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return p
}

// chooseSSHKey lets the user pick a key from ~/.ssh. With allowDefault, the
// first choice keeps git's own SSH configuration and returns "".
func chooseSSHKey(ctx context.Context, a *app.App, question string, allowDefault bool) (string, error) {
	keys, err := git.ListSSHKeys(git.SSHDir())
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		if allowDefault {
			return "", nil
		}
		return "", &errs.Error{Kind: errs.ErrNotFound, Message: "no SSH keys found in " + git.SSHDir(), Hint: "ssh-keygen -t ed25519"}
	}
	var choices []tui.Choice
	if allowDefault {
		choices = append(choices, tui.Choice{Label: "Use git's default", Description: "ssh-agent, ~/.ssh/config or core.sshCommand", Value: ""})
	}
	for _, k := range keys {
		choices = append(choices, tui.Choice{Label: k.Name, Description: joinNonEmpty(" · ", k.Type, k.Comment), Value: k.Path})
	}
	c, err := tui.Choose(ctx, a.IO, question, choices)
	if err != nil {
		return "", err
	}
	if c.Value == "" {
		return "", nil
	}
	return git.ResolveSSHKey(c.Value)
}

// sshKeyFor resolves the SSH key for an account: explicit flag, interactive
// choice (offering to remember it in the config), then the saved ssh_key.
func sshKeyFor(ctx context.Context, a *app.App, alias, flagKey string, choose bool) (string, error) {
	if flagKey != "" {
		return git.ResolveSSHKey(flagKey)
	}
	if choose {
		if !a.IO.Interactive {
			return "", errs.New(errs.ErrInteractionRequired, "--choose-key needs a terminal; use --ssh-key PATH")
		}
		key, err := chooseSSHKey(ctx, a, "SSH key:", false)
		if err != nil {
			return "", err
		}
		if pc, err := a.Config.Provider(alias); err == nil && alias != "" && pc.SSHKey != homeRelative(key) {
			if ok, err := a.IO.Confirm(fmt.Sprintf("Remember %s for %s?", homeRelative(key), alias)); err == nil && ok {
				pc.SSHKey = homeRelative(key)
				if err := a.Config.Save(); err != nil {
					return "", err
				}
				a.Out.Success("Saved ssh_key for %s", alias)
			}
		}
		return key, nil
	}
	if alias == "" {
		return "", nil
	}
	pc, err := a.Config.Provider(alias)
	if err != nil || pc.SSHKey == "" {
		return "", nil
	}
	key, err := git.ResolveSSHKey(pc.SSHKey)
	if err != nil {
		return "", errs.WithProvider(err, alias)
	}
	return key, nil
}

// remoteGit returns a Git configured with credentials for remoteURL: the
// account's SSH key for SSH remotes, trove's credential helper for HTTPS.
func remoteGit(ctx context.Context, a *app.App, remoteURL, alias, flagKey string, choose bool) (*git.Git, error) {
	g := a.Git
	if isHTTPRemote(remoteURL) {
		if alias != "" {
			if p, err := a.Open(alias); err == nil {
				if _, ok := p.(forge.GitAuthenticator); ok {
					g = g.WithCredentialHelper(credentialHelper(a, alias))
				}
			}
		}
		return g, nil
	}
	key, err := sshKeyFor(ctx, a, alias, flagKey, choose)
	if err != nil {
		return nil, err
	}
	return g.WithSSHKey(key), nil
}

func isHTTPRemote(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

// --- push -----------------------------------------------------------------------

type pushFlags struct {
	sshKey         string
	chooseKey      bool
	setUpstream    bool
	forceWithLease bool
	force          bool
	extra          []string // git push flags after "--"
	tags           bool
	pr             bool
	prReq          forge.CreatePullRequestRequest
}

func (fl *pushFlags) register(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVar(&fl.sshKey, "ssh-key", "", "SSH private key for this push (default: the account's ssh_key)")
	fs.BoolVar(&fl.chooseKey, "choose-key", false, "pick the SSH key from ~/.ssh (and optionally remember it)")
	fs.BoolVarP(&fl.setUpstream, "set-upstream", "u", false, "set the upstream branch (automatic when the branch has none)")
	fs.BoolVar(&fl.forceWithLease, "force-with-lease", false, "overwrite the remote branch if it has not changed since your last fetch")
	fs.BoolVarP(&fl.force, "force", "f", false, "overwrite the remote branch unconditionally (asks for confirmation)")
	fs.BoolVar(&fl.tags, "tags", false, "also push annotated tags reachable from the pushed commits")
	fs.BoolVar(&fl.pr, "pr", false, "open a pull/merge request after pushing")
	fs.StringVar(&fl.prReq.Title, "title", "", "pull request title (with --pr; default: last commit subject)")
	fs.StringVar(&fl.prReq.Body, "body", "", "pull request description (with --pr)")
	fs.StringVar(&fl.prReq.TargetBranch, "base", "", "pull request target branch (with --pr; default: repository default branch)")
	fs.BoolVar(&fl.prReq.Draft, "draft", false, "create the pull request as a draft (with --pr)")
	cmd.MarkFlagsMutuallyExclusive("ssh-key", "choose-key")
	cmd.MarkFlagsMutuallyExclusive("force", "force-with-lease")
}

func newPushCmd(f *Factory) *cobra.Command {
	var fl pushFlags
	cmd := &cobra.Command{
		Use:   "push [remote] [branch] [-- git push flags...]",
		Short: "Push the current branch (with the account's SSH key or stored token)",
		Long: `Push a branch using the system git, authenticated for the remote's account:

  SSH remotes    use the account's ssh_key (set during "trove provider add",
                 or with --ssh-key / --choose-key, which can remember it)
  HTTPS remotes  use the token stored by "trove auth login" via git's
                 credential helper (never in URLs or arguments)

The upstream is set automatically the first time a branch is pushed.
Any other git push flag can follow "--". For arbitrary git commands with the
same credentials, see "trove git".`,
		Example: `  trove push
  trove push --pr --draft
  trove push origin feature/login --ssh-key ~/.ssh/id_work
  trove push --choose-key
  trove push --force --yes
  trove push -- --no-verify --atomic --push-option=ci.skip`,
		Args: func(cmd *cobra.Command, args []string) error {
			if n := positionalBeforeDash(cmd, args); n > 2 {
				return fmt.Errorf("accepts at most 2 arg(s) before \"--\", received %d", n)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				fl.extra = args[dash:]
				args = args[:dash]
			}
			remote, branch := "", ""
			if len(args) > 0 {
				remote = args[0]
			}
			if len(args) > 1 {
				branch = args[1]
			}
			return runPush(cmd.Context(), a, remote, branch, fl)
		},
	}
	fl.register(cmd)
	return cmd
}

type pushResult struct {
	Remote      string              `json:"remote"`
	Branch      string              `json:"branch"`
	Provider    string              `json:"provider,omitempty"`
	Repository  string              `json:"repository,omitempty"`
	UpstreamSet bool                `json:"upstream_set"`
	SSHKey      string              `json:"ssh_key,omitempty"`
	Messages    []string            `json:"remote_messages,omitempty"`
	PullRequest *domain.PullRequest `json:"pull_request,omitempty"`
}

func runPush(ctx context.Context, a *app.App, remote, branch string, fl pushFlags) error {
	dir, err := currentCheckout(ctx, a)
	if err != nil {
		return err
	}
	if branch == "" {
		if branch, err = a.Git.CurrentBranch(ctx, dir); err != nil {
			return err
		}
		if branch == "" {
			return errs.New(errs.ErrInvalidArgument, "HEAD is detached; pass the branch to push")
		}
	}
	if remote == "" {
		remote = firstNonEmpty(a.Git.BranchRemote(ctx, dir, branch), "origin")
	}
	remotes, err := a.Git.Remotes(ctx, dir)
	if err != nil {
		return err
	}
	// The fetch URL identifies the repository and account; the push URL (if
	// different) decides which credentials the push itself needs.
	var remoteURL, identityURL string
	for _, r := range remotes {
		if r.Name == remote {
			remoteURL = firstNonEmpty(r.PushURL, r.FetchURL)
			identityURL = firstNonEmpty(r.FetchURL, r.PushURL)
		}
	}
	if remoteURL == "" {
		return &errs.Error{Kind: errs.ErrNotFound, Message: fmt.Sprintf("remote %q not found", remote), Hint: "git remote -v"}
	}
	res := pushResult{Remote: remote, Branch: branch}
	var det git.Detection
	if d, err := git.Detect(identityURL, a.DetectionAccounts(), a.Config.DefaultProvider); err == nil {
		det = d
		res.Provider, res.Repository = d.Provider, d.FullName()
	}
	g, err := remoteGit(ctx, a, remoteURL, det.Provider, fl.sshKey, fl.chooseKey)
	if err != nil {
		return err
	}
	res.SSHKey = homeRelative(g.SSHKey)
	for _, x := range fl.extra {
		if !strings.HasPrefix(x, "-") {
			return errs.New(errs.ErrInvalidArgument, "%q after \"--\" is not a flag; pass the remote and branch before \"--\" (or use trove git push ...)", x)
		}
	}
	switch {
	case fl.force:
		if err := confirm(a, fmt.Sprintf("Force-push %s to %s? Remote commits not in your branch will be lost.", branch, remote)); err != nil {
			return err
		}
	case fl.forceWithLease:
		if err := confirm(a, fmt.Sprintf("Force-push %s to %s? Remote commits not in your branch will be replaced.", branch, remote)); err != nil {
			return err
		}
	case git.IsDestructivePush(fl.extra):
		if err := confirm(a, fmt.Sprintf("Push %s to %s with %s? This can overwrite or delete remote commits.", branch, remote, strings.Join(fl.extra, " "))); err != nil {
			return err
		}
	}
	res.UpstreamSet = fl.setUpstream || a.Git.Upstream(ctx, dir, branch) == ""
	label := fmt.Sprintf("Pushing %s to %s...", branch, remote)
	if res.SSHKey != "" {
		label = fmt.Sprintf("Pushing %s to %s with %s...", branch, remote, res.SSHKey)
	}
	report, err := spin(ctx, a, label, func(ctx context.Context) (string, error) {
		return g.Push(ctx, dir, remote, git.PushOptions{SetUpstream: res.UpstreamSet, ForceWithLease: fl.forceWithLease,
			Force: fl.force, Tags: fl.tags, Extra: fl.extra}, branch)
	})
	if err != nil {
		return pushError(err, g.SSHKey != "" || !isHTTPRemote(remoteURL), det.Provider)
	}
	for _, line := range strings.Split(report, "\n") {
		if msg := strings.TrimSpace(strings.TrimPrefix(line, "remote:")); strings.HasPrefix(line, "remote:") && msg != "" {
			res.Messages = append(res.Messages, msg)
		}
	}
	a.Out.Success("Pushed %s to %s", branch, remote)
	if fl.pr {
		if det.Provider == "" {
			return &errs.Error{Kind: errs.ErrProviderNotFound, Message: fmt.Sprintf("no configured provider matches %s, so no pull request was opened", det.Host),
				Hint: "trove provider add"}
		}
		p, err := a.Open(det.Provider)
		if err != nil {
			return err
		}
		req := fl.prReq
		req.SourceBranch = branch
		ref := domain.RepositoryRef{Provider: det.Provider, Namespace: det.Namespace, Name: det.Name}
		// createPR prints the pull request itself (including JSON output).
		pr, err := createPR(ctx, a, p, ref, req)
		res.PullRequest = pr
		return err
	}
	return a.Out.Result(res, func() []string { return []string{remote + "/" + branch} }, func() error {
		th := a.Out.Theme()
		for _, m := range res.Messages {
			fmt.Fprintf(a.IO.Out, "  %s\n", th.Muted.Render(m))
		}
		return nil
	})
}

func pushError(err error, ssh bool, alias string) error {
	msg := err.Error()
	switch {
	case ssh && strings.Contains(msg, "Permission denied (publickey)"):
		return &errs.Error{Kind: errs.ErrAuthenticationFailed, Provider: alias, Cause: err,
			Message: "the SSH key was rejected by the server", Hint: "trove push --choose-key  (or add the key: trove key ssh add)"}
	case strings.Contains(msg, "[rejected]") && (strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "fetch first")):
		return &errs.Error{Kind: errs.ErrConflict, Cause: err, Message: "the remote branch has commits you don't have",
			Hint: "git pull --rebase, then trove push"}
	}
	return err
}

// --- commit ---------------------------------------------------------------------

type commitResult struct {
	Commit  string      `json:"commit"`
	Message string      `json:"message"`
	Files   []string    `json:"files"`
	Push    *pushResult `json:"push,omitempty"`
}

func newCommitCmd(f *Factory) *cobra.Command {
	var message string
	var all, amend, push bool
	var pfl pushFlags
	cmd := &cobra.Command{
		Use:   "commit [path...]",
		Short: "Commit changes (pick files interactively) and optionally push",
		Long: `Commit with the system git (your hooks, signing and identity still apply).

What gets committed:
  paths given    those paths are staged and committed
  --all / -a     every change, including new files
  otherwise      what is already staged; if nothing is, a picker lists the
                 changed files (space to toggle, a/n all/none, enter commit)`,
		Example: `  trove commit -m "Fix login redirect"
  trove commit -a -m "Update docs" --push
  trove commit src/api.go -m "Retry on 503" --push --pr`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			dir, err := currentCheckout(ctx, a)
			if err != nil {
				return err
			}
			changes, err := a.Git.Changes(ctx, dir)
			if err != nil {
				return err
			}
			if len(changes) == 0 && !amend {
				a.Out.Println("Nothing to commit, working tree clean.")
				return a.Out.Result(commitResult{Files: []string{}}, nil, func() error { return nil })
			}
			switch {
			case len(args) > 0:
				if err := a.Git.Add(ctx, dir, args...); err != nil {
					return err
				}
			case all:
				if err := a.Git.Add(ctx, dir); err != nil {
					return err
				}
			case !anyStaged(changes) && !amend:
				if !a.IO.Interactive {
					return &errs.Error{Kind: errs.ErrInvalidArgument, Message: "nothing is staged",
						Hint: "trove commit -a -m \"message\"  (or pass paths, or git add first)"}
				}
				picked, err := pickChanges(ctx, a, changes)
				if err != nil {
					return err
				}
				if err := a.Git.Add(ctx, dir, picked...); err != nil {
					return err
				}
			}
			if changes, err = a.Git.Changes(ctx, dir); err != nil {
				return err
			}
			var files []string
			for _, c := range changes {
				if c.Staged() {
					files = append(files, c.Path)
				}
			}
			if len(files) == 0 && !amend {
				return errs.New(errs.ErrInvalidArgument, "nothing to commit: the selected paths have no changes")
			}
			if a.Out.Human() {
				th := a.IO.ErrTheme()
				fmt.Fprintf(a.IO.Err, "%s\n", th.Heading.Render(fmt.Sprintf("Committing %d file(s)", len(files))))
				for _, c := range changes {
					if c.Staged() {
						fmt.Fprintf(a.IO.Err, "  %s %s\n", th.Muted.Render(fmt.Sprintf("%-9s", c.Describe())), c.Path)
					}
				}
			}
			if message == "" {
				if !a.IO.Interactive {
					return errs.New(errs.ErrInvalidArgument, "-m/--message is required")
				}
				if message, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: "Commit message:", Validate: nonEmpty}); err != nil {
					return err
				}
			}
			sha, err := a.Git.Commit(ctx, dir, message, git.CommitOptions{Amend: amend})
			if err != nil {
				return err
			}
			subject, _, _ := strings.Cut(message, "\n")
			a.Out.Success("Committed %s %s", sha, subject)
			if push {
				return runPush(ctx, a, "", "", pfl)
			}
			return a.Out.Result(commitResult{Commit: sha, Message: message, Files: files}, func() []string { return []string{sha} }, func() error { return nil })
		},
	}
	fs := cmd.Flags()
	fs.StringVarP(&message, "message", "m", "", "commit message (prompted when omitted)")
	fs.BoolVarP(&all, "all", "a", false, "stage every change, including new files")
	fs.BoolVar(&amend, "amend", false, "amend the previous commit")
	fs.BoolVar(&push, "push", false, "push after committing (push flags below apply)")
	pfl.register(cmd)
	return cmd
}

func anyStaged(cs []git.Change) bool {
	for _, c := range cs {
		if c.Staged() {
			return true
		}
	}
	return false
}

func pickChanges(ctx context.Context, a *app.App, cs []git.Change) ([]string, error) {
	items := make([]tui.Item, len(cs))
	pre := map[string]bool{}
	for i, c := range cs {
		items[i] = tui.Item{ID: c.Path, Title: c.Path, Badges: []string{c.Describe()}}
		pre[c.Path] = true
	}
	res, err := tui.RunList(ctx, a.IO, tui.ListOptions{Title: "Select files to commit", Noun: "files", Multi: true,
		Items: items, Preselected: pre, ConfirmLabel: "commit"})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Selected))
	for _, it := range res.Selected {
		out = append(out, it.ID)
	}
	return out, nil
}

// positionalBeforeDash counts positional arguments before a "--" separator.
func positionalBeforeDash(cmd *cobra.Command, args []string) int {
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		return dash
	}
	return len(args)
}
