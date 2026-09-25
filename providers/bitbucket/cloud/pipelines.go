package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

// bbState models pipeline and step states:
// {"name": "COMPLETED", "result": {"name": "SUCCESSFUL"}} or
// {"name": "IN_PROGRESS", "stage": {"name": "PAUSED"}}.
type bbState struct {
	Name   string `json:"name"`
	Result *struct {
		Name string `json:"name"`
	} `json:"result"`
	Stage *struct {
		Name string `json:"name"`
	} `json:"stage"`
}

func (s bbState) raw() string {
	switch {
	case s.Result != nil && s.Result.Name != "":
		return s.Name + "/" + s.Result.Name
	case s.Stage != nil && s.Stage.Name != "":
		return s.Name + "/" + s.Stage.Name
	}
	return s.Name
}

func (s bbState) status() domain.PipelineStatus {
	switch strings.ToUpper(s.Name) {
	case "PENDING", "READY", "PARSING":
		return domain.PipelinePending
	case "IN_PROGRESS", "RUNNING", "BUILDING":
		if s.Stage != nil {
			switch strings.ToUpper(s.Stage.Name) {
			case "PAUSED", "HALTED":
				return domain.PipelineManual
			}
		}
		return domain.PipelineRunning
	case "PAUSED", "HALTED":
		return domain.PipelineManual
	case "COMPLETED":
		if s.Result == nil {
			return domain.PipelineUnknown
		}
		switch strings.ToUpper(s.Result.Name) {
		case "SUCCESSFUL":
			return domain.PipelineSuccess
		case "FAILED", "ERROR":
			return domain.PipelineFailed
		case "STOPPED", "EXPIRED":
			return domain.PipelineCanceled
		case "NOT_RUN":
			return domain.PipelineSkipped
		}
	}
	return domain.PipelineUnknown
}

type bbPipeline struct {
	UUID        string     `json:"uuid"`
	BuildNumber int        `json:"build_number"`
	State       bbState    `json:"state"`
	CreatedOn   time.Time  `json:"created_on"`
	CompletedOn *time.Time `json:"completed_on"`
	Duration    int64      `json:"duration_in_seconds"`
	Creator     *bbAccount `json:"creator"`
	Trigger     *struct {
		Name string `json:"name"`
	} `json:"trigger"`
	Target struct {
		Type    string `json:"type"`
		RefType string `json:"ref_type"`
		RefName string `json:"ref_name"`
		Commit  *struct {
			Hash string `json:"hash"`
		} `json:"commit"`
		Selector *struct {
			Type    string `json:"type"`
			Pattern string `json:"pattern"`
		} `json:"selector"`
	} `json:"target"`
}

type bbStep struct {
	UUID        string     `json:"uuid"`
	Name        string     `json:"name"`
	State       bbState    `json:"state"`
	StartedOn   *time.Time `json:"started_on"`
	CompletedOn *time.Time `json:"completed_on"`
}

func (p *Provider) toPipeline(ref domain.RepositoryRef, pl bbPipeline) domain.Pipeline {
	out := domain.Pipeline{
		ID:        pl.UUID,
		Number:    pl.BuildNumber,
		Status:    pl.State.status(),
		RawStatus: pl.State.raw(),
		Ref:       pl.Target.RefName,
		Actor:     pl.Creator.handle(),
		CreatedAt: pl.CreatedOn,
		Duration:  time.Duration(pl.Duration) * time.Second,
	}
	if pl.BuildNumber > 0 {
		out.WebURL = p.webURL + "/" + ref.Namespace + "/" + ref.Name + "/pipelines/results/" + strconv.Itoa(pl.BuildNumber)
	}
	if pl.Target.Commit != nil {
		out.SHA = pl.Target.Commit.Hash
	}
	if s := pl.Target.Selector; s != nil && s.Pattern != "" {
		out.Name = s.Pattern
	}
	if pl.Trigger != nil {
		out.Event = strings.ToLower(pl.Trigger.Name)
	}
	if pl.CompletedOn != nil {
		out.FinishedAt = *pl.CompletedOn
	}
	return out
}

func (p *Provider) toJob(ref domain.RepositoryRef, pl *domain.Pipeline, s bbStep) domain.PipelineJob {
	j := domain.PipelineJob{
		ID:        s.UUID,
		Name:      firstNonEmpty(s.Name, s.UUID),
		Status:    s.State.status(),
		RawStatus: s.State.raw(),
	}
	if pl.WebURL != "" {
		j.WebURL = pl.WebURL + "/steps/" + s.UUID
	}
	if s.StartedOn != nil {
		j.StartedAt = *s.StartedOn
	}
	if s.CompletedOn != nil {
		j.FinishedAt = *s.CompletedOn
	}
	return j
}

// ListPipelines implements forge.PipelineProvider (newest first). Ref is
// filtered server-side (target.ref_name); Status is filtered client-side
// because one normalized status covers several Bitbucket states.
func (p *Provider) ListPipelines(ctx context.Context, ref domain.RepositoryRef, opts forge.PipelineListOptions) ([]domain.Pipeline, error) {
	const op = "list pipelines"
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	q := url.Values{"sort": {"-created_on"}, "pagelen": {itoa(forge.PageSize(opts.Limit, maxPageLen))}}
	if opts.Ref != "" {
		q.Set("target.ref_name", opts.Ref)
	}
	var filter func(bbPipeline) bool
	if opts.Status != "" {
		filter = func(pl bbPipeline) bool { return pl.State.status() == opts.Status }
	}
	raw, err := collectPages[bbPipeline](ctx, p, opts.Limit, []request{{op: op, url: path + "/pipelines", query: q,
		notFoundMsg: "repository " + ref.FullName() + " not found or Pipelines is not enabled"}}, filter)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Pipeline, 0, len(raw))
	for _, pl := range raw {
		out = append(out, p.toPipeline(ref, pl))
	}
	return out, nil
}

