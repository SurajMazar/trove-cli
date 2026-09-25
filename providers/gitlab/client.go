package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
	"github.com/SurajMazar/trove-cli/internal/redact"
)

// --- credentials -----------------------------------------------------------

func (p *provider) loginHint() string { return "trove auth login " + p.acct.Name }

func (p *provider) notLoggedIn(cause error) error {
	return &errs.Error{
		Kind:     errs.ErrNotAuthenticated,
		Provider: p.acct.Name,
		Message:  fmt.Sprintf("GitLab account %q is not logged in", p.acct.Name),
		Hint:     p.loginHint(),
		Cause:    cause,
	}
}

// credential returns the account credential, loading it on first use and
// transparently refreshing an expired OAuth access token (GitLab OAuth
// tokens live for two hours) when a refresh token is available.
func (p *provider) credential(ctx context.Context) (auth.Credential, error) {
	if err := ctx.Err(); err != nil {
		return auth.Credential{}, err
	}
	p.credMu.Lock()
	defer p.credMu.Unlock()
	if p.cred == nil {
		if p.acct.Credentials == nil {
			return auth.Credential{}, p.notLoggedIn(nil)
		}
		c, err := p.acct.Credentials.Load(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return auth.Credential{}, ctxErr
			}
			if errors.Is(err, errs.ErrNotAuthenticated) || errors.Is(err, errs.ErrSecretNotFound) {
				return auth.Credential{}, p.notLoggedIn(nil)
			}
			return auth.Credential{}, errs.WithProvider(err, p.acct.Name)
		}
		if strings.TrimSpace(c.Token) == "" {
			return auth.Credential{}, p.notLoggedIn(nil)
		}
		p.cred = &c
	}
	if p.cred.Kind == auth.KindOAuth && p.cred.Expired() && p.cred.Refreshable() {
		if _, err := p.refreshLocked(ctx); err != nil {
			return auth.Credential{}, err
		}
	}
	return *p.cred, nil
}

// oauthClientID returns the OAuth application ID used for a credential. The
// ID is stored with the credential (Credential.Username) at login so that a
// refresh keeps working even if the account config changes.
func (p *provider) oauthClientID(c auth.Credential) string {
	if c.Username != "" {
		return c.Username
	}
	return p.acct.ClientID
}

// refreshLocked exchanges the refresh token and persists the new credential.
// p.credMu must be held and p.cred must be set.
func (p *provider) refreshLocked(ctx context.Context) (auth.Credential, error) {
	old := *p.cred
	clientID := p.oauthClientID(old)
	if clientID == "" {
		return auth.Credential{}, &errs.Error{
			Kind: errs.ErrAuthenticationFailed, Provider: p.acct.Name, Op: "refresh GitLab OAuth token",
			Message: "the OAuth token expired and no OAuth application ID is known to refresh it",
			Hint:    p.loginHint(),
		}
	}
	nc, err := auth.RefreshToken(ctx, p.hc, p.webURL+"/oauth/token", clientID, old.ClientSecret, old.RefreshToken, false)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return auth.Credential{}, ctxErr
		}
		return auth.Credential{}, &errs.Error{
			Kind: errs.ErrAuthenticationFailed, Provider: p.acct.Name, Op: "refresh GitLab OAuth token",
			Message: "the OAuth token could not be refreshed: " + redact.String(err.Error()),
			Hint:    p.loginHint(),
		}
	}
	nc.Kind = auth.KindOAuth
	nc.Username = clientID
	nc.ClientSecret = old.ClientSecret
	if len(nc.Scopes) == 0 {
		nc.Scopes = old.Scopes
	}
	if p.acct.Credentials != nil {
		if err := p.acct.Credentials.Save(ctx, nc); err != nil {
			return auth.Credential{}, errs.Wrap(errs.ErrProviderAPI, err, "refreshed GitLab OAuth token could not be saved")
		}
	}
	p.cred = &nc
	return nc, nil
}

