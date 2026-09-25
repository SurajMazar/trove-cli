package custom

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

// --- user & authentication -------------------------------------------------------

// CurrentUser implements forge.Provider (GET /v1/user).
func (p *Provider) CurrentUser(ctx context.Context) (*domain.User, error) {
	return p.currentUser(ctx, nil)
}

func (p *Provider) currentUser(ctx context.Context, rc *resolvedCredential) (*domain.User, error) {
	var u domain.User
	if _, err := p.send(ctx, call{method: http.MethodGet, url: p.endpoint("/v1/user", nil), out: &u, op: "get current user", cred: rc}); err != nil {
		return nil, err
	}
	if u.Username == "" && u.ID == "" {
		return nil, p.protocolError("get current user", "GET /v1/user returned a user without id or username")
	}
	return &u, nil
}

// Login implements forge.Authenticator. TFP has no interactive flow: the
// user supplies a token (and, for the basic scheme, a username) which is
// validated against GET /v1/user. The credential is returned, not stored.
func (p *Provider) Login(ctx context.Context, req auth.Request) (*auth.Result, error) {
	method := req.Method
	if method == "" {
		method = auth.MethodToken
	}
	switch method {
	case auth.MethodToken:
	case auth.MethodBasic:
		if !p.s.basic {
			return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name,
				Message: "auth method \"basic\" requires custom.auth.scheme: basic"}
		}
	default:
		return nil, errs.Unsupported(p.acct.Name, fmt.Sprintf("the %q login method", method))
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name,
			Message: fmt.Sprintf("a %s token is required", p.s.displayName),
			Hint:    "pass the token on stdin or via the interactive prompt"}
	}
	rc, err := p.resolve(token, strings.TrimSpace(req.Username))
	if err != nil {
		return nil, err
	}
	u, err := p.currentUser(ctx, rc)
	if err != nil {
		return nil, err
	}
	cred := auth.Credential{Kind: auth.KindToken, Token: token}
	if p.s.basic {
		cred.Kind = auth.KindBasic
		cred.Username = rc.username
	}
	// The caller persists the new credential; drop any cached one so the
	// next request loads it from the store.
	p.mu.Lock()
	p.cred = nil
	p.mu.Unlock()
	return &auth.Result{Credential: cred, User: firstNonEmpty(u.Username, u.ID)}, nil
}

// AuthStatus implements forge.Authenticator.
func (p *Provider) AuthStatus(ctx context.Context) (*auth.Status, error) {
	rc, err := p.credential(ctx)
	if err != nil {
		if errors.Is(err, errs.ErrNotAuthenticated) {
			return &auth.Status{Authenticated: false, Detail: "not logged in"}, nil
		}
		return nil, err
	}
	u, err := p.currentUser(ctx, rc)
	if err != nil {
		if errors.Is(err, errs.ErrAuthenticationFailed) {
			return &auth.Status{Authenticated: false, Method: rc.method, Detail: err.Error()}, nil
		}
		return nil, err
	}
	return &auth.Status{Authenticated: true, User: firstNonEmpty(u.Username, u.ID), Method: rc.method}, nil
}

// GitCredentials implements forge.GitAuthenticator. Git-over-HTTPS uses the
// same token as the API. The username is custom.git_username when set, the
// basic-auth username for the basic scheme, and "trove" otherwise (most
// token-authenticating Git servers ignore the username).
func (p *Provider) GitCredentials(ctx context.Context) (string, string, error) {
	rc, err := p.credential(ctx)
	if err != nil {
		return "", "", err
	}
	user := firstNonEmpty(p.s.gitUsername, rc.username, defaultGitUsername)
	return user, rc.secret, nil
}

// --- repositories ------------------------------------------------------------------

