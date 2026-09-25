package cloud

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

var acmeWeb = domain.RepositoryRef{Namespace: "acme", Name: "web"}

func TestRepositoryWrites(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	for _, k := range []string{"POST /2.0/repositories/acme/my-repo", "POST /2.0/repositories/team/my-repo",
		"POST /2.0/repositories/acme/web/forks", "PUT /2.0/repositories/acme/web"} {
		f.handle(k, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, f.repoJSON("acme", "result"))
		})
	}
	f.handle("DELETE /2.0/repositories/acme/web", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })

	p := f.provider(t, withYAML("workspace: acme"))
	repo, err := p.CreateRepository(ctx, forge.CreateRepositoryRequest{Name: "My Repo", Description: "d", Visibility: domain.VisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	if repo.FullName != "acme/result" {
		t.Fatalf("repo = %+v", repo)
	}
	body := decodeBody(t, f.last("POST", "/2.0/repositories/acme/my-repo"))
	if !reflect.DeepEqual(body, map[string]any{"scm": "git", "name": "My Repo", "description": "d", "is_private": true}) {
		t.Fatalf("create body = %v", body)
	}
	if _, err := p.CreateRepository(ctx, forge.CreateRepositoryRequest{Name: "my-repo", Namespace: "team", Visibility: domain.VisibilityPublic}); err != nil {
		t.Fatal(err)
	}
	if b := decodeBody(t, f.last("POST", "/2.0/repositories/team/my-repo")); b["is_private"] != false {
		t.Fatalf("public body = %v", b)
	}
	if _, err := f.provider(t).CreateRepository(ctx, forge.CreateRepositoryRequest{Name: "x"}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("no workspace: %v", err)
	}
	if _, err := p.CreateRepository(ctx, forge.CreateRepositoryRequest{Name: "x", AutoInit: true}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("auto init: %v", err)
	}

	if _, err := p.ForkRepository(ctx, acmeWeb, forge.ForkRequest{Namespace: "team", Name: "web-fork"}); err != nil {
		t.Fatal(err)
	}
	fb := decodeBody(t, f.last("POST", "/2.0/repositories/acme/web/forks"))
	if !reflect.DeepEqual(fb, map[string]any{"name": "web-fork", "workspace": map[string]any{"slug": "team"}}) {
		t.Fatalf("fork body = %v", fb)
	}

	if _, err := p.RenameRepository(ctx, acmeWeb, "Web App"); err != nil {
		t.Fatal(err)
	}
	if rb := decodeBody(t, f.last("PUT", "/2.0/repositories/acme/web")); !reflect.DeepEqual(rb, map[string]any{"name": "Web App"}) {
		t.Fatalf("rename body = %v", rb)
	}

	if err := p.DeleteRepository(ctx, acmeWeb); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteRepository(ctx, domain.RepositoryRef{Namespace: "acme", Name: "gone"}); !errors.Is(err, errs.ErrRepositoryNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

func prJSON(id int, state string) map[string]any {
	return map[string]any{
		"type": "pullrequest", "id": id, "title": "Add feature", "description": "Body", "state": state, "draft": true,
		"author":      map[string]any{"display_name": "Jane Doe", "nickname": "jdoe"},
		"source":      map[string]any{"branch": map[string]string{"name": "feature"}, "commit": map[string]string{"hash": "abc123"}, "repository": map[string]string{"full_name": "jdoe/web"}},
		"destination": map[string]any{"branch": map[string]string{"name": "main"}, "repository": map[string]string{"full_name": "acme/web"}},
		"reviewers":   []map[string]any{{"display_name": "Rev", "nickname": "rev"}},
		"links":       map[string]any{"html": map[string]string{"href": "https://bitbucket.org/acme/web/pull-requests/7"}},
		"created_on":  "2025-05-01T10:00:00.000000+00:00",
	}
}

func TestPullRequests(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	f.handle("GET /2.0/repositories/acme/web/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		f.page(w, r, []any{prJSON(7, "OPEN"), prJSON(6, "DECLINED"), prJSON(5, "MERGED")})
	})
	f.handle("GET /2.0/repositories/acme/web/pullrequests/7", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, prJSON(7, "OPEN")) })
	f.handle("POST /2.0/repositories/acme/web/pullrequests", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 201, prJSON(8, "OPEN")) })
	f.handle("POST /2.0/repositories/acme/web/pullrequests/7/merge", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, prJSON(7, "MERGED")) })
	f.handle("POST /2.0/repositories/acme/web/pullrequests/7/decline", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, prJSON(7, "DECLINED")) })
	p := f.provider(t)

	prs, err := p.ListPullRequests(ctx, acmeWeb, forge.PullRequestListOptions{State: domain.PullRequestAll, Author: "jdoe", TargetBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last("GET", "/2.0/repositories/acme/web/pullrequests").Query
	if !reflect.DeepEqual(q["state"], []string{"OPEN", "MERGED", "DECLINED", "SUPERSEDED"}) || q.Get("pagelen") != "50" ||
		q.Get("q") != `author.nickname="jdoe" AND destination.branch.name="main"` {
		t.Fatalf("query = %v", q)
	}
	if len(prs) != 3 || prs[0].State != domain.PullRequestOpen || prs[1].State != domain.PullRequestClosed || prs[2].State != domain.PullRequestMerged {
		t.Fatalf("states = %+v", prs)
	}
	pr := prs[0]
	if pr.Number != 7 || pr.SourceBranch != "feature" || pr.TargetBranch != "main" || pr.SourceRepo != "jdoe/web" || pr.HeadSHA != "abc123" ||
		pr.Author != "jdoe" || !pr.Draft || pr.WebURL == "" || pr.Term != "Pull Request" || !reflect.DeepEqual(pr.Reviewers, []string{"rev"}) {
		t.Fatalf("pr = %+v", pr)
	}
	if _, err := p.ListPullRequests(ctx, acmeWeb, forge.PullRequestListOptions{State: domain.PullRequestClosed}); err != nil {
		t.Fatal(err)
	}
	if q := f.last("GET", "/2.0/repositories/acme/web/pullrequests").Query; !reflect.DeepEqual(q["state"], []string{"DECLINED", "SUPERSEDED"}) {
		t.Fatalf("closed states = %v", q["state"])
	}
	if _, err := p.GetPullRequest(ctx, acmeWeb, 7); err != nil {
		t.Fatal(err)
	}

	if _, err := p.CreatePullRequest(ctx, acmeWeb, forge.CreatePullRequestRequest{Title: "T", Body: "B", SourceBranch: "feature",
		TargetBranch: "main", Draft: true, SourceRepo: "jdoe/web"}); err != nil {
		t.Fatal(err)
	}
	cb := decodeBody(t, f.last("POST", "/2.0/repositories/acme/web/pullrequests"))
	want := map[string]any{
		"title": "T", "description": "B", "draft": true, "close_source_branch": false,
		"source":      map[string]any{"branch": map[string]any{"name": "feature"}, "repository": map[string]any{"full_name": "jdoe/web"}},
		"destination": map[string]any{"branch": map[string]any{"name": "main"}},
	}
	if !reflect.DeepEqual(cb, want) {
		t.Fatalf("create body = %v", cb)
	}

	for method, strategy := range map[domain.MergeMethod]string{domain.MergeMethodMerge: "merge_commit", domain.MergeMethodSquash: "squash", domain.MergeMethodRebase: "rebase_fast_forward"} {
		if err := p.MergePullRequest(ctx, acmeWeb, 7, forge.MergePullRequestRequest{Method: method, CommitTitle: "Title", CommitMessage: "Msg", DeleteSourceBranch: true}); err != nil {
			t.Fatal(err)
		}
		mb := decodeBody(t, f.last("POST", "/2.0/repositories/acme/web/pullrequests/7/merge"))
		if mb["merge_strategy"] != strategy || mb["message"] != "Title\n\nMsg" || mb["close_source_branch"] != true {
			t.Fatalf("merge body for %s = %v", method, mb)
		}
	}
	if err := p.MergePullRequest(ctx, acmeWeb, 7, forge.MergePullRequestRequest{}); err != nil {
		t.Fatal(err)
	}
	if mb := decodeBody(t, f.last("POST", "/2.0/repositories/acme/web/pullrequests/7/merge")); mb["merge_strategy"] != nil || mb["close_source_branch"] != nil {
		t.Fatalf("default merge body = %v", mb)
	}
	if err := p.MergePullRequest(ctx, acmeWeb, 7, forge.MergePullRequestRequest{Method: "octopus"}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("bad method: %v", err)
	}
	if err := p.ClosePullRequest(ctx, acmeWeb, 7); err != nil {
		t.Fatal(err)
	}
	f.last("POST", "/2.0/repositories/acme/web/pullrequests/7/decline")
}

const plUUID = "{aaaaaaaa-0000-0000-0000-000000000001}"
const plUUIDEsc = "%7Baaaaaaaa-0000-0000-0000-000000000001%7D"
const stepUUID = "{bbbbbbbb-0000-0000-0000-000000000001}"
const stepUUIDEsc = "%7Bbbbbbbbb-0000-0000-0000-000000000001%7D"
const step2UUIDEsc = "%7Bbbbbbbbb-0000-0000-0000-000000000002%7D"

func pipelineJSON(n int, state map[string]any) map[string]any {
	return map[string]any{
		"type": "pipeline", "uuid": plUUID, "build_number": n, "state": state, "created_on": "2025-06-01T10:00:00.000Z",
		"completed_on": "2025-06-01T10:05:00.000Z", "duration_in_seconds": 300,
		"creator": map[string]any{"nickname": "jdoe"}, "trigger": map[string]any{"name": "MANUAL"},
		"target": map[string]any{"type": "pipeline_ref_target", "ref_type": "branch", "ref_name": "main",
			"commit": map[string]any{"hash": "deadbeef"}, "selector": map[string]any{"type": "custom", "pattern": "deploy"}},
	}
}

func TestPipelines(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	base := "/2.0/repositories/acme/web/pipelines"
	completed := func(result string) map[string]any {
		return map[string]any{"name": "COMPLETED", "result": map[string]any{"name": result}}
	}
	f.handle("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		f.page(w, r, []any{
			pipelineJSON(3, map[string]any{"name": "IN_PROGRESS", "stage": map[string]any{"name": "RUNNING"}}),
			pipelineJSON(2, completed("FAILED")),
			pipelineJSON(1, completed("SUCCESSFUL")),
		})
	})
	f.handle("GET "+base+"/"+plUUIDEsc, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, pipelineJSON(3, map[string]any{"name": "IN_PROGRESS", "stage": map[string]any{"name": "PAUSED"}}))
	})
	f.handle("GET "+base+"/"+plUUIDEsc+"/steps", func(w http.ResponseWriter, r *http.Request) {
		f.page(w, r, []any{
			map[string]any{"uuid": stepUUID, "name": "Build", "state": completed("SUCCESSFUL"), "started_on": "2025-06-01T10:01:00Z"},
			map[string]any{"uuid": "{bbbbbbbb-0000-0000-0000-000000000002}", "name": "Deploy", "state": map[string]any{"name": "PENDING"}},
		})
	})
	f.handle("POST "+base, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 201, pipelineJSON(4, map[string]any{"name": "PENDING"}))
	})
	f.handle("POST "+base+"/"+plUUIDEsc+"/stopPipeline", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })

	// Completed logs are redirected to storage; our Authorization header
	// must not follow the redirect.
	var storageAuth atomic.Value
	storageAuth.Store("unset")
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "+ go build ./...\nok\n")
	}))
	defer storage.Close()
	f.handle("GET "+base+"/"+plUUIDEsc+"/steps/"+stepUUIDEsc+"/log", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.URL+"/logs/build.txt?sig=abc", http.StatusTemporaryRedirect)
	})
	p := f.provider(t)

	pls, err := p.ListPipelines(ctx, acmeWeb, forge.PipelineListOptions{Ref: "main", Status: domain.PipelineFailed})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last("GET", base).Query
	if q.Get("sort") != "-created_on" || q.Get("target.ref_name") != "main" || q.Get("pagelen") != "100" {
		t.Fatalf("query = %v", q)
	}
	if len(pls) != 1 || pls[0].Number != 2 || pls[0].Status != domain.PipelineFailed || pls[0].RawStatus != "COMPLETED/FAILED" {
		t.Fatalf("filtered pipelines = %+v", pls)
	}
	pl := pls[0]
	if pl.ID != plUUID || pl.Ref != "main" || pl.SHA != "deadbeef" || pl.Name != "deploy" || pl.Event != "manual" || pl.Actor != "jdoe" ||
		pl.WebURL != f.srv.URL+"/acme/web/pipelines/results/2" || pl.Duration.Seconds() != 300 || pl.FinishedAt.IsZero() {
		t.Fatalf("pipeline = %+v", pl)
	}

	got, err := p.GetPipeline(ctx, acmeWeb, plUUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.PipelineManual || len(got.Jobs) != 2 || got.Jobs[0].Name != "Build" || got.Jobs[0].Status != domain.PipelineSuccess ||
		got.Jobs[1].Status != domain.PipelinePending || got.StartedAt.IsZero() {
		t.Fatalf("pipeline = %+v", got)
	}

	if _, err := p.RunPipeline(ctx, acmeWeb, forge.RunPipelineRequest{Ref: "main", Workflow: "deploy", Variables: map[string]string{"B": "2", "A": "1"}}); err != nil {
		t.Fatal(err)
	}
	rb := decodeBody(t, f.last("POST", base))
	wantBody := map[string]any{
		"target": map[string]any{"type": "pipeline_ref_target", "ref_type": "branch", "ref_name": "main",
			"selector": map[string]any{"type": "custom", "pattern": "deploy"}},
		"variables": []any{map[string]any{"key": "A", "value": "1", "secured": false}, map[string]any{"key": "B", "value": "2", "secured": false}},
	}
	if !reflect.DeepEqual(rb, wantBody) {
		t.Fatalf("run body = %v", rb)
	}
	if _, err := p.RunPipeline(ctx, acmeWeb, forge.RunPipelineRequest{}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("missing ref: %v", err)
	}

	if err := p.CancelPipeline(ctx, acmeWeb, plUUID); err != nil {
		t.Fatal(err)
	}
	f.last("POST", base+"/"+plUUIDEsc+"/stopPipeline")

	var buf bytes.Buffer
	if err := p.PipelineLogs(ctx, acmeWeb, plUUID, stepUUID, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "+ go build ./...\nok\n" {
		t.Fatalf("log = %q", buf.String())
	}
	if a := storageAuth.Load().(string); a != "" {
		t.Fatalf("Authorization header leaked to the storage host: %q", a)
	}
	if f.last("GET", base+"/"+plUUIDEsc+"/steps/"+stepUUIDEsc+"/log").Header.Get("Authorization") == "" {
		t.Fatal("API request itself must be authenticated")
	}

	buf.Reset()
	if err := p.PipelineLogs(ctx, acmeWeb, plUUID, "", &buf); err != nil {
		t.Fatal(err)
	}
	want := "==> Build\n+ go build ./...\nok\n\n==> Deploy\n(no log available: step is pending)\n"
	if buf.String() != want {
		t.Fatalf("all logs = %q", buf.String())
	}
	f.last("GET", base+"/"+plUUIDEsc+"/steps/"+step2UUIDEsc+"/log")
}

func TestPipelineStateMapping(t *testing.T) {
	cases := map[string]domain.PipelineStatus{
		`{"name":"PENDING"}`: domain.PipelinePending,
		`{"name":"IN_PROGRESS","stage":{"name":"RUNNING"}}`: domain.PipelineRunning,
		`{"name":"IN_PROGRESS","stage":{"name":"PAUSED"}}`:  domain.PipelineManual,
		`{"name":"HALTED"}`: domain.PipelineManual,
		`{"name":"COMPLETED","result":{"name":"SUCCESSFUL"}}`: domain.PipelineSuccess,
		`{"name":"COMPLETED","result":{"name":"FAILED"}}`:     domain.PipelineFailed,
		`{"name":"COMPLETED","result":{"name":"ERROR"}}`:      domain.PipelineFailed,
		`{"name":"COMPLETED","result":{"name":"STOPPED"}}`:    domain.PipelineCanceled,
		`{"name":"COMPLETED","result":{"name":"EXPIRED"}}`:    domain.PipelineCanceled,
		`{"name":"COMPLETED","result":{"name":"NOT_RUN"}}`:    domain.PipelineSkipped,
		`{"name":"WEIRD"}`: domain.PipelineUnknown,
	}
	for in, want := range cases {
		var s bbState
		if err := jsonUnmarshal(in, &s); err != nil {
			t.Fatal(err)
		}
		if got := s.status(); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestNamespaces(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	f.handle("GET /2.0/workspaces/acme", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"uuid": "{ws-acme}", "slug": "acme", "name": "Acme Inc",
			"links": map[string]any{"html": map[string]string{"href": "https://bitbucket.org/acme/"}, "avatar": map[string]string{"href": "https://a/acme.png"}}})
	})
	f.handle("GET /2.0/workspaces/acme/projects/PROJ", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"uuid": "{p}", "key": "PROJ", "name": "Project", "description": "d"})
	})
	p := f.provider(t)
	nss, err := p.ListNamespaces(ctx, forge.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(nss) != 3 || nss[0].FullPath != "acme" || nss[0].Type != domain.NamespaceWorkspace || nss[0].Label != "Bitbucket Workspace" ||
		nss[0].ID != "{ws-acme}" || nss[0].WebURL != "https://bitbucket.org/acme/" {
		t.Fatalf("namespaces = %+v", nss)
	}
	if len(f.calls("GET", "/2.0/workspaces")) != 0 || len(f.calls("GET", "/2.0/user/permissions/workspaces")) != 0 {
		t.Fatal("removed workspace endpoints must not be used")
	}
	ns, err := p.GetNamespace(ctx, "acme")
	if err != nil || ns.Name != "Acme Inc" || ns.AvatarURL != "https://a/acme.png" {
		t.Fatalf("workspace = %+v err %v", ns, err)
	}
	ns, err = p.GetNamespace(ctx, "acme/PROJ")
	if err != nil || ns.Type != domain.NamespaceProject || ns.FullPath != "acme/PROJ" || ns.ParentPath != "acme" {
		t.Fatalf("project = %+v err %v", ns, err)
	}
	if _, err := p.GetNamespace(ctx, "nope"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("missing workspace: %v", err)
	}

	// Access tokens cannot enumerate workspaces: the configured one is used.
	pa := f.provider(t, withYAML("workspace: acme"), withCred(tokenCred()), withMethod("access-token"))
	nss, err = pa.ListNamespaces(ctx, forge.ListOptions{})
	if err != nil || len(nss) != 1 || nss[0].Name != "Acme Inc" {
		t.Fatalf("access token namespaces = %+v err %v", nss, err)
	}
}

