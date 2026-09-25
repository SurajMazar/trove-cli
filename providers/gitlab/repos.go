package gitlab

import (
	"context"
	"errors"
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

// glProject is the GitLab project entity (the subset Trove uses).
type glProject struct {
	ID                int64      `json:"id"`
	Name              string     `json:"name"`
	Path              string     `json:"path"`
	PathWithNamespace string     `json:"path_with_namespace"`
	Description       string     `json:"description"`
	DefaultBranch     string     `json:"default_branch"`
	Visibility        string     `json:"visibility"`
	Archived          bool       `json:"archived"`
	EmptyRepo         bool       `json:"empty_repo"`
	StarCount         int        `json:"star_count"`
	ForksCount        int        `json:"forks_count"`
	OpenIssuesCount   int        `json:"open_issues_count"`
	CreatedAt         *time.Time `json:"created_at"`
	LastActivityAt    *time.Time `json:"last_activity_at"`
	WebURL            string     `json:"web_url"`
	HTTPURLToRepo     string     `json:"http_url_to_repo"`
	SSHURLToRepo      string     `json:"ssh_url_to_repo"`
	Topics            []string   `json:"topics"`
	TagList           []string   `json:"tag_list"` // deprecated predecessor of topics
	ForkedFromProject *struct {
		ID int64 `json:"id"`
	} `json:"forked_from_project"`
	Namespace struct {
		ID       int64  `json:"id"`
		FullPath string `json:"full_path"`
		Kind     string `json:"kind"`
	} `json:"namespace"`
}

// projectPath returns the escaped API path of a project. GitLab accepts the
// numeric ID or the URL-encoded full path ("group%2Fsub%2Fproject").
func projectPath(ref domain.RepositoryRef) (string, error) {
	if ref.ID != "" {
		return "/projects/" + url.PathEscape(ref.ID), nil
	}
	if ref.Name == "" {
		return "", errs.New(errs.ErrInvalidArgument, "a project reference needs a namespace and name (or an ID)")
	}
	return "/projects/" + url.PathEscape(ref.FullName()), nil
}

// splitFullPath splits "group/sub/project" into ("group/sub", "project").
func splitFullPath(full string) (ns, name string) {
	full = strings.Trim(full, "/")
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[:i], full[i+1:]
	}
	return "", full
}

func (p *provider) rememberProject(id int64, fullPath string) {
	if id == 0 || fullPath == "" {
		return
	}
	p.pathMu.Lock()
	p.projectPaths[id] = fullPath
	p.pathMu.Unlock()
}

// projectFullPath resolves a project ID to its path_with_namespace, using a
// small cache. It returns "" when the project cannot be read.
func (p *provider) projectFullPath(ctx context.Context, id int64) string {
	if id == 0 {
		return ""
	}
	p.pathMu.Lock()
	full, ok := p.projectPaths[id]
	p.pathMu.Unlock()
	if ok {
		return full
	}
	var pr glProject
	if err := p.get(ctx, "get project", "/projects/"+strconv.FormatInt(id, 10), url.Values{"simple": {"true"}}, &pr); err != nil {
		return ""
	}
	p.rememberProject(pr.ID, pr.PathWithNamespace)
	return pr.PathWithNamespace
}

