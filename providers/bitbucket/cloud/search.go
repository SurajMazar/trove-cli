package cloud

import (
	"context"
	"net/url"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

type bbSearchSegment struct {
	Text  string `json:"text"`
	Match bool   `json:"match"`
}

type bbCodeResult struct {
	File struct {
		Path   string  `json:"path"`
		Links  bbLinks `json:"links"`
		Commit *struct {
			Hash       string `json:"hash"`
			Repository *struct {
				FullName string  `json:"full_name"`
				Links    bbLinks `json:"links"`
			} `json:"repository"`
		} `json:"commit"`
	} `json:"file"`
	ContentMatches []struct {
		Lines []struct {
			Line     int               `json:"line"`
			Segments []bbSearchSegment `json:"segments"`
		} `json:"lines"`
	} `json:"content_matches"`
}

// Search implements forge.Searcher.
//
//   - Repositories: BBQL name~"<query>" per workspace (q.Namespace, or every
//     workspace of the caller).
//   - Code: GET /workspaces/{workspace}/search/code; needs a workspace from
//     q.Namespace, q.Repository or the account's `workspace` setting.
//     q.Repository narrows the search with the "repo:" qualifier. Atlassian
//     has announced this API's deprecation for 1 November 2026.
//   - Issues: unsupported; the Bitbucket Cloud issue tracker and its API
//     were removed on 20 August 2026.
func (p *Provider) Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	switch q.Kind {
	case domain.SearchRepositories:
		return p.searchRepos(ctx, q)
	case domain.SearchCode:
		return p.searchCode(ctx, q)
	case domain.SearchIssues:
		return nil, errs.Unsupported(p.acct.Name, "issue search (the Bitbucket Cloud issue tracker was removed on 20 August 2026)")
	default:
		return nil, p.invalidArg("search", "unknown search kind "+string(q.Kind))
	}
}

func (p *Provider) searchRepos(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	const op = "search repositories"
	term := strings.TrimSpace(q.Query)
	if term == "" {
		return nil, p.invalidArg(op, "a search query is required")
	}
	ns := q.Namespace
	if ns == "" && q.Repository != nil {
		ns = q.Repository.Namespace
	}
	repos, err := p.searchRepositories(ctx, op, ns, false, []string{"name~" + bbqlString(term)}, q.Limit)
	if err != nil {
		return nil, err
	}
	return &domain.SearchResult{Kind: domain.SearchRepositories, Total: len(repos), Repositories: repos}, nil
}

func (p *Provider) searchCode(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	const op = "search code"
	term := strings.TrimSpace(q.Query)
	if term == "" {
		return nil, p.invalidArg(op, "a search query is required")
	}
	ws := q.Namespace
	if q.Repository != nil && q.Repository.Namespace != "" {
		ws = q.Repository.Namespace
	}
	ws = firstNonEmpty(ws, p.workspace)
	if ws == "" {
		return nil, p.invalidArg(op, "Bitbucket code search is scoped to a workspace: pass a namespace or configure `workspace` for this account")
	}
	if q.Repository != nil && q.Repository.Name != "" {
		term += " repo:" + q.Repository.Name
	}
	params := url.Values{
		"search_query": {term},
		"pagelen":      {itoa(forge.PageSize(q.Limit, maxPageLen))},
		// Ask for the repository of each hit, which is not included by default.
		"fields": {"+values.file.commit.repository"},
	}
	total := 0
	env, err := getPage[bbCodeResult](ctx, p, request{op: op, url: "/workspaces/" + pathEscape(ws) + "/search/code", query: params,
		notFoundMsg: "workspace " + ws + " not found"})
	if err != nil {
		return nil, err
	}
	if env.Size != nil {
		total = *env.Size
	}
	hits := env.Values
	if next := env.Next; next != "" && (q.Limit <= 0 || len(hits) < q.Limit) {
		rest, err := collectPages[bbCodeResult](ctx, p, remaining(q.Limit, len(hits)), []request{{op: op, url: next}}, nil)
		if err != nil {
			return nil, err
		}
		hits = append(hits, rest...)
	}
	if q.Limit > 0 && len(hits) > q.Limit {
		hits = hits[:q.Limit]
	}
	res := &domain.SearchResult{Kind: domain.SearchCode, Total: total}
	for _, h := range hits {
		res.Code = append(res.Code, p.toCodeResult(h))
	}
	if res.Total < len(res.Code) {
		res.Total = len(res.Code)
	}
	return res, nil
}

func remaining(limit, have int) int {
	if limit <= 0 {
		return 0
	}
	return limit - have
}

func (p *Provider) toCodeResult(h bbCodeResult) domain.CodeResult {
	cr := domain.CodeResult{Path: h.File.Path, WebURL: h.File.Links.Self.Href}
	if c := h.File.Commit; c != nil {
		cr.Ref = c.Hash
		if c.Repository != nil {
			cr.Repository = c.Repository.FullName
			if c.Hash != "" && c.Repository.FullName != "" {
				cr.WebURL = p.webURL + "/" + c.Repository.FullName + "/src/" + c.Hash + "/" + pathEscapeAll(h.File.Path)
			}
		}
	}
	var lines []string
	for _, m := range h.ContentMatches {
		for _, l := range m.Lines {
			var b strings.Builder
			for _, s := range l.Segments {
				b.WriteString(s.Text)
			}
			lines = append(lines, b.String())
		}
	}
	cr.Fragment = strings.Join(lines, "\n")
	return cr
}
