package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/cache"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/output"
)

func newRepoCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "repo",
		Aliases: []string{"repos", "repository"},
		Short:   "Manage repositories",
		Long: `Manage repositories. Repositories are referenced as [provider:]namespace/name,
for example github-personal:octocat/hello-world or gitlab:group/subgroup/project.
Without a provider prefix the default provider is used.`,
	}
	cmd.AddCommand(newRepoListCmd(f), newRepoViewCmd(f), newRepoCreateCmd(f), newRepoDeleteCmd(f),
		newRepoCloneCmd(f), newRepoForkCmd(f), newRepoRenameCmd(f), newRepoArchiveCmd(f), newRepoDetectCmd(f))
	return cmd
}

type repoListFlags struct {
	namespace    string
	visibility   string
	archived     bool
	owned        bool
	limit        int
	allProviders bool
}

func (fl *repoListFlags) register(cmd *cobra.Command, defLimit int) {
	fs := cmd.Flags()
	fs.StringVarP(&fl.namespace, "namespace", "n", "", "only repositories in this namespace (organization, group or workspace)")
	fs.StringVar(&fl.visibility, "visibility", "", "filter by visibility: public, private, internal")
	fs.BoolVar(&fl.archived, "archived", false, "include archived repositories")
	fs.BoolVar(&fl.owned, "owned", false, "only repositories you own")
	fs.BoolVar(&fl.allProviders, "all-providers", false, "list repositories from every configured provider")
	limitFlag(cmd, &fl.limit, defLimit)
}

func (fl *repoListFlags) options() (forge.ListRepositoryOptions, error) {
	vis, err := domain.ParseVisibility(fl.visibility)
	if err != nil {
		return forge.ListRepositoryOptions{}, errs.Wrap(errs.ErrInvalidArgument, err, "%v", err)
	}
	return forge.ListRepositoryOptions{ListOptions: forge.ListOptions{Limit: fl.limit}, Namespace: fl.namespace,
		Visibility: vis, IncludeArchived: fl.archived, OwnedOnly: fl.owned}, nil
}

// listRepos lists repositories from one provider (with caching).
func listRepos(ctx context.Context, a *app.App, p forge.Provider, opts forge.ListRepositoryOptions) ([]domain.Repository, error) {
	rp, err := capability[forge.RepositoryProvider](p, forge.CapRepositories)
	if err != nil {
		return nil, err
	}
	m := p.Metadata()
	key := cache.Key(m.Name, m.Host, "repos", opts.Namespace, string(opts.Visibility),
		strconv.FormatBool(opts.IncludeArchived), strconv.FormatBool(opts.OwnedOnly), strconv.Itoa(opts.Limit))
	repos, err := cache.Fetch(ctx, a.Cache, key, func(ctx context.Context) ([]domain.Repository, error) {
		return rp.ListRepositories(ctx, opts)
	})
	if err != nil {
		return nil, errs.WithProvider(err, m.Name)
	}
	return repos, nil
}

// listReposMulti lists repositories from several providers concurrently
// (bounded), tolerating per-provider failures.
func listReposMulti(ctx context.Context, a *app.App, aliases []string, opts forge.ListRepositoryOptions) ([]domain.Repository, []error) {
	type res struct {
		repos []domain.Repository
		err   error
	}
	results := make([]res, len(aliases))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, alias := range aliases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p, err := a.OpenOther(alias)
			if err == nil {
				results[i].repos, err = listRepos(ctx, a, p, opts)
			}
			results[i].err = err
		}()
	}
	wg.Wait()
	var all []domain.Repository
	var errsOut []error
	for _, r := range results {
		all = append(all, r.repos...)
		if r.err != nil {
			errsOut = append(errsOut, r.err)
		}
	}
	return all, errsOut
}

