package cloud

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

type bbPREndpoint struct {
	Branch struct {
		Name string `json:"name"`
	} `json:"branch"`
	Commit *struct {
		Hash string `json:"hash"`
	} `json:"commit"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type bbPullRequest struct {
	ID          int          `json:"id"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	State       string       `json:"state"`
	Draft       bool         `json:"draft"`
	Author      *bbAccount   `json:"author"`
	Source      bbPREndpoint `json:"source"`
	Destination bbPREndpoint `json:"destination"`
	Reviewers   []bbAccount  `json:"reviewers"`
	CreatedOn   time.Time    `json:"created_on"`
	UpdatedOn   time.Time    `json:"updated_on"`
	Links       bbLinks      `json:"links"`
}

func (p *Provider) toPullRequest(pr bbPullRequest) domain.PullRequest {
	out := domain.PullRequest{
		ID:           strconv.Itoa(pr.ID),
		Number:       pr.ID,
		Title:        pr.Title,
		Body:         pr.Description,
		State:        prState(pr.State),
		Draft:        pr.Draft,
		Author:       pr.Author.handle(),
		SourceBranch: pr.Source.Branch.Name,
		TargetBranch: pr.Destination.Branch.Name,
		WebURL:       pr.Links.HTML.Href,
		CreatedAt:    pr.CreatedOn,
		UpdatedAt:    pr.UpdatedOn,
		Term:         "Pull Request",
	}
	// Bitbucket exposes no fetchable pull request refs, so the source
	// repository (the base repository or a fork) and branch are what the CLI
	// fetches for checkout; always report the source repository.
	if pr.Source.Repository != nil {
		out.SourceRepo = pr.Source.Repository.FullName
	}
	if pr.Source.Commit != nil {
		out.HeadSHA = pr.Source.Commit.Hash
	}
	for _, r := range pr.Reviewers {
		if h := r.handle(); h != "" {
			out.Reviewers = append(out.Reviewers, h)
		}
	}
	return out
}

func prState(s string) domain.PullRequestState {
	switch strings.ToUpper(s) {
	case "OPEN":
		return domain.PullRequestOpen
	case "MERGED":
		return domain.PullRequestMerged
	default: // DECLINED, SUPERSEDED
		return domain.PullRequestClosed
	}
}

func prStateParams(s domain.PullRequestState) []string {
	switch s {
	case domain.PullRequestMerged:
		return []string{"MERGED"}
	case domain.PullRequestClosed:
		return []string{"DECLINED", "SUPERSEDED"}
	case domain.PullRequestAll:
		return []string{"OPEN", "MERGED", "DECLINED", "SUPERSEDED"}
	default:
		return []string{"OPEN"}
	}
}

// ListPullRequests implements forge.PullRequestProvider. States are passed
// as repeated state parameters; Author and TargetBranch become BBQL filters
// (author.uuid for "{uuid}" values, author.nickname otherwise).
func (p *Provider) ListPullRequests(ctx context.Context, ref domain.RepositoryRef, opts forge.PullRequestListOptions) ([]domain.PullRequest, error) {
	const op = "list pull requests"
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	q := url.Values{"pagelen": {itoa(forge.PageSize(opts.Limit, maxPRPageLen))}, "state": prStateParams(opts.State)}
	var bbql []string
	if a := strings.TrimSpace(opts.Author); a != "" {
		if strings.HasPrefix(a, "{") {
			bbql = append(bbql, "author.uuid="+bbqlString(a))
		} else {
			bbql = append(bbql, "author.nickname="+bbqlString(strings.TrimPrefix(a, "@")))
		}
	}
	if b := strings.TrimSpace(opts.TargetBranch); b != "" {
		bbql = append(bbql, "destination.branch.name="+bbqlString(b))
	}
	if len(bbql) > 0 {
		q.Set("q", strings.Join(bbql, " AND "))
	}
	raw, err := collectPages[bbPullRequest](ctx, p, opts.Limit, []request{{op: op, url: path + "/pullrequests", query: q,
		notFoundMsg: "repository " + ref.FullName() + " not found"}}, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.PullRequest, 0, len(raw))
	for _, pr := range raw {
		out = append(out, p.toPullRequest(pr))
	}
	return out, nil
}

// GetPullRequest implements forge.PullRequestProvider.
func (p *Provider) GetPullRequest(ctx context.Context, ref domain.RepositoryRef, number int) (*domain.PullRequest, error) {
	const op = "get pull request"
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	var pr bbPullRequest
	if err := p.do(ctx, request{op: op, method: http.MethodGet, url: path + "/pullrequests/" + strconv.Itoa(number),
		notFoundMsg: "pull request #" + strconv.Itoa(number) + " not found in " + ref.FullName()}, &pr); err != nil {
		return nil, err
	}
	out := p.toPullRequest(pr)
	return &out, nil
}

