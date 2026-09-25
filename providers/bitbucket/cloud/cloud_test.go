package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/forge/forgetest"
)

func TestContract(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	bad := f.provider(t, withCred(auth.Credential{Kind: auth.KindBasic, Username: testEmail, Token: "bad-token"}))
	forgetest.RunForgeProviderContractTests(t, p, forgetest.Options{
		ExistingRepo:    domain.RepositoryRef{Namespace: "acme", Name: "repo-001"},
		MissingRepo:     domain.RepositoryRef{Namespace: "acme", Name: "nope"},
		MinRepositories: 263,
		Secret:          testAPIToken,
		Unauthenticated: bad,
	})
}

func TestContractAccessToken(t *testing.T) {
	f := newFake(t)
	p := f.provider(t, withMethod(auth.MethodAccessToken), withCred(auth.Credential{Kind: auth.KindToken, Token: testAccessToken}),
		withYAML("type: bitbucket\nworkspace: acme\n"))
	bad := f.provider(t, withMethod(auth.MethodAccessToken), withCred(auth.Credential{Kind: auth.KindToken, Token: "nope"}),
		withYAML("workspace: acme\n"))
	forgetest.RunForgeProviderContractTests(t, p, forgetest.Options{
		ExistingRepo:    domain.RepositoryRef{Namespace: "acme", Name: "repo-002"},
		MissingRepo:     domain.RepositoryRef{Namespace: "acme", Name: "nope"},
		MinRepositories: 260,
		Secret:          testAccessToken,
		Unauthenticated: bad,
	})
}

func TestDriverMetadata(t *testing.T) {
	d := NewDriver()
	if d.Type() != "bitbucket" || d.DisplayName() != "Bitbucket Cloud" || d.DefaultHost() != "bitbucket.org" {
		t.Fatalf("unexpected driver identity: %s %s %s", d.Type(), d.DisplayName(), d.DefaultHost())
	}
	if !reflect.DeepEqual(d.AuthMethods(), []auth.Method{auth.MethodBasic, auth.MethodAccessToken, auth.MethodOAuth}) {
		t.Fatalf("auth methods = %v", d.AuthMethods())
	}
	p, err := d.New(forge.Account{Name: "bb"})
	if err != nil {
		t.Fatal(err)
	}
	m := p.Metadata()
	if m.APIURL != "https://api.bitbucket.org/2.0" || m.WebURL != "https://bitbucket.org" || m.Deployment != "cloud" || m.Terms.Namespace != "Workspace" {
		t.Fatalf("metadata = %+v", m)
	}
	for _, c := range []forge.Capability{forge.CapIssues, forge.CapIssueLabels, forge.CapSearchIssues, forge.CapRepoArchive,
		forge.CapReleases, forge.CapNotifications, forge.CapSettings, forge.CapPipelineRetry} {
		if p.Capabilities().Has(c) {
			t.Errorf("capability %s must not be declared", c)
		}
	}
	if _, ok := p.(forge.PullRequestHeadRefer); ok {
		t.Error("Bitbucket has no fetchable PR refs; PullRequestHeadRefer must not be implemented")
	}
	if _, err := d.New(forge.Account{Name: "bb", Decode: func(any) error { return errors.New("boom") }}); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("bad config: %v", err)
	}
}

func TestRepositoryURLs(t *testing.T) {
	p, _ := NewDriver().New(forge.Account{Name: "bb"})
	rp := p.(forge.RepositoryProvider)
	got := rp.RepositoryURLs(domain.RepositoryRef{Namespace: "acme", Name: "web"})
	want := domain.RepositoryURLs{Web: "https://bitbucket.org/acme/web", HTTPS: "https://bitbucket.org/acme/web.git",
		SSH: "git@ssh.bitbucket.org:acme/web.git", API: "https://api.bitbucket.org/2.0/repositories/acme/web"}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	p2, _ := NewDriver().New(forge.Account{Name: "bb", CloneBaseURL: "https://mirror.example/git/", SSHHost: "altssh.bitbucket.org", WebURL: "https://bb.example"})
	got = p2.(forge.RepositoryProvider).RepositoryURLs(domain.RepositoryRef{Namespace: "acme", Name: "web"})
	if got.HTTPS != "https://mirror.example/git/acme/web.git" || got.SSH != "git@altssh.bitbucket.org:acme/web.git" || got.Web != "https://bb.example/acme/web" {
		t.Fatalf("overrides not honored: %+v", got)
	}
}

