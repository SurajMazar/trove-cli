package custom

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/forge/forgetest"
)

const allCaps = `[repositories, repositories.create, repositories.delete, pull_requests, pull_requests.create,
    pull_requests.merge, pull_requests.close, issues, issues.create, issues.state, issues.labels,
    pipelines, releases, namespaces, summary]`

// providerYAML renders a full provider block as it appears in Trove's config
// file; custom is the body of the `custom:` section (unindented).
func providerYAML(apiURL, custom string) string {
	var b strings.Builder
	b.WriteString("type: custom\nhost: git.example.com\n")
	if apiURL != "" {
		b.WriteString("api_base_url: " + apiURL + "\n")
	}
	b.WriteString("clone_base_url: https://git.example.com\nweb_base_url: https://git.example.com\nssh_host: ssh.example.com\n")
	b.WriteString("auth:\n  type: token\n  secret_ref: keychain://trove/custom/company/token\n")
	b.WriteString("custom:\n")
	for _, line := range strings.Split(strings.TrimSpace(custom), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

func account(y string, store auth.Store) forge.Account {
	return forge.Account{
		Name: "custom-company", Type: Type, Credentials: store,
		Decode: func(v any) error { return yaml.Unmarshal([]byte(y), v) },
	}
}

func newProvider(t *testing.T, y string, store auth.Store) *Provider {
	t.Helper()
	p, err := NewDriver().New(account(y, store))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*Provider)
}

func customBlock(strategy, extra string) string {
	return "display_name: Example Forge\ncapabilities: " + allCaps + "\npagination:\n  strategy: " + strategy + "\n" + extra
}

func TestContractPerPaginationStrategy(t *testing.T) {
	cases := []struct {
		name, strategy string
		nextPageHeader bool
		multiPage      bool
	}{
		{"link", paginateLink, false, true},
		{"page", paginatePage, false, true},
		{"page with X-Next-Page", paginatePage, true, true},
		{"cursor", paginateCursor, false, true},
		{"none", paginateNone, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTFP(t, tc.strategy)
			f.nextPageHeader = tc.nextPageHeader
			y := providerYAML(f.apiURL(), customBlock(tc.strategy, ""))
			p := newProvider(t, y, auth.TokenStore(fakeToken))
			bad := newProvider(t, y, auth.TokenStore("wrong-token"))

			forgetest.RunForgeProviderContractTests(t, p, forgetest.Options{
				ExistingRepo:    domain.RepositoryRef{Namespace: "acme/platform/tools", Name: "deployer"},
				MissingRepo:     domain.RepositoryRef{Namespace: "acme", Name: "nope"},
				MinRepositories: 250,
				Secret:          fakeToken,
				Unauthenticated: bad,
			})

			f.reset()
			repos, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{IncludeArchived: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(repos) != fixtureRepos+1 {
				t.Fatalf("got %d repositories, want %d", len(repos), fixtureRepos+1)
			}
			pages := len(f.recorded())
			if tc.multiPage && pages < 3 {
				t.Errorf("expected several page requests, got %d", pages)
			}
			if !tc.multiPage && pages != 1 {
				t.Errorf("strategy none: expected exactly one request, got %d", pages)
			}
			for _, rq := range f.recorded() {
				switch tc.strategy {
				case paginateLink, paginatePage, paginateCursor:
					if rq.Query.Get("per_page") != "100" {
						t.Errorf("%s: per_page = %q, want 100", rq.RequestURI, rq.Query.Get("per_page"))
					}
				}
			}

			// Archived repositories are excluded unless requested.
			active, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range active {
				if r.Archived {
					t.Fatalf("archived repository %s listed without IncludeArchived", r.FullName)
				}
			}
			if len(active) >= len(repos) {
				t.Errorf("archived filter had no effect: %d vs %d", len(active), len(repos))
			}
		})
	}
}

func TestNestedNamespaceEscaping(t *testing.T) {
	f := newFakeTFP(t, paginateLink)
	p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), auth.TokenStore(fakeToken))
	ctx := context.Background()
	ref := domain.RepositoryRef{Namespace: "acme/platform/tools", Name: "deployer"}

	r, err := p.GetRepository(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	last := f.last()
	if !strings.Contains(last.RequestURI, "/api/v1/repositories/acme%2Fplatform%2Ftools%2Fdeployer") {
		t.Errorf("RequestURI = %q, want escaped full name", last.RequestURI)
	}
	if last.EscPath != "/api/v1/repositories/acme%2Fplatform%2Ftools%2Fdeployer" {
		t.Errorf("escaped path = %q", last.EscPath)
	}
	if r.FullName != nestedRepo || r.Namespace != "acme/platform/tools" || r.Name != "deployer" {
		t.Errorf("repository identity not normalized: %+v", r)
	}

	if _, err := p.GetPullRequest(ctx, ref, 1); err != nil {
		t.Fatal(err)
	}
	if got := f.last().EscPath; got != "/api/v1/repositories/acme%2Fplatform%2Ftools%2Fdeployer/pull-requests/1" {
		t.Errorf("PR path = %q", got)
	}
	if _, err := p.GetNamespace(ctx, "acme/platform"); err != nil {
		t.Fatal(err)
	}
	if got := f.last().RequestURI; got != "/api/v1/namespaces/acme%2Fplatform" {
		t.Errorf("namespace RequestURI = %q", got)
	}
	if got := p.RepositoryURLs(ref).API; got != f.apiURL()+"/v1/repositories/acme%2Fplatform%2Ftools%2Fdeployer" {
		t.Errorf("API URL = %q", got)
	}

	// Namespace filter is sent and enforced (including nested namespaces).
	repos, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{Namespace: fixtureNSPath, IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.last().Query.Get("namespace") != fixtureNSPath {
		t.Errorf("namespace query = %q", f.last().Query.Get("namespace"))
	}
	for _, r := range repos {
		if r.Namespace != fixtureNSPath && !strings.HasPrefix(r.Namespace, fixtureNSPath+"/") {
			t.Errorf("repository %s outside namespace filter", r.FullName)
		}
	}
	if len(repos) == 0 {
		t.Fatal("namespace filter returned nothing")
	}
}

func TestConfigValidation(t *testing.T) {
	const api = "https://git.example.com/api"
	cases := []struct {
		name   string
		yaml   string
		expect string
	}{
		{"missing api_base_url", providerYAML("", "capabilities: [repositories]"), "api_base_url is required"},
		{"relative api_base_url", providerYAML("git.example.com/api", "capabilities: [repositories]"), "absolute http(s) URL"},
		{"ftp api_base_url", providerYAML("ftp://git.example.com", "capabilities: [repositories]"), "absolute http(s) URL"},
		{"unknown capability", providerYAML(api, "capabilities: [repositories, teleport]"), `"teleport"`},
		{"unsupported capability", providerYAML(api, "capabilities: [repositories, notifications]"), `"notifications" is not supported by Trove Forge Protocol v1`},
		{"unsupported sub-capability", providerYAML(api, "capabilities: [repositories, repositories.fork]"), `"repositories.fork" is not supported`},
		{"sub-capability without parent", providerYAML(api, "capabilities: [repositories.create]"), `"repositories.create" requires "repositories"`},
		{"issue labels without issues", providerYAML(api, "capabilities: [repositories, issues.labels]"), `"issues.labels" requires "issues"`},
		{"bad pagination strategy", providerYAML(api, "pagination:\n  strategy: offset"), `pagination.strategy "offset"`},
		{"negative page size", providerYAML(api, "pagination:\n  max_page_size: -5"), "max_page_size"},
		{"bad clone strategy", providerYAML(api, "clone:\n  strategy: magic"), `clone.strategy "magic"`},
		{"unknown placeholder", providerYAML(api, "clone:\n  https: \"{clone_base_url}/{owner}/{name}.git\""), "{owner}"},
		{"unbalanced template", providerYAML(api, "clone:\n  web: \"{web_base_url}/{namespace\""), "unbalanced"},
		{"https template not http", providerYAML(api, "clone:\n  https: \"git://{host}/{full_name}\""), "does not produce an http(s) URL"},
		{"bad header", providerYAML(api, "auth:\n  header: \"X Bad Header\""), "auth.header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDriver().New(account(tc.yaml, nil))
			if err == nil {
				t.Fatal("New succeeded, want configuration error")
			}
			if !errors.Is(err, errs.ErrInvalidConfiguration) {
				t.Errorf("want ErrInvalidConfiguration, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.expect)
			}
		})
	}

	t.Run("template strategy needs clone_base_url", func(t *testing.T) {
		y := "type: custom\nhost: git.example.com\napi_base_url: " + api + "\n"
		_, err := NewDriver().New(account(y, nil))
		if !errors.Is(err, errs.ErrInvalidConfiguration) || !strings.Contains(err.Error(), "clone_base_url is required") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("reports every problem", func(t *testing.T) {
		_, err := NewDriver().New(account(providerYAML("", "capabilities: [bogus]\npagination:\n  strategy: nope"), nil))
		for _, want := range []string{"api_base_url", "bogus", "nope"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v does not mention %q", err, want)
			}
		}
	})

	t.Run("defaults", func(t *testing.T) {
		y := "type: custom\napi_base_url: https://forge.corp.example:8443/tfp/\nclone_base_url: https://forge.corp.example\n"
		p := newProvider(t, y, nil)
		m := p.Metadata()
		if m.DisplayName != "Custom Forge" || m.Host != "forge.corp.example:8443" || m.Deployment != "custom" || m.Type != "custom" {
			t.Errorf("metadata = %+v", m)
		}
		if m.APIURL != "https://forge.corp.example:8443/tfp" {
			t.Errorf("APIURL = %q", m.APIURL)
		}
		if m.WebURL != "https://forge.corp.example:8443" {
			t.Errorf("WebURL = %q", m.WebURL)
		}
		if got := p.Capabilities().List(); !reflect.DeepEqual(got, []forge.Capability{forge.CapRepositories}) {
			t.Errorf("default capabilities = %v", got)
		}
		if !reflect.DeepEqual(m.AuthMethods, []auth.Method{auth.MethodToken}) {
			t.Errorf("auth methods = %v", m.AuthMethods)
		}
		u := p.RepositoryURLs(domain.RepositoryRef{Namespace: "a/b", Name: "c"})
		if u.SSH != "git@forge.corp.example:a/b/c.git" || u.HTTPS != "https://forge.corp.example/a/b/c.git" {
			t.Errorf("default URLs = %+v", u)
		}
		if m.Terms.PullRequest != "Pull Request" || m.Terms.Namespace == "" {
			t.Errorf("terms = %+v", m.Terms)
		}
	})

	t.Run("account fields override decoded ones and terms apply", func(t *testing.T) {
		acct := account(providerYAML("https://ignored.example/api", "capabilities: [repositories, pull_requests]\nterms:\n  pull_request: Change Request\n  pull_request_short: CR"), nil)
		acct.APIURL = "https://real.example/api"
		acct.Host = "real.example"
		p, err := NewDriver().New(acct)
		if err != nil {
			t.Fatal(err)
		}
		m := p.Metadata()
		if m.APIURL != "https://real.example/api" || m.Host != "real.example" {
			t.Errorf("metadata = %+v", m)
		}
		if m.Terms.PullRequest != "Change Request" || m.Terms.PullRequestShort != "CR" {
			t.Errorf("terms = %+v", m.Terms)
		}
	})
}

// failTransport fails the test if any request is made.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("network used")
	return nil, errors.New("no network")
}

type failStore struct{ t *testing.T }

func (f failStore) Load(context.Context) (auth.Credential, error) {
	f.t.Error("credentials loaded")
	return auth.Credential{}, errors.New("no")
}
func (failStore) Save(context.Context, auth.Credential) error { return nil }
func (failStore) Delete(context.Context) error                { return nil }

func TestConstructionIsOffline(t *testing.T) {
	acct := account(providerYAML("https://git.example.com/api", customBlock(paginateLink, "")), failStore{t})
	acct.Transport = failTransport{t}
	p, err := NewDriver().New(acct)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Metadata()
	_ = p.Capabilities()
	_ = p.(*Provider).RepositoryURLs(domain.RepositoryRef{Namespace: "a", Name: "b"})
}

func TestUndeclaredCapabilityRefused(t *testing.T) {
	f := newFakeTFP(t, paginateLink)
	p := newProvider(t, providerYAML(f.apiURL(), "capabilities: [repositories, issues]"), auth.TokenStore(fakeToken))
	ref := domain.RepositoryRef{Namespace: "acme/platform/tools", Name: "deployer"}
	ctx := context.Background()

	if _, err := forge.As[forge.RepositoryDeleter](p, forge.CapRepoDelete); !errors.Is(err, errs.ErrUnsupportedCapability) {
		t.Errorf("forge.As(repositories.delete): want unsupported, got %v", err)
	}
	if _, err := forge.As[forge.PullRequestProvider](p, forge.CapPullRequests); !errors.Is(err, errs.ErrUnsupportedCapability) {
		t.Errorf("forge.As(pull_requests): want unsupported, got %v", err)
	}
	if _, err := forge.As[forge.IssueProvider](p, forge.CapIssues); err != nil {
		t.Errorf("forge.As(issues): %v", err)
	}
	// Direct calls that bypass forge.As are refused too, without a request.
	checks := map[string]error{
		"DeleteRepository": p.DeleteRepository(ctx, ref),
		"ClosePullRequest": p.ClosePullRequest(ctx, ref, 1),
	}
	_, checks["Summary"] = p.Summary(ctx)
	_, checks["ListIssues with labels"] = p.ListIssues(ctx, ref, forge.IssueListOptions{Labels: []string{"a"}})
	_, checks["CreateIssue"] = p.CreateIssue(ctx, ref, forge.CreateIssueRequest{Title: "x"})
	for name, err := range checks {
		if !errors.Is(err, errs.ErrUnsupportedCapability) {
			t.Errorf("%s: want ErrUnsupportedCapability, got %v", name, err)
		}
	}
	if n := len(f.recorded()); n != 0 {
		t.Errorf("%d requests made for undeclared capabilities", n)
	}
	if problems := forge.ValidateCapabilities(p); len(problems) != 0 {
		t.Errorf("ValidateCapabilities: %v", problems)
	}
}

func TestCloneStrategies(t *testing.T) {
	ctx := context.Background()
	t.Run("template", func(t *testing.T) {
		f := newFakeTFP(t, paginateLink)
		p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "clone:\n  strategy: template\n  ssh: \"ssh://git@{ssh_host}:2222/{full_name}.git\"")), auth.TokenStore(fakeToken))
		repos, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{ListOptions: forge.ListOptions{Limit: 4}, IncludeArchived: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range repos {
			if r.URLs.HTTPS != "https://git.example.com/"+r.FullName+".git" {
				t.Errorf("HTTPS = %q (template must win over server URLs)", r.URLs.HTTPS)
			}
			if r.URLs.SSH != "ssh://git@ssh.example.com:2222/"+r.FullName+".git" {
				t.Errorf("SSH = %q", r.URLs.SSH)
			}
			if r.URLs.Web != "https://git.example.com/"+r.FullName {
				t.Errorf("Web = %q", r.URLs.Web)
			}
			if r.Provider != "custom-company" || r.ProviderType != "custom" {
				t.Errorf("server-supplied provider fields leaked: %q/%q", r.Provider, r.ProviderType)
			}
		}
	})
	t.Run("api", func(t *testing.T) {
		f := newFakeTFP(t, paginateLink)
		p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "clone:\n  strategy: api")), auth.TokenStore(fakeToken))
		repos, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{ListOptions: forge.ListOptions{Limit: 4}, IncludeArchived: true})
		if err != nil {
			t.Fatal(err)
		}
		// Fixture: even-indexed repos carry server URLs, odd ones none.
		if got := repos[0].URLs.HTTPS; got != "https://forge.internal/git/acme/repo-000.git" {
			t.Errorf("server HTTPS URL not used: %q", got)
		}
		if got := repos[0].URLs.SSH; got != "ssh://git@forge.internal:2222/acme/repo-000.git" {
			t.Errorf("server SSH URL not used: %q", got)
		}
		if got := repos[1].URLs.HTTPS; got != "https://git.example.com/acme/platform/repo-001.git" {
			t.Errorf("missing HTTPS URL not filled from template: %q", got)
		}
		if got := repos[1].URLs.SSH; got != "git@ssh.example.com:acme/platform/repo-001.git" {
			t.Errorf("missing SSH URL not filled from template: %q", got)
		}
	})
	t.Run("api without clone_base_url", func(t *testing.T) {
		y := "type: custom\nhost: git.example.com\napi_base_url: https://git.example.com/api\ncustom:\n  clone:\n    strategy: api\n"
		p := newProvider(t, y, nil)
		u := p.RepositoryURLs(domain.RepositoryRef{Namespace: "a", Name: "b"})
		if u.HTTPS != "" || u.Web != "https://git.example.com/a/b" || u.SSH != "git@git.example.com:a/b.git" {
			t.Errorf("URLs = %+v", u)
		}
	})
}

