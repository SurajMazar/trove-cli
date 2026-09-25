package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/redact"
)

// defaultOAuthScopes are requested by the device flow unless overridden.
var defaultOAuthScopes = []string{"repo", "read:org", "gist", "notifications", "workflow", "admin:public_key"}

type apiUser struct {
	ID                int64   `json:"id"`
	Login             string  `json:"login"`
	Name              *string `json:"name"`
	Email             *string `json:"email"`
	HTMLURL           string  `json:"html_url"`
	AvatarURL         string  `json:"avatar_url"`
	Type              string  `json:"type"`
	Blog              *string `json:"blog"`
	Company           *string `json:"company"`
	Location          *string `json:"location"`
	Bio               *string `json:"bio"`
	TwitterUsername   *string `json:"twitter_username"`
	Hireable          *bool   `json:"hireable"`
	PublicRepos       *int    `json:"public_repos"`
	TotalPrivateRepos *int    `json:"total_private_repos"`
}

func (u apiUser) toDomain() *domain.User {
	return &domain.User{
		ID: strconv.FormatInt(u.ID, 10), Username: u.Login, Name: str(u.Name), Email: str(u.Email),
		WebURL: u.HTMLURL, AvatarURL: u.AvatarURL,
	}
}

type apiApp struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
	Owner   *struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// botLogin is how GitHub names an App's bot account.
func (a apiApp) botLogin() string { return a.Slug + "[bot]" }

func (a apiApp) toDomain() *domain.User {
	return &domain.User{ID: strconv.FormatInt(a.ID, 10), Username: a.botLogin(), Name: a.Name, WebURL: a.HTMLURL}
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- credential loading -------------------------------------------------------

func (p *Provider) notLoggedIn(cause error) error {
	return &errs.Error{
		Kind: errs.ErrNotAuthenticated, Provider: p.acct.Name, Cause: cause,
		Message: fmt.Sprintf("%s account %q is not logged in", p.meta.DisplayName, p.acct.Name),
		Hint:    p.loginHint(),
	}
}

// credential loads (once) and returns the stored credential. Expired OAuth
// credentials that carry a refresh token (GitHub App user tokens) are
// refreshed and persisted transparently.
func (p *Provider) credential(ctx context.Context) (auth.Credential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var c auth.Credential
	if p.cred != nil {
		c = *p.cred
	} else {
		if p.acct.Credentials == nil {
			return auth.Credential{}, p.notLoggedIn(nil)
		}
		loaded, err := p.acct.Credentials.Load(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return auth.Credential{}, p.canceled("load credentials", ctxErr)
			}
			if errors.Is(err, errs.ErrNotAuthenticated) || errors.Is(err, errs.ErrSecretNotFound) {
				return auth.Credential{}, p.notLoggedIn(err)
			}
			return auth.Credential{}, errs.WithProvider(err, p.acct.Name)
		}
		if strings.TrimSpace(loaded.Token) == "" {
			return auth.Credential{}, p.notLoggedIn(nil)
		}
		c = loaded
	}
	if !p.isAppCredential(c) && c.Expired() && c.Refreshable() {
		nc, err := p.refreshAndSave(ctx, c)
		if err != nil && nc.Token == "" {
			return auth.Credential{}, err
		}
		if err != nil {
			p.log.WarnContext(ctx, "refreshed GitHub token could not be saved", "account", p.acct.Name, "error", err.Error())
		}
		c = nc
	}
	p.cred = &c
	return c, nil
}

func (p *Provider) isAppCredential(c auth.Credential) bool {
	return c.Kind == auth.KindApp || (p.isAppAccount() && strings.HasPrefix(strings.TrimSpace(c.Token), "-----BEGIN"))
}

// accessToken returns the bearer token for API calls: the stored token, or a
// freshly minted (cached) installation token for GitHub App accounts.
func (p *Provider) accessToken(ctx context.Context) (string, error) {
	c, err := p.credential(ctx)
	if err != nil {
		return "", err
	}
	if p.isAppCredential(c) {
		return p.installationToken(ctx, c)
	}
	return c.Token, nil
}

