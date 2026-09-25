package github

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/forge/forgetest"
)

var existing = domain.RepositoryRef{Namespace: "octocat", Name: "repo-001"}

func newTestProvider(t *testing.T, f *fakeGitHub, mut func(*forge.Account)) *Provider {
	t.Helper()
	acct := forge.Account{
		Name: "gh-test", Type: DriverType, Host: "github.com",
		APIURL: f.apiRoot(), WebURL: f.webRoot(), Credentials: auth.TokenStore(testToken),
	}
	if f.prefix != "" {
		acct.Name, acct.Host = "ghe-test", "ghe.example.com"
	}
	if mut != nil {
		mut(&acct)
	}
	p, err := newProvider(acct)
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	return p
}

func assertKind(t *testing.T, err error, kind error) *errs.Error {
	t.Helper()
	if !errors.Is(err, kind) {
		t.Fatalf("want %v, got %v", kind, err)
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not *errs.Error", err)
	}
	for _, secret := range []string{testToken, testBadToken, testFineGrained, testInstToken, testRefreshToken} {
		if strings.Contains(err.Error(), secret) || strings.Contains(e.Hint, secret) {
			t.Fatalf("error leaks a token: %q", err.Error())
		}
	}
	return e
}

// --- contract ------------------------------------------------------------------------

func TestContract(t *testing.T) {
	for _, tc := range []struct{ name, prefix string }{{"github.com", ""}, {"GHES", "/api/v3"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, tc.prefix)
			p := newTestProvider(t, f, func(a *forge.Account) {
				if tc.prefix != "" {
					a.WebURL = "" // GHES derives web URLs from the host
				}
			})
			bad := newTestProvider(t, f, func(a *forge.Account) { a.Credentials = auth.TokenStore(testBadToken) })
			forgetest.RunForgeProviderContractTests(t, p, forgetest.Options{
				ExistingRepo:    existing,
				MissingRepo:     domain.RepositoryRef{Namespace: "octocat", Name: "does-not-exist"},
				MinRepositories: 250,
				Secret:          testToken,
				Unauthenticated: bad,
			})
			if tc.prefix != "" {
				if got := f.requests(http.MethodGet, "/user"); len(got) == 0 {
					t.Fatal("GHES requests did not reach the /api/v3 prefix")
				}
			}
		})
	}
}