func TestRepositoryMapping(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	r, err := p.GetRepository(context.Background(), domain.RepositoryRef{Namespace: "acme", Name: "repo-001"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "repo-001" || r.Namespace != "acme" || r.FullName != "acme/repo-001" || r.Visibility != domain.VisibilityPrivate ||
		r.DefaultBranch != "main" || r.ID != "{repo-acme-repo-001}" || r.ProviderType != "bitbucket" || r.Provider != "bb" {
		t.Fatalf("repository = %+v", r)
	}
	// The username Bitbucket embeds in HTTPS clone links is stripped.
	if r.URLs.HTTPS != "https://bitbucket.org/acme/repo-001.git" || r.URLs.SSH != "git@ssh.bitbucket.org:acme/repo-001.git" {
		t.Fatalf("clone URLs = %+v", r.URLs)
	}
	_, err = p.GetRepository(context.Background(), domain.RepositoryRef{Namespace: "acme", Name: "missing"})
	if !errors.Is(err, errs.ErrRepositoryNotFound) || !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing repo error = %v", err)
	}
}

func TestListRepositoriesAcrossWorkspaces(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	repos, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 263 {
		t.Fatalf("got %d repos", len(repos))
	}
	// 3 pages for acme (100/100/60), 1 for the empty workspace, 1 for solo.
	acme := f.calls("GET", "/2.0/repositories/acme")
	if len(acme) != 3 {
		t.Fatalf("acme pages = %d", len(acme))
	}
	for _, c := range acme {
		if c.Query.Get("role") != "member" || c.Query.Get("pagelen") != "100" || c.Query.Get("sort") != "-updated_on" {
			t.Errorf("unexpected query %v", c.Query)
		}
	}
	if len(f.calls("GET", "/2.0/repositories/empty")) != 1 || len(f.calls("GET", "/2.0/repositories/solo")) != 1 {
		t.Fatal("empty workspace must be skipped, not end the listing")
	}
	if got := repos[len(repos)-1].FullName; got != "solo/three" {
		t.Fatalf("last repo = %s", got)
	}

	// Namespace + visibility + owned-only filters.
	_, err = p.ListRepositories(context.Background(), forge.ListRepositoryOptions{Namespace: "acme", Visibility: domain.VisibilityPrivate,
		OwnedOnly: true, ListOptions: forge.ListOptions{Limit: 5}})
	if err != nil {
		t.Fatal(err)
	}
	c := f.last("GET", "/2.0/repositories/acme")
	if c.Query.Get("q") != "is_private=true" || c.Query.Get("role") != "owner" || c.Query.Get("pagelen") != "5" {
		t.Fatalf("filters = %v", c.Query)
	}
	_, err = p.ListRepositories(context.Background(), forge.ListRepositoryOptions{Visibility: domain.VisibilityInternal})
	if !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("internal visibility: %v", err)
	}
}

func TestPaginationRefusesForeignNext(t *testing.T) {
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilHits.Add(1)
		writeJSON(w, 200, map[string]any{"values": []any{}})
	}))
	defer evil.Close()
	f := newFake(t)
	f.handle("GET /2.0/repositories/acme", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"values": []any{f.repoJSON("acme", "a")}, "next": evil.URL + "/2.0/repositories/acme?page=2"})
	})
	p := f.provider(t)
	_, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{Namespace: "acme"})
	if err == nil || !strings.Contains(err.Error(), "refusing to follow") {
		t.Fatalf("want refusal, got %v", err)
	}
	if evilHits.Load() != 0 {
		t.Fatal("request was sent to the foreign host")
	}
	// Same host but outside the API base path is refused too.
	f.handle("GET /2.0/repositories/acme", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"values": []any{f.repoJSON("acme", "a")}, "next": f.srv.URL + "/elsewhere?page=2"})
	})
	if _, err := p.ListRepositories(context.Background(), forge.ListRepositoryOptions{Namespace: "acme"}); err == nil {
		t.Fatal("link outside the API base must be refused")
	}
}