func TestAuthHeaderSchemes(t *testing.T) {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:"+fakeToken))
	cases := []struct {
		name, cfg, header, value string
		methods                  []auth.Method
	}{
		{"bearer default", "", "Authorization", "Bearer " + fakeToken, []auth.Method{auth.MethodToken}},
		{"raw token", "auth:\n  scheme: \"\"", "Authorization", fakeToken, []auth.Method{auth.MethodToken}},
		{"custom header and scheme", "auth:\n  header: X-Forge-Key\n  scheme: Token", "X-Forge-Key", "Token " + fakeToken, []auth.Method{auth.MethodToken}},
		{"basic", "auth:\n  scheme: basic\n  username: alice", "Authorization", basic, []auth.Method{auth.MethodToken, auth.MethodBasic}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTFP(t, paginateLink)
			f.authHeader, f.authValue = tc.header, tc.value
			var logs bytes.Buffer
			acct := account(providerYAML(f.apiURL(), customBlock(paginateLink, tc.cfg)), auth.TokenStore(fakeToken))
			acct.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			pp, err := NewDriver().New(acct)
			if err != nil {
				t.Fatal(err)
			}
			p := pp.(*Provider)
			u, err := p.CurrentUser(context.Background())
			if err != nil {
				t.Fatalf("CurrentUser: %v", err)
			}
			if u.Username != "alice" {
				t.Errorf("user = %+v", u)
			}
			if !reflect.DeepEqual(p.Metadata().AuthMethods, tc.methods) {
				t.Errorf("auth methods = %v, want %v", p.Metadata().AuthMethods, tc.methods)
			}
			if !strings.Contains(logs.String(), "http request") {
				t.Error("expected debug request logs")
			}
			if strings.Contains(logs.String(), fakeToken) || strings.Contains(logs.String(), strings.TrimPrefix(basic, "Basic ")) {
				t.Errorf("debug log leaks the credential:\n%s", logs.String())
			}
		})
	}

	t.Run("basic without username", func(t *testing.T) {
		f := newFakeTFP(t, paginateLink)
		p := newProvider(t, providerYAML(f.apiURL(), "auth:\n  scheme: basic"), auth.TokenStore(fakeToken))
		if _, err := p.CurrentUser(context.Background()); !errors.Is(err, errs.ErrInvalidConfiguration) {
			t.Errorf("want ErrInvalidConfiguration, got %v", err)
		}
	})
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		code, msg  string
		retryAfter string
		repo       bool
		want       error
		wantRetry  time.Duration
		wantMsg    string
	}{
		{name: "401", status: 401, code: "unauthorized", want: errs.ErrAuthenticationFailed, wantMsg: "authentication failed"},
		{name: "403", status: 403, code: "forbidden", msg: "no access to acme", want: errs.ErrPermissionDenied, wantMsg: "no access to acme"},
		{name: "403 with Retry-After", status: 403, retryAfter: "120", want: errs.ErrRateLimited, wantRetry: 120 * time.Second},
		{name: "404 repository", status: 404, code: "not_found", repo: true, want: errs.ErrRepositoryNotFound},
		{name: "404 other", status: 404, code: "not_found", want: errs.ErrNotFound},
		{name: "409", status: 409, code: "conflict", msg: "already exists", want: errs.ErrConflict, wantMsg: "already exists"},
		{name: "422", status: 422, code: "invalid", msg: "name is too long", want: errs.ErrInvalidArgument, wantMsg: "name is too long"},
		{name: "400 without envelope", status: 400, want: errs.ErrInvalidArgument},
		{name: "429 Retry-After", status: 429, code: "rate_limited", retryAfter: "120", want: errs.ErrRateLimited, wantRetry: 120 * time.Second, wantMsg: "retry after 2m0s"},
		{name: "code wins over status", status: 400, code: "rate_limited", want: errs.ErrRateLimited},
		{name: "500", status: 500, code: "boom", msg: "database down", want: errs.ErrProviderAPI, wantMsg: "database down"},
		{name: "echoed token scrubbed", status: 403, code: "forbidden", msg: "token " + fakeToken + " lacks scope", want: errs.ErrPermissionDenied, wantMsg: "lacks scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTFP(t, paginateLink)
			f.handler = func(w http.ResponseWriter, r *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				if tc.code == "" {
					w.WriteHeader(tc.status)
					return
				}
				writeErr(w, tc.status, tc.code, tc.msg)
			}
			p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), auth.TokenStore(fakeToken))
			var err error
			if tc.repo {
				_, err = p.GetRepository(context.Background(), domain.RepositoryRef{Namespace: "a", Name: "b"})
			} else {
				_, err = p.GetIssue(context.Background(), domain.RepositoryRef{Namespace: "a", Name: "b"}, 1)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if tc.repo && !errors.Is(err, errs.ErrNotFound) {
				t.Error("ErrRepositoryNotFound must also match ErrNotFound")
			}
			if !tc.repo && tc.want == errs.ErrNotFound && errors.Is(err, errs.ErrRepositoryNotFound) {
				t.Error("non-repository 404 must not be ErrRepositoryNotFound")
			}
			var e *errs.Error
			if !errors.As(err, &e) {
				t.Fatalf("not an *errs.Error: %T", err)
			}
			if e.Provider != "custom-company" || e.Status != tc.status || e.Op == "" {
				t.Errorf("error context: provider=%q status=%d op=%q", e.Provider, e.Status, e.Op)
			}
			if e.RetryAfter != tc.wantRetry {
				t.Errorf("RetryAfter = %v, want %v", e.RetryAfter, tc.wantRetry)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantMsg)
			}
			if strings.Contains(err.Error(), fakeToken) {
				t.Errorf("error leaks token: %q", err.Error())
			}
			if tc.want == errs.ErrAuthenticationFailed && e.Hint != "trove auth login custom-company" {
				t.Errorf("hint = %q", e.Hint)
			}
		})
	}
}

