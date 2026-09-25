package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// DashboardData is what the dashboard shows. Loader fills it asynchronously.
type DashboardData struct {
	ProviderLabel string // "GitHub Personal"
	ProviderType  string // "GitHub"
	Host          string
	User          *domain.User
	Summary       *domain.AccountSummary
	Err           error
}

// DashboardAction is the menu entry chosen.
type DashboardAction string

const (
	ActionRepositories DashboardAction = "repositories"
	ActionPullRequests DashboardAction = "pull-requests"
	ActionIssues       DashboardAction = "issues"
	ActionPipelines    DashboardAction = "pipelines"
	ActionProviders    DashboardAction = "providers"
	ActionSettings     DashboardAction = "settings"
	ActionQuit         DashboardAction = "quit"
)

type menuEntry struct {
	action DashboardAction
	label  string
	hint   string
}

var dashboardMenu = []menuEntry{
	{ActionRepositories, "Repositories", "browse and clone"},
	{ActionPullRequests, "Pull Requests", "pick a repository"},
	{ActionIssues, "Issues", "pick a repository"},
	{ActionPipelines, "Pipelines", "pick a repository"},
	{ActionProviders, "Providers", "switch account"},
	{ActionSettings, "Settings", "configuration"},
	{ActionQuit, "Quit", ""},
}

type dashLoaded struct{ data DashboardData }

type dashModel struct {
	menu    []menuEntry
	hidden  map[DashboardAction]bool
	data    DashboardData
	loading bool
	load    func(context.Context) DashboardData
	ctx     context.Context
	cursor  int
	sp      spinner.Model
	th      *terminal.Theme
	width   int
	chosen  DashboardAction
	prTerm  string
}

func (m *dashModel) Init() tea.Cmd {
	return tea.Batch(m.sp.Tick, func() tea.Msg { return dashLoaded{m.load(m.ctx)} })
}

func (m *dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case dashLoaded:
		label, typ, host := m.data.ProviderLabel, m.data.ProviderType, m.data.Host
		m.data = msg.data
		if m.data.ProviderLabel == "" {
			m.data.ProviderLabel, m.data.ProviderType, m.data.Host = label, typ, host
		}
		m.loading = false
		return m, nil
	case spinner.TickMsg:
		if !m.loading {
			return m, nil
		}
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.chosen = ActionQuit
			return m, tea.Quit
		case "up", "k":
			m.cursor = (m.cursor - 1 + len(m.menu)) % len(m.menu)
		case "down", "j", "tab":
			m.cursor = (m.cursor + 1) % len(m.menu)
		case "enter", " ":
			m.chosen = m.menu[m.cursor].action
			return m, tea.Quit
		case "r":
			m.chosen = ActionRepositories
			return m, tea.Quit
		case "p":
			m.chosen = ActionProviders
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *dashModel) count(v *int, suffix string) string {
	if m.loading {
		return m.sp.View()
	}
	if v == nil {
		return m.th.Muted.Render("—")
	}
	s := fmt.Sprintf("%d", *v)
	if suffix != "" {
		s += " " + suffix
	}
	return s
}

func (m *dashModel) View() string {
	if m.chosen != "" {
		return ""
	}
	th := m.th
	inner := clamp(m.width-4, 44, 60)
	rule := th.Muted.Render(strings.Repeat("─", inner))
	var b strings.Builder
	b.WriteString(th.Title.Render(terminal.SymProvider+" TROVE") + "\n")
	b.WriteString(th.Muted.Render("Developer Repository Manager") + "\n")
	b.WriteString(rule + "\n\n")

	kv := func(k, v string) { b.WriteString(fmt.Sprintf("%-13s%s\n", th.Muted.Render(pad13(k)), v)) }
	provider := th.Provider.Render(m.data.ProviderLabel)
	if m.data.ProviderType != "" {
		provider += th.Muted.Render("  " + m.data.ProviderType)
	}
	kv("Provider", provider)
	switch {
	case m.loading:
		kv("Account", m.sp.View())
	case m.data.User != nil:
		kv("Account", m.data.User.Display())
	case m.data.Err != nil:
		kv("Account", th.Warning.Render(terminal.SymWarning+" "+shortErr(m.data.Err)))
	}
	if m.data.Host != "" {
		kv("Host", m.data.Host)
	}
	b.WriteString("\n")
	s := m.data.Summary
	if s == nil {
		s = &domain.AccountSummary{}
	}
	pr := m.prTerm
	if pr == "" {
		pr = "Open PRs"
	}
	if !m.hidden[ActionRepositories] {
		kv("Repositories", m.count(s.Repositories, ""))
	}
	if !m.hidden[ActionPullRequests] {
		kv(pr, m.count(s.OpenPullRequests, ""))
	}
	if !m.hidden[ActionIssues] {
		kv("Issues", m.count(s.OpenIssues, ""))
	}
	if !m.hidden[ActionPipelines] && (s.RunningPipelines != nil || m.loading) {
		kv("Pipelines", m.count(s.RunningPipelines, "running"))
	}
	if s.Unread != nil {
		kv("Unread", m.count(s.Unread, ""))
	}
	b.WriteString("\n" + rule + "\n")
	for i, e := range m.menu {
		cursor := "  "
		label := e.label
		if i == m.cursor {
			cursor = th.Accent.Render(terminal.SymCursor) + " "
			label = th.Selected.Render(label)
		}
		line := cursor + label
		if e.hint != "" && i == m.cursor {
			line += "  " + th.Muted.Render(e.hint)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + helpLine(th, inner, [2]string{"↑↓", "navigate"}, [2]string{"enter", "select"}, [2]string{"q", "quit"}))
	panel := th.Panel.Width(inner + 2).Render(b.String())
	return "\n" + panel + "\n"
}

func pad13(s string) string {
	if w := lipgloss.Width(s); w < 13 {
		return s + strings.Repeat(" ", 13-w)
	}
	return s
}

func shortErr(err error) string {
	s := err.Error()
	if h := errs.HintOf(err); h != "" {
		s += " — " + h
	}
	return truncate(s, 48)
}

// DashboardOptions tune the menu.
type DashboardOptions struct {
	// PRTerm labels the pull request count ("Open MRs"); PRLabel names the
	// menu entry ("Merge Requests").
	PRTerm  string
	PRLabel string
	// Hidden actions are omitted (e.g. Issues for a provider without issues).
	Hidden map[DashboardAction]bool
	// RepoHint describes which repository per-repository actions use, e.g.
	// "acme/api" when run inside a checkout.
	RepoHint string
}

// Dashboard shows the root dashboard and returns the chosen action.
func Dashboard(ctx context.Context, t *terminal.IO, initial DashboardData, o DashboardOptions, load func(context.Context) DashboardData) (DashboardAction, error) {
	th := t.ErrTheme()
	prTerm := o.PRTerm
	var menu []menuEntry
	for _, e := range dashboardMenu {
		if o.Hidden[e.action] {
			continue
		}
		if o.RepoHint != "" && (e.action == ActionPullRequests || e.action == ActionIssues || e.action == ActionPipelines) {
			e.hint = o.RepoHint
		}
		if e.action == ActionPullRequests && o.PRLabel != "" {
			e.label = o.PRLabel
		}
		menu = append(menu, e)
	}
	m := &dashModel{menu: menu, hidden: o.Hidden, data: initial, loading: true, load: load, ctx: ctx, th: th, width: t.Width(), prTerm: prTerm,
		sp: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(th.Muted))}
	final, err := run(ctx, t, m)
	if err != nil {
		return ActionQuit, err
	}
	return final.(*dashModel).chosen, nil
}
