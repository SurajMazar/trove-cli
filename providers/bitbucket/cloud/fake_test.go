package cloud

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

	"gopkg.in/yaml.v3"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

const (
	testEmail       = "dev@example.com"
	testAPIToken    = "ATATT-good-secret-token"
	testAccessToken = "ws-access-token-secret"
	testOAuthToken  = "oauth-access-token-1"
	testUserUUID    = "{11111111-2222-3333-4444-555555555555}"
)

type recorded struct {
	Method string
	Path   string // escaped path
	Query  url.Values
	Body   []byte
	Header http.Header
}

// fakeBitbucket is an httptest-backed fake of the Bitbucket Cloud REST API
// 2.0 plus the OAuth token endpoint (on the same server, as the "web" host).
type fakeBitbucket struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []recorded
	// bearer tokens accepted, mapped to their kind ("access", "oauth").
	bearer map[string]string
	// workspaces returned by /user/workspaces, in order.
	workspaces []string
	repos      map[string][]string // workspace -> slugs
	// handlers overrides/extends routes, keyed by "METHOD /escaped/path".
	handlers map[string]http.HandlerFunc
	// token is the OAuth token endpoint handler.
	token http.HandlerFunc
}

func newFake(t *testing.T) *fakeBitbucket {
	t.Helper()
	f := &fakeBitbucket{
		t:          t,
		bearer:     map[string]string{testAccessToken: "access", testOAuthToken: "oauth"},
		workspaces: []string{"acme", "empty", "solo"},
		repos:      map[string][]string{"acme": nil, "empty": nil, "solo": {"one", "two", "three"}},
		handlers:   map[string]http.HandlerFunc{},
	}
	for i := 1; i <= 260; i++ {
		f.repos["acme"] = append(f.repos["acme"], fmt.Sprintf("repo-%03d", i))
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBitbucket) apiURL() string { return f.srv.URL + "/2.0" }

func (f *fakeBitbucket) handle(key string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[key] = h
}

func (f *fakeBitbucket) calls(method, path string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeBitbucket) last(method, path string) recorded {
	f.t.Helper()
	cs := f.calls(method, path)
	if len(cs) == 0 {
		f.t.Fatalf("no %s %s request was made", method, path)
	}
	return cs[len(cs)-1]
}

func (f *fakeBitbucket) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := r.URL.EscapedPath()
	f.mu.Lock()
	f.requests = append(f.requests, recorded{Method: r.Method, Path: path, Query: r.URL.Query(), Body: body, Header: r.Header.Clone()})
	h := f.handlers[r.Method+" "+path]
	tok := f.token
	f.mu.Unlock()
	r.Body = io.NopCloser(strings.NewReader(string(body)))

	if path == "/site/oauth2/access_token" {
		if tok == nil {
			writeError(w, http.StatusNotFound, "no token endpoint")
			return
		}
		tok(w, r)
		return
	}
	identity := f.authenticate(r)
	if identity == "" {
		writeError(w, http.StatusUnauthorized, "Invalid credentials")
		return
	}
	if h != nil {
		h(w, r)
		return
	}
	f.builtin(w, r, identity)
}

// authenticate returns "basic", "oauth", "access" or "" (rejected).
func (f *fakeBitbucket) authenticate(r *http.Request) string {
	if u, p, ok := r.BasicAuth(); ok {
		if u == testEmail && p == testAPIToken {
			return "basic"
		}
		return ""
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.bearer[tok]
	}
	return ""
}

