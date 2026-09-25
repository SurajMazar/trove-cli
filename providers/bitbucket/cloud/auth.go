package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/httpx"
)

// Git-over-HTTPS usernames documented by Atlassian. The password is always
// the token itself.
const (
	// gitUserAPIToken is the static username for Atlassian API tokens.
	gitUserAPIToken = "x-bitbucket-api-token-auth"
	// gitUserBearer is used with repository/project/workspace access tokens
	// and OAuth access tokens.
	gitUserBearer = "x-token-auth"
)

// accessTokenUserPrefix prefixes the synthetic user ID returned for access
// tokens, which do not belong to a Bitbucket user.
const accessTokenUserPrefix = "access-token:"

// Login implements forge.Authenticator. Supported methods:
//
//   - basic: Atlassian account email (req.Username or the account's
//     username) + API token (req.Token), sent with HTTP Basic auth.
//   - access-token: a repository, project or workspace access token
//     (req.Token), sent as a Bearer token. Requires the account's
//     `workspace` setting, because such tokens have no user to validate.
//   - oauth: an OAuth consumer (req.ClientID + req.ClientSecret) using the
//     client credentials grant, or a pre-issued OAuth access token
//     (req.Token without a client secret, which cannot be refreshed).
//
// The credential is validated against the API but not persisted.
func (p *Provider) Login(ctx context.Context, req auth.Request) (*auth.Result, error) {
	method := req.Method
	if method == "" {
		method = p.acct.AuthMethod
	}
	if method == "" {
		method = auth.MethodBasic
	}
	var cred auth.Credential
	switch method {
	case auth.MethodBasic:
		user := firstNonEmpty(req.Username, p.acct.Username)
		if user == "" {
			return nil, p.invalidArg("login", "Bitbucket API tokens require your Atlassian account email as the username")
		}
		if strings.TrimSpace(req.Token) == "" {
			return nil, p.invalidArg("login", "an Atlassian API token is required (create one at https://id.atlassian.com/manage-profile/security/api-tokens)")
		}
		cred = auth.Credential{Kind: auth.KindBasic, Username: user, Token: strings.TrimSpace(req.Token)}
	case auth.MethodAccessToken:
		if strings.TrimSpace(req.Token) == "" {
			return nil, p.invalidArg("login", "a Bitbucket repository, project or workspace access token is required")
		}
		cred = auth.Credential{Kind: auth.KindToken, Token: strings.TrimSpace(req.Token)}
	case auth.MethodOAuth:
		clientID := firstNonEmpty(req.ClientID, p.acct.ClientID)
		switch {
		case req.ClientSecret != "":
			if clientID == "" {
				return nil, p.invalidArg("login", "an OAuth consumer key (client ID) is required")
			}
			c, err := p.clientCredentialsGrant(ctx, clientID, req.ClientSecret)
			if err != nil {
				return nil, err
			}
			cred = c
		case strings.TrimSpace(req.Token) != "":
			cred = auth.Credential{Kind: auth.KindOAuth, Token: strings.TrimSpace(req.Token)}
		default:
			return nil, p.invalidArg("login", "OAuth login needs a consumer key and secret (client credentials grant) or a pre-issued access token")
		}
	default:
		return nil, p.invalidArg("login", fmt.Sprintf("Bitbucket Cloud does not support auth method %q (supported: basic, access-token, oauth)", method))
	}

	u, _, err := p.identify(ctx, method, &cred)
	if err != nil {
		return nil, err
	}
	user := u.Username
	if user == "" {
		user = u.Name
	}
	return &auth.Result{Credential: cred, User: user}, nil
}

// AuthStatus implements forge.Authenticator.
func (p *Provider) AuthStatus(ctx context.Context) (*auth.Status, error) {
	cred, err := p.credential(ctx)
	if err != nil {
		if errors.Is(err, errs.ErrAuthenticationFailed) {
			return &auth.Status{Authenticated: false, Method: p.acct.AuthMethod, Detail: err.Error()}, nil
		}
		return nil, err
	}
	method := p.authMethodFor(cred)
	u, scopes, err := p.identify(ctx, method, nil)
	if err != nil {
		if errors.Is(err, errs.ErrAuthenticationFailed) {
			return &auth.Status{Authenticated: false, Method: method, Detail: err.Error()}, nil
		}
		return nil, err
	}
	st := &auth.Status{Authenticated: true, Method: method, ExpiresAt: cred.Expiry}
	switch method {
	case auth.MethodBasic:
		st.User = u.Username
		// API tokens offer no scope introspection.
		st.Detail = "Atlassian API token for " + firstNonEmpty(cred.Username, p.acct.Username)
	case auth.MethodAccessToken:
		st.User = u.Name
		st.Detail = "access token validated against workspace " + p.workspace
	default:
		st.User = u.Username
		st.Scopes = scopes
		if len(st.Scopes) == 0 {
			st.Scopes = cred.Scopes
		}
		if cred.ClientSecret != "" {
			st.Detail = "OAuth consumer (client credentials grant)"
		} else {
			st.Detail = "pre-issued OAuth access token"
		}
	}
	return st, nil
}

