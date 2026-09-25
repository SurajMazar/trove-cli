package gitlab

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

// Notifications are GitLab To-Do items. GitLab keeps two states: "pending"
// (unread) and "done"; marking a notification read marks the To-Do done.

type glTodo struct {
	ID         int64  `json:"id"`
	ActionName string `json:"action_name"` // assigned, mentioned, review_requested, ...
	TargetType string `json:"target_type"` // Issue, MergeRequest, Commit, ...
	TargetURL  string `json:"target_url"`
	Body       string `json:"body"`
	State      string `json:"state"`
	Target     *struct {
		Title string `json:"title"`
	} `json:"target"`
	Project *struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	CreatedAt *time.Time `json:"created_at"`
	UpdatedAt *time.Time `json:"updated_at"`
}

func (t glTodo) toDomain() domain.Notification {
	n := domain.Notification{
		ID: strconv.FormatInt(t.ID, 10), Reason: t.ActionName, Type: t.TargetType,
		Unread: t.State == "pending", WebURL: t.TargetURL,
	}
	if t.Target != nil {
		n.Title = t.Target.Title
	}
	if n.Title == "" {
		n.Title = t.Body
	}
	if t.Project != nil {
		n.Repository = t.Project.PathWithNamespace
	}
	if t.UpdatedAt != nil {
		n.UpdatedAt = *t.UpdatedAt
	} else {
		setTime(&n.UpdatedAt, t.CreatedAt)
	}
	return n
}

// ListNotifications lists pending To-Do items, or pending followed by done
// items when opts.All is set. (GET /todos returns only pending items unless
// state=done is requested, so "all" takes two listings.)
func (p *provider) ListNotifications(ctx context.Context, opts forge.NotificationListOptions) ([]domain.Notification, error) {
	const op = "list To-Do items"
	todos, err := list[glTodo](ctx, p, op, "/todos", url.Values{"state": {"pending"}}, opts.Limit, nil)
	if err != nil {
		return nil, err
	}
	if opts.All && (opts.Limit <= 0 || len(todos) < opts.Limit) {
		rest := 0
		if opts.Limit > 0 {
			rest = opts.Limit - len(todos)
		}
		done, err := list[glTodo](ctx, p, op, "/todos", url.Values{"state": {"done"}}, rest, nil)
		if err != nil {
			return nil, err
		}
		todos = append(todos, done...)
	}
	out := make([]domain.Notification, 0, len(todos))
	for _, t := range todos {
		out = append(out, t.toDomain())
	}
	return out, nil
}

// GetNotification finds a To-Do by ID. GitLab has no GET /todos/:id, so the
// pending and then the done lists are scanned page by page until it is found.
func (p *provider) GetNotification(ctx context.Context, id string) (*domain.Notification, error) {
	op := "get To-Do item " + id
	want, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, p.invalid(op, "To-Do ID %q is not numeric", id)
	}
	for _, state := range []string{"pending", "done"} {
		next := p.endpoint("/todos", url.Values{"state": {state}, "per_page": {strconv.Itoa(maxPerPage)}})
		seen := map[string]bool{}
		for next != "" && !seen[next] {
			seen[next] = true
			var page []glTodo
			h, err := p.do(ctx, call{method: http.MethodGet, rawURL: next, op: op}, &page)
			if err != nil {
				return nil, err
			}
			for _, t := range page {
				if t.ID == want {
					n := t.toDomain()
					return &n, nil
				}
			}
			if len(page) == 0 {
				break
			}
			next = p.nextPage(h, next)
		}
	}
	return nil, p.notFound(op, "To-Do item %s was not found", id)
}

func (p *provider) MarkNotificationRead(ctx context.Context, id string) error {
	op := "mark To-Do item " + id + " as done"
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return p.invalid(op, "To-Do ID %q is not numeric", id)
	}
	_, err := p.do(ctx, call{method: http.MethodPost, path: "/todos/" + id + "/mark_as_done", op: op}, nil)
	return err
}

func (p *provider) MarkAllNotificationsRead(ctx context.Context) error {
	_, err := p.do(ctx, call{method: http.MethodPost, path: "/todos/mark_as_done", op: "mark all To-Do items as done"}, nil)
	return err
}