// ListRepositories implements forge.RepositoryProvider (GET /v1/repositories).
func (p *Provider) ListRepositories(ctx context.Context, opts forge.ListRepositoryOptions) ([]domain.Repository, error) {
	if err := p.require(forge.CapRepositories); err != nil {
		return nil, err
	}
	ns := strings.Trim(opts.Namespace, "/")
	q := url.Values{}
	if ns != "" {
		q.Set("namespace", ns)
	}
	if opts.Visibility != "" {
		q.Set("visibility", string(opts.Visibility))
	}
	if opts.IncludeArchived {
		q.Set("include_archived", "true")
	}
	if opts.OwnedOnly {
		q.Set("owned", "true")
	}
	return list(ctx, p, listSpec[domain.Repository]{
		op: "list repositories", path: "/v1/repositories", query: q, limit: opts.Limit,
		keep: func(r *domain.Repository) bool {
			if !p.normalizeRepo(r) {
				return false
			}
			if ns != "" && r.Namespace != ns && !strings.HasPrefix(r.Namespace, ns+"/") {
				return false
			}
			if opts.Visibility != "" && r.Visibility != opts.Visibility {
				return false
			}
			return opts.IncludeArchived || !r.Archived
		},
	})
}

// GetRepository implements forge.RepositoryProvider (GET /v1/repositories/{repo}).
func (p *Provider) GetRepository(ctx context.Context, ref domain.RepositoryRef) (*domain.Repository, error) {
	if err := p.require(forge.CapRepositories); err != nil {
		return nil, err
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, err
	}
	var r domain.Repository
	op := "get repository " + ref.FullName()
	if _, err := p.send(ctx, call{method: http.MethodGet, url: p.endpoint(path, nil), out: &r, op: op, notFound: errs.ErrRepositoryNotFound}); err != nil {
		return nil, err
	}
	if !p.normalizeRepo(&r) {
		return nil, p.protocolError(op, "repository payload has no name or namespace")
	}
	return &r, nil
}

// RepositoryURLs implements forge.RepositoryProvider. URLs come from the
// configured templates; no request is made.
func (p *Provider) RepositoryURLs(ref domain.RepositoryRef) domain.RepositoryURLs {
	return p.templateURLs(ref.Namespace, ref.Name)
}

type createRepositoryBody struct {
	Name          string            `json:"name"`
	Namespace     string            `json:"namespace,omitempty"`
	Description   string            `json:"description,omitempty"`
	Visibility    domain.Visibility `json:"visibility,omitempty"`
	DefaultBranch string            `json:"default_branch,omitempty"`
	AutoInit      bool              `json:"auto_init"`
}

// CreateRepository implements forge.RepositoryCreator (POST /v1/repositories).
func (p *Provider) CreateRepository(ctx context.Context, req forge.CreateRepositoryRequest) (*domain.Repository, error) {
	if err := p.require(forge.CapRepoCreate); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Name) == "" || strings.Contains(req.Name, "/") {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: "repository name must be non-empty and must not contain '/'"}
	}
	vis, err := domain.ParseVisibility(string(req.Visibility))
	if err != nil {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: err.Error()}
	}
	body := createRepositoryBody{
		Name: req.Name, Namespace: strings.Trim(req.Namespace, "/"), Description: req.Description,
		Visibility: vis, DefaultBranch: req.DefaultBranch, AutoInit: req.AutoInit,
	}
	var r domain.Repository
	op := "create repository " + req.Name
	if _, err := p.send(ctx, call{method: http.MethodPost, url: p.endpoint("/v1/repositories", nil), body: body, out: &r, op: op}); err != nil {
		return nil, err
	}
	if !p.normalizeRepo(&r) {
		return nil, p.protocolError(op, "repository payload has no name or namespace")
	}
	return &r, nil
}

// DeleteRepository implements forge.RepositoryDeleter (DELETE /v1/repositories/{repo}).
func (p *Provider) DeleteRepository(ctx context.Context, ref domain.RepositoryRef) error {
	if err := p.require(forge.CapRepoDelete); err != nil {
		return err
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return err
	}
	_, err = p.send(ctx, call{method: http.MethodDelete, url: p.endpoint(path, nil), op: "delete repository " + ref.FullName(), notFound: errs.ErrRepositoryNotFound})
	return err
}

