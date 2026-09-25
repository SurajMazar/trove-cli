package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
	"github.com/SurajMazar/trove-cli/internal/redact"
)

// Page sizes. Most Bitbucket collections accept pagelen up to 100; the pull
// request collections are capped at 50.
const (
	maxPageLen   = 100
	maxPRPageLen = 50
)

// request describes one API call.
type request struct {
	op     string // human readable operation for errors
	method string
	// url is either a path relative to the API base ("/user") or an absolute
	// URL returned by the API (pagination "next" links). Absolute URLs must
	// point at the configured API base.
	url   string
	query url.Values
	// body is JSON-encoded unless contentType is set, in which case it must
	// be an io.Reader or []byte.
	body        any
	contentType string
	accept      string
	// notFound is the error kind used for 404 (default errs.ErrNotFound).
	notFound error
	// notFoundMsg overrides the message for 404 responses.
	notFoundMsg string
	// cred overrides the stored credential (used by Login to validate a
	// credential that has not been persisted yet).
	cred *auth.Credential
	// credMethod is the auth method of cred (defaults to authMethodFor).
	credMethod auth.Method
}

// send performs r and returns the response for 2xx/3xx statuses. The caller
// closes the body. Non-success statuses are converted to *errs.Error.
func (p *Provider) send(ctx context.Context, r request) (*http.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", r.op, err)
	}
	u, err := p.resolve(r.url, r.query)
	if err != nil {
		return nil, p.wrapOp(r.op, err)
	}
	cred := r.cred
	if cred == nil {
		c, err := p.credential(ctx)
		if err != nil {
			return nil, p.wrapOp(r.op, err)
		}
		cred = &c
	}
	var body io.Reader
	switch b := r.body.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(b)
	case io.Reader:
		body = b
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return nil, fmt.Errorf("%s: encode request: %w", r.op, err)
		}
		body = bytes.NewReader(buf)
		if r.contentType == "" {
			r.contentType = "application/json"
		}
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.op, err)
	}
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	req.Header.Set("Accept", firstNonEmpty(r.accept, "application/json"))
	req.Header.Set("User-Agent", httpx.UserAgent())
	method := r.credMethod
	if method == "" {
		method = p.authMethodFor(*cred)
	}
	if err := p.authorize(req, *cred, method); err != nil {
		return nil, p.wrapOp(r.op, err)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("%s: %w", r.op, cerr)
		}
		return nil, &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: r.op,
			Message: "request to Bitbucket failed", Cause: errors.New(redact.String(err.Error()))}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, p.apiError(r, resp)
	}
	return resp, nil
}

// do performs r and decodes a JSON response into out (which may be nil).
func (p *Provider) do(ctx context.Context, r request, out any) error {
	resp, err := p.send(ctx, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := httpx.DecodeJSON(resp, out); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("%s: %w", r.op, cerr)
		}
		return &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: r.op,
			Message: "unexpected response from Bitbucket", Cause: err}
	}
	return nil
}

// resolve turns a relative path or absolute API URL into a request URL. An
// absolute URL (as found in pagination "next" links) is only accepted when it
// points at the configured API base, so credentials are never sent to
// another host even if a response is tampered with.
func (p *Provider) resolve(raw string, q url.Values) (string, error) {
	var u *url.URL
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", errs.New(errs.ErrProviderAPI, "Bitbucket returned an invalid link")
		}
		if !strings.EqualFold(parsed.Scheme, p.api.Scheme) || !strings.EqualFold(parsed.Host, p.api.Host) ||
			!strings.HasPrefix(parsed.EscapedPath(), strings.TrimRight(p.api.EscapedPath(), "/")+"/") {
			return "", errs.New(errs.ErrProviderAPI,
				"refusing to follow a Bitbucket link to %s: it is outside the configured API URL %s",
				parsed.Scheme+"://"+parsed.Host, p.apiURL)
		}
		u = parsed
	} else {
		parsed, err := url.Parse(p.apiURL + raw)
		if err != nil {
			return "", fmt.Errorf("invalid request path: %w", err)
		}
		u = parsed
	}
	if len(q) > 0 {
		merged := u.Query()
		for k, vs := range q {
			merged[k] = vs
		}
		u.RawQuery = merged.Encode()
	}
	return u.String(), nil
}

