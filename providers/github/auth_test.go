package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

func tokenStore(s string) *auth.MemoryStore { return auth.TokenStore(s) }

type prompter struct {
	mu   sync.Mutex
	uri  string
	code string
}

func (p *prompter) DeviceCode(uri, code string, _ time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.uri, p.code = uri, code
}
func (p *prompter) Info(string) {}

func TestLoginToken(t *testing.T) {
	f, p := setup(t)
	res, err := p.Login(bg, auth.Request{Method: auth.MethodToken, Token: " " + testToken + "\n"})
	if err != nil {
		t.Fatal(err)
	}
	if res.User != "octocat" || res.Credential.Kind != auth.KindToken || res.Credential.Token != testToken ||
		!reflect.DeepEqual(res.Credential.Scopes, []string{"repo", "gist", "read:org"}) {
		t.Fatalf("result = %+v scopes %v", res.User, res.Credential.Scopes)
	}
	if h := f.last(http.MethodGet, "/user").Header.Get("Authorization"); h != "Bearer "+testToken {
		t.Fatal("login did not validate the supplied token")
	}

	res, err = p.Login(bg, auth.Request{Token: testFineGrained}) // method inferred from the token
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC); !res.Credential.Expiry.Equal(want) || res.Credential.Scopes != nil {
		t.Fatalf("fine-grained: expiry %v scopes %v", res.Credential.Expiry, res.Credential.Scopes)
	}

	_, err = p.Login(bg, auth.Request{Method: auth.MethodToken, Token: testBadToken})
	assertKind(t, err, errs.ErrAuthenticationFailed)
	_, err = p.Login(bg, auth.Request{Method: auth.MethodToken})
	assertKind(t, err, errs.ErrInvalidArgument)
	_, err = p.Login(bg, auth.Request{Method: auth.MethodBasic, Token: "x"})
	assertKind(t, err, errs.ErrInvalidArgument)

	// Pre-issued OAuth token.
	res, err = p.Login(bg, auth.Request{Method: auth.MethodOAuth, Token: testToken})
	if err != nil || res.Credential.Kind != auth.KindOAuth {
		t.Fatalf("oauth token: %+v %v", res, err)
	}
}

func TestDeviceFlowLogin(t *testing.T) {
	t.Parallel()
	f := newFake(t, "/api/v3") // GHES: device endpoints live on the web host
	p := newTestProvider(t, f, nil)
	f.devicePending = 1
	pr := &prompter{}
	res, err := p.Login(bg, auth.Request{Method: auth.MethodOAuth, ClientID: testClientID, Prompter: pr})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Credential
	if res.User != "octocat" || c.Kind != auth.KindOAuth || c.Token != testDeviceToken || c.RefreshToken != testRefreshToken ||
		c.Username != testClientID || c.Expiry.IsZero() || !reflect.DeepEqual(c.Scopes, []string{"repo", "gist", "read:org"}) {
		t.Fatalf("credential: user=%s kind=%s user=%s scopes=%v", res.User, c.Kind, c.Username, c.Scopes)
	}
	if pr.code != "ABCD-1234" || !strings.HasSuffix(pr.uri, "/login/device") {
		t.Fatalf("prompter got %q %q", pr.uri, pr.code)
	}
	dc := f.last(http.MethodPost, "/login/device/code")
	if dc.Query.Get("scope") != "repo read:org gist notifications workflow admin:public_key" || dc.Query.Get("client_id") != testClientID {
		t.Fatalf("device code form = %v", dc.Query)
	}
	if polls := f.requests(http.MethodPost, "/login/oauth/access_token"); len(polls) != 2 {
		t.Fatalf("polls = %d (want pending then success)", len(polls))
	}
}

