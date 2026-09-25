package gitlab

import (
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

func TestContractGitLabCom(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	bad := f.newProvider(t, withStore(auth.TokenStore("glpat-wrongTOKENwrongTOKEN99")))
	m := p.Metadata()
	if m.Deployment != "cloud" || m.DisplayName != "GitLab" || m.Type != "gitlab" {
		t.Fatalf("unexpected metadata %+v", m)
	}
	forgetest.RunForgeProviderContractTests(t, p, forgetest.Options{
		ExistingRepo:    domain.RepositoryRef{Namespace: "acme/platform/infra", Name: "proj-003"},
		MissingRepo:     domain.RepositoryRef{Namespace: "acme/platform", Name: "does-not-exist"},
		MinRepositories: fakeProjectCount,
		Secret:          testPAT,
		Unauthenticated: bad,
	})
}

func TestContractSelfManaged(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t, withHost("gitlab.example.com"), withStore(oauthStore(testOAuth, "", time.Time{})))
	bad := f.newProvider(t, withHost("gitlab.example.com"), withStore(oauthStore("oauth-bad-token-000000000", "", time.Time{})))
	m := p.Metadata()
	if m.Deployment != "self-managed" || m.DisplayName != "GitLab Self-Managed" || m.Host != "gitlab.example.com" {
		t.Fatalf("unexpected metadata %+v", m)
	}
	if m.Terms.PullRequest != "Merge Request" || m.Terms.Notification != "To-Do item" || m.Terms.Namespace != "Group" || m.Terms.Repository != "Project" {
		t.Fatalf("unexpected terms %+v", m.Terms)
	}
	forgetest.RunForgeProviderContractTests(t, p, forgetest.Options{
		ExistingRepo:    domain.RepositoryRef{Namespace: "alice", Name: "proj-004"},
		MissingRepo:     domain.RepositoryRef{Namespace: "alice", Name: "nope"},
		MinRepositories: fakeProjectCount,
		Secret:          testOAuth,
		Unauthenticated: bad,
	})
}

func TestDefaultsWithoutOverrides(t *testing.T) {
	p, err := NewDriver().New(forge.Account{Name: "x", Host: "gitlab.example.com:8443"})
	if err != nil {
		t.Fatal(err)
	}
	m := p.Metadata()
	if m.APIURL != "https://gitlab.example.com:8443/api/v4" || m.WebURL != "https://gitlab.example.com:8443" {
		t.Fatalf("metadata URLs = %q %q", m.APIURL, m.WebURL)
	}
	p2, _ := NewDriver().New(forge.Account{Name: "y"})
	if p2.Metadata().Host != "gitlab.com" || p2.Metadata().Deployment != "cloud" {
		t.Fatalf("default host metadata = %+v", p2.Metadata())
	}
	if _, err := NewDriver().New(forge.Account{Name: "z", APIURL: "::bad"}); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("bad api_url: want ErrInvalidConfiguration, got %v", err)
	}
}

func TestRepositoryURLOverrides(t *testing.T) {
	p, err := NewDriver().New(forge.Account{Name: "x", Host: "git.corp.example",
		CloneBaseURL: "https://clone.corp.example/", SSHHost: "ssh.corp.example:2222"})
	if err != nil {
		t.Fatal(err)
	}
	u := p.(forge.RepositoryProvider).RepositoryURLs(domain.RepositoryRef{Namespace: "g/sub", Name: "proj"})
	want := domain.RepositoryURLs{
		Web:   "https://git.corp.example/g/sub/proj",
		HTTPS: "https://clone.corp.example/g/sub/proj.git",
		SSH:   "ssh://git@ssh.corp.example:2222/g/sub/proj.git",
		API:   "https://git.corp.example/api/v4/projects/g%2Fsub%2Fproj",
	}
	if u != want {
		t.Fatalf("URLs = %+v\nwant %+v", u, want)
	}
	p2, _ := NewDriver().New(forge.Account{Name: "y"})
	u2 := p2.(forge.RepositoryProvider).RepositoryURLs(domain.RepositoryRef{Namespace: "g", Name: "p"})
	if u2.SSH != "git@gitlab.com:g/p.git" || u2.HTTPS != "https://gitlab.com/g/p.git" {
		t.Fatalf("default URLs = %+v", u2)
	}
}