func TestSearch(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	f.handle("GET /2.0/workspaces/acme/search/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"size": 7, "page": 1, "pagelen": 10, "values": []any{map[string]any{
			"type": "code_search_result",
			"file": map[string]any{"path": "cmd/main.go", "links": map[string]any{"self": map[string]string{"href": "https://api.example/src"}},
				"commit": map[string]any{"hash": "c0ffee", "repository": map[string]any{"full_name": "acme/web"}}},
			"content_matches": []any{map[string]any{"lines": []any{
				map[string]any{"line": 3, "segments": []any{map[string]any{"text": "func "}, map[string]any{"text": "main", "match": true}, map[string]any{"text": "() {"}}},
			}}},
		}}})
	})
	p := f.provider(t, withYAML("workspace: acme"))
	res, err := p.Search(ctx, domain.SearchQuery{Kind: domain.SearchCode, Query: "main", Repository: &acmeWeb})
	if err != nil {
		t.Fatal(err)
	}
	q := f.last("GET", "/2.0/workspaces/acme/search/code").Query
	if q.Get("search_query") != "main repo:web" || q.Get("fields") != "+values.file.commit.repository" {
		t.Fatalf("code query = %v", q)
	}
	want := domain.CodeResult{Repository: "acme/web", Path: "cmd/main.go", Ref: "c0ffee", Fragment: "func main() {",
		WebURL: f.srv.URL + "/acme/web/src/c0ffee/cmd/main.go"}
	if res.Total != 7 || len(res.Code) != 1 || res.Code[0] != want {
		t.Fatalf("code result = %+v", res)
	}
	if _, err := f.provider(t).Search(ctx, domain.SearchQuery{Kind: domain.SearchCode, Query: "x"}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("code search without workspace: %v", err)
	}

	res, err = p.Search(ctx, domain.SearchQuery{Kind: domain.SearchRepositories, Query: `we"b`, Namespace: "solo"})
	if err != nil {
		t.Fatal(err)
	}
	if q := f.last("GET", "/2.0/repositories/solo").Query; q.Get("q") != `name~"we\"b"` {
		t.Fatalf("repo search query = %v", q)
	}
	if res.Kind != domain.SearchRepositories || res.Total != 3 {
		t.Fatalf("repo search = %+v", res)
	}

	_, err = p.Search(ctx, domain.SearchQuery{Kind: domain.SearchIssues, Query: "bug", Repository: &acmeWeb})
	if !errors.Is(err, errs.ErrUnsupportedCapability) {
		t.Fatalf("issue search: %v", err)
	}
	if _, err := forge.As[forge.IssueProvider](p, forge.CapIssues); !errors.Is(err, errs.ErrUnsupportedCapability) {
		t.Fatalf("issues capability: %v", err)
	}
}

