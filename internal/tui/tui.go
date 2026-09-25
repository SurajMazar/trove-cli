// Package tui contains Trove's reusable Bubble Tea components: list and
// multi-select pickers with search/filter/pagination, text and secret
// inputs, a spinner, bulk-clone progress and the root dashboard.
//
// Components render to stderr so stdout stays clean for pipes, never talk to
// forge providers directly (callers pass data or loader funcs in), and
// degrade to plain output when the session is not interactive.
package tui

import (
	"context"
	"errors"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// run executes a Bubble Tea program on the IO's streams, translating context
// cancellation and Ctrl+C into Trove errors.
func run(ctx context.Context, t *terminal.IO, m tea.Model) (tea.Model, error) {
	if !t.Interactive {
		return nil, errs.New(errs.ErrInteractionRequired, "an interactive terminal is required for this view")
	}
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(t.In), tea.WithOutput(t.Err))
	final, err := p.Run()
	if err != nil {
		if errors.Is(err, tea.ErrProgramKilled) || ctx.Err() != nil {
			return nil, errs.Wrap(errs.ErrCanceled, context.Canceled, "canceled")
		}
		return nil, err
	}
	return final, nil
}

// runOutputOnly runs a program that takes no keyboard input (spinners,
// progress). It does not put the terminal into raw mode, so Ctrl+C reaches
// the process signal handler and cancels ctx.
func runOutputOnly(ctx context.Context, w io.Writer, m tea.Model) (tea.Model, error) {
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(w), tea.WithoutSignalHandler())
	final, err := p.Run()
	if err != nil && (errors.Is(err, tea.ErrProgramKilled) || ctx.Err() != nil) {
		return final, nil
	}
	return final, err
}

// truncate cuts s to w display cells with an ellipsis.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return runewidth.Truncate(s, w, "…")
}

// helpLine renders "key action" pairs, wrapping to width.
func helpLine(th *terminal.Theme, width int, pairs ...[2]string) string {
	var parts []string
	for _, p := range pairs {
		parts = append(parts, th.Key.Render(p[0])+" "+th.Muted.Render(p[1]))
	}
	var lines []string
	line := ""
	for _, part := range parts {
		if line != "" && lipgloss.Width(line)+3+lipgloss.Width(part) > width {
			lines = append(lines, line)
			line = part
			continue
		}
		if line == "" {
			line = part
		} else {
			line += "   " + part
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n  ")
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