// refreshAfter401 refreshes the stored OAuth credential after the server
// rejected it. If another request already refreshed it, the newer
// credential is returned without a second exchange.
func (p *provider) refreshAfter401(ctx context.Context, used auth.Credential) (auth.Credential, error) {
	p.credMu.Lock()
	defer p.credMu.Unlock()
	if p.cred == nil {
		return auth.Credential{}, p.notLoggedIn(nil)
	}
	if p.cred.Token != used.Token {
		return *p.cred, nil
	}
	return p.refreshLocked(ctx)
}

// authorize sets GitLab's auth header. Personal, project and group access
// tokens use PRIVATE-TOKEN; OAuth access tokens must be sent as a Bearer
// token (GitLab rejects OAuth tokens in PRIVATE-TOKEN).
func authorize(req *http.Request, c auth.Credential) {
	if c.Kind == auth.KindOAuth {
		req.Header.Set("Authorization", "Bearer "+c.Token)
		return
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
}

// --- requests --------------------------------------------------------------

// call describes one API request. path is relative to the API base and must
// already be escaped (see projectPath); rawURL, when set, is an absolute URL
// (a pagination "next" link) used instead of path+query.
type call struct {
	method   string
	path     string
	query    url.Values
	rawURL   string
	body     any
	op       string
	notFound error // error kind for 404 (defaults to errs.ErrNotFound)
	accept   string
	// cred overrides the stored credential (used while logging in).
	cred *auth.Credential
}

func (p *provider) endpoint(path string, q url.Values) string {
	u := p.apiURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// send performs c and returns the successful response; the caller must close
// its body. Non-2xx responses are mapped to *errs.Error.
func (p *provider) send(ctx context.Context, c call) (*http.Response, error) {
	var cred auth.Credential
	if c.cred != nil {
		cred = *c.cred
	} else {
		var err error
		if cred, err = p.credential(ctx); err != nil {
			return nil, withOp(err, c.op)
		}
	}
	target := c.rawURL
	if target == "" {
		target = p.endpoint(c.path, c.query)
	}
	var payload []byte
	if c.body != nil {
		var err error
		if payload, err = json.Marshal(c.body); err != nil {
			return nil, fmt.Errorf("%s: encode request: %w", c.op, err)
		}
	}
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		// NewRequest parses target with url.Parse, which keeps the escaped
		// form in URL.RawPath, so "%2F" in project paths reaches GitLab
		// intact instead of being decoded into path separators.
		req, err := http.NewRequestWithContext(ctx, c.method, target, body)
		if err != nil {
			return nil, fmt.Errorf("%s: build request: %w", c.op, err)
		}
		accept := c.accept
		if accept == "" {
			accept = "application/json"
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("User-Agent", httpx.UserAgent())
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		authorize(req, cred)

		resp, err := p.hc.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("%s: %w", c.op, ctxErr)
			}
			return nil, &errs.Error{
				Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: c.op,
				Message: "request to GitLab failed",
				Cause:   errors.New(redact.String(err.Error())),
			}
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.cred == nil &&
			cred.Kind == auth.KindOAuth && cred.Refreshable() {
			drain(resp)
			if cred, err = p.refreshAfter401(ctx, cred); err != nil {
				return nil, withOp(err, c.op)
			}
			continue
		}
		if resp.StatusCode >= 400 {
			defer resp.Body.Close()
			return nil, p.apiError(resp, c.op, c.notFound)
		}
		return resp, nil
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// do performs c and decodes a JSON response into out (which may be nil).
// It returns the response headers for pagination and totals.
func (p *provider) do(ctx context.Context, c call, out any) (http.Header, error) {
	resp, err := p.send(ctx, c)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return resp.Header, nil
	}
	if err := httpx.DecodeJSON(resp, out); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("%s: %w", c.op, ctxErr)
		}
		return nil, &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: c.op,
			Message: "GitLab returned an unexpected response", Cause: err}
	}
	return resp.Header, nil
}

