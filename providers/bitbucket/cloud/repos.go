package cloud

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

type bbWorkspaceRef struct {
	Slug string `json:"slug"`
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type bbBranch struct {
	Name string `json:"name"`
}

type bbRepository struct {
	UUID        string          `json:"uuid"`
	Name        string          `json:"name"`
	Slug        string          `json:"slug"`
	FullName    string          `json:"full_name"`
	Description string          `json:"description"`
	IsPrivate   bool            `json:"is_private"`
	Language    string          `json:"language"`
	CreatedOn   time.Time       `json:"created_on"`
	UpdatedOn   time.Time       `json:"updated_on"`
	Mainbranch  *bbBranch       `json:"mainbranch"`
	Parent      *bbRepository   `json:"parent"`
	Workspace   *bbWorkspaceRef `json:"workspace"`
	Size        int64           `json:"size"`
	Links       bbLinks         `json:"links"`
}

// ref splits the repository into workspace slug and repository slug.
func (r bbRepository) ref() (ws, slug string) {
	if i := strings.Index(r.FullName, "/"); i > 0 {
		ws, slug = r.FullName[:i], r.FullName[i+1:]
	}
	if r.Workspace != nil && r.Workspace.Slug != "" {
		ws = r.Workspace.Slug
	}
	if r.Slug != "" {
		slug = r.Slug
	}
	return ws, slug
}

func (p *Provider) toRepository(r bbRepository) domain.Repository {
	ws, slug := r.ref()
	ref := domain.RepositoryRef{Namespace: ws, Name: slug}
	urls := p.RepositoryURLs(ref)
	if r.Links.HTML.Href != "" {
		urls.Web = r.Links.HTML.Href
	}
	if r.Links.Self.Href != "" {
		urls.API = r.Links.Self.Href
	}
	// Prefer the clone links from the payload unless the account overrides
	// clone hosts. Bitbucket embeds the requesting user's name in the HTTPS
	// link ("https://jdoe@bitbucket.org/..."), which is stripped.
	for _, l := range r.Links.Clone {
		switch l.Name {
		case "https":
			if p.acct.CloneBaseURL == "" {
				if u, err := url.Parse(l.Href); err == nil && u.Host != "" {
					u.User = nil
					urls.HTTPS = u.String()
				}
			}
		case "ssh":
			if p.acct.SSHHost == "" && l.Href != "" {
				urls.SSH = l.Href
			}
		}
	}
	repo := domain.Repository{
		ID:           r.UUID,
		Provider:     p.acct.Name,
		ProviderType: DriverType,
		Name:         slug,
		Namespace:    ws,
		FullName:     ws + "/" + slug,
		Description:  r.Description,
		Visibility:   domain.VisibilityPublic,
		Fork:         r.Parent != nil,
		Language:     r.Language,
		CreatedAt:    r.CreatedOn,
		UpdatedAt:    r.UpdatedOn,
		URLs:         urls,
	}
	if r.IsPrivate {
		repo.Visibility = domain.VisibilityPrivate
	}
	if r.Mainbranch != nil {
		repo.DefaultBranch = r.Mainbranch.Name
	} else {
		repo.Empty = true
	}
	return repo
}

// --- workspaces ------------------------------------------------------------------

type bbWorkspaceAccess struct {
	Administrator bool        `json:"administrator"`
	Workspace     bbWorkspace `json:"workspace"`
}

type bbWorkspace struct {
	UUID      string  `json:"uuid"`
	Slug      string  `json:"slug"`
	Name      string  `json:"name"`
	IsPrivate bool    `json:"is_private"`
	Links     bbLinks `json:"links"`
}

// userWorkspaces lists the workspaces accessible to the caller with
// GET /user/workspaces, the only cross-workspace endpoint Bitbucket Cloud
// still offers (GET /workspaces and GET /user/permissions/workspaces were
// removed with CHANGE-2770 on 14 April 2026).
func (p *Provider) userWorkspaces(ctx context.Context, limit int) ([]bbWorkspaceAccess, error) {
	return collectPages[bbWorkspaceAccess](ctx, p, limit, []request{{
		op: "list workspaces", url: "/user/workspaces",
		query: url.Values{"pagelen": {itoa(forge.PageSize(limit, maxPageLen))}, "sort": {"slug"}},
	}}, nil)
}

// listingWorkspaces returns the workspaces a cross-workspace listing should
// visit. Access tokens are confined to the configured workspace.
func (p *Provider) listingWorkspaces(ctx context.Context, op string) ([]string, error) {
	cred, err := p.credential(ctx)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	if p.authMethodFor(cred) == auth.MethodAccessToken {
		if p.workspace == "" {
			return nil, p.needWorkspace(op, "Bitbucket access tokens are scoped to one workspace; a workspace must be configured")
		}
		return []string{p.workspace}, nil
	}
	was, err := p.userWorkspaces(ctx, 0)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(was))
	for _, wa := range was {
		if wa.Workspace.Slug != "" {
			out = append(out, wa.Workspace.Slug)
		}
	}
	return out, nil
}

