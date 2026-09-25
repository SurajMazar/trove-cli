package github

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

func setup(t *testing.T) (*fakeGitHub, *Provider) {
	t.Helper()
	f := newFake(t, "")
	return f, newTestProvider(t, f, nil)
}

var bg = context.Background()

func TestListRepositoriesNamespaces(t *testing.T) {
	f, p := setup(t)
	org, err := p.ListRepositories(bg, forge.ListRepositoryOptions{Namespace: "octo-org"})
	if err != nil {
		t.Fatal(err)
	}
	if len(org) != 2 { // one archived
		t.Fatalf("org repos = %d", len(org))
	}
	if org[1].Visibility != domain.VisibilityInternal {
		t.Fatalf("internal visibility lost: %+v", org[1])
	}
	if q := f.last(http.MethodGet, "/orgs/octo-org/repos").Query; q.Get("type") != "all" {
		t.Fatalf("org query = %v", q)
	}

	// Not an org -> user fallback.
	hubot, err := p.ListRepositories(bg, forge.ListRepositoryOptions{Namespace: "hubot"})
	if err != nil || len(hubot) != 2 || hubot[0].FullName != "hubot/h1" {
		t.Fatalf("user fallback: %v %v", hubot, err)
	}
	if len(f.requests(http.MethodGet, "/orgs/hubot/repos")) != 1 || len(f.requests(http.MethodGet, "/users/hubot/repos")) != 1 {
		t.Fatal("expected org attempt then user fallback")
	}

	// The authenticated user's own namespace uses /user/repos (private too).
	own, err := p.ListRepositories(bg, forge.ListRepositoryOptions{Namespace: "octocat", IncludeArchived: true})
	if err != nil || len(own) != numUserRepos {
		t.Fatalf("own namespace: %d %v", len(own), err)
	}
	if q := f.last(http.MethodGet, "/user/repos").Query; q.Get("affiliation") != "owner" {
		t.Fatalf("own namespace query = %v", q)
	}

	_, err = p.ListRepositories(bg, forge.ListRepositoryOptions{Namespace: "nobody"})
	assertKind(t, err, errs.ErrNotFound)
}

func TestRepositoryMutations(t *testing.T) {
	f, p := setup(t)

	r, err := p.CreateRepository(bg, forge.CreateRepositoryRequest{Name: "new", Description: "d", Visibility: domain.VisibilityPrivate})
	if err != nil || r.FullName != "octocat/new" || r.Visibility != domain.VisibilityPrivate {
		t.Fatalf("create: %+v %v", r, err)
	}
	body := f.last(http.MethodPost, "/user/repos").Body
	if !reflect.DeepEqual(body, map[string]any{"name": "new", "description": "d", "private": true}) {
		t.Fatalf("create body = %v", body)
	}

	r, err = p.CreateRepository(bg, forge.CreateRepositoryRequest{Name: "svc", Namespace: "octo-org", Visibility: domain.VisibilityInternal})
	if err != nil || r.FullName != "octo-org/svc" || r.Visibility != domain.VisibilityInternal {
		t.Fatalf("org create: %+v %v", r, err)
	}
	if body := f.last(http.MethodPost, "/orgs/octo-org/repos").Body; body["visibility"] != "internal" {
		t.Fatalf("org create body = %v", body)
	}

	_, err = p.CreateRepository(bg, forge.CreateRepositoryRequest{Name: "x", Visibility: domain.VisibilityInternal})
	assertKind(t, err, errs.ErrInvalidArgument)
	_, err = p.CreateRepository(bg, forge.CreateRepositoryRequest{Name: "x", DefaultBranch: "trunk"})
	assertKind(t, err, errs.ErrInvalidArgument)

	// Auto-init with a different default branch renames it.
	if _, err := p.CreateRepository(bg, forge.CreateRepositoryRequest{Name: "repo-init", AutoInit: true, DefaultBranch: "trunk"}); err != nil {
		t.Fatal(err)
	}
	if b := f.last(http.MethodPost, "/repos/octocat/repo-init/branches/master/rename").Body; b["new_name"] != "trunk" {
		t.Fatalf("rename branch body = %v", b)
	}

	if err := p.DeleteRepository(bg, existing); err != nil {
		t.Fatal(err)
	}
	f.last(http.MethodDelete, "/repos/octocat/repo-001")
	err = p.DeleteRepository(bg, domain.RepositoryRef{Namespace: "octocat", Name: "gone"})
	assertKind(t, err, errs.ErrRepositoryNotFound)

	fork, err := p.ForkRepository(bg, existing, forge.ForkRequest{Namespace: "octo-org", Name: "myfork"})
	if err != nil || fork.FullName != "octo-org/myfork" {
		t.Fatalf("fork: %+v %v", fork, err)
	}
	if b := f.last(http.MethodPost, "/repos/octocat/repo-001/forks").Body; !reflect.DeepEqual(b, map[string]any{"organization": "octo-org", "name": "myfork"}) {
		t.Fatalf("fork body = %v", b)
	}
	if _, err := p.ForkRepository(bg, existing, forge.ForkRequest{Namespace: "octocat"}); err != nil {
		t.Fatal(err)
	}
	if b := f.last(http.MethodPost, "/repos/octocat/repo-001/forks").Body; len(b) != 0 {
		t.Fatalf("personal fork must omit organization: %v", b)
	}

	rn, err := p.RenameRepository(bg, existing, "repo-renamed")
	if err != nil || rn.Name != "repo-renamed" {
		t.Fatalf("rename: %+v %v", rn, err)
	}
	if b := f.last(http.MethodPatch, "/repos/octocat/repo-001").Body; b["name"] != "repo-renamed" {
		t.Fatalf("rename body = %v", b)
	}

	ar, err := p.SetRepositoryArchived(bg, existing, true)
	if err != nil || !ar.Archived {
		t.Fatalf("archive: %+v %v", ar, err)
	}
	un, err := p.SetRepositoryArchived(bg, existing, false)
	if err != nil || un.Archived {
		t.Fatalf("unarchive: %+v %v", un, err)
	}
	if b := f.last(http.MethodPatch, "/repos/octocat/repo-001").Body; b["archived"] != false {
		t.Fatalf("unarchive body = %v", b)
	}
}

