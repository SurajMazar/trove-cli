package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/output"
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
			if err := pickProvider(ctx, a, false); err != nil {
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
		long, short := prTerms(p)
		caps := p.Capabilities()
		initial := tui.DashboardData{ProviderLabel: a.Label(alias), ProviderType: m.DisplayName, Host: m.Host}
		opts := tui.DashboardOptions{PRTerm: "Open " + short + "s", PRLabel: long + "s", Hidden: map[tui.DashboardAction]bool{
			tui.ActionRepositories: !caps.Has(forge.CapRepositories),
			tui.ActionPullRequests: !caps.Has(forge.CapPullRequests),
			tui.ActionIssues:       !caps.Has(forge.CapIssues),
			tui.ActionPipelines:    !caps.Has(forge.CapPipelines),
		}}
		if d, ok := checkoutRepo(ctx, a, alias); ok {
			opts.RepoHint = d
		}
		action, err := tui.Dashboard(ctx, a.IO, initial, opts, func(ctx context.Context) tui.DashboardData {
			return loadDashboard(ctx, p, initial)
		})
		if err != nil {
			return err
		}
		var actErr error
		switch action {
		case tui.ActionQuit, "":
			return nil
		case tui.ActionRepositories:
			actErr = runClone(ctx, a, nil, cloneFlags{retries: -1, fromDashboard: true}, false)
		case tui.ActionPullRequests, tui.ActionIssues, tui.ActionPipelines:
			refArg, err := dashboardRepo(ctx, a, p, alias)
			if err == nil {
				switch action {
				case tui.ActionIssues, tui.ActionPullRequests:
					// Pick one and read it; esc returns here, q quits Trove.
					var rp forge.Provider
					var ref domain.RepositoryRef
					if rp, ref, err = resolveRepo(ctx, a, refArg); err == nil {
						if action == tui.ActionIssues {
							err = browseIssues(ctx, a, rp, ref, forge.IssueListOptions{ListOptions: forge.ListOptions{Limit: 100}, State: domain.IssueOpen}, true)
						} else {
							err = browsePRs(ctx, a, rp, ref, forge.PullRequestListOptions{ListOptions: forge.ListOptions{Limit: 100}, State: domain.PullRequestOpen}, true)
						}
					}
				default:
					err = runSub(ctx, f, a, "pipeline", "list", "--repo", refArg)
				}
			}
			actErr = err
		case tui.ActionSettings:
			actErr = runSub(ctx, f, a, "provider", "show", alias)
		case tui.ActionProviders:
			if err := pickProvider(ctx, a, true); err != nil {
				if errors.Is(err, tui.ErrQuit) {
					return nil
				}
				if errs.ExitCode(err) == 130 {
					continue // esc: back to the dashboard
				}
				return err
			}
			if err := useProvider(a, a.Opts.Provider); err != nil {
				return err
			}
			a.Opts.Provider = ""
			continue
		}
		back, err := afterDashboardAction(ctx, a, actErr)
		if err != nil || !back {
			return err
		}
	}
}

// afterDashboardAction shows any error from a dashboard view without exiting,
// then lets the user return to the dashboard. Closing a picker (q/esc)
// returns immediately.
func afterDashboardAction(ctx context.Context, a *app.App, actErr error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(actErr, tui.ErrQuit) {
		return false, nil // q: leave Trove
	}
	if actErr != nil && errs.ExitCode(actErr) == 130 {
		return true, nil // esc: back to the dashboard
	}
	if actErr != nil {
		output.RenderError(a.IO, a.Out.Mode, actErr, a.Opts.Debug)
	}
	return tui.Pause(ctx, a.IO, "dashboard")
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

// checkoutRepo returns "namespace/name" when the current directory is a
// checkout of a repository on the given account.
func checkoutRepo(ctx context.Context, a *app.App, alias string) (string, bool) {
	if !a.Git.IsRepository(ctx, ".") {
		return "", false
	}
	d, err := a.DetectCurrent(ctx)
	if err != nil || d.Provider != alias {
		return "", false
	}
	return d.FullName(), true
}

// dashboardRepo picks the repository for per-repository dashboard actions:
// the current checkout when it belongs to this account, otherwise one the
// user chooses from the account's repositories.
func dashboardRepo(ctx context.Context, a *app.App, p forge.Provider, alias string) (string, error) {
	if full, ok := checkoutRepo(ctx, a, alias); ok {
		return alias + ":" + full, nil
	}
	repos, err := spin(ctx, a, "Fetching repositories...", func(ctx context.Context) ([]domain.Repository, error) {
		return listRepos(ctx, a, p, forge.ListRepositoryOptions{})
	})
	if err != nil {
		return "", err
	}
	if len(repos) == 0 {
		return "", noReposError(a, false)
	}
	sortRepos(repos)
	items := make([]tui.Item, len(repos))
	for i, r := range repos {
		items[i] = tui.Item{ID: r.FullName, Title: r.FullName, Subtitle: r.Description, Badges: []string{string(r.Visibility)},
			Facets: map[string]string{"namespace": r.Namespace}}
	}
	res, err := tui.RunList(ctx, a.IO, tui.ListOptions{Title: "Select repository", Context: terminal.SymProvider + " " + a.Label(alias),
		Noun: "repositories", Items: items, ConfirmLabel: "open", EscBack: true,
		Facets: []tui.Facet{{Key: "namespace", Label: "Namespace", Binding: "f"}}})
	if err != nil {
		return "", err
	}
	return alias + ":" + res.Selected[0].ID, nil
}

// runSub runs another command line with the same factory (shared App).
func runSub(ctx context.Context, f *Factory, a *app.App, args ...string) error {
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