// --- repositories ----------------------------------------------------------------

// ListRepositories implements forge.RepositoryProvider.
//
// With a namespace, GET /repositories/{workspace} lists every repository of
// that workspace visible to the caller. Without one, the listing visits each
// workspace returned by GET /user/workspaces with role=member (Bitbucket no
// longer offers a cross-workspace repository listing). OwnedOnly maps to
// role=owner. Bitbucket has no archive flag, so IncludeArchived is moot.
func (p *Provider) ListRepositories(ctx context.Context, opts forge.ListRepositoryOptions) ([]domain.Repository, error) {
	const op = "list repositories"
	q, err := p.visibilityQuery(op, opts.Visibility)
	if err != nil {
		return nil, err
	}
	return p.searchRepositories(ctx, op, opts.Namespace, opts.OwnedOnly, q, opts.Limit)
}

func (p *Provider) searchRepositories(ctx context.Context, op, namespace string, owned bool, bbql []string, limit int) ([]domain.Repository, error) {
	var workspaces []string
	role := ""
	if namespace != "" {
		workspaces = []string{namespace}
	} else {
		ws, err := p.listingWorkspaces(ctx, op)
		if err != nil {
			return nil, err
		}
		workspaces = ws
		role = "member"
	}
	if owned {
		role = "owner"
	}
	firsts := make([]request, 0, len(workspaces))
	for _, ws := range workspaces {
		q := url.Values{"pagelen": {itoa(forge.PageSize(limit, maxPageLen))}, "sort": {"-updated_on"}}
		if role != "" {
			q.Set("role", role)
		}
		if len(bbql) > 0 {
			q.Set("q", strings.Join(bbql, " AND "))
		}
		firsts = append(firsts, request{op: op, url: "/repositories/" + pathEscape(ws), query: q,
			notFoundMsg: "workspace " + ws + " not found"})
	}
	raw, err := collectPages[bbRepository](ctx, p, limit, firsts, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Repository, 0, len(raw))
	for _, r := range raw {
		out = append(out, p.toRepository(r))
	}
	return out, nil
}

func (p *Provider) visibilityQuery(op string, v domain.Visibility) ([]string, error) {
	switch v {
	case "":
		return nil, nil
	case domain.VisibilityPrivate:
		return []string{"is_private=true"}, nil
	case domain.VisibilityPublic:
		return []string{"is_private=false"}, nil
	default:
		return nil, p.invalidArg(op, "Bitbucket repositories are either public or private; internal visibility does not exist")
	}
}

func (p *Provider) repoPath(ref domain.RepositoryRef) (string, error) {
	name := firstNonEmpty(ref.Name, ref.ID)
	if ref.Namespace == "" || name == "" {
		return "", p.invalidArg("", "a Bitbucket repository is identified as <workspace>/<repository-slug>")
	}
	return "/repositories/" + pathEscape(ref.Namespace) + "/" + pathEscape(name), nil
}

// GetRepository implements forge.RepositoryProvider.
func (p *Provider) GetRepository(ctx context.Context, ref domain.RepositoryRef) (*domain.Repository, error) {
	const op = "get repository"
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	var r bbRepository
	if err := p.do(ctx, request{op: op, method: http.MethodGet, url: path,
		notFound: errs.ErrRepositoryNotFound, notFoundMsg: "repository " + ref.FullName() + " not found"}, &r); err != nil {
		return nil, err
	}
	repo := p.toRepository(r)
	return &repo, nil
}

