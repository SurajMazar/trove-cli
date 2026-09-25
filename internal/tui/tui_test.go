package tui

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/services"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(m tea.Model, keys ...string) tea.Model {
	for _, k := range keys {
		m, _ = m.Update(key(k))
	}
	return m
}

func typeText(m tea.Model, s string) tea.Model {
	for _, r := range s {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

func sampleItems() []Item {
	mk := func(id, ns, prov string) Item {
		return Item{ID: id, Title: id, Facets: map[string]string{"namespace": ns, "provider": prov}}
	}
	return []Item{
		mk("acme/frontend", "acme", "gh"), mk("acme/frontend-admin", "acme", "gh"), mk("acme/backend", "acme", "gh"),
		mk("tools/infra", "tools", "gl"), mk("tools/docs", "tools", "gl"),
	}
}

func theme() *terminal.Theme { return terminal.NewTheme(&bytes.Buffer{}, false) }

func TestMultiSelectKeys(t *testing.T) {
	m := newListModel(theme(), 100, ListOptions{Multi: true, Items: sampleItems(), Noun: "repositories",
		Facets: []Facet{{Key: "namespace", Label: "Namespace", Binding: "f"}, {Key: "provider", Label: "Provider", Binding: "p"}}})
	var tm tea.Model = m
	tm = press(tm, " ", " ") // select first two
	if len(m.selected) != 2 || !m.selected["acme/frontend"] || !m.selected["acme/frontend-admin"] {
		t.Fatalf("space selection = %v", m.selected)
	}
	tm = press(tm, "n")
	if len(m.selected) != 0 {
		t.Fatalf("n should deselect all: %v", m.selected)
	}
	tm = press(tm, "a")
	if len(m.selected) != 5 {
		t.Fatalf("a should select all: %v", m.selected)
	}
	tm = press(tm, "n")
	// Search narrows and "a" selects only visible items.
	tm = press(tm, "/")
	tm = typeText(tm, "front")
	if len(m.filtered) != 2 {
		t.Fatalf("search 'front' matched %d", len(m.filtered))
	}
	tm = press(tm, "enter", "a")
	if len(m.selected) != 2 {
		t.Fatalf("select-all within filter = %v", m.selected)
	}
	// Esc clears the filter.
	tm = press(tm, "esc")
	if len(m.filtered) != 5 || m.query != "" {
		t.Fatalf("esc should clear the filter: %d %q", len(m.filtered), m.query)
	}
	// Facet filters.
	tm = press(tm, "f")
	if m.facetSel["namespace"] != "acme" || len(m.filtered) != 3 {
		t.Fatalf("namespace facet: %v %d", m.facetSel, len(m.filtered))
	}
	tm = press(tm, "f")
	if m.facetSel["namespace"] != "tools" || len(m.filtered) != 2 {
		t.Fatalf("second namespace: %v %d", m.facetSel, len(m.filtered))
	}
	tm = press(tm, "f", "p")
	if m.facetSel["namespace"] != "" || m.facetSel["provider"] != "gh" || len(m.filtered) != 3 {
		t.Fatalf("provider facet: %v %d", m.facetSel, len(m.filtered))
	}
	view := m.View()
	for _, want := range []string{"2 selected", "3 of 5 repositories", "Provider: gh", "space select"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	_, cmd := tm.Update(key("enter"))
	if cmd == nil || !m.done || len(m.result()) != 2 {
		t.Fatalf("enter should finish with the selection: done=%v result=%v", m.done, m.result())
	}
}

func TestEnterSelectsCurrentWhenNothingSelected(t *testing.T) {
	m := newListModel(theme(), 100, ListOptions{Multi: true, Items: sampleItems()})
	press(m, "down", "down", "enter")
	if r := m.result(); len(r) != 1 || r[0].ID != "acme/backend" {
		t.Fatalf("result = %v", r)
	}
}

func TestSingleSelectAndQuit(t *testing.T) {
	m := newListModel(theme(), 100, ListOptions{Items: sampleItems()})
	press(m, "down", "enter")
	if r := m.result(); len(r) != 1 || r[0].ID != "acme/frontend-admin" {
		t.Fatalf("single result = %v", r)
	}
	q := newListModel(theme(), 100, ListOptions{Items: sampleItems()})
	press(q, "q")
	if !q.aborted {
		t.Fatal("q should abort")
	}
	c := newListModel(theme(), 100, ListOptions{Items: sampleItems()})
	press(c, "/", "q") // in search mode, q is text
	if c.aborted || c.query != "q" {
		t.Fatalf("q in search mode: aborted=%v query=%q", c.aborted, c.query)
	}
	press(c, "ctrl+c")
	if !c.aborted {
		t.Fatal("ctrl+c should always abort")
	}
}

func TestProtocolToggleAndPagination(t *testing.T) {
	var items []Item
	for i := 0; i < 120; i++ {
		items = append(items, Item{ID: string(rune('A'+i%26)) + strings.Repeat("x", i), Title: "repo"})
	}
	m := newListModel(theme(), 80, ListOptions{Multi: true, Items: items, ProtocolToggle: true, Protocol: domain.ProtocolHTTPS})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	if !strings.Contains(m.View(), "HTTPS") || !strings.Contains(m.View(), "page 1/") {
		t.Fatalf("view:\n%s", m.View())
	}
	press(m, "t")
	if m.protocol != domain.ProtocolSSH || !strings.Contains(m.View(), "SSH") {
		t.Fatal("t should toggle SSH")
	}
	press(m, "right")
	if m.offset == 0 || !strings.Contains(m.View(), "page 2/") {
		t.Fatalf("page down did not move: offset=%d", m.offset)
	}
	press(m, "G")
	if m.cursor != 119 {
		t.Fatalf("end: cursor=%d", m.cursor)
	}
	// The rendered view respects the terminal width.
	for _, line := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Errorf("line wider than terminal (%d): %q", w, line)
		}
	}
}

func TestRunListNonInteractive(t *testing.T) {
	tio := terminal.Test(nil, &bytes.Buffer{}, &bytes.Buffer{})
	_, err := RunList(context.Background(), tio, ListOptions{Items: sampleItems()})
	if !errors.Is(err, errs.ErrInteractionRequired) {
		t.Fatalf("want ErrInteractionRequired, got %v", err)
	}
	if _, err := RunList(context.Background(), tio, ListOptions{}); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("empty list: %v", err)
	}
}

