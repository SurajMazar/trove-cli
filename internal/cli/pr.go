package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/output"
	"github.com/SurajMazar/trove-cli/internal/tui"
)

func newPRCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pr",
		Aliases: []string{"mr", "pull-request", "merge-request"},
		Short:   "Work with pull requests (GitLab: merge requests)",
		Long: `Work with pull requests. On GitLab these are merge requests; Trove uses the
provider's own terminology in its output. Numbers are the provider's
per-repository number (GitHub number, GitLab IID, Bitbucket ID).`,
	}
	cmd.AddCommand(newPRListCmd(f), newPRViewCmd(f), newPRCreateCmd(f), newPRCheckoutCmd(f), newPRMergeCmd(f), newPRCloseCmd(f))
	return cmd
}

func prTerms(p forge.Provider) (long, short string) {
	t := p.Metadata().Terms
	long, short = t.PullRequest, t.PullRequestShort
	if long == "" {
		long = "Pull Request"
	}
	if short == "" {
		short = "PR"
	}
	return long, short
}

func newPRListCmd(f *Factory) *cobra.Command {
	var repo, state, author, base string
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List pull/merge requests",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			st, err := parsePRState(state)
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			pp, err := capability[forge.PullRequestProvider](p, forge.CapPullRequests)
			if err != nil {
				return err
			}
			long, _ := prTerms(p)
			prs, err := spin(cmd.Context(), a, "Fetching "+strings.ToLower(long)+"s...", func(ctx context.Context) ([]domain.PullRequest, error) {
				return pp.ListPullRequests(ctx, ref, forge.PullRequestListOptions{ListOptions: forge.ListOptions{Limit: limit}, State: st, Author: author, TargetBranch: base})
			})
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			if prs == nil {
				prs = []domain.PullRequest{}
			}
			return a.Out.Result(prs, func() []string {
				out := make([]string, len(prs))
				for i, pr := range prs {
					out[i] = strconv.Itoa(pr.Number)
				}
				return out
			}, func() error {
				providerHeader(a, p, ref.FullName())
				if len(prs) == 0 {
					a.Out.Printf("No %s %ss.\n", st, strings.ToLower(long))
					return nil
				}
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: "#"}, {Header: "Title", Flex: true}, {Header: "Branch", Flex: true, MinTerminal: 100},
					{Header: "Author", MinTerminal: 90}, {Header: "State"}, {Header: "Updated", MinTerminal: 120}}}
				for _, pr := range prs {
					stateS := string(pr.State)
					if pr.Draft {
						stateS = "draft"
					}
					t.Add(output.S("#"+strconv.Itoa(pr.Number), th.Accent), output.C(pr.Title),
						output.S(pr.SourceBranch+" → "+pr.TargetBranch, th.Muted), output.C(pr.Author),
						output.C(stateText(th, stateS)), output.S(output.RelTime(pr.UpdatedAt), th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	limitFlag(cmd, &limit, 30)
	cmd.Flags().StringVarP(&state, "state", "s", "open", "open, closed, merged or all")
	cmd.Flags().StringVar(&author, "author", "", "filter by author username")
	cmd.Flags().StringVarP(&base, "base", "B", "", "filter by target branch")
	return cmd
}

func parsePRState(s string) (domain.PullRequestState, error) {
	switch domain.PullRequestState(strings.ToLower(s)) {
	case "", domain.PullRequestOpen, "opened":
		return domain.PullRequestOpen, nil
	case domain.PullRequestClosed:
		return domain.PullRequestClosed, nil
	case domain.PullRequestMerged:
		return domain.PullRequestMerged, nil
	case domain.PullRequestAll:
		return domain.PullRequestAll, nil
	}
	return "", errs.New(errs.ErrInvalidArgument, "invalid state %q (want open, closed, merged or all)", s)
}

func getPR(ctx context.Context, a *app.App, repo, num string) (forge.Provider, domain.RepositoryRef, *domain.PullRequest, error) {
	n, err := parseNumber(num, "pull request")
	if err != nil {
		return nil, domain.RepositoryRef{}, nil, err
	}
	p, ref, err := resolveRepo(ctx, a, repo)
	if err != nil {
		return nil, ref, nil, err
	}
	pp, err := capability[forge.PullRequestProvider](p, forge.CapPullRequests)
	if err != nil {
		return nil, ref, nil, err
	}
	long, _ := prTerms(p)
	pr, err := spin(ctx, a, "Fetching "+strings.ToLower(long)+"...", func(ctx context.Context) (*domain.PullRequest, error) {
		return pp.GetPullRequest(ctx, ref, n)
	})
	if err != nil {
		return nil, ref, nil, errs.WithProvider(err, ref.Provider)
	}
	return p, ref, pr, nil
}

func newPRViewCmd(f *Factory) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "view <number>",
		Short: "Show a pull/merge request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, ref, pr, err := getPR(cmd.Context(), a, repo, args[0])
			if err != nil {
				return err
			}
			return a.Out.Result(pr, func() []string { return []string{strconv.Itoa(pr.Number)} }, func() error {
				th := a.Out.Theme()
				long, short := prTerms(p)
				providerHeader(a, p, ref.FullName())
				fmt.Fprintf(a.IO.Out, "%s %s\n", th.Title.Render(pr.Title), th.Muted.Render(fmt.Sprintf("%s #%d", short, pr.Number)))
				state := string(pr.State)
				if pr.Draft {
					state += " (draft)"
				}
				fmt.Fprintln(a.IO.Out)
				mergeable := ""
				if pr.Mergeable != nil {
					mergeable = yesNo(*pr.Mergeable)
				}
				a.Out.KeyValues([]output.KV{
					{Key: "Type", Value: long},
					{Key: "State", Value: stateText(th, state)},
					{Key: "Author", Value: pr.Author},
					{Key: "Branches", Value: pr.SourceBranch + " → " + pr.TargetBranch},
					{Key: "From fork", Value: pr.SourceRepo},
					{Key: "Mergeable", Value: mergeable},
					{Key: "Labels", Value: strings.Join(pr.Labels, ", ")},
					{Key: "Reviewers", Value: strings.Join(pr.Reviewers, ", ")},
					{Key: "Created", Value: output.RelTime(pr.CreatedAt)},
					{Key: "Updated", Value: output.RelTime(pr.UpdatedAt)},
					{Key: "Merged", Value: output.RelTime(pr.MergedAt)},
					{Key: "URL", Value: pr.WebURL},
				})
				if strings.TrimSpace(pr.Body) != "" {
					fmt.Fprintln(a.IO.Out)
					fmt.Fprintln(a.IO.Out, strings.TrimSpace(pr.Body))
				}
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	return cmd
}

func newPRCreateCmd(f *Factory) *cobra.Command {
	var repo string
	var req forge.CreatePullRequestRequest
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a pull/merge request",
		Example: `  trove pr create --title "Add retries" --body "Fixes #12"
  trove pr create --head feature/x --base main --draft`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			p, ref, err := resolveRepo(ctx, a, repo)
			if err != nil {
				return err
			}
			pc, err := capability[forge.PullRequestCreator](p, forge.CapPRCreate)
			if err != nil {
				return err
			}
			if req.SourceBranch == "" {
				br, err := a.Git.CurrentBranch(ctx, ".")
				if err != nil || br == "" {
					return errs.New(errs.ErrInvalidArgument, "--head is required (could not detect the current branch)")
				}
				req.SourceBranch = br
			}
			if req.TargetBranch == "" {
				if rp, err := capability[forge.RepositoryProvider](p, forge.CapRepositories); err == nil {
					if r, err := rp.GetRepository(ctx, ref); err == nil {
						req.TargetBranch = r.DefaultBranch
					}
				}
				if req.TargetBranch == "" {
					return errs.New(errs.ErrInvalidArgument, "--base is required")
				}
			}
			if req.Title == "" {
				if !a.IO.Interactive {
					return errs.New(errs.ErrInvalidArgument, "--title is required")
				}
				if req.Title, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: "Title:", Validate: nonEmpty}); err != nil {
					return err
				}
				if req.Body == "" {
					if req.Body, err = tui.Input(ctx, a.IO, tui.InputOptions{Label: "Description (optional):"}); err != nil {
						return err
					}
				}
			}
			long, _ := prTerms(p)
			pr, err := spin(ctx, a, "Creating "+strings.ToLower(long)+"...", func(ctx context.Context) (*domain.PullRequest, error) {
				return pc.CreatePullRequest(ctx, ref, req)
			})
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			a.Out.Success("Created %s #%d", strings.ToLower(long), pr.Number)
			return a.Out.Result(pr, func() []string { return []string{strconv.Itoa(pr.Number)} }, func() error {
				a.Out.Println(pr.WebURL)
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	fs := cmd.Flags()
	fs.StringVarP(&req.Title, "title", "t", "", "title")
	fs.StringVarP(&req.Body, "body", "b", "", "description")
	fs.StringVarP(&req.SourceBranch, "head", "H", "", "source branch (default: current branch)")
	fs.StringVarP(&req.TargetBranch, "base", "B", "", "target branch (default: repository default branch)")
	fs.BoolVarP(&req.Draft, "draft", "d", false, "create as draft")
	fs.StringVar(&req.SourceRepo, "head-repo", "", "source repository when the branch lives in a fork (namespace/name)")
	return cmd
}

func nonEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("required")
	}
	return nil
}

func newPRCheckoutCmd(f *Factory) *cobra.Command {
	var repo, branch string
	cmd := &cobra.Command{
		Use:   "checkout <number>",
		Short: "Check out a pull/merge request locally",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			dir, err := currentCheckout(ctx, a)
			if err != nil {
				return err
			}
			p, ref, pr, err := getPR(ctx, a, repo, args[0])
			if err != nil {
				return err
			}
			local := branch
			if local == "" {
				local = pr.SourceBranch
			}
			if local == "" {
				local = fmt.Sprintf("pr-%d", pr.Number)
			}
			remote := remoteFor(ctx, a, dir, ref)
			g := a.Git
			if h := credentialHelper(a, ref.Provider); h != "" {
				if _, ok := p.(forge.GitAuthenticator); ok && remoteIsHTTPS(ctx, a, dir, remote) {
					g = g.WithCredentialHelper(h)
				}
			}
			if hr, ok := p.(forge.PullRequestHeadRefer); ok {
				if err := g.Fetch(ctx, dir, remote, hr.PullRequestHeadRef(pr.Number)); err != nil {
					return err
				}
			} else {
				// No PR refs (e.g. Bitbucket): fetch the source branch from the
				// source repository, which may be a fork.
				src := remote
				if pr.SourceRepo != "" && !strings.EqualFold(pr.SourceRepo, ref.FullName()) {
					sref, err := domain.ParseRepositoryRef(pr.SourceRepo)
					if err != nil {
						return err
					}
					rp, err := capability[forge.RepositoryProvider](p, forge.CapRepositories)
					if err != nil {
						return err
					}
					u := rp.RepositoryURLs(sref)
					src = u.HTTPS
					if !remoteIsHTTPS(ctx, a, dir, remote) && u.SSH != "" {
						src = u.SSH
					}
				}
				if err := g.Fetch(ctx, dir, src, pr.SourceBranch); err != nil {
					return err
				}
			}
			if err := a.Git.Checkout(ctx, dir, local, "FETCH_HEAD"); err != nil {
				return err
			}
			_, short := prTerms(p)
			a.Out.Success("Checked out %s #%d as branch %s", short, pr.Number, local)
			return a.Out.Result(map[string]any{"number": pr.Number, "branch": local}, func() []string { return []string{local} }, func() error { return nil })
		},
	}
	repoFlag(cmd, &repo)
	cmd.Flags().StringVarP(&branch, "branch", "b", "", "local branch name (default: the source branch name)")
	return cmd
}