func TestSnippets(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	snip := func(ws, id string) map[string]any {
		return map[string]any{"id": id, "title": "Snip " + id, "is_private": id == "kypj",
			"owner": map[string]any{"nickname": "jdoe"},
			"links": map[string]any{"self": map[string]string{"href": f.apiURL() + "/snippets/" + ws + "/" + id},
				"html": map[string]string{"href": "https://bitbucket.org/snippets/" + ws + "/" + id}},
			"files": map[string]any{"b.txt": map[string]any{}, "a.go": map[string]any{}}}
	}
	f.handle("GET /2.0/snippets/acme", func(w http.ResponseWriter, r *http.Request) { f.page(w, r, []any{snip("acme", "kypj")}) })
	f.handle("GET /2.0/snippets/empty", func(w http.ResponseWriter, r *http.Request) { f.page(w, r, nil) })
	f.handle("GET /2.0/snippets/solo", func(w http.ResponseWriter, r *http.Request) { f.page(w, r, []any{snip("solo", "7")}) })
	f.handle("GET /2.0/snippets/acme/kypj", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, snip("acme", "kypj")) })
	// The files convenience endpoint redirects to the latest revision on the
	// same host, so credentials must be kept.
	for _, name := range []string{"a.go", "b.txt"} {
		f.handle("GET /2.0/snippets/acme/kypj/files/"+name, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/2.0/snippets/acme/kypj/rev1/files/"+name, http.StatusFound)
		})
		f.handle("GET /2.0/snippets/acme/kypj/rev1/files/"+name, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "content of "+strings.TrimPrefix(r.URL.Path, "/2.0/snippets/acme/kypj/rev1/files/"))
		})
	}
	var form *multipart.Form
	f.handle("POST /2.0/snippets/acme", func(w http.ResponseWriter, r *http.Request) {
		mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt != "multipart/form-data" {
			t.Errorf("content type = %s", mt)
		}
		var err error
		form, err = multipart.NewReader(r.Body, params["boundary"]).ReadForm(1 << 20)
		if err != nil {
			t.Error(err)
		}
		writeJSON(w, 201, snip("acme", "new1"))
	})
	p := f.provider(t, withYAML("workspace: acme"))

	list, err := p.ListSnippets(ctx, forge.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "acme/kypj" || list[0].Visibility != domain.VisibilityPrivate || list[1].ID != "solo/7" ||
		list[0].Files[0].Name != "a.go" || list[0].Term != "Snippet" || list[0].Owner != "jdoe" {
		t.Fatalf("snippets = %+v", list)
	}
	if q := f.last("GET", "/2.0/snippets/acme").Query; q.Get("role") != "member" {
		t.Fatalf("list query = %v", q)
	}
	if len(f.calls("GET", "/2.0/snippets")) != 0 {
		t.Fatal("the removed cross-workspace /snippets listing must not be used")
	}

	s, err := p.GetSnippet(ctx, "acme/kypj")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 2 || s.Files[0].Content != "content of a.go" || s.Files[1].Content != "content of b.txt" {
		t.Fatalf("snippet files = %+v", s.Files)
	}
	if f.last("GET", "/2.0/snippets/acme/kypj/rev1/files/a.go").Header.Get("Authorization") == "" {
		t.Fatal("same-host redirect must keep credentials")
	}
	if _, err := p.GetSnippet(ctx, "kypj"); err != nil {
		t.Fatalf("bare id with configured workspace: %v", err)
	}
	if _, err := f.provider(t).GetSnippet(ctx, "kypj"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("bare id without workspace: %v", err)
	}

	created, err := p.CreateSnippet(ctx, forge.CreateSnippetRequest{Title: "My snippet", Visibility: domain.VisibilityPublic,
		Files: []domain.SnippetFile{{Name: "a.go", Content: "package a"}, {Name: "b.txt", Content: "bee"}}})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "acme/new1" {
		t.Fatalf("created = %+v", created)
	}
	if form == nil || form.Value["title"][0] != "My snippet" || form.Value["is_private"][0] != "false" || len(form.File["file"]) != 2 ||
		form.File["file"][0].Filename != "a.go" {
		t.Fatalf("multipart form = %+v", form)
	}
	fh, _ := form.File["file"][1].Open()
	b, _ := io.ReadAll(fh)
	if string(b) != "bee" {
		t.Fatalf("file content = %q", b)
	}
	if _, err := p.CreateSnippet(ctx, forge.CreateSnippetRequest{Title: "t", Description: "d", Files: []domain.SnippetFile{{Name: "x"}}}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("title+description: %v", err)
	}
}