func TestAuthHeaders(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()

	if _, err := f.provider(t).CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	u, pw, ok := (&http.Request{Header: f.last("GET", "/2.0/user").Header}).BasicAuth()
	if !ok || u != testEmail || pw != testAPIToken {
		t.Fatalf("basic auth = %q %v", u, ok)
	}

	// A bare token under a basic account uses the account username.
	pb := f.provider(t, withCred(auth.Credential{Kind: auth.KindToken, Token: testAPIToken}))
	if _, err := pb.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}

	po := f.provider(t, withMethod(auth.MethodOAuth), withCred(auth.Credential{Kind: auth.KindOAuth, Token: testOAuthToken}))
	if _, err := po.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	if h := f.last("GET", "/2.0/user").Header.Get("Authorization"); h != "Bearer "+testOAuthToken {
		t.Fatalf("oauth header = %q", h)
	}

	pa := f.provider(t, withMethod(auth.MethodAccessToken), withCred(auth.Credential{Kind: auth.KindToken, Token: testAccessToken}),
		withYAML("workspace: acme"))
	if _, err := pa.GetRepository(ctx, domain.RepositoryRef{Namespace: "acme", Name: "repo-003"}); err != nil {
		t.Fatal(err)
	}
	if h := f.last("GET", "/2.0/repositories/acme/repo-003").Header.Get("Authorization"); h != "Bearer "+testAccessToken {
		t.Fatalf("access token header = %q", h)
	}

	// Basic auth without an email is a configuration error.
	pn := f.provider(t, func(a *forge.Account) { a.Username = "" }, withCred(auth.Credential{Kind: auth.KindBasic, Token: testAPIToken}))
	if _, err := pn.CurrentUser(ctx); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("missing email: %v", err)
	}
}

func TestNotLoggedIn(t *testing.T) {
	f := newFake(t)
	p := f.provider(t, func(a *forge.Account) { a.Credentials = nil })
	_, err := p.CurrentUser(context.Background())
	if !errors.Is(err, errs.ErrNotAuthenticated) || errs.HintOf(err) != "trove auth login bb" {
		t.Fatalf("err = %v hint %q", err, errs.HintOf(err))
	}
	p = f.provider(t, func(a *forge.Account) { a.Credentials = &auth.MemoryStore{} })
	if _, err := p.CurrentUser(context.Background()); !errors.Is(err, errs.ErrNotAuthenticated) {
		t.Fatalf("empty store: %v", err)
	}
}

func TestCurrentUser(t *testing.T) {
	f := newFake(t)
	u, err := f.provider(t).CurrentUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := domain.User{ID: testUserUUID, Username: "jdoe", Name: "Jane Doe", Email: testEmail,
		WebURL: "https://bitbucket.org/jdoe/", AvatarURL: "https://avatar.example/jdoe.png"}
	if *u != want {
		t.Fatalf("user = %+v", u)
	}
	// Email lookup failures are ignored.
	f.handle("GET /2.0/user/emails", func(w http.ResponseWriter, r *http.Request) { writeError(w, 403, "missing scope email") })
	u, err = f.provider(t).CurrentUser(context.Background())
	if err != nil || u.Email != "" {
		t.Fatalf("user = %+v err %v", u, err)
	}
}