func (p *provider) get(ctx context.Context, op, path string, q url.Values, out any) error {
	_, err := p.do(ctx, call{method: http.MethodGet, path: path, query: q, op: op}, out)
	return err
}

func withOp(err error, op string) error {
	var e *errs.Error
	if errors.As(err, &e) && e.Op == "" {
		cp := *e
		cp.Op = op
		return &cp
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", op, err)
	}
	return err
}

// --- pagination --------------------------------------------------------------

// list collects a paginated collection. GitLab uses offset pagination and
// advertises the next page both in the Link header (rel="next") and in
// X-Next-Page; some proxies strip Link, so X-Next-Page is the fallback.
// X-Total may be absent for very large collections and is not relied on.
func list[T any](ctx context.Context, p *provider, op, path string, q url.Values, limit int, notFound error) ([]T, error) {
	items, _, err := pages[T](ctx, p, op, path, q, limit, notFound)
	return items, err
}

// pages is list that also returns the X-Total of the first page, or nil
// when GitLab did not report it.
func pages[T any](ctx context.Context, p *provider, op, path string, q url.Values, limit int, notFound error) ([]T, *int, error) {
	var tot *int
	first := true
	items, err := forge.Collect(ctx, limit, func(ctx context.Context, cursor string) ([]T, string, error) {
		var page []T
		c := call{method: http.MethodGet, op: op, notFound: notFound}
		if cursor != "" {
			c.rawURL = cursor
		} else {
			qq := cloneValues(q)
			qq.Set("per_page", strconv.Itoa(forge.PageSize(limit, maxPerPage)))
			c.path, c.query = path, qq
		}
		h, err := p.do(ctx, c, &page)
		if err != nil {
			return nil, "", err
		}
		if first {
			tot, first = total(h), false
		}
		current := c.rawURL
		if current == "" {
			current = p.endpoint(c.path, c.query)
		}
		return page, p.nextPage(h, current), nil
	})
	return items, tot, err
}

// nextPage returns the URL of the next page, or "".
func (p *provider) nextPage(h http.Header, current string) string {
	if next := httpx.ParseLinkHeader(h.Get("Link"))["next"]; next != "" && p.sameOrigin(next) {
		return next
	}
	n := strings.TrimSpace(h.Get("X-Next-Page"))
	if n == "" {
		return ""
	}
	if _, err := strconv.Atoi(n); err != nil {
		return ""
	}
	u, err := url.Parse(current)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("page", n)
	u.RawQuery = q.Encode()
	return u.String()
}

// sameOrigin guards against following a Link header to another host, which
// would send the token there.
func (p *provider) sameOrigin(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	base, err := url.Parse(p.apiURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, base.Scheme) && strings.EqualFold(u.Host, base.Host)
}