func TestNotLoggedIn(t *testing.T) {
	f := newFakeTFP(t, paginateLink)
	for name, store := range map[string]auth.Store{"nil store": nil, "empty store": &auth.MemoryStore{}} {
		t.Run(name, func(t *testing.T) {
			p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), store)
			_, err := p.CurrentUser(context.Background())
			if !errors.Is(err, errs.ErrNotAuthenticated) || errs.HintOf(err) != "trove auth login custom-company" {
				t.Fatalf("got %v (hint %q)", err, errs.HintOf(err))
			}
		})
	}
	if n := len(f.recorded()); n != 0 {
		t.Errorf("%d requests without a credential", n)
	}
}

func TestLinkToForeignHostRefused(t *testing.T) {
	var hits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeJSON(w, 200, []domain.Repository{})
	}))
	defer evil.Close()

	f := newFakeTFP(t, paginateLink)
	f.linkNext = evil.URL + "/api/v1/repositories?page=2"
	p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), auth.TokenStore(fakeToken))
	_, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{IncludeArchived: true})
	if !errors.Is(err, errs.ErrProviderAPI) || !strings.Contains(err.Error(), "refusing to follow pagination link") {
		t.Fatalf("got %v", err)
	}
	if hits.Load() != 0 {
		t.Error("foreign host was contacted")
	}

	// Relative links resolve against the request URL and are followed.
	f.linkNext = "/api/v1/repositories?page=2&per_page=100&include_archived=true"
	repos, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{IncludeArchived: true, ListOptions: forge.ListOptions{Limit: 150}})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 150 {
		t.Errorf("got %d repositories", len(repos))
	}
}