func TestAccessTokenCurrentUser(t *testing.T) {
	f := newFake(t)
	p := f.provider(t, withMethod(auth.MethodAccessToken), withCred(auth.Credential{Kind: auth.KindToken, Token: testAccessToken}),
		withYAML("workspace: acme\nother: ignored\n"))
	u, err := p.CurrentUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "access-token:acme" || u.Username != "" || u.Name != "acme access token" {
		t.Fatalf("user = %+v", u)
	}
	if len(f.calls("GET", "/2.0/user")) != 0 {
		t.Fatal("access tokens must not call GET /user")
	}
	if c := f.last("GET", "/2.0/repositories/acme"); c.Query.Get("pagelen") != "1" {
		t.Fatalf("validation query = %v", c.Query)
	}
	st, err := p.AuthStatus(context.Background())
	if err != nil || !st.Authenticated || st.Method != auth.MethodAccessToken || st.User != "acme access token" {
		t.Fatalf("status = %+v err %v", st, err)
	}
	user, pw, err := p.GitCredentials(context.Background())
	if err != nil || user != "x-token-auth" || pw != testAccessToken {
		t.Fatalf("git creds = %q err %v", user, err)
	}
	if _, err := p.ListSSHKeys(context.Background()); !errors.Is(err, errs.ErrUnsupportedCapability) {
		t.Fatalf("ssh keys with access token: %v", err)
	}

	noWS := f.provider(t, withMethod(auth.MethodAccessToken), withCred(auth.Credential{Kind: auth.KindToken, Token: testAccessToken}))
	_, err = noWS.CurrentUser(context.Background())
	if !errors.Is(err, errs.ErrInvalidConfiguration) || !strings.Contains(errs.HintOf(err), "workspace") {
		t.Fatalf("no workspace: %v (hint %q)", err, errs.HintOf(err))
	}
}

func TestLoginBasic(t *testing.T) {
	f := newFake(t)
	p := f.provider(t, func(a *forge.Account) { a.Credentials = nil; a.Username = "" })
	res, err := p.Login(context.Background(), auth.Request{Method: auth.MethodBasic, Username: testEmail, Token: testAPIToken})
	if err != nil {
		t.Fatal(err)
	}
	if res.User != "jdoe" || res.Credential.Kind != auth.KindBasic || res.Credential.Username != testEmail || res.Credential.Token != testAPIToken {
		t.Fatalf("result user=%q kind=%s", res.User, res.Credential.Kind)
	}
	if _, err := p.Login(context.Background(), auth.Request{Method: auth.MethodBasic, Token: testAPIToken}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("missing email: %v", err)
	}
	_, err = p.Login(context.Background(), auth.Request{Method: auth.MethodBasic, Username: testEmail, Token: "wrong-secret"})
	if !errors.Is(err, errs.ErrAuthenticationFailed) || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := p.Login(context.Background(), auth.Request{Method: "app"}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("unsupported method: %v", err)
	}
}

func TestLoginAccessToken(t *testing.T) {
	f := newFake(t)
	p := f.provider(t, withMethod(auth.MethodAccessToken), withYAML("workspace: acme"), func(a *forge.Account) { a.Credentials = nil })
	res, err := p.Login(context.Background(), auth.Request{Method: auth.MethodAccessToken, Token: testAccessToken})
	if err != nil {
		t.Fatal(err)
	}
	if res.Credential.Kind != auth.KindToken || res.User != "acme access token" {
		t.Fatalf("result = %q %s", res.User, res.Credential.Kind)
	}
}

