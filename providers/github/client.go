package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
	"github.com/SurajMazar/trove-cli/internal/redact"
)

// authMode selects how a request is authenticated.
type authMode int

const (
	authDefault  authMode = iota // stored credential (installation token for Apps)
	authNone                     // no Authorization header
	authExplicit                 // request.token (login validation, App JWT)
)

// request describes one REST call.
type request struct {
	op     string
	method string
	// path is relative to the API base ("/user/repos") or an absolute URL on
	// the API origin (pagination links).
	path  string
	query url.Values
	body  any
	out   any
	// notFound is the error kind for 404 (default errs.ErrNotFound).
	notFound error
	// notFoundMsg replaces the generic 404 message.
	notFoundMsg string
	accept      string
	auth        authMode
	token       string
	// statusKinds overrides the error kind for specific HTTP statuses.
	statusKinds map[int]error
}

// apiURL resolves a request path against the API base. Absolute URLs are
// only accepted on the API origin so pagination links can never redirect
// credentials to another host.
func (p *Provider) apiURL(path string, q url.Values) (*url.URL, error) {
	var u *url.URL
	var err error
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		u, err = url.Parse(path)
		if err != nil {
			return nil, err
		}
		if u.Scheme != p.api.Scheme || !strings.EqualFold(u.Host, p.api.Host) {
			return nil, fmt.Errorf("refusing to follow a link to another host (%s)", u.Host)
		}
	} else {
		u, err = url.Parse(strings.TrimRight(p.api.String(), "/") + path)
		if err != nil {
			return nil, err
		}
	}
	if len(q) > 0 {
		merged := u.Query()
		for k, v := range q {
			merged[k] = v
		}
		u.RawQuery = merged.Encode()
	}
	return u, nil
}

