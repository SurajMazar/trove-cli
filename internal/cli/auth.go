package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/git"
	"github.com/SurajMazar/trove-cli/internal/output"
	"github.com/SurajMazar/trove-cli/internal/terminal"
	"github.com/SurajMazar/trove-cli/internal/tui"
)

func newAuthCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Log in, log out and inspect credentials",
		Long: `Authenticate provider accounts. Authentication flows are provider specific
(GitHub PAT/OAuth device flow/GitHub App, GitLab PAT/OAuth, Bitbucket API
token/access token/OAuth consumer). Credentials are stored in the configured
secret provider and never written to the configuration file.

In CI, set TROVE_TOKEN (selected provider) or TROVE_TOKEN_<ALIAS>; environment
credentials are used as-is and never persisted.`,
	}
	cmd.AddCommand(newAuthLoginCmd(f), newAuthLogoutCmd(f), newAuthStatusCmd(f), newAuthRefreshCmd(f), newAuthGitCredentialCmd(f))
	return cmd
}

type loginFlags struct {
	withToken      bool
	method         string
	username       string
	clientID       string
	scopes         []string
	privateKeyFile string
}

func newAuthLoginCmd(f *Factory) *cobra.Command {
	var fl loginFlags
	cmd := &cobra.Command{
		Use:   "login [alias]",
		Short: "Authenticate a provider account",
		Long: `Authenticate a provider account and store the credential in its secret provider.

Examples:
  trove auth login github-personal                      # interactive
  echo "$TOKEN" | trove auth login gitlab-work --with-token
  trove auth login gh --method oauth                    # device flow (needs auth.client_id)
  trove auth login gh-app --method app --private-key-file app.pem`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProviderArgs(f),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			explicit := ""
			if len(args) == 1 {
				explicit = args[0]
			}
			alias, err := a.ProviderName(explicit)
			if err != nil {
				return err
			}
			return authLogin(cmd.Context(), a, alias, fl)
		},
	}
	fs := cmd.Flags()
	fs.BoolVar(&fl.withToken, "with-token", false, "read the token (or private key / client secret) from standard input")
	fs.StringVar(&fl.method, "method", "", "authentication method (default: the account's auth.type)")
	fs.StringVar(&fl.username, "username", "", "username/email for basic authentication")
	fs.StringVar(&fl.clientID, "client-id", "", "OAuth client ID")
	fs.StringSliceVar(&fl.scopes, "scopes", nil, "OAuth scopes to request")
	fs.StringVar(&fl.privateKeyFile, "private-key-file", "", "GitHub App private key (PEM) file")
	return cmd
}

type devicePrompter struct {
	tio *terminal.IO
}

func (p devicePrompter) DeviceCode(uri, code string, expires time.Duration) {
	th := p.tio.ErrTheme()
	fmt.Fprintf(p.tio.Err, "\n%s One-time code: %s\n", th.Warning.Render("!"), th.Bold.Render(code))
	fmt.Fprintf(p.tio.Err, "  Open %s and enter the code", th.Accent.Render(uri))
	if expires > 0 {
		fmt.Fprintf(p.tio.Err, " %s", th.Muted.Render(fmt.Sprintf("(expires in %s)", expires.Round(time.Minute))))
	}
	fmt.Fprintf(p.tio.Err, "\n  %s\n", th.Muted.Render("Waiting for authorization..."))
}

func (p devicePrompter) Info(msg string) {
	fmt.Fprintf(p.tio.Err, "%s %s\n", p.tio.ErrTheme().Muted.Render(terminal.SymInfo), msg)
}