// GitCredentials implements forge.GitAuthenticator. GitHub accepts any
// username with a token as the password over HTTPS; "x-access-token" is the
// documented convention and required for App installation tokens.
func (p *Provider) GitCredentials(ctx context.Context) (string, string, error) {
	tok, err := p.accessToken(ctx)
	if err != nil {
		return "", "", err
	}
	return "x-access-token", tok, nil
}

// --- current user ----------------------------------------------------------------

// CurrentUser implements forge.Provider. For GitHub App accounts it returns
// the App's bot identity (GET /app), since installation tokens cannot call
// GET /user.
func (p *Provider) CurrentUser(ctx context.Context) (*domain.User, error) {
	const op = "get current user"
	if err := p.ctxErr(ctx, op); err != nil {
		return nil, err
	}
	c, err := p.credential(ctx)
	if err != nil {
		return nil, err
	}
	if p.isAppCredential(c) {
		app, err := p.getApp(ctx, c)
		if err != nil {
			return nil, err
		}
		return app.toDomain(), nil
	}
	var u apiUser
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/user", out: &u}); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.login = u.Login
	p.mu.Unlock()
	return u.toDomain(), nil
}

// currentLogin returns the authenticated user's login, fetching it once.
func (p *Provider) currentLogin(ctx context.Context) (string, error) {
	p.mu.Lock()
	login := p.login
	p.mu.Unlock()
	if login != "" {
		return login, nil
	}
	u, err := p.CurrentUser(ctx)
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// --- login -------------------------------------------------------------------------

type tokenInfo struct {
	scopes      []string
	scopesKnown bool // X-OAuth-Scopes present (classic PAT / OAuth App token)
	expiry      time.Time
}

// validateToken calls GET /user with token and reads token metadata headers.
func (p *Provider) validateToken(ctx context.Context, op, token string) (*apiUser, tokenInfo, error) {
	var u apiUser
	resp, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/user", out: &u, auth: authExplicit, token: token})
	if err != nil {
		return nil, tokenInfo{}, err
	}
	var info tokenInfo
	if vals, ok := resp.Header[http.CanonicalHeaderKey("X-OAuth-Scopes")]; ok {
		info.scopesKnown = true
		info.scopes = parseScopes(strings.Join(vals, ","))
	}
	info.expiry = parseTokenExpiration(resp.Header.Get("GitHub-Authentication-Token-Expiration"))
	return &u, info, nil
}

func parseScopes(s string) []string {
	var out []string
	for _, sc := range strings.Split(s, ",") {
		if sc = strings.TrimSpace(sc); sc != "" {
			out = append(out, sc)
		}
	}
	return out
}

// parseTokenExpiration parses the expiration header GitHub sends for
// expiring PATs, e.g. "2026-10-01 12:00:00 UTC" or "... -0700".
func parseTokenExpiration(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Login implements forge.Authenticator. It never persists the credential.
func (p *Provider) Login(ctx context.Context, req auth.Request) (*auth.Result, error) {
	const op = "login"
	method := req.Method
	if method == "" {
		method = p.acct.AuthMethod
	}
	if method == "" {
		method = auth.MethodOAuth
		if strings.TrimSpace(req.Token) != "" {
			method = auth.MethodToken
		}
	}
	switch method {
	case auth.MethodToken:
		tok := strings.TrimSpace(req.Token)
		if tok == "" {
			return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "a GitHub personal access token is required")
		}
		return p.loginWithToken(ctx, op, tok, auth.KindToken)
	case auth.MethodOAuth:
		if tok := strings.TrimSpace(req.Token); tok != "" {
			// A pre-issued OAuth access token (e.g. from another tool).
			return p.loginWithToken(ctx, op, tok, auth.KindOAuth)
		}
		return p.deviceLogin(ctx, op, req)
	case auth.MethodApp:
		return p.appLogin(ctx, op, req)
	default:
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "%s does not support auth method %q (supported: token, oauth, app)", p.meta.DisplayName, method)
	}
}