// RefreshCredential implements forge.Refresher for OAuth consumer
// credentials: it uses the refresh token (or re-runs the client credentials
// grant) and persists the result through the account's credential store.
func (p *Provider) RefreshCredential(ctx context.Context) (*auth.Status, error) {
	cur, err := p.credential(ctx)
	if err != nil {
		return nil, err
	}
	if cur.Kind != auth.KindOAuth || (cur.RefreshToken == "" && cur.ClientSecret == "") {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Op: "refresh credential",
			Message: "this Bitbucket credential cannot be refreshed (only OAuth consumer credentials can)"}
	}
	p.mu.Lock()
	c, err := p.refreshLocked(ctx, cur)
	if err == nil {
		p.cred = &c
	}
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &auth.Status{Authenticated: true, Method: auth.MethodOAuth, Scopes: c.Scopes, ExpiresAt: c.Expiry,
		Detail: "OAuth access token refreshed"}, nil
}

// refreshLocked obtains a fresh OAuth access token. Bitbucket requires the
// consumer key/secret via HTTP Basic auth on the token endpoint. p.mu must be
// held.
func (p *Provider) refreshLocked(ctx context.Context, c auth.Credential) (auth.Credential, error) {
	if c.Username == "" || c.ClientSecret == "" {
		return auth.Credential{}, &errs.Error{Kind: errs.ErrAuthenticationFailed, Provider: p.acct.Name,
			Message: "Bitbucket OAuth access token expired and cannot be refreshed",
			Hint:    "trove auth login " + p.acct.Name}
	}
	var (
		nc  auth.Credential
		err error
	)
	if c.RefreshToken != "" {
		nc, err = auth.RefreshToken(ctx, p.hc, p.tokenURL, c.Username, c.ClientSecret, c.RefreshToken, true)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return auth.Credential{}, cerr
			}
			p.logger.DebugContext(ctx, "bitbucket refresh token grant failed; re-running client credentials grant",
				"provider", p.acct.Name, "oauth_error", auth.IsOAuthError(err, ""))
		}
	}
	if c.RefreshToken == "" || err != nil {
		// Client credentials tokens can always be re-issued with the
		// consumer secret.
		nc, err = p.clientCredentialsGrant(ctx, c.Username, c.ClientSecret)
		if err != nil {
			return auth.Credential{}, err
		}
	}
	nc.Kind = auth.KindOAuth
	nc.Username = c.Username
	nc.ClientSecret = c.ClientSecret
	if len(nc.Scopes) == 0 {
		nc.Scopes = c.Scopes
	}
	if p.acct.Credentials != nil {
		if err := p.acct.Credentials.Save(ctx, nc); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return auth.Credential{}, cerr
			}
			// The new token still works for this process; it will simply be
			// refreshed again next time.
			p.logger.WarnContext(ctx, "could not persist refreshed Bitbucket OAuth token", "provider", p.acct.Name,
				"error", err.Error())
		}
	}
	return nc, nil
}

