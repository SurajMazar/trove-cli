package custom

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
	"github.com/SurajMazar/trove-cli/internal/redact"
)

// --- credentials ---------------------------------------------------------------

// resolvedCredential is a credential turned into the header value TFP
// expects. secret is kept only to scrub server messages that echo it.
type resolvedCredential struct {
	header   string
	secret   string
	username string
	method   auth.Method
}

// credential loads and caches the account credential on first use.
func (p *Provider) credential(ctx context.Context) (*resolvedCredential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cred != nil {
		return p.cred, nil
	}
	if p.acct.Credentials == nil {
		return nil, p.notLoggedIn()
	}
	c, err := p.acct.Credentials.Load(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("load credential: %w", ctxErr)
		}
		if errors.Is(err, errs.ErrNotAuthenticated) || errors.Is(err, errs.ErrSecretNotFound) {
			return nil, p.notLoggedIn()
		}
		return nil, errs.WithProvider(err, p.acct.Name)
	}
	if c.Token == "" {
		return nil, p.notLoggedIn()
	}
	rc, err := p.resolve(c.Token, c.Username)
	if err != nil {
		return nil, err
	}
	p.cred = rc
	return rc, nil
}

// resolve builds the auth header value for token according to the
// configured scheme.
func (p *Provider) resolve(token, username string) (*resolvedCredential, error) {
	if strings.ContainsAny(token, "\r\n") {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: "the stored token contains invalid characters"}
	}
	rc := &resolvedCredential{secret: token, method: auth.MethodToken}
	switch {
	case p.s.basic:
		user := firstNonEmpty(username, p.s.username)
		if user == "" {
			return nil, &errs.Error{
				Kind: errs.ErrInvalidConfiguration, Provider: p.acct.Name,
				Message: "auth scheme \"basic\" needs a username",
				Hint:    "set custom.auth.username or log in with a username",
			}
		}
		if strings.ContainsAny(user, ":\r\n") {
			return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: "basic auth username must not contain ':' or line breaks"}
		}
		rc.username = user
		rc.method = auth.MethodBasic
		rc.header = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token))
	case p.s.authScheme == "":
		rc.header = token
	default:
		rc.header = p.s.authScheme + " " + token
	}
	return rc, nil
}

func (p *Provider) notLoggedIn() error {
	return &errs.Error{
		Kind:     errs.ErrNotAuthenticated,
		Provider: p.acct.Name,
		Message:  fmt.Sprintf("%s account %q is not logged in", p.s.displayName, p.acct.Name),
		Hint:     "trove auth login " + p.acct.Name,
	}
}

// --- transport -----------------------------------------------------------------

type authKey struct{}

type authValue struct {
	header, value string
}

// authTransport attaches the credential header, but only to requests for the
// api_base_url origin. It runs beneath the logging transport so the header
// is never logged, and it is consulted again for every redirect hop, so a
// redirect to another host never receives the credential.
type authTransport struct {
	base   http.RoundTripper
	origin string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if v, ok := req.Context().Value(authKey{}).(authValue); ok && v.value != "" && originOf(req.URL) == t.origin {
		req = req.Clone(req.Context())
		req.Header.Set(v.header, v.value)
	}
	return base.RoundTrip(req)
}

// originOf returns scheme://host[:port] with default ports removed.
func originOf(u *url.URL) string {
	if u == nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// rateLimited recognizes TFP's secondary rate-limit signal: 403 with a
// Retry-After header (some gateways cannot emit 429). 429 is handled by
// httpx itself.
func rateLimited(resp *http.Response) (bool, time.Duration) {
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("Retry-After") != "" {
		return true, httpx.RetryAfter(resp.Header)
	}
	return false, 0
}

// --- requests ------------------------------------------------------------------

// endpoint returns an absolute URL for an already-escaped path.
func (p *Provider) endpoint(path string, q url.Values) string {
	u := p.s.apiBaseStr + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// repoPath returns /v1/repositories/{repo} where {repo} is the path-escaped
// full name, so "a/b/c" becomes "a%2Fb%2Fc" and stays one path segment.
func (p *Provider) repoPath(ref domain.RepositoryRef) (string, error) {
	if strings.TrimSpace(ref.Namespace) == "" || strings.TrimSpace(ref.Name) == "" {
		return "", &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name,
			Message: fmt.Sprintf("repository reference %q needs a namespace and a name", ref.FullName())}
	}
	return "/v1/repositories/" + url.PathEscape(ref.Namespace+"/"+ref.Name), nil
}

type call struct {
	method string
	url    string
	body   any
	out    any
	op     string
	// notFound is the error kind used for 404/not_found (default ErrNotFound).
	notFound error
	// cred overrides the stored credential (used by Login).
	cred *resolvedCredential
}

// send performs one TFP request and decodes a 2xx JSON response into c.out.
func (p *Provider) send(ctx context.Context, c call) (http.Header, error) {
	rc := c.cred
	if rc == nil {
		var err error
		if rc, err = p.credential(ctx); err != nil {
			return nil, err
		}
	}
	var body io.Reader
	if c.body != nil {
		b, err := json.Marshal(c.body)
		if err != nil {
			return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Op: c.op, Message: "could not encode request", Cause: err}
		}
		body = bytes.NewReader(b)
	}
	rctx := context.WithValue(ctx, authKey{}, authValue{header: p.s.authHeader, value: rc.header})
	req, err := http.NewRequestWithContext(rctx, c.method, c.url, body)
	if err != nil {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Op: c.op, Message: "could not build request", Cause: errors.New(redact.String(err.Error()))}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", httpx.UserAgent())
	if c.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.Canceled) {
				return nil, &errs.Error{Kind: errs.ErrCanceled, Provider: p.acct.Name, Op: c.op, Cause: ctxErr}
			}
			return nil, &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: c.op, Message: "request timed out", Cause: ctxErr}
		}
		return nil, &errs.Error{
			Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: c.op,
			Message: fmt.Sprintf("request to %s failed", p.s.displayName),
			Cause:   errors.New(scrub(err.Error(), rc.secret)),
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Header, p.apiError(resp, c.op, c.notFound, rc.secret)
	}
	if c.out != nil && resp.StatusCode != http.StatusNoContent {
		if err := httpx.DecodeJSON(resp, c.out); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, &errs.Error{Kind: errs.ErrCanceled, Provider: p.acct.Name, Op: c.op, Cause: ctxErr}
			}
			return nil, &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: c.op, Status: resp.StatusCode,
				Message: fmt.Sprintf("%s returned a response that is not valid TFP v1 JSON", p.s.displayName), Cause: err}
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	}
	return resp.Header, nil
}