func TestRepositoryMapping(t *testing.T) {
	_, p := setup(t)
	r, err := p.GetRepository(bg, domain.RepositoryRef{Namespace: "octocat", Name: "repo-007"})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "1" || r.DefaultBranch != "main" || r.Language != "Go" || r.URLs.SSH != "git@example.test:octocat/repo-007.git" ||
		!strings.HasSuffix(r.URLs.HTTPS, "/octocat/repo-007.git") || r.CreatedAt.IsZero() || r.Topics[0] != "x" {
		t.Fatalf("mapping: %+v", r)
	}
}

func TestPullRequests(t *testing.T) {
	f, p := setup(t)
	path := "/repos/octocat/repo-001/pulls"

	open, err := p.ListPullRequests(bg, existing, forge.PullRequestListOptions{})
	if err != nil || len(open) != 2 || f.last(http.MethodGet, path).Query.Get("state") != "open" {
		t.Fatalf("open: %v %v", open, err)
	}
	if open[1].SourceRepo != "contrib/repo-001" || open[0].SourceRepo != "" {
		t.Fatalf("fork source repo: %+v", open)
	}
	merged, _ := p.ListPullRequests(bg, existing, forge.PullRequestListOptions{State: domain.PullRequestMerged})
	if len(merged) != 1 || merged[0].Number != 2 || merged[0].State != domain.PullRequestMerged || merged[0].MergedAt.IsZero() {
		t.Fatalf("merged: %+v", merged)
	}
	if f.last(http.MethodGet, path).Query.Get("state") != "closed" {
		t.Fatal("merged must query state=closed")
	}
	closed, _ := p.ListPullRequests(bg, existing, forge.PullRequestListOptions{State: domain.PullRequestClosed})
	if len(closed) != 1 || closed[0].Number != 3 {
		t.Fatalf("closed: %+v", closed)
	}
	byAuthor, _ := p.ListPullRequests(bg, existing, forge.PullRequestListOptions{State: domain.PullRequestAll, Author: "@Contrib", TargetBranch: "main"})
	if len(byAuthor) != 1 || byAuthor[0].Number != 4 || f.last(http.MethodGet, path).Query.Get("base") != "main" {
		t.Fatalf("author filter: %+v", byAuthor)
	}

	pr, err := p.GetPullRequest(bg, existing, 1)
	if err != nil || pr.Title != "PR 1" || pr.Author != "octocat" || pr.Labels[0] != "bug" || pr.Reviewers[0] != "rev" ||
		pr.Term != "Pull Request" || *pr.Mergeable != true {
		t.Fatalf("get: %+v %v", pr, err)
	}
	_, err = p.GetPullRequest(bg, existing, 99)
	assertKind(t, err, errs.ErrNotFound)

	// Create: same repo, fork, and same-owner other repo.
	for _, tc := range []struct {
		src  string
		want map[string]any
	}{
		{"", map[string]any{"title": "T", "head": "feat", "base": "main", "draft": true, "body": "B"}},
		{"contrib/repo-001", map[string]any{"title": "T", "head": "contrib:feat", "base": "main", "draft": true, "body": "B"}},
		{"octocat/other", map[string]any{"title": "T", "head": "feat", "head_repo": "other", "base": "main", "draft": true, "body": "B"}},
	} {
		if _, err := p.CreatePullRequest(bg, existing, forge.CreatePullRequestRequest{Title: "T", Body: "B", SourceBranch: "feat", TargetBranch: "main", Draft: true, SourceRepo: tc.src}); err != nil {
			t.Fatal(err)
		}
		if b := f.last(http.MethodPost, path).Body; !reflect.DeepEqual(b, tc.want) {
			t.Fatalf("create body (%q) = %v", tc.src, b)
		}
	}

	// Merge with branch deletion (same repo).
	if err := p.MergePullRequest(bg, existing, 1, forge.MergePullRequestRequest{Method: domain.MergeMethodSquash, CommitTitle: "t", DeleteSourceBranch: true}); err != nil {
		t.Fatal(err)
	}
	if b := f.last(http.MethodPut, path+"/1/merge").Body; !reflect.DeepEqual(b, map[string]any{"merge_method": "squash", "commit_title": "t"}) {
		t.Fatalf("merge body = %v", b)
	}
	f.last(http.MethodDelete, "/repos/octocat/repo-001/git/refs/heads/feature-1")
	// Fork PR: the branch lives elsewhere and is not deleted.
	if err := p.MergePullRequest(bg, existing, 4, forge.MergePullRequestRequest{DeleteSourceBranch: true}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.requests(http.MethodDelete, "/repos/octocat/repo-001/git/refs/heads/feature-4")); n != 0 {
		t.Fatal("deleted a fork's branch")
	}
	err = p.MergePullRequest(bg, existing, 9, forge.MergePullRequestRequest{})
	if e := assertKind(t, err, errs.ErrConflict); e.Message != "Pull Request is not mergeable" {
		t.Fatalf("405 message = %q", e.Message)
	}
	assertKind(t, p.MergePullRequest(bg, existing, 1, forge.MergePullRequestRequest{Method: "fast-forward"}), errs.ErrInvalidArgument)

	if err := p.ClosePullRequest(bg, existing, 1); err != nil {
		t.Fatal(err)
	}
	if b := f.last(http.MethodPatch, path+"/1").Body; b["state"] != "closed" {
		t.Fatalf("close body = %v", b)
	}
	if p.PullRequestHeadRef(12) != "refs/pull/12/head" {
		t.Fatal("head ref")
	}
	if !reflect.DeepEqual(p.MergeMethods(), []domain.MergeMethod{"merge", "squash", "rebase"}) {
		t.Fatal("merge methods")
	}
}