func (p *provider) toRepository(pr glProject) domain.Repository {
	full := pr.PathWithNamespace
	ns, name := splitFullPath(full)
	if pr.Path != "" && name != pr.Path {
		// Defensive: path_with_namespace always ends with path.
		name = pr.Path
	}
	if ns == "" {
		ns = pr.Namespace.FullPath
	}
	p.rememberProject(pr.ID, ns+"/"+name)
	r := domain.Repository{
		ID:            strconv.FormatInt(pr.ID, 10),
		Provider:      p.acct.Name,
		ProviderType:  DriverType,
		Name:          name,
		Namespace:     ns,
		FullName:      ns + "/" + name,
		Description:   pr.Description,
		Visibility:    visibility(pr.Visibility),
		DefaultBranch: pr.DefaultBranch,
		Archived:      pr.Archived,
		Fork:          pr.ForkedFromProject != nil,
		Empty:         pr.EmptyRepo,
		Stars:         pr.StarCount,
		Forks:         pr.ForksCount,
		OpenIssues:    pr.OpenIssuesCount,
		Topics:        pr.Topics,
	}
	if len(r.Topics) == 0 {
		r.Topics = pr.TagList
	}
	if pr.CreatedAt != nil {
		r.CreatedAt = *pr.CreatedAt
	}
	if pr.LastActivityAt != nil {
		r.UpdatedAt = *pr.LastActivityAt
	}
	r.URLs = p.RepositoryURLs(r.Ref())
	r.URLs.API = p.apiURL + "/projects/" + r.ID
	if pr.WebURL != "" {
		r.URLs.Web = pr.WebURL
	}
	// Prefer the URLs GitLab reports unless the account overrides them.
	if pr.HTTPURLToRepo != "" && p.acct.CloneBaseURL == "" {
		r.URLs.HTTPS = pr.HTTPURLToRepo
	}
	if pr.SSHURLToRepo != "" && p.acct.SSHHost == "" {
		r.URLs.SSH = pr.SSHURLToRepo
	}
	return r
}

// visibility maps GitLab visibility levels. Projects the API returns without
// a visibility (never observed for full entities) are treated as private,
// the most restrictive level.
func visibility(v string) domain.Visibility {
	switch v {
	case "public":
		return domain.VisibilityPublic
	case "internal":
		return domain.VisibilityInternal
	default:
		return domain.VisibilityPrivate
	}
}

// RepositoryURLs computes project URLs without an API call.
func (p *provider) RepositoryURLs(ref domain.RepositoryRef) domain.RepositoryURLs {
	full := ref.FullName()
	var u domain.RepositoryURLs
	if full == "" {
		return u
	}
	u.Web = p.webURL + "/" + full
	cloneBase := p.webURL
	if p.acct.CloneBaseURL != "" {
		cloneBase = strings.TrimRight(p.acct.CloneBaseURL, "/")
	}
	u.HTTPS = cloneBase + "/" + full + ".git"
	sshHost := p.acct.SSHHost
	if sshHost == "" {
		if wu, err := url.Parse(p.webURL); err == nil {
			sshHost = wu.Hostname()
		}
	}
	if sshHost != "" {
		sshHost = strings.TrimPrefix(strings.TrimPrefix(sshHost, "ssh://"), "git@")
		if strings.Contains(sshHost, ":") {
			// A custom SSH port needs the ssh:// URL form.
			u.SSH = "ssh://git@" + sshHost + "/" + full + ".git"
		} else {
			u.SSH = "git@" + sshHost + ":" + full + ".git"
		}
	}
	if ref.ID != "" {
		u.API = p.apiURL + "/projects/" + url.PathEscape(ref.ID)
	} else {
		u.API = p.apiURL + "/projects/" + url.PathEscape(full)
	}
	return u
}

func (p *provider) CurrentUser(ctx context.Context) (*domain.User, error) {
	u, err := p.fetchUser(ctx, nil)
	if err != nil {
		return nil, err
	}
	return u.toDomain(), nil
}

type glUser struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	PublicEmail string `json:"public_email"`
	WebURL      string `json:"web_url"`
	AvatarURL   string `json:"avatar_url"`
	Bot         bool   `json:"bot"`
}

func (u glUser) toDomain() *domain.User {
	email := u.Email
	if email == "" {
		email = u.PublicEmail
	}
	return &domain.User{
		ID: strconv.FormatInt(u.ID, 10), Username: u.Username, Name: u.Name,
		Email: email, WebURL: u.WebURL, AvatarURL: u.AvatarURL,
	}
}