// total returns the X-Total header, or nil when GitLab omitted it (it does
// so for collections above 10,000 items).
func total(h http.Header) *int {
	v := strings.TrimSpace(h.Get("X-Total"))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

// --- errors ------------------------------------------------------------------

// apiErrorBody covers GitLab's error shapes:
//
//	{"message": "404 Project Not Found"}
//	{"message": ["name has already been taken"]}
//	{"message": {"name": ["has already been taken"], "path": ["is invalid"]}}
//	{"error": "insufficient_scope", "error_description": "...", "scope": "api"}
//	{"error": "scope does not have a valid value"}
type apiErrorBody struct {
	Message          json.RawMessage `json:"message"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
	Scope            string          `json:"scope"`
}

// errorMessage extracts a concise, credential-free message from a body.
func errorMessage(body []byte) (msg string, parsed apiErrorBody) {
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", apiErrorBody{}
	}
	var parts []string
	if len(parsed.Message) > 0 {
		var v any
		if json.Unmarshal(parsed.Message, &v) == nil {
			if s := flattenMessage("", v); s != "" {
				parts = append(parts, s)
			}
		}
	}
	switch {
	case parsed.ErrorDescription != "":
		parts = append(parts, parsed.ErrorDescription)
	case parsed.Error != "":
		parts = append(parts, parsed.Error)
	}
	msg = strings.Join(parts, "; ")
	msg = redact.String(msg)
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	return msg, parsed
}

// flattenMessage renders string / array / field-map messages.
func flattenMessage(field string, v any) string {
	prefix := ""
	if field != "" {
		prefix = field + " "
	}
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return prefix + t
	case []any:
		var items []string
		for _, it := range t {
			if s := flattenMessage("", it); s != "" {
				items = append(items, s)
			}
		}
		if len(items) == 0 {
			return ""
		}
		if field != "" {
			return prefix + strings.Join(items, ", ")
		}
		return strings.Join(items, "; ")
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var items []string
		for _, k := range keys {
			name := k
			if field != "" {
				name = field + "." + k
			}
			if s := flattenMessage(name, t[k]); s != "" {
				items = append(items, s)
			}
		}
		return strings.Join(items, "; ")
	default:
		return prefix + fmt.Sprint(t)
	}
}

// apiError maps a non-2xx response to a typed error.
func (p *provider) apiError(resp *http.Response, op string, notFound error) error {
	msg, parsed := errorMessage(httpx.ReadBody(resp, 64<<10))
	e := &errs.Error{Provider: p.acct.Name, Op: op, Status: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Kind = errs.ErrAuthenticationFailed
		e.Message = "GitLab authentication failed: token is invalid or expired"
		e.Hint = p.loginHint()
	case http.StatusForbidden:
		e.Kind = errs.ErrPermissionDenied
		e.Message = "GitLab denied access"
		if msg != "" {
			e.Message += ": " + msg
		}
		if parsed.Error == "insufficient_scope" {
			scope := parsed.Scope
			if scope == "" {
				scope = "api"
			}
			e.Hint = fmt.Sprintf("the token lacks the required scope (%s); create a token with that scope and run %s", redact.String(scope), p.loginHint())
		}
	case http.StatusNotFound:
		e.Kind = errs.ErrNotFound
		if notFound != nil && notFound != errs.ErrNotFound {
			e.Kind = notFound
			e.Cause = errs.ErrNotFound
		}
		e.Message = msg
		if e.Message == "" {
			e.Message = "not found on GitLab"
		}
	case http.StatusConflict:
		e.Kind = errs.ErrConflict
		e.Message = nonEmpty(msg, "GitLab reported a conflict")
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		e.Kind = errs.ErrInvalidArgument
		e.Message = nonEmpty(msg, "GitLab rejected the request")
	case http.StatusTooManyRequests:
		e.Kind = errs.ErrRateLimited
		e.RetryAfter = retryAfter(resp.Header)
		e.Message = "GitLab rate limit exceeded"
		if e.RetryAfter > 0 {
			e.Message += fmt.Sprintf("; retry after %s", e.RetryAfter.Round(time.Second))
		}
	default:
		e.Kind = errs.ErrProviderAPI
		e.Message = fmt.Sprintf("GitLab API error (HTTP %d)", resp.StatusCode)
		if msg != "" {
			e.Message += ": " + msg
		}
	}
	return e
}

// retryAfter prefers Retry-After and falls back to RateLimit-Reset (a Unix
// timestamp) which GitLab sends alongside throttled responses.
func retryAfter(h http.Header) time.Duration {
	if d := httpx.RetryAfter(h); d > 0 {
		return d
	}
	if v := strings.TrimSpace(h.Get("RateLimit-Reset")); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			if d := time.Until(time.Unix(secs, 0)); d > 0 {
				return d
			}
		}
	}
	return 0
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

func (p *provider) invalid(op, format string, args ...any) *errs.Error {
	e := errs.New(errs.ErrInvalidArgument, format, args...)
	e.Op = op
	e.Provider = p.acct.Name
	return e
}

func (p *provider) notFound(op, format string, args ...any) *errs.Error {
	e := errs.New(errs.ErrNotFound, format, args...)
	e.Op = op
	e.Provider = p.acct.Name
	return e
}
