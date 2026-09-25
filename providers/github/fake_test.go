package github

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tokens used by the fake. They look like real GitHub tokens so the
// redaction patterns are exercised too.
const (
	testToken        = "ghp_TESTtokenABCDEFGHIJKLMNOPQRSTUVWXYZ0123"
	testFineGrained  = "github_pat_11TESTfinegrained_ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	testBadToken     = "ghp_BADtokenABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	testDeviceToken  = "gho_DEVICEtokenABCDEFGHIJKLMNOPQRSTUVWXYZ01"
	testRefreshed    = "ghu_REFRESHEDtokenABCDEFGHIJKLMNOPQRSTUVWX"
	testRefreshToken = "ghr_VALIDrefreshABCDEFGHIJKLMNOPQRSTUVWXYZ"
	testInstToken    = "ghs_INSTALLATIONtokenABCDEFGHIJKLMNOPQRSTU"
	testAppID        = "4242"
	testInstID       = "777"
	testClientID     = "Iv1.testclientid"
	numUserRepos     = 260
)

type recorded struct {
	Method string
	Path   string // relative to the API root (prefix stripped)
	Query  url.Values
	Body   map[string]any
	Header http.Header
}

// fakeGitHub is an in-memory GitHub REST API. prefix is "" for a
// github.com-style API root and "/api/v3" for GHES.
type fakeGitHub struct {
	t      *testing.T
	srv    *httptest.Server
	blob   *httptest.Server // second origin: log redirects and raw gist files
	prefix string

	appPub *rsa.PublicKey

	mu            sync.Mutex
	reqs          []recorded
	blobAuth      []string
	blobHits      int
	tokens        map[string]bool
	user          map[string]any
	devicePending int
	devicePolls   int
	exchanges     int
	lastJWTClaims map[string]any
	// override lets a test intercept any API request.
	override func(w http.ResponseWriter, r *http.Request, path string) bool
}

func newFake(t *testing.T, prefix string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, prefix: prefix, tokens: map[string]bool{testToken: true, testFineGrained: true}}
	f.user = map[string]any{
		"id": 1, "login": "octocat", "name": "The Octocat", "email": "octocat@example.com", "type": "User",
		"blog": "https://octo.example", "company": "GitHub", "location": "SF", "bio": "cat",
		"twitter_username": nil, "hireable": true, "public_repos": 250, "total_private_repos": 10,
	}
	f.blob = httptest.NewServer(http.HandlerFunc(f.serveBlob))
	root := http.NewServeMux()
	root.HandleFunc("POST /login/device/code", f.deviceCode)
	root.HandleFunc("POST /login/oauth/access_token", f.accessToken)
	api := http.HandlerFunc(f.serveAPI)
	if prefix == "" {
		root.Handle("/", api)
	} else {
		root.Handle(prefix+"/", http.StripPrefix(prefix, api))
	}
	f.srv = httptest.NewServer(root)
	t.Cleanup(func() { f.srv.Close(); f.blob.Close() })
	return f
}

func (f *fakeGitHub) apiRoot() string { return f.srv.URL + f.prefix }
func (f *fakeGitHub) webRoot() string { return f.srv.URL }

func (f *fakeGitHub) requests(method, path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.reqs {
		if (method == "" || r.Method == method) && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGitHub) last(method, path string) recorded {
	f.t.Helper()
	rs := f.requests(method, path)
	if len(rs) == 0 {
		f.t.Fatalf("no %s %s request recorded", method, path)
	}
	return rs[len(rs)-1]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMsg(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"message": msg, "documentation_url": "https://docs.github.com/rest"})
}

// --- repositories fixtures ----------------------------------------------------------

