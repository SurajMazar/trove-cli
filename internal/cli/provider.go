package cli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/config"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/output"
	"github.com/SurajMazar/trove-cli/internal/secrets"
	"github.com/SurajMazar/trove-cli/internal/terminal"
	"github.com/SurajMazar/trove-cli/internal/tui"
)

func newProviderCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "provider",
		Aliases: []string{"providers"},
		Short:   "Manage forge provider accounts",
		Long:    "Configure GitHub, GitLab, Bitbucket and custom forge accounts. Each account has a local alias such as github-personal.",
	}
	cmd.AddCommand(newProviderListCmd(f), newProviderAddCmd(f), newProviderRemoveCmd(f),
		newProviderShowCmd(f), newProviderUseCmd(f), newProviderTestCmd(f))
	return cmd
}

func accountView(a *app.App, name string) domain.ProviderAccount {
	pc := a.Config.Providers[name]
	acct := domain.ProviderAccount{Name: name, Type: pc.Type, Host: pc.Host, APIURL: pc.APIBaseURL,
		AuthType: pc.Auth.Type, Default: name == a.Config.DefaultProvider}
	if ref, err := a.SecretRef(name, pc); err == nil {
		acct.SecretRef = ref.String()
	}
	if acct.AuthType == "" {
		acct.AuthType = string(auth.MethodToken)
	}
	return acct
}

func newProviderListCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured provider accounts",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			var accts []domain.ProviderAccount
			for _, n := range a.Config.ProviderNames() {
				accts = append(accts, accountView(a, n))
			}
			if accts == nil {
				accts = []domain.ProviderAccount{}
			}
			return a.Out.Result(accts, func() []string {
				var out []string
				for _, x := range accts {
					out = append(out, x.Name)
				}
				return out
			}, func() error {
				if len(accts) == 0 {
					a.Out.Println("No providers configured. Add one with: trove provider add")
					return nil
				}
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: ""}, {Header: "Name"}, {Header: "Type"}, {Header: "Host", Flex: true}, {Header: "Auth"}, {Header: "Secret", MinTerminal: 120, Flex: true}}}
				for _, x := range accts {
					mark := output.C(" ")
					if x.Default {
						mark = output.S(terminal.SymProvider, th.Provider)
					}
					t.Add(mark, output.C(x.Name), output.C(x.Type), output.C(x.Host), output.C(x.AuthType), output.S(x.SecretRef, th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
}

var authMethodLabels = map[auth.Method]string{
	auth.MethodToken:       "Personal Access Token",
	auth.MethodOAuth:       "OAuth",
	auth.MethodApp:         "GitHub App",
	auth.MethodBasic:       "API token (username/email + token)",
	auth.MethodAccessToken: "Access token (repository, project or workspace)",
}

func methodLabel(m auth.Method) string {
	if l, ok := authMethodLabels[m]; ok {
		return l
	}
	return string(m)
}

type providerAddFlags struct {
	typ, host, name, authType, apiURL, webURL, cloneURL, sshHost, protocol string
	secretRef, secretProvider, username, clientID, appID                   string
	installationID                                                         string
	scopes, set                                                            []string
	makeDefault, login                                                     bool
}

func newProviderAddCmd(f *Factory) *cobra.Command {
	var fl providerAddFlags
	cmd := &cobra.Command{
		Use:   "add [alias]",
		Short: "Add a provider account",
		Long: `Add a provider account. Without flags in a terminal, an interactive setup
asks for the provider type, host and authentication method.

Examples:
  trove provider add github-personal
  trove provider add gitlab-work --type gitlab --host gitlab.company.com --auth-type token
  trove provider add bb --type bitbucket --auth-type basic --username me@example.com
  trove provider add company --type custom --host git.example.com --api-url https://git.example.com/api`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			alias := ""
			if len(args) == 1 {
				alias = args[0]
			}
			return providerAdd(cmd.Context(), f, a, alias, fl)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&fl.typ, "type", "", "provider type: github, gitlab, bitbucket, custom")
	fs.StringVar(&fl.host, "host", "", "hostname, e.g. github.com or gitlab.company.com")
	fs.StringVar(&fl.name, "name", "", "display label, e.g. \"GitHub Personal\"")
	fs.StringVar(&fl.authType, "auth-type", "", "authentication method (provider specific: token, oauth, app, basic, access-token)")
	fs.StringVar(&fl.apiURL, "api-url", "", "API base URL override (self-hosted/custom)")
	fs.StringVar(&fl.webURL, "web-url", "", "web base URL override")
	fs.StringVar(&fl.cloneURL, "clone-url", "", "HTTPS clone base URL override")
	fs.StringVar(&fl.sshHost, "ssh-host", "", "SSH clone host override")
	fs.StringVar(&fl.protocol, "protocol", "", "default clone protocol for this account: https or ssh")
	fs.StringVar(&fl.secretRef, "secret-ref", "", "where the credential is stored, e.g. bitwarden://trove/github/personal/token")
	fs.StringVar(&fl.secretProvider, "secret-provider", "", "secret provider for the default secret_ref (bitwarden, keychain, secretservice, env)")
	fs.StringVar(&fl.username, "username", "", "username/email for basic-auth methods")
	fs.StringVar(&fl.clientID, "client-id", "", "OAuth client ID")
	fs.StringVar(&fl.appID, "app-id", "", "GitHub App ID")
	fs.StringVar(&fl.installationID, "installation-id", "", "GitHub App installation ID")
	fs.StringSliceVar(&fl.scopes, "scopes", nil, "OAuth scopes")
	fs.StringArrayVar(&fl.set, "set", nil, "provider-specific setting KEY=VALUE (repeatable), e.g. --set workspace=acme")
	fs.BoolVar(&fl.makeDefault, "default", false, "make this the default provider")
	fs.BoolVar(&fl.login, "login", false, "log in right after adding")
	return cmd
}

func providerAdd(ctx context.Context, f *Factory, a *app.App, alias string, fl providerAddFlags) error {
	interactive := a.IO.Interactive
	var err error
	if alias == "" {
		if !interactive {
			return errs.New(errs.ErrInvalidArgument, "provider alias is required")
		}
		alias, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: "Alias", Placeholder: "github-personal",
			Help: "A short local name for this account", Validate: func(s string) error {
				if !config.ValidAlias(s) {
					return fmt.Errorf("use letters, digits, '-' or '_'")
				}
				if _, exists := a.Config.Providers[s]; exists {
					return fmt.Errorf("%q already exists", s)
				}
				return nil
			}})
		if err != nil {
			return err
		}
	}
	if !config.ValidAlias(alias) {
		return errs.New(errs.ErrInvalidArgument, "invalid alias %q: use letters, digits, '-' or '_'", alias)
	}
	if _, exists := a.Config.Providers[alias]; exists {
		return &errs.Error{Kind: errs.ErrConflict, Message: fmt.Sprintf("provider %q already exists", alias), Hint: "trove provider remove " + alias}
	}

	// Provider type.
	typ := fl.typ
	if typ == "" {
		if !interactive {
			return errs.New(errs.ErrInvalidArgument, "--type is required (one of: %s)", strings.Join(driverTypes(a), ", "))
		}
		var choices []tui.Choice
		for _, d := range a.Registry.List() {
			choices = append(choices, tui.Choice{Label: d.DisplayName(), Description: d.Description(), Value: d.Type()})
		}
		c, err := tui.Choose(ctx, a.IO, "Provider type:", choices)
		if err != nil {
			return err
		}
		typ = c.Value
	}
	d, err := a.Registry.Get(typ)
	if err != nil {
		return err
	}

	// Host.
	host := fl.host
	if host == "" {
		host = d.DefaultHost()
		if interactive && fl.typ == "" || interactive && host == "" {
			host, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: "Host:", Default: d.DefaultHost(), Placeholder: d.DefaultHost(),
				Validate: func(s string) error {
					if s == "" || strings.ContainsAny(s, "/ ") {
						return fmt.Errorf("enter a hostname such as gitlab.company.com")
					}
					return nil
				}})
			if err != nil {
				return err
			}
		}
		if host == "" {
			return errs.New(errs.ErrInvalidArgument, "--host is required for %s", typ)
		}
	}

	// Authentication method.
	methods := d.AuthMethods()
	method := auth.Method(fl.authType)
	if method == "" {
		if interactive && len(methods) > 1 {
			var choices []tui.Choice
			for _, m := range methods {
				choices = append(choices, tui.Choice{Label: methodLabel(m), Value: string(m)})
			}
			c, err := tui.Choose(ctx, a.IO, "Authentication:", choices)
			if err != nil {
				return err
			}
			method = auth.Method(c.Value)
		} else if len(methods) > 0 {
			method = methods[0]
		}
	}
	supported := false
	for _, m := range methods {
		supported = supported || m == method
	}
	if !supported {
		var ms []string
		for _, m := range methods {
			ms = append(ms, string(m))
		}
		return errs.New(errs.ErrInvalidArgument, "%s does not support auth type %q (supported: %s)", d.DisplayName(), method, strings.Join(ms, ", "))
	}

	pc := &config.ProviderConfig{
		Type: typ, Host: host, Name: fl.name,
		APIBaseURL: fl.apiURL, WebBaseURL: fl.webURL, CloneBaseURL: fl.cloneURL, SSHHost: fl.sshHost, Protocol: fl.protocol,
		Auth: config.AuthConfig{Type: string(method), Username: fl.username, ClientID: fl.clientID,
			AppID: fl.appID, InstallationID: fl.installationID, Scopes: fl.scopes},
	}
	if err := promptMethodDetails(ctx, a, pc, method, typ); err != nil {
		return err
	}
	if typ == "custom" && pc.APIBaseURL == "" {
		if !interactive {
			return errs.New(errs.ErrInvalidArgument, "--api-url is required for custom providers")
		}
		pc.APIBaseURL, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: "API base URL:", Placeholder: "https://" + host + "/api", Default: "https://" + host + "/api", Validate: validURL})
		if err != nil {
			return err
		}
		pc.CloneBaseURL, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: "Clone base URL:", Default: "https://" + host, Validate: validURL})
		if err != nil {
			return err
		}
	}
	if typ == "custom" {
		if pc.Extra == nil {
			pc.Extra = map[string]any{}
		}
		if _, ok := pc.Extra["custom"]; !ok {
			pc.Extra["custom"] = map[string]any{"capabilities": []any{"repositories"}}
		}
	}
	for _, kv := range fl.set {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return errs.New(errs.ErrInvalidArgument, "invalid --set %q (want KEY=VALUE)", kv)
		}
		if config.IsSecretKey(k) {
			return errs.New(errs.ErrInvalidArgument, "%q looks like a secret; secrets are stored with trove auth login", k)
		}
		if pc.Extra == nil {
			pc.Extra = map[string]any{}
		}
		pc.Extra[k] = v
	}

	// Repository/project/workspace access tokens have no user; the driver
	// needs to know which workspace to validate against.
	if method == auth.MethodAccessToken {
		if _, ok := pc.Extra["workspace"]; !ok {
			if !interactive {
				return errs.New(errs.ErrInvalidArgument, "access tokens need a workspace: add --set workspace=<slug>")
			}
			ws, err := tui.Input(ctx, a.IO, tui.InputOptions{Label: "Workspace:", Help: "The workspace slug the access token belongs to", Validate: nonEmpty})
			if err != nil {
				return err
			}
			if pc.Extra == nil {
				pc.Extra = map[string]any{}
			}
			pc.Extra["workspace"] = ws
		}
	}

	// Secret reference.
	if fl.secretRef != "" {
		if _, err := secrets.ParseReference(fl.secretRef); err != nil {
			return err
		}
		pc.Auth.SecretRef = fl.secretRef
	} else {
		sp := fl.secretProvider
		if sp == "" {
			sp = a.Config.Secrets.Provider
		}
		pc.Auth.SecretRef = app.DefaultSecretRef(sp, typ, alias).String()
	}

	a.Config.Providers[alias] = pc
	if fl.makeDefault || a.Config.DefaultProvider == "" {
		a.Config.DefaultProvider = alias
	}
	rollback := func() {
		delete(a.Config.Providers, alias)
		if a.Config.DefaultProvider == alias {
			a.Config.DefaultProvider = ""
		}
	}
	if problems := a.Config.Validate(a.ValidationRules()); len(problems) > 0 {
		rollback()
		return configProblemsError(problems)
	}
	// Let the driver validate provider-specific configuration.
	if _, err := a.OpenOther(alias); err != nil {
		rollback()
		return err
	}
	if err := a.Config.Save(); err != nil {
		rollback()
		return err
	}
	a.Out.Success("Added %s (%s, %s)", alias, d.DisplayName(), host)
	if a.Config.DefaultProvider == alias {
		a.Out.Info("%s is the default provider", alias)
	}
	login := fl.login
	if !login && interactive {
		ok, err := a.IO.Confirm("Log in now?")
		if err == nil && ok {
			login = true
		}
	}
	if login {
		return authLogin(ctx, a, alias, loginFlags{})
	}
	a.Out.Info("Next: trove auth login %s", alias)
	return nil
}

