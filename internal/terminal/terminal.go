// Package terminal handles TTY detection, sizing, color policy, the shared
// visual theme and simple line-based prompts.
package terminal

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	"github.com/SurajMazar/trove-cli/internal/errs"
)

// IO bundles the process streams and terminal facts. Commands receive an IO
// rather than touching os.Stdout directly, which keeps them testable.
type IO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer

	// Interactive is true when prompts and TUIs may be shown: stdin and stdout
	// are terminals and --non-interactive was not given.
	Interactive bool
	// Color is true when ANSI styling may be emitted on Out.
	Color bool
	// ErrColor is true when ANSI styling may be emitted on Err.
	ErrColor bool

	width    int
	theme    *Theme
	once     sync.Once
	errTheme *Theme
	errOnce  sync.Once

	readerOnce sync.Once
	reader     *bufio.Reader
}

// ColorMode is the user's color preference.
type ColorMode string

const (
	ColorAuto   ColorMode = "auto"
	ColorAlways ColorMode = "always"
	ColorNever  ColorMode = "never"
)

// System returns IO bound to the real process streams.
func System(nonInteractive bool, mode ColorMode) *IO {
	stdinTTY := isTerminal(os.Stdin)
	stdoutTTY := isTerminal(os.Stdout)
	stderrTTY := isTerminal(os.Stderr)
	t := &IO{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr,
		Interactive: stdinTTY && stdoutTTY && !nonInteractive && os.Getenv("CI") == "",
		Color:       colorEnabled(mode, stdoutTTY),
		ErrColor:    colorEnabled(mode, stderrTTY),
	}
	if stdoutTTY {
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
			t.width = w
		}
	}
	return t
}