// bitbucketTokenResponse is Bitbucket's OAuth token response. Bitbucket
// reports granted scopes in "scopes" (space separated) rather than the RFC
// 6749 "scope" member.
type bitbucketTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	Scopes           string `json:"scopes"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// clientCredentialsGrant runs the OAuth 2.0 client credentials grant
// (RFC 6749 §4.4) against https://bitbucket.org/site/oauth2/access_token with
// the consumer key/secret as HTTP Basic credentials.
func (p *Provider) clientCredentialsGrant(ctx context.Context, clientID, clientSecret string) (auth.Credential, error) {
	const op = "oauth client credentials grant"
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return auth.Credential{}, fmt.Errorf("%s: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httpx.UserAgent())
	req.SetBasicAuth(clientID, clientSecret)
	resp, err := p.hc.Do(req)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return auth.Credential{}, fmt.Errorf("%s: %w", op, cerr)
		}
		return auth.Credential{}, &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: op,
			Message: "could not reach the Bitbucket OAuth token endpoint"}
	}
	defer resp.Body.Close()
	var tok bitbucketTokenResponse
	body := httpx.ReadBody(resp, 1<<20)
	_ = json.Unmarshal(body, &tok)
	if resp.StatusCode != http.StatusOK || tok.Error != "" || tok.AccessToken == "" {
		e := &errs.Error{Kind: errs.ErrAuthenticationFailed, Provider: p.acct.Name, Op: op, Status: resp.StatusCode,
			Message: "Bitbucket rejected the OAuth consumer credentials", Hint: "check the consumer key and secret"}
		if tok.Error != "" {
			e.Cause = &auth.OAuthError{Code: tok.Error, Description: tok.ErrorDescription}
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			e.Kind, e.RetryAfter, e.Message, e.Hint = errs.ErrRateLimited, httpx.RetryAfter(resp.Header), "Bitbucket rate limit exceeded", ""
		} else if resp.StatusCode >= 500 {
			e.Kind, e.Message, e.Hint = errs.ErrProviderAPI, fmt.Sprintf("Bitbucket OAuth token endpoint returned HTTP %d", resp.StatusCode), ""
		}
		return auth.Credential{}, e
	}
	c := auth.Credential{
		Kind: auth.KindOAuth, Token: tok.AccessToken, RefreshToken: tok.RefreshToken, TokenType: tok.TokenType,
		Username: clientID, ClientSecret: clientSecret,
	}
	if tok.ExpiresIn > 0 {
		c.Expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	if s := firstNonEmpty(tok.Scopes, tok.Scope); s != "" {
		c.Scopes = splitScopes(s)
	}
	return c, nil
}

func splitScopes(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' })
}

// GitCredentials implements forge.GitAuthenticator using Bitbucket's
// documented Git-over-HTTPS usernames: "x-bitbucket-api-token-auth" for
// Atlassian API tokens and "x-token-auth" for access tokens and OAuth access
// tokens. Expired OAuth tokens are refreshed first.
func (p *Provider) GitCredentials(ctx context.Context) (string, string, error) {
	c, err := p.credential(ctx)
	if err != nil {
		return "", "", err
	}
	if c.Token == "" {
		return "", "", p.notLoggedIn("stored Bitbucket credential is empty")
	}
	if p.authMethodFor(c) == auth.MethodBasic {
		return gitUserAPIToken, c.Token, nil
	}
	return gitUserBearer, c.Token, nil
}

// --- current user --------------------------------------------------------------

type bbLink struct {
	Href string `json:"href"`
	Name string `json:"name"`
}

type bbLinks struct {
	Self   bbLink   `json:"self"`
	HTML   bbLink   `json:"html"`
	Avatar bbLink   `json:"avatar"`
	Clone  []bbLink `json:"clone"`
}

type bbAccount struct {
	Type        string  `json:"type"`
	UUID        string  `json:"uuid"`
	AccountID   string  `json:"account_id"`
	Username    string  `json:"username"`
	Nickname    string  `json:"nickname"`
	DisplayName string  `json:"display_name"`
	Links       bbLinks `json:"links"`
}

func (a *bbAccount) handle() string {
	if a == nil {
		return ""
	}
	return firstNonEmpty(a.Username, a.Nickname, a.DisplayName)
}

type bbEmail struct {
	Email       string `json:"email"`
	IsPrimary   bool   `json:"is_primary"`
	IsConfirmed bool   `json:"is_confirmed"`
}

// CurrentUser implements forge.Provider.
//
// Access tokens (repository/project/workspace) are not tied to a user and
// GET /user rejects them. For those the driver validates the token against
// the configured workspace (GET /repositories/{workspace}?pagelen=1) and
// returns a user that describes the token: ID "access-token:<workspace>",
// empty Username and Name "<workspace> access token".
func (p *Provider) CurrentUser(ctx context.Context) (*domain.User, error) {
	cred, err := p.credential(ctx)
	if err != nil {
		return nil, p.wrapOp("get current user", err)
	}
	u, _, err := p.identify(ctx, p.authMethodFor(cred), nil)
	return u, err
}

// identify resolves who a credential is. cred overrides the stored
// credential. It also returns the OAuth scopes reported by Bitbucket in the
// X-OAuth-Scopes response header, when present.
func (p *Provider) identify(ctx context.Context, method auth.Method, cred *auth.Credential) (*domain.User, []string, error) {
	if method == auth.MethodAccessToken {
		u, err := p.identifyAccessToken(ctx, cred, method)
		return u, nil, err
	}
	const op = "get current user"
	resp, err := p.send(ctx, request{op: op, method: http.MethodGet, url: "/user", cred: cred, credMethod: credMethod(cred, method)})
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	var acc bbAccount
	if err := httpx.DecodeJSON(resp, &acc); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, nil, fmt.Errorf("%s: %w", op, cerr)
		}
		return nil, nil, &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: op,
			Message: "unexpected response from Bitbucket", Cause: err}
	}
	var scopes []string
	if h := resp.Header.Get("X-OAuth-Scopes"); h != "" {
		scopes = splitScopes(h)
	}
	u := &domain.User{
		ID:        acc.UUID,
		Username:  acc.handle(),
		Name:      acc.DisplayName,
		WebURL:    acc.Links.HTML.Href,
		AvatarURL: acc.Links.Avatar.Href,
	}
	if u.ID == "" {
		u.ID = acc.AccountID
	}
	if acc.UUID != "" {
		p.mu.Lock()
		if cred == nil {
			p.userUUID = acc.UUID
		}
		p.mu.Unlock()
	}
	// The primary email needs the "email" scope; failures are ignored.
	var emails pageEnvelope[bbEmail]
	if err := p.do(ctx, request{op: "list emails", method: http.MethodGet, url: "/user/emails", cred: cred, credMethod: credMethod(cred, method)}, &emails); err == nil {
		for _, e := range emails.Values {
			if e.IsPrimary {
				u.Email = e.Email
				break
			}
		}
	}
	return u, scopes, nil
}

func (p *Provider) identifyAccessToken(ctx context.Context, cred *auth.Credential, method auth.Method) (*domain.User, error) {
	const op = "validate access token"
	if p.workspace == "" {
		return nil, p.needWorkspace(op, "Bitbucket access tokens are not tied to a user and can only be validated against a workspace")
	}
	var env pageEnvelope[bbRepository]
	err := p.do(ctx, request{
		op: op, method: http.MethodGet,
		url:   "/repositories/" + pathEscape(p.workspace),
		query: url.Values{"pagelen": {"1"}},
		cred:  cred, credMethod: credMethod(cred, method), notFoundMsg: fmt.Sprintf("workspace %q not found or not accessible with this access token", p.workspace),
	}, &env)
	if err != nil {
		return nil, err
	}
	return &domain.User{
		ID:     accessTokenUserPrefix + p.workspace,
		Name:   p.workspace + " access token",
		WebURL: p.webURL + "/" + p.workspace,
	}, nil
}

// credMethod returns the explicit auth method to use with an override
// credential (empty for the stored credential).
func credMethod(cred *auth.Credential, m auth.Method) auth.Method {
	if cred == nil {
		return ""
	}
	return m
}

// selectedUser returns the current user's UUID for /users/{selected_user}
// endpoints.
func (p *Provider) selectedUser(ctx context.Context, feature string) (string, error) {
	cred, err := p.credential(ctx)
	if err != nil {
		return "", err
	}
	if p.authMethodFor(cred) == auth.MethodAccessToken {
		return "", &errs.Error{Kind: errs.ErrUnsupportedCapability, Provider: p.acct.Name,
			Message: fmt.Sprintf("%s require a user credential; Bitbucket access tokens do not belong to a user", feature)}
	}
	p.mu.Lock()
	id := p.userUUID
	p.mu.Unlock()
	if id != "" {
		return id, nil
	}
	var acc bbAccount
	if err := p.do(ctx, request{op: "get current user", method: http.MethodGet, url: "/user"}, &acc); err != nil {
		return "", err
	}
	if acc.UUID == "" {
		return "", &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Message: "Bitbucket did not return a user UUID"}
	}
	p.mu.Lock()
	p.userUUID = acc.UUID
	p.mu.Unlock()
	return acc.UUID, nil
}

func (p *Provider) invalidArg(op, msg string) *errs.Error {
	return &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Op: op, Message: msg}
}

func (p *Provider) needWorkspace(op, why string) *errs.Error {
	return &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: p.acct.Name, Op: op,
		Message: why,
		Hint:    fmt.Sprintf("set `workspace: <slug>` in the configuration of account %q", p.acct.Name)}
}