// CreatePullRequest implements forge.PullRequestCreator. A SourceRepo
// ("workspace/slug") targets a fork; an empty TargetBranch lets Bitbucket use
// the repository's main branch.
func (p *Provider) CreatePullRequest(ctx context.Context, ref domain.RepositoryRef, req forge.CreatePullRequestRequest) (*domain.PullRequest, error) {
	const op = "create pull request"
	path, err := p.repoPath(ref)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.SourceBranch) == "" {
		return nil, p.invalidArg(op, "a title and a source branch are required")
	}
	source := map[string]any{"branch": map[string]string{"name": req.SourceBranch}}
	if req.SourceRepo != "" && !strings.EqualFold(req.SourceRepo, ref.FullName()) {
		source["repository"] = map[string]string{"full_name": req.SourceRepo}
	}
	body := map[string]any{
		"title":               req.Title,
		"source":              source,
		"close_source_branch": false,
	}
	if req.Body != "" {
		body["description"] = req.Body
	}
	if req.TargetBranch != "" {
		body["destination"] = map[string]any{"branch": map[string]string{"name": req.TargetBranch}}
	}
	if req.Draft {
		body["draft"] = true
	}
	var pr bbPullRequest
	if err := p.do(ctx, request{op: op, method: http.MethodPost, url: path + "/pullrequests", body: body,
		notFoundMsg: "repository " + ref.FullName() + " not found"}, &pr); err != nil {
		return nil, err
	}
	out := p.toPullRequest(pr)
	return &out, nil
}

// MergeMethods implements forge.PullRequestMerger. Bitbucket's strategies
// map as merge -> merge_commit, squash -> squash and rebase ->
// rebase_fast_forward (rebase onto the destination, then fast-forward). The
// repository's branch settings decide which strategies are allowed.
func (p *Provider) MergeMethods() []domain.MergeMethod {
	return []domain.MergeMethod{domain.MergeMethodMerge, domain.MergeMethodSquash, domain.MergeMethodRebase}
}

func mergeStrategy(m domain.MergeMethod) (string, bool) {
	switch m {
	case "":
		return "", true // repository default
	case domain.MergeMethodMerge:
		return "merge_commit", true
	case domain.MergeMethodSquash:
		return "squash", true
	case domain.MergeMethodRebase:
		return "rebase_fast_forward", true
	}
	return "", false
}

// MergePullRequest implements forge.PullRequestMerger with a synchronous
// POST .../pullrequests/{id}/merge.
func (p *Provider) MergePullRequest(ctx context.Context, ref domain.RepositoryRef, number int, req forge.MergePullRequestRequest) error {
	const op = "merge pull request"
	path, err := p.repoPath(ref)
	if err != nil {
		return p.wrapOp(op, err)
	}
	strategy, ok := mergeStrategy(req.Method)
	if !ok {
		return p.invalidArg(op, "unsupported merge method "+string(req.Method)+" (supported: merge, squash, rebase)")
	}
	body := map[string]any{"type": "pullrequest_merge_parameters"}
	if req.DeleteSourceBranch {
		// When omitted Bitbucket uses the value chosen at creation time.
		body["close_source_branch"] = true
	}
	if strategy != "" {
		body["merge_strategy"] = strategy
	}
	msg := strings.TrimSpace(req.CommitTitle)
	if m := strings.TrimSpace(req.CommitMessage); m != "" {
		if msg != "" {
			msg += "\n\n"
		}
		msg += m
	}
	if msg != "" {
		body["message"] = msg
	}
	return p.do(ctx, request{op: op, method: http.MethodPost, url: path + "/pullrequests/" + strconv.Itoa(number) + "/merge",
		body: body, notFoundMsg: "pull request #" + strconv.Itoa(number) + " not found in " + ref.FullName()}, nil)
}

// ClosePullRequest implements forge.PullRequestCloser by declining the pull
// request (Bitbucket's "closed without merging" state).
func (p *Provider) ClosePullRequest(ctx context.Context, ref domain.RepositoryRef, number int) error {
	const op = "decline pull request"
	path, err := p.repoPath(ref)
	if err != nil {
		return p.wrapOp(op, err)
	}
	return p.do(ctx, request{op: op, method: http.MethodPost, url: path + "/pullrequests/" + strconv.Itoa(number) + "/decline",
		notFoundMsg: "pull request #" + strconv.Itoa(number) + " not found in " + ref.FullName()}, nil)
}
