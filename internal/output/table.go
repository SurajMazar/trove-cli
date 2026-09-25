package output

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// Column describes a table column.
type Column struct {
	Header string
	// MinTerminal hides the column when the terminal is narrower than this
	// (responsive layouts: 80 compact, 120 expanded, 160+ full).
	MinTerminal int
	// Flex columns absorb truncation first (e.g. descriptions, titles).
	Flex bool
	// Max caps the column width (0 = no cap).
	Max int
}

// Cell is a table cell with optional style applied after truncation.
type Cell struct {
	Text  string
	Style *lipgloss.Style
}

// C builds an unstyled cell.
func C(s string) Cell { return Cell{Text: s} }

// S builds a styled cell.
func S(s string, st lipgloss.Style) Cell { return Cell{Text: s, Style: &st} }

// Table renders rows under columns, adapting to terminal width.
type Table struct {
	Columns []Column
	Rows    [][]Cell
}

// Add appends a row.
func (t *Table) Add(cells ...Cell) { t.Rows = append(t.Rows, cells) }

const colGap = 2

// Render prints the table in human mode.
func (p *Printer) Table(t *Table) {
	if !p.Human() {
		return
	}
	fmt.Fprint(p.IO.Out, t.Render(p.IO.Width(), p.Theme().Muted, p.Theme().Bold))
}

// Render lays the table out for the given width.
func (t *Table) Render(width int, headerStyle, _ lipgloss.Style) string {
	// Choose visible columns.
	var vis []int
	for i, c := range t.Columns {
		if c.MinTerminal == 0 || width >= c.MinTerminal {
			vis = append(vis, i)
		}
	}
	if len(vis) == 0 {
		return ""
	}
	widths := make([]int, len(vis))
	for vi, ci := range vis {
		w := runewidth.StringWidth(t.Columns[ci].Header)
		for _, r := range t.Rows {
			if ci < len(r) {
				if cw := runewidth.StringWidth(r[ci].Text); cw > w {
					w = cw
				}
			}
		}
		if m := t.Columns[ci].Max; m > 0 && w > m {
			w = m
		}
		widths[vi] = w
	}
	// Shrink to fit: flex columns first, then the widest others.
	total := func() int {
		s := 0
		for _, w := range widths {
			s += w
		}
		return s + colGap*(len(widths)-1)
	}
	avail := width - 1
	for total() > avail {
		idx := -1
		for vi, ci := range vis {
			if t.Columns[ci].Flex && widths[vi] > 12 && (idx < 0 || widths[vi] > widths[idx]) {
				idx = vi
			}
		}
		if idx < 0 {
			for vi := range vis {
				if widths[vi] > 8 && (idx < 0 || widths[vi] > widths[idx]) {
					idx = vi
				}
			}
		}
		if idx < 0 {
			break
		}
		widths[idx]--
	}

	var b strings.Builder
	// Header.
	var hdr []string
	for vi, ci := range vis {
		hdr = append(hdr, pad(truncate(strings.ToUpper(t.Columns[ci].Header), widths[vi]), widths[vi], vi == len(vis)-1))
	}
	b.WriteString(headerStyle.Render(strings.Join(hdr, strings.Repeat(" ", colGap))))
	b.WriteString("\n")
	for _, r := range t.Rows {
		var line []string
		for vi, ci := range vis {
			var c Cell
			if ci < len(r) {
				c = r[ci]
			}
			txt := pad(truncate(c.Text, widths[vi]), widths[vi], vi == len(vis)-1)
			if c.Style != nil {
				// Style only the text, not the padding, to keep alignment.
				trimmed := strings.TrimRight(txt, " ")
				txt = c.Style.Render(trimmed) + txt[len(trimmed):]
			}
			line = append(line, txt)
		}
		b.WriteString(strings.TrimRight(strings.Join(line, strings.Repeat(" ", colGap)), " "))
		b.WriteString("\n")
	}
	return b.String()
}

func truncate(s string, w int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\r", "")
	if runewidth.StringWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return runewidth.Truncate(s, w, "")
	}
	return runewidth.Truncate(s, w, "…")
}

func pad(s string, w int, last bool) string {
	if last {
		return s
	}
	if d := w - runewidth.StringWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