func (p *Provider) loginWithToken(ctx context.Context, op, tok string, kind auth.CredentialKind) (*auth.Result, error) {
	u, info, err := p.validateToken(ctx, op, tok)
	if err != nil {
		return nil, err
	}
	c := auth.Credential{Kind: kind, Token: tok, Scopes: info.scopes, Expiry: info.expiry}
	return &auth.Result{Credential: c, User: u.Login}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (p *Provider) tokenURL() string { return p.web + "/login/oauth/access_token" }

func (p *Provider) deviceLogin(ctx context.Context, op string, req auth.Request) (*auth.Result, error) {
	clientID := firstNonEmpty(req.ClientID, p.acct.ClientID)
	if clientID == "" {
		return nil, &errs.Error{
			Kind: errs.ErrInvalidConfiguration, Provider: p.acct.Name, Op: op,
			Message: "the GitHub OAuth device flow needs an OAuth App client ID",
			Hint: "register an OAuth App (Settings > Developer settings > OAuth Apps) with Device Flow enabled, " +
				"then set auth.client_id for account " + p.acct.Name + " (or log in with a token)",
		}
	}
	if req.Prompter == nil {
		return nil, p.errorf(errs.ErrInteractionRequired, op, 0, "the device flow needs an interactive terminal to show the verification code")
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = p.acct.Scopes
	}
	if len(scopes) == 0 {
		scopes = defaultOAuthScopes
	}
	flow := &auth.DeviceFlow{
		DeviceCodeURL: p.web + "/login/device/code",
		TokenURL:      p.tokenURL(),
		ClientID:      clientID,
		Scopes:        scopes,
		HTTPClient:    p.hc,
	}
	// RequestCode and Poll are called separately (rather than Run) so that a
	// rejected device authorization request can be reported as a
	// configuration problem.
	dc, err := flow.RequestCode(ctx)
	if err != nil {
		return nil, p.deviceCodeError(ctx, op, err)
	}
	req.Prompter.DeviceCode(dc.VerificationURI, dc.UserCode, time.Duration(dc.ExpiresIn)*time.Second)
	c, err := flow.Poll(ctx, dc)
	if err != nil {
		return nil, p.oauthError(ctx, op, err)
	}
	c.Kind = auth.KindOAuth
	c.Username = clientID // needed to refresh GitHub App user tokens
	u, info, err := p.validateToken(ctx, op, c.Token)
	if err != nil {
		return nil, err
	}
	if info.scopesKnown {
		c.Scopes = info.scopes
	}
	return &auth.Result{Credential: c, User: u.Login}, nil
}

// deviceCodeError maps failures of the device authorization request. An
// unknown client ID or an App without Device Flow enabled does not always
// produce an RFC 6749 error body, so anything other than a transport error
// is reported as a configuration problem.
func (p *Provider) deviceCodeError(ctx context.Context, op string, err error) error {
	var oe *auth.OAuthError
	var ue *url.Error
	if ctx.Err() != nil || errors.As(err, &oe) || errors.As(err, &ue) {
		return p.oauthError(ctx, op, err)
	}
	return &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: p.acct.Name, Op: op,
		Message: p.meta.DisplayName + " rejected the device authorization request: " + safeMessage(err.Error()),
		Hint:    "check auth.client_id for account " + p.acct.Name + " and that Device Flow is enabled for the OAuth App"}
}

// oauthError maps device-flow / refresh errors (RFC 6749 codes plus GitHub's
// own, e.g. device_flow_disabled) to typed errors.
func (p *Provider) oauthError(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return p.canceled(op, ctxErr)
	}
	var oe *auth.OAuthError
	if !errors.As(err, &oe) {
		return p.errorf(errs.ErrProviderAPI, op, 0, "OAuth request failed: %s", redact.String(err.Error()))
	}
	e := &errs.Error{Provider: p.acct.Name, Op: op, Kind: errs.ErrAuthenticationFailed}
	switch oe.Code {
	case "access_denied":
		e.Kind, e.Message = errs.ErrAborted, "authorization was denied"
	case "expired_token":
		e.Message, e.Hint = "the device code expired before authorization completed", p.loginHint()
	case "device_flow_disabled":
		e.Kind, e.Message = errs.ErrInvalidConfiguration, "Device Flow is not enabled for this OAuth App"
		e.Hint = "enable Device Flow in the OAuth App settings on " + p.web
	case "incorrect_client_credentials", "unauthorized_client", "invalid_client":
		e.Kind, e.Message = errs.ErrInvalidConfiguration, "the OAuth client ID was rejected"
		e.Hint = "check auth.client_id for account " + p.acct.Name
	case "bad_refresh_token", "invalid_grant":
		e.Message, e.Hint = "the refresh token is invalid or expired", p.loginHint()
	default:
		e.Message = "OAuth error " + safeMessage(oe.Code)
		if oe.Description != "" {
			e.Message += ": " + safeMessage(oe.Description)
		}
	}
	return e
}