func promptMethodDetails(ctx context.Context, a *app.App, pc *config.ProviderConfig, method auth.Method, typ string) error {
	var err error
	ask := func(dst *string, label, help string) error {
		if *dst != "" || !a.IO.Interactive {
			return nil
		}
		*dst, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: label, Help: help})
		return err
	}
	switch method {
	case auth.MethodBasic:
		if err := ask(&pc.Auth.Username, "Username / email:", "Bitbucket API tokens authenticate with your Atlassian account email"); err != nil {
			return err
		}
		if pc.Auth.Username == "" {
			return errs.New(errs.ErrInvalidArgument, "--username is required for basic authentication")
		}
	case auth.MethodApp:
		if err := ask(&pc.Auth.AppID, "GitHub App ID:", ""); err != nil {
			return err
		}
		if err := ask(&pc.Auth.InstallationID, "Installation ID:", ""); err != nil {
			return err
		}
		if pc.Auth.AppID == "" || pc.Auth.InstallationID == "" {
			return errs.New(errs.ErrInvalidArgument, "--app-id and --installation-id are required for GitHub App authentication")
		}
	case auth.MethodOAuth:
		if typ != "bitbucket" {
			if err := ask(&pc.Auth.ClientID, "OAuth client ID (leave empty to paste a token):", "Register an OAuth application with device flow enabled"); err != nil {
				return err
			}
		}
	}
	return nil
}

func validURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("enter an absolute URL such as https://git.example.com/api")
	}
	return nil
}

func driverTypes(a *app.App) []string {
	var out []string
	for _, d := range a.Registry.List() {
		out = append(out, d.Type())
	}
	return out
}

func configProblemsError(ps []config.Problem) error {
	var lines []string
	for _, p := range ps {
		lines = append(lines, p.String())
	}
	return &errs.Error{Kind: errs.ErrInvalidConfiguration, Message: strings.Join(lines, "\n")}
}

func newProviderRemoveCmd(f *Factory) *cobra.Command {
	var keep bool
	cmd := &cobra.Command{
		Use:               "remove <alias>",
		Aliases:           []string{"rm"},
		Short:             "Remove a provider account and its stored credential",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeProviderArgs(f),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			alias := args[0]
			pc, err := a.Config.Provider(alias)
			if err != nil {
				return err
			}
			q := fmt.Sprintf("Remove provider %q?", alias)
			if !keep {
				q = fmt.Sprintf("Remove provider %q and delete its stored credential?", alias)
			}
			if err := confirm(a, q); err != nil {
				return err
			}
			if !keep {
				ref, err := a.SecretRef(alias, pc)
				if err == nil && ref.Provider != "env" {
					if err := a.Secrets.Remove(cmd.Context(), ref); err != nil {
						a.Out.Warn("Could not delete the stored credential (%s): %v", ref.Provider, err)
					}
				}
			}
			delete(a.Config.Providers, alias)
			if a.Config.DefaultProvider == alias {
				a.Config.DefaultProvider = ""
			}
			if err := a.Config.Save(); err != nil {
				return err
			}
			_ = a.Cache.Invalidate()
			a.Out.Success("Removed %s", alias)
			return nil
		},
	}
	cmd.Flags().BoolVar(&keep, "keep-credential", false, "keep the credential in the secret provider")
	return cmd
}

func completeProviderArgs(f *Factory) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeProviders(f)(cmd, args, toComplete)
	}
}

type providerDetails struct {
	Account      domain.ProviderAccount `json:"account"`
	Metadata     forge.Metadata         `json:"metadata"`
	Capabilities []forge.Capability     `json:"capabilities"`
}

func newProviderShowCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "show [alias]",
		Short:             "Show a provider account, its endpoints and capabilities",
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
			det := providerDetails{Account: accountView(a, alias), Metadata: p.Metadata(), Capabilities: p.Capabilities().List()}
			return a.Out.Result(det, func() []string { return []string{alias} }, func() error {
				th := a.Out.Theme()
				m := det.Metadata
				fmt.Fprintln(a.IO.Out, th.Provider.Render(terminal.SymProvider+" "+a.Label(alias))+th.Muted.Render("  "+m.DisplayName))
				fmt.Fprintln(a.IO.Out)
				var methods []string
				for _, x := range m.AuthMethods {
					methods = append(methods, string(x))
				}
				a.Out.KeyValues([]output.KV{
					{Key: "Alias", Value: alias},
					{Key: "Type", Value: m.Type},
					{Key: "Deployment", Value: m.Deployment},
					{Key: "Host", Value: m.Host},
					{Key: "Web", Value: m.WebURL},
					{Key: "API", Value: m.APIURL},
					{Key: "Auth", Value: det.Account.AuthType + th.Muted.Render("  (supports: "+strings.Join(methods, ", ")+")")},
					{Key: "Secret", Value: det.Account.SecretRef},
					{Key: "Default", Value: yesNo(det.Account.Default)},
				})
				fmt.Fprintln(a.IO.Out)
				fmt.Fprintln(a.IO.Out, th.Heading.Render("Capabilities"))
				caps := p.Capabilities()
				for _, c := range forge.AllCapabilities {
					if caps.Has(c) {
						fmt.Fprintf(a.IO.Out, "  %s %s\n", th.Success.Render(terminal.SymSuccess), forge.FeatureName(c))
					} else {
						fmt.Fprintf(a.IO.Out, "  %s %s\n", th.Muted.Render("·"), th.Muted.Render(forge.FeatureName(c)))
					}
				}
				return nil
			})
		},
	}
}

func newProviderUseCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "use [alias]",
		Aliases:           []string{"switch"},
		Short:             "Set the default provider",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeProviderArgs(f),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			alias := ""
			if len(args) == 1 {
				alias = args[0]
			} else {
				if err := pickProvider(cmd.Context(), a, false); err != nil {
					return err
				}
				alias = a.Opts.Provider
			}
			return useProvider(a, alias)
		},
	}
}

func useProvider(a *app.App, alias string) error {
	if _, err := a.Config.Provider(alias); err != nil {
		return err
	}
	a.Config.DefaultProvider = alias
	if err := a.Config.Save(); err != nil {
		return err
	}
	a.Out.Success("Default provider is now %s", alias)
	return a.Out.Result(map[string]string{"default_provider": alias}, func() []string { return []string{alias} }, func() error { return nil })
}

type providerTestResult struct {
	Provider      string       `json:"provider"`
	Reachable     bool         `json:"reachable"`
	Authenticated bool         `json:"authenticated"`
	User          string       `json:"user,omitempty"`
	LatencyMS     int64        `json:"latency_ms"`
	Capabilities  int          `json:"capabilities"`
	Error         string       `json:"error,omitempty"`
	Status        *auth.Status `json:"auth,omitempty"`
}

func newProviderTestCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "test [alias]",
		Short:             "Test connectivity and authentication for a provider",
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
			res := providerTestResult{Provider: alias, Capabilities: len(p.Capabilities().List())}
			start := time.Now()
			u, err := spin(cmd.Context(), a, "Contacting "+p.Metadata().DisplayName+"...", p.CurrentUser)
			res.LatencyMS = time.Since(start).Milliseconds()
			if err == nil {
				res.Reachable, res.Authenticated, res.User = true, true, u.Display()
			} else {
				res.Error = err.Error()
				res.Reachable = !isTransportError(err)
			}
			if authn, ok := p.(forge.Authenticator); ok && err == nil {
				if st, serr := authn.AuthStatus(cmd.Context()); serr == nil {
					res.Status = st
				}
			}
			outErr := a.Out.Result(res, func() []string { return []string{alias} }, func() error {
				th := a.Out.Theme()
				mark := func(ok bool) string {
					if ok {
						return th.Success.Render(terminal.SymSuccess)
					}
					return th.Error.Render(terminal.SymError)
				}
				fmt.Fprintf(a.IO.Out, "%s API reachable  %s\n", mark(res.Reachable), th.Muted.Render(fmt.Sprintf("%s · %dms", p.Metadata().APIURL, res.LatencyMS)))
				fmt.Fprintf(a.IO.Out, "%s Authenticated  %s\n", mark(res.Authenticated), th.Muted.Render(res.User))
				if res.Status != nil && len(res.Status.Scopes) > 0 {
					fmt.Fprintf(a.IO.Out, "  %s %s\n", th.Muted.Render("Scopes:"), strings.Join(res.Status.Scopes, ", "))
				}
				if res.Status != nil && !res.Status.ExpiresAt.IsZero() {
					fmt.Fprintf(a.IO.Out, "  %s %s\n", th.Muted.Render("Expires:"), res.Status.ExpiresAt.Format(time.RFC1123))
				}
				fmt.Fprintf(a.IO.Out, "%s %d capabilities\n", th.Success.Render(terminal.SymSuccess), res.Capabilities)
				return nil
			})
			if outErr != nil {
				return outErr
			}
			if err != nil {
				return errs.WithProvider(err, alias)
			}
			return nil
		},
	}
}

func isTransportError(err error) bool {
	s := strings.ToLower(err.Error())
	for _, p := range []string{"no such host", "connection refused", "i/o timeout", "tls:", "network is unreachable", "deadline exceeded"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