// Test returns an IO over buffers for tests (non-interactive, no color).
func Test(in io.Reader, out, errw io.Writer) *IO {
	if in == nil {
		in = strings.NewReader("")
	}
	return &IO{In: in, Out: out, Err: errw, width: 100}
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

func colorEnabled(mode ColorMode, tty bool) bool {
	switch mode {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return tty
}

// Width returns the terminal width (COLUMNS, detected size, or 80).
func (t *IO) Width() int {
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 20 {
		return c
	}
	if t.width > 0 {
		return t.width
	}
	return 80
}

// SetWidth overrides the detected width (tests).
func (t *IO) SetWidth(w int) { t.width = w }

// Theme returns styles for Out.
func (t *IO) Theme() *Theme {
	t.once.Do(func() { t.theme = NewTheme(t.Out, t.Color) })
	return t.theme
}

// ErrTheme returns styles for Err.
func (t *IO) ErrTheme() *Theme {
	t.errOnce.Do(func() { t.errTheme = NewTheme(t.Err, t.ErrColor) })
	return t.errTheme
}

func (t *IO) lineReader() *bufio.Reader {
	t.readerOnce.Do(func() { t.reader = bufio.NewReader(t.In) })
	return t.reader
}

// Confirm asks a yes/no question on Err, defaulting to "no". It returns
// ErrInteractionRequired when prompting is impossible, so destructive
// commands fail closed in scripts unless --yes is given.
func (t *IO) Confirm(question string) (bool, error) {
	if !t.Interactive {
		return false, &errs.Error{Kind: errs.ErrInteractionRequired,
			Message: "confirmation required but the session is non-interactive",
			Hint:    "re-run with --yes to confirm"}
	}
	th := t.ErrTheme()
	fmt.Fprintf(t.Err, "%s %s ", question, th.Muted.Render("[y/N]"))
	line, err := t.lineReader().ReadString('\n')
	if err != nil && line == "" {
		return false, errs.Wrap(errs.ErrAborted, err, "no answer")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// Input asks for a line of text with an optional default.
func (t *IO) Input(label, def string) (string, error) {
	if !t.Interactive {
		if def != "" {
			return def, nil
		}
		return "", &errs.Error{Kind: errs.ErrInteractionRequired, Message: fmt.Sprintf("%s is required", strings.TrimSuffix(label, ":"))}
	}
	th := t.ErrTheme()
	if def != "" {
		fmt.Fprintf(t.Err, "%s %s ", th.Bold.Render(label), th.Muted.Render("("+def+")"))
	} else {
		fmt.Fprintf(t.Err, "%s ", th.Bold.Render(label))
	}
	line, err := t.lineReader().ReadString('\n')
	if err != nil && line == "" {
		return "", errs.Wrap(errs.ErrAborted, err, "no input")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// Secret reads a secret without echo. The value is never displayed.
func (t *IO) Secret(label string) (string, error) {
	f, ok := t.In.(*os.File)
	if !t.Interactive || !ok || !isTerminal(f) {
		return "", &errs.Error{Kind: errs.ErrInteractionRequired,
			Message: "a secret is required but the session is non-interactive",
			Hint:    "pipe it with --with-token (reads stdin) or set TROVE_TOKEN"}
	}
	fmt.Fprintf(t.Err, "%s ", t.ErrTheme().Bold.Render(label))
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(t.Err)
	if err != nil {
		return "", errs.Wrap(errs.ErrAborted, err, "could not read secret")
	}
	return strings.TrimSpace(string(b)), nil
}

// ReadAllIn reads a secret or document from stdin (e.g. --with-token).
func (t *IO) ReadAllIn(limit int64) (string, error) {
	b, err := io.ReadAll(io.LimitReader(t.lineReader(), limit))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Theme is Trove's visual language. Every style degrades to plain text when
// color is disabled; meaning is always carried by symbols and words too.
type Theme struct {
	Color bool

	Title    lipgloss.Style
	Heading  lipgloss.Style
	Bold     lipgloss.Style
	Primary  lipgloss.Style
	Accent   lipgloss.Style
	Muted    lipgloss.Style
	Success  lipgloss.Style
	Warning  lipgloss.Style
	Error    lipgloss.Style
	Selected lipgloss.Style
	Provider lipgloss.Style
	Panel    lipgloss.Style
	Key      lipgloss.Style

	renderer *lipgloss.Renderer
}

// Symbols used across the UI (all have textual meaning without color).
const (
	SymSuccess  = "✓"
	SymWarning  = "⚠"
	SymError    = "✗"
	SymInfo     = "•"
	SymProvider = "◆"
	SymSelected = "◉"
	SymUnsel    = "○"
	SymCursor   = "›"
	SymPending  = "○"
)

// NewTheme builds a theme rendering to w. ANSI 16-color palette only, so it
// follows the user's terminal color scheme and keeps readable contrast.
func NewTheme(w io.Writer, color bool) *Theme {
	r := lipgloss.NewRenderer(w)
	if !color {
		r.SetColorProfile(termenv.Ascii)
	} else if r.ColorProfile() == termenv.Ascii {
		r.SetColorProfile(termenv.ANSI)
	}
	s := r.NewStyle
	primary := lipgloss.Color("5") // magenta
	accent := lipgloss.Color("6")  // cyan
	muted := lipgloss.Color("8")   // bright black
	return &Theme{
		Color:    color,
		Title:    s().Bold(true).Foreground(primary),
		Heading:  s().Bold(true),
		Bold:     s().Bold(true),
		Primary:  s().Foreground(primary),
		Accent:   s().Foreground(accent),
		Muted:    s().Foreground(muted),
		Success:  s().Foreground(lipgloss.Color("2")),
		Warning:  s().Foreground(lipgloss.Color("3")),
		Error:    s().Foreground(lipgloss.Color("1")),
		Selected: s().Bold(true).Foreground(accent),
		Provider: s().Bold(true).Foreground(primary),
		Panel:    s().Border(lipgloss.RoundedBorder()).BorderForeground(muted).Padding(0, 1),
		Key:      s().Bold(true).Foreground(accent),
		renderer: r,
	}
}

// Renderer exposes the lipgloss renderer (for TUIs).
func (th *Theme) Renderer() *lipgloss.Renderer { return th.renderer }
