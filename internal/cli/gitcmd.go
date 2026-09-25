package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/git"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// ExitCodeError makes Execute exit with Code without rendering an error
// (git already reported it).
type ExitCodeError struct{ Code int }

func (e *ExitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

type gitCmdOptions struct {
	sshKey    string
	chooseKey bool
}

func newGitCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "git [trove options] <git command> [git args...]",
		Short: "Run any git command with the account's SSH key or stored token",
		Long: `Run any git command through the system git, authenticated for the
repository's account: the account's ssh_key for SSH remotes (via
GIT_SSH_COMMAND with IdentitiesOnly=yes) and the token stored by
"trove auth login" for HTTPS remotes (via git's credential helper).

Every git flag is passed through unchanged, and git runs attached to your
terminal: editors, pagers, progress and passphrase prompts work, and git's
exit code is returned.

The account is taken from the remote named in the command (e.g. "push
upstream"), else the current branch's remote, else origin; for "clone", from
the URL. Trove options go before the git command:

  --ssh-key PATH      use this private key (path or name in ~/.ssh)
  --choose-key        pick a key from ~/.ssh (can be remembered)
  -P, --provider NAME use this account
  -y, --yes           confirm force pushes and remote deletions
  --config FILE       configuration file

Force pushes and remote ref deletions (--force, -f, --force-with-lease,
--delete, --mirror, +refspec, :ref) ask for confirmation, or need --yes.`,
		Example: `  trove git push --force origin feature/login
  trove git pull --rebase
  trove git fetch --all --prune
  trove git clone git@github.com:acme/api.git
  trove git --ssh-key id_work push -u origin main
  trove git -C ~/code/api submodule update --init --recursive`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, gitArgs, help, err := parseGitCmdArgs(f, args)
			if err != nil {
				return err
			}
			if help {
				return cmd.Help()
			}
			a, err := f.App()
			if err != nil {
				return err
			}
			return runGitPassthrough(cmd.Context(), a, opts, gitArgs)
		},
	}
}

// parseGitCmdArgs consumes Trove's own leading options; everything from the
// first other argument on belongs to git.
func parseGitCmdArgs(f *Factory, args []string) (gitCmdOptions, []string, bool, error) {
	var o gitCmdOptions
	value := func(i *int, name string) (string, error) {
		a := args[*i]
		if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
			return v, nil
		}
		if *i+1 >= len(args) {
			return "", errs.New(errs.ErrInvalidArgument, "%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		name, _, _ := strings.Cut(a, "=")
		var err error
		switch name {
		case "--ssh-key":
			o.sshKey, err = value(&i, "--ssh-key")
		case "--choose-key":
			o.chooseKey = true
		case "-P", "--provider":
			f.opts.Provider, err = value(&i, "--provider")
		case "--config":
			f.opts.ConfigPath, err = value(&i, "--config")
		case "-y", "--yes":
			f.opts.Yes = true
		case "--non-interactive":
			f.opts.NonInteractive = true
		case "--debug":
			f.opts.Debug = true
		case "-h", "--help":
			if i == len(args)-1 {
				return o, nil, true, nil
			}
			return o, args[i:], false, nil
		default:
			goto done
		}
		if err != nil {
			return o, nil, false, err
		}
	}
done:
	if o.sshKey != "" && o.chooseKey {
		return o, nil, false, errs.New(errs.ErrInvalidArgument, "--ssh-key and --choose-key cannot be combined")
	}
	if i >= len(args) {
		return o, nil, true, nil
	}
	return o, args[i:], false, nil
}

// gitTarget works out which account and remote URL a git command line uses.
func gitTarget(ctx context.Context, a *app.App, inv git.GitInvocation) (alias, remoteURL string) {
	alias = a.Opts.Provider
	detect := func(u string) {
		if alias != "" {
			return
		}
		if d, err := git.Detect(u, a.DetectionAccounts(), a.Config.DefaultProvider); err == nil {
			alias = d.Provider
		}
	}
	if inv.Subcommand == "clone" {
		for _, arg := range inv.Args {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			if _, err := git.ParseRemoteURL(arg); err == nil {
				detect(arg)
				return alias, arg
			}
		}
		return alias, ""
	}
	if !a.Git.IsRepository(ctx, inv.Dir) {
		return alias, ""
	}
	remotes, err := a.Git.Remotes(ctx, inv.Dir)
	if err != nil || len(remotes) == 0 {
		return alias, ""
	}
	byName := map[string]git.Remote{}
	for _, r := range remotes {
		byName[r.Name] = r
	}
	var chosen *git.Remote
	for _, arg := range inv.Args {
		if r, ok := byName[arg]; ok {
			chosen = &r
			break
		}
	}
	if chosen == nil {
		if br, _ := a.Git.CurrentBranch(ctx, inv.Dir); br != "" {
			if r, ok := byName[a.Git.BranchRemote(ctx, inv.Dir, br)]; ok {
				chosen = &r
			}
		}
	}
	if chosen == nil {
		if r, ok := byName["origin"]; ok {
			chosen = &r
		} else {
			chosen = &remotes[0]
		}
	}
	// The fetch URL identifies the account; pushes use the push URL.
	detect(chosen.FetchURL)
	remoteURL = chosen.FetchURL
	if inv.Subcommand == "push" && chosen.PushURL != "" {
		remoteURL = chosen.PushURL
	}
	return alias, remoteURL
}

func runGitPassthrough(ctx context.Context, a *app.App, o gitCmdOptions, gitArgs []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	inv := git.ParseInvocation(cwd, gitArgs)
	alias, remoteURL := gitTarget(ctx, a, inv)

	g := a.Git
	key, err := sshKeyFor(ctx, a, alias, o.sshKey, o.chooseKey)
	if err != nil {
		if o.sshKey != "" || o.chooseKey {
			return err
		}
		// A broken saved ssh_key must not block local commands (status, log).
		a.Out.Warn("ignoring the saved ssh_key for %s: %v", alias, err)
		key = ""
	}
	g = g.WithSSHKey(key)
	usedHelper := false
	if isHTTPRemote(remoteURL) && alias != "" {
		if p, err := a.Open(alias); err == nil {
			if _, ok := p.(forge.GitAuthenticator); ok {
				g = g.WithCredentialHelper(credentialHelper(a, alias))
				usedHelper = true
			}
		}
	}
	if inv.Subcommand == "push" && git.IsDestructivePush(inv.Args) {
		if err := confirm(a, fmt.Sprintf("Run \"git %s\"? This can overwrite or delete commits on the remote.", strings.Join(gitArgs, " "))); err != nil {
			return err
		}
	}
	if a.Out.Human() && (key != "" || usedHelper) {
		th := a.IO.ErrTheme()
		via := []string{}
		if alias != "" {
			via = append(via, alias)
		}
		if key != "" {
			via = append(via, "SSH key "+homeRelative(key))
		}
		if usedHelper {
			via = append(via, "stored token")
		}
		fmt.Fprintf(a.IO.Err, "%s %s\n", th.Muted.Render(terminal.SymInfo), th.Muted.Render("git "+inv.Subcommand+" via "+strings.Join(via, " · ")))
	}
	code, err := g.Passthrough(git.PassthroughIO{Dir: cwd, Stdin: a.IO.In, Stdout: a.IO.Out, Stderr: a.IO.Err}, gitArgs...)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitCodeError{Code: code}
	}
	return nil
}
