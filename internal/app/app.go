// Package app is Trove's application core. It wires configuration, the forge
// driver registry, secret providers, output and git together, and resolves
// which provider account a command operates on. It contains no
// provider-specific API behavior and no terminal UI code.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/cache"
	"github.com/SurajMazar/trove-cli/internal/config"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/git"
	"github.com/SurajMazar/trove-cli/internal/output"
	"github.com/SurajMazar/trove-cli/internal/secrets"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// Options are global flags and environment overrides.
type Options struct {
	ConfigPath     string
	Provider       string // --provider / TROVE_PROVIDER
	NonInteractive bool
	Yes            bool
	Debug          bool
	LogLevel       string
	Output         output.Mode
	NoCache        bool
}

// App is the composed application.
type App struct {
	Opts     Options
	Config   *config.Config
	Registry *forge.Registry
	Secrets  *secrets.Resolver
	IO       *terminal.IO
	Out      *output.Printer
	Log      *slog.Logger
	Cache    *cache.Cache
	Git      *git.Git
	// Transport is the base HTTP transport handed to drivers (tests inject
	// fakes); nil means http.DefaultTransport.
	Transport http.RoundTripper
	// Getenv is used for environment lookups (tests override it).
	Getenv func(string) string
	// Executable is the path used for the git credential helper.
	Executable string
}

// SecretProviderNames returns registered secret provider schemes.
func (a *App) SecretProviderNames() []string { return a.Secrets.Names() }

// ValidationRules builds config validation rules from registered drivers and
// secret providers.
func (a *App) ValidationRules() config.Rules {
	r := config.Rules{AuthMethods: map[string][]string{}, SecretProviders: a.Secrets.Names()}
	for _, d := range a.Registry.List() {
		var ms []string
		for _, m := range d.AuthMethods() {
			ms = append(ms, string(m))
		}
		r.AuthMethods[d.Type()] = ms
	}
	return r
}

// ProviderName resolves the account alias for a command: explicit flag,
// TROVE_PROVIDER, the configured default, or the only configured provider.
func (a *App) ProviderName(explicit string) (string, error) {
	for _, n := range []string{explicit, a.Opts.Provider} {
		if n != "" {
			if _, err := a.Config.Provider(n); err != nil {
				return "", err
			}
			return n, nil
		}
	}
	if d := a.Config.DefaultProvider; d != "" {
		if _, err := a.Config.Provider(d); err != nil {
			return "", err
		}
		return d, nil
	}
	names := a.Config.ProviderNames()
	switch len(names) {
	case 0:
		return "", &errs.Error{Kind: errs.ErrProviderNotFound, Message: "no providers are configured", Hint: "trove provider add"}
	case 1:
		return names[0], nil
	}
	return "", &errs.Error{Kind: errs.ErrProviderNotFound,
		Message: fmt.Sprintf("several providers are configured (%s) and none is the default", strings.Join(names, ", ")),
		Hint:    "trove provider use <name>  (or pass --provider)"}
}

// Driver returns the driver for an account.
func (a *App) Driver(pc *config.ProviderConfig) (forge.Driver, error) {
	return a.Registry.Get(pc.Type)
}

// SecretRef returns the secret reference used for an account's credential.
func (a *App) SecretRef(alias string, pc *config.ProviderConfig) (secrets.SecretReference, error) {
	if pc.Auth.SecretRef != "" {
		return secrets.ParseReference(pc.Auth.SecretRef)
	}
	sp := a.Config.Secrets.Provider
	if sp == "" {
		sp = config.DefaultSecretProvider()
	}
	return DefaultSecretRef(sp, pc.Type, alias), nil
}

// DefaultSecretRef is the conventional location of an account's credential
// in secret provider sp. For the read-only env provider the key is an
// environment variable name (TROVE_TOKEN_<ALIAS>).
func DefaultSecretRef(sp, forgeType, alias string) secrets.SecretReference {
	if sp == "env" {
		return secrets.SecretReference{Provider: sp, Key: EnvTokenNames(alias)[0]}
	}
	return secrets.SecretReference{Provider: sp, Key: secrets.DefaultKey(forgeType, alias, "token")}
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]+`)

// EnvTokenNames returns the environment variables consulted for an alias:
// TROVE_TOKEN_<ALIAS> (alias upper-cased, non-alphanumerics → '_'), then
// TROVE_TOKEN when alias is the command's selected provider.
func EnvTokenNames(alias string) []string {
	return []string{"TROVE_TOKEN_" + strings.ToUpper(strings.Trim(nonAlnum.ReplaceAllString(alias, "_"), "_")), "TROVE_TOKEN"}
}

func (a *App) getenv(k string) string {
	if a.Getenv != nil {
		return a.Getenv(k)
	}
	return os.Getenv(k)
}

// CredentialSource describes where an account's credential comes from.
type CredentialSource struct {
	Kind string // "env" or "secret"
	Env  string
	Ref  secrets.SecretReference
}

// credentialStore picks the credential store for an account. Environment
// credentials (CI) take precedence and are never persisted.
func (a *App) credentialStore(alias string, pc *config.ProviderConfig, selected bool) (auth.Store, CredentialSource, error) {
	names := EnvTokenNames(alias)
	for i, n := range names {
		if i == 1 && !selected {
			continue
		}
		if v := a.getenv(n); v != "" {
			cred := auth.Credential{Kind: auth.KindToken, Token: v}
			if pc.Auth.Type == string(auth.MethodBasic) {
				cred.Kind = auth.KindBasic
				cred.Username = pc.Auth.Username
			}
			if pc.Auth.Type == string(auth.MethodOAuth) {
				cred.Kind = auth.KindOAuth
			}
			st := auth.NewMemoryStore(cred)
			st.ReadOnly = true
			return st, CredentialSource{Kind: "env", Env: n}, nil
		}
	}
	ref, err := a.SecretRef(alias, pc)
	if err != nil {
		return nil, CredentialSource{}, err
	}
	return &secretStore{resolver: a.Secrets, ref: ref, alias: alias}, CredentialSource{Kind: "secret", Ref: ref}, nil
}

// Account builds the forge.Account for an alias.
func (a *App) Account(alias string, selected bool) (forge.Account, CredentialSource, error) {
	pc, err := a.Config.Provider(alias)
	if err != nil {
		return forge.Account{}, CredentialSource{}, err
	}
	store, src, err := a.credentialStore(alias, pc, selected)
	if err != nil {
		return forge.Account{}, CredentialSource{}, err
	}
	acct := forge.Account{
		Name: alias, Type: pc.Type, Host: pc.Host,
		APIURL: pc.APIBaseURL, WebURL: pc.WebBaseURL, CloneBaseURL: pc.CloneBaseURL, SSHHost: pc.SSHHost,
		AuthMethod: auth.Method(pc.Auth.Type), Username: pc.Auth.Username, ClientID: pc.Auth.ClientID,
		AppID: pc.Auth.AppID, InstallationID: pc.Auth.InstallationID, Scopes: pc.Auth.Scopes,
		Credentials: store, Logger: a.Log.With("provider", alias), Transport: a.Transport,
		Decode: pc.DecodeInto,
	}
	if acct.AuthMethod == "" {
		acct.AuthMethod = auth.MethodToken
	}
	return acct, src, nil
}

// Open constructs the provider for alias.
func (a *App) Open(alias string) (forge.Provider, error) {
	return a.open(alias, true)
}

func (a *App) open(alias string, selected bool) (forge.Provider, error) {
	pc, err := a.Config.Provider(alias)
	if err != nil {
		return nil, err
	}
	d, err := a.Driver(pc)
	if err != nil {
		return nil, errs.WithProvider(err, alias)
	}
	acct, _, err := a.Account(alias, selected)
	if err != nil {
		return nil, err
	}
	p, err := d.New(acct)
	if err != nil {
		return nil, errs.WithProvider(err, alias)
	}
	return p, nil
}

// OpenOther opens a provider that is not the command's selected one (so the
// generic TROVE_TOKEN is not applied to it).
func (a *App) OpenOther(alias string) (forge.Provider, error) { return a.open(alias, false) }

// Current resolves and opens the provider for a command.
func (a *App) Current(explicit string) (forge.Provider, error) {
	name, err := a.ProviderName(explicit)
	if err != nil {
		return nil, err
	}
	return a.Open(name)
}

// Label returns the display label for an account.
func (a *App) Label(alias string) string {
	if pc, err := a.Config.Provider(alias); err == nil {
		return pc.Label(alias)
	}
	return alias
}

// DetectionAccounts returns configured accounts for remote detection.
func (a *App) DetectionAccounts() []git.Account {
	var out []git.Account
	for _, alias := range a.Config.ProviderNames() {
		pc := a.Config.Providers[alias]
		hosts := []string{pc.Host}
		for _, h := range []string{pc.SSHHost, pc.APIBaseURL, pc.WebBaseURL, pc.CloneBaseURL} {
			if h != "" {
				hosts = append(hosts, h)
			}
		}
		out = append(out, git.Account{Alias: alias, Type: pc.Type, Hosts: hosts})
	}
	return out
}

// ResolveRepo turns a user reference ("provider:ns/name", "ns/name" or empty
// for the current directory's remote) into a provider and repository ref.
func (a *App) ResolveRepo(ctx context.Context, explicitProvider, arg string) (forge.Provider, domain.RepositoryRef, error) {
	if arg == "" {
		d, err := a.DetectCurrent(ctx)
		if err != nil {
			return nil, domain.RepositoryRef{}, err
		}
		alias := explicitProvider
		if alias == "" {
			alias = a.Opts.Provider
		}
		if alias == "" {
			alias = d.Provider
		}
		if alias == "" {
			return nil, domain.RepositoryRef{}, &errs.Error{Kind: errs.ErrProviderNotFound,
				Message: fmt.Sprintf("no configured provider matches host %q of the current repository", d.Host),
				Hint:    "trove provider add  (or pass --provider and a repository argument)"}
		}
		p, err := a.Open(alias)
		if err != nil {
			return nil, domain.RepositoryRef{}, err
		}
		return p, domain.RepositoryRef{Provider: alias, Namespace: d.Namespace, Name: d.Name}, nil
	}
	ref, err := domain.ParseRepositoryRef(arg)
	if err != nil {
		return nil, domain.RepositoryRef{}, errs.Wrap(errs.ErrInvalidArgument, err, "invalid repository")
	}
	if ref.Provider != "" && explicitProvider != "" && ref.Provider != explicitProvider {
		return nil, domain.RepositoryRef{}, errs.New(errs.ErrInvalidArgument, "repository %q names provider %q but --provider is %q", arg, ref.Provider, explicitProvider)
	}
	if ref.Provider == "" {
		ref.Provider, err = a.ProviderName(explicitProvider)
		if err != nil {
			return nil, domain.RepositoryRef{}, err
		}
	}
	p, err := a.Open(ref.Provider)
	if err != nil {
		return nil, domain.RepositoryRef{}, err
	}
	return p, ref, nil
}

// DetectCurrent detects the forge repository of the current directory,
// preferring the "upstream"-less "origin" remote.
func (a *App) DetectCurrent(ctx context.Context) (git.Detection, error) {
	ds, err := a.DetectRemotes(ctx, ".")
	if err != nil {
		return git.Detection{}, err
	}
	for _, d := range ds {
		if d.Remote == "origin" {
			return d, nil
		}
	}
	return ds[0], nil
}

// DetectRemotes inspects every remote of the repository at dir.
func (a *App) DetectRemotes(ctx context.Context, dir string) ([]git.Detection, error) {
	if !a.Git.IsRepository(ctx, dir) {
		return nil, &errs.Error{Kind: errs.ErrNotFound, Message: "not inside a git repository",
			Hint: "run inside a repository or pass a repository argument such as owner/name"}
	}
	remotes, err := a.Git.Remotes(ctx, dir)
	if err != nil {
		return nil, err
	}
	if len(remotes) == 0 {
		return nil, errs.New(errs.ErrNotFound, "the repository has no remotes")
	}
	var out []git.Detection
	var firstErr error
	for _, r := range remotes {
		d, err := git.Detect(r.FetchURL, a.DetectionAccounts(), a.Config.DefaultProvider)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("remote %s: %w", r.Name, err)
			}
			continue
		}
		d.Remote = r.Name
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, errs.Wrap(errs.ErrInvalidArgument, firstErr, "no remote could be parsed")
	}
	return out, nil
}

// secretStore persists credentials through a secret provider.
type secretStore struct {
	resolver *secrets.Resolver
	ref      secrets.SecretReference
	alias    string
}

func (s *secretStore) Load(ctx context.Context) (auth.Credential, error) {
	v, err := s.resolver.Resolve(ctx, s.ref)
	if err != nil {
		if errors.Is(err, errs.ErrSecretNotFound) {
			return auth.Credential{}, &errs.Error{Kind: errs.ErrNotAuthenticated, Provider: s.alias, Cause: err,
				Message: fmt.Sprintf("no credential stored for %s (%s)", s.alias, s.ref.Provider),
				Hint:    "trove auth login " + s.alias}
		}
		return auth.Credential{}, errs.WithProvider(err, s.alias)
	}
	return auth.DecodeCredential(v)
}

func (s *secretStore) Save(ctx context.Context, c auth.Credential) error {
	return s.resolver.Store(ctx, s.ref, c.Encode())
}

func (s *secretStore) Delete(ctx context.Context) error {
	return s.resolver.Remove(ctx, s.ref)
}