func TestRunListWithScriptedInput(t *testing.T) {
	var out bytes.Buffer
	tio := terminal.Test(strings.NewReader(" \x1b[B \r"), &bytes.Buffer{}, &out)
	tio.Interactive = true
	res, err := RunList(context.Background(), tio, ListOptions{Multi: true, Items: sampleItems()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 2 {
		t.Fatalf("selected %v", res.Selected)
	}
}

func TestDashboardModel(t *testing.T) {
	n := 42
	m := &dashModel{data: DashboardData{ProviderLabel: "GitHub Personal", ProviderType: "GitHub"}, loading: false, th: theme(), width: 80}
	m.data.User = &domain.User{Username: "octo"}
	m.data.Summary = &domain.AccountSummary{Repositories: &n}
	v := m.View()
	for _, want := range []string{"TROVE", "GitHub Personal", "@octo", "42", "Repositories", "Quit"} {
		if !strings.Contains(v, want) {
			t.Errorf("dashboard missing %q:\n%s", want, v)
		}
	}
	for _, line := range strings.Split(v, "\n") {
		if w := lipgloss.Width(line); w > 80 {
			t.Errorf("dashboard wider than 80 columns (%d): %q", w, line)
		}
	}
	if !strings.Contains(v, "—") {
		t.Error("unavailable counts should render as —, never as 0")
	}
	press(m, "down", "enter")
	if m.chosen != ActionPullRequests {
		t.Fatalf("chosen = %s", m.chosen)
	}
}

func TestProgressModel(t *testing.T) {
	names := []string{"a/one", "a/two", "a/three"}
	m := &progressModel{title: "Cloning", names: names, states: make([]services.CloneState, 3), th: theme(), width: 80}
	m.Update(progressEvent{Index: 0, State: services.CloneDone})
	m.Update(progressEvent{Index: 1, State: services.CloneFailed})
	m.Update(progressEvent{Index: 2, State: services.CloneRunning})
	v := m.View()
	if !strings.Contains(v, "✓ a/one") || !strings.Contains(v, "✗ a/two") || !strings.Contains(v, "2 / 3 completed") || !strings.Contains(v, "1 failed") {
		t.Fatalf("progress view:\n%s", v)
	}
	_, cmd := m.Update(progressDone{})
	if cmd == nil || !m.finished {
		t.Fatal("progressDone should quit")
	}
}

func TestInputModelValidationAndSecret(t *testing.T) {
	tio := terminal.Test(strings.NewReader("\rhello\r"), &bytes.Buffer{}, &bytes.Buffer{})
	tio.Interactive = true
	calls := 0
	v, err := Input(context.Background(), tio, InputOptions{Label: "Name:", Validate: func(s string) error {
		calls++
		if s == "" {
			return errors.New("required")
		}
		return nil
	}})
	if err != nil || v != "hello" || calls != 2 {
		t.Fatalf("Input = %q, %v (validate calls %d)", v, err, calls)
	}
	var errOut bytes.Buffer
	tio2 := terminal.Test(strings.NewReader("s3cr3t-value\r"), &bytes.Buffer{}, &errOut)
	tio2.Interactive = true
	v, err = Input(context.Background(), tio2, InputOptions{Label: "Token:", Secret: true})
	if err != nil || v != "s3cr3t-value" {
		t.Fatalf("secret input = %q %v", v, err)
	}
	if strings.Contains(errOut.String(), "s3cr3t-value") {
		t.Fatal("secret input was echoed to the terminal")
	}
}