func TestOAuthClientCredentialsAndRefresh(t *testing.T) {
	f := newFake(t)
	var grants []url.Values
	f.token = func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok || id != "consumer-key" || secret != "consumer-secret" {
			writeJSON(w, 400, map[string]string{"error": "invalid_client", "error_description": "Invalid OAuth client credentials"})
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.PostForm.Get("client_secret") != "" || r.PostForm.Get("client_id") != "" {
			t.Error("client credentials must be sent with HTTP Basic auth, not in the form")
		}
		f.mu.Lock()
		grants = append(grants, r.PostForm)
		n := len(grants)
		f.mu.Unlock()
		switch r.PostForm.Get("grant_type") {
		case "client_credentials":
			f.mu.Lock()
			f.bearer["oauth-cc-token"] = "oauth"
			f.mu.Unlock()
			writeJSON(w, 200, map[string]any{"access_token": "oauth-cc-token", "refresh_token": "refresh-1",
				"expires_in": 7200, "scopes": "repository account", "token_type": "bearer"})
		case "refresh_token":
			if r.PostForm.Get("refresh_token") != "refresh-1" {
				writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "bad refresh token"})
				return
			}
			tok := "oauth-refreshed-" + string(rune('0'+n))
			f.mu.Lock()
			f.bearer[tok] = "oauth"
			f.mu.Unlock()
			writeJSON(w, 200, map[string]any{"access_token": tok, "refresh_token": "refresh-1", "expires_in": 7200, "token_type": "bearer"})
		}
	}
	ctx := context.Background()

	// Login with the client credentials grant.
	p := f.provider(t, withMethod(auth.MethodOAuth), func(a *forge.Account) { a.Credentials = nil })
	res, err := p.Login(ctx, auth.Request{Method: auth.MethodOAuth, ClientID: "consumer-key", ClientSecret: "consumer-secret"})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Credential
	if c.Kind != auth.KindOAuth || c.Token != "oauth-cc-token" || c.RefreshToken != "refresh-1" || c.Username != "consumer-key" ||
		c.ClientSecret != "consumer-secret" || c.Expiry.Before(time.Now().Add(time.Hour)) || !reflect.DeepEqual(c.Scopes, []string{"repository", "account"}) {
		t.Fatalf("credential mismatch (token=%q kind=%s scopes=%v)", c.Token, c.Kind, c.Scopes)
	}
	if grants[0].Get("grant_type") != "client_credentials" {
		t.Fatalf("grant = %v", grants[0])
	}
	if _, err := p.Login(ctx, auth.Request{Method: auth.MethodOAuth, ClientID: "consumer-key", ClientSecret: "nope"}); !errors.Is(err, errs.ErrAuthenticationFailed) || strings.Contains(err.Error(), "nope") {
		t.Fatalf("bad secret: %v", err)
	}

	// An expired credential is refreshed transparently and persisted.
	expired := c
	expired.Token, expired.Expiry = "expired-token", time.Now().Add(-time.Minute)
	store := auth.NewMemoryStore(expired)
	p = f.provider(t, withMethod(auth.MethodOAuth), func(a *forge.Account) { a.Credentials = store })
	if _, err := p.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	saved, _ := store.Load(ctx)
	if !strings.HasPrefix(saved.Token, "oauth-refreshed-") || saved.ClientSecret != "consumer-secret" || saved.Username != "consumer-key" || saved.Expired() {
		t.Fatalf("saved credential not refreshed (token %q)", saved.Token)
	}
	if last := grants[len(grants)-1]; last.Get("grant_type") != "refresh_token" || last.Get("refresh_token") != "refresh-1" {
		t.Fatalf("refresh grant = %v", last)
	}
	if h := f.last("GET", "/2.0/user").Header.Get("Authorization"); h != "Bearer "+saved.Token {
		t.Fatalf("API call used %q", h)
	}
	st, err := p.AuthStatus(ctx)
	if err != nil || !st.Authenticated || !reflect.DeepEqual(st.Scopes, []string{"repository", "account", "pullrequest"}) {
		t.Fatalf("status = %+v err %v", st, err)
	}

	// Explicit refresh.
	st, err = p.RefreshCredential(ctx)
	if err != nil || !st.Authenticated {
		t.Fatalf("refresh: %+v %v", st, err)
	}

	// A rejected refresh token falls back to re-running the client
	// credentials grant.
	bad := c
	bad.RefreshToken, bad.Expiry = "revoked", time.Now().Add(-time.Minute)
	store = auth.NewMemoryStore(bad)
	p = f.provider(t, withMethod(auth.MethodOAuth), func(a *forge.Account) { a.Credentials = store })
	user, pw, err := p.GitCredentials(ctx)
	if err != nil || user != "x-token-auth" || pw != "oauth-cc-token" {
		t.Fatalf("git creds after re-grant: %q err %v", user, err)
	}
	if last := grants[len(grants)-1]; last.Get("grant_type") != "client_credentials" {
		t.Fatalf("expected re-grant, got %v", last)
	}

	// A pre-issued token without a client secret cannot be refreshed.
	p = f.provider(t, withMethod(auth.MethodOAuth), withCred(auth.Credential{Kind: auth.KindOAuth, Token: "x", Expiry: time.Now().Add(-time.Hour)}))
	if _, err := p.CurrentUser(ctx); !errors.Is(err, errs.ErrAuthenticationFailed) {
		t.Fatalf("expired pre-issued token: %v", err)
	}
	if _, err := f.provider(t).RefreshCredential(ctx); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("refresh of basic credential: %v", err)
	}

	// Pre-issued OAuth access token login.
	res, err = f.provider(t, withMethod(auth.MethodOAuth)).Login(ctx, auth.Request{Method: auth.MethodOAuth, Token: testOAuthToken})
	if err != nil || res.Credential.Kind != auth.KindOAuth || res.Credential.RefreshToken != "" || res.User != "jdoe" {
		t.Fatalf("pre-issued login: %+v %v", res, err)
	}
}

