package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, slog.LevelDebug, true)
	l.Debug("sending Bearer ghp_abcdefghijklmnopqrstuvwxyz0123",
		"token", "glpat-zzzzzzzzzzzzzzzzzzzz",
		"Authorization", "Basic dXNlcjpwYXNz",
		"secret_ref", "bitwarden://trove/github/personal/token",
		"url", "https://x/api?private_token=glpat-yyyyyyyyyyyyyyyyyyyy",
		"note", "contains ghp_abcdefghijklmnopqrstuvwxyz0123")
	out := buf.String()
	for _, leak := range []string{"ghp_abcdefghijklmnopqrstuvwxyz0123", "glpat-zzzz", "dXNlcjpwYXNz", "trove/github/personal", "glpat-yyyy"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaked %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "bitwarden://[REDACTED]") {
		t.Errorf("secret_ref scheme should be kept: %s", out)
	}
}

func TestDefaultIsQuiet(t *testing.T) {
	lvl, on, err := ParseLevel("")
	if err != nil || on {
		t.Fatalf("empty level should disable logging: %v %v %v", lvl, on, err)
	}
	if _, _, err := ParseLevel("loud"); err == nil {
		t.Fatal("invalid level accepted")
	}
}
