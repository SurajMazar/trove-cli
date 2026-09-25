package gitlab

import (
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

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

const (
	testPAT   = "glpat-TESTtokenABCDEFGHIJ1234"
	testOAuth = "oauth-access-TOKEN-1234567890abcdef"
)

// fakeProjectNamespaces spreads the fixture projects across a personal
// namespace, a group and nested subgroups.
var fakeProjectNamespaces = []string{"alice", "acme", "acme/platform", "acme/platform/infra"}

const fakeProjectCount = 260

type recorded struct {
	Method     string
	RequestURI string
	RawPath    string
	Header     http.Header
	Body       []byte
}

func (r recorded) query() url.Values {
	u, _ := url.ParseRequestURI(r.RequestURI)
	return u.Query()
}

func (r recorded) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("request body is not JSON: %v (%q)", err, r.Body)
	}
	return m
}

// fakeGitLab is an in-memory GitLab REST API v4.
type fakeGitLab struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	pat        string
	oauthToken string
	requests   []recorded
	handlers   map[string]http.HandlerFunc
	projects   []map[string]any
}

func newFake(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{t: t, pat: testPAT, oauthToken: testOAuth, handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	for i := 1; i <= fakeProjectCount; i++ {
		ns := fakeProjectNamespaces[i%len(fakeProjectNamespaces)]
		f.projects = append(f.projects, f.project(int64(i), ns, fmt.Sprintf("proj-%03d", i)))
	}
	return f
}

func (f *fakeGitLab) project(id int64, ns, path string) map[string]any {
	full := ns + "/" + path
	vis := []string{"private", "internal", "public"}[id%3]
	return map[string]any{
		"id": id, "name": strings.ToUpper(path[:1]) + path[1:], "path": path, "path_with_namespace": full,
		"description": "project " + path, "default_branch": "main", "visibility": vis,
		"archived": id%50 == 0, "empty_repo": false, "star_count": id, "forks_count": 1, "open_issues_count": 2,
		"created_at": "2024-01-02T03:04:05.000Z", "last_activity_at": "2025-06-07T08:09:10.123Z",
		"web_url":          f.srv.URL + "/" + full,
		"http_url_to_repo": f.srv.URL + "/" + full + ".git",
		"ssh_url_to_repo":  "git@" + strings.TrimPrefix(f.srv.URL, "http://") + ":" + full + ".git",
		"topics":           []string{"go"},
		"namespace":        map[string]any{"id": 1, "full_path": ns, "kind": map[bool]string{true: "user", false: "group"}[ns == "alice"]},
	}
}

// handle registers a handler for "METHOD /escaped/path" (the path exactly as
// sent on the wire, so %2F must be preserved by the client to match).
func (f *fakeGitLab) handle(pattern string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[pattern] = h
}

// json registers a handler replying with a fixed JSON value.
func (f *fakeGitLab) json(pattern string, status int, v any) {
	f.handle(pattern, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, status, v) })
}