func TestDeviceFlowConfig(t *testing.T) {
	t.Parallel()
	f := newFake(t, "")
	p := newTestProvider(t, f, func(a *forge.Account) { a.ClientID = testClientID; a.Scopes = []string{"repo"} })
	if _, err := p.Login(bg, auth.Request{Method: auth.MethodOAuth, Prompter: &prompter{}}); err != nil {
		t.Fatal(err)
	}
	if s := f.last(http.MethodPost, "/login/device/code").Query.Get("scope"); s != "repo" {
		t.Fatalf("account scopes not used: %q", s)
	}

	noID := newTestProvider(t, f, nil)
	_, err := noID.Login(bg, auth.Request{Method: auth.MethodOAuth, Prompter: &prompter{}})
	if e := assertKind(t, err, errs.ErrInvalidConfiguration); !strings.Contains(e.Hint, "auth.client_id") || !strings.Contains(e.Hint, "OAuth App") {
		t.Fatalf("hint = %q", e.Hint)
	}
	_, err = noID.Login(bg, auth.Request{Method: auth.MethodOAuth, ClientID: testClientID})
	assertKind(t, err, errs.ErrInteractionRequired)
	_, err = noID.Login(bg, auth.Request{Method: auth.MethodOAuth, ClientID: "wrong", Prompter: &prompter{}})
	assertKind(t, err, errs.ErrInvalidConfiguration)
}

func oauthStore(token string, expiry time.Time) *auth.MemoryStore {
	return auth.NewMemoryStore(auth.Credential{Kind: auth.KindOAuth, Token: token, RefreshToken: testRefreshToken,
		Username: testClientID, Expiry: expiry})
}

func TestRefreshCredential(t *testing.T) {
	f := newFake(t, "")
	store := oauthStore(testToken, time.Now().Add(time.Hour))
	p := newTestProvider(t, f, func(a *forge.Account) { a.Credentials = store })
	st, err := p.RefreshCredential(bg)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Authenticated || st.Method != auth.MethodOAuth || st.User != "octocat" {
		t.Fatalf("status = %+v", st)
	}
	saved, _ := store.Load(bg)
	if saved.Token != testRefreshed || saved.RefreshToken != "ghr_ROTATEDrefreshABCDEFGHIJKLMNOPQRSTUVWX" || saved.Username != testClientID || saved.Expiry.IsZero() {
		t.Fatalf("saved credential not updated (kind %s)", saved.Kind)
	}
	form := f.last(http.MethodPost, "/login/oauth/access_token").Query
	if form.Get("grant_type") != "refresh_token" || form.Get("client_id") != testClientID {
		t.Fatalf("refresh form = %v", form)
	}
	if _, err := p.CurrentUser(bg); err != nil {
		t.Fatal(err)
	}
	if h := f.last(http.MethodGet, "/user").Header.Get("Authorization"); h != "Bearer "+testRefreshed {
		t.Fatal("refreshed token not used")
	}

	pat := newTestProvider(t, f, nil)
	_, err = pat.RefreshCredential(bg)
	if e := assertKind(t, err, errs.ErrInvalidArgument); !strings.Contains(e.Message, "cannot be refreshed") {
		t.Fatalf("message = %q", e.Message)
	}

	badStore := auth.NewMemoryStore(auth.Credential{Kind: auth.KindOAuth, Token: testToken, RefreshToken: "ghr_bad", Username: testClientID})
	bad := newTestProvider(t, f, func(a *forge.Account) { a.Credentials = badStore })
	_, err = bad.RefreshCredential(bg)
	assertKind(t, err, errs.ErrAuthenticationFailed)
}

func TestGitCredentials(t *testing.T) {
	f := newFake(t, "")
	u, pw, err := newTestProvider(t, f, nil).GitCredentials(bg)
	if err != nil || u != "x-access-token" || pw != testToken {
		t.Fatalf("token: %s %v", u, err)
	}

	// An expired OAuth credential with a refresh token is refreshed first.
	store := oauthStore("gho_EXPIREDtokenABCDEFGHIJKLMNOPQRSTUVWXYZ", time.Now().Add(-time.Minute))
	p := newTestProvider(t, f, func(a *forge.Account) { a.Credentials = store })
	u, pw, err = p.GitCredentials(bg)
	if err != nil || u != "x-access-token" || pw != testRefreshed {
		t.Fatalf("refreshed: %s %v (got refreshed=%v)", u, err, pw == testRefreshed)
	}
	if saved, _ := store.Load(bg); saved.Token != testRefreshed {
		t.Fatal("refreshed token not persisted")
	}

	_, _, err = newTestProvider(t, f, func(a *forge.Account) { a.Credentials = nil }).GitCredentials(bg)
	assertKind(t, err, errs.ErrNotAuthenticated)
}

