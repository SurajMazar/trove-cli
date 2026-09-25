package gitlab

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

type glMergeRequest struct {
	ID                  int64       `json:"id"`
	IID                 int         `json:"iid"`
	Title               string      `json:"title"`
	Description         string      `json:"description"`
	State               string      `json:"state"`
	Draft               bool        `json:"draft"`
	WorkInProgress      bool        `json:"work_in_progress"` // pre-14.0 name of draft
	SourceBranch        string      `json:"source_branch"`
	TargetBranch        string      `json:"target_branch"`
	SourceProjectID     int64       `json:"source_project_id"`
	TargetProjectID     int64       `json:"target_project_id"`
	SHA                 string      `json:"sha"`
	MergeStatus         string      `json:"merge_status"`          // deprecated in 15.6
	DetailedMergeStatus string      `json:"detailed_merge_status"` // 15.6+
	Labels              []string    `json:"labels"`
	WebURL              string      `json:"web_url"`
	CreatedAt           *time.Time  `json:"created_at"`
	UpdatedAt           *time.Time  `json:"updated_at"`
	MergedAt            *time.Time  `json:"merged_at"`
	ClosedAt            *time.Time  `json:"closed_at"`
	Author              *glUserRef  `json:"author"`
	Reviewers           []glUserRef `json:"reviewers"`
}

type glUserRef struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func (u *glUserRef) name() string {
	if u == nil {
		return ""
	}
	return u.Username
}

func mrState(s string) domain.PullRequestState {
	switch s {
	case "merged":
		return domain.PullRequestMerged
	case "closed":
		return domain.PullRequestClosed
	default: // "opened", and "locked" (an open MR that is transiently locked while merging)
		return domain.PullRequestOpen
	}
}

// mergeable interprets detailed_merge_status (or the legacy merge_status).
// Transitional states ("checking", "unchecked", ...) yield nil: unknown.
func (m glMergeRequest) mergeable() *bool {
	yes, no := true, false
	switch m.DetailedMergeStatus {
	case "mergeable":
		return &yes
	case "", "checking", "unchecked", "preparing", "approvals_syncing":
	default:
		return &no
	}
	switch m.MergeStatus {
	case "can_be_merged":
		return &yes
	case "cannot_be_merged", "cannot_be_merged_recheck":
		return &no
	}
	return nil
}

func (p *provider) toPullRequest(ctx context.Context, m glMergeRequest) domain.PullRequest {
	pr := domain.PullRequest{
		ID: strconv.FormatInt(m.ID, 10), Number: m.IID, Title: m.Title, Body: m.Description,
		State: mrState(m.State), Draft: m.Draft || m.WorkInProgress, Author: m.Author.name(),
		SourceBranch: m.SourceBranch, TargetBranch: m.TargetBranch, HeadSHA: m.SHA,
		Mergeable: m.mergeable(), Labels: m.Labels, WebURL: m.WebURL, Term: "Merge Request",
	}
	for _, r := range m.Reviewers {
		pr.Reviewers = append(pr.Reviewers, r.Username)
	}
	if m.SourceProjectID != 0 && m.TargetProjectID != 0 && m.SourceProjectID != m.TargetProjectID {
		// Cross-project (fork) MR: GitLab only reports the source project ID.
		pr.SourceRepo = p.projectFullPath(ctx, m.SourceProjectID)
		if pr.SourceRepo == "" {
			pr.SourceRepo = strconv.FormatInt(m.SourceProjectID, 10)
		}
	}
	setTime(&pr.CreatedAt, m.CreatedAt)
	setTime(&pr.UpdatedAt, m.UpdatedAt)
	setTime(&pr.MergedAt, m.MergedAt)
	setTime(&pr.ClosedAt, m.ClosedAt)
	return pr
}

func setTime(dst *time.Time, src *time.Time) {
	if src != nil {
		*dst = *src
	}
}

