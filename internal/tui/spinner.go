package tui

import (
	"context"
	"os"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/SurajMazar/trove-cli/internal/terminal"
)

type spinDone struct{ err error }

type spinModel struct {
	sp    spinner.Model
	label string
	th    *terminal.Theme
	done  bool
}

func (m *spinModel) Init() tea.Cmd { return m.sp.Tick }

func (m *spinModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinDone:
		m.done = true
		return m, tea.Quit
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *spinModel) View() string {
	if m.done {
		return ""
	}
	return m.sp.View() + " " + m.th.Muted.Render(m.label)
}

// Spin runs fn while showing "⠋ label" on stderr when it is a terminal and
// the output mode is human. The spinner line is removed when fn returns, so
// results print on a clean line.
func Spin(ctx context.Context, t *terminal.IO, label string, fn func(ctx context.Context) error) error {
	if !spinnerAllowed(t) {
		return fn(ctx)
	}
	th := t.ErrTheme()
	sp := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(th.Accent))
	m := &spinModel{sp: sp, label: label, th: th}
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(t.Err), tea.WithoutSignalHandler())
	errc := make(chan error, 1)
	go func() {
		err := fn(ctx)
		errc <- err
		p.Send(spinDone{err: err})
	}()
	_, _ = p.Run()
	return <-errc
}

// SpinValue is Spin for functions returning a value.
func SpinValue[T any](ctx context.Context, t *terminal.IO, label string, fn func(ctx context.Context) (T, error)) (T, error) {
	var v T
	err := Spin(ctx, t, label, func(ctx context.Context) error {
		var err error
		v, err = fn(ctx)
		return err
	})
	return v, err
}

func spinnerAllowed(t *terminal.IO) bool {
	if !t.Interactive {
		return false
	}
	f, ok := t.Err.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
