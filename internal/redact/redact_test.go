package redact

import (
	"net/http"
	"strings"
	"testing"
)

func TestHeadersRedactsCredentials(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	h.Set("Private-Token", "glpat-abcdefghijklmnopqrst")
	h.Set("Cookie", "session=abc")
	h.Set("Accept", "application/json")
	out := Headers(h)
	for _, k := range []string{"Authorization", "Private-Token", "Cookie"} {
		if out.Get(k) != Placeholder {
			t.Errorf("%s = %q, want redacted", k, out.Get(k))
		}
	}
	if out.Get("Accept") != "application/json" {
		t.Errorf("Accept was modified: %q", out.Get("Accept"))
	}
}

func TestURLRedactsQueryAndUserinfo(t *testing.T) {
	got := URL("https://user:pass@gitlab.example.com/api/v4/projects?private_token=glpat-secret123456789012&page=2")
	if strings.Contains(got, "pass") || strings.Contains(got, "glpat-secret") {
		t.Fatalf("URL not redacted: %s", got)
	}
	if !strings.Contains(got, "page=2") {
		t.Fatalf("non-secret params lost: %s", got)
	}
}

func TestStringMasksTokens(t *testing.T) {
	cases := []string{
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"github_pat_11ABCDEFG0123456789_abcdefghijklmnop",
		"glpat-abcdefghijklmnopqrst",
		"Bearer abc123def456ghi789",
		"ATATT3xFfGF0abcdefghijklmnopqrstuvwxyz",
	}
	for _, c := range cases {
		got := String("error: " + c + " rejected")
		if strings.Contains(got, c) {
			t.Errorf("String(%q) not masked: %q", c, got)
		}
	}
}

func TestStringKeepsProse(t *testing.T) {
	for _, s := range []string{
		"token authentication failed",
		"basic authentication is not supported",
		"token is invalid or expired",
	} {
		if got := String(s); got != s {
			t.Errorf("String(%q) = %q, prose should be untouched", s, got)
		}
	}
}