// --- status / refresh --------------------------------------------------------------

// AuthStatus implements forge.Authenticator. A rejected credential is
// reported as Authenticated=false rather than as an error; other failures
// (not logged in, network) are returned as errors.
func (p *Provider) AuthStatus(ctx context.Context) (*auth.Status, error) {
	const op = "auth status"
	c, err := p.credential(ctx)
	if err != nil {
		return nil, err
	}
	if p.isAppCredential(c) {
		app, err := p.getApp(ctx, c)
		if err == nil {
			_, err = p.installationToken(ctx, c)
		}
		if err != nil {
			if errors.Is(err, errs.ErrAuthenticationFailed) {
				return &auth.Status{Method: auth.MethodApp, Detail: detailOf(err)}, nil
			}
			return nil, err
		}
		p.appMu.Lock()
		exp := p.instExpiry
		p.appMu.Unlock()
		return &auth.Status{
			Authenticated: true, User: app.botLogin(), Method: auth.MethodApp, ExpiresAt: exp,
			Detail: fmt.Sprintf("GitHub App %q, installation %s (installation tokens are minted on demand and expire after an hour)", app.Slug, p.acct.InstallationID),
		}, nil
	}
	method := auth.MethodToken
	if c.Kind == auth.KindOAuth {
		method = auth.MethodOAuth
	}
	u, info, err := p.validateToken(ctx, op, c.Token)
	if err != nil {
		if errors.Is(err, errs.ErrAuthenticationFailed) {
			return &auth.Status{Method: method, Detail: detailOf(err)}, nil
		}
		return nil, err
	}
	p.mu.Lock()
	p.login = u.Login
	p.mu.Unlock()
	st := &auth.Status{Authenticated: true, User: u.Login, Method: method, Scopes: c.Scopes, ExpiresAt: c.Expiry}
	if info.scopesKnown {
		st.Scopes = info.scopes
	} else {
		st.Detail = "fine-grained token: permissions are not reported as OAuth scopes"
	}
	if !info.expiry.IsZero() {
		st.ExpiresAt = info.expiry
	}
	return st, nil
}

func detailOf(err error) string {
	var e *errs.Error
	if errors.As(err, &e) && e.Message != "" {
		return e.Message
	}
	return err.Error()
}

// RefreshCredential implements forge.Refresher. Only credentials that carry
// a refresh token (GitHub App user-to-server tokens with expiration enabled)
// can be refreshed; classic OAuth App tokens and PATs cannot.
func (p *Provider) RefreshCredential(ctx context.Context) (*auth.Status, error) {
	const op = "refresh credential"
	if err := p.ctxErr(ctx, op); err != nil {
		return nil, err
	}
	if p.acct.Credentials == nil {
		return nil, p.notLoggedIn(nil)
	}
	c, err := p.acct.Credentials.Load(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, p.canceled(op, ctxErr)
		}
		if errors.Is(err, errs.ErrNotAuthenticated) || errors.Is(err, errs.ErrSecretNotFound) {
			return nil, p.notLoggedIn(err)
		}
		return nil, errs.WithProvider(err, p.acct.Name)
	}
	if !c.Refreshable() || p.isAppCredential(c) {
		return nil, &errs.Error{
			Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Op: op,
			Message: "this GitHub credential cannot be refreshed: it has no refresh token " +
				"(personal access tokens and OAuth App tokens do not use refresh tokens; App installation tokens are minted automatically)",
			Hint: p.loginHint(),
		}
	}
	nc, err := p.refreshAndSave(ctx, c)
	if nc.Token != "" {
		p.mu.Lock()
		p.cred = &nc
		p.mu.Unlock()
	}
	if err != nil {
		return nil, err
	}
	return p.AuthStatus(ctx)
}

