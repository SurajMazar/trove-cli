package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/redact"
)

// defaultOAuthScopes are requested by the device flow when none are given.
var defaultOAuthScopes = []string{"api", "read_user"}

// Login obtains and validates a credential. It never persists it.
//
//   - token: a personal, project or group access token (project and group
//     access tokens authenticate as a bot user, which GET /user returns).
//   - oauth: either a pre-issued OAuth access token (req.Token), or the OAuth
//     2.0 Device Authorization Grant (GitLab 17.2+, generally available in
//     17.9) using an OAuth application registered on the instance with the
//     "Device authorization grant" option enabled.
func (p *provider) Login(ctx context.Context, req auth.Request) (*auth.Result, error) {
	const op = "log in to GitLab"
	method := req.Method
	if method == "" {
		method = p.acct.AuthMethod
	}
	if method == "" {
		method = auth.MethodToken
	}
	var cred auth.Credential
	switch method {
	case auth.MethodToken:
		tok := strings.TrimSpace(req.Token)
		if tok == "" {
			e := p.invalid(op, "a GitLab personal, project or group access token is required")
			e.Hint = "create one under User Settings > Access tokens (scope: api) at " + p.webURL + "/-/user_settings/personal_access_tokens"
			return nil, e
		}
		cred = auth.Credential{Kind: auth.KindToken, Token: tok}
	case auth.MethodOAuth:
		clientID := req.ClientID
		if clientID == "" {
			clientID = p.acct.ClientID
		}
		if tok := strings.TrimSpace(req.Token); tok != "" {
			// A pre-issued OAuth access token cannot be refreshed by Trove.
			cred = auth.Credential{Kind: auth.KindOAuth, Token: tok, Username: clientID}
			break
		}
		var err error
		if cred, err = p.deviceLogin(ctx, req, clientID); err != nil {
			return nil, err
		}
	default:
		return nil, p.invalid(op, "GitLab does not support the %q authentication method (use token or oauth)", method)
	}
	u, err := p.fetchUser(ctx, &cred)
	if err != nil {
		return nil, err
	}
	return &auth.Result{Credential: cred, User: u.Username}, nil
}

func (p *provider) deviceLogin(ctx context.Context, req auth.Request, clientID string) (auth.Credential, error) {
	const op = "GitLab device authorization"
	if clientID == "" {
		return auth.Credential{}, &errs.Error{
			Kind: errs.ErrInvalidConfiguration, Provider: p.acct.Name, Op: op,
			Message: "an OAuth application ID (client_id) is required for the device flow",
			Hint: "register an OAuth application at " + p.webURL + "/-/user_settings/applications " +
				"(non-confidential, scopes api and read_user, \"Device authorization grant\" enabled) and set client_id on the account",
		}
	}
	if req.Prompter == nil {
		return auth.Credential{}, &errs.Error{
			Kind: errs.ErrInteractionRequired, Provider: p.acct.Name, Op: op,
			Message: "the device flow needs an interactive terminal to show the verification code",
			Hint:    "pass a token instead: trove auth login " + p.acct.Name + " --with-token",
		}
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = p.acct.Scopes
	}
	if len(scopes) == 0 {
		scopes = defaultOAuthScopes
	}
	flow := &auth.DeviceFlow{
		DeviceCodeURL: p.webURL + "/oauth/authorize_device",
		TokenURL:      p.webURL + "/oauth/token",
		ClientID:      clientID,
		ClientSecret:  req.ClientSecret,
		Scopes:        scopes,
		HTTPClient:    p.hc,
	}
	cred, err := flow.Run(ctx, req.Prompter)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return auth.Credential{}, ctxErr
		}
		e := &errs.Error{Kind: errs.ErrAuthenticationFailed, Provider: p.acct.Name, Op: op,
			Message: redact.String(err.Error()), Hint: p.loginHint()}
		switch {
		case auth.IsOAuthError(err, "invalid_client"), auth.IsOAuthError(err, "unauthorized_client"):
			e.Kind = errs.ErrInvalidConfiguration
			e.Hint = "check client_id and that the OAuth application has \"Device authorization grant\" enabled"
		case auth.IsOAuthError(err, "access_denied"):
			e.Message = "authorization was denied in the browser"
		case auth.IsOAuthError(err, "invalid_scope"):
			e.Kind = errs.ErrInvalidConfiguration
			e.Hint = "request only scopes the OAuth application allows (default: api read_user)"
		case !auth.IsOAuthError(err, ""):
			e.Hint = "the device authorization grant requires GitLab 17.2 or later; alternatively log in with an access token"
		}
		return auth.Credential{}, e
	}
	cred.Kind = auth.KindOAuth
	cred.Username = clientID
	cred.ClientSecret = req.ClientSecret
	if len(cred.Scopes) == 0 {
		cred.Scopes = scopes
	}
	return cred, nil
}

