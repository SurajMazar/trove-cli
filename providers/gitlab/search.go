package gitlab

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
)

// defaultSearchLimit bounds searches without an explicit limit; global
// searches on large instances can otherwise page through millions of hits.
const defaultSearchLimit = 100

type glBlob struct {
	Basename  string `json:"basename"`
	Data      string `json:"data"`
	Path      string `json:"path"`
	Filename  string `json:"filename"`
	Ref       string `json:"ref"`
	Startline int    `json:"startline"`
	ProjectID int64  `json:"project_id"`
}

// Search searches projects, issues or code.
//
//   - repositories: GET /projects?search= (or /groups/:id/projects with
//     include_subgroups). Unlike /search?scope=projects this returns the full
//     project entity, including visibility and archive state.
//   - issues: GET /search?scope=issues, /groups/:id/search or
//     /projects/:id/search.
//   - code: scope=blobs. Project-level code search works on every instance;
//     global and group code search need Advanced Search (Elasticsearch/
//     OpenSearch) or exact code search (Zoekt), which GitLab.com and many
//     Premium/Ultimate instances have but most Free self-managed ones do not.
func (p *provider) Search(ctx context.Context, q domain.SearchQuery) (*domain.SearchResult, error) {
	op := "search GitLab " + string(q.Kind)
	if strings.TrimSpace(q.Query) == "" {
		return nil, p.invalid(op, "a search query is required")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	ns := strings.Trim(q.Namespace, "/")
	res := &domain.SearchResult{Kind: q.Kind}

	switch q.Kind {
	case domain.SearchRepositories:
		if q.Repository != nil {
			return nil, p.invalid(op, "project search cannot be scoped to a single project; scope it to a group instead")
		}
		params := url.Values{"search": {q.Query}, "order_by": {"last_activity_at"}, "simple": {"false"}}
		path := "/projects"
		if ns != "" {
			path = "/groups/" + url.PathEscape(ns) + "/projects"
			params.Set("include_subgroups", "true")
		}
		projects, tot, err := pages[glProject](ctx, p, op, path, params, limit, nil)
		if err != nil {
			return nil, err
		}
		for _, pr := range projects {
			res.Repositories = append(res.Repositories, p.toRepository(pr))
		}
		res.Total = totalOr(tot, len(res.Repositories))
		return res, nil

	case domain.SearchIssues:
		path, err := p.searchPath(op, q.Repository, ns)
		if err != nil {
			return nil, err
		}
		issues, tot, err := pages[glIssue](ctx, p, op, path, url.Values{"scope": {"issues"}, "search": {q.Query}}, limit, nil)
		if err != nil {
			return nil, err
		}
		for _, i := range issues {
			res.Issues = append(res.Issues, i.toDomain())
		}
		res.Total = totalOr(tot, len(res.Issues))
		return res, nil

	case domain.SearchCode:
		path, err := p.searchPath(op, q.Repository, ns)
		if err != nil {
			return nil, err
		}
		blobs, tot, err := pages[glBlob](ctx, p, op, path, url.Values{"scope": {"blobs"}, "search": {q.Query}}, limit, nil)
		if err != nil {
			if q.Repository == nil && advancedSearchMissing(err) {
				e := errs.Unsupported(p.acct.Name, "instance-wide or group-wide code search")
				e.Op = op
				e.Message = "this GitLab instance only supports code search within a single project (group and global code search need Advanced Search or exact code search)"
				e.Hint = "scope the search to a project, e.g. --repo group/project"
				e.Cause = err
				return nil, e
			}
			return nil, err
		}
		var repoName string
		if q.Repository != nil {
			repoName = q.Repository.FullName()
		}
		for _, b := range blobs {
			res.Code = append(res.Code, p.toCodeResult(ctx, b, repoName))
		}
		res.Total = totalOr(tot, len(res.Code))
		return res, nil
	}
	return nil, p.invalid(op, "unknown search kind %q", q.Kind)
}

func (p *provider) searchPath(op string, repo *domain.RepositoryRef, ns string) (string, error) {
	switch {
	case repo != nil:
		path, err := projectPath(*repo)
		if err != nil {
			return "", withProvider(err, p.acct.Name, op)
		}
		return path + "/search", nil
	case ns != "":
		return "/groups/" + url.PathEscape(ns) + "/search", nil
	default:
		return "/search", nil
	}
}

// advancedSearchMissing recognizes GitLab's answers to a blobs search that
// needs Advanced Search: EE "Scope supported only with advanced search or
// exact code search", older EE "Scope not supported without Elasticsearch!",
// and CE's parameter validation "scope does not have a valid value".
func advancedSearchMissing(err error) bool {
	var e *errs.Error
	if !errors.As(err, &e) || (e.Status != http.StatusBadRequest && e.Status != http.StatusForbidden) {
		return false
	}
	m := strings.ToLower(e.Message)
	for _, s := range []string{"advanced search", "elasticsearch", "exact code search", "does not have a valid value", "is not available for this search"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

func (p *provider) toCodeResult(ctx context.Context, b glBlob, repoName string) domain.CodeResult {
	path := b.Path
	if path == "" {
		path = b.Filename
	}
	out := domain.CodeResult{Path: path, Ref: b.Ref, Fragment: b.Data, Repository: repoName}
	if out.Repository == "" {
		out.Repository = p.projectFullPath(ctx, b.ProjectID)
	}
	if out.Repository == "" {
		// The project could not be read; report its ID rather than nothing.
		out.Repository = strconv.FormatInt(b.ProjectID, 10)
		return out
	}
	if b.Ref != "" && path != "" {
		out.WebURL = p.webURL + "/" + out.Repository + "/-/blob/" + escapeSegments(b.Ref) + "/" + escapeSegments(path)
		if b.Startline > 0 {
			out.WebURL += "#L" + strconv.Itoa(b.Startline)
		}
	}
	return out
}

// escapeSegments escapes each "/"-separated segment of a path.
func escapeSegments(s string) string {
	parts := strings.Split(s, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func totalOr(t *int, n int) int {
	if t != nil {
		return *t
	}
	return n
}
