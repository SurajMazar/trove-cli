package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/output"
)

func newPipelineCmd(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "pipeline",
		Aliases: []string{"pipelines", "ci"},
		Short:   "Work with CI pipelines (GitHub Actions, GitLab CI, Bitbucket Pipelines)",
		Long: `Work with CI pipelines. GitHub Actions workflow runs, GitLab CI pipelines and
Bitbucket Pipelines have different semantics; Trove normalizes status and
shows each provider's own terminology. Operations a provider cannot perform
(for example retrying a Bitbucket pipeline) are reported as unsupported.`,
	}
	cmd.AddCommand(newPipelineListCmd(f), newPipelineViewCmd(f), newPipelineRunCmd(f),
		newPipelineCancelCmd(f), newPipelineRetryCmd(f), newPipelineLogsCmd(f))
	return cmd
}

func pipelineTerm(p forge.Provider) string {
	if t := p.Metadata().Terms.Pipeline; t != "" {
		return t
	}
	return "Pipeline"
}

func newPipelineListCmd(f *Factory) *cobra.Command {
	var repo, ref, status string
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List recent pipelines",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, rref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			pp, err := capability[forge.PipelineProvider](p, forge.CapPipelines)
			if err != nil {
				return err
			}
			term := pipelineTerm(p)
			pls, err := spin(cmd.Context(), a, "Fetching "+strings.ToLower(term)+"s...", func(ctx context.Context) ([]domain.Pipeline, error) {
				return pp.ListPipelines(ctx, rref, forge.PipelineListOptions{ListOptions: forge.ListOptions{Limit: limit}, Ref: ref, Status: domain.PipelineStatus(status)})
			})
			if err != nil {
				return errs.WithProvider(err, rref.Provider)
			}
			if pls == nil {
				pls = []domain.Pipeline{}
			}
			return a.Out.Result(pls, func() []string {
				out := make([]string, len(pls))
				for i, pl := range pls {
					out[i] = pl.ID
				}
				return out
			}, func() error {
				providerHeader(a, p, rref.FullName())
				if len(pls) == 0 {
					a.Out.Printf("No %ss found.\n", strings.ToLower(term))
					return nil
				}
				th := a.Out.Theme()
				t := &output.Table{Columns: []output.Column{{Header: ""}, {Header: "ID"}, {Header: "Name", Flex: true}, {Header: "Ref", Flex: true},
					{Header: "Status"}, {Header: "Event", MinTerminal: 120}, {Header: "Duration", MinTerminal: 100}, {Header: "Started"}}}
				for _, pl := range pls {
					t.Add(output.C(pipelineSymbol(th, pl.Status)), output.S(pl.ID, th.Accent), output.C(pl.Name), output.S(pl.Ref, th.Muted),
						output.C(stateText(th, string(pl.Status))), output.S(pl.Event, th.Muted), output.C(duration(pl)),
						output.S(output.RelTime(firstTime(pl.StartedAt, pl.CreatedAt)), th.Muted))
				}
				a.Out.Table(t)
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	limitFlag(cmd, &limit, 20)
	cmd.Flags().StringVar(&ref, "ref", "", "filter by branch or tag")
	cmd.Flags().StringVar(&status, "status", "", "filter by status: pending, running, success, failed, canceled")
	return cmd
}

func firstTime(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func duration(pl domain.Pipeline) string {
	d := pl.Duration
	if d == 0 && !pl.StartedAt.IsZero() {
		end := pl.FinishedAt
		if end.IsZero() {
			end = time.Now()
		}
		d = end.Sub(pl.StartedAt)
	}
	if d <= 0 {
		return ""
	}
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

func newPipelineViewCmd(f *Factory) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "view <id>",
		Short: "Show a pipeline and its jobs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, rref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			pp, err := capability[forge.PipelineProvider](p, forge.CapPipelines)
			if err != nil {
				return err
			}
			pl, err := spin(cmd.Context(), a, "Fetching "+strings.ToLower(pipelineTerm(p))+"...", func(ctx context.Context) (*domain.Pipeline, error) {
				return pp.GetPipeline(ctx, rref, args[0])
			})
			if err != nil {
				return errs.WithProvider(err, rref.Provider)
			}
			return a.Out.Result(pl, func() []string { return []string{pl.ID} }, func() error {
				th := a.Out.Theme()
				providerHeader(a, p, rref.FullName())
				fmt.Fprintf(a.IO.Out, "%s %s %s\n\n", pipelineSymbol(th, pl.Status), th.Title.Render(firstNonEmpty(pl.Name, pipelineTerm(p))), th.Muted.Render(pl.ID))
				a.Out.KeyValues([]output.KV{
					{Key: "Status", Value: stateText(th, string(pl.Status)) + th.Muted.Render(rawSuffix(pl.RawStatus, string(pl.Status)))},
					{Key: "Ref", Value: pl.Ref},
					{Key: "Commit", Value: shortSHA(pl.SHA)},
					{Key: "Event", Value: pl.Event},
					{Key: "Actor", Value: pl.Actor},
					{Key: "Started", Value: output.RelTime(pl.StartedAt)},
					{Key: "Duration", Value: duration(*pl)},
					{Key: "URL", Value: pl.WebURL},
				})
				if len(pl.Jobs) > 0 {
					fmt.Fprintln(a.IO.Out)
					fmt.Fprintln(a.IO.Out, th.Heading.Render("Jobs"))
					for _, j := range pl.Jobs {
						name := j.Name
						if j.Stage != "" {
							name = th.Muted.Render(j.Stage+" / ") + j.Name
						}
						fmt.Fprintf(a.IO.Out, "  %s %s  %s\n", pipelineSymbol(th, j.Status), name, th.Muted.Render(j.ID))
					}
				}
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	return cmd
}

func rawSuffix(raw, norm string) string {
	if raw == "" || strings.EqualFold(raw, norm) {
		return ""
	}
	return " (" + raw + ")"
}

func shortSHA(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

func newPipelineRunCmd(f *Factory) *cobra.Command {
	var repo string
	var vars []string
	var req forge.RunPipelineRequest
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Trigger a pipeline",
		Example: `  trove pipeline run --ref main                               # GitLab / Bitbucket
  trove pipeline run --workflow ci.yml --ref main              # GitHub Actions (workflow_dispatch)
  trove pipeline run --ref main --var DEPLOY_ENV=staging`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			p, rref, err := resolveRepo(ctx, a, repo)
			if err != nil {
				return err
			}
			pr, err := capability[forge.PipelineRunner](p, forge.CapPipelineRun)
			if err != nil {
				return err
			}
			if req.Variables, err = parseVars(vars); err != nil {
				return err
			}
			if req.Ref == "" {
				if br, err := a.Git.CurrentBranch(ctx, "."); err == nil && br != "" {
					req.Ref = br
				} else {
					return errs.New(errs.ErrInvalidArgument, "--ref is required")
				}
			}
			term := pipelineTerm(p)
			pl, err := spin(ctx, a, "Triggering "+strings.ToLower(term)+"...", func(ctx context.Context) (*domain.Pipeline, error) {
				return pr.RunPipeline(ctx, rref, req)
			})
			if err != nil {
				return errs.WithProvider(err, rref.Provider)
			}
			if pl == nil {
				a.Out.Success("%s requested for %s (the provider starts it asynchronously)", term, req.Ref)
				return a.Out.Result(map[string]any{"accepted": true, "ref": req.Ref}, nil, func() error { return nil })
			}
			a.Out.Success("Started %s %s on %s", strings.ToLower(term), pl.ID, req.Ref)
			return a.Out.Result(pl, func() []string { return []string{pl.ID} }, func() error {
				if pl.WebURL != "" {
					a.Out.Println(pl.WebURL)
				}
				return nil
			})
		},
	}
	repoFlag(cmd, &repo)
	cmd.Flags().StringVar(&req.Ref, "ref", "", "branch or tag (default: current branch)")
	cmd.Flags().StringVarP(&req.Workflow, "workflow", "w", "", "workflow file/ID (GitHub) or custom pipeline name (Bitbucket)")
	cmd.Flags().StringArrayVar(&vars, "var", nil, "variable/input KEY=VALUE (repeatable)")
	return cmd
}

func newPipelineCancelCmd(f *Factory) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a running pipeline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, rref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			pc, err := capability[forge.PipelineCanceler](p, forge.CapPipelineCancel)
			if err != nil {
				return err
			}
			term := strings.ToLower(pipelineTerm(p))
			if err := confirm(a, fmt.Sprintf("Cancel %s %s in %s?", term, args[0], rref.FullName())); err != nil {
				return err
			}
			if _, err := spin(cmd.Context(), a, "Canceling...", func(ctx context.Context) (struct{}, error) {
				return struct{}{}, pc.CancelPipeline(ctx, rref, args[0])
			}); err != nil {
				return errs.WithProvider(err, rref.Provider)
			}
			a.Out.Success("Cancellation requested for %s %s", term, args[0])
			return a.Out.Result(map[string]any{"id": args[0], "canceled": true}, func() []string { return []string{args[0]} }, func() error { return nil })
		},
	}
	repoFlag(cmd, &repo)
	return cmd
}

func newPipelineRetryCmd(f *Factory) *cobra.Command {
	var repo string
	cmd := &cobra.Command{
		Use:   "retry <id>",
		Short: "Retry a pipeline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, rref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			pr, err := capability[forge.PipelineRetrier](p, forge.CapPipelineRetry)
			if err != nil {
				return err
			}
			if _, err := spin(cmd.Context(), a, "Retrying...", func(ctx context.Context) (struct{}, error) {
				return struct{}{}, pr.RetryPipeline(ctx, rref, args[0])
			}); err != nil {
				return errs.WithProvider(err, rref.Provider)
			}
			a.Out.Success("Retry requested for %s %s", strings.ToLower(pipelineTerm(p)), args[0])
			return a.Out.Result(map[string]any{"id": args[0], "retried": true}, func() []string { return []string{args[0]} }, func() error { return nil })
		},
	}
	repoFlag(cmd, &repo)
	return cmd
}

func newPipelineLogsCmd(f *Factory) *cobra.Command {
	var repo, job string
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Print pipeline logs (all jobs, or one with --job)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := f.App()
			if err != nil {
				return err
			}
			p, rref, err := resolveRepo(cmd.Context(), a, repo)
			if err != nil {
				return err
			}
			pl, err := capability[forge.PipelineLogger](p, forge.CapPipelineLogs)
			if err != nil {
				return err
			}
			if a.Out.JSON() {
				return errs.New(errs.ErrInvalidArgument, "logs are plain text; --json is not supported for this command")
			}
			if err := pl.PipelineLogs(cmd.Context(), rref, args[0], job, a.IO.Out); err != nil {
				return errs.WithProvider(err, rref.Provider)
			}
			return nil
		},
	}
	repoFlag(cmd, &repo)
	cmd.Flags().StringVarP(&job, "job", "j", "", "only this job/step ID")
	return cmd
}
