package github

import (
	"context"
	"errors"
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

type apiOwner struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type apiRepo struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Owner         apiOwner  `json:"owner"`
	Description   *string   `json:"description"`
	Private       bool      `json:"private"`
	Visibility    string    `json:"visibility"`
	DefaultBranch string    `json:"default_branch"`
	Archived      bool      `json:"archived"`
	Fork          bool      `json:"fork"`
	Language      *string   `json:"language"`
	Stargazers    int       `json:"stargazers_count"`
	Forks         int       `json:"forks_count"`
	OpenIssues    int       `json:"open_issues_count"`
	Size          int       `json:"size"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	PushedAt      time.Time `json:"pushed_at"`
	HTMLURL       string    `json:"html_url"`
	CloneURL      string    `json:"clone_url"`
	SSHURL        string    `json:"ssh_url"`
	URL           string    `json:"url"`
	Topics        []string  `json:"topics"`
}

// repoVisibility prefers the "visibility" field (public/private/internal;
// internal exists on GHES and Enterprise Cloud) and falls back to the
// legacy "private" boolean on older GHES releases.
func repoVisibility(visibility string, private bool) domain.Visibility {
	switch strings.ToLower(visibility) {
	case "public":
		return domain.VisibilityPublic
	case "private":
		return domain.VisibilityPrivate
	case "internal":
		return domain.VisibilityInternal
	}
	if private {
		return domain.VisibilityPrivate
	}
	return domain.VisibilityPublic
}

func (p *Provider) toRepository(r apiRepo) domain.Repository {
	ns, name := r.Owner.Login, r.Name
	if ns == "" {
		if i := strings.Index(r.FullName, "/"); i > 0 {
			ns = r.FullName[:i]
		}
	}
	computed := p.RepositoryURLs(domain.RepositoryRef{Namespace: ns, Name: name})
	urls := domain.RepositoryURLs{Web: r.HTMLURL, HTTPS: r.CloneURL, SSH: r.SSHURL, API: r.URL}
	// Explicit clone overrides (proxies, SSH over 443) win over payload URLs.
	if urls.HTTPS == "" || p.acct.CloneBaseURL != "" {
		urls.HTTPS = computed.HTTPS
	}
	if urls.SSH == "" || p.acct.SSHHost != "" {
		urls.SSH = computed.SSH
	}
	if urls.Web == "" || p.acct.WebURL != "" {
		urls.Web = computed.Web
	}
	if urls.API == "" {
		urls.API = computed.API
	}
	return domain.Repository{
		ID:            strconv.FormatInt(r.ID, 10),
		Provider:      p.acct.Name,
		ProviderType:  DriverType,
		Name:          name,
		Namespace:     ns,
		FullName:      ns + "/" + name,
		Description:   str(r.Description),
		Visibility:    repoVisibility(r.Visibility, r.Private),
		DefaultBranch: r.DefaultBranch,
		Archived:      r.Archived,
		Fork:          r.Fork,
		Language:      str(r.Language),
		Stars:         r.Stargazers,
		Forks:         r.Forks,
		OpenIssues:    r.OpenIssues,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
		PushedAt:      r.PushedAt,
		URLs:          urls,
		Topics:        r.Topics,
	}
}

func (p *Provider) repoFilter(opts forge.ListRepositoryOptions) func(apiRepo) (domain.Repository, bool) {
	return func(r apiRepo) (domain.Repository, bool) {
		repo := p.toRepository(r)
		if repo.Archived && !opts.IncludeArchived {
			return repo, false
		}
		if opts.Visibility != "" && repo.Visibility != opts.Visibility {
			return repo, false
		}
		return repo, true
	}
}

// ListRepositories implements forge.RepositoryProvider.
//
// Without a namespace it lists repositories the user can access
// (GET /user/repos). With a namespace it lists an organization's
// repositories, falling back to a user's when no such organization exists.
// Visibility and archived filters are applied client-side so they behave the
// same on every endpoint (GET /orgs/{org}/repos has no archived filter and
// /user/repos cannot filter "internal").
func (p *Provider) ListRepositories(ctx context.Context, opts forge.ListRepositoryOptions) ([]domain.Repository, error) {
	const op = "list repositories"
	if err := p.ctxErr(ctx, op); err != nil {
		return nil, err
	}
	filter := p.repoFilter(opts)
	ns := strings.TrimSpace(opts.Namespace)
	if ns == "" {
		c, err := p.credential(ctx)
		if err != nil {
			return nil, err
		}
		if p.isAppCredential(c) {
			return p.listInstallationRepos(ctx, opts, filter)
		}
		return p.listOwnRepos(ctx, op, opts, filter)
	}

	// Every repository of an organization is owned by it, so OwnedOnly
	// needs no extra filter here.
	q := url.Values{"type": {"all"}, "sort": {"updated"}}
	repos, err := paginate(ctx, p, listSpec[apiRepo]{
		request: request{op: op, path: "/orgs/" + esc(ns) + "/repos", query: q},
		perPage: maxPerPage, limit: opts.Limit,
	}, filter)
	if err == nil || !errors.Is(err, errs.ErrNotFound) {
		return repos, err
	}

	// Not an organization: maybe a user. The authenticated user's own
	// namespace must use /user/repos, because /users/{name}/repos only
	// returns public repositories.
	if login, lerr := p.currentLogin(ctx); lerr == nil && strings.EqualFold(login, ns) {
		own := opts
		own.OwnedOnly = true
		return p.listOwnRepos(ctx, op, own, filter)
	}
	uq := url.Values{"type": {"owner"}, "sort": {"updated"}}
	if !opts.OwnedOnly {
		uq.Set("type", "all")
	}
	repos, err = paginate(ctx, p, listSpec[apiRepo]{
		request: request{op: op, path: "/users/" + esc(ns) + "/repos", query: uq,
			notFoundMsg: fmt.Sprintf("no organization or user named %q", ns)},
		perPage: maxPerPage, limit: opts.Limit,
	}, filter)
	return repos, err
}

func (p *Provider) listOwnRepos(ctx context.Context, op string, opts forge.ListRepositoryOptions, filter func(apiRepo) (domain.Repository, bool)) ([]domain.Repository, error) {
	q := url.Values{"sort": {"updated"}, "affiliation": {"owner,collaborator,organization_member"}}
	if opts.OwnedOnly {
		q.Set("affiliation", "owner")
	}
	switch opts.Visibility {
	case domain.VisibilityPublic, domain.VisibilityPrivate:
		q.Set("visibility", string(opts.Visibility))
	default:
		// "internal" is not a valid filter value here; filter client-side.
		q.Set("visibility", "all")
	}
	return paginate(ctx, p, listSpec[apiRepo]{
		request: request{op: op, path: "/user/repos", query: q},
		perPage: maxPerPage, limit: opts.Limit,
	}, filter)
}

// listInstallationRepos lists repositories a GitHub App installation can
// access (installation tokens cannot call /user/repos).
func (p *Provider) listInstallationRepos(ctx context.Context, opts forge.ListRepositoryOptions, filter func(apiRepo) (domain.Repository, bool)) ([]domain.Repository, error) {
	return paginate(ctx, p, listSpec[apiRepo]{
		request: request{op: "list installation repositories", path: "/installation/repositories"},
		perPage: maxPerPage, limit: opts.Limit,
		decode: envelope[apiRepo]("repositories", nil),
	}, filter)
}

func repoNotFound(ref domain.RepositoryRef) string {
	return fmt.Sprintf("repository %s not found", ref.FullName())
}

// GetRepository implements forge.RepositoryProvider.
func (p *Provider) GetRepository(ctx context.Context, ref domain.RepositoryRef) (*domain.Repository, error) {
	var r apiRepo
	_, err := p.do(ctx, request{
		op: "get repository", method: http.MethodGet, path: repoPath(ref.Namespace, ref.Name), out: &r,
		notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref),
	})
	if err != nil {
		return nil, err
	}
	repo := p.toRepository(r)
	return &repo, nil
}

// CreateRepository implements forge.RepositoryCreator.
//
// GitHub has no default-branch parameter at creation: with AutoInit the
// initial branch follows the owner's default-branch setting, so a different
// requested name is applied by renaming that branch afterwards. Without
// AutoInit the repository is empty and the first push decides the branch.
func (p *Provider) CreateRepository(ctx context.Context, req forge.CreateRepositoryRequest) (*domain.Repository, error) {
	const op = "create repository"
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "repository name is required")
	}
	if req.DefaultBranch != "" && !req.AutoInit {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0,
			"GitHub cannot set the default branch of an empty repository; enable auto-init or push the branch %q first", req.DefaultBranch)
	}
	body := map[string]any{"name": name}
	if req.Description != "" {
		body["description"] = req.Description
	}
	if req.AutoInit {
		body["auto_init"] = true
	}
	path := "/user/repos"
	if ns := strings.TrimSpace(req.Namespace); ns != "" {
		login, err := p.currentLogin(ctx)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(login, ns) {
			path = "/orgs/" + esc(ns) + "/repos"
		}
	}
	if path == "/user/repos" {
		switch req.Visibility {
		case domain.VisibilityInternal:
			return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "internal visibility is only available for organization repositories on GitHub Enterprise")
		case domain.VisibilityPrivate:
			body["private"] = true
		case domain.VisibilityPublic:
			body["private"] = false
		}
	} else if req.Visibility != "" {
		body["visibility"] = string(req.Visibility)
	}
	var r apiRepo
	_, err := p.do(ctx, request{op: op, method: http.MethodPost, path: path, body: body, out: &r,
		notFoundMsg: fmt.Sprintf("organization %q not found or the token cannot create repositories in it", req.Namespace)})
	if err != nil {
		return nil, err
	}
	if req.DefaultBranch != "" && r.DefaultBranch != "" && r.DefaultBranch != req.DefaultBranch {
		ns := r.Owner.Login
		_, err := p.do(ctx, request{
			op: "rename default branch", method: http.MethodPost,
			path: repoPath(ns, r.Name, "branches", escRef(r.DefaultBranch), "rename"),
			body: map[string]string{"new_name": req.DefaultBranch},
		})
		if err != nil {
			return nil, err
		}
		return p.GetRepository(ctx, domain.RepositoryRef{Namespace: ns, Name: r.Name})
	}
	repo := p.toRepository(r)
	return &repo, nil
}

// DeleteRepository implements forge.RepositoryDeleter (needs the delete_repo
// scope for classic tokens).
func (p *Provider) DeleteRepository(ctx context.Context, ref domain.RepositoryRef) error {
	_, err := p.do(ctx, request{
		op: "delete repository", method: http.MethodDelete, path: repoPath(ref.Namespace, ref.Name),
		notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref),
	})
	return err
}

// ForkRepository implements forge.RepositoryForker. GitHub creates forks
// asynchronously (202 Accepted) but returns the new repository immediately.
func (p *Provider) ForkRepository(ctx context.Context, ref domain.RepositoryRef, req forge.ForkRequest) (*domain.Repository, error) {
	body := map[string]any{}
	if ns := strings.TrimSpace(req.Namespace); ns != "" {
		// "organization" must be omitted when forking into the user's own
		// account.
		login, err := p.currentLogin(ctx)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(login, ns) {
			body["organization"] = ns
		}
	}
	if req.Name != "" {
		body["name"] = req.Name
	}
	var r apiRepo
	_, err := p.do(ctx, request{
		op: "fork repository", method: http.MethodPost, path: repoPath(ref.Namespace, ref.Name, "forks"),
		body: body, out: &r, notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref),
	})
	if err != nil {
		return nil, err
	}
	repo := p.toRepository(r)
	return &repo, nil
}

func (p *Provider) patchRepo(ctx context.Context, op string, ref domain.RepositoryRef, body map[string]any) (*domain.Repository, error) {
	var r apiRepo
	_, err := p.do(ctx, request{
		op: op, method: http.MethodPatch, path: repoPath(ref.Namespace, ref.Name), body: body, out: &r,
		notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref),
	})
	if err != nil {
		return nil, err
	}
	repo := p.toRepository(r)
	return &repo, nil
}

// RenameRepository implements forge.RepositoryRenamer. GitHub redirects the
// old name to the new one.
func (p *Provider) RenameRepository(ctx context.Context, ref domain.RepositoryRef, newName string) (*domain.Repository, error) {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, "rename repository", 0, "new repository name is required")
	}
	return p.patchRepo(ctx, "rename repository", ref, map[string]any{"name": newName})
}

// SetRepositoryArchived implements forge.RepositoryArchiver. The REST API
// supports both archiving and unarchiving (archived=false).
func (p *Provider) SetRepositoryArchived(ctx context.Context, ref domain.RepositoryRef, archived bool) (*domain.Repository, error) {
	op := "archive repository"
	if !archived {
		op = "unarchive repository"
	}
	return p.patchRepo(ctx, op, ref, map[string]any{"archived": archived})
}