func TestIssues(t *testing.T) {
	f, p := setup(t)
	path := "/repos/octocat/repo-001/issues"
	issues, err := p.ListIssues(bg, existing, forge.IssueListOptions{Labels: []string{"bug", "ui"}, Assignee: "@octocat", Author: "hubot", State: domain.IssueAll})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].Number != 10 || issues[1].Number != 12 {
		t.Fatalf("pull requests not filtered: %+v", issues)
	}
	q := f.last(http.MethodGet, path).Query
	if q.Get("labels") != "bug,ui" || q.Get("assignee") != "octocat" || q.Get("creator") != "hubot" || q.Get("state") != "all" {
		t.Fatalf("query = %v", q)
	}
	// A page consisting only of PRs must not end pagination early.
	one, err := p.ListIssues(bg, existing, forge.IssueListOptions{ListOptions: forge.ListOptions{Limit: 2}})
	if err != nil || len(one) != 2 {
		t.Fatalf("limit with PR filtering: %+v %v", one, err)
	}

	i, err := p.GetIssue(bg, existing, 10)
	if err != nil || i.Title != "Issue 10" || i.Comments != 3 || i.Assignees[0] != "octocat" {
		t.Fatalf("get: %+v %v", i, err)
	}
	_, err = p.GetIssue(bg, existing, 11)
	if e := assertKind(t, err, errs.ErrNotFound); !strings.Contains(e.Message, "is a pull request") {
		t.Fatalf("PR as issue: %v", e)
	}

	if _, err := p.CreateIssue(bg, existing, forge.CreateIssueRequest{Title: "New", Body: "b", Labels: []string{"bug"}, Assignees: []string{"@octocat"}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"title": "New", "body": "b", "labels": []any{"bug"}, "assignees": []any{"octocat"}}
	if b := f.last(http.MethodPost, path).Body; !reflect.DeepEqual(b, want) {
		t.Fatalf("create body = %v", b)
	}

	closed, err := p.SetIssueState(bg, existing, 10, domain.IssueClosed)
	if err != nil || closed.State != domain.IssueClosed || closed.RawState != "completed" {
		t.Fatalf("close: %+v %v", closed, err)
	}
	if b := f.last(http.MethodPatch, path+"/10").Body; !reflect.DeepEqual(b, map[string]any{"state": "closed", "state_reason": "completed"}) {
		t.Fatalf("close body = %v", b)
	}
	if _, err := p.SetIssueState(bg, existing, 10, domain.IssueOpen); err != nil {
		t.Fatal(err)
	}
	if b := f.last(http.MethodPatch, path+"/10").Body; b["state_reason"] != "reopened" {
		t.Fatalf("reopen body = %v", b)
	}
	_, err = p.SetIssueState(bg, existing, 10, domain.IssueAll)
	assertKind(t, err, errs.ErrInvalidArgument)
}