func (p *provider) fetchUser(ctx context.Context, cred *auth.Credential) (*glUser, error) {
	var u glUser
	c := call{method: http.MethodGet, path: "/user", op: "get current user"}
	if cred != nil {
		c.cred = cred
	}
	if _, err := p.do(ctx, c, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// --- repositories --------------------------------------------------------------

func (p *provider) ListRepositories(ctx context.Context, opts forge.ListRepositoryOptions) ([]domain.Repository, error) {
	const op = "list projects"
	q := url.Values{"order_by": {"last_activity_at"}, "simple": {"false"}}
	if opts.Visibility != "" {
		q.Set("visibility", string(opts.Visibility))
	}
	if !opts.IncludeArchived {
		q.Set("archived", "false")
	}
	if opts.OwnedOnly {
		q.Set("owned", "true")
	}
	var projects []glProject
	var err error
	if ns := strings.Trim(opts.Namespace, "/"); ns != "" {
		gq := cloneValues(q)
		gq.Set("include_subgroups", "true")
		projects, err = list[glProject](ctx, p, op, "/groups/"+url.PathEscape(ns)+"/projects", gq, opts.Limit, nil)
		if errors.Is(err, errs.ErrNotFound) && !strings.Contains(ns, "/") {
			// Not a group: try a personal namespace (usernames never nest).
			projects, err = list[glProject](ctx, p, op, "/users/"+url.PathEscape(ns)+"/projects", q, opts.Limit, nil)
		}
	} else {
		q.Set("membership", "true")
		projects, err = list[glProject](ctx, p, op, "/projects", q, opts.Limit, nil)
	}
	if err != nil {
		return nil, err
	}
	out := make([]domain.Repository, 0, len(projects))
	for _, pr := range projects {
		out = append(out, p.toRepository(pr))
	}
	return out, nil
}

func cloneValues(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (p *provider) projectCall(ctx context.Context, method string, ref domain.RepositoryRef, suffix, op string, body any) (*domain.Repository, error) {
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	var pr glProject
	if _, err := p.do(ctx, call{method: method, path: path + suffix, op: op, body: body, notFound: errs.ErrRepositoryNotFound}, &pr); err != nil {
		return nil, err
	}
	r := p.toRepository(pr)
	return &r, nil
}

func withProvider(err error, provider, op string) error {
	var e *errs.Error
	if errors.As(err, &e) {
		cp := *e
		cp.Provider = provider
		if cp.Op == "" {
			cp.Op = op
		}
		return &cp
	}
	return err
}

func (p *provider) GetRepository(ctx context.Context, ref domain.RepositoryRef) (*domain.Repository, error) {
	return p.projectCall(ctx, http.MethodGet, ref, "", "get project "+ref.FullName(), nil)
}

// resolveNamespaceID resolves a namespace full path (user or group) to its ID.
func (p *provider) resolveNamespaceID(ctx context.Context, path string) (int64, error) {
	var ns glNamespace
	if _, err := p.do(ctx, call{method: http.MethodGet, path: "/namespaces/" + url.PathEscape(strings.Trim(path, "/")), op: "resolve namespace " + path}, &ns); err != nil {
		return 0, err
	}
	return ns.ID, nil
}

func (p *provider) CreateRepository(ctx context.Context, req forge.CreateRepositoryRequest) (*domain.Repository, error) {
	const op = "create project"
	if strings.TrimSpace(req.Name) == "" {
		return nil, p.invalid(op, "a project name is required")
	}
	body := map[string]any{"name": req.Name, "path": req.Name}
	if req.Namespace != "" {
		id, err := p.resolveNamespaceID(ctx, req.Namespace)
		if err != nil {
			return nil, err
		}
		body["namespace_id"] = id
	}
	if req.Description != "" {
		body["description"] = req.Description
	}
	if req.Visibility != "" {
		body["visibility"] = string(req.Visibility)
	}
	if req.DefaultBranch != "" {
		// GitLab only applies default_branch when the project is initialized
		// with a README; otherwise the first pushed branch becomes default.
		body["default_branch"] = req.DefaultBranch
	}
	if req.AutoInit {
		body["initialize_with_readme"] = true
	}
	var pr glProject
	if _, err := p.do(ctx, call{method: http.MethodPost, path: "/projects", op: op, body: body}, &pr); err != nil {
		return nil, err
	}
	r := p.toRepository(pr)
	return &r, nil
}

// DeleteRepository deletes a project. GitLab answers 202 Accepted: deletion
// is asynchronous and, on GitLab.com and instances with "delayed project
// deletion", the project is only marked for deletion and removed after the
// retention period.
func (p *provider) DeleteRepository(ctx context.Context, ref domain.RepositoryRef) error {
	op := "delete project " + ref.FullName()
	path, err := projectPath(ref)
	if err != nil {
		return withProvider(err, p.acct.Name, op)
	}
	_, err = p.do(ctx, call{method: http.MethodDelete, path: path, op: op, notFound: errs.ErrRepositoryNotFound}, nil)
	return err
}

// ForkRepository forks a project. GitLab creates forks asynchronously; the
// returned project may still be importing.
func (p *provider) ForkRepository(ctx context.Context, ref domain.RepositoryRef, req forge.ForkRequest) (*domain.Repository, error) {
	body := map[string]any{}
	if req.Namespace != "" {
		body["namespace_path"] = strings.Trim(req.Namespace, "/")
	}
	if req.Name != "" {
		body["name"] = req.Name
		body["path"] = req.Name
	}
	return p.projectCall(ctx, http.MethodPost, ref, "/fork", "fork project "+ref.FullName(), body)
}

// RenameRepository changes both the display name and the path (URL slug).
func (p *provider) RenameRepository(ctx context.Context, ref domain.RepositoryRef, newName string) (*domain.Repository, error) {
	op := "rename project " + ref.FullName()
	if strings.TrimSpace(newName) == "" {
		return nil, p.invalid(op, "a new name is required")
	}
	return p.projectCall(ctx, http.MethodPut, ref, "", op, map[string]any{"name": newName, "path": newName})
}

func (p *provider) SetRepositoryArchived(ctx context.Context, ref domain.RepositoryRef, archived bool) (*domain.Repository, error) {
	if archived {
		return p.projectCall(ctx, http.MethodPost, ref, "/archive", "archive project "+ref.FullName(), nil)
	}
	return p.projectCall(ctx, http.MethodPost, ref, "/unarchive", "unarchive project "+ref.FullName(), nil)
}

// --- namespaces --------------------------------------------------------------

type glNamespace struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	FullPath  string `json:"full_path"`
	ParentID  *int64 `json:"parent_id"`
	AvatarURL string `json:"avatar_url"`
	WebURL    string `json:"web_url"`
}

func (n glNamespace) toDomain() domain.Namespace {
	out := domain.Namespace{
		ID: strconv.FormatInt(n.ID, 10), Name: n.Name, FullPath: n.FullPath,
		WebURL: n.WebURL, AvatarURL: n.AvatarURL,
	}
	switch {
	case n.Kind == "user":
		out.Type, out.Label = domain.NamespaceUser, "GitLab User Namespace"
	case n.ParentID != nil:
		out.Type, out.Label = domain.NamespaceSubgroup, "GitLab Subgroup"
		out.ParentPath, _ = splitFullPath(n.FullPath)
	default:
		out.Type, out.Label = domain.NamespaceGroup, "GitLab Group"
	}
	return out
}

func (p *provider) ListNamespaces(ctx context.Context, opts forge.ListOptions) ([]domain.Namespace, error) {
	nss, err := list[glNamespace](ctx, p, "list namespaces", "/namespaces", nil, opts.Limit, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Namespace, 0, len(nss))
	for _, n := range nss {
		out = append(out, n.toDomain())
	}
	return out, nil
}

func (p *provider) GetNamespace(ctx context.Context, path string) (*domain.Namespace, error) {
	op := "get namespace " + path
	path = strings.Trim(path, "/")
	if path == "" {
		return nil, p.invalid(op, "a namespace path is required")
	}
	var n glNamespace
	if _, err := p.do(ctx, call{method: http.MethodGet, path: "/namespaces/" + url.PathEscape(path), op: op}, &n); err != nil {
		return nil, err
	}
	d := n.toDomain()
	return &d, nil
}
