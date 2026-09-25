package github

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
)

type apiCodeResult struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	SHA        string `json:"sha"`
	HTMLURL    string `json:"html_url"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	TextMatches []struct {
		Fragment string `json:"fragment"`
	} `json:"text_matches"`
}

// issueTypeQualifier matches qualifiers that already choose between issues
// and pull requests.
var issueTypeQualifier = regexp.MustCompile(`(?i)(^|\s)-?(is|type):(issue|pr|pull-request)\b`)

// Search implements forge.Searcher using the REST search API. GitHub caps
// search at 1,000 results per query; issue search requires an is:issue or
// is:pr qualifier for some token types, so is:issue is added unless the
// query already chooses. Code search requires authentication and searches
// default branches only.
func (p *Provider) Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	const op = "search"
	text := strings.TrimSpace(q.Query)
	if text == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "a search query is required")
	}
	scope, err := p.searchScope(ctx, q)
	if err != nil {
		return nil, err
	}
	if scope != "" {
		text += " " + scope
	}
	res := &domain.SearchResult{Kind: q.Kind}
	switch q.Kind {
	case domain.SearchRepositories:
		res.Repositories, err = paginate(ctx, p, listSpec[apiRepo]{
			request: request{op: "search repositories", path: "/search/repositories", query: url.Values{"q": {text}}},
			perPage: maxPerPage, limit: q.Limit, decode: envelope[apiRepo]("items", &res.Total),
		}, func(r apiRepo) (domain.Repository, bool) { return p.toRepository(r), true })
	case domain.SearchIssues:
		if !issueTypeQualifier.MatchString(text) {
			text += " is:issue"
		}
		res.Issues, err = paginate(ctx, p, listSpec[apiIssue]{
			request: request{op: "search issues", path: "/search/issues", query: url.Values{"q": {text}}},
			perPage: maxPerPage, limit: q.Limit, decode: envelope[apiIssue]("items", &res.Total),
		}, func(i apiIssue) (domain.Issue, bool) { return toIssue(i), true })
	case domain.SearchCode:
		res.Code, err = paginate(ctx, p, listSpec[apiCodeResult]{
			request: request{op: "search code", path: "/search/code", query: url.Values{"q": {text}},
				// The text-match media type adds matching fragments.
				accept: "application/vnd.github.text-match+json"},
			perPage: maxPerPage, limit: q.Limit, decode: envelope[apiCodeResult]("items", &res.Total),
		}, func(c apiCodeResult) (domain.CodeResult, bool) {
			out := domain.CodeResult{Repository: c.Repository.FullName, Path: c.Path, Ref: c.SHA, WebURL: c.HTMLURL}
			if len(c.TextMatches) > 0 {
				out.Fragment = c.TextMatches[0].Fragment
			}
			return out, true
		})
	default:
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "unsupported search kind %q (want repositories, issues or code)", q.Kind)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// searchScope turns a repository or namespace scope into a qualifier. The
// namespace is resolved once to choose between org: and user:.
func (p *Provider) searchScope(ctx context.Context, q domain.SearchQuery) (string, error) {
	if q.Repository != nil && !q.Repository.IsZero() {
		return "repo:" + q.Repository.FullName(), nil
	}
	ns := strings.TrimSpace(q.Namespace)
	if ns == "" {
		return "", nil
	}
	n, err := p.GetNamespace(ctx, ns)
	if err != nil {
		return "", err
	}
	if n.Type == domain.NamespaceOrganization {
		return "org:" + n.FullPath, nil
	}
	return "user:" + n.FullPath, nil
}
