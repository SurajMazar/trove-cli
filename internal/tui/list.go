package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// Item is one selectable row.
type Item struct {
	ID       string
	Title    string
	Subtitle string            // muted secondary text (description, host)
	Badges   []string          // short tags such as "private", "archived"
	Facets   map[string]string // filterable attributes, e.g. namespace/provider
	Value    any
}

// Facet is a filterable attribute cycled with a key.
type Facet struct {
	Key     string // key in Item.Facets
	Label   string // "Namespace"
	Binding string // single key, e.g. "f"
}

// ListOptions configure a picker.
type ListOptions struct {
	Title       string
	Context     string // right-aligned context, e.g. "◆ GitHub Personal"
	Noun        string // plural noun for counts, e.g. "repositories"
	Multi       bool
	Items       []Item
	Facets      []Facet
	Preselected map[string]bool
	// ProtocolToggle shows the clone protocol and lets "t" switch it.
	ProtocolToggle bool
	Protocol       domain.GitProtocol
	// ConfirmLabel names the Enter action ("Clone", "Open", "Select").
	ConfirmLabel string
	// EscBack is set when the picker was opened from another screen (the
	// dashboard): esc then means "back" while q still quits Trove.
	EscBack bool
}

// ListResult is the picker outcome.
type ListResult struct {
	Selected []Item
	Protocol domain.GitProtocol
}

// RunList shows a single- or multi-select picker. It returns ErrAborted if
// the user quits.
func RunList(ctx context.Context, t *terminal.IO, o ListOptions) (ListResult, error) {
	if len(o.Items) == 0 {
		return ListResult{}, errs.New(errs.ErrNotFound, "no %s to choose from", nounOr(o.Noun, "items"))
	}
	m := newListModel(t.ErrTheme(), t.Width(), o)
	final, err := run(ctx, t, m)
	if err != nil {
		return ListResult{}, err
	}
	lm := final.(*listModel)
	if lm.quit {
		return ListResult{}, &errs.Error{Kind: errs.ErrAborted, Cause: ErrQuit, Message: "quit"}
	}
	if lm.aborted {
		return ListResult{}, errs.New(errs.ErrAborted, "selection canceled")
	}
	return ListResult{Selected: lm.result(), Protocol: lm.protocol}, nil
}

func nounOr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

type listModel struct {
	o        ListOptions
	th       *terminal.Theme
	width    int
	height   int
	cursor   int // index into filtered
	offset   int // first visible row
	selected map[string]bool
	filtered []int // indexes into o.Items
	query    string
	search   bool
	facetSel map[string]string // facet key -> chosen value ("" = all)
	facetVal map[string][]string
	protocol domain.GitProtocol
	done     bool
	aborted  bool // esc: leave this screen
	quit     bool // q / ctrl+c: leave the program
}

func newListModel(th *terminal.Theme, width int, o ListOptions) *listModel {
	m := &listModel{o: o, th: th, width: width, height: 24, selected: map[string]bool{},
		facetSel: map[string]string{}, facetVal: map[string][]string{}, protocol: o.Protocol}
	if m.protocol == "" {
		m.protocol = domain.ProtocolHTTPS
	}
	for k, v := range o.Preselected {
		if v {
			m.selected[k] = true
		}
	}
	for _, f := range o.Facets {
		seen := map[string]bool{}
		for _, it := range o.Items {
			if v := it.Facets[f.Key]; v != "" && !seen[v] {
				seen[v] = true
				m.facetVal[f.Key] = append(m.facetVal[f.Key], v)
			}
		}
		sort.Strings(m.facetVal[f.Key])
	}
	m.refilter()
	return m
}

func (m *listModel) Init() tea.Cmd { return nil }

// pageSize is how many rows fit.
func (m *listModel) pageSize() int {
	chrome := 8
	if m.search || m.query != "" {
		chrome++
	}
	if len(m.o.Facets) > 0 {
		chrome++
	}
	return clamp(m.height-chrome, 3, 50)
}

func (m *listModel) refilter() {
	q := strings.ToLower(strings.TrimSpace(m.query))
	m.filtered = m.filtered[:0]
	for i, it := range m.o.Items {
		ok := true
		for k, v := range m.facetSel {
			if v != "" && it.Facets[k] != v {
				ok = false
				break
			}
		}
		if ok && q != "" {
			hay := strings.ToLower(it.Title + " " + it.Subtitle + " " + strings.Join(it.Badges, " "))
			for _, v := range it.Facets {
				hay += " " + strings.ToLower(v)
			}
			for _, term := range strings.Fields(q) {
				if !strings.Contains(hay, term) {
					ok = false
					break
				}
			}
		}
		if ok {
			m.filtered = append(m.filtered, i)
		}
	}
	m.cursor = clamp(m.cursor, 0, max(len(m.filtered)-1, 0))
	m.fixOffset()
}

