package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

type apiIssue struct {
	ID          int64           `json:"id"`
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        *string         `json:"body"`
	State       string          `json:"state"`
	StateReason *string         `json:"state_reason"`
	User        *apiOwner       `json:"user"`
	Assignees   []apiOwner      `json:"assignees"`
	Labels      []apiLabel      `json:"labels"`
	Comments    int             `json:"comments"`
	HTMLURL     string          `json:"html_url"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	ClosedAt    *time.Time      `json:"closed_at"`
	PullRequest json.RawMessage `json:"pull_request"`
}

// isPullRequest reports whether an issues-API item is really a PR: GitHub
// models every PR as an issue and includes them in issue listings.
func (i apiIssue) isPullRequest() bool {
	return len(i.PullRequest) > 0 && string(i.PullRequest) != "null"
}

func toIssue(i apiIssue) domain.Issue {
	state := domain.IssueOpen
	if i.State == "closed" {
		state = domain.IssueClosed
	}
	out := domain.Issue{
		ID: strconv.FormatInt(i.ID, 10), Number: i.Number, Title: i.Title, Body: str(i.Body),
		State: state, Assignees: logins(i.Assignees), Labels: labelNames(i.Labels), Comments: i.Comments,
		WebURL: i.HTMLURL, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt, ClosedAt: timeOf(i.ClosedAt),
	}
	if i.StateReason != nil && *i.StateReason != "" {
		out.RawState = *i.StateReason // completed, not_planned, reopened, duplicate
	}
	if i.User != nil {
		out.Author = i.User.Login
	}
	return out
}

// ListIssues implements forge.IssueProvider. Pull requests returned by the
// issues endpoint are skipped.
func (p *Provider) ListIssues(ctx context.Context, ref domain.RepositoryRef, opts forge.IssueListOptions) ([]domain.Issue, error) {
	q := url.Values{}
	switch opts.State {
	case "", domain.IssueOpen:
		q.Set("state", "open")
	case domain.IssueClosed, domain.IssueAll:
		q.Set("state", string(opts.State))
	default:
		return nil, p.errorf(errs.ErrInvalidArgument, "list issues", 0, "invalid issue state %q", opts.State)
	}
	if len(opts.Labels) > 0 {
		q.Set("labels", strings.Join(opts.Labels, ","))
	}
	if opts.Assignee != "" {
		q.Set("assignee", strings.TrimPrefix(opts.Assignee, "@"))
	}
	if opts.Author != "" {
		q.Set("creator", strings.TrimPrefix(opts.Author, "@"))
	}
	return paginate(ctx, p, listSpec[apiIssue]{
		request: request{op: "list issues", path: repoPath(ref.Namespace, ref.Name, "issues"), query: q,
			notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref)},
		perPage: maxPerPage, limit: opts.Limit,
	}, func(i apiIssue) (domain.Issue, bool) {
		return toIssue(i), !i.isPullRequest()
	})
}

// GetIssue implements forge.IssueProvider.
func (p *Provider) GetIssue(ctx context.Context, ref domain.RepositoryRef, number int) (*domain.Issue, error) {
	const op = "get issue"
	var i apiIssue
	_, err := p.do(ctx, request{op: op, method: http.MethodGet, path: repoPath(ref.Namespace, ref.Name, "issues", strconv.Itoa(number)),
		out: &i, notFoundMsg: fmt.Sprintf("issue #%d not found in %s", number, ref.FullName())})
	if err != nil {
		return nil, err
	}
	if i.isPullRequest() {
		return nil, &errs.Error{Kind: errs.ErrNotFound, Provider: p.acct.Name, Op: op, Status: http.StatusOK,
			Message: fmt.Sprintf("#%d in %s is a pull request, not an issue", number, ref.FullName()),
			Hint:    "use the pull request commands to view it"}
	}
	out := toIssue(i)
	return &out, nil
}

// CreateIssue implements forge.IssueCreator. Labels that do not exist are
// created by GitHub; assignees must have access to the repository.
func (p *Provider) CreateIssue(ctx context.Context, ref domain.RepositoryRef, req forge.CreateIssueRequest) (*domain.Issue, error) {
	const op = "create issue"
	if strings.TrimSpace(req.Title) == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "issue title is required")
	}
	body := map[string]any{"title": req.Title}
	if req.Body != "" {
		body["body"] = req.Body
	}
	if len(req.Labels) > 0 {
		body["labels"] = req.Labels
	}
	if len(req.Assignees) > 0 {
		as := make([]string, len(req.Assignees))
		for i, a := range req.Assignees {
			as[i] = strings.TrimPrefix(a, "@")
		}
		body["assignees"] = as
	}
	var i apiIssue
	_, err := p.do(ctx, request{op: op, method: http.MethodPost, path: repoPath(ref.Namespace, ref.Name, "issues"),
		body: body, out: &i, notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref)})
	if err != nil {
		return nil, err
	}
	out := toIssue(i)
	return &out, nil
}

// SetIssueState implements forge.IssueStateChanger.
func (p *Provider) SetIssueState(ctx context.Context, ref domain.RepositoryRef, number int, state domain.IssueState) (*domain.Issue, error) {
	const op = "set issue state"
	var body map[string]string
	switch state {
	case domain.IssueClosed:
		body = map[string]string{"state": "closed", "state_reason": "completed"}
	case domain.IssueOpen:
		body = map[string]string{"state": "open", "state_reason": "reopened"}
	default:
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "invalid issue state %q (want open or closed)", state)
	}
	var i apiIssue
	_, err := p.do(ctx, request{op: op, method: http.MethodPatch, path: repoPath(ref.Namespace, ref.Name, "issues", strconv.Itoa(number)),
		body: body, out: &i, notFoundMsg: fmt.Sprintf("issue #%d not found in %s", number, ref.FullName())})
	if err != nil {
		return nil, err
	}
	out := toIssue(i)
	return &out, nil
}