func TestRunStatusMapping(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, tc := range []struct {
		status     string
		conclusion *string
		want       domain.PipelineStatus
	}{
		{"queued", nil, domain.PipelinePending}, {"waiting", nil, domain.PipelinePending},
		{"requested", nil, domain.PipelinePending}, {"pending", nil, domain.PipelinePending},
		{"in_progress", nil, domain.PipelineRunning},
		{"completed", s("success"), domain.PipelineSuccess},
		{"completed", s("failure"), domain.PipelineFailed}, {"completed", s("timed_out"), domain.PipelineFailed},
		{"completed", s("startup_failure"), domain.PipelineFailed},
		{"completed", s("cancelled"), domain.PipelineCanceled},
		{"completed", s("skipped"), domain.PipelineSkipped}, {"completed", s("neutral"), domain.PipelineSkipped},
		{"completed", s("action_required"), domain.PipelineManual}, {"action_required", nil, domain.PipelineManual},
		{"completed", s("stale"), domain.PipelineUnknown}, {"mystery", nil, domain.PipelineUnknown},
	} {
		if got, _ := runStatus(tc.status, tc.conclusion); got != tc.want {
			t.Errorf("%s/%v = %s, want %s", tc.status, str(tc.conclusion), got, tc.want)
		}
	}
}

func TestPipelines(t *testing.T) {
	f, p := setup(t)
	base := "/repos/octocat/repo-001/actions"

	runs, err := p.ListPipelines(bg, existing, forge.PipelineListOptions{Ref: "refs/heads/main"})
	if err != nil || len(runs) != 4 {
		t.Fatalf("list: %v %v", runs, err)
	}
	if f.last(http.MethodGet, base+"/runs").Query.Get("branch") != "main" {
		t.Fatal("branch filter not sent")
	}
	r0 := runs[0]
	if r0.ID != "1001" || r0.Status != domain.PipelineSuccess || r0.Name != "CI: Fix bug" || r0.Actor != "octocat" ||
		r0.Duration.Minutes() != 9 || r0.FinishedAt.IsZero() {
		t.Fatalf("run mapping: %+v", r0)
	}
	if runs[1].Status != domain.PipelineRunning || !runs[1].FinishedAt.IsZero() || runs[3].Status != domain.PipelinePending {
		t.Fatalf("statuses: %+v", runs)
	}
	failed, _ := p.ListPipelines(bg, existing, forge.PipelineListOptions{Status: domain.PipelineFailed})
	if len(failed) != 1 || failed[0].RawStatus != "timed_out" || f.last(http.MethodGet, base+"/runs").Query.Has("status") {
		t.Fatalf("failed filter must be client-side: %+v", failed)
	}
	if _, err := p.ListPipelines(bg, existing, forge.PipelineListOptions{Status: domain.PipelineRunning}); err != nil {
		t.Fatal(err)
	}
	if f.last(http.MethodGet, base+"/runs").Query.Get("status") != "in_progress" {
		t.Fatal("running must map to status=in_progress")
	}

	pl, err := p.GetPipeline(bg, existing, "1001")
	if err != nil || len(pl.Jobs) != 3 || pl.Jobs[0].Name != "build" || pl.Jobs[1].Status != domain.PipelineSkipped || pl.Jobs[0].Stage != "CI" {
		t.Fatalf("get: %+v %v", pl, err)
	}
	_, err = p.GetPipeline(bg, existing, "../../x")
	assertKind(t, err, errs.ErrInvalidArgument)
	_, err = p.GetPipeline(bg, existing, "999")
	assertKind(t, err, errs.ErrNotFound)

	// Run: workflow required; GHES-style 204 returns nil; 200 returns the run.
	_, err = p.RunPipeline(bg, existing, forge.RunPipelineRequest{Ref: "main"})
	assertKind(t, err, errs.ErrInvalidArgument)
	got, err := p.RunPipeline(bg, existing, forge.RunPipelineRequest{Workflow: ".github/workflows/ci.yml", Variables: map[string]string{"env": "prod"}})
	if err != nil || got != nil {
		t.Fatalf("204 dispatch: %v %v", got, err)
	}
	b := f.last(http.MethodPost, base+"/workflows/ci.yml/dispatches").Body
	if !reflect.DeepEqual(b, map[string]any{"ref": "main", "inputs": map[string]any{"env": "prod"}}) {
		t.Fatalf("dispatch body = %v (ref must default to the default branch)", b)
	}
	got, err = p.RunPipeline(bg, existing, forge.RunPipelineRequest{Workflow: "deploy.yml", Ref: "v1"})
	if err != nil || got == nil || got.ID != "1004" || got.Status != domain.PipelinePending {
		t.Fatalf("200 dispatch: %+v %v", got, err)
	}
	_, err = p.RunPipeline(bg, existing, forge.RunPipelineRequest{Workflow: "nope.yml", Ref: "main"})
	assertKind(t, err, errs.ErrNotFound)

	if err := p.CancelPipeline(bg, existing, "1002"); err != nil {
		t.Fatal(err)
	}
	f.last(http.MethodPost, base+"/runs/1002/cancel")
	assertKind(t, p.CancelPipeline(bg, existing, "1001"), errs.ErrConflict)
	if err := p.RetryPipeline(bg, existing, "1003"); err != nil {
		t.Fatal(err)
	}
	f.last(http.MethodPost, base+"/runs/1003/rerun")
}

