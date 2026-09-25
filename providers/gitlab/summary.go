package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/SurajMazar/trove-cli/internal/domain"
)

// Summary returns dashboard counts from the X-Total header of one-item
// listings. GitLab omits X-Total for collections above 10,000 items, in
// which case the count is left nil. Running pipelines have no cross-project
// endpoint and are reported as unavailable.
func (p *provider) Summary(ctx context.Context) (*domain.AccountSummary, error) {
	count := func(op, path string, q url.Values) (*int, error) {
		q.Set("per_page", "1")
		var discard []json.RawMessage
		h, err := p.do(ctx, call{method: http.MethodGet, path: path, query: q, op: op}, &discard)
		if err != nil {
			return nil, err
		}
		return total(h), nil
	}
	var s domain.AccountSummary
	var err error
	if s.Repositories, err = count("count projects", "/projects", url.Values{"membership": {"true"}, "simple": {"true"}}); err != nil {
		return nil, err
	}
	if s.OpenPullRequests, err = count("count merge requests", "/merge_requests", url.Values{"state": {"opened"}, "scope": {"created_by_me"}}); err != nil {
		return nil, err
	}
	if s.OpenIssues, err = count("count issues", "/issues", url.Values{"state": {"opened"}, "scope": {"assigned_to_me"}}); err != nil {
		return nil, err
	}
	if s.Unread, err = count("count To-Do items", "/todos", url.Values{"state": {"pending"}}); err != nil {
		return nil, err
	}
	return &s, nil
}
