package gitlab

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

var ctx = context.Background()

var infraRef = domain.RepositoryRef{Namespace: "acme/platform/infra", Name: "proj-003"}

const infraPath = "/api/v4/projects/acme%2Fplatform%2Finfra%2Fproj-003"

func TestRepositoryMutations(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)

	f.json("GET /api/v4/namespaces/acme%2Fplatform", 200, map[string]any{"id": 77, "kind": "group", "full_path": "acme/platform"})
	f.json("POST /api/v4/projects", 201, f.project(500, "acme/platform", "new-svc"))
	r, err := p.CreateRepository(ctx, forge.CreateRepositoryRequest{Name: "new-svc", Namespace: "acme/platform",
		Description: "d", Visibility: domain.VisibilityInternal, DefaultBranch: "trunk", AutoInit: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.FullName != "acme/platform/new-svc" {
		t.Fatalf("created %+v", r)
	}
	body := f.last(t, "POST", "/api/v4/projects").json(t)
	want := map[string]any{"name": "new-svc", "path": "new-svc", "namespace_id": float64(77), "description": "d",
		"visibility": "internal", "default_branch": "trunk", "initialize_with_readme": true}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("create body = %v", body)
	}

	f.json("DELETE "+infraPath, 202, map[string]any{"message": "202 Accepted"})
	if err := p.DeleteRepository(ctx, infraRef); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteRepository(ctx, domain.RepositoryRef{Namespace: "acme", Name: "gone"}); !errors.Is(err, errs.ErrRepositoryNotFound) {
		t.Fatalf("delete missing: %v", err)
	}

	f.json("POST "+infraPath+"/fork", 201, f.project(501, "alice", "infra-fork"))
	r, err = p.ForkRepository(ctx, infraRef, forge.ForkRequest{Namespace: "alice", Name: "infra-fork"})
	if err != nil || r.FullName != "alice/infra-fork" {
		t.Fatalf("fork: %+v %v", r, err)
	}
	if b := f.last(t, "POST", infraPath+"/fork").json(t); b["namespace_path"] != "alice" || b["name"] != "infra-fork" || b["path"] != "infra-fork" {
		t.Fatalf("fork body = %v", b)
	}

	f.json("PUT "+infraPath, 200, f.project(3, "acme/platform/infra", "renamed"))
	if r, err = p.RenameRepository(ctx, infraRef, "renamed"); err != nil || r.Name != "renamed" {
		t.Fatalf("rename: %+v %v", r, err)
	}
	if b := f.last(t, "PUT", infraPath).json(t); b["name"] != "renamed" || b["path"] != "renamed" {
		t.Fatalf("rename body = %v", b)
	}

	arch := f.project(3, "acme/platform/infra", "proj-003")
	arch["archived"] = true
	f.json("POST "+infraPath+"/archive", 201, arch)
	f.json("POST "+infraPath+"/unarchive", 201, f.project(3, "acme/platform/infra", "proj-003"))
	if r, err = p.SetRepositoryArchived(ctx, infraRef, true); err != nil || !r.Archived {
		t.Fatalf("archive: %+v %v", r, err)
	}
	if r, err = p.SetRepositoryArchived(ctx, infraRef, false); err != nil || r.Archived {
		t.Fatalf("unarchive: %+v %v", r, err)
	}
}

