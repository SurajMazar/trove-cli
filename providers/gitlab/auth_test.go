package gitlab

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/errs"
)

type recordingPrompter struct {
	mu   sync.Mutex
	uri  string
	code string
}

func (r *recordingPrompter) DeviceCode(uri, code string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uri, r.code = uri, code
}
func (r *recordingPrompter) Info(string) {}

func TestLoginWithToken(t *testing.T) {
	f := newFake(t)
	store := &auth.MemoryStore{}
	p := f.newProvider(t, withStore(store))
	res, err := p.Login(context.Background(), auth.Request{Method: auth.MethodToken, Token: " " + testPAT + "\n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.User != "alice" || res.Credential.Kind != auth.KindToken || res.Credential.Token != testPAT {
		t.Fatalf("result = user %q kind %q", res.User, res.Credential.Kind)
	}
	if h := f.last(t, "GET", "/api/v4/user").Header; h.Get("PRIVATE-TOKEN") != testPAT {
		t.Fatal("login did not validate with the supplied token")
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("Login persisted the credential")
	}

	_, err = p.Login(context.Background(), auth.Request{Method: auth.MethodToken, Token: "glpat-invalidINVALIDinvalid00"})
	if !errors.Is(err, errs.ErrAuthenticationFailed) || strings.Contains(err.Error(), "glpat-invalid") {
		t.Fatalf("invalid token: %v", err)
	}
	if _, err := p.Login(context.Background(), auth.Request{Method: auth.MethodToken}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("missing token: %v", err)
	}
	if _, err := p.Login(context.Background(), auth.Request{Method: auth.MethodApp}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("app method: %v", err)
	}
}

func TestLoginWithPreIssuedOAuthToken(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t, withClientID("cid"))
	res, err := p.Login(context.Background(), auth.Request{Method: auth.MethodOAuth, Token: testOAuth})
	if err != nil {
		t.Fatal(err)
	}
	if res.Credential.Kind != auth.KindOAuth || res.Credential.Username != "cid" {
		t.Fatalf("credential kind %q client %q", res.Credential.Kind, res.Credential.Username)
	}
	if h := f.last(t, "GET", "/api/v4/user").Header; h.Get("Authorization") != "Bearer "+testOAuth {
		t.Fatal("OAuth token not sent as Bearer")
	}
}

func TestDeviceFlowLogin(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t, withClientID("app-123"))
	f.handle("POST /oauth/authorize_device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "app-123" || r.Form.Get("scope") != "api read_user" {
			t.Errorf("device code form = %v", r.Form)
		}
		writeJSON(w, 200, map[string]any{"device_code": "dev-1", "user_code": "ABCD-EFGH",
			"verification_uri": f.srv.URL + "/oauth/device", "expires_in": 300, "interval": 1})
	})
	polls := 0
	f.handle("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "dev-1" {
			t.Errorf("token form = %v", r.Form)
		}
		polls++
		writeJSON(w, 200, map[string]any{"access_token": testOAuth, "refresh_token": "refresh-1",
			"token_type": "Bearer", "expires_in": 7200, "scope": "api read_user"})
	})
	pr := &recordingPrompter{}
	res, err := p.Login(context.Background(), auth.Request{Method: auth.MethodOAuth, Prompter: pr})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Credential
	if c.Kind != auth.KindOAuth || c.Token != testOAuth || c.RefreshToken != "refresh-1" || c.Username != "app-123" ||
		c.Expiry.IsZero() || len(c.Scopes) != 2 || res.User != "alice" {
		t.Fatalf("credential = kind %q user %q scopes %v expiry %v", c.Kind, res.User, c.Scopes, c.Expiry)
	}
	if pr.code != "ABCD-EFGH" || !strings.HasSuffix(pr.uri, "/oauth/device") || polls != 1 {
		t.Fatalf("prompter got %q %q, polls %d", pr.uri, pr.code, polls)
	}
}

