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

type apiLabel struct {
	Name string `json:"name"`
}

type apiBranch struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"` // null when the fork was deleted
}

type apiPull struct {
	ID                 int64      `json:"id"`
	Number             int        `json:"number"`
	Title              string     `json:"title"`
	Body               *string    `json:"body"`
	State              string     `json:"state"`
	Draft              bool       `json:"draft"`
	User               *apiOwner  `json:"user"`
	Head               apiBranch  `json:"head"`
	Base               apiBranch  `json:"base"`
	Mergeable          *bool      `json:"mergeable"`
	Labels             []apiLabel `json:"labels"`
	RequestedReviewers []apiOwner `json:"requested_reviewers"`
	HTMLURL            string     `json:"html_url"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	MergedAt           *time.Time `json:"merged_at"`
	ClosedAt           *time.Time `json:"closed_at"`
}

func timeOf(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func labelNames(ls []apiLabel) []string {
	if len(ls) == 0 {
		return nil
	}
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func logins(us []apiOwner) []string {
	if len(us) == 0 {
		return nil
	}
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.Login)
	}
	return out
}

func (p *Provider) toPullRequest(pr apiPull) domain.PullRequest {
	state := domain.PullRequestOpen
	if pr.State == "closed" {
		state = domain.PullRequestClosed
		if pr.MergedAt != nil {
			// GitHub reports merged PRs as state "closed" with merged_at set.
			state = domain.PullRequestMerged
		}
	}
	out := domain.PullRequest{
		ID: strconv.FormatInt(pr.ID, 10), Number: pr.Number, Title: pr.Title, Body: str(pr.Body),
		State: state, Draft: pr.Draft, SourceBranch: pr.Head.Ref, TargetBranch: pr.Base.Ref,
		HeadSHA: pr.Head.SHA, Mergeable: pr.Mergeable, Labels: labelNames(pr.Labels),
		Reviewers: logins(pr.RequestedReviewers), WebURL: pr.HTMLURL,
		CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt, MergedAt: timeOf(pr.MergedAt), ClosedAt: timeOf(pr.ClosedAt),
		Term: p.meta.Terms.PullRequest,
	}
	if pr.User != nil {
		out.Author = pr.User.Login
	}
	if pr.Head.Repo != nil && pr.Base.Repo != nil && !strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName) {
		out.SourceRepo = pr.Head.Repo.FullName
	}
	return out
}

// ListPullRequests implements forge.PullRequestProvider. GitHub has no
// "merged" state filter and no author filter on this endpoint: merged/closed
// are separated client-side from state=closed (merged_at set or not), and
// authors are matched client-side.
func (p *Provider) ListPullRequests(ctx context.Context, ref domain.RepositoryRef, opts forge.PullRequestListOptions) ([]domain.PullRequest, error) {
	q := url.Values{}
	switch opts.State {
	case "", domain.PullRequestOpen:
		q.Set("state", "open")
	case domain.PullRequestClosed, domain.PullRequestMerged:
		q.Set("state", "closed")
	case domain.PullRequestAll:
		q.Set("state", "all")
	default:
		return nil, p.errorf(errs.ErrInvalidArgument, "list pull requests", 0, "invalid pull request state %q", opts.State)
	}
	if opts.TargetBranch != "" {
		q.Set("base", opts.TargetBranch)
	}
	return paginate(ctx, p, listSpec[apiPull]{
		request: request{op: "list pull requests", path: repoPath(ref.Namespace, ref.Name, "pulls"), query: q,
			notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref)},
		perPage: maxPerPage, limit: opts.Limit,
	}, func(raw apiPull) (domain.PullRequest, bool) {
		pr := p.toPullRequest(raw)
		switch opts.State {
		case domain.PullRequestClosed, domain.PullRequestMerged:
			if pr.State != opts.State {
				return pr, false
			}
		}
		if opts.Author != "" && !strings.EqualFold(pr.Author, strings.TrimPrefix(opts.Author, "@")) {
			return pr, false
		}
		return pr, true
	})
}

func (p *Provider) getPull(ctx context.Context, op string, ref domain.RepositoryRef, number int) (*apiPull, error) {
	var pr apiPull
	_, err := p.do(ctx, request{
		op: op, method: http.MethodGet, path: repoPath(ref.Namespace, ref.Name, "pulls", strconv.Itoa(number)), out: &pr,
		notFoundMsg: fmt.Sprintf("pull request #%d not found in %s", number, ref.FullName()),
	})
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

// GetPullRequest implements forge.PullRequestProvider.
func (p *Provider) GetPullRequest(ctx context.Context, ref domain.RepositoryRef, number int) (*domain.PullRequest, error) {
	pr, err := p.getPull(ctx, "get pull request", ref, number)
	if err != nil {
		return nil, err
	}
	out := p.toPullRequest(*pr)
	return &out, nil
}

// CreatePullRequest implements forge.PullRequestCreator. For a source branch
// in a fork, head is "owner:branch"; for another repository with the same
// owner, GitHub needs head_repo instead.
func (p *Provider) CreatePullRequest(ctx context.Context, ref domain.RepositoryRef, req forge.CreatePullRequestRequest) (*domain.PullRequest, error) {
	const op = "create pull request"
	if strings.TrimSpace(req.Title) == "" || req.SourceBranch == "" || req.TargetBranch == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "title, source branch and target branch are required")
	}
	body := map[string]any{"title": req.Title, "head": req.SourceBranch, "base": req.TargetBranch, "draft": req.Draft}
	if req.Body != "" {
		body["body"] = req.Body
	}
	if src := strings.Trim(req.SourceRepo, "/"); src != "" && !strings.EqualFold(src, ref.FullName()) {
		owner, repo, _ := strings.Cut(src, "/")
		if strings.EqualFold(owner, ref.Namespace) && repo != "" {
			body["head_repo"] = repo
		} else {
			body["head"] = owner + ":" + req.SourceBranch
		}
	}
	var pr apiPull
	_, err := p.do(ctx, request{op: op, method: http.MethodPost, path: repoPath(ref.Namespace, ref.Name, "pulls"),
		body: body, out: &pr, notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref)})
	if err != nil {
		return nil, err
	}
	out := p.toPullRequest(pr)
	return &out, nil
}

// MergeMethods implements forge.PullRequestMerger. Each repository can
// disable some of them; GitHub then rejects the merge with 405.
func (p *Provider) MergeMethods() []domain.MergeMethod {
	return []domain.MergeMethod{domain.MergeMethodMerge, domain.MergeMethodSquash, domain.MergeMethodRebase}
}

// MergePullRequest implements forge.PullRequestMerger. GitHub has no
// "delete source branch" merge option, so the branch ref is deleted
// afterwards, and only when it lives in the base repository.
func (p *Provider) MergePullRequest(ctx context.Context, ref domain.RepositoryRef, number int, req forge.MergePullRequestRequest) error {
	const op = "merge pull request"
	method := req.Method
	if method == "" {
		method = domain.MergeMethodMerge
	}
	switch method {
	case domain.MergeMethodMerge, domain.MergeMethodSquash, domain.MergeMethodRebase:
	default:
		return p.errorf(errs.ErrInvalidArgument, op, 0, "unsupported merge method %q (want merge, squash or rebase)", method)
	}
	var pr *apiPull
	if req.DeleteSourceBranch {
		var err error
		if pr, err = p.getPull(ctx, op, ref, number); err != nil {
			return err
		}
	}
	body := map[string]any{"merge_method": string(method)}
	if req.CommitTitle != "" {
		body["commit_title"] = req.CommitTitle
	}
	if req.CommitMessage != "" {
		body["commit_message"] = req.CommitMessage
	}
	_, err := p.do(ctx, request{
		op: op, method: http.MethodPut, path: repoPath(ref.Namespace, ref.Name, "pulls", strconv.Itoa(number), "merge"),
		body: body, notFoundMsg: fmt.Sprintf("pull request #%d not found in %s", number, ref.FullName()),
		// 405: not mergeable (conflicts, checks, disabled method);
		// 409: head changed during the merge.
		statusKinds: map[int]error{http.StatusMethodNotAllowed: errs.ErrConflict},
	})
	if err != nil {
		return err
	}
	if pr == nil || pr.Head.Repo == nil || pr.Base.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName) {
		return nil
	}
	_, err = p.do(ctx, request{
		op: "delete source branch", method: http.MethodDelete,
		path: repoPath(ref.Namespace, ref.Name, "git", "refs", "heads", escRef(pr.Head.Ref)),
	})
	// 422 "Reference does not exist": the repository deleted it
	// automatically ("Automatically delete head branches").
	if err != nil && (errors.Is(err, errs.ErrInvalidArgument) || errors.Is(err, errs.ErrNotFound)) {
		return nil
	}
	return err
}

// ClosePullRequest implements forge.PullRequestCloser.
func (p *Provider) ClosePullRequest(ctx context.Context, ref domain.RepositoryRef, number int) error {
	_, err := p.do(ctx, request{
		op: "close pull request", method: http.MethodPatch, path: repoPath(ref.Namespace, ref.Name, "pulls", strconv.Itoa(number)),
		body: map[string]string{"state": "closed"}, notFoundMsg: fmt.Sprintf("pull request #%d not found in %s", number, ref.FullName()),
	})
	return err
}

// PullRequestHeadRef implements forge.PullRequestHeadRefer.
func (p *Provider) PullRequestHeadRef(number int) string {
	return fmt.Sprintf("refs/pull/%d/head", number)
}