// refreshAndSave exchanges the refresh token and persists the result. When
// the exchange succeeds but saving fails, it returns the new credential
// together with the error (GitHub rotates refresh tokens, so the caller
// should keep using the new one).
func (p *Provider) refreshAndSave(ctx context.Context, c auth.Credential) (auth.Credential, error) {
	const op = "refresh credential"
	clientID := firstNonEmpty(c.Username, p.acct.ClientID)
	if clientID == "" {
		return auth.Credential{}, p.errorf(errs.ErrInvalidConfiguration, op, 0, "cannot refresh the token without the OAuth client ID (set auth.client_id)")
	}
	nc, err := auth.RefreshToken(ctx, p.hc, p.tokenURL(), clientID, c.ClientSecret, c.RefreshToken, false)
	if err != nil {
		return auth.Credential{}, p.oauthError(ctx, op, err)
	}
	nc.Kind = auth.KindOAuth
	nc.Username = clientID
	nc.ClientSecret = c.ClientSecret
	if len(nc.Scopes) == 0 {
		nc.Scopes = c.Scopes
	}
	if p.acct.Credentials != nil {
		if err := p.acct.Credentials.Save(ctx, nc); err != nil {
			return nc, &errs.Error{Kind: errs.ErrSecretUnavailable, Provider: p.acct.Name, Op: op,
				Message: "the refreshed token could not be saved", Cause: err}
		}
	}
	return nc, nil
}

// --- GitHub App installation auth --------------------------------------------------

func (p *Provider) appConfig(op string, appID, instID string) (string, string, error) {
	appID = firstNonEmpty(appID, p.acct.AppID)
	instID = firstNonEmpty(instID, p.acct.InstallationID)
	if appID == "" || instID == "" {
		return "", "", &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: p.acct.Name, Op: op,
			Message: "GitHub App authentication requires both an app ID and an installation ID",
			Hint:    "set auth.app_id and auth.installation_id for account " + p.acct.Name}
	}
	if _, err := strconv.ParseInt(instID, 10, 64); err != nil {
		return "", "", p.errorf(errs.ErrInvalidConfiguration, op, 0, "invalid installation ID %q: must be a number", instID)
	}
	return appID, instID, nil
}

// parseRSAKey parses a PKCS#1 ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE KEY")
// PEM-encoded RSA private key, as downloaded from the App settings page.
func parseRSAKey(pemData string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemData)))
	if block == nil {
		return nil, errors.New("the GitHub App private key is not PEM encoded")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("the GitHub App private key is not an RSA key")
		}
		return rk, nil
	}
	return nil, fmt.Errorf("unsupported PEM block %q for a GitHub App private key", block.Type)
}