func authLogin(ctx context.Context, a *app.App, alias string, fl loginFlags) error {
	pc, err := a.Config.Provider(alias)
	if err != nil {
		return err
	}
	d, err := a.Driver(pc)
	if err != nil {
		return err
	}
	method := auth.Method(fl.method)
	if method == "" {
		method = auth.Method(pc.Auth.Type)
	}
	if method == "" {
		method = d.AuthMethods()[0]
	}
	ok := false
	for _, m := range d.AuthMethods() {
		ok = ok || m == method
	}
	if !ok {
		return errs.New(errs.ErrInvalidArgument, "%s does not support the %q method", d.DisplayName(), method)
	}
	ref, err := a.SecretRef(alias, pc)
	if err != nil {
		return err
	}
	if ref.Provider == "env" {
		return &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: alias,
			Message: "this account reads its credential from an environment variable, which trove never writes",
			Hint:    fmt.Sprintf("export %s=... or change auth.secret_ref", ref.Key)}
	}
	p, err := a.OpenOther(alias)
	if err != nil {
		return err
	}
	authn, err := asAuthenticator(p)
	if err != nil {
		return err
	}
	req := auth.Request{
		Method: method, Username: firstNonEmpty(fl.username, pc.Auth.Username),
		ClientID: firstNonEmpty(fl.clientID, pc.Auth.ClientID), Scopes: pc.Auth.Scopes,
		AppID: pc.Auth.AppID, InstallationID: pc.Auth.InstallationID,
		Prompter: devicePrompter{a.IO},
	}
	if len(fl.scopes) > 0 {
		req.Scopes = fl.scopes
	}
	readSecret := func(label string) (string, error) {
		if fl.withToken {
			return a.IO.ReadAllIn(1 << 20)
		}
		return tui.Input(ctx, a.IO, tui.InputOptions{Label: label, Secret: true, Validate: func(s string) error {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("value is required")
			}
			return nil
		}})
	}
	nonInteractiveHint := func() error {
		return &errs.Error{Kind: errs.ErrInteractionRequired, Provider: alias,
			Message: "a credential is required but the session is non-interactive",
			Hint:    fmt.Sprintf("echo \"$TOKEN\" | trove auth login %s --with-token", alias)}
	}
	switch method {
	case auth.MethodApp:
		switch {
		case fl.privateKeyFile != "":
			b, err := os.ReadFile(fl.privateKeyFile)
			if err != nil {
				return errs.Wrap(errs.ErrInvalidArgument, err, "read private key")
			}
			req.Token = string(b)
		case fl.withToken:
			if req.Token, err = a.IO.ReadAllIn(1 << 20); err != nil {
				return err
			}
		default:
			return errs.New(errs.ErrInvalidArgument, "GitHub App login needs --private-key-file or --with-token (PEM on stdin)")
		}
	case auth.MethodOAuth:
		if pc.Type == "bitbucket" {
			// Bitbucket OAuth consumers use the client credentials grant.
			// Without a consumer key, accept a pre-issued OAuth access token.
			if req.ClientID == "" && a.IO.Interactive && !fl.withToken {
				if req.ClientID, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: "OAuth consumer key (leave empty to paste an access token):"}); err != nil {
					return err
				}
			}
			if !fl.withToken && !a.IO.Interactive {
				return nonInteractiveHint()
			}
			if req.ClientID == "" {
				if req.Token, err = readSecret("Paste an OAuth access token:"); err != nil {
					return err
				}
			} else if req.ClientSecret, err = readSecret("OAuth consumer secret:"); err != nil {
				return err
			}
		} else if req.ClientID != "" && !fl.withToken && a.Opts.NonInteractive {
			// The device flow waits for a person in a browser; never start it
			// when the caller asked for a non-interactive run.
			return &errs.Error{Kind: errs.ErrInteractionRequired, Provider: alias,
				Message: "the OAuth device flow needs a person to authorize it in a browser",
				Hint:    fmt.Sprintf("echo \"$TOKEN\" | trove auth login %s --method oauth --with-token", alias)}
		} else if req.ClientID == "" || fl.withToken {
			// No OAuth application configured (or --with-token): accept a
			// pre-issued OAuth token.
			if !fl.withToken && !a.IO.Interactive {
				return nonInteractiveHint()
			}
			if req.Token, err = readSecret("Paste an OAuth access token:"); err != nil {
				return err
			}
		}
	default:
		if !fl.withToken && !a.IO.Interactive {
			return nonInteractiveHint()
		}
		if method == auth.MethodBasic && req.Username == "" {
			if !a.IO.Interactive {
				return errs.New(errs.ErrInvalidArgument, "--username is required for basic authentication")
			}
			if req.Username, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: "Username / email:"}); err != nil {
				return err
			}
		}
		if req.Token, err = readSecret(fmt.Sprintf("Paste your %s:", strings.ToLower(methodLabel(method)))); err != nil {
			return err
		}
	}
	var res *auth.Result
	if req.Token == "" && method == auth.MethodOAuth && pc.Type != "bitbucket" {
		res, err = authn.Login(ctx, req) // device flow prints its own prompts
	} else {
		res, err = spin(ctx, a, "Verifying credential...", func(ctx context.Context) (*auth.Result, error) { return authn.Login(ctx, req) })
	}
	if err != nil {
		return errs.WithProvider(err, alias)
	}
	if err := a.Secrets.Store(ctx, ref, res.Credential.Encode()); err != nil {
		return errs.WithProvider(err, alias)
	}
	pc.Auth.Type = string(method)
	if pc.Auth.SecretRef == "" {
		pc.Auth.SecretRef = ref.String()
	}
	if req.Username != "" && method == auth.MethodBasic {
		pc.Auth.Username = req.Username
	}
	if req.ClientID != "" && method == auth.MethodOAuth {
		pc.Auth.ClientID = req.ClientID
	}
	if err := a.Config.Save(); err != nil {
		return err
	}
	_ = a.Cache.Invalidate()
	user := res.User
	if user != "" && !strings.HasPrefix(user, "@") {
		user = "@" + user
	}
	a.Out.Success("Logged in to %s as %s", a.Label(alias), firstNonEmpty(user, "(token owner)"))
	a.Out.Info("Credential stored in %s", ref.Provider)
	return a.Out.Result(map[string]string{"provider": alias, "user": res.User, "method": string(method), "secret_ref": ref.String()},
		func() []string { return []string{alias} }, func() error { return nil })
}

