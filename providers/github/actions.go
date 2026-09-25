package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

type apiRun struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	DisplayTitle string     `json:"display_title"`
	RunNumber    int        `json:"run_number"`
	RunAttempt   int        `json:"run_attempt"`
	Status       string     `json:"status"`
	Conclusion   *string    `json:"conclusion"`
	HeadBranch   string     `json:"head_branch"`
	HeadSHA      string     `json:"head_sha"`
	Event        string     `json:"event"`
	Actor        *apiOwner  `json:"actor"`
	HTMLURL      string     `json:"html_url"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	RunStartedAt *time.Time `json:"run_started_at"`
}

type apiJob struct {
	ID           int64      `json:"id"`
	RunID        int64      `json:"run_id"`
	Name         string     `json:"name"`
	WorkflowName string     `json:"workflow_name"`
	Status       string     `json:"status"`
	Conclusion   *string    `json:"conclusion"`
	HTMLURL      string     `json:"html_url"`
	StartedAt    *time.Time `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at"`
}

// runStatus normalizes GitHub's two-field status: "status" is the lifecycle
// (queued, in_progress, completed, ...) and "conclusion" the outcome once
// completed.
func runStatus(status string, conclusion *string) (domain.PipelineStatus, string) {
	c := str(conclusion)
	switch status {
	case "queued", "waiting", "requested", "pending":
		return domain.PipelinePending, status
	case "in_progress":
		return domain.PipelineRunning, status
	case "completed":
		switch c {
		case "success":
			return domain.PipelineSuccess, c
		case "failure", "timed_out", "startup_failure":
			return domain.PipelineFailed, c
		case "cancelled":
			return domain.PipelineCanceled, c
		case "skipped", "neutral":
			return domain.PipelineSkipped, c
		case "action_required":
			return domain.PipelineManual, c
		}
		return domain.PipelineUnknown, orDefault(c, status)
	case "action_required":
		return domain.PipelineManual, status
	}
	return domain.PipelineUnknown, status
}

func toPipeline(r apiRun) domain.Pipeline {
	st, raw := runStatus(r.Status, r.Conclusion)
	name := r.Name
	if r.DisplayTitle != "" && r.DisplayTitle != r.Name {
		name = r.Name + ": " + r.DisplayTitle
	}
	out := domain.Pipeline{
		ID: strconv.FormatInt(r.ID, 10), Number: r.RunNumber, Name: name, Status: st, RawStatus: raw,
		Ref: r.HeadBranch, SHA: r.HeadSHA, Event: r.Event, WebURL: r.HTMLURL, CreatedAt: r.CreatedAt,
		StartedAt: timeOf(r.RunStartedAt),
	}
	if r.Actor != nil {
		out.Actor = r.Actor.Login
	}
	if r.Status == "completed" {
		// Runs have no completed_at; updated_at is set when the run
		// finishes and does not change afterwards (until a re-run).
		out.FinishedAt = r.UpdatedAt
		if !out.StartedAt.IsZero() && out.FinishedAt.After(out.StartedAt) {
			out.Duration = out.FinishedAt.Sub(out.StartedAt)
		}
	}
	return out
}

func toJob(j apiJob) domain.PipelineJob {
	st, raw := runStatus(j.Status, j.Conclusion)
	return domain.PipelineJob{
		ID: strconv.FormatInt(j.ID, 10), Name: j.Name, Stage: j.WorkflowName, Status: st, RawStatus: raw,
		WebURL: j.HTMLURL, StartedAt: timeOf(j.StartedAt), FinishedAt: timeOf(j.CompletedAt),
	}
}

// serverRunStatus returns the "status" query value when a normalized status
// corresponds to exactly one GitHub value; otherwise filtering happens
// client-side only (e.g. "failed" spans failure, timed_out, startup_failure).
func serverRunStatus(s domain.PipelineStatus) string {
	switch s {
	case domain.PipelineRunning:
		return "in_progress"
	case domain.PipelineSuccess:
		return "success"
	case domain.PipelineCanceled:
		return "cancelled"
	case domain.PipelineManual:
		return "action_required"
	}
	return ""
}