func TestPipelineLogs(t *testing.T) {
	f, p := setup(t)
	var one bytes.Buffer
	if err := p.PipelineLogs(bg, existing, "1001", "5001", &one); err != nil {
		t.Fatal(err)
	}
	if one.String() != "2024-01-01T00:00:00Z log line for job 5001\n" {
		t.Fatalf("log = %q", one.String())
	}
	for _, a := range f.blobAuthHeaders() {
		if a != "" {
			t.Fatalf("Authorization header forwarded to the redirect host: %q", a)
		}
	}
	if len(f.blobAuthHeaders()) != 1 {
		t.Fatal("redirect not followed")
	}
	assertKind(t, p.PipelineLogs(bg, existing, "1001", "5002", &bytes.Buffer{}), errs.ErrNotFound)

	var all bytes.Buffer
	if err := p.PipelineLogs(bg, existing, "1001", "", &all); err != nil {
		t.Fatal(err)
	}
	want := "==> build\n2024-01-01T00:00:00Z log line for job 5001\n\n==> test\n(no logs available)\n\n" +
		"==> deploy\n(logs are available once the job completes; status: queued)\n"
	if all.String() != want {
		t.Fatalf("all logs =\n%s\nwant\n%s", all.String(), want)
	}
	for _, a := range f.blobAuthHeaders() {
		if a != "" {
			t.Fatal("Authorization header forwarded to the redirect host")
		}
	}
}

func TestCheckRedirectStripsCrossOriginAuth(t *testing.T) {
	mk := func(u string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, u, nil)
		r.Header.Set("Authorization", "Bearer x")
		return r
	}
	orig := mk("https://api.github.com/a")
	same := mk("https://api.github.com/b")
	if err := checkRedirect(same, []*http.Request{orig}); err != nil || same.Header.Get("Authorization") == "" {
		t.Fatal("same-origin redirect lost auth")
	}
	for _, u := range []string{"https://api.github.com:8443/b", "http://api.github.com/b", "https://sub.api.github.com/b", "https://blob.example/b"} {
		r := mk(u)
		if err := checkRedirect(r, []*http.Request{orig}); err != nil || r.Header.Get("Authorization") != "" {
			t.Fatalf("%s kept Authorization", u)
		}
	}
}

func TestReleases(t *testing.T) {
	f, p := setup(t)
	path := "/repos/octocat/repo-001/releases"
	rels, err := p.ListReleases(bg, existing, forge.ListOptions{})
	if err != nil || len(rels) != 3 || rels[1].Prerelease != true || rels[0].Assets[0].Size != 42 || rels[0].Author != "octocat" {
		t.Fatalf("list: %+v %v", rels, err)
	}
	r, err := p.GetRelease(bg, existing, "v1.0.0")
	if err != nil || r.ID != "1" || r.Name != "Release v1.0.0" {
		t.Fatalf("get: %+v %v", r, err)
	}
	d, err := p.GetRelease(bg, existing, "v3.0.0")
	if err != nil || !d.Draft || d.ID != "3" {
		t.Fatalf("draft lookup: %+v %v", d, err)
	}
	_, err = p.GetRelease(bg, existing, "v9")
	assertKind(t, err, errs.ErrNotFound)

	if _, err := p.CreateRelease(bg, existing, forge.CreateReleaseRequest{Tag: "v4", Name: "Four", Body: "n", Target: "main", Prerelease: true}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"tag_name": "v4", "name": "Four", "body": "n", "target_commitish": "main", "draft": false, "prerelease": true}
	if b := f.last(http.MethodPost, path).Body; !reflect.DeepEqual(b, want) {
		t.Fatalf("create body = %v", b)
	}
	if err := p.DeleteRelease(bg, existing, "v3.0.0"); err != nil {
		t.Fatal(err)
	}
	f.last(http.MethodDelete, path+"/3")
}