func TestDriver(t *testing.T) {
	d := NewDriver()
	if d.Type() != "github" || d.DefaultHost() != "github.com" || len(d.AuthMethods()) != 3 {
		t.Fatalf("driver = %s %s %v", d.Type(), d.DefaultHost(), d.AuthMethods())
	}
	p, err := d.New(forge.Account{Name: "x", Host: "github.com"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Metadata().Name != "x" {
		t.Fatal("metadata name not set")
	}
	if _, err := d.New(forge.Account{Name: "x", APIURL: "::bad"}); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("bad api_url: %v", err)
	}
}

func TestMetadataAndURLs(t *testing.T) {
	for _, tc := range []struct {
		acct                                  forge.Account
		api, web, deploy, display, https, ssh string
	}{
		{forge.Account{Name: "a", Host: "github.com"}, "https://api.github.com", "https://github.com", "cloud", "GitHub",
			"https://github.com/o/r.git", "git@github.com:o/r.git"},
		{forge.Account{Name: "a"}, "https://api.github.com", "https://github.com", "cloud", "GitHub",
			"https://github.com/o/r.git", "git@github.com:o/r.git"},
		{forge.Account{Name: "a", Host: "ghe.corp.example"}, "https://ghe.corp.example/api/v3", "https://ghe.corp.example",
			"enterprise-server", "GitHub Enterprise Server", "https://ghe.corp.example/o/r.git", "git@ghe.corp.example:o/r.git"},
		{forge.Account{Name: "a", Host: "acme.ghe.com"}, "https://api.acme.ghe.com", "https://acme.ghe.com", "cloud",
			"GitHub Enterprise Cloud", "https://acme.ghe.com/o/r.git", "git@acme.ghe.com:o/r.git"},
		{forge.Account{Name: "a", Host: "github.com", CloneBaseURL: "https://proxy.example/gh/", SSHHost: "ssh.github.com:443"},
			"https://api.github.com", "https://github.com", "cloud", "GitHub",
			"https://proxy.example/gh/o/r.git", "ssh://git@ssh.github.com:443/o/r.git"},
	} {
		p, err := newProvider(tc.acct)
		if err != nil {
			t.Fatal(err)
		}
		m := p.Metadata()
		if m.APIURL != tc.api || m.WebURL != tc.web || m.Deployment != tc.deploy || m.DisplayName != tc.display {
			t.Errorf("%s: metadata = %+v", tc.acct.Host, m)
		}
		if m.Terms.Pipeline != "Workflow run" || m.Terms.Snippet != "Gist" || m.Terms.PullRequestShort != "PR" || m.Terms.Namespace != "Organization" {
			t.Errorf("terms = %+v", m.Terms)
		}
		u := p.RepositoryURLs(domain.RepositoryRef{Namespace: "o", Name: "r"})
		if u.HTTPS != tc.https || u.SSH != tc.ssh || u.Web != tc.web+"/o/r" || u.API != tc.api+"/repos/o/r" {
			t.Errorf("%s: urls = %+v", tc.acct.Host, u)
		}
	}
}

func TestAppAccountCapabilities(t *testing.T) {
	p, _ := newProvider(forge.Account{Name: "app", AuthMethod: auth.MethodApp})
	caps := p.Capabilities()
	for _, c := range []forge.Capability{forge.CapNotifications, forge.CapSnippets, forge.CapSSHKeys, forge.CapSettings} {
		if caps.Has(c) {
			t.Errorf("app account declares %s", c)
		}
	}
	if !caps.Has(forge.CapRepositories) || len(forge.ValidateCapabilities(p)) != 0 {
		t.Error("app account capabilities invalid")
	}
}

// --- HTTP mechanics ------------------------------------------------------------------

func TestRequestHeaders(t *testing.T) {
	f := newFake(t, "")
	p := newTestProvider(t, f, nil)
	if _, err := p.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := f.last(http.MethodGet, "/user").Header
	if h.Get("Accept") != "application/vnd.github+json" || h.Get("X-GitHub-Api-Version") != "2022-11-28" ||
		h.Get("Authorization") != "Bearer "+testToken || !strings.HasPrefix(h.Get("User-Agent"), "trove/") {
		t.Fatalf("headers = %v", h)
	}
}

func TestPagination(t *testing.T) {
	f := newFake(t, "")
	p := newTestProvider(t, f, nil)
	ctx := context.Background()

	all, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != numUserRepos {
		t.Fatalf("got %d repos", len(all))
	}
	reqs := f.requests(http.MethodGet, "/user/repos")
	if len(reqs) != 3 {
		t.Fatalf("want 3 page requests, got %d", len(reqs))
	}
	q := reqs[0].Query
	if q.Get("per_page") != "100" || q.Get("sort") != "updated" || q.Get("affiliation") != "owner,collaborator,organization_member" || q.Get("visibility") != "all" {
		t.Fatalf("first page query = %v", q)
	}
	if reqs[2].Query.Get("page") != "3" {
		t.Fatalf("third request did not follow Link next: %v", reqs[2].Query)
	}

	// Archived repositories are skipped by default.
	active, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != numUserRepos-numUserRepos/10 {
		t.Fatalf("archived filter: got %d", len(active))
	}

	// Limit stops early and shrinks per_page.
	before := len(f.requests(http.MethodGet, "/user/repos"))
	five, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{ListOptions: forge.ListOptions{Limit: 5}, IncludeArchived: true})
	if err != nil || len(five) != 5 {
		t.Fatalf("limit 5: %d %v", len(five), err)
	}
	reqs = f.requests(http.MethodGet, "/user/repos")
	if len(reqs)-before != 1 || reqs[len(reqs)-1].Query.Get("per_page") != "5" {
		t.Fatalf("limit made %d requests, per_page %s", len(reqs)-before, reqs[len(reqs)-1].Query.Get("per_page"))
	}

	// Visibility filter + fallback to "private" when the field is missing.
	priv, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{Visibility: domain.VisibilityPrivate, OwnedOnly: true, IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) != numUserRepos/3 {
		t.Fatalf("private repos = %d", len(priv))
	}
	last := f.last(http.MethodGet, "/user/repos").Query
	if last.Get("affiliation") != "owner" || last.Get("visibility") != "private" {
		t.Fatalf("owned private query = %v", last)
	}
	for _, r := range priv {
		if r.Visibility != domain.VisibilityPrivate {
			t.Fatalf("%s visibility %s", r.FullName, r.Visibility)
		}
	}
}