func mrJSON(iid int, state string, extra map[string]any) map[string]any {
	m := map[string]any{
		"id": 1000 + iid, "iid": iid, "title": "MR " + state, "description": "body", "state": state,
		"draft": false, "work_in_progress": true, "source_branch": "feat", "target_branch": "main",
		"source_project_id": 3, "target_project_id": 3, "sha": "abc123", "detailed_merge_status": "mergeable",
		"labels": []string{"bug"}, "web_url": "https://gl/mr", "author": map[string]any{"username": "bob"},
		"reviewers":  []any{map[string]any{"username": "carol"}},
		"created_at": "2025-01-01T00:00:00Z", "merged_at": nil,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestMergeRequests(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	f.json("GET "+infraPath+"/merge_requests", 200, []any{
		mrJSON(5, "opened", nil),
		mrJSON(6, "locked", map[string]any{"source_project_id": 4, "detailed_merge_status": "checking"}),
	})
	prs, err := p.ListPullRequests(ctx, infraRef, forge.PullRequestListOptions{Author: "bob", TargetBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last(t, "GET", infraPath+"/merge_requests").query()
	if q.Get("state") != "opened" || q.Get("author_username") != "bob" || q.Get("target_branch") != "main" {
		t.Fatalf("query = %v", q)
	}
	if len(prs) != 2 {
		t.Fatalf("got %d", len(prs))
	}
	a, b := prs[0], prs[1]
	if a.Number != 5 || a.ID != "1005" || a.State != domain.PullRequestOpen || !a.Draft || a.Author != "bob" ||
		a.Mergeable == nil || !*a.Mergeable || a.Reviewers[0] != "carol" || a.Term != "Merge Request" || a.SourceRepo != "" {
		t.Fatalf("mapped MR = %+v", a)
	}
	if b.State != domain.PullRequestOpen || b.Mergeable != nil || b.SourceRepo != "alice/proj-004" {
		t.Fatalf("locked fork MR = %+v", b)
	}

	for state, want := range map[domain.PullRequestState]string{domain.PullRequestMerged: "merged", domain.PullRequestClosed: "closed", domain.PullRequestAll: ""} {
		if _, err := p.ListPullRequests(ctx, infraRef, forge.PullRequestListOptions{State: state}); err != nil {
			t.Fatal(err)
		}
		if got := f.last(t, "GET", infraPath+"/merge_requests").query().Get("state"); got != want {
			t.Fatalf("state %s sent as %q", state, got)
		}
	}

	f.json("GET "+infraPath+"/merge_requests/5", 200, mrJSON(5, "merged", map[string]any{"merged_at": "2025-02-02T00:00:00Z"}))
	pr, err := p.GetPullRequest(ctx, infraRef, 5)
	if err != nil || pr.State != domain.PullRequestMerged || pr.MergedAt.IsZero() {
		t.Fatalf("get MR: %+v %v", pr, err)
	}

	f.json("POST "+infraPath+"/merge_requests", 201, mrJSON(7, "opened", nil))
	if _, err := p.CreatePullRequest(ctx, infraRef, forge.CreatePullRequestRequest{Title: "Add x", Body: "why", SourceBranch: "feat", TargetBranch: "main", Draft: true}); err != nil {
		t.Fatal(err)
	}
	body := f.last(t, "POST", infraPath+"/merge_requests").json(t)
	if body["title"] != "Draft: Add x" || body["source_branch"] != "feat" || body["target_branch"] != "main" || body["description"] != "why" || body["target_project_id"] != nil {
		t.Fatalf("create body = %v", body)
	}

	// From a fork: created on the fork with target_project_id = upstream ID;
	// the target branch defaults to the upstream default branch.
	f.json("POST /api/v4/projects/alice%2Fproj-004/merge_requests", 201, mrJSON(8, "opened", map[string]any{"source_project_id": 4}))
	pr, err = p.CreatePullRequest(ctx, infraRef, forge.CreatePullRequestRequest{Title: "Fix", SourceBranch: "fix", SourceRepo: "alice/proj-004"})
	if err != nil {
		t.Fatal(err)
	}
	body = f.last(t, "POST", "/api/v4/projects/alice%2Fproj-004/merge_requests").json(t)
	if body["target_project_id"] != float64(3) || body["target_branch"] != "main" || body["title"] != "Fix" {
		t.Fatalf("fork create body = %v", body)
	}
	if pr.SourceRepo != "alice/proj-004" {
		t.Fatalf("fork MR source = %q", pr.SourceRepo)
	}

	f.json("PUT "+infraPath+"/merge_requests/5/merge", 200, mrJSON(5, "merged", nil))
	if err := p.MergePullRequest(ctx, infraRef, 5, forge.MergePullRequestRequest{Method: domain.MergeMethodSquash, CommitTitle: "T", CommitMessage: "M", DeleteSourceBranch: true}); err != nil {
		t.Fatal(err)
	}
	body = f.last(t, "PUT", infraPath+"/merge_requests/5/merge").json(t)
	if !reflect.DeepEqual(body, map[string]any{"squash": true, "squash_commit_message": "T\n\nM", "should_remove_source_branch": true}) {
		t.Fatalf("merge body = %v", body)
	}
	if err := p.MergePullRequest(ctx, infraRef, 5, forge.MergePullRequestRequest{CommitTitle: "Merge it"}); err != nil {
		t.Fatal(err)
	}
	body = f.last(t, "PUT", infraPath+"/merge_requests/5/merge").json(t)
	if !reflect.DeepEqual(body, map[string]any{"squash": false, "merge_commit_message": "Merge it"}) {
		t.Fatalf("merge body = %v", body)
	}
	n := len(f.recorded("PUT", infraPath+"/merge_requests/5/merge"))
	if err := p.MergePullRequest(ctx, infraRef, 5, forge.MergePullRequestRequest{Method: domain.MergeMethodRebase}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("rebase: %v", err)
	}
	if len(f.recorded("PUT", infraPath+"/merge_requests/5/merge")) != n {
		t.Fatal("rebase request reached the server")
	}
	f.json("PUT "+infraPath+"/merge_requests/9/merge", 405, map[string]any{"message": "405 Method Not Allowed"})
	if err := p.MergePullRequest(ctx, infraRef, 9, forge.MergePullRequestRequest{}); !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "cannot be merged") {
		t.Fatalf("not mergeable: %v", err)
	}
	if got := p.MergeMethods(); !reflect.DeepEqual(got, []domain.MergeMethod{"merge", "squash"}) {
		t.Fatalf("MergeMethods = %v", got)
	}

	f.json("PUT "+infraPath+"/merge_requests/5", 200, mrJSON(5, "closed", nil))
	if err := p.ClosePullRequest(ctx, infraRef, 5); err != nil {
		t.Fatal(err)
	}
	if b := f.last(t, "PUT", infraPath+"/merge_requests/5").json(t); b["state_event"] != "close" {
		t.Fatalf("close body = %v", b)
	}
	if p.PullRequestHeadRef(12) != "refs/merge-requests/12/head" {
		t.Fatal("head ref")
	}
}

func issueJSON(iid int, state string) map[string]any {
	return map[string]any{"id": 2000 + iid, "iid": iid, "project_id": 3, "title": "Issue", "description": "d", "state": state,
		"labels": []string{"a", "b"}, "author": map[string]any{"username": "bob"},
		"assignees": []any{map[string]any{"username": "carol"}}, "user_notes_count": 4, "web_url": "https://gl/i",
		"created_at": "2025-01-01T00:00:00Z", "closed_at": nil}
}

func TestIssues(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	f.json("GET "+infraPath+"/issues", 200, []any{issueJSON(1, "opened"), issueJSON(2, "closed")})
	issues, err := p.ListIssues(ctx, infraRef, forge.IssueListOptions{State: domain.IssueAll, Labels: []string{"a", "b"}, Assignee: "carol", Author: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last(t, "GET", infraPath+"/issues").query()
	if q.Has("state") || q.Get("labels") != "a,b" || q.Get("assignee_username") != "carol" || q.Get("author_username") != "bob" {
		t.Fatalf("query = %v", q)
	}
	if len(issues) != 2 || issues[0].Number != 1 || issues[0].State != domain.IssueOpen || issues[1].State != domain.IssueClosed ||
		issues[0].Comments != 4 || issues[0].Assignees[0] != "carol" || !reflect.DeepEqual(issues[0].Labels, []string{"a", "b"}) {
		t.Fatalf("issues = %+v", issues)
	}
	if _, err := p.ListIssues(ctx, infraRef, forge.IssueListOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.last(t, "GET", infraPath+"/issues").query().Get("state") != "opened" {
		t.Fatal("default state should be opened")
	}

	f.json("GET "+infraPath+"/issues/1", 200, issueJSON(1, "opened"))
	if i, err := p.GetIssue(ctx, infraRef, 1); err != nil || i.Number != 1 {
		t.Fatalf("get issue: %+v %v", i, err)
	}

	f.handle("GET /api/v4/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("username") == "carol" {
			writeJSON(w, 200, []any{map[string]any{"id": 31, "username": "carol"}})
			return
		}
		writeJSON(w, 200, []any{})
	})
	f.json("POST "+infraPath+"/issues", 201, issueJSON(3, "opened"))
	if _, err := p.CreateIssue(ctx, infraRef, forge.CreateIssueRequest{Title: "T", Body: "B", Labels: []string{"x", "y"}, Assignees: []string{"@carol"}}); err != nil {
		t.Fatal(err)
	}
	body := f.last(t, "POST", infraPath+"/issues").json(t)
	if !reflect.DeepEqual(body, map[string]any{"title": "T", "description": "B", "labels": "x,y", "assignee_ids": []any{float64(31)}}) {
		t.Fatalf("create body = %v", body)
	}
	if _, err := p.CreateIssue(ctx, infraRef, forge.CreateIssueRequest{Title: "T", Assignees: []string{"nobody"}}); !errors.Is(err, errs.ErrInvalidArgument) || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("unknown assignee: %v", err)
	}

	f.json("PUT "+infraPath+"/issues/1", 200, issueJSON(1, "opened"))
	if i, err := p.SetIssueState(ctx, infraRef, 1, domain.IssueOpen); err != nil || i.State != domain.IssueOpen {
		t.Fatalf("reopen: %+v %v", i, err)
	}
	if b := f.last(t, "PUT", infraPath+"/issues/1").json(t); b["state_event"] != "reopen" {
		t.Fatalf("state body = %v", b)
	}
	if _, err := p.SetIssueState(ctx, infraRef, 1, domain.IssueClosed); err != nil {
		t.Fatal(err)
	}
	if b := f.last(t, "PUT", infraPath+"/issues/1").json(t); b["state_event"] != "close" {
		t.Fatalf("state body = %v", b)
	}
}

func TestPipelines(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	pl := func(id int, status string) map[string]any {
		return map[string]any{"id": id, "iid": id - 100, "status": status, "ref": "main", "sha": "abc", "source": "push",
			"web_url": "https://gl/p", "created_at": "2025-01-01T00:00:00Z", "duration": 61.5, "user": map[string]any{"username": "bob"}}
	}
	f.json("GET "+infraPath+"/pipelines", 200, []any{pl(110, "waiting_for_resource"), pl(109, "canceling"), pl(108, "manual"), pl(107, "weird")})
	pls, err := p.ListPipelines(ctx, infraRef, forge.PipelineListOptions{Ref: "main", Status: domain.PipelineFailed})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last(t, "GET", infraPath+"/pipelines").query()
	if q.Get("ref") != "main" || q.Get("status") != "failed" || q.Get("order_by") != "id" || q.Get("sort") != "desc" {
		t.Fatalf("query = %v", q)
	}
	wantStatus := []domain.PipelineStatus{domain.PipelinePending, domain.PipelineCanceled, domain.PipelineManual, domain.PipelineUnknown}
	for i, s := range wantStatus {
		if pls[i].Status != s {
			t.Fatalf("pipeline %d status %q, want %q", i, pls[i].Status, s)
		}
	}
	if pls[0].ID != "110" || pls[0].Number != 10 || pls[0].Actor != "bob" || pls[0].Event != "push" || pls[0].Duration.Seconds() != 61.5 || pls[0].RawStatus != "waiting_for_resource" {
		t.Fatalf("pipeline = %+v", pls[0])
	}

	f.json("GET "+infraPath+"/pipelines/110", 200, pl(110, "running"))
	f.json("GET "+infraPath+"/pipelines/110/jobs", 200, []any{
		map[string]any{"id": 502, "name": "test", "stage": "test", "status": "failed"},
		map[string]any{"id": 501, "name": "build", "stage": "build", "status": "success"},
		map[string]any{"id": 503, "name": "deploy", "stage": "deploy", "status": "manual"},
	})
	got, err := p.GetPipeline(ctx, infraRef, "110")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.PipelineRunning || len(got.Jobs) != 3 || got.Jobs[0].Name != "build" || got.Jobs[0].Stage != "build" || got.Jobs[1].Status != domain.PipelineFailed {
		t.Fatalf("pipeline = %+v", got)
	}
	if _, err := p.GetPipeline(ctx, infraRef, "../x"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("non-numeric id: %v", err)
	}

	f.json("POST "+infraPath+"/pipeline", 201, pl(111, "created"))
	run, err := p.RunPipeline(ctx, infraRef, forge.RunPipelineRequest{Ref: "release", Variables: map[string]string{"B": "2", "A": "1"}})
	if err != nil || run.ID != "111" || run.Status != domain.PipelinePending {
		t.Fatalf("run: %+v %v", run, err)
	}
	body := f.last(t, "POST", infraPath+"/pipeline").json(t)
	want := map[string]any{"ref": "release", "variables": []any{map[string]any{"key": "A", "value": "1"}, map[string]any{"key": "B", "value": "2"}}}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("run body = %v", body)
	}
	if _, err := p.RunPipeline(ctx, infraRef, forge.RunPipelineRequest{}); err != nil {
		t.Fatal(err)
	}
	if b := f.last(t, "POST", infraPath+"/pipeline").json(t); b["ref"] != "main" {
		t.Fatalf("default ref body = %v", b)
	}

	f.json("POST "+infraPath+"/pipelines/110/cancel", 200, pl(110, "canceled"))
	f.json("POST "+infraPath+"/pipelines/110/retry", 201, pl(110, "pending"))
	if err := p.CancelPipeline(ctx, infraRef, "110"); err != nil {
		t.Fatal(err)
	}
	if err := p.RetryPipeline(ctx, infraRef, "110"); err != nil {
		t.Fatal(err)
	}
	f.last(t, "POST", infraPath+"/pipelines/110/cancel")
	f.last(t, "POST", infraPath+"/pipelines/110/retry")

	trace := func(text string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(text))
		}
	}
	f.handle("GET "+infraPath+"/jobs/501/trace", trace("building\ndone"))
	f.handle("GET "+infraPath+"/jobs/502/trace", trace("FAIL\n"))
	f.json("GET "+infraPath+"/jobs/503/trace", 404, map[string]any{"message": "404 Not Found"})
	var buf bytes.Buffer
	if err := p.PipelineLogs(ctx, infraRef, "110", "502", &buf); err != nil || buf.String() != "FAIL\n" {
		t.Fatalf("single job log %q %v", buf.String(), err)
	}
	buf.Reset()
	if err := p.PipelineLogs(ctx, infraRef, "110", "", &buf); err != nil {
		t.Fatal(err)
	}
	wantLog := "==> build/build\nbuilding\ndone\n\n==> test/test\nFAIL\n\n==> deploy/deploy\n(no log available)\n"
	if buf.String() != wantLog {
		t.Fatalf("all logs:\n%q\nwant\n%q", buf.String(), wantLog)
	}
}