func TestGitCredentialsBasic(t *testing.T) {
	f := newFake(t)
	user, pw, err := f.provider(t).GitCredentials(context.Background())
	if err != nil || user != "x-bitbucket-api-token-auth" || pw != testAPIToken {
		t.Fatalf("git creds = %q err %v", user, err)
	}
}

func TestAuthStatusBasic(t *testing.T) {
	f := newFake(t)
	st, err := f.provider(t).AuthStatus(context.Background())
	if err != nil || !st.Authenticated || st.User != "jdoe" || st.Method != auth.MethodBasic || len(st.Scopes) != 0 {
		t.Fatalf("status = %+v err %v", st, err)
	}
	st, err = f.provider(t, withCred(auth.Credential{Kind: auth.KindBasic, Username: testEmail, Token: "revoked"})).AuthStatus(context.Background())
	if err != nil || st.Authenticated {
		t.Fatalf("revoked status = %+v err %v", st, err)
	}
}

func TestErrorMapping(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	ctx := context.Background()
	cases := []struct {
		status int
		header map[string]string
		kind   error
		msg    string
	}{
		{401, nil, errs.ErrAuthenticationFailed, "authentication failed"},
		{403, nil, errs.ErrPermissionDenied, "You do not have access"},
		{404, nil, errs.ErrNotFound, "pull request #1 not found in acme/x"},
		{409, nil, errs.ErrConflict, "You do not have access"},
		{400, nil, errs.ErrInvalidArgument, "You do not have access: extra detail"},
		{429, map[string]string{"Retry-After": "120"}, errs.ErrRateLimited, "rate limit"},
		{500, nil, errs.ErrProviderAPI, "HTTP 500"},
	}
	for _, tc := range cases {
		f.handle("GET /2.0/repositories/acme/x/pullrequests/1", func(w http.ResponseWriter, r *http.Request) {
			for k, v := range tc.header {
				w.Header().Set(k, v)
			}
			writeJSON(w, tc.status, map[string]any{"type": "error", "error": map[string]any{
				"message": "You do not have access", "detail": "extra detail"}})
		})
		_, err := p.GetPullRequest(ctx, domain.RepositoryRef{Namespace: "acme", Name: "x"}, 1)
		if !errors.Is(err, tc.kind) {
			t.Errorf("%d: want %v, got %v", tc.status, tc.kind, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%d: message %q lacks %q", tc.status, err.Error(), tc.msg)
		}
		if strings.Contains(err.Error(), testAPIToken) || strings.Contains(err.Error(), testEmail) {
			t.Errorf("%d: error leaks credentials: %v", tc.status, err)
		}
		var e *errs.Error
		if !errors.As(err, &e) || e.Status != tc.status || e.Provider != "bb" {
			t.Errorf("%d: bad *errs.Error %+v", tc.status, e)
		}
		if tc.status == 429 && e.RetryAfter != 120*time.Second {
			t.Errorf("RetryAfter = %v", e.RetryAfter)
		}
	}
}

func TestRateLimitRetried(t *testing.T) {
	f := newFake(t)
	var n atomic.Int32
	f.handle("GET /2.0/repositories/acme/x", func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			writeError(w, 429, "Rate limit for this resource has been exceeded")
			return
		}
		writeJSON(w, 200, f.repoJSON("acme", "x"))
	})
	if _, err := f.provider(t).GetRepository(context.Background(), domain.RepositoryRef{Namespace: "acme", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Fatalf("attempts = %d", n.Load())
	}
}

func TestContextCanceledDuringRequest(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.handle("GET /2.0/repositories/acme/slow", func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	})
	_, err := f.provider(t).GetRepository(ctx, domain.RepositoryRef{Namespace: "acme", Name: "slow"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