func (p *provider) ListPullRequests(ctx context.Context, ref domain.RepositoryRef, opts forge.PullRequestListOptions) ([]domain.PullRequest, error) {
	op := "list merge requests of " + ref.FullName()
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	q := url.Values{}
	switch opts.State {
	case "", domain.PullRequestOpen:
		q.Set("state", "opened")
	case domain.PullRequestClosed:
		q.Set("state", "closed")
	case domain.PullRequestMerged:
		q.Set("state", "merged")
	case domain.PullRequestAll:
		// No state filter returns merge requests in every state.
	default:
		return nil, p.invalid(op, "unknown merge request state %q", opts.State)
	}
	if opts.Author != "" {
		q.Set("author_username", opts.Author)
	}
	if opts.TargetBranch != "" {
		q.Set("target_branch", opts.TargetBranch)
	}
	mrs, err := list[glMergeRequest](ctx, p, op, path+"/merge_requests", q, opts.Limit, errs.ErrRepositoryNotFound)
	if err != nil {
		return nil, err
	}
	out := make([]domain.PullRequest, 0, len(mrs))
	for _, m := range mrs {
		out = append(out, p.toPullRequest(ctx, m))
	}
	return out, nil
}

// GetPullRequest fetches a merge request by its IID (the project-scoped
// number shown as !N), not by its global ID.
func (p *provider) GetPullRequest(ctx context.Context, ref domain.RepositoryRef, number int) (*domain.PullRequest, error) {
	op := fmt.Sprintf("get merge request !%d of %s", number, ref.FullName())
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	var m glMergeRequest
	if err := p.get(ctx, op, path+"/merge_requests/"+strconv.Itoa(number), nil, &m); err != nil {
		return nil, err
	}
	pr := p.toPullRequest(ctx, m)
	return &pr, nil
}

// draftPrefixed reports whether a title already marks the MR as a draft.
// GitLab recognizes "Draft:", "[Draft]" and "(Draft)" prefixes.
func draftPrefixed(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	return strings.HasPrefix(t, "draft:") || strings.HasPrefix(t, "[draft]") || strings.HasPrefix(t, "(draft)")
}

// CreatePullRequest opens a merge request against ref.
//
// Drafts are created by prefixing the title with "Draft: ", which is how the
// GitLab API marks merge requests as drafts.
//
// When the source branch lives in a fork (req.SourceRepo), GitLab requires
// the merge request to be created on the fork (the source project) with
// target_project_id set to the upstream project's numeric ID.
func (p *provider) CreatePullRequest(ctx context.Context, ref domain.RepositoryRef, req forge.CreatePullRequestRequest) (*domain.PullRequest, error) {
	op := "create merge request in " + ref.FullName()
	if strings.TrimSpace(req.Title) == "" {
		return nil, p.invalid(op, "a title is required")
	}
	if req.SourceBranch == "" {
		return nil, p.invalid(op, "a source branch is required")
	}
	targetPath, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	title := req.Title
	if req.Draft && !draftPrefixed(title) {
		title = "Draft: " + title
	}
	body := map[string]any{"source_branch": req.SourceBranch, "title": title}
	if req.Body != "" {
		body["description"] = req.Body
	}

	createPath := targetPath
	fork := req.SourceRepo != "" && !strings.EqualFold(strings.Trim(req.SourceRepo, "/"), ref.FullName()) && req.SourceRepo != ref.ID
	var target *domain.Repository
	if fork || req.TargetBranch == "" {
		if target, err = p.GetRepository(ctx, ref); err != nil {
			return nil, err
		}
	}
	targetBranch := req.TargetBranch
	if targetBranch == "" {
		targetBranch = target.DefaultBranch
		if targetBranch == "" {
			return nil, p.invalid(op, "a target branch is required (the project has no default branch)")
		}
	}
	body["target_branch"] = targetBranch
	if fork {
		id, err := strconv.ParseInt(target.ID, 10, 64)
		if err != nil {
			return nil, p.invalid(op, "could not determine the numeric ID of %s", ref.FullName())
		}
		body["target_project_id"] = id
		createPath = "/projects/" + url.PathEscape(strings.Trim(req.SourceRepo, "/"))
	}
	var m glMergeRequest
	if _, err := p.do(ctx, call{method: http.MethodPost, path: createPath + "/merge_requests", op: op, body: body, notFound: errs.ErrRepositoryNotFound}, &m); err != nil {
		return nil, err
	}
	pr := p.toPullRequest(ctx, m)
	return &pr, nil
}

