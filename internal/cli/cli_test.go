package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/config"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/secrets"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// --- mock forge driver ------------------------------------------------------------

type mockBackend struct {
	mu      sync.Mutex
	repos   []domain.Repository
	deleted []string
	prs     []domain.PullRequest
}

type mockDriver struct{ be *mockBackend }

func (d mockDriver) Type() string               { return "mock" }
func (d mockDriver) DisplayName() string        { return "Mock Forge" }
func (d mockDriver) Description() string        { return "test double" }
func (d mockDriver) DefaultHost() string        { return "mock.example" }
func (d mockDriver) AuthMethods() []auth.Method { return []auth.Method{auth.MethodToken} }
func (d mockDriver) New(a forge.Account) (forge.Provider, error) {
	return &mockProvider{acct: a, be: d.be}, nil
}

type mockProvider struct {
	acct forge.Account
	be   *mockBackend
}

func (p *mockProvider) Metadata() forge.Metadata {
	return forge.Metadata{Name: p.acct.Name, Type: "mock", DisplayName: "Mock Forge", Host: p.acct.Host,
		APIURL: "https://" + p.acct.Host + "/api", WebURL: "https://" + p.acct.Host, Deployment: "cloud",
		AuthMethods: []auth.Method{auth.MethodToken},
		Terms:       forge.Terms{Repository: "Repository", PullRequest: "Change", PullRequestShort: "CH", Pipeline: "Pipeline", Snippet: "Snippet", Notification: "Notification", Namespace: "Org"}}
}

func (p *mockProvider) Capabilities() forge.Capabilities {
	return forge.NewCapabilities(forge.CapRepositories, forge.CapRepoDelete, forge.CapPullRequests, forge.CapSummary)
}

func (p *mockProvider) token(ctx context.Context) (string, error) {
	if p.acct.Credentials == nil {
		return "", errs.New(errs.ErrNotAuthenticated, "no credentials")
	}
	c, err := p.acct.Credentials.Load(ctx)
	if err != nil {
		return "", err
	}
	if c.Token != "good-token" {
		return "", &errs.Error{Kind: errs.ErrAuthenticationFailed, Provider: p.acct.Name, Message: "token is invalid or expired", Hint: "trove auth login " + p.acct.Name}
	}
	return c.Token, nil
}

func (p *mockProvider) CurrentUser(ctx context.Context) (*domain.User, error) {
	if _, err := p.token(ctx); err != nil {
		return nil, err
	}
	return &domain.User{ID: "1", Username: "tester"}, nil
}

func (p *mockProvider) Login(ctx context.Context, req auth.Request) (*auth.Result, error) {
	if req.Token != "good-token" {
		return nil, &errs.Error{Kind: errs.ErrAuthenticationFailed, Message: "token rejected"}
	}
	return &auth.Result{Credential: auth.Credential{Kind: auth.KindToken, Token: req.Token}, User: "tester"}, nil
}

func (p *mockProvider) AuthStatus(ctx context.Context) (*auth.Status, error) {
	u, err := p.CurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	return &auth.Status{Authenticated: true, User: u.Username, Method: auth.MethodToken}, nil
}

func (p *mockProvider) GitCredentials(ctx context.Context) (string, string, error) {
	t, err := p.token(ctx)
	return "x-mock", t, err
}

func (p *mockProvider) ListRepositories(ctx context.Context, o forge.ListRepositoryOptions) ([]domain.Repository, error) {
	if _, err := p.token(ctx); err != nil {
		return nil, err
	}
	p.be.mu.Lock()
	defer p.be.mu.Unlock()
	var out []domain.Repository
	for _, r := range p.be.repos {
		if o.Namespace != "" && r.Namespace != o.Namespace {
			continue
		}
		r.Provider = p.acct.Name
		out = append(out, r)
		if o.Limit > 0 && len(out) == o.Limit {
			break
		}
	}
	return out, nil
}