func asAuthenticator(p forge.Provider) (forge.Authenticator, error) {
	authn, ok := p.(forge.Authenticator)
	if !ok {
		return nil, errs.Unsupported(p.Metadata().Name, "authentication")
	}
	return authn, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func newAuthLogoutCmd(f *Factory) *cobra.Command {
	var revoke bool
	cmd := &cobra.Command{
		Use:               "logout [alias]",
		Short:             "Remove a stored credential",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProviderArgs(f),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			explicit := ""
			if len(args) == 1 {
				explicit = args[0]
			}
			alias, err := a.ProviderName(explicit)
			if err != nil {
				return err
			}
			pc := a.Config.Providers[alias]
			_, src, err := a.Account(alias, true)
			if err != nil {
				return err
			}
			if src.Kind == "env" {
				a.Out.Warn("%s is set in the environment; unset it to stop using that credential", src.Env)
			}
			if revoke {
				p, err := a.OpenOther(alias)
				if err != nil {
					return err
				}
				rv, ok := p.(forge.Revoker)
				if !ok {
					return errs.Unsupported(alias, "revoking credentials")
				}
				if err := confirm(a, fmt.Sprintf("Revoke the %s credential on the server? This cannot be undone.", alias)); err != nil {
					return err
				}
				if err := rv.RevokeCredential(cmd.Context()); err != nil {
					return errs.WithProvider(err, alias)
				}
				a.Out.Success("Revoked the credential on %s", p.Metadata().DisplayName)
			}
			ref, err := a.SecretRef(alias, pc)
			if err != nil {
				return err
			}
			if ref.Provider != "env" {
				if err := a.Secrets.Remove(cmd.Context(), ref); err != nil {
					return errs.WithProvider(err, alias)
				}
			}
			_ = a.Cache.Invalidate()
			a.Out.Success("Logged out of %s", alias)
			return a.Out.Result(map[string]string{"provider": alias, "status": "logged_out"}, func() []string { return []string{alias} }, func() error { return nil })
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "also revoke the credential server-side where the provider supports it")
	return cmd
}

type authStatusRow struct {
	Provider string       `json:"provider"`
	Type     string       `json:"type"`
	Host     string       `json:"host"`
	Source   string       `json:"source"`
	Status   *auth.Status `json:"status,omitempty"`
	Error    string       `json:"error,omitempty"`
	Hint     string       `json:"hint,omitempty"`
}

func newAuthStatusCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "status [alias]",
		Short:             "Show authentication status for provider accounts",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProviderArgs(f),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			names := a.Config.ProviderNames()
			single := len(args) == 1 || a.Opts.Provider != ""
			if single {
				explicit := ""
				if len(args) == 1 {
					explicit = args[0]
				}
				n, err := a.ProviderName(explicit)
				if err != nil {
					return err
				}
				names = []string{n}
			}
			if len(names) == 0 {
				return &errs.Error{Kind: errs.ErrProviderNotFound, Message: "no providers are configured", Hint: "trove provider add"}
			}
			rows, err := spin(cmd.Context(), a, "Checking credentials...", func(ctx context.Context) ([]authStatusRow, error) {
				return collectAuthStatus(ctx, a, names), nil
			})
			if err != nil {
				return err
			}
			if err := a.Out.Result(rows, func() []string {
				var out []string
				for _, r := range rows {
					if r.Status != nil && r.Status.Authenticated {
						out = append(out, r.Provider)
					}
				}
				return out
			}, func() error { renderAuthStatus(a, rows); return nil }); err != nil {
				return err
			}
			if single && (rows[0].Status == nil || !rows[0].Status.Authenticated) {
				return &errs.Error{Kind: errs.ErrNotAuthenticated, Provider: rows[0].Provider,
					Message: "not authenticated", Hint: "trove auth login " + rows[0].Provider, Status: -1}
			}
			return nil
		},
	}
}