// MergeMethods lists what PUT /merge_requests/:iid/merge can do. GitLab has
// no per-request "rebase" merge: rebasing is a separate operation
// (PUT /merge_requests/:iid/rebase) and whether a merge produces a merge
// commit, a semi-linear history or a fast-forward is a project setting.
func (p *provider) MergeMethods() []domain.MergeMethod {
	return []domain.MergeMethod{domain.MergeMethodMerge, domain.MergeMethodSquash}
}

func (p *provider) MergePullRequest(ctx context.Context, ref domain.RepositoryRef, number int, req forge.MergePullRequestRequest) error {
	op := fmt.Sprintf("merge merge request !%d of %s", number, ref.FullName())
	path, err := projectPath(ref)
	if err != nil {
		return withProvider(err, p.acct.Name, op)
	}
	body := map[string]any{}
	msg := req.CommitTitle
	if req.CommitMessage != "" {
		if msg != "" {
			msg += "\n\n"
		}
		msg += req.CommitMessage
	}
	switch req.Method {
	case "", domain.MergeMethodMerge:
		body["squash"] = false
		if msg != "" {
			body["merge_commit_message"] = msg
		}
	case domain.MergeMethodSquash:
		body["squash"] = true
		if msg != "" {
			body["squash_commit_message"] = msg
		}
	case domain.MergeMethodRebase:
		e := p.invalid(op, "GitLab does not merge by rebasing in a single step; use the merge or squash method (the project's merge method setting decides between merge commits, semi-linear history and fast-forward merges)")
		e.Hint = "rebase the source branch first (GitLab: \"Rebase\" button or PUT /merge_requests/:iid/rebase), then merge"
		return e
	default:
		return p.invalid(op, "unknown merge method %q (GitLab supports merge and squash)", req.Method)
	}
	if req.DeleteSourceBranch {
		// Only sent when requested so the MR's own "delete source branch"
		// setting is otherwise respected.
		body["should_remove_source_branch"] = true
	}
	_, err = p.do(ctx, call{method: http.MethodPut, path: path + "/merge_requests/" + strconv.Itoa(number) + "/merge", op: op, body: body}, nil)
	var e *errs.Error
	if errors.As(err, &e) {
		switch e.Status {
		case http.StatusMethodNotAllowed, http.StatusNotAcceptable, http.StatusUnprocessableEntity:
			// 405: not open / draft / blocked by discussions or approvals;
			// 406 or 422: the branch cannot be merged (conflicts).
			cp := *e
			cp.Kind = errs.ErrConflict
			cp.Message = "the merge request cannot be merged in its current state"
			detail := strings.TrimPrefix(e.Message, fmt.Sprintf("GitLab API error (HTTP %d): ", e.Status))
			if detail != "" && !strings.HasPrefix(detail, "GitLab API error") {
				cp.Message += ": " + detail
			}
			return &cp
		}
	}
	return err
}

func (p *provider) ClosePullRequest(ctx context.Context, ref domain.RepositoryRef, number int) error {
	op := fmt.Sprintf("close merge request !%d of %s", number, ref.FullName())
	path, err := projectPath(ref)
	if err != nil {
		return withProvider(err, p.acct.Name, op)
	}
	_, err = p.do(ctx, call{method: http.MethodPut, path: path + "/merge_requests/" + strconv.Itoa(number), op: op, body: map[string]any{"state_event": "close"}}, nil)
	return err
}

// PullRequestHeadRef is the ref GitLab publishes on the target project for
// every merge request, including those from forks.
func (p *provider) PullRequestHeadRef(number int) string {
	return fmt.Sprintf("refs/merge-requests/%d/head", number)
}