func TestNamespaces(t *testing.T) {
	_, p := setup(t)
	ns, err := p.ListNamespaces(bg, forge.ListOptions{})
	if err != nil || len(ns) != 3 {
		t.Fatalf("list: %+v %v", ns, err)
	}
	if ns[0].Type != domain.NamespaceUser || ns[0].FullPath != "octocat" || ns[1].Type != domain.NamespaceOrganization ||
		ns[1].Label != "GitHub Organization" || !strings.HasSuffix(ns[1].WebURL, "/octo-org") {
		t.Fatalf("mapping: %+v", ns)
	}
	if one, _ := p.ListNamespaces(bg, forge.ListOptions{Limit: 1}); len(one) != 1 {
		t.Fatal("limit 1")
	}
	if two, _ := p.ListNamespaces(bg, forge.ListOptions{Limit: 2}); len(two) != 2 {
		t.Fatal("limit 2")
	}
	org, err := p.GetNamespace(bg, "octo-org")
	if err != nil || org.Type != domain.NamespaceOrganization || org.Name != "Octo Org" {
		t.Fatalf("org: %+v %v", org, err)
	}
	u, err := p.GetNamespace(bg, "hubot")
	if err != nil || u.Type != domain.NamespaceUser || u.Name != "hubot" {
		t.Fatalf("user: %+v %v", u, err)
	}
	_, err = p.GetNamespace(bg, "nobody")
	assertKind(t, err, errs.ErrNotFound)
	_, err = p.GetNamespace(bg, "a/b")
	assertKind(t, err, errs.ErrInvalidArgument)
}

func TestSearch(t *testing.T) {
	f, p := setup(t)
	res, err := p.Search(bg, domain.SearchQuery{Kind: domain.SearchRepositories, Query: "cli", Namespace: "octo-org", Limit: 1})
	if err != nil || res.Total != 2 || len(res.Repositories) != 1 {
		t.Fatalf("repos: %+v %v", res, err)
	}
	if q := f.last(http.MethodGet, "/search/repositories").Query; q.Get("q") != "cli org:octo-org" || q.Get("per_page") != "1" {
		t.Fatalf("repo query = %v", q)
	}
	if _, err := p.Search(bg, domain.SearchQuery{Kind: domain.SearchRepositories, Query: "cli", Namespace: "hubot"}); err != nil {
		t.Fatal(err)
	}
	if q := f.last(http.MethodGet, "/search/repositories").Query.Get("q"); q != "cli user:hubot" {
		t.Fatalf("user scope = %q", q)
	}

	res, err = p.Search(bg, domain.SearchQuery{Kind: domain.SearchIssues, Query: "crash", Repository: &existing})
	if err != nil || len(res.Issues) != 2 || res.Issues[1].State != domain.IssueClosed {
		t.Fatalf("issues: %+v %v", res, err)
	}
	if q := f.last(http.MethodGet, "/search/issues").Query.Get("q"); q != "crash repo:octocat/repo-001 is:issue" {
		t.Fatalf("issue query = %q", q)
	}
	if _, err := p.Search(bg, domain.SearchQuery{Kind: domain.SearchIssues, Query: "is:pr review:required"}); err != nil {
		t.Fatal(err)
	}
	if q := f.last(http.MethodGet, "/search/issues").Query.Get("q"); q != "is:pr review:required" {
		t.Fatalf("is:pr query rewritten: %q", q)
	}

	res, err = p.Search(bg, domain.SearchQuery{Kind: domain.SearchCode, Query: "func main"})
	if err != nil || len(res.Code) != 1 || res.Code[0].Fragment != "func main()" || res.Code[0].Repository != "octocat/repo-001" {
		t.Fatalf("code: %+v %v", res, err)
	}
	if a := f.last(http.MethodGet, "/search/code").Header.Get("Accept"); a != "application/vnd.github.text-match+json" {
		t.Fatalf("code accept = %q", a)
	}
	_, err = p.Search(bg, domain.SearchQuery{Kind: domain.SearchCode, Query: "  "})
	assertKind(t, err, errs.ErrInvalidArgument)
	_, err = p.Search(bg, domain.SearchQuery{Kind: "commits", Query: "x"})
	assertKind(t, err, errs.ErrInvalidArgument)
}

func TestNotifications(t *testing.T) {
	f, p := setup(t)
	ns, err := p.ListNotifications(bg, forge.NotificationListOptions{All: true})
	if err != nil || len(ns) != 3 {
		t.Fatalf("list: %v %v", ns, err)
	}
	q := f.last(http.MethodGet, "/notifications").Query
	if q.Get("all") != "true" || q.Get("per_page") != "50" {
		t.Fatalf("query = %v", q)
	}
	web := f.webRoot() + "/octocat/repo-001"
	if ns[0].WebURL != web+"/pull/1" || ns[1].WebURL != web+"/issues/10" || ns[2].WebURL != web {
		t.Fatalf("web urls: %s %s %s", ns[0].WebURL, ns[1].WebURL, ns[2].WebURL)
	}
	if ns[0].Title != "Thread 11" || ns[0].Type != "PullRequest" || ns[0].Reason != "mention" || !ns[0].Unread || ns[0].Repository != "octocat/repo-001" {
		t.Fatalf("mapping: %+v", ns[0])
	}
	n, err := p.GetNotification(bg, "11")
	if err != nil || n.ID != "11" {
		t.Fatalf("get: %+v %v", n, err)
	}
	_, err = p.GetNotification(bg, "12")
	assertKind(t, err, errs.ErrNotFound)
	_, err = p.GetNotification(bg, "abc")
	assertKind(t, err, errs.ErrInvalidArgument)
	if err := p.MarkNotificationRead(bg, "11"); err != nil {
		t.Fatal(err)
	}
	f.last(http.MethodPatch, "/notifications/threads/11")
	if err := p.MarkAllNotificationsRead(bg); err != nil {
		t.Fatal(err)
	}
	f.last(http.MethodPut, "/notifications")
}