func collectAuthStatus(ctx context.Context, a *app.App, names []string) []authStatusRow {
	rows := make([]authStatusRow, len(names))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pc := a.Config.Providers[n]
			row := authStatusRow{Provider: n, Type: pc.Type, Host: pc.Host}
			selected := n == a.Opts.Provider || (a.Opts.Provider == "" && n == a.Config.DefaultProvider)
			_, src, err := a.Account(n, selected)
			if err == nil {
				row.Source = src.Kind
				if src.Kind == "env" {
					row.Source = "env:" + src.Env
				} else {
					row.Source = src.Ref.Provider
				}
			}
			var p forge.Provider
			if selected {
				p, err = a.Open(n)
			} else {
				p, err = a.OpenOther(n)
			}
			if err == nil {
				var authn forge.Authenticator
				if authn, err = asAuthenticator(p); err == nil {
					cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
					row.Status, err = authn.AuthStatus(cctx)
					cancel()
				}
			}
			if err != nil {
				row.Error = err.Error()
				row.Hint = errs.HintOf(err)
				if errors.Is(err, errs.ErrNotAuthenticated) || errors.Is(err, errs.ErrAuthenticationFailed) {
					row.Hint = "trove auth login " + n
				}
			}
			rows[i] = row
		}()
	}
	wg.Wait()
	return rows
}

func renderAuthStatus(a *app.App, rows []authStatusRow) {
	th := a.Out.Theme()
	for i, r := range rows {
		if i > 0 {
			fmt.Fprintln(a.IO.Out)
		}
		fmt.Fprintf(a.IO.Out, "%s %s\n", th.Provider.Render(terminal.SymProvider+" "+a.Label(r.Provider)), th.Muted.Render(r.Type+" · "+r.Host))
		switch {
		case r.Status != nil && r.Status.Authenticated:
			user := r.Status.User
			if user != "" && !strings.HasPrefix(user, "@") {
				user = "@" + user
			}
			fmt.Fprintf(a.IO.Out, "  %s Logged in as %s\n", th.Success.Render(terminal.SymSuccess), th.Bold.Render(firstNonEmpty(user, "(token)")))
			kv := []output.KV{{Key: "Method", Value: string(r.Status.Method)}, {Key: "Source", Value: r.Source}}
			if len(r.Status.Scopes) > 0 {
				kv = append(kv, output.KV{Key: "Scopes", Value: strings.Join(r.Status.Scopes, ", ")})
			}
			if !r.Status.ExpiresAt.IsZero() {
				exp := r.Status.ExpiresAt.Format("2006-01-02 15:04 MST")
				if time.Until(r.Status.ExpiresAt) < 7*24*time.Hour {
					exp = th.Warning.Render(exp + " (" + output.RelTime(r.Status.ExpiresAt) + ")")
				}
				kv = append(kv, output.KV{Key: "Expires", Value: exp})
			}
			if r.Status.Detail != "" {
				kv = append(kv, output.KV{Key: "Detail", Value: r.Status.Detail})
			}
			for _, x := range kv {
				if x.Value != "" {
					fmt.Fprintf(a.IO.Out, "    %s %s\n", th.Muted.Render(fmt.Sprintf("%-8s", x.Key)), x.Value)
				}
			}
		case r.Status != nil:
			fmt.Fprintf(a.IO.Out, "  %s Not logged in %s\n", th.Warning.Render(terminal.SymWarning), th.Muted.Render(r.Status.Detail))
			fmt.Fprintf(a.IO.Out, "    %s trove auth login %s\n", th.Muted.Render("Try:"), r.Provider)
		default:
			fmt.Fprintf(a.IO.Out, "  %s %s\n", th.Error.Render(terminal.SymError), r.Error)
			if r.Hint != "" {
				fmt.Fprintf(a.IO.Out, "    %s %s\n", th.Muted.Render("Try:"), th.Accent.Render(r.Hint))
			}
		}
	}
}

func newAuthRefreshCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "refresh [alias]",
		Short:             "Refresh an expiring OAuth credential",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProviderArgs(f),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			explicit := ""
			if len(args) == 1 {
				explicit = args[0]
			}
			alias, err := a.ProviderName(explicit)
			if err != nil {
				return err
			}
			p, err := a.Open(alias)
			if err != nil {
				return err
			}
			r, ok := p.(forge.Refresher)
			if !ok {
				return errs.Unsupported(alias, "credential refresh")
			}
			st, err := spin(cmd.Context(), a, "Refreshing credential...", r.RefreshCredential)
			if err != nil {
				return errs.WithProvider(err, alias)
			}
			_ = a.Cache.Invalidate()
			a.Out.Success("Refreshed credential for %s", alias)
			if !st.ExpiresAt.IsZero() {
				a.Out.Info("Expires %s", st.ExpiresAt.Format(time.RFC1123))
			}
			return a.Out.Result(st, func() []string { return []string{alias} }, func() error { return nil })
		},
	}
}

// newAuthGitCredentialCmd implements git's credential-helper protocol so that
// HTTPS clones authenticate without tokens in URLs or argv.
func newAuthGitCredentialCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "git-credential [get|store|erase]",
		Short: "Git credential helper (used by trove for HTTPS remotes)",
		Long: `Implements git's credential helper protocol. Trove configures it
automatically for clones it performs. To use it for all git operations
against a host:

  git config --global credential.https://github.com.helper '!trove auth git-credential --provider github-personal'`,
		Args:   cobra.MaximumNArgs(1),
		Hidden: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			action := "get"
			if len(args) == 1 {
				action = args[0]
			}
			if action != "get" {
				return nil // store/erase: trove owns credential storage
			}
			a, err := f.App()
			if err != nil {
				return err
			}
			req, err := git.ReadCredentialRequest(a.IO.In)
			if err != nil {
				return err
			}
			if req.Protocol != "https" && req.Protocol != "http" {
				return nil
			}
			alias := a.Opts.Provider
			if alias == "" {
				d, derr := git.Detect("https://"+req.Host+"/x/y", a.DetectionAccounts(), a.Config.DefaultProvider)
				if derr != nil || d.Provider == "" {
					return nil // not ours; let git try other helpers
				}
				alias = d.Provider
			}
			pc, err := a.Config.Provider(alias)
			if err != nil {
				return err
			}
			// Only ever answer for hosts that belong to the account.
			if !hostBelongs(req.Host, a, alias, pc.Host) {
				return nil
			}
			p, err := a.Open(alias)
			if err != nil {
				return err
			}
			ga, ok := p.(forge.GitAuthenticator)
			if !ok {
				return nil
			}
			user, pass, err := ga.GitCredentials(cmd.Context())
			if err != nil {
				return errs.WithProvider(err, alias)
			}
			return git.WriteCredential(a.IO.Out, user, pass)
		},
	}
}

func hostBelongs(host string, a *app.App, alias, primary string) bool {
	want := hostname(host)
	candidates := []string{primary}
	for _, acct := range a.DetectionAccounts() {
		if acct.Alias == alias {
			candidates = append(candidates, acct.Hosts...)
		}
	}
	for _, h := range candidates {
		if h != "" && strings.EqualFold(hostname(h), want) {
			return true
		}
	}
	return false
}

// hostname extracts the lower-cased host (no scheme, port or path) from a
// host, host:port or URL.
func hostname(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil {
			return u.Hostname()
		}
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

// credentialHelper returns the git credential helper command for an alias.
func credentialHelper(a *app.App, alias string) string {
	if a.Executable == "" {
		return ""
	}
	args := []string{}
	if p := a.Config.Path(); p != "" {
		args = append(args, "--config", p)
	}
	args = append(args, "--provider", alias, "auth", "git-credential")
	return git.HelperCommand(a.Executable, args...)
}