func (f *fakeGitHub) repo(owner, name string, id int, vis string, archived bool) map[string]any {
	r := map[string]any{
		"id": id, "name": name, "full_name": owner + "/" + name,
		"owner":       map[string]any{"login": owner, "type": "User"},
		"description": "repo " + name, "private": vis != "public", "archived": archived, "fork": false,
		"default_branch": "main", "language": "Go", "stargazers_count": id % 7, "forks_count": 1,
		"open_issues_count": 2, "created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-06-01T00:00:00Z",
		"pushed_at": "2024-06-01T00:00:00Z", "html_url": f.webRoot() + "/" + owner + "/" + name,
		"clone_url": f.webRoot() + "/" + owner + "/" + name + ".git",
		"ssh_url":   "git@example.test:" + owner + "/" + name + ".git",
		"url":       f.apiRoot() + "/repos/" + owner + "/" + name, "topics": []string{"x"},
	}
	// Older GHES releases have no "visibility" field; every 7th repo mimics
	// that to exercise the fallback to "private".
	if id%7 != 0 {
		r["visibility"] = vis
	}
	return r
}

func (f *fakeGitHub) userRepos() []any {
	out := make([]any, 0, numUserRepos)
	for i := 1; i <= numUserRepos; i++ {
		vis := "public"
		if i%3 == 0 {
			vis = "private"
		}
		out = append(out, f.repo("octocat", fmt.Sprintf("repo-%03d", i), i, vis, i%10 == 0))
	}
	return out
}

func (f *fakeGitHub) orgRepos() []any {
	return []any{
		f.repo("octo-org", "org-repo-1", 1001, "public", false),
		f.repo("octo-org", "org-repo-2", 1002, "internal", false),
		f.repo("octo-org", "org-repo-3", 1003, "private", true),
	}
}

// writePage serves items with page/per_page pagination and Link headers.
func (f *fakeGitHub) writePage(w http.ResponseWriter, r *http.Request, items []any, envelope string) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if per <= 0 {
		per = 30
	}
	if per > 100 {
		per = 100
	}
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if pg <= 0 {
		pg = 1
	}
	start := (pg - 1) * per
	if start > len(items) {
		start = len(items)
	}
	end := start + per
	if end > len(items) {
		end = len(items)
	}
	lastPage := (len(items) + per - 1) / per
	link := func(n int) string {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(n))
		q.Set("per_page", strconv.Itoa(per))
		return "http://" + r.Host + f.prefix + r.URL.Path + "?" + q.Encode()
	}
	var links []string
	if pg < lastPage {
		links = append(links, fmt.Sprintf(`<%s>; rel="next"`, link(pg+1)), fmt.Sprintf(`<%s>; rel="last"`, link(lastPage)))
	}
	if pg > 1 {
		links = append(links, fmt.Sprintf(`<%s>; rel="prev"`, link(pg-1)), fmt.Sprintf(`<%s>; rel="first"`, link(1)))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	pageItems := items[start:end]
	if envelope == "" {
		writeJSON(w, http.StatusOK, pageItems)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(items), envelope: pageItems})
}

// --- pulls / issues / runs fixtures ------------------------------------------------

func (f *fakeGitHub) pull(n int, state string, merged bool, headRepo, user string) map[string]any {
	pr := map[string]any{
		"id": 9000 + n, "number": n, "title": fmt.Sprintf("PR %d", n), "body": "body", "state": state, "draft": false,
		"user": map[string]any{"login": user},
		"head": map[string]any{"ref": fmt.Sprintf("feature-%d", n), "sha": "abc123",
			"repo": map[string]any{"full_name": headRepo}},
		"base":   map[string]any{"ref": "main", "sha": "def456", "repo": map[string]any{"full_name": "octocat/repo-001"}},
		"labels": []any{map[string]any{"name": "bug"}}, "requested_reviewers": []any{map[string]any{"login": "rev"}},
		"html_url":   f.webRoot() + "/octocat/repo-001/pull/" + strconv.Itoa(n),
		"created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-02T00:00:00Z", "merged_at": nil, "closed_at": nil,
		"mergeable": true,
	}
	if state == "closed" {
		pr["closed_at"] = "2024-01-03T00:00:00Z"
	}
	if merged {
		pr["merged_at"] = "2024-01-03T00:00:00Z"
	}
	return pr
}

