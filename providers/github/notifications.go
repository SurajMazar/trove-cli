package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

type apiThread struct {
	ID        string    `json:"id"`
	Unread    bool      `json:"unread"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
	Subject   struct {
		Title string  `json:"title"`
		URL   *string `json:"url"`
		Type  string  `json:"type"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
		HTMLURL  string `json:"html_url"`
	} `json:"repository"`
}

func (p *Provider) toNotification(t apiThread) domain.Notification {
	return domain.Notification{
		ID: t.ID, Title: t.Subject.Title, Reason: t.Reason, Type: t.Subject.Type,
		Repository: t.Repository.FullName, Unread: t.Unread, WebURL: p.subjectWebURL(t), UpdatedAt: t.UpdatedAt,
	}
}

// subjectWebURL derives a browser URL from the subject's API URL
// (".../repos/o/r/pulls/5" -> "<web>/o/r/pull/5"), falling back to the
// repository page. Notifications carry no html_url of their own.
func (p *Provider) subjectWebURL(t apiThread) string {
	fallback := t.Repository.HTMLURL
	api := str(t.Subject.URL)
	prefix := strings.TrimRight(p.api.String(), "/") + "/repos/"
	if !strings.HasPrefix(api, prefix) {
		return fallback
	}
	parts := strings.Split(strings.TrimPrefix(api, prefix), "/")
	if len(parts) < 2 {
		return fallback
	}
	repoWeb := p.web + "/" + parts[0] + "/" + parts[1]
	if fallback == "" {
		fallback = repoWeb
	}
	if len(parts) < 4 {
		return repoWeb
	}
	switch parts[2] {
	case "pulls":
		return repoWeb + "/pull/" + parts[3]
	case "issues":
		return repoWeb + "/issues/" + parts[3]
	case "commits":
		return repoWeb + "/commit/" + parts[3]
	case "discussions":
		return repoWeb + "/discussions/" + parts[3]
	case "releases":
		// The API URL has the numeric release ID, not the tag.
		return repoWeb + "/releases"
	case "actions":
		return repoWeb + "/actions"
	}
	return fallback
}

// ListNotifications implements forge.NotificationProvider (page size is at
// most 50 on this endpoint).
func (p *Provider) ListNotifications(ctx context.Context, opts forge.NotificationListOptions) ([]domain.Notification, error) {
	q := url.Values{"all": {strconv.FormatBool(opts.All)}}
	return paginate(ctx, p, listSpec[apiThread]{
		request: request{op: "list notifications", path: "/notifications", query: q},
		perPage: maxNotificationsPerPage, limit: opts.Limit,
	}, func(t apiThread) (domain.Notification, bool) { return p.toNotification(t), true })
}

func (p *Provider) threadPath(op, id string) (string, error) {
	n, err := p.parseNumericID(op, "notification thread ID", id)
	if err != nil {
		return "", err
	}
	return "/notifications/threads/" + strconv.FormatInt(n, 10), nil
}

// GetNotification implements forge.NotificationProvider.
func (p *Provider) GetNotification(ctx context.Context, id string) (*domain.Notification, error) {
	const op = "get notification"
	path, err := p.threadPath(op, id)
	if err != nil {
		return nil, err
	}
	var t apiThread
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: path, out: &t,
		notFoundMsg: fmt.Sprintf("notification thread %s not found", id)}); err != nil {
		return nil, err
	}
	out := p.toNotification(t)
	return &out, nil
}

// MarkNotificationRead implements forge.NotificationProvider (205 Reset Content).
func (p *Provider) MarkNotificationRead(ctx context.Context, id string) error {
	const op = "mark notification read"
	path, err := p.threadPath(op, id)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, request{op: op, method: http.MethodPatch, path: path,
		notFoundMsg: fmt.Sprintf("notification thread %s not found", id)})
	return err
}

// MarkAllNotificationsRead implements forge.NotificationProvider. GitHub
// answers 205, or 202 when it marks a large backlog asynchronously.
func (p *Provider) MarkAllNotificationsRead(ctx context.Context) error {
	_, err := p.do(ctx, request{op: "mark all notifications read", method: http.MethodPut, path: "/notifications",
		body: map[string]any{"read": true}})
	return err
}