func TestGists(t *testing.T) {
	f, p := setup(t)
	list, err := p.ListSnippets(bg, forge.ListOptions{})
	if err != nil || len(list) != 2 || list[0].Visibility != domain.VisibilityPublic || list[1].Visibility != domain.VisibilityPrivate {
		t.Fatalf("list: %+v %v", list, err)
	}
	if list[0].Title != "a-big.go" || list[0].Files[0].Content != "" || list[0].Term != "Gist" {
		t.Fatalf("list mapping: %+v", list[0])
	}
	g, err := p.GetSnippet(bg, "aa11")
	if err != nil {
		t.Fatal(err)
	}
	if g.Files[0].Name != "a-big.go" || g.Files[0].Content != "package big // full content" || g.Files[1].Content != "small" {
		t.Fatalf("truncated file not fetched: %+v", g.Files)
	}
	if hs := f.blobAuthHeaders(); len(hs) != 1 || hs[0] != "" {
		t.Fatalf("raw fetch: %v", hs)
	}
	_, err = p.GetSnippet(bg, "../etc")
	assertKind(t, err, errs.ErrInvalidArgument)
	_, err = p.GetSnippet(bg, "ff00")
	assertKind(t, err, errs.ErrNotFound)

	s, err := p.CreateSnippet(bg, forge.CreateSnippetRequest{Title: "Notes", Description: "misc", Files: []domain.SnippetFile{{Name: "n.md", Content: "# hi"}}})
	if err != nil || s.ID != "cc33" || s.Visibility != domain.VisibilityPrivate {
		t.Fatalf("create: %+v %v", s, err)
	}
	want := map[string]any{"description": "Notes - misc", "public": false, "files": map[string]any{"n.md": map[string]any{"content": "# hi"}}}
	if b := f.last(http.MethodPost, "/gists").Body; !reflect.DeepEqual(b, want) {
		t.Fatalf("create body = %v", b)
	}
	_, err = p.CreateSnippet(bg, forge.CreateSnippetRequest{Visibility: domain.VisibilityInternal, Files: []domain.SnippetFile{{Name: "a", Content: "b"}}})
	assertKind(t, err, errs.ErrInvalidArgument)
	_, err = p.CreateSnippet(bg, forge.CreateSnippetRequest{})
	assertKind(t, err, errs.ErrInvalidArgument)
	_, err = p.CreateSnippet(bg, forge.CreateSnippetRequest{Files: []domain.SnippetFile{{Name: "a", Content: ""}}})
	assertKind(t, err, errs.ErrInvalidArgument)
}