func TestPaginationLinkHeader(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	repos, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != fakeProjectCount {
		t.Fatalf("got %d projects", len(repos))
	}
	reqs := f.recorded("GET", "/api/v4/projects")
	if len(reqs) != 3 {
		t.Fatalf("want 3 page requests, got %d: %v", len(reqs), f.paths())
	}
	q := reqs[0].query()
	if q.Get("per_page") != "100" || q.Get("membership") != "true" || q.Get("order_by") != "last_activity_at" || q.Has("archived") {
		t.Fatalf("first page query = %v", q)
	}
	if reqs[2].query().Get("page") != "3" {
		t.Fatalf("third request query = %v", reqs[2].query())
	}
	r := repos[2] // id 3
	if r.FullName != "acme/platform/infra/proj-003" || r.Namespace != "acme/platform/infra" || r.Name != "proj-003" ||
		r.ID != "3" || r.Visibility != domain.VisibilityPrivate || r.Stars != 3 || r.DefaultBranch != "main" ||
		r.UpdatedAt.IsZero() || r.URLs.SSH == "" || r.URLs.API != f.srv.URL+"/api/v4/projects/3" {
		t.Fatalf("mapped repository = %+v", r)
	}

	// Archived projects are excluded unless requested; limit respected.
	repos, err = p.ListRepositories(context.Background(), forge.ListRepositoryOptions{ListOptions: forge.ListOptions{Limit: 150}})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 150 {
		t.Fatalf("limit 150 returned %d", len(repos))
	}
	for _, r := range repos {
		if r.Archived {
			t.Fatalf("archived project %s returned", r.FullName)
		}
	}
	if q := f.last(t, "GET", "/api/v4/projects").query(); q.Get("archived") != "false" {
		t.Fatalf("archived filter missing: %v", q)
	}
}

func TestPaginationXNextPageOnly(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	repos, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{
		Namespace: "acme", IncludeArchived: true, OwnedOnly: true, Visibility: domain.VisibilityInternal})
	if err != nil {
		t.Fatal(err)
	}
	// acme, acme/platform and acme/platform/infra: 3/4 of the projects.
	if len(repos) != 195 {
		t.Fatalf("got %d projects", len(repos))
	}
	reqs := f.recorded("GET", "/api/v4/groups/acme/projects")
	if len(reqs) != 2 {
		t.Fatalf("want 2 page requests, got %v", f.paths())
	}
	q := reqs[0].query()
	if q.Get("include_subgroups") != "true" || q.Get("owned") != "true" || q.Get("visibility") != "internal" || q.Has("membership") {
		t.Fatalf("group projects query = %v", q)
	}
	if reqs[1].query().Get("page") != "2" {
		t.Fatalf("second page query = %v", reqs[1].query())
	}
}

func TestListRepositoriesSubgroupAndUserFallback(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	repos, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{Namespace: "acme/platform", IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 130 {
		t.Fatalf("subgroup listing returned %d", len(repos))
	}
	if len(f.recorded("GET", "/api/v4/groups/acme%2Fplatform/projects")) == 0 {
		t.Fatalf("subgroup path not encoded: %v", f.paths())
	}
	f.json("GET /api/v4/users/bob/projects", 200, []any{f.project(900, "bob", "dots")})
	repos, err = p.ListRepositories(context.Background(), forge.ListRepositoryOptions{Namespace: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].FullName != "bob/dots" {
		t.Fatalf("user fallback = %+v", repos)
	}
}

func TestNextLinkToForeignHostIsIgnored(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	calls := 0
	f.handle("GET /api/v4/user/keys", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Link", `<https://evil.example/api/v4/user/keys?page=2>; rel="next"`)
			w.Header().Set("X-Next-Page", "2")
			writeJSON(w, 200, []any{map[string]any{"id": 1, "title": "a", "key": "ssh-ed25519 AAAA"}})
			return
		}
		if r.URL.Query().Get("page") != "2" {
			t.Errorf("fallback page = %q", r.URL.Query().Get("page"))
		}
		writeJSON(w, 200, []any{map[string]any{"id": 2, "title": "b", "key": "ssh-ed25519 BBBB"}})
	})
	keys, err := p.ListSSHKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys", len(keys))
	}
}

