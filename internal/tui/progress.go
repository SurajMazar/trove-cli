package tui

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/SurajMazar/trove-cli/internal/services"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// maxRowsAll is the number of jobs below which every row is shown; above it
// the view aggregates to running jobs plus recent completions.
const maxRowsAll = 12

type progressEvent services.CloneEvent
type progressDone struct{}

type progressModel struct {
	title    string
	names    []string
	states   []services.CloneState
	recent   []int
	sp       spinner.Model
	th       *terminal.Theme
	width    int
	finished bool
}

func (m *progressModel) Init() tea.Cmd { return m.sp.Tick }

func (m *progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case progressEvent:
		if msg.Index >= 0 && msg.Index < len(m.states) {
			m.states[msg.Index] = msg.State
			switch msg.State {
			case services.CloneDone, services.CloneFailed, services.CloneSkipped:
				m.recent = append(m.recent, msg.Index)
				if len(m.recent) > 5 {
					m.recent = m.recent[len(m.recent)-5:]
				}
			}
		}
		return m, nil
	case progressDone:
		m.finished = true
		return m, tea.Quit
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *progressModel) row(i int) string {
	th := m.th
	name := truncate(m.names[i], clamp(m.width-6, 20, 120))
	switch m.states[i] {
	case services.CloneDone:
		return th.Success.Render(terminal.SymSuccess) + " " + name
	case services.CloneFailed:
		return th.Error.Render(terminal.SymError) + " " + name
	case services.CloneSkipped:
		return th.Muted.Render("− " + name + " (skipped)")
	case services.CloneRunning:
		return m.sp.View() + " " + name
	default:
		return th.Muted.Render(terminal.SymPending + " " + name)
	}
}

func (m *progressModel) counts() (done, failed, skipped, running int) {
	for _, s := range m.states {
		switch s {
		case services.CloneDone:
			done++
		case services.CloneFailed:
			failed++
		case services.CloneSkipped:
			skipped++
		case services.CloneRunning:
			running++
		}
	}
	return
}

func (m *progressModel) View() string {
	var b strings.Builder
	b.WriteString("\n" + m.th.Title.Render(m.title) + "\n\n")
	if len(m.names) <= maxRowsAll {
		for i := range m.names {
			b.WriteString(m.row(i) + "\n")
		}
	} else {
		for _, i := range m.recent {
			b.WriteString(m.row(i) + "\n")
		}
		for i, s := range m.states {
			if s == services.CloneRunning {
				b.WriteString(m.row(i) + "\n")
			}
		}
	}
	done, failed, skipped, _ := m.counts()
	total := len(m.names)
	finished := done + failed + skipped
	bar := progressBar(finished, total, clamp(m.width-30, 10, 40))
	line := fmt.Sprintf("%s  %d / %d completed", bar, finished, total)
	if failed > 0 {
		line += " · " + m.th.Error.Render(fmt.Sprintf("%d failed", failed))
	}
	if skipped > 0 {
		line += fmt.Sprintf(" · %d skipped", skipped)
	}
	b.WriteString("\n" + line + "\n")
	return b.String()
}

func progressBar(n, total, width int) string {
	if total == 0 {
		return ""
	}
	filled := n * width / total
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// CloneProgress consumes clone events and renders progress until events is
// closed. In non-interactive sessions it prints one line per finished
// repository to w instead of animating.
func CloneProgress(ctx context.Context, t *terminal.IO, title string, names []string, events <-chan services.CloneEvent) {
	if !spinnerAllowed(t) {
		plainProgress(t.Err, t.ErrTheme(), events)
		return
	}
	th := t.ErrTheme()
	m := &progressModel{title: title, names: names, states: make([]services.CloneState, len(names)),
		sp: spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(th.Accent)), th: th, width: t.Width()}
	for i := range m.states {
		m.states[i] = services.CloneQueued
	}
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(t.Err), tea.WithoutSignalHandler())
	go func() {
		for e := range events {
			p.Send(progressEvent(e))
		}
		p.Send(progressDone{})
	}()
	if _, err := p.Run(); err != nil {
		// The program stopped early (canceled); drain remaining events so the
		// cloner never blocks.
		for range events {
		}
	}
}

func plainProgress(w io.Writer, th *terminal.Theme, events <-chan services.CloneEvent) {
	for e := range events {
		switch e.State {
		case services.CloneDone:
			fmt.Fprintf(w, "%s %s\n", th.Success.Render(terminal.SymSuccess), e.Job.Repo.FullName)
		case services.CloneFailed:
			fmt.Fprintf(w, "%s %s\n", th.Error.Render(terminal.SymError), e.Job.Repo.FullName)
		case services.CloneSkipped:
			fmt.Fprintf(w, "%s %s (%s)\n", th.Muted.Render("−"), e.Job.Repo.FullName, e.Reason)
		}
	}
}