// errorEnvelope is the TFP error body: {"error": {"code": "...", "message": "..."}}.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// apiError maps a non-2xx response to an *errs.Error. The envelope code is
// authoritative; the HTTP status is used when the code is missing or unknown.
func (p *Provider) apiError(resp *http.Response, op string, notFound error, secret string) error {
	var env errorEnvelope
	_ = json.Unmarshal(httpx.ReadBody(resp, 64<<10), &env)
	msg := scrub(env.Error.Message, secret)
	e := &errs.Error{Provider: p.acct.Name, Op: op, Status: resp.StatusCode}

	kind := ""
	switch code := strings.ToLower(strings.TrimSpace(env.Error.Code)); code {
	case "not_found", "unauthorized", "forbidden", "conflict", "invalid", "rate_limited":
		kind = code
	default:
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			kind = "unauthorized"
		case http.StatusForbidden:
			kind = "forbidden"
			if resp.Header.Get("Retry-After") != "" {
				kind = "rate_limited"
			}
		case http.StatusNotFound:
			kind = "not_found"
		case http.StatusConflict:
			kind = "conflict"
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			kind = "invalid"
		case http.StatusTooManyRequests:
			kind = "rate_limited"
		}
	}

	switch kind {
	case "unauthorized":
		e.Kind = errs.ErrAuthenticationFailed
		e.Message = fmt.Sprintf("%s authentication failed: token is invalid or expired", p.s.displayName)
		e.Hint = "trove auth login " + p.acct.Name
	case "forbidden":
		e.Kind = errs.ErrPermissionDenied
		e.Message = firstNonEmpty(msg, "permission denied")
	case "not_found":
		if notFound == nil {
			notFound = errs.ErrNotFound
		}
		e.Kind = notFound
		if notFound != errs.ErrNotFound {
			// ErrRepositoryNotFound must also satisfy errors.Is(err, ErrNotFound).
			e.Cause = errs.ErrNotFound
		}
		e.Message = firstNonEmpty(msg, notFound.Error())
	case "conflict":
		e.Kind = errs.ErrConflict
		e.Message = firstNonEmpty(msg, "conflict")
	case "invalid":
		e.Kind = errs.ErrInvalidArgument
		e.Message = firstNonEmpty(msg, "request rejected as invalid")
	case "rate_limited":
		e.Kind = errs.ErrRateLimited
		e.RetryAfter = httpx.RetryAfter(resp.Header)
		e.Message = fmt.Sprintf("%s rate limit exceeded", p.s.displayName)
		if e.RetryAfter > 0 {
			e.Message += fmt.Sprintf("; retry after %s", e.RetryAfter.Round(time.Second))
		}
	default:
		e.Kind = errs.ErrProviderAPI
		e.Message = fmt.Sprintf("%s API error (HTTP %d)", p.s.displayName, resp.StatusCode)
		if msg != "" {
			e.Message += ": " + msg
		}
	}
	return e
}

// scrub removes the credential and other secret-looking material from a
// server-supplied message, strips control characters and bounds its length.
func scrub(msg, secret string) string {
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, redact.Placeholder)
	}
	msg = redact.String(msg)
	msg = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, msg)
	msg = strings.TrimSpace(msg)
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "..."
	}
	return msg
}

// --- pagination ----------------------------------------------------------------

type listSpec[T any] struct {
	op    string
	path  string // escaped, relative to api_base_url
	query url.Values
	limit int
	// keep is an optional client-side filter applied to each item. TFP
	// servers should filter themselves; this guarantees correct results when
	// an adapter ignores a filter parameter.
	keep     func(*T) bool
	notFound error
}