func newRepoListCmd(f *Factory) *cobra.Command {
	var fl repoListFlags
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List repositories",
		Example: `  trove repo list
  trove repo list --provider gitlab-work --namespace platform/backend
  trove repo list --visibility private --json | jq -r '.[].full_name'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			opts, err := fl.options()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var repos []domain.Repository
			var header forge.Provider
			if fl.allProviders {
				var failures []error
				repos, err = spin(ctx, a, "Fetching repositories...", func(ctx context.Context) ([]domain.Repository, error) {
					r, fs := listReposMulti(ctx, a, a.Config.ProviderNames(), opts)
					failures = fs
					return r, nil
				})
				for _, e := range failures {
					a.Out.Warn("%s: %v", errs.ProviderOf(e), e)
				}
				if len(failures) > 0 && len(repos) == 0 {
					return failures[0]
				}
			} else {
				p, err := a.Current("")
				if err != nil {
					return err
				}
				header = p
				repos, err = spin(ctx, a, "Fetching repositories...", func(ctx context.Context) ([]domain.Repository, error) {
					return listRepos(ctx, a, p, opts)
				})
				if err != nil {
					return err
				}
			}
			if repos == nil {
				repos = []domain.Repository{}
			}
			return a.Out.Result(repos, func() []string {
				out := make([]string, len(repos))
				for i, r := range repos {
					out[i] = repoRefString(r, fl.allProviders)
				}
				return out
			}, func() error {
				if header != nil {
					providerHeader(a, header, "")
				}
				if len(repos) == 0 {
					a.Out.Println("No repositories found.")
					return nil
				}
				renderRepoTable(a, repos, fl.allProviders)
				if fl.limit > 0 && len(repos) == fl.limit {
					a.Out.Info("Showing the first %d; use --limit 0 for all", fl.limit)
				}
				return nil
			})
		},
	}
	fl.register(cmd, 100)
	return cmd
}

func repoRefString(r domain.Repository, withProvider bool) string {
	if withProvider {
		return r.Provider + ":" + r.FullName
	}
	return r.FullName
}

func renderRepoTable(a *app.App, repos []domain.Repository, withProvider bool) {
	th := a.Out.Theme()
	cols := []output.Column{{Header: "Name", Max: 60}}
	if withProvider {
		cols = append(cols, output.Column{Header: "Provider"})
	}
	cols = append(cols,
		output.Column{Header: "Visibility"},
		output.Column{Header: "Description", Flex: true, MinTerminal: 100},
		output.Column{Header: "Branch", MinTerminal: 160},
		output.Column{Header: "Updated"})
	t := &output.Table{Columns: cols}
	for _, r := range repos {
		name := r.FullName
		var tags []string
		if r.Archived {
			tags = append(tags, "archived")
		}
		if r.Fork {
			tags = append(tags, "fork")
		}
		row := []output.Cell{output.C(name)}
		if withProvider {
			row = append(row, output.S(r.Provider, th.Provider))
		}
		vis := string(r.Visibility)
		if len(tags) > 0 {
			vis += " · " + strings.Join(tags, " · ")
		}
		visCell := output.C(vis)
		if r.Visibility == domain.VisibilityPublic {
			visCell = output.S(vis, th.Muted)
		}
		updated := r.UpdatedAt
		if r.PushedAt.After(updated) {
			updated = r.PushedAt
		}
		row = append(row, visCell, output.S(r.Description, th.Muted), output.C(r.DefaultBranch), output.S(output.RelTime(updated), th.Muted))
		t.Add(row...)
	}
	a.Out.Table(t)
}

func newRepoViewCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "view [repository]",
		Short: "Show repository details",
		Example: `  trove repo view github:octocat/hello-world
  trove repo view company/backend
  trove repo view            # the repository in the current directory`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			p, ref, err := resolveRepo(cmd.Context(), a, arg)
			if err != nil {
				return err
			}
			rp, err := capability[forge.RepositoryProvider](p, forge.CapRepositories)
			if err != nil {
				return err
			}
			r, err := spin(cmd.Context(), a, "Fetching repository...", func(ctx context.Context) (*domain.Repository, error) {
				return rp.GetRepository(ctx, ref)
			})
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			return a.Out.Result(r, func() []string { return []string{r.FullName} }, func() error {
				renderRepo(a, p, r)
				return nil
			})
		},
	}
}

func renderRepo(a *app.App, p forge.Provider, r *domain.Repository) {
	th := a.Out.Theme()
	providerHeader(a, p, "")
	fmt.Fprintln(a.IO.Out, th.Title.Render(r.FullName))
	if r.Description != "" {
		fmt.Fprintln(a.IO.Out, r.Description)
	}
	fmt.Fprintln(a.IO.Out)
	var flags []string
	if r.Archived {
		flags = append(flags, "archived")
	}
	if r.Fork {
		flags = append(flags, "fork")
	}
	if r.Empty {
		flags = append(flags, "empty")
	}
	a.Out.KeyValues([]output.KV{
		{Key: "Visibility", Value: joinNonEmpty(" · ", string(r.Visibility), strings.Join(flags, " · "))},
		{Key: "Default branch", Value: r.DefaultBranch},
		{Key: "Language", Value: r.Language},
		{Key: "Stars", Value: nonZero(r.Stars)},
		{Key: "Forks", Value: nonZero(r.Forks)},
		{Key: "Open issues", Value: nonZero(r.OpenIssues)},
		{Key: "Topics", Value: strings.Join(r.Topics, ", ")},
		{Key: "Created", Value: output.RelTime(r.CreatedAt)},
		{Key: "Updated", Value: output.RelTime(r.UpdatedAt)},
		{Key: "Web", Value: r.URLs.Web},
		{Key: "HTTPS", Value: r.URLs.HTTPS},
		{Key: "SSH", Value: r.URLs.SSH},
	})
}