func TestRedirectDoesNotLeakCredential(t *testing.T) {
	var gotAuth atomic.Value
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("X-Forge-Key") + r.Header.Get("Authorization"))
		writeErr(w, 401, "unauthorized", "who are you")
	}))
	defer evil.Close()
	f := newFakeTFP(t, paginateLink)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/v1/user", http.StatusFound)
	}
	p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "auth:\n  header: X-Forge-Key")), auth.TokenStore(fakeToken))
	_, _ = p.CurrentUser(context.Background())
	if v, _ := gotAuth.Load().(string); v != "" {
		t.Errorf("credential forwarded to redirect target: %q", v)
	}
	if gotAuth.Load() == nil {
		t.Error("redirect target was not reached; test is not exercising redirects")
	}
}

func TestCapabilityRequestShapes(t *testing.T) {
	f := newFakeTFP(t, paginateLink)
	p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "terms:\n  pull_request: Change Request\n  pull_request_short: CR")), auth.TokenStore(fakeToken))
	ctx := context.Background()
	ref := domain.RepositoryRef{Namespace: "acme/platform/tools", Name: "deployer"}
	const repoPath = "/api/v1/repositories/acme%2Fplatform%2Ftools%2Fdeployer"

	expect := func(t *testing.T, method, escPath string, body map[string]any) recorded {
		t.Helper()
		r := f.last()
		if r.Method != method || r.EscPath != escPath {
			t.Errorf("request = %s %s, want %s %s", r.Method, r.EscPath, method, escPath)
		}
		if body != nil {
			var got map[string]any
			if err := json.Unmarshal(r.Body, &got); err != nil {
				t.Fatalf("body %q: %v", r.Body, err)
			}
			if !reflect.DeepEqual(got, body) {
				t.Errorf("body = %v, want %v", got, body)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
		}
		if r.Header.Get("Accept") != "application/json" || !strings.HasPrefix(r.Header.Get("User-Agent"), "trove/") {
			t.Errorf("headers = %v", r.Header)
		}
		return r
	}

	t.Run("repositories", func(t *testing.T) {
		r, err := p.CreateRepository(ctx, forge.CreateRepositoryRequest{Name: "svc", Namespace: "acme/platform", Description: "d",
			Visibility: domain.VisibilityInternal, DefaultBranch: "trunk", AutoInit: true})
		if err != nil {
			t.Fatal(err)
		}
		expect(t, "POST", "/api/v1/repositories", map[string]any{"name": "svc", "namespace": "acme/platform", "description": "d",
			"visibility": "internal", "default_branch": "trunk", "auto_init": true})
		if r.FullName != "acme/platform/svc" || r.URLs.HTTPS == "" || r.ProviderType != "custom" {
			t.Errorf("created = %+v", r)
		}
		if _, err := p.CreateRepository(ctx, forge.CreateRepositoryRequest{Name: "svc", Namespace: "acme/platform"}); !errors.Is(err, errs.ErrConflict) {
			t.Errorf("duplicate create: %v", err)
		}
		if err := p.DeleteRepository(ctx, domain.RepositoryRef{Namespace: "acme/platform", Name: "svc"}); err != nil {
			t.Fatal(err)
		}
		expect(t, "DELETE", "/api/v1/repositories/acme%2Fplatform%2Fsvc", nil)
		if err := p.DeleteRepository(ctx, domain.RepositoryRef{Namespace: "acme/platform", Name: "svc"}); !errors.Is(err, errs.ErrRepositoryNotFound) {
			t.Errorf("delete missing: %v", err)
		}
		if _, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{Visibility: domain.VisibilityPublic, OwnedOnly: true, Namespace: "solo"}); err != nil {
			t.Fatal(err)
		}
		q := f.last().Query
		if q.Get("visibility") != "public" || q.Get("owned") != "true" || q.Get("namespace") != "solo" || q.Has("include_archived") {
			t.Errorf("list query = %v", q)
		}
	})

	t.Run("pull requests", func(t *testing.T) {
		prs, err := p.ListPullRequests(ctx, ref, forge.PullRequestListOptions{State: domain.PullRequestOpen, Author: "bob", TargetBranch: "release"})
		if err != nil {
			t.Fatal(err)
		}
		q := f.last().Query
		if q.Get("state") != "open" || q.Get("author") != "bob" || q.Get("target_branch") != "release" {
			t.Errorf("query = %v", q)
		}
		// The fake ignores author/target_branch; the client filters.
		if len(prs) != 1 || prs[0].Number != 3 || prs[0].Term != "Change Request" {
			t.Errorf("prs = %+v", prs)
		}
		pr, err := p.CreatePullRequest(ctx, ref, forge.CreatePullRequestRequest{Title: "T", Body: "B", SourceBranch: "s", TargetBranch: "main", Draft: true, SourceRepo: "fork/deployer"})
		if err != nil {
			t.Fatal(err)
		}
		expect(t, "POST", repoPath+"/pull-requests", map[string]any{"title": "T", "body": "B", "source_branch": "s", "target_branch": "main", "draft": true, "source_repo": "fork/deployer"})
		if pr.Term != "Change Request" || pr.Number == 0 {
			t.Errorf("created = %+v", pr)
		}
		if err := p.MergePullRequest(ctx, ref, 1, forge.MergePullRequestRequest{Method: domain.MergeMethodSquash, CommitTitle: "ct", CommitMessage: "cm", DeleteSourceBranch: true}); err != nil {
			t.Fatal(err)
		}
		expect(t, "POST", repoPath+"/pull-requests/1/merge", map[string]any{"method": "squash", "commit_title": "ct", "commit_message": "cm", "delete_source_branch": true})
		if err := p.MergePullRequest(ctx, ref, 1, forge.MergePullRequestRequest{Method: "octopus"}); !errors.Is(err, errs.ErrInvalidArgument) {
			t.Errorf("bad merge method: %v", err)
		}
		if err := p.ClosePullRequest(ctx, ref, 1); err != nil {
			t.Fatal(err)
		}
		expect(t, "POST", repoPath+"/pull-requests/1/close", nil)
		if _, err := p.GetPullRequest(ctx, ref, 42); !errors.Is(err, errs.ErrNotFound) || errors.Is(err, errs.ErrRepositoryNotFound) {
			t.Errorf("missing PR: %v", err)
		}
		if got := p.MergeMethods(); len(got) != 3 {
			t.Errorf("merge methods = %v", got)
		}
	})

	t.Run("issues", func(t *testing.T) {
		issues, err := p.ListIssues(ctx, ref, forge.IssueListOptions{State: domain.IssueOpen, Labels: []string{"a", "b"}})
		if err != nil {
			t.Fatal(err)
		}
		q := f.last().Query
		if q.Get("labels") != "a,b" || q.Get("state") != "open" {
			t.Errorf("query = %v", q)
		}
		if len(issues) != 1 || issues[0].Number != 1 {
			t.Errorf("issues = %+v", issues)
		}
		if _, err := p.CreateIssue(ctx, ref, forge.CreateIssueRequest{Title: "New", Body: "b", Labels: []string{"bug"}, Assignees: []string{"alice"}}); err != nil {
			t.Fatal(err)
		}
		expect(t, "POST", repoPath+"/issues", map[string]any{"title": "New", "body": "b", "labels": []any{"bug"}, "assignees": []any{"alice"}})
		is, err := p.SetIssueState(ctx, ref, 1, domain.IssueClosed)
		if err != nil {
			t.Fatal(err)
		}
		expect(t, "PATCH", repoPath+"/issues/1", map[string]any{"state": "closed"})
		if is.State != domain.IssueClosed {
			t.Errorf("state = %q", is.State)
		}
		if _, err := p.SetIssueState(ctx, ref, 1, domain.IssueAll); !errors.Is(err, errs.ErrInvalidArgument) {
			t.Errorf("bad state: %v", err)
		}
	})

	t.Run("pipelines", func(t *testing.T) {
		pls, err := p.ListPipelines(ctx, ref, forge.PipelineListOptions{Ref: "main"})
		if err != nil {
			t.Fatal(err)
		}
		if f.last().Query.Get("ref") != "main" {
			t.Errorf("query = %v", f.last().Query)
		}
		if len(pls) != 2 || pls[1].Status != domain.PipelineUnknown || pls[1].RawStatus != "weird" {
			t.Errorf("pipelines = %+v", pls)
		}
		if _, err := p.ListPipelines(ctx, ref, forge.PipelineListOptions{Status: domain.PipelineFailed}); err != nil {
			t.Fatal(err)
		}
		if f.last().Query.Get("status") != "failed" {
			t.Errorf("query = %v", f.last().Query)
		}
		pl, err := p.GetPipeline(ctx, ref, "100")
		if err != nil {
			t.Fatal(err)
		}
		expect(t, "GET", repoPath+"/pipelines/100", nil)
		if len(pl.Jobs) != 2 || pl.Jobs[1].Status != domain.PipelineUnknown || pl.Jobs[1].RawStatus != "exploded" || pl.Duration != 90*time.Second {
			t.Errorf("pipeline = %+v", pl)
		}
	})

	t.Run("releases", func(t *testing.T) {
		rels, err := p.ListReleases(ctx, ref, forge.ListOptions{})
		if err != nil || len(rels) != 2 {
			t.Fatalf("releases = %v, %v", rels, err)
		}
		rel, err := p.GetRelease(ctx, ref, "v1.0/rc")
		if err != nil {
			t.Fatal(err)
		}
		expect(t, "GET", repoPath+"/releases/v1.0%2Frc", nil)
		if rel.Name != "RC" {
			t.Errorf("release = %+v", rel)
		}
	})

	t.Run("namespaces", func(t *testing.T) {
		nss, err := p.ListNamespaces(ctx, forge.ListOptions{})
		if err != nil || len(nss) != 2 {
			t.Fatalf("namespaces = %v, %v", nss, err)
		}
		if nss[0].Label != "Namespace" {
			t.Errorf("label = %q", nss[0].Label)
		}
		if _, err := p.GetNamespace(ctx, "missing"); !errors.Is(err, errs.ErrNotFound) {
			t.Errorf("missing namespace: %v", err)
		}
	})

	t.Run("summary", func(t *testing.T) {
		s, err := p.Summary(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expect(t, "GET", "/api/v1/summary", nil)
		if s.Repositories == nil || *s.Repositories != 261 || s.OpenPullRequests == nil || *s.OpenPullRequests != 3 {
			t.Errorf("summary = %+v", s)
		}
		if s.OpenIssues != nil || s.RunningPipelines != nil || s.Unread != nil {
			t.Error("absent summary fields must stay nil")
		}
	})
}