func (m *listModel) fixOffset() {
	ps := m.pageSize()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+ps {
		m.offset = m.cursor - ps + 1
	}
	m.offset = clamp(m.offset, 0, max(len(m.filtered)-ps, 0))
}

func (m *listModel) current() *Item {
	if len(m.filtered) == 0 {
		return nil
	}
	return &m.o.Items[m.filtered[m.cursor]]
}

func (m *listModel) result() []Item {
	var out []Item
	if !m.o.Multi {
		if c := m.current(); c != nil {
			out = append(out, *c)
		}
		return out
	}
	for _, it := range m.o.Items {
		if m.selected[it.ID] {
			out = append(out, it)
		}
	}
	return out
}

func (m *listModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.fixOffset()
		return m, nil
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" {
			m.aborted, m.quit = true, true
			return m, tea.Quit
		}
		if m.search {
			switch key {
			case "esc", "enter":
				// Leave search mode, keeping the filter; Esc again clears it.
				m.search = false
			case "backspace":
				if r := []rune(m.query); len(r) > 0 {
					m.query = string(r[:len(r)-1])
					m.refilter()
				}
			case "up", "down":
				m.move(key)
			default:
				switch {
				case msg.Type == tea.KeyRunes:
					m.query += string(msg.Runes)
					m.refilter()
				case msg.Type == tea.KeySpace:
					m.query += " "
					m.refilter()
				}
			}
			return m, nil
		}
		switch key {
		case "q":
			m.aborted, m.quit = true, true
			return m, tea.Quit
		case "esc":
			if m.query != "" {
				m.query = ""
				m.refilter()
				return m, nil
			}
			m.aborted = true
			return m, tea.Quit
		case "/":
			m.search = true
		case "up", "k", "down", "j", "pgup", "pgdown", "left", "right", "home", "end", "g", "G":
			m.move(key)
		case " ":
			if m.o.Multi {
				if c := m.current(); c != nil {
					m.selected[c.ID] = !m.selected[c.ID]
					if !m.selected[c.ID] {
						delete(m.selected, c.ID)
					}
				}
				m.move("down")
			}
		case "a", "A":
			if m.o.Multi {
				for _, i := range m.filtered {
					m.selected[m.o.Items[i].ID] = true
				}
			}
		case "n", "N":
			if m.o.Multi {
				for _, i := range m.filtered {
					delete(m.selected, m.o.Items[i].ID)
				}
			}
		case "t":
			if m.o.ProtocolToggle {
				if m.protocol == domain.ProtocolSSH {
					m.protocol = domain.ProtocolHTTPS
				} else {
					m.protocol = domain.ProtocolSSH
				}
			}
		case "enter":
			if len(m.filtered) == 0 && (!m.o.Multi || len(m.selected) == 0) {
				return m, nil
			}
			if m.o.Multi && len(m.selected) == 0 {
				if c := m.current(); c != nil {
					m.selected[c.ID] = true
				}
			}
			m.done = true
			return m, tea.Quit
		default:
			for _, f := range m.o.Facets {
				if key == f.Binding {
					m.cycleFacet(f.Key)
				}
			}
		}
	}
	return m, nil
}

func (m *listModel) cycleFacet(key string) {
	vals := m.facetVal[key]
	cur := m.facetSel[key]
	next := ""
	if cur == "" && len(vals) > 0 {
		next = vals[0]
	} else {
		for i, v := range vals {
			if v == cur && i+1 < len(vals) {
				next = vals[i+1]
			}
		}
	}
	m.facetSel[key] = next
	m.cursor, m.offset = 0, 0
	m.refilter()
}

func (m *listModel) move(key string) {
	ps := m.pageSize()
	n := len(m.filtered)
	switch key {
	case "up", "k":
		m.cursor--
	case "down", "j":
		m.cursor++
	case "pgup", "left":
		m.cursor -= ps
		m.offset -= ps
	case "pgdown", "right":
		m.cursor += ps
		m.offset += ps
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = n - 1
	}
	m.cursor = clamp(m.cursor, 0, max(n-1, 0))
	m.fixOffset()
}

