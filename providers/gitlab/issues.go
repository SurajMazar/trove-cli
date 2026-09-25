package gitlab

import (
	"context"
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

type glIssue struct {
	ID             int64       `json:"id"`
	IID            int         `json:"iid"`
	ProjectID      int64       `json:"project_id"`
	Title          string      `json:"title"`
	Description    string      `json:"description"`
	State          string      `json:"state"`
	Labels         []string    `json:"labels"`
	Author         *glUserRef  `json:"author"`
	Assignees      []glUserRef `json:"assignees"`
	UserNotesCount int         `json:"user_notes_count"`
	WebURL         string      `json:"web_url"`
	CreatedAt      *time.Time  `json:"created_at"`
	UpdatedAt      *time.Time  `json:"updated_at"`
	ClosedAt       *time.Time  `json:"closed_at"`
}

func (i glIssue) toDomain() domain.Issue {
	out := domain.Issue{
		ID: strconv.FormatInt(i.ID, 10), Number: i.IID, Title: i.Title, Body: i.Description,
		State: domain.IssueOpen, Author: i.Author.name(), Labels: i.Labels,
		Comments: i.UserNotesCount, WebURL: i.WebURL,
	}
	if i.State == "closed" {
		out.State = domain.IssueClosed
	}
	for _, a := range i.Assignees {
		out.Assignees = append(out.Assignees, a.Username)
	}
	setTime(&out.CreatedAt, i.CreatedAt)
	setTime(&out.UpdatedAt, i.UpdatedAt)
	setTime(&out.ClosedAt, i.ClosedAt)
	return out
}

func (p *provider) ListIssues(ctx context.Context, ref domain.RepositoryRef, opts forge.IssueListOptions) ([]domain.Issue, error) {
	op := "list issues of " + ref.FullName()
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	q := url.Values{}
	switch opts.State {
	case "", domain.IssueOpen:
		q.Set("state", "opened")
	case domain.IssueClosed:
		q.Set("state", "closed")
	case domain.IssueAll:
		// Omitting state returns issues in every state.
	default:
		return nil, p.invalid(op, "unknown issue state %q", opts.State)
	}
	if len(opts.Labels) > 0 {
		q.Set("labels", strings.Join(opts.Labels, ","))
	}
	if opts.Assignee != "" {
		q.Set("assignee_username", opts.Assignee)
	}
	if opts.Author != "" {
		q.Set("author_username", opts.Author)
	}
	issues, err := list[glIssue](ctx, p, op, path+"/issues", q, opts.Limit, errs.ErrRepositoryNotFound)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Issue, 0, len(issues))
	for _, i := range issues {
		out = append(out, i.toDomain())
	}
	return out, nil
}

// GetIssue fetches an issue by IID (the project-scoped #N).
func (p *provider) GetIssue(ctx context.Context, ref domain.RepositoryRef, number int) (*domain.Issue, error) {
	op := fmt.Sprintf("get issue #%d of %s", number, ref.FullName())
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	var i glIssue
	if err := p.get(ctx, op, path+"/issues/"+strconv.Itoa(number), nil, &i); err != nil {
		return nil, err
	}
	d := i.toDomain()
	return &d, nil
}

// resolveUserIDs maps usernames to user IDs; the issues API only accepts IDs.
func (p *provider) resolveUserIDs(ctx context.Context, op string, usernames []string) ([]int64, error) {
	ids := make([]int64, 0, len(usernames))
	for _, name := range usernames {
		name = strings.TrimPrefix(strings.TrimSpace(name), "@")
		if name == "" {
			continue
		}
		var users []glUserRef
		if err := p.get(ctx, op, "/users", url.Values{"username": {name}}, &users); err != nil {
			return nil, err
		}
		if len(users) == 0 {
			return nil, p.invalid(op, "unknown GitLab user %q", name)
		}
		ids = append(ids, users[0].ID)
	}
	return ids, nil
}

func (p *provider) CreateIssue(ctx context.Context, ref domain.RepositoryRef, req forge.CreateIssueRequest) (*domain.Issue, error) {
	op := "create issue in " + ref.FullName()
	if strings.TrimSpace(req.Title) == "" {
		return nil, p.invalid(op, "a title is required")
	}
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	body := map[string]any{"title": req.Title}
	if req.Body != "" {
		body["description"] = req.Body
	}
	if len(req.Labels) > 0 {
		body["labels"] = strings.Join(req.Labels, ",")
	}
	if len(req.Assignees) > 0 {
		ids, err := p.resolveUserIDs(ctx, op, req.Assignees)
		if err != nil {
			return nil, err
		}
		if len(ids) > 0 {
			body["assignee_ids"] = ids
		}
	}
	var i glIssue
	if _, err := p.do(ctx, call{method: http.MethodPost, path: path + "/issues", op: op, body: body, notFound: errs.ErrRepositoryNotFound}, &i); err != nil {
		return nil, err
	}
	d := i.toDomain()
	return &d, nil
}

func (p *provider) SetIssueState(ctx context.Context, ref domain.RepositoryRef, number int, state domain.IssueState) (*domain.Issue, error) {
	op := fmt.Sprintf("update issue #%d of %s", number, ref.FullName())
	var event string
	switch state {
	case domain.IssueClosed:
		event = "close"
	case domain.IssueOpen:
		event = "reopen"
	default:
		return nil, p.invalid(op, "issues can only be set to open or closed, not %q", state)
	}
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	var i glIssue
	if _, err := p.do(ctx, call{method: http.MethodPut, path: path + "/issues/" + strconv.Itoa(number), op: op, body: map[string]any{"state_event": event}}, &i); err != nil {
		return nil, err
	}
	d := i.toDomain()
	return &d, nil
}