func TestPaginationRefusesForeignHost(t *testing.T) {
	f := newFake(t, "")
	p := newTestProvider(t, f, nil)
	f.override = func(w http.ResponseWriter, r *http.Request, path string) bool {
		if path != "/user/repos" {
			return false
		}
		w.Header().Set("Link", `<`+f.blob.URL+`/steal?page=2>; rel="next"`)
		writeJSON(w, http.StatusOK, []any{f.repo("octocat", "repo-001", 1, "public", false)})
		return true
	}
	_, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{})
	assertKind(t, err, errs.ErrProviderAPI)
	if len(f.blobAuthHeaders()) != 0 {
		t.Fatal("request was sent to the foreign host")
	}
}

func TestErrorMapping(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Unix()
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		body    map[string]any
		kind    error
		check   func(t *testing.T, e *errs.Error)
	}{
		{"401", 401, nil, map[string]any{"message": "Bad credentials"}, errs.ErrAuthenticationFailed, func(t *testing.T, e *errs.Error) {
			if e.Hint != "trove auth login gh-test" || !strings.Contains(e.Message, "authentication failed") {
				t.Errorf("401: %+v", e)
			}
		}},
		{"403", 403, map[string]string{"X-Accepted-OAuth-Scopes": "repo"}, map[string]any{"message": "Must have admin rights"}, errs.ErrPermissionDenied, func(t *testing.T, e *errs.Error) {
			if e.Message != "Must have admin rights" || !strings.Contains(e.Hint, "repo") {
				t.Errorf("403: %+v", e)
			}
		}},
		{"403 SSO", 403, map[string]string{"X-GitHub-SSO": "required; url=https://github.com/orgs/x/sso?authorization_request=1"}, map[string]any{"message": "Resource protected by organization SAML enforcement."}, errs.ErrPermissionDenied, func(t *testing.T, e *errs.Error) {
			if !strings.Contains(e.Hint, "single sign-on") {
				t.Errorf("SSO hint: %+v", e)
			}
		}},
		{"403 primary rate limit", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa64(reset)}, map[string]any{"message": "API rate limit exceeded for user ID 1."}, errs.ErrRateLimited, func(t *testing.T, e *errs.Error) {
			if e.RetryAfter < 29*time.Minute || e.RetryAfter > 31*time.Minute {
				t.Errorf("RetryAfter = %v", e.RetryAfter)
			}
		}},
		{"403 secondary rate limit", 403, map[string]string{"Retry-After": "120"}, map[string]any{"message": "You have exceeded a secondary rate limit."}, errs.ErrRateLimited, func(t *testing.T, e *errs.Error) {
			if e.RetryAfter != 120*time.Second {
				t.Errorf("RetryAfter = %v", e.RetryAfter)
			}
		}},
		{"403 secondary without header", 403, nil, map[string]any{"message": "You have exceeded a secondary rate limit."}, errs.ErrRateLimited, func(t *testing.T, e *errs.Error) {
			if e.RetryAfter != time.Minute {
				t.Errorf("RetryAfter = %v", e.RetryAfter)
			}
		}},
		{"429", 429, map[string]string{"Retry-After": "90"}, map[string]any{"message": "slow down"}, errs.ErrRateLimited, func(t *testing.T, e *errs.Error) {
			if e.RetryAfter != 90*time.Second || e.Status != 429 {
				t.Errorf("429: %+v", e)
			}
		}},
		{"404", 404, nil, map[string]any{"message": "Not Found"}, errs.ErrRepositoryNotFound, func(t *testing.T, e *errs.Error) {
			if !errors.Is(e, errs.ErrNotFound) || !strings.Contains(e.Message, "octocat/repo-001") {
				t.Errorf("404: %+v", e)
			}
		}},
		{"409", 409, nil, map[string]any{"message": "Repository is empty."}, errs.ErrConflict, nil},
		{"422", 422, nil, map[string]any{"message": "Validation Failed", "errors": []any{
			map[string]any{"resource": "Repository", "field": "name", "code": "already_exists"},
			map[string]any{"resource": "Repository", "code": "custom", "message": "name is too long"},
			"plain string error",
		}}, errs.ErrInvalidArgument, func(t *testing.T, e *errs.Error) {
			if e.Message != "Validation Failed: name already exists; name is too long; plain string error" {
				t.Errorf("422 message = %q", e.Message)
			}
		}},
		{"500", 500, nil, map[string]any{"message": "boom " + testToken}, errs.ErrProviderAPI, func(t *testing.T, e *errs.Error) {
			if !strings.Contains(e.Message, "HTTP 500") {
				t.Errorf("500: %+v", e)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, "")
			p := newTestProvider(t, f, nil)
			f.override = func(w http.ResponseWriter, r *http.Request, path string) bool {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				writeJSON(w, tc.status, tc.body)
				return true
			}
			_, err := p.GetRepository(context.Background(), existing)
			e := assertKind(t, err, tc.kind)
			if e.Provider != "gh-test" || e.Op == "" {
				t.Errorf("provider/op not set: %+v", e)
			}
			if tc.check != nil {
				tc.check(t, e)
			}
		})
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func TestRateLimitDetector(t *testing.T) {
	mk := func(status int, h map[string]string) *http.Response {
		r := &http.Response{StatusCode: status, Header: http.Header{}}
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	if ok, _ := rateLimited(mk(403, nil)); ok {
		t.Error("plain 403 detected as rate limit")
	}
	if ok, d := rateLimited(mk(403, map[string]string{"Retry-After": "5"})); !ok || d != 5*time.Second {
		t.Errorf("secondary: %v %v", ok, d)
	}
	if ok, d := rateLimited(mk(403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa64(time.Now().Add(time.Minute).Unix())})); !ok || d <= 0 {
		t.Errorf("primary: %v %v", ok, d)
	}
	if ok, _ := rateLimited(mk(429, nil)); !ok {
		t.Error("429 not detected")
	}
	if ok, _ := rateLimited(mk(200, map[string]string{"X-RateLimit-Remaining": "0"})); ok {
		t.Error("200 detected as rate limit")
	}
}

