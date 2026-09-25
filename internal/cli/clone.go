package cli

import (
	"bufio"
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/config"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/services"
	"github.com/SurajMazar/trove-cli/internal/terminal"
	"github.com/SurajMazar/trove-cli/internal/tui"
)

type cloneFlags struct {
	list        repoListFlags
	all         bool
	selected    bool
	stdin       bool
	protocol    string
	dir         string
	layout      string
	concurrency int
	retries     int
	retryFailed bool
	// dests pins destinations per "provider:full_name" (used by
	// --retry-failed so retries land where the original attempt did).
	dests map[string]string
	// fromDashboard makes the picker's q/esc read "back".
	fromDashboard bool
}

func newRepoCloneCmd(f *Factory) *cobra.Command {
	var fl cloneFlags
	cmd := &cobra.Command{
		Use:   "clone [repository...]",
		Short: "Clone repositories (interactive picker, explicit list, or --all)",
		Long: `Clone one or more repositories.

Without arguments in a terminal, an interactive picker opens: search with /,
select with space, a/n to select all/none, f to filter by namespace, p by
provider, t to toggle HTTPS/SSH, enter to clone.

Bulk clones run with bounded concurrency (clone.concurrency, default 4).
A failed repository never stops the others; failures are recorded and can be
retried with --retry-failed.`,
		Example: `  trove repo clone                                  # interactive picker
  trove repo clone octocat/hello-world
  trove repo clone --all --namespace my-org --protocol ssh
  trove repo clone --all --all-providers --dir ~/src --layout host
  trove repo list --quiet | grep api | trove repo clone --stdin
  trove repo clone --retry-failed`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			return runClone(cmd.Context(), a, args, fl, cmd.Flags().Changed("layout"))
		},
	}
	fl.list.register(cmd, 0)
	fs := cmd.Flags()
	fs.BoolVar(&fl.all, "all", false, "clone every repository matching the filters")
	fs.BoolVar(&fl.selected, "selected", false, "choose repositories in the interactive picker (default in a terminal)")
	fs.BoolVar(&fl.stdin, "stdin", false, "read repository references from standard input, one per line")
	fs.StringVar(&fl.protocol, "protocol", "", "clone protocol: https or ssh (default: provider protocol, then clone.protocol)")
	fs.StringVar(&fl.dir, "dir", "", "base directory (default: clone.directory or the current directory)")
	fs.StringVar(&fl.layout, "layout", "", "directory layout: flat, namespace or host (default: clone.layout)")
	fs.IntVar(&fl.concurrency, "concurrency", 0, "parallel clones (default: clone.concurrency)")
	fs.IntVar(&fl.retries, "retries", -1, "extra attempts for transient failures (default: clone.retries)")
	fs.BoolVar(&fl.retryFailed, "retry-failed", false, "retry the repositories that failed in the previous bulk clone")
	cmd.MarkFlagsMutuallyExclusive("all", "selected", "stdin", "retry-failed")
	return cmd
}

// failureFile records failed bulk clones for --retry-failed. It lives in a
// subdirectory so cache invalidation (which clears *.json entries) keeps it.
func failureFile(a *app.App) string {
	return filepath.Join(a.Cache.Dir, "state", "clone-failures.json")
}

