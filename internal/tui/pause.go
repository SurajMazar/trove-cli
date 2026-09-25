package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/SurajMazar/trove-cli/internal/terminal"
)

type pauseModel struct {
	label string
	th    *terminal.Theme
	back  bool
	done  bool
}

func (m *pauseModel) Init() tea.Cmd { return nil }

func (m *pauseModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "enter", "esc", "backspace", "left", "b", " ":
			m.back, m.done = true, true
			return m, tea.Quit
		case "q", "ctrl+c":
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *pauseModel) View() string {
	if m.done {
		return ""
	}
	return "\n  " + helpLine(m.th, 80, [2]string{"enter/esc", "back to " + m.label}, [2]string{"q", "quit"}) + "\n"
}

// Pause waits after a view finishes so the user can read it, then reports
// whether they want to go back (enter/esc) or quit (q).
func Pause(ctx context.Context, t *terminal.IO, backTo string) (bool, error) {
	final, err := run(ctx, t, &pauseModel{label: backTo, th: t.ErrTheme()})
	if err != nil {
		return false, err
	}
	return final.(*pauseModel).back, nil
}