func TestContextCancellation(t *testing.T) {
	f := newFake(t, "")
	p := newTestProvider(t, f, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.ListRepositories(ctx, forge.ListRepositoryOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled: %v", err)
	}
	if err := p.PipelineLogs(ctx, existing, "1001", "", &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("logs pre-canceled: %v", err)
	}

	// In-flight cancellation.
	started := make(chan struct{})
	f.override = func(w http.ResponseWriter, r *http.Request, path string) bool {
		if path != "/user/repos" {
			return false
		}
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		return true
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { <-started; cancel2() }()
	_, err := p.ListRepositories(ctx2, forge.ListRepositoryOptions{})
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errs.ErrCanceled) {
		t.Fatalf("in-flight: %v", err)
	}
}

func TestNotLoggedIn(t *testing.T) {
	f := newFake(t, "")
	p := newTestProvider(t, f, func(a *forge.Account) { a.Credentials = nil })
	_, err := p.CurrentUser(context.Background())
	e := assertKind(t, err, errs.ErrNotAuthenticated)
	if e.Hint != "trove auth login gh-test" {
		t.Fatalf("hint = %q", e.Hint)
	}
	p = newTestProvider(t, f, func(a *forge.Account) { a.Credentials = &auth.MemoryStore{} })
	_, err = p.ListRepositories(context.Background(), forge.ListRepositoryOptions{})
	assertKind(t, err, errs.ErrNotAuthenticated)
}
