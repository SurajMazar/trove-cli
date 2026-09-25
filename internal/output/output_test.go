package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

func TestTableTruncatesToWidth(t *testing.T) {
	tb := &Table{Columns: []Column{{Header: "Name"}, {Header: "Description", Flex: true}, {Header: "Updated", MinTerminal: 120}}}
	tb.Add(C("frontend"), C(strings.Repeat("very long description ", 10)), C("2h ago"))
	tb.Add(C("api"), C("short"), C("1d ago"))
	out := tb.Render(60, lipgloss.NewStyle(), lipgloss.NewStyle())
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := lipgloss.Width(line); w > 60 {
			t.Errorf("line exceeds width (%d): %q", w, line)
		}
	}
	if strings.Contains(out, "UPDATED") {
		t.Error("column with MinTerminal 120 should be hidden at width 60")
	}
	if !strings.Contains(out, "…") {
		t.Error("expected truncation marker")
	}
	wide := tb.Render(160, lipgloss.NewStyle(), lipgloss.NewStyle())
	if !strings.Contains(wide, "UPDATED") {
		t.Error("UPDATED column should be visible at width 160")
	}
}

func TestJSONModeWritesOnlyJSON(t *testing.T) {
	var out, errb bytes.Buffer
	p := New(terminal.Test(nil, &out, &errb), ModeJSON)
	p.Success("should not appear")
	p.Info("nor this")
	if err := p.Result(map[string]int{"n": 1}, nil, func() error { t.Fatal("human called"); return nil }); err != nil {
		t.Fatal(err)
	}
	var v map[string]int
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || v["n"] != 1 {
		t.Fatalf("stdout is not the JSON document: %q (%v)", out.String(), err)
	}
	if errb.Len() != 0 {
		t.Fatalf("unexpected stderr in JSON mode: %q", errb.String())
	}
}

func TestQuietMode(t *testing.T) {
	var out bytes.Buffer
	p := New(terminal.Test(nil, &out, &bytes.Buffer{}), ModeQuiet)
	_ = p.Result(nil, func() []string { return []string{"a/b", "c/d"} }, nil)
	if out.String() != "a/b\nc/d\n" {
		t.Fatalf("quiet output = %q", out.String())
	}
}

func TestRenderErrorHouseStyle(t *testing.T) {
	var errb bytes.Buffer
	io := terminal.Test(nil, &bytes.Buffer{}, &errb)
	err := &errs.Error{Kind: errs.ErrAuthenticationFailed, Provider: "gitlab-work",
		Message: "GitLab authentication failed: token is invalid or expired", Hint: "trove auth login gitlab-work"}
	RenderError(io, ModeHuman, err, false)
	s := errb.String()
	for _, want := range []string{"✗ Authentication failed", "token is invalid or expired", "Provider: gitlab-work", "Try:", "trove auth login gitlab-work"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	errb.Reset()
	RenderError(io, ModeJSON, err, false)
	var doc map[string]map[string]any
	if json.Unmarshal(errb.Bytes(), &doc) != nil || doc["error"]["code"] != "authentication_failed" {
		t.Fatalf("bad JSON error: %s", errb.String())
	}
}

func TestRenderErrorRedacts(t *testing.T) {
	var errb bytes.Buffer
	RenderError(terminal.Test(nil, &bytes.Buffer{}, &errb), ModeHuman, errs.New(errs.ErrProviderAPI, "bad token glpat-abcdefghijklmnopqrst"), true)
	if strings.Contains(errb.String(), "glpat-abcdefghijklmnopqrst") {
		t.Fatalf("token leaked: %s", errb.String())
	}
}

func TestRelTime(t *testing.T) {
	if RelTime(time.Time{}) != "" {
		t.Error("zero time should be empty")
	}
	if got := RelTime(time.Now().Add(-2 * time.Hour)); got != "2h ago" {
		t.Errorf("RelTime = %q", got)
	}
	if got := RelTime(time.Now().Add(-3 * 24 * time.Hour)); got != "3d ago" {
		t.Errorf("RelTime = %q", got)
	}
}