// checkRedirect strips credentials when a redirect leaves the API host. The
// pipeline step log endpoint, for example, answers 307 with a pre-signed
// storage URL that must not receive our Authorization header.
func (p *Provider) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !strings.EqualFold(req.URL.Host, p.api.Host) || !strings.EqualFold(req.URL.Scheme, p.api.Scheme) {
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
	}
	return nil
}

// authMethodFor returns the effective auth method for a credential. The
// credential kind decides; a bare token (e.g. pasted into a secret store by
// hand) follows the account's configured method.
func (p *Provider) authMethodFor(c auth.Credential) auth.Method {
	switch c.Kind {
	case auth.KindBasic:
		return auth.MethodBasic
	case auth.KindOAuth:
		return auth.MethodOAuth
	}
	switch p.acct.AuthMethod {
	case auth.MethodBasic, auth.MethodOAuth:
		return p.acct.AuthMethod
	}
	return auth.MethodAccessToken
}

func (p *Provider) authorize(req *http.Request, c auth.Credential, method auth.Method) error {
	if c.Token == "" {
		return p.notLoggedIn("stored Bitbucket credential is empty")
	}
	switch method {
	case auth.MethodBasic:
		// Atlassian API tokens: HTTP Basic with the Atlassian account email.
		user := firstNonEmpty(c.Username, p.acct.Username)
		if user == "" {
			return &errs.Error{Kind: errs.ErrInvalidConfiguration, Provider: p.acct.Name,
				Message: "Bitbucket API tokens require the Atlassian account email as username",
				Hint:    "trove auth login " + p.acct.Name}
		}
		req.SetBasicAuth(user, c.Token)
	default:
		// Access tokens and OAuth access tokens are Bearer tokens.
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return nil
}

func (p *Provider) notLoggedIn(msg string) *errs.Error {
	return &errs.Error{Kind: errs.ErrNotAuthenticated, Provider: p.acct.Name,
		Message: msg, Hint: "trove auth login " + p.acct.Name}
}

// credential returns the account credential, loading it lazily and
// refreshing expired OAuth tokens.
func (p *Provider) credential(ctx context.Context) (auth.Credential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cred == nil {
		if p.acct.Credentials == nil {
			return auth.Credential{}, p.notLoggedIn("Bitbucket account " + p.acct.Name + " is not logged in")
		}
		c, err := p.acct.Credentials.Load(ctx)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return auth.Credential{}, cerr
			}
			if errors.Is(err, errs.ErrNotAuthenticated) || errors.Is(err, errs.ErrSecretNotFound) {
				return auth.Credential{}, p.notLoggedIn("Bitbucket account " + p.acct.Name + " is not logged in")
			}
			return auth.Credential{}, errs.WithProvider(err, p.acct.Name)
		}
		p.cred = &c
	}
	if p.cred.Kind == auth.KindOAuth && p.cred.Expired() {
		c, err := p.refreshLocked(ctx, *p.cred)
		if err != nil {
			return auth.Credential{}, err
		}
		p.cred = &c
	}
	return *p.cred, nil
}

func (p *Provider) wrapOp(op string, err error) error {
	var e *errs.Error
	if errors.As(err, &e) {
		cp := *e
		if cp.Op == "" {
			cp.Op = op
		}
		if cp.Provider == "" {
			cp.Provider = p.acct.Name
		}
		return &cp
	}
	return fmt.Errorf("%s: %w", op, err)
}

// errorEnvelope is Bitbucket's error document:
// {"type": "error", "error": {"message": "...", "detail": ...}}.
type errorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
	} `json:"error"`
}

