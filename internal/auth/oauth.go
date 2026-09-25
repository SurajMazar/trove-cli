package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DeviceFlow implements the OAuth 2.0 Device Authorization Grant (RFC 8628).
// Endpoints are provider specific and supplied by each driver; the protocol
// itself is standardized, which is why it lives here.
type DeviceFlow struct {
	DeviceCodeURL string
	TokenURL      string
	ClientID      string
	ClientSecret  string // optional; most public device-flow clients have none
	Scopes        []string
	// ScopeSeparator defaults to a single space (RFC 6749).
	ScopeSeparator string
	HTTPClient     *http.Client
	// now/sleep are overridable in tests.
	sleep func(ctx context.Context, d time.Duration) error
}

// DeviceCodeResponse is the device authorization response.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// TokenResponse is an OAuth token endpoint response.
type TokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Credential converts the response into a Credential.
func (t TokenResponse) Credential() Credential {
	c := Credential{Kind: KindOAuth, Token: t.AccessToken, RefreshToken: t.RefreshToken, TokenType: t.TokenType}
	if t.ExpiresIn > 0 {
		c.Expiry = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	if t.Scope != "" {
		c.Scopes = strings.FieldsFunc(t.Scope, func(r rune) bool { return r == ' ' || r == ',' })
	}
	return c
}

// OAuthError is a token endpoint error. It never contains secrets.
type OAuthError struct {
	Code        string
	Description string
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("oauth error %s: %s", e.Code, e.Description)
	}
	return "oauth error " + e.Code
}

func (f *DeviceFlow) client() *http.Client {
	if f.HTTPClient != nil {
		return f.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Run performs the complete device flow: request a code, show it to the user,
// then poll until the user authorizes, denies, or the code expires.
func (f *DeviceFlow) Run(ctx context.Context, p Prompter) (Credential, error) {
	dc, err := f.RequestCode(ctx)
	if err != nil {
		return Credential{}, err
	}
	if p != nil {
		p.DeviceCode(dc.VerificationURI, dc.UserCode, time.Duration(dc.ExpiresIn)*time.Second)
	}
	return f.Poll(ctx, dc)
}

// RequestCode requests a device and user code.
func (f *DeviceFlow) RequestCode(ctx context.Context) (*DeviceCodeResponse, error) {
	form := url.Values{"client_id": {f.ClientID}}
	if len(f.Scopes) > 0 {
		sep := f.ScopeSeparator
		if sep == "" {
			sep = " "
		}
		form.Set("scope", strings.Join(f.Scopes, sep))
	}
	var out DeviceCodeResponse
	var oerr TokenResponse
	if err := f.post(ctx, f.DeviceCodeURL, form, &out, &oerr); err != nil {
		return nil, err
	}
	if oerr.Error != "" {
		return nil, &OAuthError{Code: oerr.Error, Description: oerr.ErrorDescription}
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, fmt.Errorf("device authorization response is missing device_code or user_code")
	}
	if out.Interval <= 0 {
		out.Interval = 5
	}
	if out.ExpiresIn <= 0 {
		out.ExpiresIn = 900
	}
	return &out, nil
}

// Poll polls the token endpoint per RFC 8628 §3.4/3.5.
func (f *DeviceFlow) Poll(ctx context.Context, dc *DeviceCodeResponse) (Credential, error) {
	interval := time.Duration(dc.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	sleep := f.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	for {
		if err := sleep(ctx, interval); err != nil {
			return Credential{}, err
		}
		if time.Now().After(deadline) {
			return Credential{}, &OAuthError{Code: "expired_token", Description: "the device code expired before authorization completed"}
		}
		form := url.Values{
			"client_id":   {f.ClientID},
			"device_code": {dc.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}
		if f.ClientSecret != "" {
			form.Set("client_secret", f.ClientSecret)
		}
		var tok TokenResponse
		if err := f.post(ctx, f.TokenURL, form, &tok, &tok); err != nil {
			return Credential{}, err
		}
		switch tok.Error {
		case "":
			if tok.AccessToken == "" {
				return Credential{}, fmt.Errorf("token response did not include an access token")
			}
			return tok.Credential(), nil
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		default:
			return Credential{}, &OAuthError{Code: tok.Error, Description: tok.ErrorDescription}
		}
	}
}

// RefreshToken exchanges a refresh token for a new access token using the
// standard refresh_token grant. If basicAuth is true, client credentials are
// sent via HTTP Basic auth (required by e.g. Bitbucket) instead of the form.
func RefreshToken(ctx context.Context, hc *http.Client, tokenURL, clientID, clientSecret, refreshToken string, basicAuth bool) (Credential, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
	if !basicAuth {
		form.Set("client_id", clientID)
		if clientSecret != "" {
			form.Set("client_secret", clientSecret)
		}
	}
	f := &DeviceFlow{HTTPClient: hc}
	var tok TokenResponse
	req := func(r *http.Request) {
		if basicAuth {
			r.SetBasicAuth(clientID, clientSecret)
		}
	}
	if err := f.postWith(ctx, tokenURL, form, &tok, &tok, req); err != nil {
		return Credential{}, err
	}
	if tok.Error != "" {
		return Credential{}, &OAuthError{Code: tok.Error, Description: tok.ErrorDescription}
	}
	if tok.AccessToken == "" {
		return Credential{}, fmt.Errorf("refresh response did not include an access token")
	}
	c := tok.Credential()
	if c.RefreshToken == "" {
		c.RefreshToken = refreshToken
	}
	return c, nil
}

func (f *DeviceFlow) post(ctx context.Context, u string, form url.Values, okOut, errOut any) error {
	return f.postWith(ctx, u, form, okOut, errOut, nil)
}

func (f *DeviceFlow) postWith(ctx context.Context, u string, form url.Values, okOut, errOut any, mutate func(*http.Request)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if mutate != nil {
		mutate(req)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("oauth request to %s failed: %w", redactURL(u), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if err := json.Unmarshal(body, okOut); err != nil {
			return fmt.Errorf("decode oauth response: %w", err)
		}
		// Some servers (GitHub) report OAuth errors with HTTP 200.
		if errOut != okOut {
			_ = json.Unmarshal(body, errOut)
		}
		return nil
	}
	// RFC 6749 errors come back as 400 with a JSON body.
	if err := json.Unmarshal(body, errOut); err == nil {
		if tr, ok := errOut.(*TokenResponse); ok && tr.Error != "" {
			return nil
		}
	}
	return fmt.Errorf("oauth endpoint %s returned HTTP %d", redactURL(u), resp.StatusCode)
}

func redactURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "(invalid url)"
	}
	p.RawQuery = ""
	p.User = nil
	return p.String()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// IsOAuthError reports whether err is an OAuth protocol error with the given
// code (or any code when code is empty).
func IsOAuthError(err error, code string) bool {
	var oe *OAuthError
	if !errors.As(err, &oe) {
		return false
	}
	return code == "" || oe.Code == code
}
