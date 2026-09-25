// Package output renders command results as human-readable text, tables,
// JSON or quiet (identifier-only) output.
//
// JSON mode writes exactly one valid JSON document to stdout and nothing
// decorative; progress and diagnostics go to stderr.
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/redact"
	"github.com/SurajMazar/trove-cli/internal/terminal"
)

// Mode selects the output format.
type Mode string

const (
	ModeHuman Mode = "human"
	ModeJSON  Mode = "json"
	ModeQuiet Mode = "quiet"
)

// ParseMode validates a TROVE_OUTPUT / output.format value.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "human", "table", "text":
		return ModeHuman, nil
	case "json":
		return ModeJSON, nil
	case "quiet":
		return ModeQuiet, nil
	}
	return "", fmt.Errorf("invalid output mode %q (want table, json or quiet)", s)
}

// Printer writes results.
type Printer struct {
	IO   *terminal.IO
	Mode Mode
}

// New returns a Printer.
func New(t *terminal.IO, mode Mode) *Printer { return &Printer{IO: t, Mode: mode} }

// JSON reports whether output is machine-readable.
func (p *Printer) JSON() bool { return p.Mode == ModeJSON }

// Quiet reports whether only identifiers should be printed.
func (p *Printer) Quiet() bool { return p.Mode == ModeQuiet }

// Human reports whether decorated output is allowed.
func (p *Printer) Human() bool { return p.Mode == ModeHuman }

// Theme returns stdout styles.
func (p *Printer) Theme() *terminal.Theme { return p.IO.Theme() }

