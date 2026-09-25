package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

type glPipeline struct {
	ID         int64      `json:"id"`
	IID        int        `json:"iid"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Ref        string     `json:"ref"`
	SHA        string     `json:"sha"`
	Source     string     `json:"source"`
	WebURL     string     `json:"web_url"`
	CreatedAt  *time.Time `json:"created_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Duration   *float64   `json:"duration"`
	User       *glUserRef `json:"user"`
}

type glJob struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Stage      string     `json:"stage"`
	Status     string     `json:"status"`
	WebURL     string     `json:"web_url"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// pipelineStatus normalizes GitLab pipeline and job statuses.
func pipelineStatus(s string) domain.PipelineStatus {
	switch s {
	case "created", "waiting_for_resource", "preparing", "pending", "scheduled", "waiting_for_callback":
		return domain.PipelinePending
	case "running":
		return domain.PipelineRunning
	case "success":
		return domain.PipelineSuccess
	case "failed":
		return domain.PipelineFailed
	case "canceled", "canceling":
		return domain.PipelineCanceled
	case "skipped":
		return domain.PipelineSkipped
	case "manual":
		return domain.PipelineManual
	}
	return domain.PipelineUnknown
}

func (g glPipeline) toDomain() domain.Pipeline {
	out := domain.Pipeline{
		ID: strconv.FormatInt(g.ID, 10), Number: g.IID, Name: g.Name,
		Status: pipelineStatus(g.Status), RawStatus: g.Status,
		Ref: g.Ref, SHA: g.SHA, Event: g.Source, Actor: g.User.name(), WebURL: g.WebURL,
	}
	setTime(&out.CreatedAt, g.CreatedAt)
	setTime(&out.StartedAt, g.StartedAt)
	setTime(&out.FinishedAt, g.FinishedAt)
	if g.Duration != nil {
		out.Duration = time.Duration(*g.Duration * float64(time.Second))
	}
	return out
}

func (j glJob) toDomain() domain.PipelineJob {
	out := domain.PipelineJob{
		ID: strconv.FormatInt(j.ID, 10), Name: j.Name, Stage: j.Stage,
		Status: pipelineStatus(j.Status), RawStatus: j.Status, WebURL: j.WebURL,
	}
	setTime(&out.StartedAt, j.StartedAt)
	setTime(&out.FinishedAt, j.FinishedAt)
	return out
}

func (p *provider) ListPipelines(ctx context.Context, ref domain.RepositoryRef, opts forge.PipelineListOptions) ([]domain.Pipeline, error) {
	op := "list pipelines of " + ref.FullName()
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	q := url.Values{"order_by": {"id"}, "sort": {"desc"}}
	if opts.Ref != "" {
		q.Set("ref", opts.Ref)
	}
	if opts.Status != "" {
		switch opts.Status {
		case domain.PipelinePending, domain.PipelineRunning, domain.PipelineSuccess, domain.PipelineFailed,
			domain.PipelineCanceled, domain.PipelineSkipped, domain.PipelineManual:
			// The normalized names are also GitLab status values; "pending"
			// matches only GitLab's own pending state.
			q.Set("status", string(opts.Status))
		default:
			return nil, p.invalid(op, "cannot filter GitLab pipelines by status %q", opts.Status)
		}
	}
	pls, err := list[glPipeline](ctx, p, op, path+"/pipelines", q, opts.Limit, errs.ErrRepositoryNotFound)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Pipeline, 0, len(pls))
	for _, pl := range pls {
		out = append(out, pl.toDomain())
	}
	return out, nil
}

func pipelineIDPath(op string, p *provider, ref domain.RepositoryRef, id string) (string, error) {
	path, err := projectPath(ref)
	if err != nil {
		return "", withProvider(err, p.acct.Name, op)
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return "", p.invalid(op, "pipeline ID %q is not numeric", id)
	}
	return path + "/pipelines/" + id, nil
}

func (p *provider) jobs(ctx context.Context, op, pipelinePath string) ([]glJob, error) {
	jobs, err := list[glJob](ctx, p, op, pipelinePath+"/jobs", nil, 0, nil)
	if err != nil {
		return nil, err
	}
	// GitLab lists newest first; present jobs in creation (≈ stage) order.
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs, nil
}

func (p *provider) GetPipeline(ctx context.Context, ref domain.RepositoryRef, id string) (*domain.Pipeline, error) {
	op := "get pipeline " + id + " of " + ref.FullName()
	path, err := pipelineIDPath(op, p, ref, id)
	if err != nil {
		return nil, err
	}
	var g glPipeline
	if err := p.get(ctx, op, path, nil, &g); err != nil {
		return nil, err
	}
	out := g.toDomain()
	jobs, err := p.jobs(ctx, op, path)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, j.toDomain())
	}
	return &out, nil
}

// RunPipeline creates a pipeline for req.Ref (the project's default branch
// when empty). GitLab runs the project's .gitlab-ci.yml, so req.Workflow has
// no GitLab equivalent and is ignored.
func (p *provider) RunPipeline(ctx context.Context, ref domain.RepositoryRef, req forge.RunPipelineRequest) (*domain.Pipeline, error) {
	op := "run pipeline in " + ref.FullName()
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	gitRef := req.Ref
	if gitRef == "" {
		repo, err := p.GetRepository(ctx, ref)
		if err != nil {
			return nil, err
		}
		if gitRef = repo.DefaultBranch; gitRef == "" {
			return nil, p.invalid(op, "a ref is required (the project has no default branch)")
		}
	}
	body := map[string]any{"ref": gitRef}
	if len(req.Variables) > 0 {
		keys := make([]string, 0, len(req.Variables))
		for k := range req.Variables {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		vars := make([]map[string]string, 0, len(keys))
		for _, k := range keys {
			vars = append(vars, map[string]string{"key": k, "value": req.Variables[k]})
		}
		body["variables"] = vars
	}
	var g glPipeline
	if _, err := p.do(ctx, call{method: http.MethodPost, path: path + "/pipeline", op: op, body: body, notFound: errs.ErrRepositoryNotFound}, &g); err != nil {
		return nil, err
	}
	out := g.toDomain()
	return &out, nil
}

func (p *provider) CancelPipeline(ctx context.Context, ref domain.RepositoryRef, id string) error {
	op := "cancel pipeline " + id + " of " + ref.FullName()
	path, err := pipelineIDPath(op, p, ref, id)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, call{method: http.MethodPost, path: path + "/cancel", op: op}, nil)
	return err
}

// RetryPipeline retries the failed and canceled jobs of a pipeline.
func (p *provider) RetryPipeline(ctx context.Context, ref domain.RepositoryRef, id string) error {
	op := "retry pipeline " + id + " of " + ref.FullName()
	path, err := pipelineIDPath(op, p, ref, id)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, call{method: http.MethodPost, path: path + "/retry", op: op}, nil)
	return err
}

// PipelineLogs streams job traces. With an empty jobID it writes every job of
// the pipeline, each preceded by a "==> <stage>/<name>" header.
func (p *provider) PipelineLogs(ctx context.Context, ref domain.RepositoryRef, pipelineID, jobID string, w io.Writer) error {
	op := "get logs of pipeline " + pipelineID + " of " + ref.FullName()
	projPath, err := projectPath(ref)
	if err != nil {
		return withProvider(err, p.acct.Name, op)
	}
	if jobID != "" {
		if _, err := strconv.ParseInt(jobID, 10, 64); err != nil {
			return p.invalid(op, "job ID %q is not numeric", jobID)
		}
		_, err := p.trace(ctx, op, projPath, jobID, w)
		return err
	}
	path, err := pipelineIDPath(op, p, ref, pipelineID)
	if err != nil {
		return err
	}
	jobs, err := p.jobs(ctx, op, path)
	if err != nil {
		return err
	}
	for i, j := range jobs {
		if i > 0 {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "==> %s/%s\n", j.Stage, j.Name); err != nil {
			return err
		}
		n, err := p.trace(ctx, op, projPath, strconv.FormatInt(j.ID, 10), w)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			// Jobs that never started (created, manual, skipped) have no trace.
			if _, err := io.WriteString(w, "(no log available)\n"); err != nil {
				return err
			}
		case err != nil:
			return err
		case n == 0:
			if _, err := io.WriteString(w, "(no log output)\n"); err != nil {
				return err
			}
		}
	}
	return nil
}

// trace copies one job's plain-text trace to w, ensuring a trailing newline.
func (p *provider) trace(ctx context.Context, op, projPath, jobID string, w io.Writer) (int64, error) {
	resp, err := p.send(ctx, call{method: http.MethodGet, path: projPath + "/jobs/" + jobID + "/trace", op: op, accept: "text/plain"})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	lw := &lastByteWriter{w: w}
	n, err := io.Copy(lw, resp.Body)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return n, fmt.Errorf("%s: %w", op, ctxErr)
		}
		return n, err
	}
	if n > 0 && lw.last != '\n' {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return n, err
		}
	}
	return n, nil
}

type lastByteWriter struct {
	w    io.Writer
	last byte
}

func (l *lastByteWriter) Write(b []byte) (int, error) {
	n, err := l.w.Write(b)
	if n > 0 {
		l.last = b[n-1]
	}
	return n, err
}