func TestLoginAuthStatusAndGitCredentials(t *testing.T) {
	f := newFakeTFP(t, paginateLink)
	ctx := context.Background()

	t.Run("login validates without persisting", func(t *testing.T) {
		store := &auth.MemoryStore{}
		p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), store)
		res, err := p.Login(ctx, auth.Request{Token: fakeToken})
		if err != nil {
			t.Fatal(err)
		}
		if res.User != "alice" || res.Credential.Token != fakeToken || res.Credential.Kind != auth.KindToken {
			t.Errorf("result user=%q kind=%q", res.User, res.Credential.Kind)
		}
		if _, err := store.Load(ctx); err == nil {
			t.Error("Login persisted the credential")
		}
		_, err = p.Login(ctx, auth.Request{Token: "bad-token"})
		if !errors.Is(err, errs.ErrAuthenticationFailed) || strings.Contains(err.Error(), "bad-token") {
			t.Errorf("bad token: %v", err)
		}
		if _, err := p.Login(ctx, auth.Request{}); !errors.Is(err, errs.ErrInvalidArgument) {
			t.Errorf("empty token: %v", err)
		}
		if _, err := p.Login(ctx, auth.Request{Method: auth.MethodOAuth, Token: fakeToken}); !errors.Is(err, errs.ErrUnsupportedCapability) {
			t.Errorf("oauth: %v", err)
		}
		if _, err := p.Login(ctx, auth.Request{Method: auth.MethodBasic, Token: fakeToken}); !errors.Is(err, errs.ErrInvalidArgument) {
			t.Errorf("basic without basic scheme: %v", err)
		}
	})

	t.Run("basic login", func(t *testing.T) {
		fb := newFakeTFP(t, paginateLink)
		fb.authValue = "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:"+fakeToken))
		p := newProvider(t, providerYAML(fb.apiURL(), customBlock(paginateLink, "auth:\n  scheme: basic")), nil)
		res, err := p.Login(ctx, auth.Request{Method: auth.MethodBasic, Username: "bob", Token: fakeToken})
		if err != nil {
			t.Fatal(err)
		}
		if res.Credential.Kind != auth.KindBasic || res.Credential.Username != "bob" {
			t.Errorf("credential kind=%q user=%q", res.Credential.Kind, res.Credential.Username)
		}
		// The stored basic credential carries its username.
		p2 := newProvider(t, providerYAML(fb.apiURL(), customBlock(paginateLink, "auth:\n  scheme: basic")), auth.NewMemoryStore(res.Credential))
		st, err := p2.AuthStatus(ctx)
		if err != nil || !st.Authenticated || st.Method != auth.MethodBasic {
			t.Errorf("status = %+v, %v", st, err)
		}
		user, pass, err := p2.GitCredentials(ctx)
		if err != nil || user != "bob" || pass != fakeToken {
			t.Errorf("git credentials = %q, %v", user, err)
		}
	})

	t.Run("auth status", func(t *testing.T) {
		good := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), auth.TokenStore(fakeToken))
		st, err := good.AuthStatus(ctx)
		if err != nil || !st.Authenticated || st.User != "alice" || st.Method != auth.MethodToken {
			t.Errorf("good status = %+v, %v", st, err)
		}
		bad := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), auth.TokenStore("nope"))
		st, err = bad.AuthStatus(ctx)
		if err != nil || st.Authenticated {
			t.Errorf("bad status = %+v, %v", st, err)
		}
		none := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), nil)
		st, err = none.AuthStatus(ctx)
		if err != nil || st.Authenticated {
			t.Errorf("missing status = %+v, %v", st, err)
		}
	})

	t.Run("git credentials", func(t *testing.T) {
		p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), auth.TokenStore(fakeToken))
		user, pass, err := p.GitCredentials(ctx)
		if err != nil || user != "trove" || pass != fakeToken {
			t.Errorf("default git credentials = %q, %v", user, err)
		}
		p = newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "git_username: deploy-bot")), auth.TokenStore(fakeToken))
		if user, _, err := p.GitCredentials(ctx); err != nil || user != "deploy-bot" {
			t.Errorf("configured git username = %q, %v", user, err)
		}
		none := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateLink, "")), nil)
		if _, _, err := none.GitCredentials(ctx); !errors.Is(err, errs.ErrNotAuthenticated) {
			t.Errorf("git credentials without login: %v", err)
		}
	})
}