func TestAuthStatus(t *testing.T) {
	f, p := setup(t)
	st, err := p.AuthStatus(bg)
	if err != nil || !st.Authenticated || st.User != "octocat" || st.Method != auth.MethodToken || len(st.Scopes) != 3 {
		t.Fatalf("classic: %+v %v", st, err)
	}
	fg := newTestProvider(t, f, func(a *forge.Account) { a.Credentials = tokenStore(testFineGrained) })
	st, err = fg.AuthStatus(bg)
	if err != nil || !st.Authenticated || st.ExpiresAt.Year() != 2030 || st.Detail == "" || st.Scopes != nil {
		t.Fatalf("fine-grained: %+v %v", st, err)
	}
	bad := newTestProvider(t, f, func(a *forge.Account) { a.Credentials = tokenStore(testBadToken) })
	st, err = bad.AuthStatus(bg)
	if err != nil || st.Authenticated || !strings.Contains(st.Detail, "authentication failed") {
		t.Fatalf("bad: %+v %v", st, err)
	}
	_, err = newTestProvider(t, f, func(a *forge.Account) { a.Credentials = nil }).AuthStatus(bg)
	assertKind(t, err, errs.ErrNotAuthenticated)
}

// --- GitHub App --------------------------------------------------------------------

func genKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pemPKCS1(k *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func pemPKCS8(t *testing.T, k *rsa.PrivateKey) string {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}))
}

func appProvider(t *testing.T, f *fakeGitHub, keyPEM string) *Provider {
	return newTestProvider(t, f, func(a *forge.Account) {
		a.AuthMethod = auth.MethodApp
		a.AppID, a.InstallationID = testAppID, testInstID
		a.Credentials = auth.NewMemoryStore(auth.Credential{Kind: auth.KindApp, Token: keyPEM})
	})
}

func TestGitHubApp(t *testing.T) {
	key := genKey(t)
	for name, keyPEM := range map[string]string{"PKCS1": pemPKCS1(key), "PKCS8": pemPKCS8(t, key)} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t, "")
			f.appPub = &key.PublicKey
			p := appProvider(t, f, keyPEM)

			res, err := p.Login(bg, auth.Request{Method: auth.MethodApp, Token: keyPEM})
			if err != nil {
				t.Fatal(err)
			}
			if res.User != "trove-app[bot]" || res.Credential.Kind != auth.KindApp || res.Credential.Token != strings.TrimSpace(keyPEM) {
				t.Fatalf("login = %s %s", res.User, res.Credential.Kind)
			}
			if f.lastJWTClaims["iss"] != float64(4242) {
				t.Fatalf("claims = %v", f.lastJWTClaims)
			}

			u, err := p.CurrentUser(bg)
			if err != nil || u.Username != "trove-app[bot]" || u.ID != "4242" {
				t.Fatalf("current user: %+v %v", u, err)
			}

			repos, err := p.ListRepositories(bg, forge.ListRepositoryOptions{IncludeArchived: true})
			if err != nil || len(repos) != 120 {
				t.Fatalf("installation repos: %d %v", len(repos), err)
			}
			if h := f.last(http.MethodGet, "/installation/repositories").Header.Get("Authorization"); h != "Bearer "+testInstToken {
				t.Fatalf("installation token not used: %q", h)
			}
			before := f.exchanges
			_, pw, err := p.GitCredentials(bg)
			if err != nil || pw != testInstToken || f.exchanges != before {
				t.Fatalf("git credentials: %v (re-minted %d)", err, f.exchanges-before)
			}

			// A token within a minute of expiry is replaced.
			p.appMu.Lock()
			p.instExpiry = time.Now().Add(30 * time.Second)
			p.appMu.Unlock()
			if _, _, err := p.GitCredentials(bg); err != nil || f.exchanges != before+1 {
				t.Fatalf("expiring token not re-minted: %v", err)
			}

			ns, err := p.ListNamespaces(bg, forge.ListOptions{})
			if err != nil || len(ns) != 1 || ns[0].FullPath != "octo-org" || ns[0].Type != "organization" {
				t.Fatalf("namespaces: %+v %v", ns, err)
			}
			s, err := p.Summary(bg)
			if err != nil || s.Repositories == nil || *s.Repositories != 120 || s.OpenIssues != nil {
				t.Fatalf("summary: %+v %v", s, err)
			}
			st, err := p.AuthStatus(bg)
			if err != nil || !st.Authenticated || st.Method != auth.MethodApp || st.ExpiresAt.IsZero() || st.User != "trove-app[bot]" {
				t.Fatalf("status: %+v %v", st, err)
			}
		})
	}
}