func runClone(ctx context.Context, a *app.App, args []string, fl cloneFlags, layoutSet bool) error {
	if fl.retryFailed {
		rec, err := services.LoadFailures(failureFile(a))
		if err != nil {
			return err
		}
		var repos []domain.Repository
		fl.dests = map[string]string{}
		for _, j := range rec.Jobs {
			repos = append(repos, j.Repo)
			fl.dests[j.Repo.Provider+":"+j.Repo.FullName] = j.Dest
		}
		a.Out.Info("Retrying %d repositories that failed %s", len(repos), rec.Time.Format("2006-01-02 15:04"))
		return cloneRepos(ctx, a, repos, fl, false)
	}
	opts, err := fl.list.options()
	if err != nil {
		return err
	}
	var repos []domain.Repository
	switch {
	case len(args) > 0 || fl.stdin:
		refs := args
		if fl.stdin {
			sc := bufio.NewScanner(a.IO.In)
			for sc.Scan() {
				if s := strings.TrimSpace(sc.Text()); s != "" && !strings.HasPrefix(s, "#") {
					refs = append(refs, strings.Fields(s)[0])
				}
			}
			if err := sc.Err(); err != nil {
				return err
			}
		}
		if len(refs) == 0 {
			return errs.New(errs.ErrInvalidArgument, "no repositories given")
		}
		for _, arg := range refs {
			r, err := resolveCloneTarget(ctx, a, arg)
			if err != nil {
				return err
			}
			repos = append(repos, *r)
		}
		if len(refs) == 1 && !layoutSet && !fl.stdin {
			fl.layout = "flat" // behave like `git clone` for a single repository
		}
	case fl.all:
		repos, err = cloneCandidates(ctx, a, fl, opts)
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			a.Out.Println("No repositories match.")
			return nil
		}
	default:
		if !a.IO.Interactive {
			return &errs.Error{Kind: errs.ErrInteractionRequired,
				Message: "no repositories specified",
				Hint:    "pass repositories, --all, or --stdin (the picker needs a terminal)"}
		}
		candidates, err := cloneCandidates(ctx, a, fl, opts)
		if err != nil {
			return err
		}
		picked, proto, err := pickRepos(ctx, a, candidates, fl)
		if err != nil {
			return err
		}
		fl.protocol = string(proto)
		repos = picked
	}
	return cloneRepos(ctx, a, repos, fl, len(repos) == 1)
}

func resolveCloneTarget(ctx context.Context, a *app.App, arg string) (*domain.Repository, error) {
	p, ref, err := resolveRepo(ctx, a, arg)
	if err != nil {
		return nil, err
	}
	rp, err := capability[forge.RepositoryProvider](p, forge.CapRepositories)
	if err != nil {
		return nil, err
	}
	r, err := rp.GetRepository(ctx, ref)
	if err != nil {
		return nil, errs.WithProvider(err, ref.Provider)
	}
	return r, nil
}

func cloneCandidates(ctx context.Context, a *app.App, fl cloneFlags, opts forge.ListRepositoryOptions) ([]domain.Repository, error) {
	if fl.list.allProviders {
		var failures []error
		repos, _ := spin(ctx, a, "Fetching repositories...", func(ctx context.Context) ([]domain.Repository, error) {
			r, fs := listReposMulti(ctx, a, a.Config.ProviderNames(), opts)
			failures = fs
			return r, nil
		})
		for _, e := range failures {
			a.Out.Warn("%s: %v", errs.ProviderOf(e), e)
		}
		if len(repos) == 0 && len(failures) > 0 {
			return nil, failures[0]
		}
		sortRepos(repos)
		return repos, nil
	}
	p, err := a.Current("")
	if err != nil {
		return nil, err
	}
	repos, err := spin(ctx, a, "Fetching repositories...", func(ctx context.Context) ([]domain.Repository, error) {
		return listRepos(ctx, a, p, opts)
	})
	if err != nil {
		return nil, err
	}
	sortRepos(repos)
	return repos, nil
}