func (p *mockProvider) GetRepository(ctx context.Context, ref domain.RepositoryRef) (*domain.Repository, error) {
	repos, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{})
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		if r.FullName == ref.FullName() {
			return &r, nil
		}
	}
	return nil, &errs.Error{Kind: errs.ErrRepositoryNotFound, Cause: errs.ErrNotFound, Message: "repository " + ref.FullName() + " not found"}
}

func (p *mockProvider) RepositoryURLs(ref domain.RepositoryRef) domain.RepositoryURLs {
	return domain.RepositoryURLs{Web: "https://mock.example/" + ref.FullName(), HTTPS: "https://mock.example/" + ref.FullName() + ".git"}
}

func (p *mockProvider) DeleteRepository(ctx context.Context, ref domain.RepositoryRef) error {
	if _, err := p.token(ctx); err != nil {
		return err
	}
	p.be.mu.Lock()
	defer p.be.mu.Unlock()
	p.be.deleted = append(p.be.deleted, ref.FullName())
	return nil
}

func (p *mockProvider) ListPullRequests(ctx context.Context, ref domain.RepositoryRef, o forge.PullRequestListOptions) ([]domain.PullRequest, error) {
	return p.be.prs, nil
}

func (p *mockProvider) GetPullRequest(ctx context.Context, ref domain.RepositoryRef, n int) (*domain.PullRequest, error) {
	for _, pr := range p.be.prs {
		if pr.Number == n {
			return &pr, nil
		}
	}
	return nil, errs.New(errs.ErrNotFound, "no such change")
}

func (p *mockProvider) Summary(ctx context.Context) (*domain.AccountSummary, error) {
	n := len(p.be.repos)
	return &domain.AccountSummary{Repositories: &n}, nil
}

// --- harness ------------------------------------------------------------------

type harness struct {
	t       *testing.T
	dir     string
	cfgPath string
	be      *mockBackend
	mem     *secrets.MemoryProvider
	env     map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, dir: dir, cfgPath: filepath.Join(dir, "config.yaml"), be: &mockBackend{},
		mem: secrets.NewMemoryProvider("mem"), env: map[string]string{}}
	return h
}