func (f *fakeGitLab) recorded(method, escapedPath string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.Method == method && r.RawPath == escapedPath {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGitLab) last(t *testing.T, method, escapedPath string) recorded {
	t.Helper()
	rs := f.recorded(method, escapedPath)
	if len(rs) == 0 {
		t.Fatalf("no %s %s request was made; got %v", method, escapedPath, f.paths())
	}
	return rs[len(rs)-1]
}

func (f *fakeGitLab) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		out = append(out, r.Method+" "+r.RequestURI)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitLab) authorized(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tok := r.Header.Get("PRIVATE-TOKEN"); tok != "" {
		return tok == f.pat && r.Header.Get("Authorization") == ""
	}
	return r.Header.Get("Authorization") == "Bearer "+f.oauthToken
}

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := r.URL.EscapedPath()
	f.mu.Lock()
	f.requests = append(f.requests, recorded{Method: r.Method, RequestURI: r.RequestURI, RawPath: path, Header: r.Header.Clone(), Body: body})
	h := f.handlers[r.Method+" "+path]
	f.mu.Unlock()
	r.Body = io.NopCloser(strings.NewReader(string(body)))

	// OAuth endpoints live on the web host and take no API token.
	if strings.HasPrefix(path, "/oauth/") {
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h(w, r)
		return
	}
	if !f.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "401 Unauthorized"})
		return
	}
	if h != nil {
		h(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && path == "/api/v4/user":
		writeJSON(w, 200, map[string]any{"id": 42, "username": "alice", "name": "Alice", "email": "alice@example.com",
			"web_url": f.srv.URL + "/alice", "avatar_url": f.srv.URL + "/a.png"})
	case r.Method == http.MethodGet && path == "/api/v4/projects":
		// Offset pagination advertised via the Link header only.
		f.paginate(w, r, f.projects, true)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v4/groups/") && strings.HasSuffix(path, "/projects"):
		group, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/api/v4/groups/"), "/projects"))
		var items []map[string]any
		for _, p := range f.projects {
			ns := p["namespace"].(map[string]any)["full_path"].(string)
			if ns == group || strings.HasPrefix(ns, group+"/") {
				items = append(items, p)
			}
		}
		if items == nil {
			writeJSON(w, 404, map[string]any{"message": "404 Group Not Found"})
			return
		}
		// Offset pagination advertised via X-Next-Page only.
		f.paginate(w, r, items, false)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v4/projects/"):
		idOrPath, _ := url.PathUnescape(strings.TrimPrefix(path, "/api/v4/projects/"))
		for _, p := range f.projects {
			if strconv.FormatInt(p["id"].(int64), 10) == idOrPath || p["path_with_namespace"] == idOrPath {
				writeJSON(w, 200, p)
				return
			}
		}
		writeJSON(w, 404, map[string]any{"message": "404 Project Not Found"})
	default:
		writeJSON(w, 404, map[string]any{"error": "404 Not Found"})
	}
}

func (f *fakeGitLab) paginate(w http.ResponseWriter, r *http.Request, items []map[string]any, link bool) {
	q := r.URL.Query()
	if q.Get("archived") == "false" {
		var kept []map[string]any
		for _, it := range items {
			if !it["archived"].(bool) {
				kept = append(kept, it)
			}
		}
		items = kept
	}
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per <= 0 {
		per = 20
	}
	if per > 100 {
		per = 100
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * per
	end := min(start+per, len(items))
	if start > len(items) {
		start = end
	}
	w.Header().Set("X-Total", strconv.Itoa(len(items)))
	w.Header().Set("X-Page", strconv.Itoa(page))
	w.Header().Set("X-Per-Page", strconv.Itoa(per))
	if end < len(items) {
		if link {
			nq := r.URL.Query()
			nq.Set("page", strconv.Itoa(page+1))
			next := f.srv.URL + r.URL.EscapedPath() + "?" + nq.Encode()
			w.Header().Set("Link", `<`+next+`>; rel="next", <`+f.srv.URL+r.URL.EscapedPath()+`?page=1>; rel="first"`)
		} else {
			w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
	} else if !link {
		w.Header().Set("X-Next-Page", "")
	}
	writeJSON(w, 200, items[start:end])
}

type providerOpt func(*forge.Account)

func withHost(h string) providerOpt { return func(a *forge.Account) { a.Host = h } }

func withStore(s auth.Store) providerOpt { return func(a *forge.Account) { a.Credentials = s } }

func withClientID(id string) providerOpt { return func(a *forge.Account) { a.ClientID = id } }

// newProvider builds a provider pointed at the fake with a PAT by default.
func (f *fakeGitLab) newProvider(t *testing.T, opts ...providerOpt) *provider {
	t.Helper()
	acct := forge.Account{
		Name: "gl", Type: DriverType, Host: "gitlab.com",
		APIURL: f.srv.URL + "/api/v4", WebURL: f.srv.URL,
		Credentials: auth.TokenStore(testPAT),
	}
	for _, o := range opts {
		o(&acct)
	}
	p, err := NewDriver().New(acct)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*provider)
}

func oauthStore(token, refresh string, expiry time.Time) *auth.MemoryStore {
	return auth.NewMemoryStore(auth.Credential{
		Kind: auth.KindOAuth, Token: token, RefreshToken: refresh, Expiry: expiry,
		Username: "client-123", Scopes: []string{"api"},
	})
}
