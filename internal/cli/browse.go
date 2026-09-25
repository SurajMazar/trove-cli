package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/app"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/output"
	"github.com/SurajMazar/trove-cli/internal/terminal"
	"github.com/SurajMazar/trove-cli/internal/tui"
)

// browseContext is the right-aligned label for browse views.
func browseContext(a *app.App, p forge.Provider, ref domain.RepositoryRef) string {
	return terminal.SymProvider + " " + a.Label(p.Metadata().Name) + " · " + ref.FullName()
}

// browseIssues lets the user pick an issue and read it, looping back to the
// list after each one. With escBack, esc on the list returns to the caller
// (errs.ErrAborted); q anywhere returns an error wrapping tui.ErrQuit.
func browseIssues(ctx context.Context, a *app.App, p forge.Provider, ref domain.RepositoryRef, opts forge.IssueListOptions, escBack bool) error {
	ip, err := capability[forge.IssueProvider](p, forge.CapIssues)
	if err != nil {
		return err
	}
	issues, err := spin(ctx, a, "Fetching issues...", func(ctx context.Context) ([]domain.Issue, error) {
		return ip.ListIssues(ctx, ref, opts)
	})
	if err != nil {
		return errs.WithProvider(err, ref.Provider)
	}
	if len(issues) == 0 {
		fmt.Fprintf(a.IO.Out, "No %s issues in %s.\n", opts.State, ref.FullName())
		return nil
	}
	last := ""
	for {
		items := make([]tui.Item, len(issues))
		for i, is := range issues {
			badges := []string{string(is.State)}
			badges = append(badges, is.Labels...)
			items[i] = tui.Item{ID: strconv.Itoa(is.Number), Title: fmt.Sprintf("#%d %s", is.Number, is.Title),
				Subtitle: joinNonEmpty(" · ", is.Author, output.RelTime(is.UpdatedAt)), Badges: badges, Value: i,
				Facets: map[string]string{"author": is.Author, "label": strings.Join(is.Labels, ","), "title": is.Title}}
		}
		res, err := tui.RunList(ctx, a.IO, tui.ListOptions{Title: "Issues", Context: browseContext(a, p, ref), Noun: "issues",
			Items: items, ConfirmLabel: "read", EscBack: escBack, InitialID: last,
			Facets: []tui.Facet{{Key: "author", Label: "Author", Binding: "u"}}, SearchHint: "title words, author:name, label:bug"})
		if err != nil {
			return err
		}
		picked := issues[res.Selected[0].Value.(int)]
		last = strconv.Itoa(picked.Number)
		full, err := spin(ctx, a, fmt.Sprintf("Fetching issue #%d...", picked.Number), func(ctx context.Context) (*domain.Issue, error) {
			return ip.GetIssue(ctx, ref, picked.Number)
		})
		if err != nil {
			return errs.WithProvider(err, ref.Provider)
		}
		if err := tui.Read(ctx, a.IO, issueDocument(a, p, ref, full), "issues"); isQuit(err) || !isBack(err) {
			return err
		}
	}
}

func issueDocument(a *app.App, p forge.Provider, ref domain.RepositoryRef, is *domain.Issue) tui.Document {
	state := string(is.State)
	if is.RawState != "" && is.RawState != state {
		state += " (" + is.RawState + ")"
	}
	return tui.Document{
		Title:    is.Title,
		Subtitle: joinNonEmpty(" · ", fmt.Sprintf("#%d", is.Number), state, prefixed("by ", is.Author), prefixed("updated ", output.RelTime(is.UpdatedAt))),
		Context:  browseContext(a, p, ref),
		Fields: [][2]string{
			{"Labels", strings.Join(is.Labels, ", ")},
			{"Assignees", strings.Join(is.Assignees, ", ")},
			{"Comments", nonZero(is.Comments)},
			{"Created", output.RelTime(is.CreatedAt)},
			{"URL", is.WebURL},
		},
		Body: is.Body,
	}
}