func remoteIsHTTPS(ctx context.Context, a *app.App, dir, remote string) bool {
	rs, err := a.Git.Remotes(ctx, dir)
	if err != nil {
		return false
	}
	for _, r := range rs {
		if r.Name == remote {
			return strings.HasPrefix(r.FetchURL, "https://") || strings.HasPrefix(r.FetchURL, "http://")
		}
	}
	return false
}

func newPRMergeCmd(f *Factory) *cobra.Command {
	var repo, method string
	var req forge.MergePullRequestRequest
	cmd := &cobra.Command{
		Use:   "merge <number>",
		Short: "Merge a pull/merge request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			n, err := parseNumber(args[0], "pull request")
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			pm, err := capability[forge.PullRequestMerger](p, forge.CapPRMerge)
			if err != nil {
				return err
			}
			req.Method = domain.MergeMethod(strings.ToLower(method))
			supported := pm.MergeMethods()
			ok := false
			var names []string
			for _, m := range supported {
				ok = ok || m == req.Method
				names = append(names, string(m))
			}
			if !ok {
				return errs.New(errs.ErrInvalidArgument, "%s does not support the %q merge method (supported: %s)", p.Metadata().DisplayName, method, strings.Join(names, ", "))
			}
			_, short := prTerms(p)
			if err := confirm(a, fmt.Sprintf("Merge %s #%d in %s using %s?", short, n, ref.FullName(), req.Method)); err != nil {
				return err
			}
			if _, err := spin(cmd.Context(), a, "Merging...", func(ctx context.Context) (struct{}, error) {
				return struct{}{}, pm.MergePullRequest(ctx, ref, n, req)
			}); err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			a.Out.Success("Merged %s #%d", short, n)
			return a.Out.Result(map[string]any{"number": n, "merged": true, "method": req.Method}, func() []string { return []string{strconv.Itoa(n)} }, func() error { return nil })
		},
	}
	repoFlag(cmd, &repo)
	fs := cmd.Flags()
	fs.StringVarP(&method, "method", "m", "merge", "merge method: merge, squash or rebase (provider dependent)")
	fs.BoolVarP(&req.DeleteSourceBranch, "delete-branch", "d", false, "delete the source branch after merging")
	fs.StringVar(&req.CommitTitle, "subject", "", "merge commit subject")
	fs.StringVar(&req.CommitMessage, "body", "", "merge commit body")
	return cmd
}

func newPRCloseCmd(f *Factory) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "close <number>",
		Short: "Close a pull/merge request without merging",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			n, err := parseNumber(args[0], "pull request")
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			pc, err := capability[forge.PullRequestCloser](p, forge.CapPRClose)
			if err != nil {
				return err
			}
			if _, err := spin(cmd.Context(), a, "Closing...", func(ctx context.Context) (struct{}, error) {
				return struct{}{}, pc.ClosePullRequest(ctx, ref, n)
			}); err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			_, short := prTerms(p)
			a.Out.Success("Closed %s #%d", short, n)
			return a.Out.Result(map[string]any{"number": n, "state": "closed"}, func() []string { return []string{strconv.Itoa(n)} }, func() error { return nil })
		},
	}
	repoFlag(cmd, &repo)
	return cmd
}
