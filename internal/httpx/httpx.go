// Package httpx provides HTTP plumbing shared by provider API clients:
// a redacting debug-logging transport, a conservative retry transport that
// understands HTTP 429 / Retry-After, Link header parsing and small helpers.
//
// It deliberately contains no provider-specific behavior. Each driver owns its
// API client (base URL, auth headers, pagination, error mapping) and only
// borrows these building blocks.
package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/redact"
	"github.com/SurajMazar/trove-cli/internal/version"
)

// UserAgent is sent with every API request.
func UserAgent() string { return "trove/" + version.Version }

// Options configure NewClient.
type Options struct {
	Timeout time.Duration
	Logger  *slog.Logger
	Retry   RetryPolicy
	// Base is the underlying transport (defaults to http.DefaultTransport).
	Base http.RoundTripper
}

// NewClient builds an *http.Client with logging and retry transports.
func NewClient(o Options) *http.Client {
	base := o.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	var rt http.RoundTripper = &RetryTransport{Base: base, Policy: o.Retry.withDefaults(), Logger: o.Logger}
	if o.Logger != nil {
		rt = &LoggingTransport{Base: rt, Logger: o.Logger}
	}
	return &http.Client{Transport: rt, Timeout: o.Timeout}
}

// LoggingTransport logs requests at debug level with credentials redacted.
type LoggingTransport struct {
	Base   http.RoundTripper
	Logger *slog.Logger
}

// RoundTrip implements http.RoundTripper.
func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	ctx := req.Context()
	if t.Logger.Enabled(ctx, slog.LevelDebug) {
		t.Logger.DebugContext(ctx, "http request",
			"method", req.Method,
			"url", redact.URL(req.URL.String()),
			"headers", headerString(redact.Headers(req.Header)))
	}
	resp, err := t.Base.RoundTrip(req)
	if err != nil {
		t.Logger.DebugContext(ctx, "http error", "method", req.Method, "url", redact.URL(req.URL.String()), "error", redact.String(err.Error()))
		return nil, err
	}
	if t.Logger.Enabled(ctx, slog.LevelDebug) {
		t.Logger.DebugContext(ctx, "http response",
			"method", req.Method,
			"url", redact.URL(req.URL.String()),
			"status", resp.StatusCode,
			"duration", time.Since(start).Round(time.Millisecond).String(),
			"headers", headerString(redact.Headers(resp.Header)))
	}
	return resp, nil
}

func headerString(h http.Header) string {
	var b strings.Builder
	first := true
	for k, v := range h {
		if !first {
			b.WriteString("; ")
		}
		first = false
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(strings.Join(v, ","))
	}
	return b.String()
}

// RetryPolicy controls RetryTransport.
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	// MaxWait is the longest Retry-After we are willing to sleep for. Longer
	// waits surface as rate-limit errors to the caller instead.
	MaxWait time.Duration
	// RateLimited lets a driver recognize provider-specific rate limit
	// responses (e.g. GitHub's 403 + x-ratelimit-remaining: 0) and return the
	// wait duration if known.
	RateLimited func(resp *http.Response) (limited bool, wait time.Duration)
	// Disabled turns off retries entirely.
	Disabled bool
}

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxRetries == 0 {
		p.MaxRetries = 3
	}
	if p.BaseDelay == 0 {
		p.BaseDelay = 500 * time.Millisecond
	}
	if p.MaxDelay == 0 {
		p.MaxDelay = 20 * time.Second
	}
	if p.MaxWait == 0 {
		p.MaxWait = 60 * time.Second
	}
	return p
}

type retrySafeKey struct{}

// WithRetrySafe marks a non-idempotent request as safe to retry.
func WithRetrySafe(ctx context.Context) context.Context {
	return context.WithValue(ctx, retrySafeKey{}, true)
}

// WithNoRetry disables retries for a request.
func WithNoRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, retrySafeKey{}, false)
}

func retryable(req *http.Request) bool {
	if v, ok := req.Context().Value(retrySafeKey{}).(bool); ok {
		return v && (req.Body == nil || req.GetBody != nil)
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// RetryTransport retries idempotent requests on 429, transient 5xx and
// network errors with exponential backoff and jitter, honoring Retry-After
// and context cancellation. Non-idempotent requests (POST, PATCH, DELETE...)
// are never retried unless marked with WithRetrySafe.
type RetryTransport struct {
	Base   http.RoundTripper
	Policy RetryPolicy
	Logger *slog.Logger
}

// RoundTrip implements http.RoundTripper.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p := t.Policy
	if p.Disabled || !retryable(req) {
		return t.Base.RoundTrip(req)
	}
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		r := req
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			r = req.Clone(ctx)
			r.Body = body
		}
		resp, err := t.Base.RoundTrip(r)
		if attempt >= p.MaxRetries {
			return resp, err
		}
		var wait time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, err
			}
			wait = backoff(p, attempt)
		case resp.StatusCode == http.StatusTooManyRequests:
			wait = RetryAfter(resp.Header)
			if wait == 0 {
				wait = backoff(p, attempt)
			}
		case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout:
			wait = RetryAfter(resp.Header)
			if wait == 0 {
				wait = backoff(p, attempt)
			}
		default:
			if p.RateLimited != nil {
				if limited, w := p.RateLimited(resp); limited {
					wait = w
					if wait == 0 {
						wait = backoff(p, attempt)
					}
					break
				}
			}
			return resp, nil
		}
		if wait > p.MaxWait {
			// Too long to wait silently; let the driver report the limit.
			return resp, err
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		if t.Logger != nil {
			t.Logger.DebugContext(ctx, "retrying request", "method", req.Method,
				"url", redact.URL(req.URL.String()), "attempt", attempt+1, "wait", wait.String())
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func backoff(p RetryPolicy, attempt int) time.Duration {
	d := p.BaseDelay << attempt
	if d > p.MaxDelay || d <= 0 {
		d = p.MaxDelay
	}
	// Full jitter in [d/2, d).
	half := int64(d / 2)
	if half <= 0 {
		return d
	}
	return time.Duration(half + rand.Int64N(half))
}

// RetryAfter parses a Retry-After header (delta-seconds or HTTP date).
func RetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

var linkRe = regexp.MustCompile(`<([^>]+)>\s*((?:;\s*[a-zA-Z]+="?[^",;]*"?\s*)+)`)
var relRe = regexp.MustCompile(`rel="?([^",;]+)"?`)

// ParseLinkHeader parses an RFC 8288 Link header into rel -> URL.
func ParseLinkHeader(h string) map[string]string {
	out := map[string]string{}
	for _, m := range linkRe.FindAllStringSubmatch(h, -1) {
		if rm := relRe.FindStringSubmatch(m[2]); rm != nil {
			for _, rel := range strings.Fields(rm[1]) {
				out[rel] = m[1]
			}
		}
	}
	return out
}

// ReadBody reads up to limit bytes of a response body.
func ReadBody(resp *http.Response, limit int64) []byte {
	if limit <= 0 {
		limit = 1 << 20
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, limit))
	return b
}

// DecodeJSON decodes a JSON response body into v.
func DecodeJSON(resp *http.Response, v any) error {
	if v == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	if err := dec.Decode(v); err != nil {
		if err == io.EOF {
			return nil
		}
		return fmt.Errorf("decode %s response: %w", resp.Request.URL.Path, err)
	}
	return nil
}

// Sleep waits for d or until ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