func TestDeviceFlowErrors(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	_, err := p.Login(context.Background(), auth.Request{Method: auth.MethodOAuth, Prompter: &recordingPrompter{}})
	if !errors.Is(err, errs.ErrInvalidConfiguration) || !strings.Contains(errs.HintOf(err), "Device authorization grant") {
		t.Fatalf("missing client ID: %v (hint %q)", err, errs.HintOf(err))
	}
	_, err = p.Login(context.Background(), auth.Request{Method: auth.MethodOAuth, ClientID: "x"})
	if !errors.Is(err, errs.ErrInteractionRequired) {
		t.Fatalf("no prompter: %v", err)
	}
	f.json("POST /oauth/authorize_device", 401, map[string]any{"error": "invalid_client", "error_description": "Client authentication failed"})
	_, err = p.Login(context.Background(), auth.Request{Method: auth.MethodOAuth, ClientID: "x", Prompter: &recordingPrompter{}})
	if !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("invalid client: %v", err)
	}
	f2 := newFake(t) // no device endpoint (GitLab < 17.2): HTTP 404
	p2 := f2.newProvider(t)
	_, err = p2.Login(context.Background(), auth.Request{Method: auth.MethodOAuth, ClientID: "x", Prompter: &recordingPrompter{}})
	if !errors.Is(err, errs.ErrAuthenticationFailed) || !strings.Contains(errs.HintOf(err), "17.2") {
		t.Fatalf("old instance: %v (hint %q)", err, errs.HintOf(err))
	}
}

func refreshHandler(t *testing.T, f *fakeGitLab, newToken string, calls *int) {
	f.handle("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-old" || r.Form.Get("client_id") != "client-123" {
			t.Errorf("refresh form = %v", r.Form)
		}
		*calls++
		f.mu.Lock()
		f.oauthToken = newToken
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"access_token": newToken, "refresh_token": "refresh-new", "token_type": "Bearer", "expires_in": 7200})
	})
}

func TestExpiredOAuthCredentialIsRefreshedAndSaved(t *testing.T) {
	f := newFake(t)
	store := oauthStore("oauth-expired-token-000000", "refresh-old", time.Now().Add(-time.Hour))
	p := f.newProvider(t, withStore(store))
	calls := 0
	refreshHandler(t, f, "oauth-fresh-token-11111111", &calls)
	u, err := p.CurrentUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "alice" || calls != 1 {
		t.Fatalf("user %q, refresh calls %d", u.Username, calls)
	}
	if h := f.last(t, "GET", "/api/v4/user").Header; h.Get("Authorization") != "Bearer oauth-fresh-token-11111111" {
		t.Fatalf("request used %q", h.Get("Authorization"))
	}
	saved, _ := store.Load(context.Background())
	if saved.Token != "oauth-fresh-token-11111111" || saved.RefreshToken != "refresh-new" || saved.Username != "client-123" ||
		saved.Kind != auth.KindOAuth || saved.Expired() {
		t.Fatalf("saved credential: kind %q refresh ok=%v expired=%v", saved.Kind, saved.RefreshToken == "refresh-new", saved.Expired())
	}
	// The refreshed credential is reused without another exchange.
	if _, err := p.CurrentUser(context.Background()); err != nil || calls != 1 {
		t.Fatalf("second call: %v, refresh calls %d", err, calls)
	}
}

func TestOAuth401TriggersRefresh(t *testing.T) {
	f := newFake(t)
	// Expiry unknown, but the server no longer accepts the token.
	store := oauthStore("oauth-revoked-token-000000", "refresh-old", time.Time{})
	p := f.newProvider(t, withStore(store))
	calls := 0
	refreshHandler(t, f, "oauth-fresh-token-22222222", &calls)
	if _, err := p.CurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("refresh calls %d", calls)
	}
	if saved, _ := store.Load(context.Background()); saved.Token != "oauth-fresh-token-22222222" {
		t.Fatal("refreshed credential not saved")
	}
}

func TestRefreshFailureIsAuthError(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t, withStore(oauthStore("oauth-expired-token-000000", "refresh-old", time.Now().Add(-time.Hour))))
	f.json("POST /oauth/token", 400, map[string]any{"error": "invalid_grant", "error_description": "The provided authorization grant is invalid"})
	_, err := p.CurrentUser(context.Background())
	if !errors.Is(err, errs.ErrAuthenticationFailed) || errs.HintOf(err) == "" || strings.Contains(err.Error(), "refresh-old") {
		t.Fatalf("got %v", err)
	}
}