// apiError maps an HTTP error response to a typed error. Only the provider's
// message/detail strings are surfaced, never the raw body.
func (p *Provider) apiError(r request, resp *http.Response) error {
	msg := providerMessage(httpx.ReadBody(resp, 64<<10))
	e := &errs.Error{Provider: p.acct.Name, Op: r.op, Status: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Kind = errs.ErrAuthenticationFailed
		e.Message = "Bitbucket authentication failed: credential is invalid or expired"
		e.Hint = "trove auth login " + p.acct.Name
	case http.StatusForbidden:
		e.Kind = errs.ErrPermissionDenied
		e.Message = firstNonEmpty(msg, "Bitbucket denied access to this resource")
	case http.StatusNotFound:
		e.Kind = errs.ErrNotFound
		if r.notFound != nil && r.notFound != errs.ErrNotFound {
			e.Kind = r.notFound
			e.Cause = errs.ErrNotFound
		}
		e.Message = firstNonEmpty(r.notFoundMsg, msg, "not found")
	case http.StatusConflict:
		e.Kind = errs.ErrConflict
		e.Message = firstNonEmpty(msg, "conflict")
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		e.Kind = errs.ErrInvalidArgument
		e.Message = firstNonEmpty(msg, "Bitbucket rejected the request")
	case http.StatusTooManyRequests:
		e.Kind = errs.ErrRateLimited
		e.RetryAfter = httpx.RetryAfter(resp.Header)
		e.Message = "Bitbucket rate limit exceeded"
		if e.RetryAfter > 0 {
			e.Message += fmt.Sprintf("; retry after %s", e.RetryAfter)
		}
	default:
		e.Kind = errs.ErrProviderAPI
		e.Message = fmt.Sprintf("Bitbucket API returned HTTP %d", resp.StatusCode)
		if msg != "" {
			e.Message += ": " + msg
		}
	}
	return e
}

func providerMessage(body []byte) string {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	msg := strings.TrimSpace(env.Error.Message)
	var detail string
	if len(env.Error.Detail) > 0 && env.Error.Detail[0] == '"' {
		_ = json.Unmarshal(env.Error.Detail, &detail)
	}
	detail = strings.TrimSpace(detail)
	if detail != "" && detail != msg {
		if msg == "" {
			msg = detail
		} else {
			msg += ": " + detail
		}
	}
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return redact.String(msg)
}

// pageEnvelope is Bitbucket's paged collection document.
type pageEnvelope[T any] struct {
	Values  []T    `json:"values"`
	Next    string `json:"next"`
	Size    *int   `json:"size"`
	Page    int    `json:"page"`
	Pagelen int    `json:"pagelen"`
}

func getPage[T any](ctx context.Context, p *Provider, r request) (pageEnvelope[T], error) {
	var env pageEnvelope[T]
	r.method = http.MethodGet
	err := p.do(ctx, r, &env)
	return env, err
}

// collectPages follows "next" links across one or more collections (for
// example the same listing in several workspaces) and normalizes them with
// forge.Collect. firsts are the first-page requests of each collection and
// share op/notFound settings with firsts[0]; empty collections are skipped
// transparently. filter, when set, drops items client-side.
func collectPages[T any](ctx context.Context, p *Provider, limit int, firsts []request, filter func(T) bool) ([]T, error) {
	if len(firsts) == 0 {
		return nil, nil
	}
	urls := make([]string, len(firsts))
	for i, r := range firsts {
		u, err := p.resolve(r.url, r.query)
		if err != nil {
			return nil, p.wrapOp(r.op, err)
		}
		urls[i] = u
	}
	tmpl := firsts[0]
	idx := 0 // next collection whose first page has not been requested
	fetch := func(ctx context.Context, cursor string) ([]T, string, error) {
		u := cursor
		if u == "" {
			u, idx = urls[0], 1
		}
		for {
			r := tmpl
			r.url, r.query = u, nil
			env, err := getPage[T](ctx, p, r)
			if err != nil {
				return nil, "", err
			}
			items := env.Values
			if filter != nil {
				kept := make([]T, 0, len(items))
				for _, it := range items {
					if filter(it) {
						kept = append(kept, it)
					}
				}
				items = kept
			}
			next := env.Next
			if len(env.Values) == 0 {
				next = ""
			}
			if next == "" && idx < len(urls) {
				next = urls[idx]
				idx++
			}
			if len(items) > 0 || next == "" {
				return items, next, nil
			}
			u = next
		}
	}
	return forge.Collect(ctx, limit, fetch)
}

// pathEscape escapes one URL path segment. Bitbucket UUIDs are written with
// braces ("{...}") which must be percent-encoded.
func pathEscape(s string) string { return url.PathEscape(s) }

// pathEscapeAll escapes each "/"-separated segment of a path.
func pathEscapeAll(s string) string {
	parts := strings.Split(s, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// bbqlString quotes s as a BBQL string literal.
func bbqlString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
