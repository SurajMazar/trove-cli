package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/terminal"
	"github.com/SurajMazar/trove-cli/internal/tui"
)

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// repoFlag adds -R/--repo to a command.
func repoFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVarP(dst, "repo", "R", "", "repository as [provider:]namespace/name (default: detected from the current directory)")
}

// limitFlag adds --limit.
func limitFlag(cmd *cobra.Command, dst *int, def int) {
	cmd.Flags().IntVarP(dst, "limit", "L", def, "maximum number of results (0 = all)")
}

// resolveRepo resolves -R/--repo or the current directory's remote.
func resolveRepo(ctx context.Context, a *app.App, repo string) (forge.Provider, domain.RepositoryRef, error) {
	return a.ResolveRepo(ctx, a.Opts.Provider, repo)
}

// capability fetches a capability implementation or a typed error.
func capability[T any](p forge.Provider, c forge.Capability) (T, error) {
	return forge.As[T](p, c)
}

// confirm asks for confirmation unless --yes was given.
func confirm(a *app.App, question string) error {
	if a.Opts.Yes {
		return nil
	}
	ok, err := a.IO.Confirm(question)
	if err != nil {
		return err
	}
	if !ok {
		return errs.New(errs.ErrAborted, "aborted")
	}
	return nil
}

// spin runs fn with a spinner in human interactive mode.
func spin[T any](ctx context.Context, a *app.App, label string, fn func(context.Context) (T, error)) (T, error) {
	if !a.Out.Human() {
		return fn(ctx)
	}
	return tui.SpinValue(ctx, a.IO, label, fn)
}

// parseNumber parses a PR/issue number argument ("#12" or "12").
func parseNumber(s, what string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	if err != nil || n <= 0 {
		return 0, errs.New(errs.ErrInvalidArgument, "invalid %s number %q", what, s)
	}
	return n, nil
}

// providerHeader prints "◆ GitHub Personal · owner/repo" above human output.
func providerHeader(a *app.App, p forge.Provider, extra string) {
	if !a.Out.Human() {
		return
	}
	th := a.Out.Theme()
	m := p.Metadata()
	line := th.Provider.Render(terminal.SymProvider+" "+a.Label(m.Name)) + th.Muted.Render("  "+m.DisplayName)
	if extra != "" {
		line += th.Muted.Render(" · ") + extra
	}
	fmt.Fprintln(a.IO.Out, line)
	fmt.Fprintln(a.IO.Out)
}

// pickProvider lets the user choose the provider for this invocation.
func pickProvider(ctx context.Context, a *app.App) error {
	names := a.Config.ProviderNames()
	if len(names) == 0 {
		return &errs.Error{Kind: errs.ErrProviderNotFound, Message: "no providers are configured", Hint: "trove provider add"}
	}
	if !a.IO.Interactive {
		return errs.New(errs.ErrInteractionRequired, "--interactive needs a terminal; use --provider instead")
	}
	items := providerItems(a)
	res, err := tui.RunList(ctx, a.IO, tui.ListOptions{Title: "Select provider", Items: items, Noun: "providers", ConfirmLabel: "use"})
	if err != nil {
		return err
	}
	a.Opts.Provider = res.Selected[0].ID
	return nil
}

func providerItems(a *app.App) []tui.Item {
	var items []tui.Item
	for _, n := range a.Config.ProviderNames() {
		pc := a.Config.Providers[n]
		var badges []string
		if n == a.Config.DefaultProvider {
			badges = append(badges, "default")
		}
		items = append(items, tui.Item{ID: n, Title: pc.Label(n), Subtitle: fmt.Sprintf("%s · %s", pc.Type, pc.Host), Badges: badges})
	}
	return items
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseVars parses KEY=VALUE pairs.
func parseVars(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, errs.New(errs.ErrInvalidArgument, "invalid variable %q (want KEY=VALUE)", p)
		}
		out[k] = v
	}
	return out, nil
}

// stateStyle colors normalized states.
func stateText(th *terminal.Theme, s string) string {
	switch s {
	case "open", "opened", "running", "pending":
		return th.Success.Render(s)
	case "merged":
		return th.Primary.Render(s)
	case "closed", "canceled", "skipped":
		return th.Muted.Render(s)
	case "failed":
		return th.Error.Render(s)
	case "success":
		return th.Success.Render(s)
	}
	return s
}

func pipelineSymbol(th *terminal.Theme, s domain.PipelineStatus) string {
	switch s {
	case domain.PipelineSuccess:
		return th.Success.Render(terminal.SymSuccess)
	case domain.PipelineFailed:
		return th.Error.Render(terminal.SymError)
	case domain.PipelineRunning:
		return th.Accent.Render("●")
	case domain.PipelinePending, domain.PipelineManual:
		return th.Warning.Render("○")
	case domain.PipelineCanceled, domain.PipelineSkipped:
		return th.Muted.Render("−")
	}
	return th.Muted.Render("?")
}

// isSelected reports whether alias is this invocation's selected provider
// (the one generic environment credentials such as TROVE_TOKEN apply to).
func isSelected(a *app.App, alias string) bool {
	if a.Opts.Provider != "" {
		return alias == a.Opts.Provider
	}
	return alias == a.Config.DefaultProvider || len(a.Config.Providers) == 1
}

// expandHome expands a leading "~/" in user-supplied paths.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}
