package cli

import (
	"context"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/terminal"
	"github.com/SurajMazar/trove-cli/internal/tui"
)

// runDashboard shows the root dashboard (interactive `trove` with no args).
func runDashboard(ctx context.Context, f *Factory, a *app.App) error {
	for {
		if len(a.Config.Providers) == 0 {
			th := a.IO.ErrTheme()
			fmt.Fprintf(a.IO.Err, "%s\n\n", th.Title.Render(terminal.SymProvider+" TROVE"))
			fmt.Fprintln(a.IO.Err, "No providers are configured yet.")
			ok, err := a.IO.Confirm("Add one now?")
			if err != nil || !ok {
				fmt.Fprintln(a.IO.Err, "Run "+th.Accent.Render("trove provider add")+" when you're ready.")
				return nil
			}
			if err := providerAdd(ctx, f, a, "", providerAddFlags{}); err != nil {
				return err
			}
			continue
		}
		alias, err := a.ProviderName("")
		if err != nil {
			if err := pickProvider(ctx, a); err != nil {
				return err
			}
			if err := useProvider(a, a.Opts.Provider); err != nil {
				return err
			}
			continue
		}
		p, err := a.Open(alias)
		if err != nil {
			return err
		}
		m := p.Metadata()
		long, _ := prTerms(p)
		initial := tui.DashboardData{ProviderLabel: a.Label(alias), ProviderType: m.DisplayName, Host: m.Host}
		action, err := tui.Dashboard(ctx, a.IO, initial, "Open "+long+"s", func(ctx context.Context) tui.DashboardData {
			return loadDashboard(ctx, p, initial)
		})
		if err != nil {
			return err
		}
		switch action {
		case tui.ActionQuit, "":
			return nil
		case tui.ActionRepositories:
			return runClone(ctx, a, nil, cloneFlags{retries: -1}, false)
		case tui.ActionPullRequests:
			return runSub(ctx, f, a, "pr", "list")
		case tui.ActionIssues:
			return runSub(ctx, f, a, "issue", "list")
		case tui.ActionPipelines:
			return runSub(ctx, f, a, "pipeline", "list")
		case tui.ActionSettings:
			return runSub(ctx, f, a, "provider", "show", alias)
		case tui.ActionProviders:
			if err := pickProvider(ctx, a); err != nil {
				if errs.ExitCode(err) == 130 {
					continue // back to the dashboard
				}
				return err
			}
			if err := useProvider(a, a.Opts.Provider); err != nil {
				return err
			}
			a.Opts.Provider = ""
		}
	}
}

func loadDashboard(ctx context.Context, p forge.Provider, d tui.DashboardData) tui.DashboardData {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u, err := p.CurrentUser(ctx)
	if err != nil {
		d.Err = err
		return d
	}
	d.User = u
	if sp, err := forge.As[forge.SummaryProvider](p, forge.CapSummary); err == nil {
		if s, err := sp.Summary(ctx); err == nil {
			d.Summary = s
		}
	}
	if d.Summary == nil {
		d.Summary = &domain.AccountSummary{}
	}
	return d
}

// runSub runs another command line with the same factory (shared App).
func runSub(ctx context.Context, f *Factory, a *app.App, args ...string) error {
	if args[0] != "provider" && !a.Git.IsRepository(ctx, ".") {
		fmt.Fprintln(a.IO.Err, "Run this from inside a repository, or use: trove "+args[0]+" list --repo namespace/name")
		return nil
	}
	f.interactive = false
	root := NewRoot(f)
	root.SetArgs(args)
	root.SetOut(a.IO.Out)
	root.SetErr(a.IO.Err)
	return root.ExecuteContext(ctx)
}

// yamlToJSON converts YAML bytes into JSON-marshalable values.
func yamlToJSON(b []byte) (any, error) {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}