func TestEncodedProjectPathReachesServer(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	r, err := p.GetRepository(context.Background(), domain.RepositoryRef{Namespace: "acme/platform/infra", Name: "proj-003"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "3" {
		t.Fatalf("got %+v", r)
	}
	req := f.last(t, "GET", "/api/v4/projects/acme%2Fplatform%2Finfra%2Fproj-003")
	if !strings.HasPrefix(req.RequestURI, "/api/v4/projects/acme%2Fplatform%2Finfra%2Fproj-003") {
		t.Fatalf("RequestURI = %q", req.RequestURI)
	}
	// Refs with an ID use it instead of the path.
	if _, err := p.GetRepository(context.Background(), domain.RepositoryRef{Namespace: "x", Name: "y", ID: "7"}); err != nil {
		t.Fatal(err)
	}
	f.last(t, "GET", "/api/v4/projects/7")
}

func TestAuthHeaderSelection(t *testing.T) {
	f := newFake(t)
	pat := f.newProvider(t)
	if _, err := pat.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := f.last(t, "GET", "/api/v4/user").Header
	if h.Get("PRIVATE-TOKEN") != testPAT || h.Get("Authorization") != "" {
		t.Fatalf("PAT headers: PRIVATE-TOKEN=%q Authorization=%q", h.Get("PRIVATE-TOKEN"), h.Get("Authorization"))
	}
	oa := f.newProvider(t, withStore(oauthStore(testOAuth, "", time.Time{})))
	if _, err := oa.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	h = f.last(t, "GET", "/api/v4/user").Header
	if h.Get("Authorization") != "Bearer "+testOAuth || h.Get("PRIVATE-TOKEN") != "" {
		t.Fatalf("OAuth headers: PRIVATE-TOKEN=%q Authorization=%q", h.Get("PRIVATE-TOKEN"), h.Get("Authorization"))
	}
	if !strings.HasPrefix(h.Get("User-Agent"), "trove/") {
		t.Fatalf("User-Agent = %q", h.Get("User-Agent"))
	}
}

func TestNotLoggedIn(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t, withStore(nil))
	_, err := p.CurrentUser(context.Background())
	if !errors.Is(err, errs.ErrNotAuthenticated) || errs.HintOf(err) != "trove auth login gl" {
		t.Fatalf("nil store: got %v (hint %q)", err, errs.HintOf(err))
	}
	empty := &auth.MemoryStore{}
	p = f.newProvider(t, withStore(empty))
	if _, err := p.CurrentUser(context.Background()); !errors.Is(err, errs.ErrNotAuthenticated) {
		t.Fatalf("empty store: got %v", err)
	}
	if len(f.paths()) != 0 {
		t.Fatalf("requests were made without credentials: %v", f.paths())
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    any
		header  map[string]string
		kind    error
		msg     string
		retry   time.Duration
		hintHas string
	}{
		{"string message", 400, map[string]any{"message": "400 Bad request - title is missing"}, nil, errs.ErrInvalidArgument, "title is missing", 0, ""},
		{"array message", 400, map[string]any{"message": []string{"name is too long", "path is invalid"}}, nil, errs.ErrInvalidArgument, "name is too long; path is invalid", 0, ""},
		{"object message", 400, map[string]any{"message": map[string]any{"path": []string{"is invalid"}, "name": []string{"has already been taken", "is reserved"}}}, nil, errs.ErrInvalidArgument, "name has already been taken, is reserved; path is invalid", 0, ""},
		{"error field", 422, map[string]any{"error": "title is invalid"}, nil, errs.ErrInvalidArgument, "title is invalid", 0, ""},
		{"unauthorized", 401, map[string]any{"message": "401 Unauthorized"}, nil, errs.ErrAuthenticationFailed, "token is invalid or expired", 0, "trove auth login gl"},
		{"insufficient scope", 403, map[string]any{"error": "insufficient_scope", "error_description": "The request requires higher privileges than provided by the access token.", "scope": "api"}, nil, errs.ErrPermissionDenied, "higher privileges", 0, "required scope (api)"},
		{"forbidden is not rate limit", 403, map[string]any{"message": "403 Forbidden"}, map[string]string{"Retry-After": "5"}, errs.ErrPermissionDenied, "403 Forbidden", 0, ""},
		{"not found", 404, map[string]any{"message": "404 Not found"}, nil, errs.ErrNotFound, "404 Not found", 0, ""},
		{"conflict", 409, map[string]any{"message": "Branch already exists"}, nil, errs.ErrConflict, "Branch already exists", 0, ""},
		{"rate limited", 429, map[string]any{"message": "Retry later"}, map[string]string{"Retry-After": "120", "RateLimit-Reset": "1"}, errs.ErrRateLimited, "rate limit", 120 * time.Second, ""},
		{"server error", 500, map[string]any{"message": "500 Internal Server Error"}, nil, errs.ErrProviderAPI, "HTTP 500", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			p := f.newProvider(t)
			f.handle("GET /api/v4/namespaces/grp", func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				writeJSON(w, tc.status, tc.body)
			})
			_, err := p.GetNamespace(context.Background(), "grp")
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("message %q does not contain %q", err.Error(), tc.msg)
			}
			if strings.Contains(err.Error(), testPAT) {
				t.Fatalf("token leaked: %q", err)
			}
			var e *errs.Error
			if !errors.As(err, &e) || e.Status != tc.status || e.Provider != "gl" {
				t.Fatalf("error details = %#v", e)
			}
			if e.RetryAfter != tc.retry {
				t.Fatalf("RetryAfter = %v, want %v", e.RetryAfter, tc.retry)
			}
			if !strings.Contains(e.Hint, tc.hintHas) {
				t.Fatalf("hint %q does not contain %q", e.Hint, tc.hintHas)
			}
		})
	}
}