// open performs the request and returns the successful response with its
// body still open. Non-2xx responses are mapped to *errs.Error.
func (p *Provider) open(ctx context.Context, hc *http.Client, r request) (*http.Response, error) {
	if err := p.ctxErr(ctx, r.op); err != nil {
		return nil, err
	}
	u, err := p.apiURL(r.path, r.query)
	if err != nil {
		return nil, p.errorf(errs.ErrProviderAPI, r.op, 0, "%s", err.Error())
	}
	var body io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, p.errorf(errs.ErrInvalidArgument, r.op, 0, "encode request: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), body)
	if err != nil {
		return nil, p.errorf(errs.ErrProviderAPI, r.op, 0, "build request: %v", err)
	}
	accept := r.accept
	if accept == "" {
		accept = mediaType
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", httpx.UserAgent())
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch r.auth {
	case authExplicit:
		req.Header.Set("Authorization", "Bearer "+r.token)
	case authDefault:
		tok, err := p.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if hc == nil {
		hc = p.hc
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, p.transportError(ctx, r.op, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	return nil, p.apiError(r, resp)
}

// do performs the request, decodes a JSON body into r.out (if set) and
// returns the response (body closed) so callers can read headers.
func (p *Provider) do(ctx context.Context, r request) (*http.Response, error) {
	resp, err := p.open(ctx, nil, r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if r.out == nil || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusResetContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resp, nil
	}
	if err := httpx.DecodeJSON(resp, r.out); err != nil {
		if ctxErr := p.ctxErr(ctx, r.op); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, p.errorf(errs.ErrProviderAPI, r.op, resp.StatusCode, "unexpected response from %s: %v", p.meta.DisplayName, err)
	}
	return resp, nil
}

func (p *Provider) errorf(kind error, op string, status int, format string, args ...any) *errs.Error {
	return &errs.Error{Kind: kind, Provider: p.acct.Name, Op: op, Status: status, Message: fmt.Sprintf(format, args...)}
}

func (p *Provider) canceled(op string, err error) error {
	kind := errs.ErrCanceled
	if errors.Is(err, context.DeadlineExceeded) {
		kind = errs.ErrProviderAPI
	}
	return &errs.Error{Kind: kind, Provider: p.acct.Name, Op: op, Cause: err}
}

func (p *Provider) transportError(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return p.canceled(op, ctxErr)
	}
	return p.errorf(errs.ErrProviderAPI, op, 0, "could not reach %s: %s", p.meta.DisplayName, redact.String(err.Error()))
}

func (p *Provider) loginHint() string { return "trove auth login " + p.acct.Name }

// apiErrorBody is GitHub's error envelope. "errors" items are usually
// objects but some endpoints return plain strings.
type apiErrorBody struct {
	Message          string            `json:"message"`
	Errors           []json.RawMessage `json:"errors"`
	DocumentationURL string            `json:"documentation_url"`
}

type apiErrorDetail struct {
	Resource string `json:"resource"`
	Field    string `json:"field"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

func (b apiErrorBody) details() []string {
	var out []string
	for _, raw := range b.Errors {
		if len(out) >= 5 {
			break
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s != "" {
				out = append(out, s)
			}
			continue
		}
		var d apiErrorDetail
		if json.Unmarshal(raw, &d) != nil {
			continue
		}
		switch {
		case d.Message != "":
			out = append(out, d.Message)
		case d.Field != "" && d.Code != "":
			out = append(out, d.Field+" "+strings.ReplaceAll(d.Code, "_", " "))
		case d.Code != "":
			out = append(out, strings.ReplaceAll(d.Code, "_", " "))
		}
	}
	return out
}

// safeMessage limits and redacts a provider-supplied message.
func safeMessage(s string) string {
	s = strings.TrimSpace(redact.String(s))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// apiError maps a non-2xx response to a typed error.
func (p *Provider) apiError(r request, resp *http.Response) error {
	var body apiErrorBody
	_ = json.Unmarshal(httpx.ReadBody(resp, 64<<10), &body)
	msg := safeMessage(body.Message)
	status := resp.StatusCode
	e := &errs.Error{Provider: p.acct.Name, Op: r.op, Status: status}
	name := p.meta.DisplayName

	if kind, ok := r.statusKinds[status]; ok {
		e.Kind = kind
		e.Message = orDefault(msg, fmt.Sprintf("%s returned HTTP %d", name, status))
		return e
	}
	if limited, wait := rateLimited(resp); limited || (status == http.StatusForbidden && strings.Contains(strings.ToLower(msg), "rate limit")) {
		if wait <= 0 {
			wait = time.Minute // GitHub asks clients to wait at least a minute
		}
		e.Kind = errs.ErrRateLimited
		e.RetryAfter = wait
		e.Message = fmt.Sprintf("%s API rate limit exceeded; retry in %s", name, wait.Round(time.Second))
		if msg != "" {
			e.Message += " (" + msg + ")"
		}
		return e
	}
	switch status {
	case http.StatusUnauthorized:
		e.Kind = errs.ErrAuthenticationFailed
		e.Message = name + " authentication failed: token is invalid or expired"
		e.Hint = p.loginHint()
	case http.StatusForbidden:
		e.Kind = errs.ErrPermissionDenied
		e.Message = orDefault(msg, name+" denied access")
		if sso := resp.Header.Get("X-GitHub-SSO"); sso != "" {
			e.Hint = "authorize the token for SAML single sign-on in the organization settings"
			if i := strings.Index(sso, "url="); i >= 0 {
				e.Hint += ": " + strings.TrimSpace(sso[i+4:])
			}
		} else if need := resp.Header.Get("X-Accepted-OAuth-Scopes"); need != "" {
			e.Hint = "the token may be missing a required scope (accepted: " + need + ")"
		}
	case http.StatusNotFound:
		kind := r.notFound
		if kind == nil {
			kind = errs.ErrNotFound
		}
		e.Kind = kind
		if kind == errs.ErrRepositoryNotFound {
			e.Cause = errs.ErrNotFound
			e.Hint = "private repositories return 404 when the token lacks access"
		}
		e.Message = orDefault(r.notFoundMsg, "not found")
	case http.StatusConflict:
		e.Kind = errs.ErrConflict
		e.Message = orDefault(msg, "conflict")
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		e.Kind = errs.ErrInvalidArgument
		m := orDefault(msg, "request was rejected")
		if d := body.details(); len(d) > 0 {
			m += ": " + safeMessage(strings.Join(d, "; "))
		}
		e.Message = m
	case http.StatusTooManyRequests:
		e.Kind = errs.ErrRateLimited
		e.RetryAfter = httpx.RetryAfter(resp.Header)
		e.Message = name + " API rate limit exceeded"
	default:
		e.Kind = errs.ErrProviderAPI
		e.Message = fmt.Sprintf("%s returned HTTP %d", name, status)
		if msg != "" {
			e.Message += ": " + msg
		}
	}
	return e
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// rateLimited recognizes GitHub rate limit responses: 429, or 403 with
// either Retry-After (secondary limits) or x-ratelimit-remaining: 0 (primary
// limit; wait until x-ratelimit-reset).
func rateLimited(resp *http.Response) (bool, time.Duration) {
	h := resp.Header
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		if d := httpx.RetryAfter(h); d > 0 {
			return true, d
		}
		return true, untilReset(h)
	case http.StatusForbidden:
		if h.Get("Retry-After") != "" {
			return true, httpx.RetryAfter(h)
		}
		if h.Get("X-RateLimit-Remaining") == "0" {
			return true, untilReset(h)
		}
	}
	return false, 0
}

func untilReset(h http.Header) time.Duration {
	secs, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64)
	if err != nil {
		return 0
	}
	d := time.Until(time.Unix(secs, 0))
	if d < 0 {
		return 0
	}
	return d.Round(time.Second) + time.Second
}

// esc escapes one path segment.
func esc(s string) string { return url.PathEscape(s) }

// escRef escapes a ref such as "feature/x" segment by segment, keeping the
// slashes GitHub expects.
func escRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func repoPath(ns, name string, rest ...string) string {
	var b strings.Builder
	b.WriteString("/repos/")
	b.WriteString(esc(ns))
	b.WriteString("/")
	b.WriteString(esc(name))
	for _, r := range rest {
		b.WriteString("/")
		b.WriteString(r)
	}
	return b.String()
}

// page is one page of a list endpoint.
type page[R any] struct {
	items []R
	next  string
}

// listSpec configures paginate.
type listSpec[R any] struct {
	request
	perPage int // max page size for this endpoint
	limit   int
	// decode extracts items from an envelope; nil means a top-level array.
	decode func(resp *http.Response) ([]R, error)
}

// paginate walks Link rel="next" pages, converting and filtering items with
// convert, and normalizes via forge.Collect. Pages whose items are all
// filtered out are skipped transparently so Collect never mistakes them for
// the end of the list.
func paginate[R, D any](ctx context.Context, p *Provider, s listSpec[R], convert func(R) (D, bool)) ([]D, error) {
	perPage := forge.PageSize(s.limit, s.perPage)
	fetch := func(ctx context.Context, cursor string) ([]D, string, error) {
		for {
			pg, err := fetchPage(ctx, p, s, cursor, perPage)
			if err != nil {
				return nil, "", err
			}
			out := make([]D, 0, len(pg.items))
			for _, raw := range pg.items {
				if d, ok := convert(raw); ok {
					out = append(out, d)
				}
			}
			if len(out) > 0 || pg.next == "" || pg.next == cursor {
				return out, pg.next, nil
			}
			cursor = pg.next
		}
	}
	return forge.Collect(ctx, s.limit, fetch)
}

func fetchPage[R any](ctx context.Context, p *Provider, s listSpec[R], cursor string, perPage int) (page[R], error) {
	r := s.request
	if r.method == "" {
		r.method = http.MethodGet
	}
	if cursor != "" {
		// The next link already carries every query parameter.
		r.path, r.query = cursor, nil
	} else {
		q := url.Values{}
		for k, v := range s.query {
			q[k] = v
		}
		q.Set("per_page", strconv.Itoa(perPage))
		r.query = q
	}
	var items []R
	resp, err := p.open(ctx, nil, r)
	if err != nil {
		return page[R]{}, err
	}
	defer resp.Body.Close()
	if s.decode != nil {
		items, err = s.decode(resp)
	} else {
		err = httpx.DecodeJSON(resp, &items)
	}
	if err != nil {
		if ctxErr := p.ctxErr(ctx, r.op); ctxErr != nil {
			return page[R]{}, ctxErr
		}
		return page[R]{}, p.errorf(errs.ErrProviderAPI, r.op, resp.StatusCode, "unexpected response from %s: %v", p.meta.DisplayName, err)
	}
	return page[R]{items: items, next: httpx.ParseLinkHeader(resp.Header.Get("Link"))["next"]}, nil
}

// envelope decodes {"<field>": [...]} list responses and reports total_count.
func envelope[R any](field string, total *int) func(*http.Response) ([]R, error) {
	return func(resp *http.Response) ([]R, error) {
		var raw map[string]json.RawMessage
		if err := httpx.DecodeJSON(resp, &raw); err != nil {
			return nil, err
		}
		if total != nil {
			if t, ok := raw["total_count"]; ok {
				_ = json.Unmarshal(t, total)
			}
		}
		var items []R
		if v, ok := raw[field]; ok {
			if err := json.Unmarshal(v, &items); err != nil {
				return nil, fmt.Errorf("decode %s: %w", field, err)
			}
		}
		return items, nil
	}
}

// identity is a convert func that keeps every item.
func identity[T any](v T) (T, bool) { return v, true }

// parseNumericID validates an ID used in a URL path.
func (p *Provider) parseNumericID(op, what, id string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil || n <= 0 {
		return 0, p.errorf(errs.ErrInvalidArgument, op, 0, "invalid %s %q: must be a positive number", what, id)
	}
	return n, nil
}