// GetPipeline implements forge.PipelineProvider. id is the pipeline UUID
// (with braces, as Bitbucket returns it); its steps become the jobs.
func (p *Provider) GetPipeline(ctx context.Context, ref domain.RepositoryRef, id string) (*domain.Pipeline, error) {
	const op = "get pipeline"
	base, err := p.pipelinePath(ref, id)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	var pl bbPipeline
	if err := p.do(ctx, request{op: op, method: http.MethodGet, url: base, notFoundMsg: "pipeline " + id + " not found"}, &pl); err != nil {
		return nil, err
	}
	out := p.toPipeline(ref, pl)
	steps, err := p.pipelineSteps(ctx, base)
	if err != nil {
		return nil, err
	}
	for _, s := range steps {
		j := p.toJob(ref, &out, s)
		out.Jobs = append(out.Jobs, j)
		if !j.StartedAt.IsZero() && (out.StartedAt.IsZero() || j.StartedAt.Before(out.StartedAt)) {
			out.StartedAt = j.StartedAt
		}
	}
	return &out, nil
}

func (p *Provider) pipelinePath(ref domain.RepositoryRef, id string) (string, error) {
	path, err := p.repoPath(ref)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(id) == "" {
		return "", p.invalidArg("", "a pipeline UUID is required")
	}
	return path + "/pipelines/" + pathEscape(strings.TrimSpace(id)), nil
}

func (p *Provider) pipelineSteps(ctx context.Context, base string) ([]bbStep, error) {
	return collectPages[bbStep](ctx, p, 0, []request{{op: "list pipeline steps", url: base + "/steps",
		query: url.Values{"pagelen": {itoa(maxPageLen)}}}}, nil)
}

// RunPipeline implements forge.PipelineRunner. It triggers the pipeline for
// the branch req.Ref; req.Workflow selects a custom pipeline
// (selector type "custom") defined in bitbucket-pipelines.yml.
func (p *Provider) RunPipeline(ctx context.Context, ref domain.RepositoryRef, req forge.RunPipelineRequest) (*domain.Pipeline, error) {
	const op = "run pipeline"
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	if strings.TrimSpace(req.Ref) == "" {
		return nil, p.invalidArg(op, "a branch is required to run a Bitbucket pipeline")
	}
	target := map[string]any{"type": "pipeline_ref_target", "ref_type": "branch", "ref_name": req.Ref}
	if req.Workflow != "" {
		target["selector"] = map[string]string{"type": "custom", "pattern": req.Workflow}
	}
	body := map[string]any{"target": target}
	if len(req.Variables) > 0 {
		keys := make([]string, 0, len(req.Variables))
		for k := range req.Variables {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		vars := make([]map[string]any, 0, len(keys))
		for _, k := range keys {
			vars = append(vars, map[string]any{"key": k, "value": req.Variables[k], "secured": false})
		}
		body["variables"] = vars
	}
	var pl bbPipeline
	if err := p.do(ctx, request{op: op, method: http.MethodPost, url: path + "/pipelines", body: body,
		notFoundMsg: "repository " + ref.FullName() + " or branch " + req.Ref + " not found"}, &pl); err != nil {
		return nil, err
	}
	out := p.toPipeline(ref, pl)
	return &out, nil
}

// CancelPipeline implements forge.PipelineCanceler (POST .../stopPipeline).
func (p *Provider) CancelPipeline(ctx context.Context, ref domain.RepositoryRef, id string) error {
	const op = "stop pipeline"
	base, err := p.pipelinePath(ref, id)
	if err != nil {
		return p.wrapOp(op, err)
	}
	return p.do(ctx, request{op: op, method: http.MethodPost, url: base + "/stopPipeline",
		notFoundMsg: "pipeline " + id + " not found"}, nil)
}

// PipelineLogs implements forge.PipelineLogger. Completed step logs are
// served through a 307 redirect to long-term storage; the redirect is
// followed without our Authorization header (see checkRedirect). With an
// empty jobID every step is written, each preceded by "==> <step name>".
func (p *Provider) PipelineLogs(ctx context.Context, ref domain.RepositoryRef, pipelineID, jobID string, w io.Writer) error {
	const op = "get pipeline logs"
	base, err := p.pipelinePath(ref, pipelineID)
	if err != nil {
		return p.wrapOp(op, err)
	}
	if jobID != "" {
		return p.stepLog(ctx, base, jobID, w)
	}
	steps, err := p.pipelineSteps(ctx, base)
	if err != nil {
		return err
	}
	for i, s := range steps {
		if i > 0 {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "==> %s\n", firstNonEmpty(s.Name, s.UUID)); err != nil {
			return err
		}
		err := p.stepLog(ctx, base, s.UUID, w)
		if errors.Is(err, errs.ErrNotFound) {
			// Steps that have not run yet (or were skipped) have no log.
			if _, err := fmt.Fprintf(w, "(no log available: step is %s)\n", strings.ToLower(s.State.raw())); err != nil {
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

func (p *Provider) stepLog(ctx context.Context, base, stepID string, w io.Writer) error {
	const op = "get pipeline step log"
	resp, err := p.send(ctx, request{op: op, method: http.MethodGet, url: base + "/steps/" + pathEscape(stepID) + "/log",
		accept: "application/octet-stream, text/plain, */*", notFoundMsg: "log for step " + stepID + " not found"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, resp.Body); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("%s: %w", op, cerr)
		}
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}