// ListPipelines implements forge.PipelineProvider (GitHub Actions workflow
// runs, newest first).
func (p *Provider) ListPipelines(ctx context.Context, ref domain.RepositoryRef, opts forge.PipelineListOptions) ([]domain.Pipeline, error) {
	q := url.Values{}
	if opts.Ref != "" {
		q.Set("branch", strings.TrimPrefix(opts.Ref, "refs/heads/"))
	}
	if s := serverRunStatus(opts.Status); s != "" {
		q.Set("status", s)
	}
	return paginate(ctx, p, listSpec[apiRun]{
		request: request{op: "list workflow runs", path: repoPath(ref.Namespace, ref.Name, "actions", "runs"), query: q,
			notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref)},
		perPage: maxPerPage, limit: opts.Limit,
		decode: envelope[apiRun]("workflow_runs", nil),
	}, func(r apiRun) (domain.Pipeline, bool) {
		pl := toPipeline(r)
		return pl, opts.Status == "" || pl.Status == opts.Status
	})
}

func (p *Provider) runPath(ref domain.RepositoryRef, op, id string, rest ...string) (string, error) {
	n, err := p.parseNumericID(op, "workflow run ID", id)
	if err != nil {
		return "", err
	}
	return repoPath(ref.Namespace, ref.Name, append([]string{"actions", "runs", strconv.FormatInt(n, 10)}, rest...)...), nil
}

func (p *Provider) listJobs(ctx context.Context, op string, ref domain.RepositoryRef, runID string) ([]apiJob, error) {
	jp, err := p.runPath(ref, op, runID, "jobs")
	if err != nil {
		return nil, err
	}
	return paginate(ctx, p, listSpec[apiJob]{
		request: request{op: op, path: jp, notFoundMsg: fmt.Sprintf("workflow run %s not found in %s", runID, ref.FullName())},
		perPage: maxPerPage,
		decode:  envelope[apiJob]("jobs", nil),
	}, identity[apiJob])
}

// GetPipeline implements forge.PipelineProvider, including the jobs of the
// latest attempt.
func (p *Provider) GetPipeline(ctx context.Context, ref domain.RepositoryRef, id string) (*domain.Pipeline, error) {
	const op = "get workflow run"
	rp, err := p.runPath(ref, op, id)
	if err != nil {
		return nil, err
	}
	var r apiRun
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: rp, out: &r,
		notFoundMsg: fmt.Sprintf("workflow run %s not found in %s", id, ref.FullName())}); err != nil {
		return nil, err
	}
	jobs, err := p.listJobs(ctx, op, ref, id)
	if err != nil {
		return nil, err
	}
	out := toPipeline(r)
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, toJob(j))
	}
	return &out, nil
}

// RunPipeline implements forge.PipelineRunner via a workflow_dispatch event.
// The workflow must declare "on: workflow_dispatch". github.com answers 200
// with the new run's ID; GitHub Enterprise Server answers 204 without one,
// in which case nil is returned (the run is created asynchronously).
func (p *Provider) RunPipeline(ctx context.Context, ref domain.RepositoryRef, req forge.RunPipelineRequest) (*domain.Pipeline, error) {
	const op = "run workflow"
	wf := strings.TrimSpace(req.Workflow)
	if wf == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "GitHub Actions needs the workflow to run: a workflow file name (e.g. ci.yml) or numeric ID")
	}
	// Accept ".github/workflows/ci.yml" as well as "ci.yml".
	wf = path.Base(wf)
	gitRef := strings.TrimSpace(req.Ref)
	if gitRef == "" {
		repo, err := p.GetRepository(ctx, ref)
		if err != nil {
			return nil, err
		}
		gitRef = repo.DefaultBranch
	}
	body := map[string]any{"ref": gitRef}
	if len(req.Variables) > 0 {
		body["inputs"] = req.Variables
	}
	var out struct {
		WorkflowRunID int64  `json:"workflow_run_id"`
		HTMLURL       string `json:"html_url"`
	}
	_, err := p.do(ctx, request{
		op: op, method: http.MethodPost,
		path: repoPath(ref.Namespace, ref.Name, "actions", "workflows", esc(wf), "dispatches"),
		body: body, out: &out,
		notFoundMsg: fmt.Sprintf("workflow %q not found in %s", wf, ref.FullName()),
	})
	if err != nil {
		return nil, err
	}
	if out.WorkflowRunID == 0 {
		return nil, nil
	}
	id := strconv.FormatInt(out.WorkflowRunID, 10)
	pl, err := p.GetPipeline(ctx, ref, id)
	if err != nil {
		if errors.Is(err, errs.ErrCanceled) {
			return nil, err
		}
		// The run was created; report what the dispatch response told us.
		return &domain.Pipeline{ID: id, Status: domain.PipelinePending, RawStatus: "requested", Ref: gitRef, WebURL: out.HTMLURL}, nil
	}
	return pl, nil
}