// browsePRs is browseIssues for pull/merge requests.
func browsePRs(ctx context.Context, a *app.App, p forge.Provider, ref domain.RepositoryRef, opts forge.PullRequestListOptions, escBack bool) error {
	pp, err := capability[forge.PullRequestProvider](p, forge.CapPullRequests)
	if err != nil {
		return err
	}
	long, short := prTerms(p)
	prs, err := spin(ctx, a, "Fetching "+strings.ToLower(long)+"s...", func(ctx context.Context) ([]domain.PullRequest, error) {
		return pp.ListPullRequests(ctx, ref, opts)
	})
	if err != nil {
		return errs.WithProvider(err, ref.Provider)
	}
	if len(prs) == 0 {
		fmt.Fprintf(a.IO.Out, "No %s %ss in %s.\n", opts.State, strings.ToLower(long), ref.FullName())
		return nil
	}
	last := ""
	for {
		items := make([]tui.Item, len(prs))
		for i, pr := range prs {
			state := string(pr.State)
			if pr.Draft {
				state = "draft"
			}
			badges := append([]string{state}, pr.Labels...)
			items[i] = tui.Item{ID: strconv.Itoa(pr.Number), Title: fmt.Sprintf("#%d %s", pr.Number, pr.Title),
				Subtitle: joinNonEmpty(" · ", pr.SourceBranch+" → "+pr.TargetBranch, pr.Author), Badges: badges, Value: i,
				Facets: map[string]string{"author": pr.Author, "label": strings.Join(pr.Labels, ","), "title": pr.Title, "branch": pr.SourceBranch}}
		}
		res, err := tui.RunList(ctx, a.IO, tui.ListOptions{Title: long + "s", Context: browseContext(a, p, ref),
			Noun: strings.ToLower(long) + "s", Items: items, ConfirmLabel: "read", EscBack: escBack, InitialID: last,
			Facets: []tui.Facet{{Key: "author", Label: "Author", Binding: "u"}}, SearchHint: "title words, author:name, branch:x"})
		if err != nil {
			return err
		}
		picked := prs[res.Selected[0].Value.(int)]
		last = strconv.Itoa(picked.Number)
		full, err := spin(ctx, a, fmt.Sprintf("Fetching %s #%d...", short, picked.Number), func(ctx context.Context) (*domain.PullRequest, error) {
			return pp.GetPullRequest(ctx, ref, picked.Number)
		})
		if err != nil {
			return errs.WithProvider(err, ref.Provider)
		}
		state := string(full.State)
		if full.Draft {
			state += " (draft)"
		}
		mergeable := ""
		if full.Mergeable != nil {
			mergeable = yesNo(*full.Mergeable)
		}
		doc := tui.Document{
			Title:    full.Title,
			Subtitle: joinNonEmpty(" · ", fmt.Sprintf("%s #%d", short, full.Number), state, prefixed("by ", full.Author), prefixed("updated ", output.RelTime(full.UpdatedAt))),
			Context:  browseContext(a, p, ref),
			Fields: [][2]string{
				{"Branches", full.SourceBranch + " → " + full.TargetBranch},
				{"From fork", full.SourceRepo},
				{"Mergeable", mergeable},
				{"Reviewers", strings.Join(full.Reviewers, ", ")},
				{"Labels", strings.Join(full.Labels, ", ")},
				{"URL", full.WebURL},
			},
			Body: full.Body,
		}
		if err := tui.Read(ctx, a.IO, doc, strings.ToLower(long)+"s"); isQuit(err) || !isBack(err) {
			return err
		}
	}
}

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}

func isQuit(err error) bool { return err != nil && errors.Is(err, tui.ErrQuit) }

// isBack reports an esc-style abort (not a quit).
func isBack(err error) bool { return err != nil && errs.ExitCode(err) == 130 && !isQuit(err) }

func newIssueBrowseCmd(f *Factory) *cobra.Command {
	var repo, state, assignee, author string
	var labels []string
	var limit int
	cmd := &cobra.Command{
		Use:   "browse",
		Short: "Pick an issue and read its description (interactive)",
		Long:  "Browse issues interactively: search the list, press enter to read an issue full screen, esc to go back, q to quit.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			if !a.IO.Interactive {
				return &errs.Error{Kind: errs.ErrInteractionRequired, Message: "browse needs an interactive terminal",
					Hint: "trove issue list  /  trove issue view <number>"}
			}
			st, err := parseIssueState(state)
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			err = browseIssues(cmd.Context(), a, p, ref, forge.IssueListOptions{ListOptions: forge.ListOptions{Limit: limit},
				State: st, Labels: labels, Assignee: assignee, Author: author}, false)
			return quietExit(err)
		},
	}
	repoFlag(cmd, &repo)
	limitFlag(cmd, &limit, 100)
	cmd.Flags().StringVarP(&state, "state", "s", "open", "open, closed or all")
	cmd.Flags().StringSliceVarP(&labels, "label", "l", nil, "filter by label (repeatable)")
	cmd.Flags().StringVarP(&assignee, "assignee", "a", "", "filter by assignee username")
	cmd.Flags().StringVar(&author, "author", "", "filter by author username")
	return cmd
}

func newPRBrowseCmd(f *Factory) *cobra.Command {
	var repo, state, author, base string
	var limit int
	cmd := &cobra.Command{
		Use:   "browse",
		Short: "Pick a pull/merge request and read its description (interactive)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			if !a.IO.Interactive {
				return &errs.Error{Kind: errs.ErrInteractionRequired, Message: "browse needs an interactive terminal",
					Hint: "trove pr list  /  trove pr view <number>"}
			}
			st, err := parsePRState(state)
			if err != nil {
				return err
			}
			p, ref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			err = browsePRs(cmd.Context(), a, p, ref, forge.PullRequestListOptions{ListOptions: forge.ListOptions{Limit: limit},
				State: st, Author: author, TargetBranch: base}, false)
			return quietExit(err)
		},
	}
	repoFlag(cmd, &repo)
	limitFlag(cmd, &limit, 100)
	cmd.Flags().StringVarP(&state, "state", "s", "open", "open, closed, merged or all")
	cmd.Flags().StringVar(&author, "author", "", "filter by author username")
	cmd.Flags().StringVarP(&base, "base", "B", "", "filter by target branch")
	return cmd
}

// quietExit treats leaving a standalone browser (esc/q) as success.
func quietExit(err error) error {
	if isQuit(err) || isBack(err) {
		return nil
	}
	return err
}
