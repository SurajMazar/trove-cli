package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `version: 1
default_provider: github-personal
providers:
  github-personal:
    type: github
    host: github.com
    auth:
      type: token
      secret_ref: bitwarden://trove/github/personal/token
  gitlab-work:
    type: gitlab
    host: gitlab.company.com
    auth:
      type: token
      secret_ref: bitwarden://trove/gitlab/work/token
  bb:
    type: bitbucket
    host: bitbucket.org
    workspace: acme
    auth:
      type: basic
      username: me@example.com
secrets:
  provider: bitwarden
clone:
  concurrency: 4
`

var rules = Rules{
	AuthMethods:     map[string][]string{"github": {"token", "oauth", "app"}, "gitlab": {"token", "oauth"}, "bitbucket": {"basic", "access-token", "oauth"}},
	SecretProviders: []string{"bitwarden", "env", "keychain", "secretservice"},
}

func TestParseAndValidateSample(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if ps := c.Validate(rules); len(ps) != 0 {
		t.Fatalf("unexpected problems: %v", ps)
	}
	if c.Providers["bb"].Extra["workspace"] != "acme" {
		t.Fatalf("provider-specific extra lost: %+v", c.Providers["bb"].Extra)
	}
	var extra struct {
		Workspace string `yaml:"workspace"`
		Type      string `yaml:"type"`
	}
	if err := c.Providers["bb"].DecodeInto(&extra); err != nil || extra.Workspace != "acme" || extra.Type != "bitbucket" {
		t.Fatalf("DecodeInto = %+v, %v", extra, err)
	}
	if c.Clone.Layout != "namespace" || c.Clone.Protocol != "https" {
		t.Fatalf("defaults not applied: %+v", c.Clone)
	}
}

func TestValidateFindsProblems(t *testing.T) {
	bad := `version: 1
default_provider: missing
providers:
  "bad alias!":
    type: nope
    host: https://github.com/
  gh:
    type: github
    host: github.com
    api_base_url: ftp://x
    auth:
      type: basic
      secret_ref: vault-without-scheme
  gl:
    type: gitlab
    host: gitlab.com
    token: glpat-oops
clone:
  concurrency: 100
  protocol: git
  layout: weird
cache:
  ttl: soon
`
	c, err := Parse([]byte(bad))
	if err != nil {
		t.Fatal(err)
	}
	ps := c.Validate(rules)
	want := []string{"default_provider", `providers.bad alias!`, "providers.bad alias!.type", "providers.bad alias!.host",
		"providers.gh.api_base_url", "providers.gh.auth.type", "providers.gh.auth.secret_ref", "secrets",
		"clone.concurrency", "clone.protocol", "clone.layout", "cache.ttl"}
	got := map[string]bool{}
	for _, p := range ps {
		got[p.Path] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing problem for %s; got %v", w, ps)
		}
	}
}

func TestUnknownRootKeyRejected(t *testing.T) {
	if _, err := Parse([]byte("version: 1\nproviderz: {}\n")); err == nil {
		t.Fatal("unknown root key accepted")
	}
}

func TestMigrationFromUnversioned(t *testing.T) {
	c, err := Parse([]byte("providers:\n  gh:\n    type: github\n    host: github.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != CurrentVersion {
		t.Fatalf("version = %d", c.Version)
	}
	if _, err := Parse([]byte("version: 99\n")); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("future version should be rejected, got %v", err)
	}
}

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	c.SetPath(path)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", st.Mode().Perm(), err)
	}
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c2.DefaultProvider != "github-personal" || len(c2.Providers) != 3 || c2.Providers["bb"].Extra["workspace"] != "acme" {
		t.Fatalf("round trip lost data: %+v", c2)
	}
}

func TestSaveRefusesSecrets(t *testing.T) {
	c := New()
	c.SetPath(filepath.Join(t.TempDir(), "c.yaml"))
	c.Providers["x"] = &ProviderConfig{Type: "github", Host: "github.com", Extra: map[string]any{"token": "ghp_x"}}
	if err := c.Save(); err == nil {
		t.Fatal("config with a token was saved")
	}
}

func TestGetSetUnset(t *testing.T) {
	c, _ := Parse([]byte(sample))
	if v, err := c.Get("providers.gitlab-work.host"); err != nil || v != "gitlab.company.com" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	if err := c.Set("clone.concurrency", "8"); err != nil || c.Clone.Concurrency != 8 {
		t.Fatalf("Set int: %v %d", err, c.Clone.Concurrency)
	}
	if err := c.Set("providers.github-personal.auth.app_id", "12345"); err != nil || c.Providers["github-personal"].Auth.AppID != "12345" {
		t.Fatalf("numeric string field: %v", err)
	}
	if err := c.Set("providers.bb.workspace", "other"); err != nil || c.Providers["bb"].Extra["workspace"] != "other" {
		t.Fatalf("extra field: %v", err)
	}
	if err := c.Set("providers.bb.auth.password", "x"); err == nil {
		t.Fatal("secret key accepted")
	}
	if err := c.Set("clone.concurrency", "lots"); err == nil {
		t.Fatal("type error not reported")
	}
	if err := c.Unset("providers.bb.workspace"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("providers.bb.workspace"); err == nil {
		t.Fatal("unset key still present")
	}
	if _, err := c.Get("no.such.key"); err == nil {
		t.Fatal("missing key returned a value")
	}
}

func TestDefaultPathHonorsEnv(t *testing.T) {
	t.Setenv("TROVE_CONFIG", "")
	x := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", x)
	p, err := DefaultPath()
	if err != nil || p != filepath.Join(x, "trove", "config.yaml") {
		t.Fatalf("DefaultPath = %q, %v", p, err)
	}
	t.Setenv("TROVE_CONFIG", filepath.Join(x, "custom.yaml"))
	if p, _ := DefaultPath(); p != filepath.Join(x, "custom.yaml") {
		t.Fatalf("TROVE_CONFIG ignored: %s", p)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil || len(c.Providers) != 0 || c.Version != CurrentVersion {
		t.Fatalf("Load missing = %+v, %v", c, err)
	}
}

func TestIsSecretKey(t *testing.T) {
	for _, k := range []string{"token", "providers.x.auth.token", "providers.x.client_secret", "api_key", "x.password", "x.private_key", "x.refresh_token"} {
		if !IsSecretKey(k) {
			t.Errorf("%s should be secret", k)
		}
	}
	for _, k := range []string{"providers.x.auth.secret_ref", "clone.concurrency", "providers.x.host", "tokens_dir"} {
		if IsSecretKey(k) {
			t.Errorf("%s should not be secret", k)
		}
	}
}