func TestRefreshCredential(t *testing.T) {
	f := newFake(t)
	store := oauthStore(testOAuth, "refresh-old", time.Now().Add(time.Hour))
	p := f.newProvider(t, withStore(store))
	calls := 0
	refreshHandler(t, f, "oauth-fresh-token-33333333", &calls)
	st, err := p.RefreshCredential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Authenticated || st.Method != auth.MethodOAuth || st.ExpiresAt.IsZero() || calls != 1 {
		t.Fatalf("status %+v calls %d", st, calls)
	}
	if saved, _ := store.Load(context.Background()); saved.Token != "oauth-fresh-token-33333333" {
		t.Fatal("not saved")
	}
	pat := f.newProvider(t)
	if _, err := pat.RefreshCredential(context.Background()); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("PAT refresh: %v", err)
	}
}

func TestAuthStatus(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	f.json("GET /api/v4/personal_access_tokens/self", 200, map[string]any{"name": "laptop", "scopes": []string{"api", "read_user"}, "expires_at": "2027-01-31", "active": true})
	st, err := p.AuthStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Authenticated || st.User != "alice" || st.Method != auth.MethodToken || len(st.Scopes) != 2 ||
		st.ExpiresAt.Format("2006-01-02") != "2027-01-31" || !strings.Contains(st.Detail, "laptop") {
		t.Fatalf("status = %+v", st)
	}

	// Older instances without the endpoint still report status.
	f2 := newFake(t)
	f2.json("GET /api/v4/user", 200, map[string]any{"id": 9, "username": "project_5_bot", "bot": true})
	st, err = f2.newProvider(t).AuthStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.User != "project_5_bot" || st.Scopes != nil || !strings.Contains(st.Detail, "bot user") {
		t.Fatalf("status = %+v", st)
	}

	bad := f.newProvider(t, withStore(auth.TokenStore("glpat-wrongTOKENwrongTOKEN99")))
	if _, err := bad.AuthStatus(context.Background()); !errors.Is(err, errs.ErrAuthenticationFailed) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestRevokeOAuth(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t, withStore(oauthStore(testOAuth, "r", time.Time{})))
	f.handle("POST /oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("token") != testOAuth || r.Form.Get("client_id") != "client-123" {
			t.Errorf("revoke form = %v", r.Form)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("PRIVATE-TOKEN") != "" {
			t.Error("revoke request carried an auth header")
		}
		writeJSON(w, 200, map[string]any{})
	})
	if err := p.RevokeCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.last(t, "POST", "/oauth/revoke")
}

func TestRevokePAT(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	f.handle("DELETE /api/v4/personal_access_tokens/self", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := p.RevokeCredential(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := f.last(t, "DELETE", "/api/v4/personal_access_tokens/self").Header; h.Get("PRIVATE-TOKEN") != testPAT {
		t.Fatal("revoke did not authenticate with the token being revoked")
	}
}

func TestGitCredentials(t *testing.T) {
	f := newFake(t)
	user, pass, err := f.newProvider(t).GitCredentials(context.Background())
	if err != nil || user != "oauth2" || pass != testPAT {
		t.Fatalf("PAT git credentials: %q, %v", user, err)
	}
	store := oauthStore("oauth-expired-token-000000", "refresh-old", time.Now().Add(-time.Minute))
	p := f.newProvider(t, withStore(store))
	calls := 0
	refreshHandler(t, f, "oauth-fresh-token-44444444", &calls)
	user, pass, err = p.GitCredentials(context.Background())
	if err != nil || user != "oauth2" || pass != "oauth-fresh-token-44444444" || calls != 1 {
		t.Fatalf("OAuth git credentials: %q, refreshed=%v, %v", user, pass == "oauth-fresh-token-44444444", err)
	}
	if _, _, err := f.newProvider(t, withStore(nil)).GitCredentials(context.Background()); !errors.Is(err, errs.ErrNotAuthenticated) {
		t.Fatalf("no credential: %v", err)
	}
}