// CancelPipeline implements forge.PipelineCanceler (202 Accepted; 409 when
// the run already finished).
func (p *Provider) CancelPipeline(ctx context.Context, ref domain.RepositoryRef, id string) error {
	const op = "cancel workflow run"
	rp, err := p.runPath(ref, op, id, "cancel")
	if err != nil {
		return err
	}
	_, err = p.do(ctx, request{op: op, method: http.MethodPost, path: rp,
		notFoundMsg: fmt.Sprintf("workflow run %s not found in %s", id, ref.FullName())})
	return err
}

// RetryPipeline implements forge.PipelineRetrier by re-running every job of
// the run (POST .../rerun, 201). The run must be completed.
func (p *Provider) RetryPipeline(ctx context.Context, ref domain.RepositoryRef, id string) error {
	const op = "re-run workflow run"
	rp, err := p.runPath(ref, op, id, "rerun")
	if err != nil {
		return err
	}
	_, err = p.do(ctx, request{op: op, method: http.MethodPost, path: rp,
		notFoundMsg: fmt.Sprintf("workflow run %s not found in %s", id, ref.FullName())})
	return err
}

// PipelineLogs implements forge.PipelineLogger. GitHub answers the job log
// request with a 302 to a short-lived pre-signed URL on another host; the
// client follows it without the Authorization header (see checkRedirect).
func (p *Provider) PipelineLogs(ctx context.Context, ref domain.RepositoryRef, pipelineID, jobID string, w io.Writer) error {
	const op = "download job logs"
	if jobID != "" {
		return p.jobLog(ctx, op, ref, jobID, w)
	}
	jobs, err := p.listJobs(ctx, op, ref, pipelineID)
	if err != nil {
		return err
	}
	for i, j := range jobs {
		if i > 0 {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "==> %s\n", j.Name); err != nil {
			return err
		}
		if j.Status != "completed" {
			if _, err := fmt.Fprintf(w, "(logs are available once the job completes; status: %s)\n", j.Status); err != nil {
				return err
			}
			continue
		}
		err := p.jobLog(ctx, op, ref, strconv.FormatInt(j.ID, 10), w)
		if errors.Is(err, errs.ErrNotFound) {
			// Skipped jobs and expired logs have nothing to download.
			if _, err := io.WriteString(w, "(no logs available)\n"); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) jobLog(ctx context.Context, op string, ref domain.RepositoryRef, jobID string, w io.Writer) error {
	n, err := p.parseNumericID(op, "job ID", jobID)
	if err != nil {
		return err
	}
	resp, err := p.open(ctx, p.stream, request{
		op: op, method: http.MethodGet,
		path:        repoPath(ref.Namespace, ref.Name, "actions", "jobs", strconv.FormatInt(n, 10), "logs"),
		notFoundMsg: fmt.Sprintf("logs for job %s not found in %s (they may have expired)", jobID, ref.FullName()),
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, resp.Body); err != nil {
		if ctxErr := p.ctxErr(ctx, op); ctxErr != nil {
			return ctxErr
		}
		return p.errorf(errs.ErrProviderAPI, op, 0, "reading job logs failed: %v", err)
	}
	return nil
}