func (f *fakeBitbucket) builtin(w http.ResponseWriter, r *http.Request, identity string) {
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/2.0/"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/2.0/user":
		if identity == "access" {
			// Access tokens have no user.
			writeError(w, http.StatusUnauthorized, "Access tokens cannot access this resource")
			return
		}
		if identity == "oauth" {
			w.Header().Set("X-OAuth-Scopes", "repository account, pullrequest")
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type": "user", "uuid": testUserUUID, "account_id": "557058:abc", "username": "jdoe",
			"nickname": "jdoe", "display_name": "Jane Doe",
			"links": map[string]any{"html": map[string]string{"href": "https://bitbucket.org/jdoe/"},
				"avatar": map[string]string{"href": "https://avatar.example/jdoe.png"}},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/2.0/user/emails":
		writeJSON(w, http.StatusOK, map[string]any{"values": []map[string]any{
			{"email": "other@example.com", "is_primary": false},
			{"email": testEmail, "is_primary": true, "is_confirmed": true},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/2.0/user/workspaces":
		var items []any
		for _, ws := range f.workspaces {
			items = append(items, map[string]any{"type": "workspace_access", "administrator": ws == "solo",
				"workspace": map[string]any{"type": "workspace_base", "slug": ws, "uuid": "{ws-" + ws + "}",
					"links": map[string]any{"html": map[string]string{"href": "https://bitbucket.org/" + ws + "/"}}}})
		}
		f.page(w, r, items)
	case r.Method == http.MethodGet && len(segs) == 2 && segs[0] == "repositories":
		slugs, ok := f.repos[segs[1]]
		if !ok {
			writeError(w, http.StatusNotFound, "No workspace with identifier '"+segs[1]+"'.")
			return
		}
		var items []any
		for _, s := range slugs {
			items = append(items, f.repoJSON(segs[1], s))
		}
		f.page(w, r, items)
	case r.Method == http.MethodGet && len(segs) == 3 && segs[0] == "repositories":
		for _, s := range f.repos[segs[1]] {
			if s == segs[2] {
				writeJSON(w, http.StatusOK, f.repoJSON(segs[1], s))
				return
			}
		}
		writeError(w, http.StatusNotFound, "Repository "+segs[1]+"/"+segs[2]+" not found")
	default:
		writeError(w, http.StatusNotFound, "Resource not found")
	}
}

func (f *fakeBitbucket) repoJSON(ws, slug string) map[string]any {
	return map[string]any{
		"type": "repository", "uuid": "{repo-" + ws + "-" + slug + "}", "name": strings.ToUpper(slug), "slug": slug,
		"full_name": ws + "/" + slug, "is_private": strings.HasSuffix(slug, "1"), "description": "desc " + slug,
		"mainbranch": map[string]string{"name": "main", "type": "branch"}, "language": "go",
		"created_on": "2024-01-02T03:04:05.000000+00:00", "updated_on": "2025-01-02T03:04:05.000000+00:00",
		"workspace": map[string]string{"slug": ws},
		"links": map[string]any{
			"self": map[string]string{"href": f.apiURL() + "/repositories/" + ws + "/" + slug},
			"html": map[string]string{"href": "https://bitbucket.org/" + ws + "/" + slug},
			"clone": []map[string]string{
				{"name": "https", "href": "https://jdoe@bitbucket.org/" + ws + "/" + slug + ".git"},
				{"name": "ssh", "href": "git@ssh.bitbucket.org:" + ws + "/" + slug + ".git"},
			},
		},
	}
}

// page serves items with Bitbucket's paged envelope and an absolute "next".
func (f *fakeBitbucket) page(w http.ResponseWriter, r *http.Request, items []any) {
	q := r.URL.Query()
	pagelen, _ := strconv.Atoi(q.Get("pagelen"))
	if pagelen <= 0 {
		pagelen = 10
	}
	if pagelen > 100 {
		writeError(w, http.StatusBadRequest, "Invalid pagelen")
		return
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	start := (page - 1) * pagelen
	end := start + pagelen
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	env := map[string]any{"values": items[start:end], "page": page, "pagelen": pagelen, "size": len(items)}
	if end < len(items) {
		q.Set("page", strconv.Itoa(page+1))
		env["next"] = "http://" + r.Host + r.URL.EscapedPath() + "?" + q.Encode()
	}
	writeJSON(w, http.StatusOK, env)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"message": msg}})
}

// --- provider construction ------------------------------------------------------

type acctOpt func(*forge.Account)

func withCred(c auth.Credential) acctOpt {
	return func(a *forge.Account) { a.Credentials = auth.NewMemoryStore(c) }
}

func withMethod(m auth.Method) acctOpt { return func(a *forge.Account) { a.AuthMethod = m } }

// withYAML makes Account.Decode decode the given YAML account block.
func withYAML(doc string) acctOpt {
	return func(a *forge.Account) {
		a.Decode = func(v any) error { return yaml.Unmarshal([]byte(doc), v) }
	}
}

func basicCred() auth.Credential {
	return auth.Credential{Kind: auth.KindBasic, Username: testEmail, Token: testAPIToken}
}

func (f *fakeBitbucket) provider(t *testing.T, opts ...acctOpt) *Provider {
	t.Helper()
	acct := forge.Account{
		Name: "bb", Type: DriverType, APIURL: f.apiURL(), WebURL: f.srv.URL,
		AuthMethod: auth.MethodBasic, Username: testEmail, Credentials: auth.NewMemoryStore(basicCred()),
	}
	for _, o := range opts {
		o(&acct)
	}
	p, err := NewDriver().New(acct)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*Provider)
}

func decodeBody(t *testing.T, rec recorded) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body, &m); err != nil {
		t.Fatalf("request body is not JSON: %v (%q)", err, rec.Body)
	}
	return m
}

func tokenCred() auth.Credential {
	return auth.Credential{Kind: auth.KindToken, Token: testAccessToken}
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }
