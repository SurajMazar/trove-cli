package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/config"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/output"
	"github.com/SurajMazar/trove-cli/internal/secrets"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

type nopDriver struct{}

func (nopDriver) Type() string        { return "nop" }
func (nopDriver) DisplayName() string { return "Nop" }
func (nopDriver) Description() string { return "" }
func (nopDriver) DefaultHost() string { return "nop.example" }
func (nopDriver) AuthMethods() []auth.Method {
	return []auth.Method{auth.MethodToken, auth.MethodBasic}
}
func (nopDriver) New(a forge.Account) (forge.Provider, error) {
	return nopProvider{a}, nil
}

type nopProvider struct{ acct forge.Account }

func (p nopProvider) Metadata() forge.Metadata         { return forge.Metadata{Name: p.acct.Name, Type: "nop"} }
func (p nopProvider) Capabilities() forge.Capabilities { return forge.NewCapabilities() }
func (p nopProvider) CurrentUser(ctx context.Context) (*domain.User, error) {
	c, err := p.acct.Credentials.Load(ctx)
	if err != nil {
		return nil, err
	}
	return &domain.User{Username: c.Username + ":" + c.Token}, nil
}

func build(t *testing.T, yaml string, env map[string]string) (*App, *secrets.MemoryProvider) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if yaml != "" {
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mem := secrets.NewMemoryProvider("mem")
	a, err := Build(Options{ConfigPath: path}, terminal.Test(nil, &bytes.Buffer{}, &bytes.Buffer{}), Deps{
		RegisterDrivers: func(r *forge.Registry) { r.MustRegister(nopDriver{}) },
		RegisterSecrets: func(r *secrets.Resolver, _ config.SecretsConfig) { r.Register(mem) },
		Getenv:          func(k string) string { return env[k] },
		CacheDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, mem
}

const twoProviders = `version: 1
providers:
  a:
    type: nop
    host: a.example
  b:
    type: nop
    host: b.example
    auth:
      type: basic
      username: me
secrets:
  provider: mem
`

func TestProviderNameResolution(t *testing.T) {
	a, _ := build(t, twoProviders, nil)
	if _, err := a.ProviderName(""); !errors.Is(err, errs.ErrProviderNotFound) {
		t.Fatalf("two providers and no default must be ambiguous: %v", err)
	}
	if n, _ := a.ProviderName("b"); n != "b" {
		t.Fatalf("explicit = %s", n)
	}
	if _, err := a.ProviderName("zzz"); !errors.Is(err, errs.ErrProviderNotFound) {
		t.Fatalf("unknown explicit: %v", err)
	}
	a.Config.DefaultProvider = "a"
	if n, _ := a.ProviderName(""); n != "a" {
		t.Fatalf("default = %s", n)
	}
	env, _ := build(t, twoProviders, map[string]string{"TROVE_PROVIDER": "b"})
	if n, _ := env.ProviderName(""); n != "b" {
		t.Fatalf("TROVE_PROVIDER ignored: %s", n)
	}
	empty, _ := build(t, "", nil)
	if _, err := empty.ProviderName(""); errs.HintOf(err) != "trove provider add" {
		t.Fatalf("no providers hint: %v", err)
	}
}

func TestCredentialSources(t *testing.T) {
	a, mem := build(t, twoProviders, map[string]string{"TROVE_TOKEN_B": "envtok"})
	_ = mem.Set(context.Background(), "trove/nop/a/token", "stored")
	pa, err := a.Open("a")
	if err != nil {
		t.Fatal(err)
	}
	if u, err := pa.CurrentUser(context.Background()); err != nil || u.Username != ":stored" {
		t.Fatalf("secret credential: %v %v", u, err)
	}
	pb, _ := a.OpenOther("b")
	if u, err := pb.CurrentUser(context.Background()); err != nil || u.Username != "me:envtok" {
		t.Fatalf("env credential with basic username: %v %v", u, err)
	}
	_, src, _ := a.Account("b", false)
	if src.Kind != "env" || src.Env != "TROVE_TOKEN_B" {
		t.Fatalf("source = %+v", src)
	}
	// Missing credential → ErrNotAuthenticated with login hint.
	a2, _ := build(t, twoProviders, nil)
	p, _ := a2.OpenOther("b")
	_, err = p.CurrentUser(context.Background())
	if !errors.Is(err, errs.ErrNotAuthenticated) || errs.HintOf(err) != "trove auth login b" {
		t.Fatalf("missing credential: %v (hint %q)", err, errs.HintOf(err))
	}
}

func TestGenericTokenOnlyForSelectedProvider(t *testing.T) {
	a, _ := build(t, twoProviders, map[string]string{"TROVE_TOKEN": "generic"})
	_, src, _ := a.Account("a", true)
	if src.Kind != "env" {
		t.Fatalf("selected provider should use TROVE_TOKEN: %+v", src)
	}
	_, src, _ = a.Account("b", false)
	if src.Kind != "secret" {
		t.Fatalf("non-selected provider must not use TROVE_TOKEN: %+v", src)
	}
}

func TestEnvTokenNames(t *testing.T) {
	got := EnvTokenNames("github-work.eu")
	if got[0] != "TROVE_TOKEN_GITHUB_WORK_EU" || got[1] != "TROVE_TOKEN" {
		t.Fatalf("EnvTokenNames = %v", got)
	}
}

func TestSecretRefDefaults(t *testing.T) {
	a, _ := build(t, twoProviders, nil)
	ref, _ := a.SecretRef("a", a.Config.Providers["a"])
	if ref.String() != "mem://trove/nop/a/token" {
		t.Fatalf("default ref = %s", ref)
	}
	if r := DefaultSecretRef("env", "github", "gh"); r.String() != "env://TROVE_TOKEN_GH" {
		t.Fatalf("env default ref = %s", r)
	}
}

func TestResolveRepoArguments(t *testing.T) {
	a, _ := build(t, twoProviders, nil)
	a.Config.DefaultProvider = "a"
	_, ref, err := a.ResolveRepo(context.Background(), "", "b:group/sub/project")
	if err != nil || ref.Provider != "b" || ref.Namespace != "group/sub" || ref.Name != "project" {
		t.Fatalf("ref = %+v %v", ref, err)
	}
	_, ref, _ = a.ResolveRepo(context.Background(), "", "org/repo")
	if ref.Provider != "a" {
		t.Fatalf("default provider not applied: %+v", ref)
	}
	if _, _, err := a.ResolveRepo(context.Background(), "a", "b:org/repo"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("conflict: %v", err)
	}
	if _, _, err := a.ResolveRepo(context.Background(), "", "justname"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("invalid: %v", err)
	}
}

func TestOutputModeFromEnvDisablesInteraction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	tio := terminal.Test(nil, &bytes.Buffer{}, &bytes.Buffer{})
	tio.Interactive = true
	a, err := Build(Options{ConfigPath: path}, tio, Deps{Getenv: func(k string) string {
		if k == "TROVE_OUTPUT" {
			return "json"
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Out.Mode != output.ModeJSON || a.IO.Interactive {
		t.Fatalf("mode=%s interactive=%v", a.Out.Mode, a.IO.Interactive)
	}
	if _, err := Build(Options{ConfigPath: path}, tio, Deps{Getenv: func(k string) string {
		if k == "TROVE_LOG_LEVEL" {
			return "chatty"
		}
		return ""
	}}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("bad log level: %v", err)
	}
}