// normalizeRepo enforces Trove's invariants on a server-supplied repository.
// Provider/ProviderType are never taken from the server. It returns false
// when the payload cannot identify a repository.
func (p *Provider) normalizeRepo(r *domain.Repository) bool {
	if (r.Namespace == "" || r.Name == "") && r.FullName != "" {
		if i := strings.LastIndex(strings.Trim(r.FullName, "/"), "/"); i > 0 {
			full := strings.Trim(r.FullName, "/")
			r.Namespace, r.Name = full[:i], full[i+1:]
		}
	}
	r.Namespace = strings.Trim(r.Namespace, "/")
	if r.Namespace == "" || r.Name == "" {
		p.logger.Warn("custom forge returned a repository without namespace or name; skipping", "provider", p.acct.Name, "id", r.ID)
		return false
	}
	r.FullName = r.Namespace + "/" + r.Name
	r.Provider = p.acct.Name
	r.ProviderType = Type
	if v, err := domain.ParseVisibility(string(r.Visibility)); err != nil || v == "" {
		// Unknown visibility: assume the most restrictive.
		r.Visibility = domain.VisibilityPrivate
	} else {
		r.Visibility = v
	}

	t := p.templateURLs(r.Namespace, r.Name)
	if p.s.clone.Strategy == cloneTemplate {
		r.URLs.HTTPS, r.URLs.SSH, r.URLs.Web = t.HTTPS, t.SSH, t.Web
	} else {
		if !isHTTPURL(r.URLs.HTTPS) {
			r.URLs.HTTPS = t.HTTPS
		}
		if r.URLs.SSH == "" {
			r.URLs.SSH = t.SSH
		}
		if !isHTTPURL(r.URLs.Web) {
			r.URLs.Web = t.Web
		}
	}
	r.URLs.API = t.API
	return true
}

// templateURLs renders the configured templates. A template whose
// placeholders cannot be resolved (only possible with the api strategy)
// yields an empty URL.
func (p *Provider) templateURLs(namespace, name string) domain.RepositoryURLs {
	var u domain.RepositoryURLs
	if namespace != "" && name != "" {
		u.API = p.endpoint("/v1/repositories/"+url.PathEscape(namespace+"/"+name), nil)
	}
	if p.s.canRender(p.s.clone.HTTPS) {
		u.HTTPS = p.s.render(p.s.clone.HTTPS, namespace, name)
	}
	if p.s.canRender(p.s.clone.SSH) {
		u.SSH = p.s.render(p.s.clone.SSH, namespace, name)
	}
	if p.s.canRender(p.s.clone.Web) {
		u.Web = p.s.render(p.s.clone.Web, namespace, name)
	}
	return u
}

// --- pull requests -----------------------------------------------------------------

// ListPullRequests implements forge.PullRequestProvider.
func (p *Provider) ListPullRequests(ctx context.Context, ref domain.RepositoryRef, opts forge.PullRequestListOptions) ([]domain.PullRequest, error) {
	if err := p.require(forge.CapPullRequests); err != nil {
		return nil, err
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", string(opts.State))
	}
	if opts.Author != "" {
		q.Set("author", opts.Author)
	}
	if opts.TargetBranch != "" {
		q.Set("target_branch", opts.TargetBranch)
	}
	return list(ctx, p, listSpec[domain.PullRequest]{
		op:   "list " + strings.ToLower(p.s.terms.PullRequest) + "s for " + ref.FullName(),
		path: path + "/pull-requests", query: q, limit: opts.Limit, notFound: errs.ErrRepositoryNotFound,
		keep: func(pr *domain.PullRequest) bool {
			p.normalizePR(pr)
			if opts.State != "" && opts.State != domain.PullRequestAll && pr.State != opts.State {
				return false
			}
			if opts.Author != "" && !strings.EqualFold(pr.Author, opts.Author) {
				return false
			}
			return opts.TargetBranch == "" || pr.TargetBranch == opts.TargetBranch
		},
	})
}