func nonZero(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func newRepoCreateCmd(f *Factory) *cobra.Command {
	var req forge.CreateRepositoryRequest
	var visibility string
	var private, public, internal, clone bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a repository",
		Example: `  trove repo create my-service --private --description "Payments API"
  trove repo create tools --namespace platform/infra --provider gitlab-work --clone`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			req.Name = args[0]
			switch {
			case private:
				visibility = "private"
			case public:
				visibility = "public"
			case internal:
				visibility = "internal"
			}
			if req.Visibility, err = domain.ParseVisibility(visibility); err != nil {
				return errs.Wrap(errs.ErrInvalidArgument, err, "%v", err)
			}
			if req.Visibility == "" {
				req.Visibility = domain.VisibilityPrivate
			}
			p, err := a.Current("")
			if err != nil {
				return err
			}
			rc, err := capability[forge.RepositoryCreator](p, forge.CapRepoCreate)
			if err != nil {
				return err
			}
			r, err := spin(cmd.Context(), a, "Creating repository...", func(ctx context.Context) (*domain.Repository, error) {
				return rc.CreateRepository(ctx, req)
			})
			if err != nil {
				return errs.WithProvider(err, p.Metadata().Name)
			}
			_ = a.Cache.Invalidate()
			a.Out.Success("Created %s", r.FullName)
			if err := a.Out.Result(r, func() []string { return []string{r.FullName} }, func() error {
				a.Out.Println(r.URLs.Web)
				return nil
			}); err != nil {
				return err
			}
			if clone {
				return cloneRepos(cmd.Context(), a, []domain.Repository{*r}, cloneFlags{layout: "flat"}, true)
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVarP(&req.Namespace, "namespace", "n", "", "namespace (organization, group or workspace); default: your account")
	fs.StringVarP(&req.Description, "description", "d", "", "description")
	fs.StringVar(&visibility, "visibility", "", "public, private or internal (default private)")
	fs.BoolVar(&private, "private", false, "make the repository private")
	fs.BoolVar(&public, "public", false, "make the repository public")
	fs.BoolVar(&internal, "internal", false, "make the repository internal (GitHub Enterprise, GitLab)")
	fs.StringVar(&req.DefaultBranch, "default-branch", "", "default branch name")
	fs.BoolVar(&req.AutoInit, "init", false, "initialize with a README")
	fs.BoolVar(&clone, "clone", false, "clone the new repository into the current directory")
	cmd.MarkFlagsMutuallyExclusive("private", "public", "internal", "visibility")
	return cmd
}

func newRepoDeleteCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <repository>",
		Short: "Delete a repository (asks for confirmation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, args[0])
			if err != nil {
				return err
			}
			rd, err := capability[forge.RepositoryDeleter](p, forge.CapRepoDelete)
			if err != nil {
				return err
			}
			q := fmt.Sprintf("Delete repository %q?\n\nThis action may be irreversible.\n\n", ref.FullName())
			if err := confirm(a, q); err != nil {
				return err
			}
			if _, err := spin(cmd.Context(), a, "Deleting repository...", func(ctx context.Context) (struct{}, error) {
				return struct{}{}, rd.DeleteRepository(ctx, ref)
			}); err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			_ = a.Cache.Invalidate()
			a.Out.Success("Deleted %s", ref.FullName())
			return a.Out.Result(map[string]string{"deleted": ref.FullName(), "provider": ref.Provider}, func() []string { return []string{ref.FullName()} }, func() error { return nil })
		},
	}
}

func newRepoForkCmd(f *Factory) *cobra.Command {
	var req forge.ForkRequest
	var clone bool
	cmd := &cobra.Command{
		Use:   "fork <repository>",
		Short: "Fork a repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, args[0])
			if err != nil {
				return err
			}
			fk, err := capability[forge.RepositoryForker](p, forge.CapRepoFork)
			if err != nil {
				return err
			}
			r, err := spin(cmd.Context(), a, "Forking repository...", func(ctx context.Context) (*domain.Repository, error) {
				return fk.ForkRepository(ctx, ref, req)
			})
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			_ = a.Cache.Invalidate()
			a.Out.Success("Forked %s to %s", ref.FullName(), r.FullName)
			if err := a.Out.Result(r, func() []string { return []string{r.FullName} }, func() error { a.Out.Println(r.URLs.Web); return nil }); err != nil {
				return err
			}
			if clone {
				return cloneRepos(cmd.Context(), a, []domain.Repository{*r}, cloneFlags{layout: "flat"}, true)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&req.Namespace, "namespace", "n", "", "namespace to fork into (default: your account)")
	cmd.Flags().StringVar(&req.Name, "name", "", "name for the fork")
	cmd.Flags().BoolVar(&clone, "clone", false, "clone the fork after creating it")
	return cmd
}

func newRepoRenameCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <repository> <new-name>",
		Short: "Rename a repository",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, args[0])
			if err != nil {
				return err
			}
			rn, err := capability[forge.RepositoryRenamer](p, forge.CapRepoRename)
			if err != nil {
				return err
			}
			if err := confirm(a, fmt.Sprintf("Rename %q to %q? Existing clones will need their remote URL updated.", ref.FullName(), args[1])); err != nil {
				return err
			}
			r, err := spin(cmd.Context(), a, "Renaming repository...", func(ctx context.Context) (*domain.Repository, error) {
				return rn.RenameRepository(ctx, ref, args[1])
			})
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			_ = a.Cache.Invalidate()
			a.Out.Success("Renamed %s to %s", ref.FullName(), r.FullName)
			return a.Out.Result(r, func() []string { return []string{r.FullName} }, func() error { return nil })
		},
	}
}

