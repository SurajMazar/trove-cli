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

func newIssueCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "issue",
		Aliases: []string{"issues"},
		Short:   "Work with issues",
	}
	cmd.AddCommand(newIssueListCmd(f), newIssueBrowseCmd(f), newIssueViewCmd(f), newIssueCreateCmd(f),
		newIssueStateCmd(f, "close", domain.IssueClosed), newIssueStateCmd(f, "reopen", domain.IssueOpen))
	return cmd
}

func newIssueListCmd(f *Factory) *cobra.Command {
	var repo, state, assignee, author string
	var labels []string
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List issues",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			st, err := parseIssueState(state)
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			ip, err := capability[forge.IssueProvider](p, forge.CapIssues)
			if err != nil {
				return err
			}
			if len(labels) > 0 {
				if _, err := capability[forge.IssueProvider](p, forge.CapIssueLabels); err != nil {
					return err
				}
			}
			issues, err := spin(cmd.Context(), a, "Fetching issues...", func(ctx context.Context) ([]domain.Issue, error) {
				return ip.ListIssues(ctx, ref, forge.IssueListOptions{ListOptions: forge.ListOptions{Limit: limit}, State: st, Labels: labels, Assignee: assignee, Author: author})
			})
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			if issues == nil {
				issues = []domain.Issue{}
			}
			return a.Out.Result(issues, func() []string {
				out := make([]string, len(issues))
				for i, is := range issues {
					out[i] = strconv.Itoa(is.Number)
				}
				return out
			}, func() error {
				providerHeader(a, p, ref.FullName())
				if len(issues) == 0 {
					a.Out.Printf("No %s issues.\n", st)
					return nil
				}
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: "#"}, {Header: "Title", Flex: true}, {Header: "Labels", Flex: true, MinTerminal: 100},
					{Header: "Author", MinTerminal: 120}, {Header: "State"}, {Header: "Updated", MinTerminal: 90}}}
				for _, is := range issues {
					t.Add(output.S("#"+strconv.Itoa(is.Number), th.Accent), output.C(is.Title), output.S(strings.Join(is.Labels, ", "), th.Muted),
						output.C(is.Author), output.C(stateText(th, string(is.State))), output.S(output.RelTime(is.UpdatedAt), th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	limitFlag(cmd, &limit, 30)
	cmd.Flags().StringVarP(&state, "state", "s", "open", "open, closed or all")
	cmd.Flags().StringSliceVarP(&labels, "label", "l", nil, "filter by label (repeatable)")
	cmd.Flags().StringVarP(&assignee, "assignee", "a", "", "filter by assignee username")
	cmd.Flags().StringVar(&author, "author", "", "filter by author username")
	return cmd
}

func parseIssueState(s string) (domain.IssueState, error) {
	switch domain.IssueState(strings.ToLower(s)) {
	case "", domain.IssueOpen, "opened":
		return domain.IssueOpen, nil
	case domain.IssueClosed:
		return domain.IssueClosed, nil
	case domain.IssueAll:
		return domain.IssueAll, nil
	}
	return "", errs.New(errs.ErrInvalidArgument, "invalid state %q (want open, closed or all)", s)
}

func newIssueViewCmd(f *Factory) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "view <number>",
		Short: "Show an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			n, err := parseNumber(args[0], "issue")
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			ip, err := capability[forge.IssueProvider](p, forge.CapIssues)
			if err != nil {
				return err
			}
			is, err := spin(cmd.Context(), a, "Fetching issue...", func(ctx context.Context) (*domain.Issue, error) { return ip.GetIssue(ctx, ref, n) })
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			return a.Out.Result(is, func() []string { return []string{strconv.Itoa(is.Number)} }, func() error {
				renderIssue(a, p, ref, is)
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	return cmd
}

func renderIssue(a *app.App, p forge.Provider, ref domain.RepositoryRef, is *domain.Issue) {
	th := a.Out.Theme()
	providerHeader(a, p, ref.FullName())
	fmt.Fprintf(a.IO.Out, "%s %s\n\n", th.Title.Render(is.Title), th.Muted.Render(fmt.Sprintf("#%d", is.Number)))
	state := string(is.State)
	if is.RawState != "" && is.RawState != state {
		state += th.Muted.Render(" (" + is.RawState + ")")
	}
	a.Out.KeyValues([]output.KV{
		{Key: "State", Value: stateText(th, string(is.State)) + strings.TrimPrefix(state, string(is.State))},
		{Key: "Author", Value: is.Author},
		{Key: "Assignees", Value: strings.Join(is.Assignees, ", ")},
		{Key: "Labels", Value: strings.Join(is.Labels, ", ")},
		{Key: "Comments", Value: nonZero(is.Comments)},
		{Key: "Created", Value: output.RelTime(is.CreatedAt)},
		{Key: "Updated", Value: output.RelTime(is.UpdatedAt)},
		{Key: "URL", Value: is.WebURL},
	})
	if strings.TrimSpace(is.Body) != "" {
		fmt.Fprintln(a.IO.Out)
		fmt.Fprintln(a.IO.Out, strings.TrimSpace(is.Body))
	}
}

func newIssueCreateCmd(f *Factory) *cobra.Command {
	var repo string
	var req forge.CreateIssueRequest
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an issue",
		Args:  cobra.NoArgs,
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
			ic, err := capability[forge.IssueCreator](p, forge.CapIssueCreate)
			if err != nil {
				return err
			}
			if len(req.Labels) > 0 {
				if _, err := capability[forge.IssueProvider](p, forge.CapIssueLabels); err != nil {
					return err
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
			is, err := spin(ctx, a, "Creating issue...", func(ctx context.Context) (*domain.Issue, error) { return ic.CreateIssue(ctx, ref, req) })
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			a.Out.Success("Created issue #%d", is.Number)
			return a.Out.Result(is, func() []string { return []string{strconv.Itoa(is.Number)} }, func() error { a.Out.Println(is.WebURL); return nil })
		},
	}
	repoFlag(cmd, &repo)
	cmd.Flags().StringVarP(&req.Title, "title", "t", "", "title")
	cmd.Flags().StringVarP(&req.Body, "body", "b", "", "description")
	cmd.Flags().StringSliceVarP(&req.Labels, "label", "l", nil, "label (repeatable)")
	cmd.Flags().StringSliceVarP(&req.Assignees, "assignee", "a", nil, "assignee username (repeatable)")
	return cmd
}

func newIssueStateCmd(f *Factory, verb string, target domain.IssueState) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   verb + " <number>",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			n, err := parseNumber(args[0], "issue")
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			sc, err := capability[forge.IssueStateChanger](p, forge.CapIssueState)
			if err != nil {
				return err
			}
			is, err := spin(cmd.Context(), a, "Updating issue...", func(ctx context.Context) (*domain.Issue, error) {
				return sc.SetIssueState(ctx, ref, n, target)
			})
			if err != nil {
				return errs.WithProvider(err, ref.Provider)
			}
			past := map[string]string{"close": "Closed", "reopen": "Reopened"}[verb]
			a.Out.Success("%s issue #%d", past, n)
			return a.Out.Result(is, func() []string { return []string{strconv.Itoa(n)} }, func() error { return nil })
		},
	}
	repoFlag(cmd, &repo)
	return cmd
}
