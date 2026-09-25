package github

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/httpx"
)

// Summary implements forge.SummaryProvider with a handful of cheap calls.
// Counts GitHub cannot provide stay nil: there is no account-wide API for
// running workflow runs, fine-grained tokens may omit total_private_repos,
// and search qualifiers such as author:@me are unavailable to some tokens.
func (p *Provider) Summary(ctx context.Context) (*domain.AccountSummary, error) {
	const op = "account summary"
	c, err := p.credential(ctx)
	if err != nil {
		return nil, err
	}
	if p.isAppCredential(c) {
		return p.appSummary(ctx)
	}
	u, err := p.profile(ctx, op)
	if err != nil {
		return nil, err
	}
	s := &domain.AccountSummary{}
	if u.PublicRepos != nil && u.TotalPrivateRepos != nil {
		n := *u.PublicRepos + *u.TotalPrivateRepos
		s.Repositories = &n
	}
	if s.OpenPullRequests, err = p.searchCount(ctx, "is:pr is:open author:@me"); err != nil {
		return nil, err
	}
	if s.OpenIssues, err = p.searchCount(ctx, "is:issue is:open assignee:@me"); err != nil {
		return nil, err
	}
	if s.Unread, err = p.unreadCount(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// optional turns non-fatal errors into "not available". Authentication,
// rate limit and cancellation errors are still returned.
func optional(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errs.ErrCanceled), errors.Is(err, errs.ErrAuthenticationFailed),
		errors.Is(err, errs.ErrNotAuthenticated), errors.Is(err, errs.ErrRateLimited):
		return err
	}
	return nil
}

func (p *Provider) searchCount(ctx context.Context, q string) (*int, error) {
	var out struct {
		TotalCount *int `json:"total_count"`
	}
	_, err := p.do(ctx, request{op: "account summary", method: http.MethodGet, path: "/search/issues",
		query: url.Values{"q": {q}, "per_page": {"1"}}, out: &out})
	if err != nil {
		return nil, optional(err)
	}
	return out.TotalCount, nil
}

// unreadCount counts unread notifications exactly with one request: with
// per_page=1 the page number in the Link rel="last" URL equals the count.
func (p *Provider) unreadCount(ctx context.Context) (*int, error) {
	var items []apiThread
	resp, err := p.do(ctx, request{op: "account summary", method: http.MethodGet, path: "/notifications",
		query: url.Values{"all": {"false"}, "per_page": {"1"}}, out: &items})
	if err != nil {
		return nil, optional(err)
	}
	links := httpx.ParseLinkHeader(resp.Header.Get("Link"))
	if last, ok := links["last"]; ok {
		if u, err := url.Parse(last); err == nil {
			if n, err := strconv.Atoi(u.Query().Get("page")); err == nil && n >= 0 {
				return &n, nil
			}
		}
		return nil, nil
	}
	if _, more := links["next"]; more {
		return nil, nil // count unknown without walking every page
	}
	n := len(items)
	return &n, nil
}

// appSummary reports what an App installation can see: its repositories.
func (p *Provider) appSummary(ctx context.Context) (*domain.AccountSummary, error) {
	var out struct {
		TotalCount *int `json:"total_count"`
	}
	if _, err := p.do(ctx, request{op: "account summary", method: http.MethodGet, path: "/installation/repositories",
		query: url.Values{"per_page": {"1"}}, out: &out}); err != nil {
		return nil, err
	}
	return &domain.AccountSummary{Repositories: out.TotalCount}, nil
}