func TestReleases(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	rel := map[string]any{"tag_name": "v1/rc", "name": "RC", "description": "notes", "created_at": "2025-01-01T00:00:00Z",
		"released_at": "2025-01-02T00:00:00Z", "author": map[string]any{"username": "bob"}, "upcoming_release": false,
		"assets": map[string]any{"links": []any{map[string]any{"name": "bin", "url": "https://x/bin", "direct_asset_url": "https://gl/-/releases/v1/downloads/bin"}}},
		"_links": map[string]any{"self": "https://gl/releases/v1"}}
	f.json("GET "+infraPath+"/releases", 200, []any{rel})
	rels, err := p.ListReleases(ctx, infraRef, forge.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := rels[0]
	if r.Tag != "v1/rc" || r.ID != "v1/rc" || r.Author != "bob" || r.PublishedAt.IsZero() || r.WebURL != "https://gl/releases/v1" ||
		len(r.Assets) != 1 || r.Assets[0].URL != "https://gl/-/releases/v1/downloads/bin" || r.Draft || r.Prerelease {
		t.Fatalf("release = %+v", r)
	}
	f.json("GET "+infraPath+"/releases/v1%2Frc", 200, rel)
	if _, err := p.GetRelease(ctx, infraRef, "v1/rc"); err != nil {
		t.Fatal(err)
	}
	f.json("POST "+infraPath+"/releases", 201, rel)
	if _, err := p.CreateRelease(ctx, infraRef, forge.CreateReleaseRequest{Tag: "v2", Name: "Two", Body: "b", Target: "main"}); err != nil {
		t.Fatal(err)
	}
	if b := f.last(t, "POST", infraPath+"/releases").json(t); !reflect.DeepEqual(b, map[string]any{"tag_name": "v2", "name": "Two", "description": "b", "ref": "main"}) {
		t.Fatalf("create body = %v", b)
	}
	if _, err := p.CreateRelease(ctx, infraRef, forge.CreateReleaseRequest{Tag: "v3", Draft: true}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("draft: %v", err)
	}
	if _, err := p.CreateRelease(ctx, infraRef, forge.CreateReleaseRequest{Tag: "v3", Prerelease: true}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("prerelease: %v", err)
	}
	f.json("DELETE "+infraPath+"/releases/v1%2Frc", 200, rel)
	if err := p.DeleteRelease(ctx, infraRef, "v1/rc"); err != nil {
		t.Fatal(err)
	}
}

func TestNamespaces(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	f.json("GET /api/v4/namespaces", 200, []any{
		map[string]any{"id": 1, "name": "alice", "path": "alice", "kind": "user", "full_path": "alice", "parent_id": nil},
		map[string]any{"id": 2, "name": "ACME", "path": "acme", "kind": "group", "full_path": "acme", "parent_id": nil, "web_url": "https://gl/groups/acme"},
		map[string]any{"id": 3, "name": "Infra", "path": "infra", "kind": "group", "full_path": "acme/platform/infra", "parent_id": 4},
	})
	nss, err := p.ListNamespaces(ctx, forge.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.Namespace{
		{ID: "1", Name: "alice", FullPath: "alice", Type: domain.NamespaceUser, Label: "GitLab User Namespace"},
		{ID: "2", Name: "ACME", FullPath: "acme", Type: domain.NamespaceGroup, Label: "GitLab Group", WebURL: "https://gl/groups/acme"},
		{ID: "3", Name: "Infra", FullPath: "acme/platform/infra", Type: domain.NamespaceSubgroup, Label: "GitLab Subgroup", ParentPath: "acme/platform"},
	}
	if !reflect.DeepEqual(nss, want) {
		t.Fatalf("namespaces = %+v", nss)
	}
	f.json("GET /api/v4/namespaces/acme%2Fplatform%2Finfra", 200, map[string]any{"id": 3, "name": "Infra", "kind": "group", "full_path": "acme/platform/infra", "parent_id": 4})
	ns, err := p.GetNamespace(ctx, "acme/platform/infra")
	if err != nil || ns.Type != domain.NamespaceSubgroup {
		t.Fatalf("get namespace: %+v %v", ns, err)
	}
	if _, err := p.GetNamespace(ctx, "missing"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing namespace: %v", err)
	}
}

func TestSearch(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)

	res, err := p.Search(ctx, domain.SearchQuery{Kind: domain.SearchRepositories, Query: "proj", Namespace: "acme/platform", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Repositories) != 5 || res.Total != 130 {
		t.Fatalf("repo search: %d results, total %d", len(res.Repositories), res.Total)
	}
	if q := f.last(t, "GET", "/api/v4/groups/acme%2Fplatform/projects").query(); q.Get("search") != "proj" || q.Get("include_subgroups") != "true" || q.Get("per_page") != "5" {
		t.Fatalf("repo search query = %v", q)
	}

	f.handle("GET "+infraPath+"/search", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("scope") {
		case "issues":
			writeJSON(w, 200, []any{issueJSON(4, "opened")})
		case "blobs":
			writeJSON(w, 200, []any{map[string]any{"basename": "main", "data": "func main()", "path": "cmd/main.go", "filename": "cmd/main.go", "ref": "main", "startline": 12, "project_id": 3}})
		}
	})
	res, err = p.Search(ctx, domain.SearchQuery{Kind: domain.SearchIssues, Query: "crash", Repository: &infraRef})
	if err != nil || len(res.Issues) != 1 || res.Issues[0].Number != 4 {
		t.Fatalf("issue search: %+v %v", res, err)
	}
	res, err = p.Search(ctx, domain.SearchQuery{Kind: domain.SearchCode, Query: "main", Repository: &infraRef})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Code[0]
	if c.Repository != "acme/platform/infra/proj-003" || c.Path != "cmd/main.go" || c.Fragment != "func main()" ||
		c.WebURL != f.srv.URL+"/acme/platform/infra/proj-003/-/blob/main/cmd/main.go#L12" {
		t.Fatalf("code result = %+v", c)
	}

	// Global blobs search resolves project names by ID (cached).
	f.json("GET /api/v4/search", 200, []any{
		map[string]any{"path": "a.go", "ref": "main", "project_id": 1, "data": "x"},
		map[string]any{"path": "b.go", "ref": "main", "project_id": 1, "data": "y"},
		map[string]any{"path": "c.go", "ref": "main", "project_id": 99999, "data": "z"},
	})
	res, err = p.Search(ctx, domain.SearchQuery{Kind: domain.SearchCode, Query: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Code[0].Repository != "acme/proj-001" || res.Code[1].Repository != "acme/proj-001" || res.Code[2].Repository != "99999" {
		t.Fatalf("code repos = %+v", res.Code)
	}
	if n := len(f.recorded("GET", "/api/v4/projects/1")); n != 1 {
		t.Fatalf("project 1 looked up %d times", n)
	}
	if q := f.last(t, "GET", "/api/v4/search").query(); q.Get("scope") != "blobs" || q.Get("search") != "x" || q.Get("per_page") != "100" {
		t.Fatalf("global search query = %v", q)
	}

	// Without Advanced Search, group/global code search is unsupported.
	for name, body := range map[string]map[string]any{
		"ee":     {"message": "Scope supported only with advanced search or exact code search"},
		"old ee": {"error": "Scope not supported without Elasticsearch!"},
		"ce":     {"error": "scope does not have a valid value"},
	} {
		f.json("GET /api/v4/groups/acme/search", 400, body)
		_, err := p.Search(ctx, domain.SearchQuery{Kind: domain.SearchCode, Query: "x", Namespace: "acme"})
		if !errors.Is(err, errs.ErrUnsupportedCapability) || !strings.Contains(errs.HintOf(err), "project") {
			t.Fatalf("%s: want ErrUnsupportedCapability, got %v", name, err)
		}
	}
	// Other 400s stay invalid-argument errors.
	f.json("GET /api/v4/groups/acme/search", 400, map[string]any{"message": "search is too short"})
	if _, err := p.Search(ctx, domain.SearchQuery{Kind: domain.SearchCode, Query: "x", Namespace: "acme"}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("plain 400: %v", err)
	}
	if _, err := p.Search(ctx, domain.SearchQuery{Kind: domain.SearchRepositories, Query: "x", Repository: &infraRef}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("repo search in project: %v", err)
	}
}

func todoJSON(id int, state string) map[string]any {
	return map[string]any{"id": id, "action_name": "review_requested", "target_type": "MergeRequest", "target_url": "https://gl/mr/1",
		"body": "fallback", "state": state, "target": map[string]any{"title": "Fix it"},
		"project": map[string]any{"path_with_namespace": "acme/proj"}, "updated_at": "2025-03-03T00:00:00Z"}
}

func TestTodos(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	f.handle("GET /api/v4/todos", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("state") == "pending" && q.Get("page") == "":
			w.Header().Set("X-Next-Page", "2")
			writeJSON(w, 200, []any{todoJSON(1, "pending")})
		case q.Get("state") == "pending":
			writeJSON(w, 200, []any{todoJSON(2, "pending")})
		case q.Get("state") == "done":
			writeJSON(w, 200, []any{todoJSON(3, "done")})
		default:
			t.Errorf("unexpected todos query %v", q)
		}
	})
	ns, err := p.ListNotifications(ctx, forge.NotificationListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 2 || !ns[0].Unread || ns[0].Title != "Fix it" || ns[0].Reason != "review_requested" || ns[0].Type != "MergeRequest" ||
		ns[0].Repository != "acme/proj" || ns[0].WebURL != "https://gl/mr/1" || ns[0].UpdatedAt.IsZero() {
		t.Fatalf("todos = %+v", ns)
	}
	all, err := p.ListNotifications(ctx, forge.NotificationListOptions{All: true})
	if err != nil || len(all) != 3 || all[2].Unread {
		t.Fatalf("all todos = %+v %v", all, err)
	}
	limited, err := p.ListNotifications(ctx, forge.NotificationListOptions{All: true, ListOptions: forge.ListOptions{Limit: 1}})
	if err != nil || len(limited) != 1 {
		t.Fatalf("limited = %+v %v", limited, err)
	}

	n, err := p.GetNotification(ctx, "3")
	if err != nil || n.ID != "3" || n.Unread {
		t.Fatalf("get todo: %+v %v", n, err)
	}
	if _, err := p.GetNotification(ctx, "404"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing todo: %v", err)
	}

	f.json("POST /api/v4/todos/1/mark_as_done", 201, todoJSON(1, "done"))
	if err := p.MarkNotificationRead(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	f.handle("POST /api/v4/todos/mark_as_done", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	if err := p.MarkAllNotificationsRead(ctx); err != nil {
		t.Fatal(err)
	}
	f.last(t, "POST", "/api/v4/todos/mark_as_done")
}

func TestSnippets(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	snip := map[string]any{"id": 9, "title": "Tools", "description": "d", "visibility": "internal", "web_url": "https://gl/-/snippets/9",
		"author": map[string]any{"username": "alice"}, "files": []any{
			map[string]any{"path": "a.sh", "raw_url": f.srv.URL + "/-/snippets/9/raw/trunk/a.sh"},
			map[string]any{"path": "dir/b.txt", "raw_url": f.srv.URL + "/-/snippets/9/raw/trunk/dir/b.txt"},
		}}
	f.json("GET /api/v4/snippets", 200, []any{snip})
	list, err := p.ListSnippets(ctx, forge.ListOptions{})
	if err != nil || len(list) != 1 || list[0].Visibility != domain.VisibilityInternal || len(list[0].Files) != 2 || list[0].Owner != "alice" || list[0].Term != "Snippet" {
		t.Fatalf("snippets = %+v %v", list, err)
	}
	f.json("GET /api/v4/snippets/9", 200, snip)
	raw := func(s string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(s)) }
	}
	f.handle("GET /api/v4/snippets/9/files/trunk/a.sh/raw", raw("echo a"))
	f.handle("GET /api/v4/snippets/9/files/trunk/dir%2Fb.txt/raw", raw("bee"))
	s, err := p.GetSnippet(ctx, "9")
	if err != nil {
		t.Fatal(err)
	}
	if s.Files[0].Content != "echo a" || s.Files[1].Content != "bee" {
		t.Fatalf("snippet files = %+v", s.Files)
	}

	// Single-file snippet whose per-file endpoint is unavailable.
	one := map[string]any{"id": 10, "title": "One", "visibility": "public", "files": []any{map[string]any{"path": "x.txt", "raw_url": "https://elsewhere/raw"}}}
	f.json("GET /api/v4/snippets/10", 200, one)
	f.handle("GET /api/v4/snippets/10/raw", raw("whole"))
	s, err = p.GetSnippet(ctx, "10")
	if err != nil || s.Files[0].Content != "whole" {
		t.Fatalf("fallback snippet = %+v %v", s, err)
	}
	f.last(t, "GET", "/api/v4/snippets/10/files/HEAD/x.txt/raw")

	f.json("POST /api/v4/snippets", 201, snip)
	if _, err := p.CreateSnippet(ctx, forge.CreateSnippetRequest{Files: []domain.SnippetFile{{Name: "a.sh", Content: "echo"}}}); err != nil {
		t.Fatal(err)
	}
	body := f.last(t, "POST", "/api/v4/snippets").json(t)
	want := map[string]any{"title": "a.sh", "visibility": "private", "files": []any{map[string]any{"file_path": "a.sh", "content": "echo"}}}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("create body = %v", body)
	}
}

const testSSHKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFVbo8EbNnOZx/a4nJRPJk3pfMYyxz/Y5inh5UOKTp0t test@example"

func TestSSHKeys(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	f.json("GET /api/v4/user/keys", 200, []any{map[string]any{"id": 5, "title": "laptop", "key": testSSHKey, "created_at": "2025-01-01T00:00:00Z"}})
	keys, err := p.ListSSHKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Expected value from `ssh-keygen -lf`.
	if keys[0].Fingerprint != "SHA256:B12HWKoUtc4TYY+sM8Xz19Yo9nzqjURg5MzA3R22Kqk" || keys[0].ID != "5" {
		t.Fatalf("key = %+v", keys[0])
	}
	f.json("POST /api/v4/user/keys", 201, map[string]any{"id": 6, "title": "test@example", "key": testSSHKey})
	if _, err := p.AddSSHKey(ctx, "", testSSHKey); err != nil {
		t.Fatal(err)
	}
	if b := f.last(t, "POST", "/api/v4/user/keys").json(t); b["title"] != "test@example" || b["key"] != testSSHKey {
		t.Fatalf("add body = %v", b)
	}
	if _, err := p.AddSSHKey(ctx, "x", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("private key: %v", err)
	}
	f.handle("DELETE /api/v4/user/keys/6", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	if err := p.RemoveSSHKey(ctx, "6"); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveSSHKey(ctx, "404"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
}

func TestSettings(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	prefs := map[string]any{"id": 1, "user_id": 42, "view_diffs_file_by_file": false, "show_whitespace_in_diffs": true, "pass_user_identities_to_ci_jwt": false}
	f.json("GET /api/v4/user/preferences", 200, prefs)
	f.json("GET /api/v4/user/status", 200, map[string]any{"message": "On leave", "emoji": "palm_tree", "availability": "busy", "message_html": "x"})
	settings, err := p.ListSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range settings {
		got[s.Key] = s.Value
		if !s.Writable || s.Description == "" {
			t.Fatalf("setting %+v", s)
		}
	}
	want := map[string]string{"view_diffs_file_by_file": "false", "show_whitespace_in_diffs": "true", "pass_user_identities_to_ci_jwt": "false",
		"status.message": "On leave", "status.emoji": "palm_tree", "status.availability": "busy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %v", got)
	}
	if s, err := p.GetSetting(ctx, "status.emoji"); err != nil || s.Value != "palm_tree" {
		t.Fatalf("get setting: %+v %v", s, err)
	}
	if _, err := p.GetSetting(ctx, "name"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("unknown setting: %v", err)
	}

	updated := map[string]any{"view_diffs_file_by_file": true, "show_whitespace_in_diffs": true, "pass_user_identities_to_ci_jwt": false}
	f.json("PUT /api/v4/user/preferences", 200, updated)
	s, err := p.SetSetting(ctx, "view_diffs_file_by_file", "true")
	if err != nil || s.Value != "true" {
		t.Fatalf("set pref: %+v %v", s, err)
	}
	if b := f.last(t, "PUT", "/api/v4/user/preferences").json(t); !reflect.DeepEqual(b, updated) {
		t.Fatalf("preferences body = %v", b)
	}
	if _, err := p.SetSetting(ctx, "view_diffs_file_by_file", "maybe"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("bad bool: %v", err)
	}

	f.json("PUT /api/v4/user/status", 200, map[string]any{"message": "Back", "emoji": "palm_tree", "availability": "busy"})
	s, err = p.SetSetting(ctx, "status.message", "Back")
	if err != nil || s.Value != "Back" {
		t.Fatalf("set status: %+v %v", s, err)
	}
	if b := f.last(t, "PUT", "/api/v4/user/status").json(t); !reflect.DeepEqual(b, map[string]any{"message": "Back", "emoji": "palm_tree", "availability": "busy"}) {
		t.Fatalf("status body = %v", b)
	}
	if _, err := p.SetSetting(ctx, "status.availability", "away"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("bad availability: %v", err)
	}
	if _, err := p.SetSetting(ctx, "email", "x@y"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("profile field: %v", err)
	}
}

func TestSummary(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	withTotal := func(total string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("per_page") != "1" {
				t.Errorf("summary request without per_page=1: %s", r.RequestURI)
			}
			if total != "" {
				w.Header().Set("X-Total", total)
			}
			writeJSON(w, 200, []any{})
		}
	}
	f.handle("GET /api/v4/merge_requests", withTotal("3"))
	f.handle("GET /api/v4/issues", withTotal("")) // X-Total omitted (>10k)
	f.handle("GET /api/v4/todos", withTotal("7"))
	s, err := p.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Repositories == nil || *s.Repositories != 260 || *s.OpenPullRequests != 3 || s.OpenIssues != nil || *s.Unread != 7 || s.RunningPipelines != nil {
		t.Fatalf("summary = %+v", s)
	}
	if q := f.last(t, "GET", "/api/v4/merge_requests").query(); q.Get("scope") != "created_by_me" || q.Get("state") != "opened" {
		t.Fatalf("MR count query = %v", q)
	}
	if q := f.last(t, "GET", "/api/v4/issues").query(); q.Get("scope") != "assigned_to_me" {
		t.Fatalf("issue count query = %v", q)
	}
}