func TestKeys(t *testing.T) {
	f, p := setup(t)
	keys, err := p.ListSSHKeys(bg)
	if err != nil || len(keys) != 1 || keys[0].Title != "laptop" || !strings.HasPrefix(keys[0].Fingerprint, "SHA256:") {
		t.Fatalf("list: %+v %v", keys, err)
	}
	k, err := p.AddSSHKey(bg, "work", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG2mBn3oIV+0B7bEYm7C3u/C8z1oqk5dNl2lVwJb4Tzu me@x\n")
	if err != nil || k.ID != "2" || k.Fingerprint != keys[0].Fingerprint {
		t.Fatalf("add: %+v %v", k, err)
	}
	if b := f.last(http.MethodPost, "/user/keys").Body; b["title"] != "work" || strings.HasSuffix(b["key"].(string), "\n") {
		t.Fatalf("add body = %v", b)
	}
	_, err = p.AddSSHKey(bg, "x", "-----BEGIN OPENSSH PRIVATE KEY-----")
	assertKind(t, err, errs.ErrInvalidArgument)
	if err := p.RemoveSSHKey(bg, "1"); err != nil {
		t.Fatal(err)
	}
	assertKind(t, p.RemoveSSHKey(bg, "5"), errs.ErrNotFound)
	assertKind(t, p.RemoveSSHKey(bg, "x/1"), errs.ErrInvalidArgument)

	gpg, err := p.ListGPGKeys(bg)
	if err != nil || len(gpg) != 1 || gpg[0].KeyID != "3262EFF25BA0D270" || gpg[0].Emails[0] != "octocat@example.com" || !gpg[0].ExpiresAt.IsZero() {
		t.Fatalf("gpg: %+v %v", gpg, err)
	}
}

func TestSettings(t *testing.T) {
	f, p := setup(t)
	all, err := p.ListSettings(bg)
	if err != nil || len(all) != 8 {
		t.Fatalf("list: %v %v", all, err)
	}
	got := map[string]string{}
	for _, s := range all {
		got[s.Key] = s.Value
		if !s.Writable || s.Description == "" {
			t.Errorf("setting %s: %+v", s.Key, s)
		}
	}
	if got["name"] != "The Octocat" || got["hireable"] != "true" || got["twitter_username"] != "" {
		t.Fatalf("values = %v", got)
	}
	s, err := p.GetSetting(bg, "Location")
	if err != nil || s.Key != "location" || s.Value != "SF" {
		t.Fatalf("get: %+v %v", s, err)
	}
	s, err = p.SetSetting(bg, "hireable", "false")
	if err != nil || s.Value != "false" {
		t.Fatalf("set: %+v %v", s, err)
	}
	if b := f.last(http.MethodPatch, "/user").Body; !reflect.DeepEqual(b, map[string]any{"hireable": false}) {
		t.Fatalf("patch body = %v", b)
	}
	if _, err := p.SetSetting(bg, "bio", "hello"); err != nil {
		t.Fatal(err)
	}
	if b := f.last(http.MethodPatch, "/user").Body; !reflect.DeepEqual(b, map[string]any{"bio": "hello"}) {
		t.Fatalf("patch body = %v", b)
	}
	_, err = p.SetSetting(bg, "hireable", "maybe")
	assertKind(t, err, errs.ErrInvalidArgument)
	_, err = p.GetSetting(bg, "password")
	if e := assertKind(t, err, errs.ErrInvalidArgument); !strings.Contains(e.Message, "name, email, blog") {
		t.Fatalf("unknown key message = %q", e.Message)
	}
}

func TestSummary(t *testing.T) {
	f, p := setup(t)
	s, err := p.Summary(bg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Repositories == nil || *s.Repositories != 260 || *s.OpenPullRequests != 7 || *s.OpenIssues != 4 || *s.Unread != 3 || s.RunningPipelines != nil {
		t.Fatalf("summary = %+v", s)
	}
	if q := f.last(http.MethodGet, "/notifications").Query; q.Get("per_page") != "1" || q.Get("all") != "false" {
		t.Fatalf("unread query = %v", q)
	}

	// Fine-grained tokens may not see total_private_repos; search failures
	// (e.g. 422 for some token types) leave counts unknown.
	f2 := newFake(t, "")
	p2 := newTestProvider(t, f2, func(a *forge.Account) { a.Credentials = tokenStore(testFineGrained) })
	f2.override = func(w http.ResponseWriter, r *http.Request, path string) bool {
		if path == "/search/issues" {
			writeMsg(w, http.StatusUnprocessableEntity, "Validation Failed")
			return true
		}
		return false
	}
	s2, err := p2.Summary(bg)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Repositories != nil || s2.OpenPullRequests != nil || s2.OpenIssues != nil || s2.Unread == nil {
		t.Fatalf("summary = %+v", s2)
	}

	// Rate limits are not swallowed.
	f2.override = func(w http.ResponseWriter, r *http.Request, path string) bool {
		if path == "/search/issues" {
			w.Header().Set("Retry-After", "600")
			writeMsg(w, http.StatusForbidden, "secondary rate limit")
			return true
		}
		return false
	}
	_, err = p2.Summary(bg)
	if !errors.Is(err, errs.ErrRateLimited) {
		t.Fatalf("rate limit swallowed: %v", err)
	}
}

func TestSubjectWebURL(t *testing.T) {
	f := newFake(t, "/api/v3")
	p := newTestProvider(t, f, nil) // web = srv.URL, api = srv.URL/api/v3
	web := f.webRoot() + "/o/r"
	for sub, want := range map[string]string{
		"/repos/o/r/pulls/5":          web + "/pull/5",
		"/repos/o/r/issues/6":         web + "/issues/6",
		"/repos/o/r/commits/abc":      web + "/commit/abc",
		"/repos/o/r/releases/123":     web + "/releases",
		"/repos/o/r/discussions/7":    web + "/discussions/7",
		"/repos/o/r/actions/runs/1":   web + "/actions",
		"/repos/o/r":                  web,
		"/repos/o/r/check-suites/9":   "https://fallback.example/o/r",
		"https://elsewhere.example/x": "https://fallback.example/o/r",
	} {
		u := sub
		if strings.HasPrefix(sub, "/") {
			u = f.apiRoot() + sub
		}
		th := apiThread{}
		th.Subject.URL = &u
		th.Repository.HTMLURL = "https://fallback.example/o/r"
		if got := p.subjectWebURL(th); got != want {
			t.Errorf("%s -> %s, want %s", sub, got, want)
		}
	}
}