// GetPullRequest implements forge.PullRequestProvider.
func (p *Provider) GetPullRequest(ctx context.Context, ref domain.RepositoryRef, number int) (*domain.PullRequest, error) {
	if err := p.require(forge.CapPullRequests); err != nil {
		return nil, err
	}
	path, err := p.prPath(ref, number)
	if err != nil {
		return nil, err
	}
	var pr domain.PullRequest
	if _, err := p.send(ctx, call{method: http.MethodGet, url: p.endpoint(path, nil), out: &pr, op: p.prOp("get", ref, number)}); err != nil {
		return nil, err
	}
	p.normalizePR(&pr)
	return &pr, nil
}

type createPullRequestBody struct {
	Title        string `json:"title"`
	Body         string `json:"body,omitempty"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	Draft        bool   `json:"draft"`
	SourceRepo   string `json:"source_repo,omitempty"`
}

// CreatePullRequest implements forge.PullRequestCreator.
func (p *Provider) CreatePullRequest(ctx context.Context, ref domain.RepositoryRef, req forge.CreatePullRequestRequest) (*domain.PullRequest, error) {
	if err := p.require(forge.CapPRCreate); err != nil {
		return nil, err
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Title) == "" || req.SourceBranch == "" || req.TargetBranch == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: "title, source branch and target branch are required"}
	}
	body := createPullRequestBody{Title: req.Title, Body: req.Body, SourceBranch: req.SourceBranch,
		TargetBranch: req.TargetBranch, Draft: req.Draft, SourceRepo: req.SourceRepo}
	var pr domain.PullRequest
	op := "create " + strings.ToLower(p.s.terms.PullRequest) + " in " + ref.FullName()
	if _, err := p.send(ctx, call{method: http.MethodPost, url: p.endpoint(path+"/pull-requests", nil), body: body, out: &pr, op: op, notFound: errs.ErrRepositoryNotFound}); err != nil {
		return nil, err
	}
	p.normalizePR(&pr)
	return &pr, nil
}

type mergePullRequestBody struct {
	Method             domain.MergeMethod `json:"method,omitempty"`
	CommitTitle        string             `json:"commit_title,omitempty"`
	CommitMessage      string             `json:"commit_message,omitempty"`
	DeleteSourceBranch bool               `json:"delete_source_branch"`
}

// MergePullRequest implements forge.PullRequestMerger.
func (p *Provider) MergePullRequest(ctx context.Context, ref domain.RepositoryRef, number int, req forge.MergePullRequestRequest) error {
	if err := p.require(forge.CapPRMerge); err != nil {
		return err
	}
	switch req.Method {
	case "", domain.MergeMethodMerge, domain.MergeMethodSquash, domain.MergeMethodRebase:
	default:
		return &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: fmt.Sprintf("unknown merge method %q", req.Method)}
	}
	path, err := p.prPath(ref, number)
	if err != nil {
		return err
	}
	body := mergePullRequestBody{Method: req.Method, CommitTitle: req.CommitTitle, CommitMessage: req.CommitMessage, DeleteSourceBranch: req.DeleteSourceBranch}
	_, err = p.send(ctx, call{method: http.MethodPost, url: p.endpoint(path+"/merge", nil), body: body, op: p.prOp("merge", ref, number)})
	return err
}

// MergeMethods implements forge.PullRequestMerger. TFP v1 has no discovery
// endpoint: all three methods are offered and the server rejects the ones it
// does not support with an "invalid" error.
func (p *Provider) MergeMethods() []domain.MergeMethod {
	return []domain.MergeMethod{domain.MergeMethodMerge, domain.MergeMethodSquash, domain.MergeMethodRebase}
}

// ClosePullRequest implements forge.PullRequestCloser.
func (p *Provider) ClosePullRequest(ctx context.Context, ref domain.RepositoryRef, number int) error {
	if err := p.require(forge.CapPRClose); err != nil {
		return err
	}
	path, err := p.prPath(ref, number)
	if err != nil {
		return err
	}
	_, err = p.send(ctx, call{method: http.MethodPost, url: p.endpoint(path+"/close", nil), op: p.prOp("close", ref, number)})
	return err
}

func (p *Provider) prPath(ref domain.RepositoryRef, number int) (string, error) {
	path, err := p.repoPath(ref)
	if err != nil {
		return "", err
	}
	if number <= 0 {
		return "", &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: fmt.Sprintf("invalid %s number %d", p.s.terms.PullRequestShort, number)}
	}
	return path + "/pull-requests/" + strconv.Itoa(number), nil
}

func (p *Provider) prOp(verb string, ref domain.RepositoryRef, number int) string {
	return fmt.Sprintf("%s %s %s#%d", verb, p.s.terms.PullRequestShort, ref.FullName(), number)
}

func (p *Provider) normalizePR(pr *domain.PullRequest) {
	pr.Term = p.s.terms.PullRequest
}

// --- issues --------------------------------------------------------------------------

// ListIssues implements forge.IssueProvider.
func (p *Provider) ListIssues(ctx context.Context, ref domain.RepositoryRef, opts forge.IssueListOptions) ([]domain.Issue, error) {
	if err := p.require(forge.CapIssues); err != nil {
		return nil, err
	}
	if len(opts.Labels) > 0 {
		if err := p.require(forge.CapIssueLabels); err != nil {
			return nil, err
		}
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", string(opts.State))
	}
	if len(opts.Labels) > 0 {
		q.Set("labels", strings.Join(opts.Labels, ","))
	}
	if opts.Assignee != "" {
		q.Set("assignee", opts.Assignee)
	}
	if opts.Author != "" {
		q.Set("author", opts.Author)
	}
	return list(ctx, p, listSpec[domain.Issue]{
		op: "list issues for " + ref.FullName(), path: path + "/issues", query: q, limit: opts.Limit,
		notFound: errs.ErrRepositoryNotFound,
		keep: func(is *domain.Issue) bool {
			if opts.State != "" && opts.State != domain.IssueAll && is.State != opts.State {
				return false
			}
			if opts.Author != "" && !strings.EqualFold(is.Author, opts.Author) {
				return false
			}
			if opts.Assignee != "" && !containsFold(is.Assignees, opts.Assignee) {
				return false
			}
			for _, l := range opts.Labels {
				if !containsFold(is.Labels, l) {
					return false
				}
			}
			return true
		},
	})
}

// GetIssue implements forge.IssueProvider.
func (p *Provider) GetIssue(ctx context.Context, ref domain.RepositoryRef, number int) (*domain.Issue, error) {
	if err := p.require(forge.CapIssues); err != nil {
		return nil, err
	}
	path, err := p.issuePath(ref, number)
	if err != nil {
		return nil, err
	}
	var is domain.Issue
	if _, err := p.send(ctx, call{method: http.MethodGet, url: p.endpoint(path, nil), out: &is, op: fmt.Sprintf("get issue %s#%d", ref.FullName(), number)}); err != nil {
		return nil, err
	}
	return &is, nil
}

type createIssueBody struct {
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Assignees []string `json:"assignees,omitempty"`
}

// CreateIssue implements forge.IssueCreator.
func (p *Provider) CreateIssue(ctx context.Context, ref domain.RepositoryRef, req forge.CreateIssueRequest) (*domain.Issue, error) {
	if err := p.require(forge.CapIssueCreate); err != nil {
		return nil, err
	}
	if len(req.Labels) > 0 {
		if err := p.require(forge.CapIssueLabels); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(req.Title) == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: "issue title is required"}
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, err
	}
	body := createIssueBody{Title: req.Title, Body: req.Body, Labels: req.Labels, Assignees: req.Assignees}
	var is domain.Issue
	if _, err := p.send(ctx, call{method: http.MethodPost, url: p.endpoint(path+"/issues", nil), body: body, out: &is, op: "create issue in " + ref.FullName(), notFound: errs.ErrRepositoryNotFound}); err != nil {
		return nil, err
	}
	return &is, nil
}

// SetIssueState implements forge.IssueStateChanger (PATCH .../issues/{number}).
func (p *Provider) SetIssueState(ctx context.Context, ref domain.RepositoryRef, number int, state domain.IssueState) (*domain.Issue, error) {
	if err := p.require(forge.CapIssueState); err != nil {
		return nil, err
	}
	if state != domain.IssueOpen && state != domain.IssueClosed {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: fmt.Sprintf("issue state must be open or closed, not %q", state)}
	}
	path, err := p.issuePath(ref, number)
	if err != nil {
		return nil, err
	}
	body := struct {
		State domain.IssueState `json:"state"`
	}{state}
	var is domain.Issue
	if _, err := p.send(ctx, call{method: http.MethodPatch, url: p.endpoint(path, nil), body: body, out: &is, op: fmt.Sprintf("set state of issue %s#%d", ref.FullName(), number)}); err != nil {
		return nil, err
	}
	return &is, nil
}

func (p *Provider) issuePath(ref domain.RepositoryRef, number int) (string, error) {
	path, err := p.repoPath(ref)
	if err != nil {
		return "", err
	}
	if number <= 0 {
		return "", &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: fmt.Sprintf("invalid issue number %d", number)}
	}
	return path + "/issues/" + strconv.Itoa(number), nil
}

// --- pipelines -----------------------------------------------------------------------

// ListPipelines implements forge.PipelineProvider.
func (p *Provider) ListPipelines(ctx context.Context, ref domain.RepositoryRef, opts forge.PipelineListOptions) ([]domain.Pipeline, error) {
	if err := p.require(forge.CapPipelines); err != nil {
		return nil, err
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.Ref != "" {
		q.Set("ref", opts.Ref)
	}
	if opts.Status != "" {
		q.Set("status", string(opts.Status))
	}
	return list(ctx, p, listSpec[domain.Pipeline]{
		op: "list pipelines for " + ref.FullName(), path: path + "/pipelines", query: q, limit: opts.Limit,
		notFound: errs.ErrRepositoryNotFound,
		keep: func(pl *domain.Pipeline) bool {
			normalizePipeline(pl)
			if opts.Ref != "" && pl.Ref != opts.Ref {
				return false
			}
			return opts.Status == "" || pl.Status == opts.Status
		},
	})
}

// GetPipeline implements forge.PipelineProvider; the response includes jobs.
func (p *Provider) GetPipeline(ctx context.Context, ref domain.RepositoryRef, id string) (*domain.Pipeline, error) {
	if err := p.require(forge.CapPipelines); err != nil {
		return nil, err
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: "pipeline id is required"}
	}
	var pl domain.Pipeline
	if _, err := p.send(ctx, call{method: http.MethodGet, url: p.endpoint(path+"/pipelines/"+url.PathEscape(id), nil), out: &pl, op: fmt.Sprintf("get pipeline %s in %s", id, ref.FullName())}); err != nil {
		return nil, err
	}
	normalizePipeline(&pl)
	return &pl, nil
}

func normalizePipeline(pl *domain.Pipeline) {
	pl.Status, pl.RawStatus = normalizeStatus(pl.Status, pl.RawStatus)
	if pl.Duration == 0 && !pl.StartedAt.IsZero() && pl.FinishedAt.After(pl.StartedAt) {
		pl.Duration = pl.FinishedAt.Sub(pl.StartedAt)
	}
	for i := range pl.Jobs {
		pl.Jobs[i].Status, pl.Jobs[i].RawStatus = normalizeStatus(pl.Jobs[i].Status, pl.Jobs[i].RawStatus)
	}
}

// normalizeStatus maps statuses outside Trove's vocabulary to "unknown",
// preserving the original in raw.
func normalizeStatus(s domain.PipelineStatus, raw string) (domain.PipelineStatus, string) {
	switch s {
	case domain.PipelinePending, domain.PipelineRunning, domain.PipelineSuccess, domain.PipelineFailed,
		domain.PipelineCanceled, domain.PipelineSkipped, domain.PipelineManual, domain.PipelineUnknown:
		return s, raw
	}
	return domain.PipelineUnknown, firstNonEmpty(raw, string(s))
}

// --- releases ------------------------------------------------------------------------

// ListReleases implements forge.ReleaseProvider.
func (p *Provider) ListReleases(ctx context.Context, ref domain.RepositoryRef, opts forge.ListOptions) ([]domain.Release, error) {
	if err := p.require(forge.CapReleases); err != nil {
		return nil, err
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, err
	}
	return list(ctx, p, listSpec[domain.Release]{
		op: "list releases for " + ref.FullName(), path: path + "/releases", limit: opts.Limit,
		notFound: errs.ErrRepositoryNotFound,
	})
}

// GetRelease implements forge.ReleaseProvider.
func (p *Provider) GetRelease(ctx context.Context, ref domain.RepositoryRef, tag string) (*domain.Release, error) {
	if err := p.require(forge.CapReleases); err != nil {
		return nil, err
	}
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(tag) == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: "release tag is required"}
	}
	var rel domain.Release
	if _, err := p.send(ctx, call{method: http.MethodGet, url: p.endpoint(path+"/releases/"+url.PathEscape(tag), nil), out: &rel, op: fmt.Sprintf("get release %s of %s", tag, ref.FullName())}); err != nil {
		return nil, err
	}
	return &rel, nil
}

// --- namespaces ----------------------------------------------------------------------

// ListNamespaces implements forge.NamespaceProvider.
func (p *Provider) ListNamespaces(ctx context.Context, opts forge.ListOptions) ([]domain.Namespace, error) {
	if err := p.require(forge.CapNamespaces); err != nil {
		return nil, err
	}
	return list(ctx, p, listSpec[domain.Namespace]{
		op: "list namespaces", path: "/v1/namespaces", limit: opts.Limit,
		keep: func(n *domain.Namespace) bool { p.normalizeNamespace(n); return true },
	})
}

// GetNamespace implements forge.NamespaceProvider. Nested paths are escaped
// into one segment: "a/b" is requested as /v1/namespaces/a%2Fb.
func (p *Provider) GetNamespace(ctx context.Context, path string) (*domain.Namespace, error) {
	if err := p.require(forge.CapNamespaces); err != nil {
		return nil, err
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return nil, &errs.Error{Kind: errs.ErrInvalidArgument, Provider: p.acct.Name, Message: "namespace path is required"}
	}
	var n domain.Namespace
	if _, err := p.send(ctx, call{method: http.MethodGet, url: p.endpoint("/v1/namespaces/"+url.PathEscape(path), nil), out: &n, op: "get namespace " + path}); err != nil {
		return nil, err
	}
	p.normalizeNamespace(&n)
	return &n, nil
}

func (p *Provider) normalizeNamespace(n *domain.Namespace) {
	if n.Label == "" {
		n.Label = p.s.terms.Namespace
	}
}

// --- summary -------------------------------------------------------------------------

// Summary implements forge.SummaryProvider (GET /v1/summary). Counts the
// server leaves out stay nil.
func (p *Provider) Summary(ctx context.Context) (*domain.AccountSummary, error) {
	if err := p.require(forge.CapSummary); err != nil {
		return nil, err
	}
	var s domain.AccountSummary
	if _, err := p.send(ctx, call{method: http.MethodGet, url: p.endpoint("/v1/summary", nil), out: &s, op: "get account summary"}); err != nil {
		return nil, err
	}
	return &s, nil
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