func (f *fakeGitHub) pulls() []any {
	return []any{
		f.pull(1, "open", false, "octocat/repo-001", "octocat"),
		f.pull(2, "closed", true, "octocat/repo-001", "octocat"),
		f.pull(3, "closed", false, "octocat/repo-001", "hubot"),
		f.pull(4, "open", false, "contrib/repo-001", "contrib"),
	}
}

func (f *fakeGitHub) issue(n int, isPR bool, state string) map[string]any {
	i := map[string]any{
		"id": 8000 + n, "number": n, "title": fmt.Sprintf("Issue %d", n), "body": "text", "state": state,
		"state_reason": nil, "user": map[string]any{"login": "octocat"},
		"assignees": []any{map[string]any{"login": "octocat"}}, "labels": []any{map[string]any{"name": "bug"}},
		"comments": 3, "html_url": f.webRoot() + "/octocat/repo-001/issues/" + strconv.Itoa(n),
		"created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-02T00:00:00Z", "closed_at": nil,
	}
	if isPR {
		i["pull_request"] = map[string]any{"url": f.apiRoot() + "/repos/octocat/repo-001/pulls/" + strconv.Itoa(n)}
	}
	if state == "closed" {
		i["state_reason"] = "completed"
		i["closed_at"] = "2024-01-03T00:00:00Z"
	}
	return i
}

func run(id int, status string, conclusion, started any) map[string]any {
	return map[string]any{
		"id": id, "name": "CI", "display_title": "Fix bug", "run_number": id - 1000, "run_attempt": 1,
		"status": status, "conclusion": conclusion, "head_branch": "main", "head_sha": "abc", "event": "push",
		"actor": map[string]any{"login": "octocat"}, "html_url": "https://example.test/runs/" + strconv.Itoa(id),
		"created_at": "2024-01-01T00:00:00Z", "run_started_at": started, "updated_at": "2024-01-01T00:10:00Z",
	}
}

func (f *fakeGitHub) runs() []any {
	return []any{
		run(1001, "completed", "success", "2024-01-01T00:01:00Z"),
		run(1002, "in_progress", nil, "2024-01-01T00:02:00Z"),
		run(1003, "completed", "timed_out", "2024-01-01T00:03:00Z"),
		run(1004, "queued", nil, nil),
	}
}

func job(id int, name, status string, conclusion any) map[string]any {
	return map[string]any{
		"id": id, "run_id": 1001, "name": name, "workflow_name": "CI", "status": status, "conclusion": conclusion,
		"html_url": "https://example.test/jobs/" + strconv.Itoa(id), "started_at": "2024-01-01T00:01:00Z",
		"completed_at": "2024-01-01T00:05:00Z", "steps": []any{map[string]any{"name": "checkout", "status": "completed"}},
	}
}

func release(id int, tag string, draft, pre bool) map[string]any {
	return map[string]any{
		"id": id, "tag_name": tag, "name": "Release " + tag, "body": "notes", "draft": draft, "prerelease": pre,
		"author": map[string]any{"login": "octocat"}, "html_url": "https://example.test/releases/" + tag,
		"created_at": "2024-01-01T00:00:00Z", "published_at": "2024-01-02T00:00:00Z",
		"assets": []any{map[string]any{"name": "bin.tgz", "browser_download_url": "https://example.test/bin.tgz", "size": 42}},
	}
}

func (f *fakeGitHub) releases() []any {
	return []any{release(1, "v1.0.0", false, false), release(2, "v2.0.0-rc1", false, true), release(3, "v3.0.0", true, false)}
}

func (f *fakeGitHub) thread(id, typ, subjectPath string, unread bool) map[string]any {
	var subj any
	if subjectPath != "" {
		subj = f.apiRoot() + "/repos/octocat/repo-001/" + subjectPath
	}
	return map[string]any{
		"id": id, "unread": unread, "reason": "mention", "updated_at": "2024-01-01T00:00:00Z",
		"subject":    map[string]any{"title": "Thread " + id, "url": subj, "type": typ},
		"repository": map[string]any{"full_name": "octocat/repo-001", "html_url": f.webRoot() + "/octocat/repo-001"},
	}
}