// CreateRepository implements forge.RepositoryCreator with
// POST /repositories/{workspace}/{slug}. The workspace defaults to the
// account's configured workspace. Bitbucket always creates empty
// repositories whose main branch is set by the first push, so AutoInit and
// DefaultBranch are rejected rather than silently ignored.
func (p *Provider) CreateRepository(ctx context.Context, req forge.CreateRepositoryRequest) (*domain.Repository, error) {
	const op = "create repository"
	ws := firstNonEmpty(req.Namespace, p.workspace)
	if ws == "" {
		return nil, p.invalidArg(op, "Bitbucket repositories belong to a workspace: pass a namespace or configure `workspace` for this account")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, p.invalidArg(op, "repository name is required")
	}
	if req.AutoInit {
		return nil, p.invalidArg(op, "Bitbucket Cloud cannot initialize repositories on creation; push an initial commit instead")
	}
	if req.DefaultBranch != "" {
		return nil, p.invalidArg(op, "Bitbucket Cloud sets the main branch from the first push; it cannot be chosen on creation")
	}
	body := map[string]any{"scm": "git", "name": name}
	if req.Description != "" {
		body["description"] = req.Description
	}
	switch req.Visibility {
	case "":
	case domain.VisibilityPrivate:
		body["is_private"] = true
	case domain.VisibilityPublic:
		body["is_private"] = false
	default:
		return nil, p.invalidArg(op, "Bitbucket repositories are either public or private")
	}
	// The URL slug is derived from the name the same way Bitbucket slugifies
	// names: lower case, whitespace replaced.
	slug := strings.ToLower(strings.Join(strings.Fields(name), "-"))
	var r bbRepository
	if err := p.do(ctx, request{op: op, method: http.MethodPost,
		url: "/repositories/" + pathEscape(ws) + "/" + pathEscape(slug), body: body,
		notFoundMsg: "workspace " + ws + " not found"}, &r); err != nil {
		return nil, err
	}
	repo := p.toRepository(r)
	return &repo, nil
}

// DeleteRepository implements forge.RepositoryDeleter.
func (p *Provider) DeleteRepository(ctx context.Context, ref domain.RepositoryRef) error {
	const op = "delete repository"
	path, err := p.repoPath(ref)
	if err != nil {
		return p.wrapOp(op, err)
	}
	return p.do(ctx, request{op: op, method: http.MethodDelete, url: path,
		notFound: errs.ErrRepositoryNotFound, notFoundMsg: "repository " + ref.FullName() + " not found"}, nil)
}

// ForkRepository implements forge.RepositoryForker with
// POST /repositories/{workspace}/{slug}/forks. The target workspace is
// req.Namespace or the configured workspace; when neither is set Bitbucket
// forks into the caller's personal workspace.
func (p *Provider) ForkRepository(ctx context.Context, ref domain.RepositoryRef, req forge.ForkRequest) (*domain.Repository, error) {
	const op = "fork repository"
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	body := map[string]any{}
	if req.Name != "" {
		body["name"] = req.Name
	}
	if ws := firstNonEmpty(req.Namespace, p.workspace); ws != "" {
		body["workspace"] = map[string]string{"slug": ws}
	}
	var r bbRepository
	if err := p.do(ctx, request{op: op, method: http.MethodPost, url: path + "/forks", body: body,
		notFound: errs.ErrRepositoryNotFound, notFoundMsg: "repository " + ref.FullName() + " not found"}, &r); err != nil {
		return nil, err
	}
	repo := p.toRepository(r)
	return &repo, nil
}

// RenameRepository implements forge.RepositoryRenamer with
// PUT /repositories/{workspace}/{slug} {"name": ...}. Bitbucket re-derives
// the slug from the new name, so the returned repository may live at a new
// path.
func (p *Provider) RenameRepository(ctx context.Context, ref domain.RepositoryRef, newName string) (*domain.Repository, error) {
	const op = "rename repository"
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	if strings.TrimSpace(newName) == "" {
		return nil, p.invalidArg(op, "new repository name is required")
	}
	var r bbRepository
	if err := p.do(ctx, request{op: op, method: http.MethodPut, url: path, body: map[string]string{"name": strings.TrimSpace(newName)},
		notFound: errs.ErrRepositoryNotFound, notFoundMsg: "repository " + ref.FullName() + " not found"}, &r); err != nil {
		return nil, err
	}
	repo := p.toRepository(r)
	return &repo, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