// PrintJSON writes v as indented JSON to stdout.
func (p *Printer) PrintJSON(v any) error {
	enc := json.NewEncoder(p.IO.Out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Result prints v as JSON in JSON mode, the quiet lines in quiet mode, or
// calls human otherwise.
func (p *Printer) Result(v any, quiet func() []string, human func() error) error {
	switch p.Mode {
	case ModeJSON:
		return p.PrintJSON(v)
	case ModeQuiet:
		if quiet != nil {
			for _, l := range quiet() {
				fmt.Fprintln(p.IO.Out, l)
			}
		}
		return nil
	default:
		return human()
	}
}

// Println writes a plain line to stdout in human mode.
func (p *Printer) Println(a ...any) {
	if p.Human() {
		fmt.Fprintln(p.IO.Out, a...)
	}
}

// Printf writes formatted text to stdout in human mode.
func (p *Printer) Printf(format string, a ...any) {
	if p.Human() {
		fmt.Fprintf(p.IO.Out, format, a...)
	}
}

// Success prints "✓ msg" to stderr (so stdout stays clean for pipes) unless
// quiet/json.
func (p *Printer) Success(format string, a ...any) {
	p.status(terminal.SymSuccess, p.IO.ErrTheme().Success, format, a...)
}

// Warn prints "⚠ msg" to stderr (shown in all modes except quiet).
func (p *Printer) Warn(format string, a ...any) {
	if p.Mode == ModeQuiet {
		return
	}
	th := p.IO.ErrTheme()
	fmt.Fprintf(p.IO.Err, "%s %s\n", th.Warning.Render(terminal.SymWarning), fmt.Sprintf(format, a...))
}

// Info prints "• msg" to stderr in human mode.
func (p *Printer) Info(format string, a ...any) {
	p.status(terminal.SymInfo, p.IO.ErrTheme().Muted, format, a...)
}

func (p *Printer) status(sym string, st lipgloss.Style, format string, a ...any) {
	if !p.Human() {
		return
	}
	fmt.Fprintf(p.IO.Err, "%s %s\n", st.Render(sym), fmt.Sprintf(format, a...))
}

// Error renders an error in the house style:
//
//	✗ Failed to authenticate with GitLab
//
//	  The configured token is invalid or expired.
//
//	  Provider: gitlab-work
//
//	  Try:
//	    trove auth login gitlab-work
//
// In JSON mode a {"error": {...}} document is written to stderr instead.
func RenderError(t *terminal.IO, mode Mode, err error, debug bool) {
	if err == nil {
		return
	}
	msg := redact.String(err.Error())
	provider := errs.ProviderOf(err)
	hint := errs.HintOf(err)
	if mode == ModeJSON {
		doc := map[string]any{"message": msg, "code": errorCode(err), "exit_code": errs.ExitCode(err)}
		if provider != "" {
			doc["provider"] = provider
		}
		if hint != "" {
			doc["hint"] = hint
		}
		b, _ := json.Marshal(map[string]any{"error": doc})
		fmt.Fprintln(t.Err, string(b))
		return
	}
	th := t.ErrTheme()
	title, detail := splitTitle(err, msg)
	fmt.Fprintf(t.Err, "%s %s\n", th.Error.Render(terminal.SymError), th.Bold.Render(title))
	if detail != "" {
		fmt.Fprintf(t.Err, "\n  %s\n", wrapIndent(detail, t.Width()-4, "  "))
	}
	if provider != "" && !strings.Contains(msg, `"`+provider+`"`) {
		fmt.Fprintf(t.Err, "\n  %s %s\n", th.Muted.Render("Provider:"), provider)
	}
	if hint != "" {
		fmt.Fprintf(t.Err, "\n  %s\n    %s\n", th.Muted.Render("Try:"), th.Accent.Render(hint))
	}
	if debug {
		fmt.Fprintf(t.Err, "\n  %s %s\n", th.Muted.Render("debug:"), redact.String(fmt.Sprintf("%#v", err)))
	}
}

func splitTitle(err error, msg string) (string, string) {
	var title string
	switch {
	case errors.Is(err, errs.ErrAuthenticationFailed):
		title = "Authentication failed"
	case errors.Is(err, errs.ErrNotAuthenticated):
		title = "Not logged in"
	case errors.Is(err, errs.ErrUnsupportedCapability):
		title = "Not supported"
	case errors.Is(err, errs.ErrPermissionDenied):
		title = "Permission denied"
	case errors.Is(err, errs.ErrRateLimited):
		title = "Rate limited"
	case errors.Is(err, errs.ErrRepositoryNotFound):
		title = "Repository not found"
	case errors.Is(err, errs.ErrSecretProviderLocked):
		title = "Secret store is locked"
	case errors.Is(err, errs.ErrSecretUnavailable):
		title = "Secret store unavailable"
	case errors.Is(err, errs.ErrSecretNotFound):
		title = "Credential not found"
	case errors.Is(err, errs.ErrInvalidConfiguration):
		title = "Invalid configuration"
	case errors.Is(err, errs.ErrProviderNotFound):
		title = "Provider not found"
	case errors.Is(err, errs.ErrInteractionRequired):
		title = "Input required"
	case errors.Is(err, errs.ErrCanceled), errors.Is(err, errs.ErrAborted):
		title = "Canceled"
	case errors.Is(err, errs.ErrNotFound):
		title = "Not found"
	case errors.Is(err, errs.ErrGit):
		title = "Git command failed"
	default:
		return capitalize(msg), ""
	}
	return title, capitalize(msg)
}

func errorCode(err error) string {
	for _, k := range []struct {
		err  error
		code string
	}{
		{errs.ErrAuthenticationFailed, "authentication_failed"}, {errs.ErrNotAuthenticated, "not_authenticated"},
		{errs.ErrUnsupportedCapability, "unsupported"}, {errs.ErrPermissionDenied, "permission_denied"},
		{errs.ErrRateLimited, "rate_limited"}, {errs.ErrRepositoryNotFound, "repository_not_found"},
		{errs.ErrSecretNotFound, "secret_not_found"}, {errs.ErrSecretProviderLocked, "secret_provider_locked"},
		{errs.ErrSecretUnavailable, "secret_provider_unavailable"}, {errs.ErrInvalidConfiguration, "invalid_configuration"},
		{errs.ErrProviderNotFound, "provider_not_found"}, {errs.ErrDriverNotFound, "provider_type_not_supported"},
		{errs.ErrInvalidArgument, "invalid_argument"}, {errs.ErrInteractionRequired, "interaction_required"},
		{errs.ErrConflict, "conflict"}, {errs.ErrNotFound, "not_found"}, {errs.ErrCanceled, "canceled"},
		{errs.ErrAborted, "aborted"}, {errs.ErrGit, "git_failed"},
	} {
		if errors.Is(err, k.err) {
			return k.code
		}
	}
	return "error"
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func wrapIndent(s string, width int, indent string) string {
	if width < 20 {
		width = 20
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		line := ""
		for _, w := range words {
			if line != "" && lipgloss.Width(line)+1+lipgloss.Width(w) > width {
				out = append(out, line)
				line = w
				continue
			}
			if line == "" {
				line = w
			} else {
				line += " " + w
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n"+indent)
}

// KV is one row of a key/value block.
type KV struct {
	Key   string
	Value string
}

// KeyValues prints an aligned key/value block.
func (p *Printer) KeyValues(rows []KV) {
	if !p.Human() {
		return
	}
	th := p.Theme()
	w := 0
	for _, r := range rows {
		if l := lipgloss.Width(r.Key); l > w {
			w = l
		}
	}
	for _, r := range rows {
		if r.Value == "" {
			continue
		}
		pad := strings.Repeat(" ", w-lipgloss.Width(r.Key))
		fmt.Fprintf(p.IO.Out, "  %s%s  %s\n", th.Muted.Render(r.Key), pad, r.Value)
	}
}

// Heading prints a bold heading line.
func (p *Printer) Heading(s string) {
	if p.Human() {
		fmt.Fprintln(p.IO.Out, p.Theme().Heading.Render(s))
	}
}

// RelTime renders t relative to now ("2h ago"), or "" for zero.
func RelTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	future := d < 0
	if future {
		d = -d
	}
	var s string
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		s = fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		s = fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		s = fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		s = fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	default:
		s = fmt.Sprintf("%dy", int(d.Hours()/24/365))
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

// Writer returns stdout (for streaming output like logs).
func (p *Printer) Writer() io.Writer { return p.IO.Out }