func newRepoArchiveCmd(f *Factory) *cobra.Command {
	var unarchive bool
	cmd := &cobra.Command{
		Use:   "archive <repository>",
		Short: "Archive (or --unarchive) a repository",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, args[0])
			if err != nil {
				return err
			}
			ar, err := capability[forge.RepositoryArchiver](p, forge.CapRepoArchive)
			if err != nil {
				return err
			}
			verb := "Archive"
			if unarchive {
				verb = "Unarchive"
			}
			if !unarchive {
				if err := confirm(a, fmt.Sprintf("Archive %q? It becomes read-only.", ref.FullName())); err != nil {
					return err
				}
			}
			r, err := spin(cmd.Context(), a, verb+" repository...", func(ctx context.Context) (*domain.Repository, error) {
				return ar.SetRepositoryArchived(ctx, ref, !unarchive)
			})
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			_ = a.Cache.Invalidate()
			a.Out.Success("%sd %s", verb, ref.FullName())
			return a.Out.Result(r, func() []string { return []string{ref.FullName()} }, func() error { return nil })
		},
	}
	cmd.Flags().BoolVar(&unarchive, "unarchive", false, "unarchive instead")
	return cmd
}

func newRepoDetectCmd(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "detect [path]",
		Short: "Identify provider, host, namespace and repository from git remotes",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			ds, err := a.DetectRemotes(cmd.Context(), dir)
			if err != nil {
				return err
			}
			return a.Out.Result(ds, func() []string {
				var out []string
				for _, d := range ds {
					out = append(out, d.Remote+"\t"+d.FullName())
				}
				return out
			}, func() error {
				th := a.Out.Theme()
				for i, d := range ds {
					if i > 0 {
						fmt.Fprintln(a.IO.Out)
					}
					fmt.Fprintln(a.IO.Out, th.Heading.Render(d.Remote))
					typ := d.Type
					if typ == "" {
						typ = th.Muted.Render("unknown")
					} else if d.Source == "heuristic" {
						typ += th.Muted.Render("  (guessed from hostname)")
					}
					prov := d.Provider
					if prov == "" {
						prov = th.Muted.Render("not configured — trove provider add")
					} else if len(d.Candidates) > 1 {
						prov += th.Muted.Render("  (also: " + strings.Join(without(d.Candidates, d.Provider), ", ") + ")")
					}
					a.Out.KeyValues([]output.KV{
						{Key: "Provider", Value: prov}, {Key: "Type", Value: typ}, {Key: "Host", Value: d.Host},
						{Key: "Namespace", Value: d.Namespace}, {Key: "Repository", Value: d.Name}, {Key: "URL", Value: d.URL},
					})
				}
				return nil
			})
		},
	}
}

func without(xs []string, s string) []string {
	var out []string
	for _, x := range xs {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// currentRepoHelper is shared by commands needing the local checkout.
func currentCheckout(ctx context.Context, a *app.App) (string, error) {
	top, err := a.Git.TopLevel(ctx, ".")
	if err != nil {
		return "", &errs.Error{Kind: errs.ErrNotFound, Message: "not inside a git repository", Hint: "cd into a clone first"}
	}
	return top, nil
}

// remoteFor finds the local remote that points at ref (falls back to origin).
func remoteFor(ctx context.Context, a *app.App, dir string, ref domain.RepositoryRef) string {
	ds, err := a.DetectRemotes(ctx, dir)
	if err == nil {
		for _, d := range ds {
			if strings.EqualFold(d.FullName(), ref.FullName()) {
				return d.Remote
			}
		}
	}
	return "origin"
}

func dirOrDot(s string) string {
	if s == "" {
		return "."
	}
	abs, err := filepath.Abs(s)
	if err != nil {
		return s
	}
	return abs
}

func sortRepos(rs []domain.Repository) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Provider != rs[j].Provider {
			return rs[i].Provider < rs[j].Provider
		}
		return strings.ToLower(rs[i].FullName) < strings.ToLower(rs[j].FullName)
	})
}