func TestContextCancellationDuringList(t *testing.T) {
	f := newFakeTFP(t, paginatePage)
	p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginatePage, "")), auth.TokenStore(fakeToken))
	ctx, cancel := context.WithCancel(context.Background())
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}
	_, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestInvalidJSONIsProtocolError(t *testing.T) {
	f := newFakeTFP(t, paginateCursor)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		// A bare array where the cursor envelope is required.
		writeJSON(w, 200, []domain.Repository{{Name: "x", Namespace: "y"}})
	}
	p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateCursor, "")), auth.TokenStore(fakeToken))
	_, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{})
	if !errors.Is(err, errs.ErrProviderAPI) || !strings.Contains(err.Error(), "not valid TFP v1 JSON") {
		t.Fatalf("got %v", err)
	}
}

func TestPageStrategyHonorsXNextPage(t *testing.T) {
	f := newFakeTFP(t, paginatePage)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("pg")
		items := make([]domain.Repository, 2)
		for i := range items {
			items[i] = domain.Repository{Namespace: "ns", Name: "p" + page + "-" + string(rune('a'+i)), Visibility: domain.VisibilityPublic}
		}
		// Full pages, but the header says page 1 -> 3 -> end.
		switch page {
		case "1":
			w.Header().Set("X-Next-Page", "3")
		default:
			w.Header()["X-Next-Page"] = []string{""}
		}
		writeJSON(w, 200, items)
	}
	p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginatePage, "  page_param: pg\n  per_page_param: size\n  max_page_size: 2")), auth.TokenStore(fakeToken))
	repos, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rs := f.recorded()
	if len(rs) != 2 || rs[0].Query.Get("pg") != "1" || rs[1].Query.Get("pg") != "3" || rs[0].Query.Get("size") != "2" {
		t.Fatalf("requests = %+v", rs)
	}
	if len(repos) != 4 {
		t.Errorf("got %d repositories", len(repos))
	}
}

func TestCursorStrategyCustomParam(t *testing.T) {
	f := newFakeTFP(t, paginateCursor)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		next := ""
		if r.URL.Query().Get("after") == "" {
			next = "opaque+/=token"
		} else if r.URL.Query().Get("after") != "opaque+/=token" {
			writeErr(w, 400, "invalid", "cursor mangled")
			return
		}
		writeJSON(w, 200, map[string]any{"items": []domain.Repository{{Namespace: "ns", Name: "r" + r.URL.Query().Get("after")}}, "next_cursor": next})
	}
	p := newProvider(t, providerYAML(f.apiURL(), customBlock(paginateCursor, "  cursor_param: after")), auth.TokenStore(fakeToken))
	repos, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Errorf("got %d repositories", len(repos))
	}
}