func TestRetryAfterFallsBackToRateLimitReset(t *testing.T) {
	h := http.Header{}
	h.Set("RateLimit-Reset", strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10))
	if d := retryAfter(h); d < 80*time.Second || d > 91*time.Second {
		t.Fatalf("retryAfter = %v", d)
	}
	h.Set("Retry-After", "7")
	if d := retryAfter(h); d != 7*time.Second {
		t.Fatalf("retryAfter with Retry-After = %v", d)
	}
}

func TestRepositoryNotFoundIsBothKinds(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	_, err := p.GetRepository(context.Background(), domain.RepositoryRef{Namespace: "acme", Name: "missing"})
	if !errors.Is(err, errs.ErrRepositoryNotFound) || !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestErrorMessageNeverEchoesTokens(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	f.json("GET /api/v4/namespaces/grp", 400, map[string]any{"message": "bad token " + testPAT})
	_, err := p.GetNamespace(context.Background(), "grp")
	if err == nil || strings.Contains(err.Error(), testPAT) {
		t.Fatalf("token leaked or no error: %v", err)
	}
}

func TestContextCanceledDuringList(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.handle("GET /api/v4/namespaces", func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})
	_, err := p.ListNamespaces(ctx, forge.ListOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestDriverDescription(t *testing.T) {
	d := NewDriver()
	if d.Type() != "gitlab" || d.DisplayName() != "GitLab" || d.DefaultHost() != "gitlab.com" || d.Description() == "" ||
		len(d.AuthMethods()) != 2 {
		t.Fatalf("driver = %s %s %s %v", d.Type(), d.DisplayName(), d.DefaultHost(), d.AuthMethods())
	}
}

func TestEmptyRepositoryRef(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	_, err := p.GetRepository(context.Background(), domain.RepositoryRef{})
	var e *errs.Error
	if !errors.Is(err, errs.ErrInvalidArgument) || !errors.As(err, &e) || e.Provider != "gl" || e.Op == "" {
		t.Fatalf("got %#v", err)
	}
	if len(f.paths()) != 0 {
		t.Fatalf("requests made: %v", f.paths())
	}
}

func TestLegacySingleFileSnippet(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	// Instances predating multi-file snippets report only file_name.
	f.json("GET /api/v4/snippets/3", 200, map[string]any{"id": 3, "title": "t", "file_name": "old.rb", "visibility": "private"})
	f.handle("GET /api/v4/snippets/3/raw", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("puts 1")) })
	s, err := p.GetSnippet(context.Background(), "3")
	if err != nil || len(s.Files) != 1 || s.Files[0].Name != "old.rb" || s.Files[0].Content != "puts 1" {
		t.Fatalf("snippet = %+v %v", s, err)
	}
	if _, err := p.GetSnippet(context.Background(), "x"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("non-numeric: %v", err)
	}
}