func (f *fakeGitHub) threads() []any {
	return []any{
		f.thread("11", "PullRequest", "pulls/1", true),
		f.thread("12", "Issue", "issues/10", true),
		f.thread("13", "Discussion", "", true),
	}
}

// --- API dispatcher --------------------------------------------------------------

func (f *fakeGitHub) record(r *http.Request, path string) {
	var body map[string]any
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			if err := json.Unmarshal(b, &body); err != nil {
				f.t.Errorf("%s %s: request body is not a JSON object: %v", r.Method, path, err)
			}
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, Path: path, Query: r.URL.Query(), Body: body, Header: r.Header.Clone()})
	f.mu.Unlock()
}

func (f *fakeGitHub) serveAPI(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	f.record(r, path)
	if f.override != nil && f.override(w, r, path) {
		return
	}
	if r.Header.Get("X-GitHub-Api-Version") != apiVersion {
		writeMsg(w, http.StatusBadRequest, "missing API version header")
		return
	}
	authz := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(authz, "Bearer ")

	// Endpoints authenticated as the App itself (JWT).
	if path == "/app" || strings.HasPrefix(path, "/app/installations/") {
		if !f.verifyJWT(tok) {
			writeMsg(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		f.serveApp(w, r, path)
		return
	}
	if tok == testInstToken {
		f.mu.Lock()
		minted := f.exchanges > 0
		f.mu.Unlock()
		if !minted {
			writeMsg(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		if path == "/installation/repositories" {
			f.writePage(w, r, f.userRepos()[:120], "repositories")
			return
		}
		if strings.HasPrefix(path, "/user") || strings.HasPrefix(path, "/notifications") || strings.HasPrefix(path, "/gists") {
			writeMsg(w, http.StatusForbidden, "Resource not accessible by integration")
			return
		}
	} else {
		f.mu.Lock()
		ok := f.tokens[tok]
		f.mu.Unlock()
		if !ok {
			writeMsg(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
	}
	f.route(w, r, path, tok)
}

func (f *fakeGitHub) route(w http.ResponseWriter, r *http.Request, path, tok string) {
	seg := strings.Split(strings.Trim(path, "/"), "/")
	m := r.Method
	rec := f.last(m, path)
	switch {
	case path == "/user" && m == http.MethodGet:
		if strings.HasPrefix(tok, "github_pat_") {
			w.Header().Set("GitHub-Authentication-Token-Expiration", "2030-01-02 03:04:05 UTC")
			u := map[string]any{}
			f.mu.Lock()
			for k, v := range f.user {
				u[k] = v
			}
			f.mu.Unlock()
			delete(u, "total_private_repos") // fine-grained tokens may omit it
			writeJSON(w, http.StatusOK, u)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "repo, gist, read:org")
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, http.StatusOK, f.user)
	case path == "/user" && m == http.MethodPatch:
		f.mu.Lock()
		for k, v := range rec.Body {
			f.user[k] = v
		}
		defer f.mu.Unlock()
		writeJSON(w, http.StatusOK, f.user)
	case path == "/user/repos" && m == http.MethodGet:
		f.writePage(w, r, f.userRepos(), "")
	case path == "/user/repos" && m == http.MethodPost:
		vis := "public"
		if rec.Body["private"] == true {
			vis = "private"
		}
		repo := f.repo("octocat", rec.Body["name"].(string), 5000, vis, false)
		if rec.Body["auto_init"] == true {
			repo["default_branch"] = "master"
		}
		writeJSON(w, http.StatusCreated, repo)
	case path == "/user/orgs":
		f.writePage(w, r, []any{
			map[string]any{"id": 100, "login": "octo-org", "description": "Octo org", "avatar_url": "a"},
			map[string]any{"id": 101, "login": "other-org", "description": nil, "avatar_url": "b"},
		}, "")
	case path == "/user/keys" && m == http.MethodGet:
		f.writePage(w, r, []any{map[string]any{"id": 1, "title": "laptop",
			"key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG2mBn3oIV+0B7bEYm7C3u/C8z1oqk5dNl2lVwJb4Tzu", "created_at": "2024-01-01T00:00:00Z"}}, "")
	case path == "/user/keys" && m == http.MethodPost:
		writeJSON(w, http.StatusCreated, map[string]any{"id": 2, "title": rec.Body["title"], "key": rec.Body["key"], "created_at": "2024-01-01T00:00:00Z"})
	case len(seg) == 3 && seg[0] == "user" && seg[1] == "keys" && m == http.MethodDelete:
		if seg[2] != "1" {
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "/user/gpg_keys":
		f.writePage(w, r, []any{map[string]any{"id": 3, "key_id": "3262EFF25BA0D270",
			"emails":     []any{map[string]any{"email": "octocat@example.com", "verified": true}},
			"created_at": "2024-01-01T00:00:00Z", "expires_at": nil}}, "")
	case len(seg) == 2 && seg[0] == "orgs" && m == http.MethodGet:
		if seg[1] != "octo-org" {
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 100, "login": "octo-org", "name": "Octo Org",
			"description": "Octo org", "html_url": f.webRoot() + "/octo-org"})
	case len(seg) == 3 && seg[0] == "orgs" && seg[2] == "repos" && m == http.MethodGet:
		if seg[1] != "octo-org" {
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		f.writePage(w, r, f.orgRepos(), "")
	case len(seg) == 3 && seg[0] == "orgs" && seg[2] == "repos" && m == http.MethodPost:
		vis, _ := rec.Body["visibility"].(string)
		writeJSON(w, http.StatusCreated, f.repo(seg[1], rec.Body["name"].(string), 6000, orDefault(vis, "public"), false))
	case len(seg) == 2 && seg[0] == "users":
		if seg[1] != "hubot" && seg[1] != "octocat" {
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 2, "login": seg[1], "name": nil, "type": "User",
			"html_url": f.webRoot() + "/" + seg[1]})
	case len(seg) == 3 && seg[0] == "users" && seg[2] == "repos":
		if seg[1] != "hubot" {
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		f.writePage(w, r, []any{f.repo("hubot", "h1", 7001, "public", false), f.repo("hubot", "h2", 7002, "public", false)}, "")
	case len(seg) >= 3 && seg[0] == "repos":
		f.routeRepo(w, r, seg[1], seg[2], seg[3:], rec)
	case path == "/search/repositories":
		f.writePage(w, r, []any{f.repo("octocat", "repo-001", 1, "public", false), f.repo("octocat", "repo-002", 2, "public", false)}, "items")
	case path == "/search/issues":
		q := r.URL.Query().Get("q")
		switch {
		case strings.Contains(q, "is:pr is:open author:@me"):
			writeJSON(w, http.StatusOK, map[string]any{"total_count": 7, "items": []any{f.issue(1, true, "open")}})
		case strings.Contains(q, "is:issue is:open assignee:@me"):
			writeJSON(w, http.StatusOK, map[string]any{"total_count": 4, "items": []any{f.issue(10, false, "open")}})
		default:
			f.writePage(w, r, []any{f.issue(10, false, "open"), f.issue(12, false, "closed")}, "items")
		}
	case path == "/search/code":
		f.writePage(w, r, []any{map[string]any{"name": "main.go", "path": "cmd/main.go", "sha": "s1",
			"html_url": "https://example.test/blob/main.go", "repository": map[string]any{"full_name": "octocat/repo-001"},
			"text_matches": []any{map[string]any{"fragment": "func main()"}}}}, "items")
	case path == "/notifications" && m == http.MethodGet:
		f.writePage(w, r, f.threads(), "")
	case path == "/notifications" && m == http.MethodPut:
		w.WriteHeader(http.StatusResetContent)
	case len(seg) == 3 && seg[0] == "notifications" && seg[1] == "threads":
		if seg[2] != "11" {
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		if m == http.MethodPatch {
			w.WriteHeader(http.StatusResetContent)
			return
		}
		writeJSON(w, http.StatusOK, f.thread("11", "PullRequest", "pulls/1", true))
	case path == "/gists" && m == http.MethodGet:
		f.writePage(w, r, []any{f.gist("aa11", true), f.gist("bb22", false)}, "")
	case path == "/gists" && m == http.MethodPost:
		files := map[string]any{}
		for name, v := range rec.Body["files"].(map[string]any) {
			files[name] = map[string]any{"filename": name, "content": v.(map[string]any)["content"], "raw_url": "x"}
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": "cc33", "description": rec.Body["description"],
			"public": rec.Body["public"], "files": files, "html_url": "https://gist.example/cc33",
			"owner": map[string]any{"login": "octocat"}, "created_at": "2024-01-01T00:00:00Z"})
	case len(seg) == 2 && seg[0] == "gists":
		if seg[1] != "aa11" {
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, f.gist("aa11", true))
	default:
		writeMsg(w, http.StatusNotFound, "Not Found")
	}
}

func (f *fakeGitHub) gist(id string, public bool) map[string]any {
	return map[string]any{
		"id": id, "description": "gist " + id, "public": public, "owner": map[string]any{"login": "octocat"},
		"html_url": "https://gist.example/" + id, "created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-02T00:00:00Z",
		"files": map[string]any{
			"b.txt":    map[string]any{"filename": "b.txt", "content": "small", "truncated": false, "raw_url": f.blob.URL + "/raw/b.txt"},
			"a-big.go": map[string]any{"filename": "a-big.go", "content": "trunc", "truncated": true, "raw_url": f.blob.URL + "/raw/a-big.go"},
		},
	}
}

func (f *fakeGitHub) routeRepo(w http.ResponseWriter, r *http.Request, owner, name string, rest []string, rec recorded) {
	m := r.Method
	exists := owner == "octocat" && strings.HasPrefix(name, "repo-") || owner == "octo-org"
	if !exists {
		writeMsg(w, http.StatusNotFound, "Not Found")
		return
	}
	sub := strings.Join(rest, "/")
	switch {
	case sub == "" && m == http.MethodGet:
		writeJSON(w, http.StatusOK, f.repo(owner, name, 1, "public", false))
	case sub == "" && m == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case sub == "" && m == http.MethodPatch:
		repo := f.repo(owner, name, 1, "public", false)
		if n, ok := rec.Body["name"].(string); ok {
			repo = f.repo(owner, n, 1, "public", false)
		}
		if a, ok := rec.Body["archived"].(bool); ok {
			repo["archived"] = a
		}
		writeJSON(w, http.StatusOK, repo)
	case sub == "forks" && m == http.MethodPost:
		org, _ := rec.Body["organization"].(string)
		newName, _ := rec.Body["name"].(string)
		writeJSON(w, http.StatusAccepted, f.repo(orDefault(org, "octocat"), orDefault(newName, name), 8000, "public", false))
	case strings.HasPrefix(sub, "branches/") && strings.HasSuffix(sub, "/rename") && m == http.MethodPost:
		writeJSON(w, http.StatusCreated, map[string]any{"name": rec.Body["new_name"]})
	case sub == "pulls" && m == http.MethodGet:
		items := f.pulls()
		var out []any
		for _, it := range items {
			st := it.(map[string]any)["state"]
			if s := r.URL.Query().Get("state"); s == "all" || s == st {
				out = append(out, it)
			}
		}
		f.writePage(w, r, out, "")
	case sub == "pulls" && m == http.MethodPost:
		pr := f.pull(5, "open", false, "octocat/repo-001", "octocat")
		pr["title"] = rec.Body["title"]
		writeJSON(w, http.StatusCreated, pr)
	case len(rest) == 2 && rest[0] == "pulls":
		n, _ := strconv.Atoi(rest[1])
		if n < 1 || n > 4 && n != 9 {
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		if m == http.MethodPatch {
			writeJSON(w, http.StatusOK, f.pull(n, "closed", false, "octocat/repo-001", "octocat"))
			return
		}
		writeJSON(w, http.StatusOK, f.pulls()[min(n, 4)-1])
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "merge":
		if rest[1] == "9" {
			writeMsg(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"merged": true, "sha": "zzz", "message": "Pull Request successfully merged"})
	case strings.HasPrefix(sub, "git/refs/heads/") && m == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case sub == "issues" && m == http.MethodGet:
		f.writePage(w, r, []any{f.issue(10, false, "open"), f.issue(11, true, "open"), f.issue(12, false, "open")}, "")
	case sub == "issues" && m == http.MethodPost:
		i := f.issue(13, false, "open")
		i["title"] = rec.Body["title"]
		writeJSON(w, http.StatusCreated, i)
	case len(rest) == 2 && rest[0] == "issues":
		switch rest[1] {
		case "10", "12":
		case "11":
			writeJSON(w, http.StatusOK, f.issue(11, true, "open"))
			return
		default:
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		n, _ := strconv.Atoi(rest[1])
		state := "open"
		if s, ok := rec.Body["state"].(string); ok {
			state = s
		}
		writeJSON(w, http.StatusOK, f.issue(n, false, state))
	case sub == "actions/runs" && m == http.MethodGet:
		f.writePage(w, r, f.runs(), "workflow_runs")
	case len(rest) == 3 && sub == "actions/runs/"+rest[2] && m == http.MethodGet:
		for _, it := range f.runs() {
			if strconv.Itoa(it.(map[string]any)["id"].(int)) == rest[2] {
				writeJSON(w, http.StatusOK, it)
				return
			}
		}
		writeMsg(w, http.StatusNotFound, "Not Found")
	case len(rest) == 4 && rest[1] == "runs" && rest[3] == "jobs":
		f.writePage(w, r, []any{job(5001, "build", "completed", "success"), job(5002, "test", "completed", "skipped"),
			job(5003, "deploy", "queued", nil)}, "jobs")
	case len(rest) == 4 && rest[1] == "runs" && rest[3] == "cancel" && m == http.MethodPost:
		if rest[2] == "1001" {
			writeMsg(w, http.StatusConflict, "Cannot cancel a workflow run that is completed.")
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{})
	case len(rest) == 4 && rest[1] == "runs" && rest[3] == "rerun" && m == http.MethodPost:
		writeJSON(w, http.StatusCreated, map[string]any{})
	case len(rest) == 4 && rest[1] == "jobs" && rest[3] == "logs":
		if rest[2] != "5001" {
			writeMsg(w, http.StatusNotFound, "Not Found")
			return
		}
		http.Redirect(w, r, f.blob.URL+"/logs/"+rest[2]+"?sig=abc", http.StatusFound)
	case len(rest) == 4 && rest[1] == "workflows" && rest[3] == "dispatches" && m == http.MethodPost:
		switch rest[2] {
		case "ci.yml":
			w.WriteHeader(http.StatusNoContent) // GHES behavior
		case "deploy.yml":
			writeJSON(w, http.StatusOK, map[string]any{"workflow_run_id": 1004,
				"run_url": f.apiRoot() + "/repos/octocat/repo-001/actions/runs/1004", "html_url": "https://example.test/runs/1004"})
		default:
			writeMsg(w, http.StatusNotFound, "Not Found")
		}
	case sub == "releases" && m == http.MethodGet:
		f.writePage(w, r, f.releases(), "")
	case sub == "releases" && m == http.MethodPost:
		rel := release(4, rec.Body["tag_name"].(string), rec.Body["draft"].(bool), rec.Body["prerelease"].(bool))
		writeJSON(w, http.StatusCreated, rel)
	case len(rest) == 3 && rest[0] == "releases" && rest[1] == "tags":
		for _, it := range f.releases() {
			rel := it.(map[string]any)
			if rel["tag_name"] == rest[2] && rel["draft"] == false {
				writeJSON(w, http.StatusOK, rel)
				return
			}
		}
		writeMsg(w, http.StatusNotFound, "Not Found")
	case len(rest) == 2 && rest[0] == "releases" && m == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeMsg(w, http.StatusNotFound, "Not Found")
	}
}

// --- GitHub App ------------------------------------------------------------------

func (f *fakeGitHub) serveApp(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case path == "/app" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"id": 4242, "slug": "trove-app", "name": "Trove App",
			"html_url": "https://example.test/apps/trove-app", "owner": map[string]any{"login": "octo-org"}})
	case path == "/app/installations/"+testInstID+"/access_tokens" && r.Method == http.MethodPost:
		f.mu.Lock()
		f.exchanges++
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"token": testInstToken,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "permissions": map[string]any{"contents": "read"}})
	case path == "/app/installations/"+testInstID && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"id": 777, "account": map[string]any{"id": 100, "login": "octo-org", "type": "Organization"}})
	default:
		writeMsg(w, http.StatusNotFound, "Not Found")
	}
}

// verifyJWT checks an App JWT's RS256 signature and claims.
func (f *fakeGitHub) verifyJWT(tok string) bool {
	if f.appPub == nil {
		return false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	hdr, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	claimsRaw, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	var h map[string]any
	if json.Unmarshal(hdr, &h) != nil || h["alg"] != "RS256" {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(f.appPub, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	var c map[string]any
	if json.Unmarshal(claimsRaw, &c) != nil {
		return false
	}
	iat, _ := c["iat"].(float64)
	exp, _ := c["exp"].(float64)
	now := float64(time.Now().Unix())
	if c["iss"] != float64(4242) || iat > now || exp <= now || exp-iat > 600 {
		f.t.Errorf("bad JWT claims: %v", c)
		return false
	}
	f.mu.Lock()
	f.lastJWTClaims = c
	f.mu.Unlock()
	return true
}

// --- OAuth device flow / refresh ------------------------------------------------

func (f *fakeGitHub) deviceCode(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, Path: r.URL.Path, Query: r.PostForm, Header: r.Header.Clone()})
	f.mu.Unlock()
	if r.Header.Get("Accept") != "application/json" {
		// Without Accept: application/json GitHub answers form-encoded.
		w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
		_, _ = io.WriteString(w, "device_code=dc")
		return
	}
	if r.PostForm.Get("client_id") != testClientID {
		writeJSON(w, http.StatusOK, map[string]any{"error": "incorrect_client_credentials", "error_description": "The client_id is not valid."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device_code": "dc-123", "user_code": "ABCD-1234",
		"verification_uri": f.webRoot() + "/login/device", "expires_in": 900, "interval": 1})
}

func (f *fakeGitHub) accessToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, Path: r.URL.Path, Query: r.PostForm, Header: r.Header.Clone()})
	f.mu.Unlock()
	switch r.PostForm.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		f.mu.Lock()
		f.devicePolls++
		pending := f.devicePolls <= f.devicePending
		f.tokens[testDeviceToken] = true
		f.mu.Unlock()
		if pending {
			// GitHub reports pending authorizations with HTTP 200.
			writeJSON(w, http.StatusOK, map[string]any{"error": "authorization_pending"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": testDeviceToken, "token_type": "bearer",
			"scope": "repo,gist", "refresh_token": testRefreshToken, "expires_in": 28800})
	case "refresh_token":
		if r.PostForm.Get("refresh_token") != testRefreshToken || r.PostForm.Get("client_id") != testClientID {
			writeJSON(w, http.StatusOK, map[string]any{"error": "bad_refresh_token", "error_description": "The refresh token passed is incorrect or expired."})
			return
		}
		f.mu.Lock()
		f.tokens[testRefreshed] = true
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"access_token": testRefreshed, "token_type": "bearer", "scope": "",
			"refresh_token": "ghr_ROTATEDrefreshABCDEFGHIJKLMNOPQRSTUVWX", "expires_in": 28800})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
	}
}

// --- second origin -----------------------------------------------------------------

func (f *fakeGitHub) serveBlob(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.blobHits++
	f.blobAuth = append(f.blobAuth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(r.URL.Path, "/logs/"):
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "2024-01-01T00:00:00Z log line for job %s\n", strings.TrimPrefix(r.URL.Path, "/logs/"))
	case r.URL.Path == "/raw/a-big.go":
		_, _ = io.WriteString(w, "package big // full content")
	case r.URL.Path == "/raw/b.txt":
		_, _ = io.WriteString(w, "small")
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) blobAuthHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.blobAuth...)
}
