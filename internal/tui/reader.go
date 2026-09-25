package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// Document is something to read in the full-screen reader, e.g. an issue.
type Document struct {
	Title    string
	Subtitle string      // e.g. "#12 · open · by alice · 2d ago"
	Fields   [][2]string // label/value pairs shown under the title
	Body     string      // Markdown-ish text; rendered with light formatting
	Context  string      // right-aligned context, e.g. "◆ GitHub Personal"
}

type readerModel struct {
	doc      Document
	th       *terminal.Theme
	vp       viewport.Model
	width    int
	height   int
	ready    bool
	back     bool
	quit     bool
	backText string
}

const readerChrome = 3 // title line, separator, help line

func (m *readerModel) Init() tea.Cmd { return nil }

func (m *readerModel) layout() {
	w := clamp(m.width, 20, 120)
	h := m.height - readerChrome
	if h < 3 {
		h = 3
	}
	if !m.ready {
		m.vp = viewport.New(w, h)
		m.ready = true
	} else {
		m.vp.Width, m.vp.Height = w, h
	}
	m.vp.SetContent(renderDocument(m.doc, w, m.th))
}

func (m *readerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Some environments report 0x0; keep the previous size then.
		if msg.Width > 0 && msg.Height > 0 {
			m.width, m.height = msg.Width, msg.Height
			m.layout()
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "esc", "backspace", "left", "h":
			m.back = true
			return m, tea.Quit
		case "q", "ctrl+c":
			m.quit = true
			return m, tea.Quit
		case "g", "home":
			m.vp.GotoTop()
			return m, nil
		case "G", "end":
			m.vp.GotoBottom()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m *readerModel) View() string {
	if m.back || m.quit || !m.ready {
		return ""
	}
	th := m.th
	w := m.vp.Width
	title := th.Title.Render(truncate(m.doc.Title, w-lipgloss.Width(m.doc.Context)-2))
	if m.doc.Context != "" {
		if gap := w - lipgloss.Width(title) - lipgloss.Width(m.doc.Context); gap > 1 {
			title += strings.Repeat(" ", gap) + th.Provider.Render(m.doc.Context)
		}
	}
	pos := "all"
	if m.vp.TotalLineCount() > m.vp.VisibleLineCount() {
		pos = fmt.Sprintf("%d%%", int(m.vp.ScrollPercent()*100))
	}
	help := helpLine(th, w, [2]string{"↑↓/pgup/pgdn", "scroll"}, [2]string{"g/G", "top/bottom"},
		[2]string{"esc", m.backText}, [2]string{"q", "quit"}) + "  " + th.Muted.Render(pos)
	return title + "\n" + th.Muted.Render(strings.Repeat("─", w)) + "\n" + m.vp.View() + "\n" + help
}

// Read shows doc full screen. It returns ErrAborted when the user goes back
// (esc) and an error wrapping ErrQuit when they quit (q).
func Read(ctx context.Context, t *terminal.IO, doc Document, backTo string) error {
	if !t.Interactive {
		return errs.New(errs.ErrInteractionRequired, "the reader needs an interactive terminal")
	}
	m := &readerModel{doc: doc, th: t.ErrTheme(), width: t.Width(), height: 24, backText: "back to " + backTo}
	m.layout()
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(t.In), tea.WithOutput(t.Err), tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		if ctx.Err() != nil {
			return errs.Wrap(errs.ErrCanceled, context.Canceled, "canceled")
		}
		return err
	}
	if final.(*readerModel).quit {
		return &errs.Error{Kind: errs.ErrAborted, Cause: ErrQuit, Message: "quit"}
	}
	return errs.New(errs.ErrAborted, "back")
}

// renderDocument lays out the header fields and a lightly formatted body:
// headings bold, quotes and code muted, long lines word-wrapped to width.
func renderDocument(d Document, width int, th *terminal.Theme) string {
	var b strings.Builder
	if d.Subtitle != "" {
		b.WriteString(th.Muted.Render(d.Subtitle) + "\n")
	}
	labelW := 0
	for _, f := range d.Fields {
		if f[1] != "" && lipgloss.Width(f[0]) > labelW {
			labelW = lipgloss.Width(f[0])
		}
	}
	for _, f := range d.Fields {
		if f[1] == "" {
			continue
		}
		label := f[0] + strings.Repeat(" ", labelW-lipgloss.Width(f[0]))
		b.WriteString(th.Muted.Render(label) + "  " + wrap(f[1], width-labelW-2, strings.Repeat(" ", labelW+2)) + "\n")
	}
	b.WriteString("\n")
	body := strings.TrimSpace(strings.ReplaceAll(d.Body, "\r\n", "\n"))
	if body == "" {
		b.WriteString(th.Muted.Render("No description provided.") + "\n")
		return b.String()
	}
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			inFence = !inFence
			b.WriteString(th.Muted.Render(truncate(line, width)) + "\n")
		case inFence:
			b.WriteString(th.Accent.Render(truncate(line, width)) + "\n")
		case strings.HasPrefix(trimmed, "#"):
			b.WriteString(th.Heading.Render(wrap(strings.TrimSpace(strings.TrimLeft(trimmed, "#")), width, "")) + "\n")
		case strings.HasPrefix(trimmed, ">"):
			b.WriteString(th.Muted.Render(wrap(line, width, "> ")) + "\n")
		default:
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
				indent += "  "
			}
			b.WriteString(wrap(line, width, indent) + "\n")
		}
	}
	return b.String()
}

// wrap word-wraps s to width, prefixing continuation lines with indent.
func wrap(s string, width int, indent string) string {
	if width < 10 || lipgloss.Width(s) <= width {
		return s
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
			if len(lines) > 0 {
				line = indent + word
			}
		case lipgloss.Width(line)+1+lipgloss.Width(word) > width:
			lines = append(lines, line)
			line = indent + word
		default:
			line += " " + word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