func (h *harness) writeConfig(yaml string) {
	h.t.Helper()
	if err := os.WriteFile(h.cfgPath, []byte(yaml), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

type result struct {
	code   int
	stdout string
	stderr string
}

func (h *harness) run(stdin string, args ...string) result {
	h.t.Helper()
	var out, errb bytes.Buffer
	tio := terminal.Test(strings.NewReader(stdin), &out, &errb)
	f := &Factory{IO: tio, Deps: app.Deps{
		RegisterDrivers: func(r *forge.Registry) { r.MustRegister(mockDriver{be: h.be}) },
		RegisterSecrets: func(r *secrets.Resolver, _ config.SecretsConfig) { r.Register(h.mem) },
		Getenv:          func(k string) string { return h.env[k] },
		CacheDir:        filepath.Join(h.dir, "cache"),
	}}
	code := Execute(context.Background(), f, append([]string{"--config", h.cfgPath}, args...))
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

const baseConfig = `version: 1
default_provider: mock1
providers:
  mock1:
    type: mock
    host: mock.example
    auth:
      type: token
      secret_ref: mem://trove/mock/mock1/token
  mock2:
    type: mock
    host: other.example
secrets:
  provider: mem
clone:
  concurrency: 2
`

func (h *harness) login() {
	h.t.Helper()
	if err := h.mem.Set(context.Background(), "trove/mock/mock1/token", "good-token"); err != nil {
		h.t.Fatal(err)
	}
}

// --- tests --------------------------------------------------------------------

func TestProviderListAndUse(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	r := h.run("", "provider", "list", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var accts []domain.ProviderAccount
	if err := json.Unmarshal([]byte(r.stdout), &accts); err != nil {
		t.Fatalf("invalid JSON %q: %v", r.stdout, err)
	}
	if len(accts) != 2 || !accts[0].Default || accts[1].Default {
		t.Fatalf("unexpected accounts: %+v", accts)
	}
	if accts[1].SecretRef != "mem://trove/mock/mock2/token" {
		t.Errorf("default secret ref = %q", accts[1].SecretRef)
	}
	if r := h.run("", "provider", "use", "mock2"); r.code != 0 {
		t.Fatalf("use: %d %s", r.code, r.stderr)
	}
	cfg, err := config.Load(h.cfgPath)
	if err != nil || cfg.DefaultProvider != "mock2" {
		t.Fatalf("default not persisted: %v %v", cfg.DefaultProvider, err)
	}
	if r := h.run("", "provider", "use", "nope"); r.code != 4 {
		t.Fatalf("unknown provider exit = %d", r.code)
	}
	r = h.run("", "provider", "list")
	if !strings.Contains(r.stdout, "mock1") || !strings.Contains(r.stdout, "◆") {
		t.Fatalf("human list missing content:\n%s", r.stdout)
	}
}

func TestProviderAddNonInteractive(t *testing.T) {
	h := newHarness(t)
	h.writeConfig("version: 1\nsecrets:\n  provider: mem\n")
	r := h.run("", "provider", "add", "work", "--type", "mock", "--host", "git.corp.example", "--non-interactive")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	cfg, err := config.Load(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	pc := cfg.Providers["work"]
	if pc == nil || pc.Host != "git.corp.example" || cfg.DefaultProvider != "work" {
		t.Fatalf("provider not saved correctly: %+v default=%s", pc, cfg.DefaultProvider)
	}
	if st, _ := os.Stat(h.cfgPath); st.Mode().Perm() != 0o600 {
		t.Errorf("config permissions = %v, want 0600", st.Mode().Perm())
	}
	if r := h.run("", "provider", "add", "work", "--type", "mock", "--non-interactive"); r.code == 0 {
		t.Fatal("duplicate alias accepted")
	}
	if r := h.run("", "provider", "add", "x", "--type", "nope", "--non-interactive"); r.code != 5 {
		t.Fatalf("unknown type exit = %d (%s)", r.code, r.stderr)
	}
	if r := h.run("", "provider", "add", "bad.alias", "--type", "mock", "--non-interactive"); r.code != 2 {
		t.Fatalf("invalid alias exit = %d", r.code)
	}
}

func TestRepoListJSONAndAuthErrors(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.be.repos = []domain.Repository{
		{ID: "1", Name: "api", Namespace: "acme", FullName: "acme/api", Visibility: domain.VisibilityPrivate, ProviderType: "mock"},
		{ID: "2", Name: "web", Namespace: "acme/frontend", FullName: "acme/frontend/web", Visibility: domain.VisibilityPublic, ProviderType: "mock"},
	}
	// Not logged in: typed error, exit code 3, JSON error on stderr only.
	r := h.run("", "repo", "list", "--json")
	if r.code != 3 || r.stdout != "" || !strings.Contains(r.stderr, `"not_authenticated"`) {
		t.Fatalf("unauthenticated: code=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
	}
	h.login()
	r = h.run("", "repo", "list", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var repos []domain.Repository
	if err := json.Unmarshal([]byte(r.stdout), &repos); err != nil || len(repos) != 2 {
		t.Fatalf("bad JSON: %v %q", err, r.stdout)
	}
	r = h.run("", "repo", "list", "--quiet", "--namespace", "acme")
	if strings.TrimSpace(r.stdout) != "acme/api" {
		t.Fatalf("quiet output = %q", r.stdout)
	}
	r = h.run("", "repo", "list")
	if !strings.Contains(r.stdout, "acme/frontend/web") || !strings.Contains(r.stdout, "Mock Forge") {
		t.Fatalf("human output:\n%s", r.stdout)
	}
	// Invalid credential → ErrAuthenticationFailed, never echoing the token.
	_ = h.mem.Set(context.Background(), "trove/mock/mock1/token", "bad-token-value")
	r = h.run("", "--no-cache", "repo", "list")
	if r.code != 3 || strings.Contains(r.stderr, "bad-token-value") || !strings.Contains(r.stderr, "trove auth login mock1") {
		t.Fatalf("auth failure rendering: code=%d stderr=%s", r.code, r.stderr)
	}
}

func TestEnvTokenOverridesAndIsNotPersisted(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.be.repos = []domain.Repository{{ID: "1", Name: "api", Namespace: "acme", FullName: "acme/api", Visibility: domain.VisibilityPrivate}}
	h.env["TROVE_TOKEN"] = "good-token"
	r := h.run("", "repo", "list", "--quiet")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "acme/api" {
		t.Fatalf("env token not used: %d %q %s", r.code, r.stdout, r.stderr)
	}
	if ok, _ := h.mem.Exists(context.Background(), "trove/mock/mock1/token"); ok {
		t.Fatal("environment credential was persisted")
	}
	// TROVE_TOKEN applies only to the selected provider; mock2 has none.
	h.env["TROVE_PROVIDER"] = "mock2"
	delete(h.env, "TROVE_TOKEN")
	h.env["TROVE_TOKEN_MOCK1"] = "good-token"
	if r := h.run("", "repo", "list"); r.code != 3 {
		t.Fatalf("mock2 should be unauthenticated, got %d", r.code)
	}
	delete(h.env, "TROVE_PROVIDER")
	if r := h.run("", "repo", "list", "--quiet"); r.code != 0 {
		t.Fatalf("TROVE_TOKEN_MOCK1 not used: %d %s", r.code, r.stderr)
	}
}

func TestAuthLoginStatusLogout(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	r := h.run("bad\n", "auth", "login", "mock1", "--with-token")
	if r.code != 3 {
		t.Fatalf("bad token login exit = %d (%s)", r.code, r.stderr)
	}
	r = h.run("good-token\n", "auth", "login", "mock1", "--with-token")
	if r.code != 0 {
		t.Fatalf("login exit %d: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stdout+r.stderr, "good-token") {
		t.Fatal("login output echoed the token")
	}
	if v, err := h.mem.Get(context.Background(), "trove/mock/mock1/token"); err != nil || v != "good-token" {
		t.Fatalf("token not stored: %v", err)
	}
	b, _ := os.ReadFile(h.cfgPath)
	if strings.Contains(string(b), "good-token") {
		t.Fatal("token written to config file")
	}
	r = h.run("", "auth", "status", "mock1", "--json")
	if r.code != 0 {
		t.Fatalf("status exit %d: %s", r.code, r.stderr)
	}
	var rows []authStatusRow
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 1 || rows[0].Status == nil || rows[0].Status.User != "tester" {
		t.Fatalf("status JSON: %v %s", err, r.stdout)
	}
	if strings.Contains(r.stdout, "good-token") {
		t.Fatal("status leaked the token")
	}
	// Non-interactive login without --with-token must not hang or prompt.
	if r := h.run("", "auth", "login", "mock2", "--non-interactive"); r.code != 2 {
		t.Fatalf("non-interactive login exit = %d", r.code)
	}
	if r := h.run("", "auth", "logout", "mock1"); r.code != 0 {
		t.Fatalf("logout exit %d: %s", r.code, r.stderr)
	}
	if ok, _ := h.mem.Exists(context.Background(), "trove/mock/mock1/token"); ok {
		t.Fatal("credential not deleted by logout")
	}
	if r := h.run("", "auth", "status", "mock1"); r.code != 3 {
		t.Fatalf("status after logout exit = %d", r.code)
	}
	if r := h.run("", "auth", "refresh", "mock1"); r.code != 5 {
		t.Fatalf("refresh on provider without Refresher exit = %d", r.code)
	}
}

func TestUnsupportedCapability(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.login()
	r := h.run("", "pipeline", "list", "-R", "acme/api")
	if r.code != 5 || !strings.Contains(r.stderr, `Provider "mock1" does not support pipelines`) {
		t.Fatalf("code=%d stderr=%s", r.code, r.stderr)
	}
	r = h.run("", "release", "list", "-R", "acme/api", "--json")
	if r.code != 5 || !strings.Contains(r.stderr, `"unsupported"`) {
		t.Fatalf("json unsupported: code=%d stderr=%s", r.code, r.stderr)
	}
}

func TestPRTerminology(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.login()
	h.be.prs = []domain.PullRequest{{Number: 7, Title: "Add retries", State: domain.PullRequestOpen, SourceBranch: "feat", TargetBranch: "main"}}
	r := h.run("", "pr", "list", "-R", "acme/api")
	if r.code != 0 || !strings.Contains(r.stdout, "#7") || !strings.Contains(r.stdout, "feat → main") {
		t.Fatalf("pr list: %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	r = h.run("", "pr", "view", "7", "-R", "acme/api")
	if !strings.Contains(r.stdout, "CH #7") {
		t.Fatalf("provider terminology not used:\n%s", r.stdout)
	}
	if r := h.run("", "pr", "view", "abc", "-R", "acme/api"); r.code != 2 {
		t.Fatalf("invalid number exit = %d", r.code)
	}
}

func TestRepoDeleteRequiresConfirmation(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.login()
	r := h.run("", "repo", "delete", "acme/prod")
	if r.code != 2 || len(h.be.deleted) != 0 {
		t.Fatalf("delete without --yes in non-interactive mode must refuse: code=%d deleted=%v", r.code, h.be.deleted)
	}
	r = h.run("", "repo", "delete", "acme/prod", "--yes")
	if r.code != 0 || len(h.be.deleted) != 1 || h.be.deleted[0] != "acme/prod" {
		t.Fatalf("delete --yes: code=%d deleted=%v stderr=%s", r.code, h.be.deleted, r.stderr)
	}
	if r := h.run("", "repo", "delete", "mock1:acme/x", "-P", "mock2", "--yes"); r.code != 2 {
		t.Fatalf("conflicting provider prefix exit = %d", r.code)
	}
}

func TestRepoCloneBulkAndSummary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.login()
	src := t.TempDir()
	for _, name := range []string{"api", "web", "docs"} {
		makeGitRepo(t, filepath.Join(src, name))
	}
	for _, name := range []string{"api", "web", "docs"} {
		h.be.repos = append(h.be.repos, domain.Repository{ID: name, Name: name, Namespace: "acme", FullName: "acme/" + name,
			Visibility: domain.VisibilityPrivate, URLs: domain.RepositoryURLs{HTTPS: filepath.Join(src, name), Web: "https://mock.example/acme/" + name}})
	}
	h.be.repos = append(h.be.repos, domain.Repository{ID: "broken", Name: "broken", Namespace: "acme", FullName: "acme/broken",
		Visibility: domain.VisibilityPrivate, URLs: domain.RepositoryURLs{HTTPS: filepath.Join(src, "does-not-exist")}})
	dest := filepath.Join(h.dir, "src")
	r := h.run("", "repo", "clone", "--all", "--dir", dest, "--json", "--retries", "0")
	if r.code == 0 {
		t.Fatalf("expected failure exit for the broken repo")
	}
	var sum struct {
		Cloned, Skipped, Failed int
		Results                 []struct{ Repository, State string }
	}
	if err := json.Unmarshal([]byte(r.stdout), &sum); err != nil {
		t.Fatalf("summary JSON: %v\n%s\n%s", err, r.stdout, r.stderr)
	}
	if sum.Cloned != 3 || sum.Failed != 1 || sum.Skipped != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	for _, name := range []string{"api", "web", "docs"} {
		if _, err := os.Stat(filepath.Join(dest, "acme", name, ".git")); err != nil {
			t.Errorf("%s not cloned into namespace layout: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "acme", "broken")); !os.IsNotExist(err) {
		t.Errorf("failed clone left a directory behind")
	}
	// Second run skips existing clones.
	r = h.run("", "repo", "clone", "--all", "--dir", dest, "--namespace", "acme", "--json", "--retries", "0")
	_ = json.Unmarshal([]byte(r.stdout), &sum)
	if sum.Skipped != 3 {
		t.Fatalf("existing clones should be skipped: %+v", sum)
	}
	// Human summary format.
	r = h.run("", "repo", "clone", "--all", "--dir", dest, "--retries", "0")
	if !strings.Contains(r.stdout, "Cloned:") || !strings.Contains(r.stdout, "Failed:") || !strings.Contains(r.stdout, "acme/broken") {
		t.Fatalf("human summary:\n%s", r.stdout)
	}
	// The failure was recorded in the (test-isolated) cache dir; after the
	// source appears, --retry-failed clones it into its original destination.
	if _, err := os.Stat(filepath.Join(h.dir, "cache", "state", "clone-failures.json")); err != nil {
		t.Fatalf("failure record missing: %v", err)
	}
	makeGitRepo(t, filepath.Join(src, "does-not-exist"))
	r = h.run("", "repo", "clone", "--retry-failed", "--json")
	if r.code != 0 {
		t.Fatalf("retry-failed: %d %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dest, "acme", "broken", ".git")); err != nil {
		t.Fatalf("retry did not clone into the original destination: %v", err)
	}
	if r := h.run("", "repo", "clone", "--retry-failed"); r.code != 4 {
		t.Fatalf("nothing left to retry should be not-found, got %d", r.code)
	}
	// Single explicit clone uses a flat layout like git clone.
	flat := filepath.Join(h.dir, "flat")
	r = h.run("", "repo", "clone", "acme/api", "--dir", flat)
	if r.code != 0 {
		t.Fatalf("single clone: %d %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(flat, "api", "README")); err != nil {
		t.Fatalf("single clone not flat: %v", err)
	}
}

func TestRepoCloneNonInteractiveNeedsTargets(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.login()
	r := h.run("", "repo", "clone")
	if r.code != 2 || !strings.Contains(r.stderr, "--all") {
		t.Fatalf("code=%d stderr=%s", r.code, r.stderr)
	}
}

func TestGitCredentialHelper(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.login()
	r := h.run("protocol=https\nhost=mock.example\npath=acme/api.git\n\n", "--provider", "mock1", "auth", "git-credential", "get")
	if r.code != 0 || r.stdout != "username=x-mock\npassword=good-token\n\n" {
		t.Fatalf("helper output = %q (code %d, %s)", r.stdout, r.code, r.stderr)
	}
	// A different host must never receive the credential.
	r = h.run("protocol=https\nhost=evil.example\n\n", "--provider", "mock1", "auth", "git-credential", "get")
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("credential leaked to foreign host: %q", r.stdout)
	}
	// Host-based detection without --provider.
	r = h.run("protocol=https\nhost=mock.example\n\n", "auth", "git-credential", "get")
	if !strings.Contains(r.stdout, "password=good-token") {
		t.Fatalf("detection by host failed: %q %s", r.stdout, r.stderr)
	}
	// store/erase are no-ops.
	if r := h.run("protocol=https\nhost=mock.example\n\n", "auth", "git-credential", "store"); r.code != 0 || r.stdout != "" {
		t.Fatalf("store should be a no-op: %q", r.stdout)
	}
}

func TestConfigCommands(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	r := h.run("", "config", "path")
	if strings.TrimSpace(r.stdout) != h.cfgPath {
		t.Fatalf("config path = %q", r.stdout)
	}
	if r := h.run("", "config", "set", "clone.concurrency", "8"); r.code != 0 {
		t.Fatalf("set: %d %s", r.code, r.stderr)
	}
	if r := h.run("", "config", "get", "clone.concurrency"); strings.TrimSpace(r.stdout) != "8" {
		t.Fatalf("get = %q", r.stdout)
	}
	if r := h.run("", "config", "set", "clone.concurrency", "999"); r.code != 2 {
		t.Fatalf("out-of-range concurrency accepted: %d", r.code)
	}
	if r := h.run("", "config", "set", "providers.mock1.auth.password", "hunter2"); r.code != 2 {
		t.Fatalf("secret-looking key accepted: %d", r.code)
	}
	b, _ := os.ReadFile(h.cfgPath)
	if strings.Contains(string(b), "hunter2") {
		t.Fatal("secret persisted")
	}
	r = h.run("", "config", "validate", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"valid": true`) {
		t.Fatalf("validate: %d %s %s", r.code, r.stdout, r.stderr)
	}
	h.writeConfig(baseConfig + "\ndefault_provider_typo: x\n")
	if r := h.run("", "config", "validate"); r.code != 2 {
		t.Fatalf("unknown root key should fail to load: %d %s", r.code, r.stderr)
	}
	h.writeConfig(strings.Replace(baseConfig, "default_provider: mock1", "default_provider: ghost", 1))
	r = h.run("", "config", "validate")
	if r.code != 2 || !strings.Contains(r.stdout, "default_provider") {
		t.Fatalf("invalid default not reported: %d %s", r.code, r.stdout)
	}
	r = h.run("", "config", "show", "--json")
	var v map[string]any
	if json.Unmarshal([]byte(r.stdout), &v) != nil || v["version"] == nil {
		t.Fatalf("config show --json: %s", r.stdout)
	}
}

func TestDoctor(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(baseConfig)
	h.login()
	r := h.run("", "doctor", "--json")
	var rep struct {
		Checks []struct{ Group, Name, Status string }
	}
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("doctor JSON: %v\n%s\n%s", err, r.stdout, r.stderr)
	}
	statuses := map[string]string{}
	for _, c := range rep.Checks {
		statuses[c.Name] = c.Status
	}
	if statuses["Config file"] != "ok" || statuses["mock1 authentication"] != "ok" {
		t.Fatalf("unexpected checks: %+v", statuses)
	}
	if statuses["mock2 authentication"] != "warn" {
		t.Fatalf("mock2 should warn (not logged in): %+v", statuses)
	}
	r = h.run("", "doctor")
	if !strings.Contains(r.stdout, "Trove Doctor") || !strings.Contains(r.stdout, "warning") {
		t.Fatalf("human doctor:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "good-token") {
		t.Fatal("doctor printed a secret")
	}
}

func TestVersionAndCompletion(t *testing.T) {
	h := newHarness(t)
	r := h.run("", "version", "--json")
	var v map[string]string
	if json.Unmarshal([]byte(r.stdout), &v) != nil || v["go_version"] == "" || v["os"] == "" {
		t.Fatalf("version JSON: %s", r.stdout)
	}
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		r := h.run("", "completion", sh)
		if r.code != 0 || !strings.Contains(r.stdout, "trove") {
			t.Errorf("completion %s failed: %d", sh, r.code)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	h := newHarness(t)
	if r := h.run("", "repo", "frobnicate"); r.code == 0 {
		t.Fatal("unknown subcommand succeeded")
	}
	if r := h.run("", "repo", "list", "--bogus"); r.code != 2 {
		t.Fatalf("unknown flag exit = %d", r.code)
	}
	if r := h.run("", "repo", "list", "--json", "--quiet"); r.code != 2 {
		t.Fatalf("--json --quiet exit = %d", r.code)
	}
}

func makeGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "--allow-empty", "-m", "init"},
	} {
		if args[0] == "-c" {
			if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "README")
		}
		gitCmd(t, dir, args...)
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