func TestKeys(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	userPath := "/2.0/users/%7B11111111-2222-3333-4444-555555555555%7D"
	f.handle("GET "+userPath+"/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		f.page(w, r, []any{map[string]any{"uuid": "{k1}", "label": "laptop", "key": "ssh-ed25519 AAAA", "fingerprint": "SHA256:x",
			"created_on": "2025-01-01T00:00:00Z"}})
	})
	f.handle("POST "+userPath+"/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 201, map[string]any{"uuid": "{k2}", "label": "new", "key": "ssh-ed25519 BBBB"})
	})
	f.handle("DELETE "+userPath+"/ssh-keys/%7Bk2%7D", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	f.handle("GET "+userPath+"/gpg-keys", func(w http.ResponseWriter, r *http.Request) {
		f.page(w, r, []any{map[string]any{"key_id": "ABCD1234", "fingerprint": "FFFF", "added_on": "2025-01-01T00:00:00Z",
			"expires_on": "2027-01-01T00:00:00Z"}})
	})
	p := f.provider(t)
	keys, err := p.ListSSHKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].ID != "{k1}" || keys[0].Title != "laptop" || keys[0].Fingerprint != "SHA256:x" || keys[0].CreatedAt.IsZero() {
		t.Fatalf("keys = %+v", keys)
	}
	k, err := p.AddSSHKey(ctx, "new", "ssh-ed25519 BBBB\n")
	if err != nil || k.ID != "{k2}" {
		t.Fatalf("add = %+v err %v", k, err)
	}
	if b := decodeBody(t, f.last("POST", userPath+"/ssh-keys")); !reflect.DeepEqual(b, map[string]any{"key": "ssh-ed25519 BBBB", "label": "new"}) {
		t.Fatalf("add body = %v", b)
	}
	if err := p.RemoveSSHKey(ctx, "{k2}"); err != nil {
		t.Fatal(err)
	}
	gpg, err := p.ListGPGKeys(ctx)
	if err != nil || len(gpg) != 1 || gpg[0].ID != "FFFF" || gpg[0].KeyID != "ABCD1234" || gpg[0].CreatedAt.IsZero() || gpg[0].ExpiresAt.IsZero() {
		t.Fatalf("gpg = %+v err %v", gpg, err)
	}
	// The user UUID is looked up once and cached.
	if n := len(f.calls("GET", "/2.0/user")); n != 1 {
		t.Fatalf("GET /user called %d times", n)
	}
}