func pickRepos(ctx context.Context, a *app.App, repos []domain.Repository, fl cloneFlags) ([]domain.Repository, domain.GitProtocol, error) {
	if len(repos) == 0 {
		return nil, "", noReposError(a, fl.list.allProviders)
	}
	items := make([]tui.Item, len(repos))
	providers := map[string]bool{}
	for i, r := range repos {
		var badges []string
		badges = append(badges, string(r.Visibility))
		if r.Archived {
			badges = append(badges, "archived")
		}
		if r.Fork {
			badges = append(badges, "fork")
		}
		providers[r.Provider] = true
		items[i] = tui.Item{ID: r.Provider + ":" + r.FullName, Title: r.FullName, Subtitle: r.Description, Badges: badges,
			Facets: map[string]string{"namespace": r.Namespace, "provider": r.Provider}, Value: i}
	}
	facets := []tui.Facet{{Key: "namespace", Label: "Namespace", Binding: "f"}}
	if len(providers) > 1 {
		facets = append(facets, tui.Facet{Key: "provider", Label: "Provider", Binding: "p"})
	}
	proto, err := domain.ParseGitProtocol(fl.protocol)
	if err != nil {
		return nil, "", errs.Wrap(errs.ErrInvalidArgument, err, "%v", err)
	}
	if fl.protocol == "" {
		proto = domain.GitProtocol(a.Config.Clone.Protocol)
	}
	ctxLabel := ""
	if len(providers) == 1 {
		for p := range providers {
			ctxLabel = terminal.SymProvider + " " + a.Label(p)
		}
	}
	res, err := tui.RunList(ctx, a.IO, tui.ListOptions{
		EscBack: fl.fromDashboard, Title: "Select repositories", Context: ctxLabel, Noun: "repositories", Multi: true, Items: items,
		Facets: facets, ProtocolToggle: true, Protocol: proto, ConfirmLabel: "clone",
	})
	if err != nil {
		return nil, "", err
	}
	out := make([]domain.Repository, 0, len(res.Selected))
	for _, it := range res.Selected {
		out = append(out, repos[it.Value.(int)])
	}
	return out, res.Protocol, nil
}

// cloneRepos builds jobs and runs the bounded worker pool with progress.
func cloneRepos(ctx context.Context, a *app.App, repos []domain.Repository, fl cloneFlags, single bool) error {
	cfg := a.Config.Clone
	base := fl.dir
	if base == "" {
		base = cfg.Directory
	}
	base = expandHome(base)
	base = dirOrDot(base)
	layout := fl.layout
	if layout == "" {
		layout = cfg.Layout
	}
	switch layout {
	case services.LayoutFlat, services.LayoutNamespace, services.LayoutHost:
	default:
		return errs.New(errs.ErrInvalidArgument, "invalid --layout %q (want flat, namespace or host)", layout)
	}
	conc := fl.concurrency
	if conc <= 0 {
		conc = cfg.Concurrency
	}
	if conc > config.MaxConcurrency {
		conc = config.MaxConcurrency
	}
	retries := fl.retries
	if retries < 0 {
		retries = cfg.Retries
	}

	providers := map[string]forge.Provider{}
	jobs := make([]services.CloneJob, 0, len(repos))
	var names []string
	for _, r := range repos {
		p, ok := providers[r.Provider]
		if !ok {
			var err error
			if isSelected(a, r.Provider) {
				p, err = a.Open(r.Provider)
			} else {
				p, err = a.OpenOther(r.Provider)
			}
			if err != nil {
				return err
			}
			providers[r.Provider] = p
		}
		proto, err := cloneProtocol(a, r.Provider, fl.protocol)
		if err != nil {
			return err
		}
		url, err := forge.CloneURL(ctx, p, r, proto)
		if err != nil {
			return err
		}
		dest, ok := fl.dests[r.Provider+":"+r.FullName]
		if !ok {
			var err error
			if dest, err = services.Destination(base, layout, p.Metadata().Host, r); err != nil {
				return err
			}
		}
		job := services.CloneJob{Repo: r, URL: url, Dest: dest}
		if proto == domain.ProtocolHTTPS {
			if _, ok := p.(forge.GitAuthenticator); ok {
				job.Helper = credentialHelper(a, r.Provider)
			}
		}
		jobs = append(jobs, job)
		names = append(names, r.FullName)
	}

	events := make(chan services.CloneEvent, 16)
	cloner := &services.Cloner{Git: a.Git, Concurrency: conc, Retries: retries, Events: events}
	done := make(chan services.CloneSummary, 1)
	go func() {
		s := cloner.Run(ctx, jobs)
		close(events)
		done <- s
	}()
	title := fmt.Sprintf("Cloning %d repositories", len(jobs))
	if len(jobs) == 1 {
		title = "Cloning " + jobs[0].Repo.FullName
	}
	if a.Out.Human() {
		tui.CloneProgress(ctx, a.IO, title, names, events)
	} else {
		for range events {
		}
	}
	summary := <-done
	if len(jobs) > 1 || fl.retryFailed {
		_ = services.SaveFailures(failureFile(a), jobs, summary)
	}
	if err := a.Out.Result(summary, func() []string {
		var out []string
		for _, r := range summary.Results {
			if r.State == services.CloneDone {
				out = append(out, r.Dest)
			}
		}
		return out
	}, func() error {
		renderCloneSummary(a, summary, single)
		return nil
	}); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errs.Wrap(errs.ErrCanceled, ctx.Err(), "clone canceled")
	}
	if summary.Failed > 0 {
		if single {
			return &errs.Error{Kind: errs.ErrGit, Message: summary.Results[0].Error}
		}
		return &errs.Error{Kind: errs.ErrGit, Message: fmt.Sprintf("%d of %d clones failed", summary.Failed, len(jobs)),
			Hint: "trove repo clone --retry-failed"}
	}
	return nil
}