// list fetches all pages (up to spec.limit) using the configured strategy.
func list[T any](ctx context.Context, p *Provider, spec listSpec[T]) ([]T, error) {
	size := forge.PageSize(spec.limit, p.s.pagination.MaxPageSize)
	fetch := func(ctx context.Context, cursor string) ([]T, string, error) {
		// Keep fetching while client-side filtering empties a page, so that
		// forge.Collect does not mistake an empty filtered page for the end.
		seen := map[string]bool{}
		for {
			seen[cursor] = true
			items, next, err := fetchPage(ctx, p, spec, size, cursor)
			if err != nil {
				return nil, "", err
			}
			if spec.keep != nil {
				kept := items[:0]
				for i := range items {
					if spec.keep(&items[i]) {
						kept = append(kept, items[i])
					}
				}
				items = kept
			}
			if len(items) > 0 || next == "" || seen[next] {
				return items, next, nil
			}
			cursor = next
		}
	}
	out, err := forge.Collect(ctx, spec.limit, fetch)
	if err != nil {
		var e *errs.Error
		if !errors.As(err, &e) && ctx.Err() != nil {
			return nil, &errs.Error{Kind: errs.ErrCanceled, Provider: p.acct.Name, Op: spec.op, Cause: err}
		}
		return nil, err
	}
	return out, nil
}

func fetchPage[T any](ctx context.Context, p *Provider, spec listSpec[T], size int, cursor string) ([]T, string, error) {
	pg := p.s.pagination
	q := url.Values{}
	for k, v := range spec.query {
		q[k] = append([]string(nil), v...)
	}
	get := func(u string, out any) (http.Header, error) {
		return p.send(ctx, call{method: http.MethodGet, url: u, out: out, op: spec.op, notFound: spec.notFound})
	}

	switch pg.Strategy {
	case paginateLink:
		u := cursor
		if u == "" {
			q.Set(pg.PerPageParam, strconv.Itoa(size))
			u = p.endpoint(spec.path, q)
		}
		var items []T
		h, err := get(u, &items)
		if err != nil {
			return nil, "", err
		}
		next := httpx.ParseLinkHeader(strings.Join(h.Values("Link"), ", "))["next"]
		if next == "" {
			return items, "", nil
		}
		nu, err := p.followable(spec.op, u, next)
		if err != nil {
			return nil, "", err
		}
		return items, nu, nil

	case paginatePage:
		page := 1
		if cursor != "" {
			n, err := strconv.Atoi(cursor)
			if err != nil || n < 1 {
				return nil, "", p.protocolError(spec.op, "X-Next-Page must be a positive integer")
			}
			page = n
		}
		q.Set(pg.PageParam, strconv.Itoa(page))
		q.Set(pg.PerPageParam, strconv.Itoa(size))
		var items []T
		h, err := get(p.endpoint(spec.path, q), &items)
		if err != nil {
			return nil, "", err
		}
		// X-Next-Page, when present, is authoritative (empty = last page).
		if vals, ok := h[http.CanonicalHeaderKey("X-Next-Page")]; ok {
			next := ""
			if len(vals) > 0 {
				next = strings.TrimSpace(vals[0])
			}
			if next != "" {
				if n, err := strconv.Atoi(next); err != nil || n < 1 {
					return nil, "", p.protocolError(spec.op, "X-Next-Page must be a positive integer")
				}
			}
			return items, next, nil
		}
		if len(items) < size {
			return items, "", nil
		}
		return items, strconv.Itoa(page + 1), nil

	case paginateCursor:
		q.Set(pg.PerPageParam, strconv.Itoa(size))
		if cursor != "" {
			q.Set(pg.CursorParam, cursor)
		}
		var env struct {
			Items      []T    `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if _, err := get(p.endpoint(spec.path, q), &env); err != nil {
			return nil, "", err
		}
		return env.Items, env.NextCursor, nil

	default: // paginateNone
		var items []T
		if _, err := get(p.endpoint(spec.path, q), &items); err != nil {
			return nil, "", err
		}
		return items, "", nil
	}
}

// followable resolves a Link rel="next" target against the current request
// URL and refuses targets outside the api_base_url origin: following them
// would send the credential to another host.
func (p *Provider) followable(op, current, next string) (string, error) {
	base, err := url.Parse(current)
	if err != nil {
		return "", p.protocolError(op, "invalid request URL")
	}
	nu, err := base.Parse(next)
	if err != nil {
		return "", p.protocolError(op, "Link rel=\"next\" is not a valid URL")
	}
	if originOf(nu) != originOf(p.s.apiBase) {
		return "", p.protocolError(op, fmt.Sprintf("refusing to follow pagination link to %q: it is outside api_base_url's origin", nu.Host))
	}
	return nu.String(), nil
}

func (p *Provider) protocolError(op, msg string) error {
	return &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: op,
		Message: fmt.Sprintf("%s violated Trove Forge Protocol v1: %s", p.s.displayName, msg)}
}