// tokenSelf is GET /personal_access_tokens/self (GitLab 15.5+).
type tokenSelf struct {
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expires_at"` // date, e.g. "2026-12-31"
	Active    bool     `json:"active"`
}

func (p *provider) AuthStatus(ctx context.Context) (*auth.Status, error) {
	cred, err := p.credential(ctx)
	if err != nil {
		return nil, withOp(err, "check GitLab authentication")
	}
	u, err := p.fetchUser(ctx, nil)
	if err != nil {
		return nil, err
	}
	st := &auth.Status{Authenticated: true, User: u.Username, Method: auth.MethodToken}
	var details []string
	if cred.Kind == auth.KindOAuth {
		st.Method = auth.MethodOAuth
		st.Scopes = cred.Scopes
		st.ExpiresAt = cred.Expiry
		details = append(details, "OAuth access token")
		if cred.Refreshable() {
			details = append(details, "refreshable")
		}
	} else {
		// Best effort: older instances (<15.5) lack the endpoint (404) and
		// some tokens may not introspect themselves (403).
		var self tokenSelf
		err := p.get(ctx, "get access token info", "/personal_access_tokens/self", nil, &self)
		switch {
		case err == nil:
			st.Scopes = self.Scopes
			if t, perr := time.Parse("2006-01-02", self.ExpiresAt); perr == nil {
				st.ExpiresAt = t
			}
			if self.Name != "" {
				details = append(details, "access token \""+self.Name+"\"")
			}
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return nil, err
		}
	}
	if u.Bot {
		details = append(details, "project or group access token (bot user)")
	}
	st.Detail = strings.Join(details, ", ")
	return st, nil
}

// RefreshCredential exchanges the OAuth refresh token and persists the result
// through Account.Credentials.
func (p *provider) RefreshCredential(ctx context.Context) (*auth.Status, error) {
	const op = "refresh GitLab OAuth token"
	if _, err := p.credential(ctx); err != nil {
		return nil, withOp(err, op)
	}
	p.credMu.Lock()
	if p.cred.Kind != auth.KindOAuth || !p.cred.Refreshable() {
		p.credMu.Unlock()
		return nil, p.invalid(op, "the stored credential is not a refreshable OAuth token (access tokens cannot be refreshed)")
	}
	_, err := p.refreshLocked(ctx)
	p.credMu.Unlock()
	if err != nil {
		return nil, err
	}
	return p.AuthStatus(ctx)
}

// RevokeCredential revokes the stored credential on the server:
//
//   - OAuth tokens via POST /oauth/revoke (RFC 7009). Doorkeeper stores the
//     refresh token on the same grant, so it is revoked too.
//   - Access tokens via DELETE /personal_access_tokens/self (GitLab 15.5+),
//     which revokes the token used to make the request.
func (p *provider) RevokeCredential(ctx context.Context) error {
	const op = "revoke GitLab credential"
	cred, err := p.credential(ctx)
	if err != nil {
		return withOp(err, op)
	}
	if cred.Kind == auth.KindOAuth {
		form := url.Values{"token": {cred.Token}}
		if id := p.oauthClientID(cred); id != "" {
			form.Set("client_id", id)
		}
		if cred.ClientSecret != "" {
			form.Set("client_secret", cred.ClientSecret)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.webURL+"/oauth/revoke", strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		resp, err := p.hc.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: op,
				Message: "request to GitLab failed", Cause: errors.New(redact.String(err.Error()))}
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return p.apiError(resp, op, nil)
		}
	} else {
		if _, err := p.do(ctx, call{method: http.MethodDelete, path: "/personal_access_tokens/self", op: op}, nil); err != nil {
			return err
		}
	}
	p.credMu.Lock()
	p.cred = nil
	p.credMu.Unlock()
	return nil
}

// GitCredentials returns HTTPS Git credentials. GitLab documents "oauth2" as
// the username for OAuth tokens and accepts any non-blank username with
// personal/project/group access tokens, so "oauth2" is used for both.
// Expired OAuth tokens are refreshed first.
func (p *provider) GitCredentials(ctx context.Context) (string, string, error) {
	cred, err := p.credential(ctx)
	if err != nil {
		return "", "", withOp(err, "get Git credentials")
	}
	return "oauth2", cred.Token, nil
}