func TestGitHubAppErrors(t *testing.T) {
	key, other := genKey(t), genKey(t)
	f := newFake(t, "")
	f.appPub = &key.PublicKey

	// Signed with the wrong key.
	wrong := appProvider(t, f, pemPKCS1(other))
	_, err := wrong.CurrentUser(bg)
	if e := assertKind(t, err, errs.ErrAuthenticationFailed); !strings.Contains(e.Message, "GitHub App") {
		t.Fatalf("message = %q", e.Message)
	}
	_, _, err = wrong.GitCredentials(bg)
	assertKind(t, err, errs.ErrAuthenticationFailed)
	st, err := wrong.AuthStatus(bg)
	if err != nil || st.Authenticated {
		t.Fatalf("status with wrong key: %+v %v", st, err)
	}
	if strings.Contains(st.Detail, "PRIVATE KEY") {
		t.Fatal("status leaks key material")
	}

	// Missing configuration and malformed keys.
	noIDs := newTestProvider(t, f, func(a *forge.Account) {
		a.AuthMethod = auth.MethodApp
		a.Credentials = auth.NewMemoryStore(auth.Credential{Kind: auth.KindApp, Token: pemPKCS1(key)})
	})
	_, err = noIDs.CurrentUser(bg)
	assertKind(t, err, errs.ErrInvalidConfiguration)
	_, err = noIDs.Login(bg, auth.Request{Method: auth.MethodApp, Token: pemPKCS1(key)})
	assertKind(t, err, errs.ErrInvalidConfiguration)

	p := appProvider(t, f, "-----BEGIN RSA PRIVATE KEY-----\nnot a key\n-----END RSA PRIVATE KEY-----")
	_, err = p.CurrentUser(bg)
	assertKind(t, err, errs.ErrInvalidConfiguration)
	_, err = p.Login(bg, auth.Request{Method: auth.MethodApp, Token: "garbage"})
	assertKind(t, err, errs.ErrInvalidArgument)
	_, err = p.Login(bg, auth.Request{Method: auth.MethodApp, Token: pemPKCS1(key), InstallationID: "999"})
	if e := assertKind(t, err, errs.ErrNotFound); !strings.Contains(e.Message, "installation 999") {
		t.Fatalf("message = %q", e.Message)
	}
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatal("not found")
	}
}

func TestSignAppJWT(t *testing.T) {
	key := genKey(t)
	f := newFake(t, "")
	f.appPub = &key.PublicKey
	jwt, err := signAppJWT(key, testAppID, time.Now())
	if err != nil || !f.verifyJWT(jwt) {
		t.Fatalf("JWT does not verify: %v", err)
	}
	if strings.Count(jwt, ".") != 2 {
		t.Fatal("malformed JWT")
	}
}

func TestOAuthErrorMapping(t *testing.T) {
	_, p := setup(t)
	for code, kind := range map[string]error{
		"access_denied":                errs.ErrAborted,
		"expired_token":                errs.ErrAuthenticationFailed,
		"device_flow_disabled":         errs.ErrInvalidConfiguration,
		"incorrect_client_credentials": errs.ErrInvalidConfiguration,
		"bad_refresh_token":            errs.ErrAuthenticationFailed,
		"something_new":                errs.ErrAuthenticationFailed,
	} {
		err := p.oauthError(bg, "login", &auth.OAuthError{Code: code, Description: "d"})
		if !errors.Is(err, kind) {
			t.Errorf("%s: got %v, want %v", code, err, kind)
		}
	}
	if err := p.oauthError(bg, "login", errors.New("boom")); !errors.Is(err, errs.ErrProviderAPI) {
		t.Errorf("generic error: %v", err)
	}
}
