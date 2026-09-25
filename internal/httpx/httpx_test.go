package httpx

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastPolicy() RetryPolicy {
	return RetryPolicy{MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, MaxWait: time.Second}
}

func TestRetryOn429WithRetryAfter(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := NewClient(Options{Retry: fastPolicy()})
	resp, err := c.Get(srv.URL)
	if err != nil || resp.StatusCode != 200 || atomic.LoadInt32(&n) != 3 {
		t.Fatalf("status=%v err=%v attempts=%d", resp, err, n)
	}
}

func TestNoRetryForPOST(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewClient(Options{Retry: fastPolicy()})
	resp, err := c.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil || resp.StatusCode != 503 || n != 1 {
		t.Fatalf("POST retried: attempts=%d", n)
	}
	// Explicitly marked safe → retried with body replayed.
	n = 0
	req, _ := http.NewRequestWithContext(WithRetrySafe(context.Background()), http.MethodPost, srv.URL, bytes.NewReader([]byte("{}")))
	resp, _ = c.Do(req)
	if resp.StatusCode != 503 || n != 4 {
		t.Fatalf("safe POST attempts = %d", n)
	}
}

func TestLongRetryAfterSurfacesToCaller(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	resp, err := NewClient(Options{Retry: fastPolicy()}).Get(srv.URL)
	if err != nil || resp.StatusCode != 429 || n != 1 {
		t.Fatalf("should not sleep an hour: attempts=%d", n)
	}
	if RetryAfter(resp.Header) != time.Hour {
		t.Fatalf("RetryAfter = %v", RetryAfter(resp.Header))
	}
}

func TestProviderSpecificRateLimitDetector(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := fastPolicy()
	p.RateLimited = func(r *http.Response) (bool, time.Duration) {
		return r.StatusCode == 403 && r.Header.Get("X-RateLimit-Remaining") == "0", 0
	}
	resp, _ := NewClient(Options{Retry: p}).Get(srv.URL)
	if resp.StatusCode != 200 || n != 2 {
		t.Fatalf("detector not honored: attempts=%d", n)
	}
}

func TestRetryHonorsCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := RetryPolicy{MaxRetries: 5, BaseDelay: time.Second, MaxDelay: time.Second, MaxWait: time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	start := time.Now()
	_, err := NewClient(Options{Retry: p}).Do(req)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 900*time.Millisecond {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
	}
}

func TestLoggingRedactsAuthorization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "cookie-secret-value"})
		w.WriteHeader(200)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewClient(Options{Logger: log, Retry: fastPolicy()})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x?private_token=glpat-querysecret12345678", nil)
	req.Header.Set("Authorization", "Bearer ghp_headersecret1234567890abcdef")
	req.Header.Set("Private-Token", "glpat-othersecret1234567890")
	if _, err := c.Do(req); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, leak := range []string{"ghp_headersecret", "glpat-querysecret", "glpat-othersecret", "cookie-secret-value"} {
		if strings.Contains(out, leak) {
			t.Errorf("debug log leaked %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction markers:\n%s", out)
	}
}

func TestParseLinkHeader(t *testing.T) {
	h := `<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=5>; rel="last"`
	l := ParseLinkHeader(h)
	if l["next"] != "https://api.github.com/user/repos?page=2" || l["last"] != "https://api.github.com/user/repos?page=5" {
		t.Fatalf("ParseLinkHeader = %v", l)
	}
	if len(ParseLinkHeader("")) != 0 {
		t.Fatal("empty header should parse to nothing")
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if d := RetryAfter(h); d < 25*time.Second || d > 31*time.Second {
		t.Fatalf("RetryAfter(date) = %v", d)
	}
}
