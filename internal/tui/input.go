package tui

import (
	"context"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// InputOptions configure a text prompt.
type InputOptions struct {
	Label       string
	Placeholder string
	Default     string
	Help        string
	Secret      bool // mask input; the value is never rendered
	Validate    func(string) error
}

type inputModel struct {
	o       InputOptions
	ti      textinput.Model
	th      *terminal.Theme
	err     error
	done    bool
	aborted bool
}

func (m *inputModel) Init() tea.Cmd { return textinput.Blink }

func (m *inputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "ctrl+c", "esc":
			m.aborted = true
			return m, tea.Quit
		case "enter":
			v := m.ti.Value()
			if v == "" {
				v = m.o.Default
			}
			if m.o.Validate != nil {
				if err := m.o.Validate(v); err != nil {
					m.err = err
					return m, nil
				}
			}
			m.ti.SetValue(v)
			m.done = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.ti, cmd = m.ti.Update(msg)
	m.err = nil
	return m, cmd
}

func (m *inputModel) View() string {
	th := m.th
	if m.done {
		shown := m.ti.Value()
		if m.o.Secret {
			shown = th.Muted.Render("(hidden)")
		}
		return th.Success.Render("?") + " " + th.Bold.Render(m.o.Label) + " " + shown + "\n"
	}
	if m.aborted {
		return ""
	}
	s := th.Accent.Render("?") + " " + th.Bold.Render(m.o.Label) + "\n  " + m.ti.View() + "\n"
	if m.err != nil {
		s += "  " + th.Error.Render(terminal.SymError+" "+m.err.Error()) + "\n"
	} else if m.o.Help != "" {
		s += "  " + th.Muted.Render(m.o.Help) + "\n"
	}
	return s
}

// Input prompts for a line of text.
func Input(ctx context.Context, t *terminal.IO, o InputOptions) (string, error) {
	ti := textinput.New()
	ti.Placeholder = o.Placeholder
	if o.Placeholder == "" {
		ti.Placeholder = o.Default
	}
	ti.Prompt = "› "
	ti.CharLimit = 4096
	ti.Width = clamp(t.Width()-6, 20, 100)
	if o.Secret {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '•'
	}
	ti.Focus()
	m := &inputModel{o: o, ti: ti, th: t.ErrTheme()}
	final, err := run(ctx, t, m)
	if err != nil {
		return "", err
	}
	im := final.(*inputModel)
	if im.aborted {
		return "", errs.New(errs.ErrAborted, "input canceled")
	}
	return im.ti.Value(), nil
}

// Choice is an option for Choose.
type Choice struct {
	Label       string
	Description string
	Value       string
}

// Choose shows a compact single-select question ("? Provider type:").
func Choose(ctx context.Context, t *terminal.IO, question string, choices []Choice) (Choice, error) {
	items := make([]Item, len(choices))
	for i, c := range choices {
		items[i] = Item{ID: c.Value, Title: c.Label, Subtitle: c.Description, Value: c}
	}
	res, err := RunList(ctx, t, ListOptions{Title: question, Items: items, Noun: "options", ConfirmLabel: "choose"})
	if err != nil {
		return Choice{}, err
	}
	c := res.Selected[0].Value.(Choice)
	th := t.ErrTheme()
	_, _ = t.Err.Write([]byte(th.Success.Render("?") + " " + th.Bold.Render(question) + " " + c.Label + "\n"))
	return c, nil
}