func cloneProtocol(a *app.App, alias, flag string) (domain.GitProtocol, error) {
	v := flag
	if v == "" {
		if pc, err := a.Config.Provider(alias); err == nil {
			v = pc.Protocol
		}
	}
	if v == "" {
		v = a.Config.Clone.Protocol
	}
	p, err := domain.ParseGitProtocol(v)
	if err != nil {
		return "", errs.Wrap(errs.ErrInvalidArgument, err, "%v", err)
	}
	return p, nil
}

func renderCloneSummary(a *app.App, s services.CloneSummary, single bool) {
	th := a.Out.Theme()
	if single && len(s.Results) == 1 {
		r := s.Results[0]
		switch r.State {
		case services.CloneDone:
			fmt.Fprintf(a.IO.Out, "%s Cloned %s into %s\n", th.Success.Render(terminal.SymSuccess), r.Repository, relPath(r.Dest))
		case services.CloneSkipped:
			fmt.Fprintf(a.IO.Out, "%s Skipped %s: %s (%s)\n", th.Muted.Render("−"), r.Repository, r.Reason, relPath(r.Dest))
		}
		return
	}
	fmt.Fprintln(a.IO.Out)
	fmt.Fprintf(a.IO.Out, "%s %4d\n", th.Muted.Render("Cloned: "), s.Cloned)
	fmt.Fprintf(a.IO.Out, "%s %4d\n", th.Muted.Render("Skipped:"), s.Skipped)
	failed := fmt.Sprintf("%4d", s.Failed)
	if s.Failed > 0 {
		failed = th.Error.Render(failed)
	}
	fmt.Fprintf(a.IO.Out, "%s %s\n", th.Muted.Render("Failed: "), failed)
	if s.Failed > 0 {
		fmt.Fprintln(a.IO.Out)
		fmt.Fprintln(a.IO.Out, th.Heading.Render("Failed:"))
		for _, r := range s.FailedResults() {
			fmt.Fprintf(a.IO.Out, "  %s %s\n", r.Repository, th.Muted.Render(firstLine(r.Error)))
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func relPath(p string) string {
	if wd, err := filepath.Abs("."); err == nil {
		if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "./" + rel
		}
	}
	return p
}

func noReposError(a *app.App, allProviders bool) error {
	scope := "this account"
	if !allProviders {
		if name, err := a.ProviderName(""); err == nil {
			scope = a.Label(name)
		}
	} else {
		scope = "any configured provider"
	}
	return &errs.Error{Kind: errs.ErrNotFound, Message: "no repositories found for " + scope,
		Hint: "trove repo create <name>  (or check filters such as --namespace and --archived)"}
}