func TestSummary(t *testing.T) {
	f := newFake(t)
	userPath := "%7B11111111-2222-3333-4444-555555555555%7D"
	for ws, n := range map[string]int{"acme": 4, "empty": 0, "solo": 1} {
		f.handle("GET /2.0/workspaces/"+ws+"/pullrequests/"+userPath, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("state") != "OPEN" {
				t.Errorf("state = %s", r.URL.Query().Get("state"))
			}
			writeJSON(w, 200, map[string]any{"values": []any{}, "size": n})
		})
	}
	sum, err := f.provider(t).Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Repositories == nil || *sum.Repositories != 263 || sum.OpenPullRequests == nil || *sum.OpenPullRequests != 5 ||
		sum.OpenIssues != nil || sum.RunningPipelines != nil || sum.Unread != nil {
		t.Fatalf("summary = %+v", sum)
	}

	// Access tokens have no user: PR counts are unavailable, not zero.
	pa := f.provider(t, withYAML("workspace: solo"), withCred(tokenCred()), withMethod("access-token"))
	sum, err = pa.Summary(context.Background())
	if err != nil || sum.Repositories == nil || *sum.Repositories != 3 || sum.OpenPullRequests != nil {
		t.Fatalf("access token summary = %+v err %v", sum, err)
	}
}