func (m *listModel) View() string {
	if m.done || m.aborted {
		return ""
	}
	th := m.th
	w := clamp(m.width-2, 30, 160)
	var b strings.Builder
	title := th.Title.Render(m.o.Title)
	if m.o.Context != "" {
		gap := w - len([]rune(m.o.Title)) - len([]rune(m.o.Context)) - 2
		if gap > 1 {
			title += strings.Repeat(" ", gap) + th.Provider.Render(m.o.Context)
		}
	}
	b.WriteString("\n" + title + "\n\n")
	if m.search || m.query != "" {
		cursor := ""
		if m.search {
			cursor = th.Accent.Render("▏")
		}
		b.WriteString("  " + th.Muted.Render("Search: ") + m.query + cursor + "\n")
	}
	if len(m.o.Facets) > 0 {
		var fs []string
		for _, f := range m.o.Facets {
			v := m.facetSel[f.Key]
			if v == "" {
				v = "all"
			}
			fs = append(fs, th.Muted.Render(f.Label+":")+" "+v)
		}
		b.WriteString("  " + strings.Join(fs, "   ") + "\n")
	}
	if m.search || m.query != "" || len(m.o.Facets) > 0 {
		b.WriteString("\n")
	}

	ps := m.pageSize()
	end := min(m.offset+ps, len(m.filtered))
	if len(m.filtered) == 0 {
		b.WriteString("  " + th.Muted.Render("No matches") + "\n")
	}
	for row := m.offset; row < end; row++ {
		it := m.o.Items[m.filtered[row]]
		isCur := row == m.cursor
		prefix := "  "
		if isCur {
			prefix = th.Accent.Render(terminal.SymCursor) + " "
		}
		mark := ""
		if m.o.Multi {
			if m.selected[it.ID] {
				mark = th.Selected.Render(terminal.SymSelected) + " "
			} else {
				mark = th.Muted.Render(terminal.SymUnsel) + " "
			}
		}
		titleW := w - 6
		meta := it.Subtitle
		if len(it.Badges) > 0 {
			if meta != "" {
				meta += " · "
			}
			meta += strings.Join(it.Badges, " · ")
		}
		name := it.Title
		nameW := len([]rune(name))
		metaW := titleW - nameW - 3
		line := name
		if isCur {
			line = th.Selected.Render(truncate(name, titleW))
		} else {
			line = truncate(name, titleW)
		}
		if meta != "" && metaW > 8 {
			line += "   " + th.Muted.Render(truncate(meta, metaW))
		}
		b.WriteString(prefix + mark + line + "\n")
	}

	// Status line.
	var status []string
	if m.o.Multi {
		status = append(status, th.Bold.Render(fmt.Sprintf("%d selected", len(m.selected))))
	}
	count := fmt.Sprintf("%d %s", len(m.filtered), nounOr(m.o.Noun, "items"))
	if len(m.filtered) != len(m.o.Items) {
		count = fmt.Sprintf("%d of %d %s", len(m.filtered), len(m.o.Items), nounOr(m.o.Noun, "items"))
	}
	status = append(status, count)
	if pages := (len(m.filtered) + ps - 1) / ps; pages > 1 {
		status = append(status, fmt.Sprintf("page %d/%d", m.cursor/ps+1, pages))
	}
	if m.o.ProtocolToggle {
		status = append(status, strings.ToUpper(string(m.protocol)))
	}
	b.WriteString("\n  " + th.Muted.Render(strings.Join(status, " · ")) + "\n\n")

	confirm := m.o.ConfirmLabel
	if confirm == "" {
		confirm = "select"
	}
	if m.search {
		// While typing a search, only these keys do something special.
		b.WriteString("  " + helpLine(th, w-2, [2]string{"type", "to filter"}, [2]string{"↑↓", "navigate"},
			[2]string{"enter", "done"}, [2]string{"esc", "done"}) + "\n")
		return b.String()
	}
	pairs := [][2]string{{"↑↓", "navigate"}, {"/", "search"}}
	if m.o.Multi {
		pairs = append(pairs, [2]string{"space", "select"}, [2]string{"a", "all"}, [2]string{"n", "none"})
	}
	for _, f := range m.o.Facets {
		pairs = append(pairs, [2]string{f.Binding, strings.ToLower(f.Label)})
	}
	if m.o.ProtocolToggle {
		pairs = append(pairs, [2]string{"t", "protocol"})
	}
	pairs = append(pairs, [2]string{"enter", strings.ToLower(confirm)})
	if m.query != "" {
		// Esc clears an active filter first, then leaves.
		pairs = append(pairs, [2]string{"esc", "clear search"}, [2]string{"q", "quit"})
	} else if m.o.EscBack {
		pairs = append(pairs, [2]string{"esc", "back"}, [2]string{"q", "quit"})
	} else {
		pairs = append(pairs, [2]string{"q/esc", "quit"})
	}
	b.WriteString("  " + helpLine(th, w-2, pairs...) + "\n")
	return b.String()
}