// keyLocked returns the parsed key for pemData; p.appMu must be held.
func (p *Provider) keyLocked(op, pemData string) (*rsa.PrivateKey, error) {
	if p.appKey != nil && p.appKeyPEM == pemData {
		return p.appKey, nil
	}
	k, err := parseRSAKey(pemData)
	if err != nil {
		// The parse error never contains key material.
		return nil, p.errorf(errs.ErrInvalidConfiguration, op, 0, "invalid GitHub App private key: %v", err)
	}
	p.appKey, p.appKeyPEM = k, pemData
	p.instToken, p.instExpiry = "", time.Time{}
	return k, nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signAppJWT creates the RS256 JWT GitHub Apps use to authenticate as the
// App: iat is backdated 60s for clock drift and exp is 9 minutes ahead
// (GitHub rejects anything over 10 minutes).
func signAppJWT(key *rsa.PrivateKey, appID string, now time.Time) (string, error) {
	var iss any = appID
	if n, err := strconv.ParseInt(appID, 10, 64); err == nil {
		iss = n // numeric App ID; client IDs ("Iv1...") stay strings
	}
	header := b64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": iss,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + b64url(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64url(sig), nil
}

func (p *Provider) appJWT(op string, c auth.Credential) (string, error) {
	appID, _, err := p.appConfig(op, "", "")
	if err != nil {
		return "", err
	}
	p.appMu.Lock()
	key, err := p.keyLocked(op, c.Token)
	p.appMu.Unlock()
	if err != nil {
		return "", err
	}
	jwt, err := signAppJWT(key, appID, p.now())
	if err != nil {
		return "", p.errorf(errs.ErrInvalidConfiguration, op, 0, "sign GitHub App JWT: %v", err)
	}
	return jwt, nil
}

// installationToken returns a cached installation token, minting a new one
// when none is cached or it expires within a minute. Installation tokens
// live in memory only and are never persisted.
func (p *Provider) installationToken(ctx context.Context, c auth.Credential) (string, error) {
	const op = "create installation token"
	appID, instID, err := p.appConfig(op, "", "")
	if err != nil {
		return "", err
	}
	p.appMu.Lock()
	defer p.appMu.Unlock()
	key, err := p.keyLocked(op, c.Token)
	if err != nil {
		return "", err
	}
	if p.instToken != "" && p.now().Before(p.instExpiry.Add(-time.Minute)) {
		return p.instToken, nil
	}
	tok, exp, err := p.exchangeInstallationToken(ctx, op, key, appID, instID)
	if err != nil {
		return "", err
	}
	p.instToken, p.instExpiry = tok, exp
	return tok, nil
}

func (p *Provider) exchangeInstallationToken(ctx context.Context, op string, key *rsa.PrivateKey, appID, instID string) (string, time.Time, error) {
	jwt, err := signAppJWT(key, appID, p.now())
	if err != nil {
		return "", time.Time{}, p.errorf(errs.ErrInvalidConfiguration, op, 0, "sign GitHub App JWT: %v", err)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	_, err = p.do(ctx, request{
		op: op, method: http.MethodPost, path: "/app/installations/" + esc(instID) + "/access_tokens",
		out: &out, auth: authExplicit, token: jwt,
		notFoundMsg: fmt.Sprintf("GitHub App installation %s was not found for app %s", instID, appID),
	})
	if err != nil {
		return "", time.Time{}, appAuthError(err)
	}
	if out.Token == "" {
		return "", time.Time{}, p.errorf(errs.ErrProviderAPI, op, 0, "installation token response did not include a token")
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = p.now().Add(time.Hour)
	}
	return out.Token, out.ExpiresAt, nil
}

// appAuthError rewrites 401 messages for JWT-authenticated calls.
func appAuthError(err error) error {
	var e *errs.Error
	if errors.As(err, &e) && errors.Is(err, errs.ErrAuthenticationFailed) {
		cp := *e
		cp.Message = "GitHub App authentication failed: the app ID or private key was rejected (clock skew can also cause this)"
		return &cp
	}
	return err
}

// getApp calls GET /app authenticated as the App (JWT).
func (p *Provider) getApp(ctx context.Context, c auth.Credential) (*apiApp, error) {
	const op = "get GitHub App"
	jwt, err := p.appJWT(op, c)
	if err != nil {
		return nil, err
	}
	var app apiApp
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/app", out: &app, auth: authExplicit, token: jwt}); err != nil {
		return nil, appAuthError(err)
	}
	return &app, nil
}

func (p *Provider) appLogin(ctx context.Context, op string, req auth.Request) (*auth.Result, error) {
	appID, instID, err := p.appConfig(op, req.AppID, req.InstallationID)
	if err != nil {
		return nil, err
	}
	keyPEM := strings.TrimSpace(req.Token)
	if keyPEM == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "GitHub App login requires the App's PEM private key")
	}
	key, err := parseRSAKey(keyPEM)
	if err != nil {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "invalid GitHub App private key: %v", err)
	}
	// Minting an installation token proves app ID, key and installation.
	if _, _, err := p.exchangeInstallationToken(ctx, op, key, appID, instID); err != nil {
		return nil, err
	}
	jwt, err := signAppJWT(key, appID, p.now())
	if err != nil {
		return nil, p.errorf(errs.ErrInvalidConfiguration, op, 0, "sign GitHub App JWT: %v", err)
	}
	var app apiApp
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/app", out: &app, auth: authExplicit, token: jwt}); err != nil {
		return nil, appAuthError(err)
	}
	return &auth.Result{Credential: auth.Credential{Kind: auth.KindApp, Token: keyPEM}, User: app.botLogin()}, nil
}
