package custom

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

	"github.com/SurajMazar/trove-cli/internal/domain"
)

// fakeTFP is an in-memory Trove Forge Protocol v1 server. It is mounted under
// /api so tests also cover api_base_url values with a path.
type fakeTFP struct {
	t   *testing.T
	srv *httptest.Server

	// strategy selects the pagination style the fake speaks.
	strategy string
	// nextPageHeader makes the page strategy emit X-Next-Page.
	nextPageHeader bool
	// linkNext, when set, replaces the Link rel="next" target.
	linkNext string
	// authHeader/authValue is the credential the fake accepts.
	authHeader, authValue string
	// handler, when set, handles every request instead of the router.
	handler http.HandlerFunc

	mu         sync.Mutex
	requests   []recorded
	repos      []domain.Repository
	pulls      map[string][]domain.PullRequest
	issues     map[string][]domain.Issue
	pipelines  map[string][]domain.Pipeline
	releases   map[string][]domain.Release
	namespaces []domain.Namespace
	summary    string
}

type recorded struct {
	Method     string
	Path       string // decoded
	EscPath    string // as sent on the wire
	RequestURI string
	Query      url.Values
	Header     http.Header
	Body       []byte
}

const (
	fakeToken     = "tfp-s3cr3t-token-value"
	nestedRepo    = "acme/platform/tools/deployer"
	fixtureRepos  = 260
	fixtureNSPath = "acme/platform"
)

var fixtureNamespaces = []string{"acme", "acme/platform", "acme/platform/tools", "solo"}

func newFakeTFP(t *testing.T, strategy string) *fakeTFP {
	t.Helper()
	f := &fakeTFP{
		t: t, strategy: strategy,
		authHeader: "Authorization", authValue: "Bearer " + fakeToken,
		pulls: map[string][]domain.PullRequest{}, issues: map[string][]domain.Issue{},
		pipelines: map[string][]domain.Pipeline{}, releases: map[string][]domain.Release{},
		summary: `{"repositories": 261, "open_pull_requests": 3}`,
	}
	vis := []domain.Visibility{domain.VisibilityPublic, domain.VisibilityPrivate, domain.VisibilityInternal}
	for i := 0; i < fixtureRepos; i++ {
		ns := fixtureNamespaces[i%len(fixtureNamespaces)]
		name := fmt.Sprintf("repo-%03d", i)
		r := domain.Repository{
			ID: strconv.Itoa(i + 1), Name: name, Namespace: ns, Visibility: vis[i%3],
			Archived: i%50 == 0, DefaultBranch: "main",
			// Servers must not be trusted for these two fields.
			Provider: "spoofed", ProviderType: "github",
		}
		if i%2 == 0 {
			r.URLs = domain.RepositoryURLs{
				Web:   "https://forge.internal/" + ns + "/" + name,
				HTTPS: "https://forge.internal/git/" + ns + "/" + name + ".git",
				SSH:   "ssh://git@forge.internal:2222/" + ns + "/" + name + ".git",
			}
		}
		f.repos = append(f.repos, r)
	}
	// A repository identified only by full_name (namespace/name omitted).
	f.repos = append(f.repos, domain.Repository{ID: "999", FullName: nestedRepo, Visibility: "", DefaultBranch: "main"})

	f.pulls[nestedRepo] = []domain.PullRequest{
		{ID: "p1", Number: 1, Title: "Add deploy", State: domain.PullRequestOpen, Author: "alice", SourceBranch: "feat", TargetBranch: "main"},
		{ID: "p2", Number: 2, Title: "Old", State: domain.PullRequestMerged, Author: "bob", SourceBranch: "old", TargetBranch: "main"},
		{ID: "p3", Number: 3, Title: "Release", State: domain.PullRequestOpen, Author: "bob", SourceBranch: "rel", TargetBranch: "release"},
	}
	f.issues[nestedRepo] = []domain.Issue{
		{ID: "i1", Number: 1, Title: "Bug", State: domain.IssueOpen, Labels: []string{"a", "b"}, Author: "alice"},
		{ID: "i2", Number: 2, Title: "Bug only a", State: domain.IssueOpen, Labels: []string{"a"}},
		{ID: "i3", Number: 3, Title: "Closed", State: domain.IssueClosed, Labels: []string{"a", "b"}},
	}
	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	f.pipelines[nestedRepo] = []domain.Pipeline{
		{ID: "100", Number: 7, Status: domain.PipelineFailed, Ref: "main", StartedAt: t0, FinishedAt: t0.Add(90 * time.Second),
			Jobs: []domain.PipelineJob{{ID: "j1", Name: "build", Status: domain.PipelineSuccess}, {ID: "j2", Name: "test", Status: "exploded"}}},
		{ID: "101", Number: 8, Status: domain.PipelineSuccess, Ref: "feature"},
		{ID: "102", Number: 9, Status: "weird", Ref: "main"},
	}
	f.releases[nestedRepo] = []domain.Release{{ID: "r1", Tag: "v1.0/rc", Name: "RC"}, {ID: "r2", Tag: "v0.9"}}
	f.namespaces = []domain.Namespace{
		{ID: "n1", Name: "acme", FullPath: "acme", Type: domain.NamespaceOrganization},
		{ID: "n2", Name: "platform", FullPath: "acme/platform", Type: domain.NamespaceGroup, ParentPath: "acme"},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTFP) apiURL() string { return f.srv.URL + "/api" }

func (f *fakeTFP) recorded() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

func (f *fakeTFP) last() recorded {
	rs := f.recorded()
	if len(rs) == 0 {
		f.t.Fatal("no request recorded")
	}
	return rs[len(rs)-1]
}

func (f *fakeTFP) reset() {
	f.mu.Lock()
	f.requests = nil
	f.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (f *fakeTFP) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{
		Method: r.Method, Path: r.URL.Path, EscPath: r.URL.EscapedPath(), RequestURI: r.RequestURI,
		Query: r.URL.Query(), Header: r.Header.Clone(), Body: body,
	})
	f.mu.Unlock()
	if f.handler != nil {
		f.handler(w, r)
		return
	}
	if r.Header.Get(f.authHeader) != f.authValue {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "bad credentials")
		return
	}
	esc, ok := strings.CutPrefix(r.URL.EscapedPath(), "/api/v1/")
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	// Split on the escaped path so %2F stays inside a segment.
	var seg []string
	for _, s := range strings.Split(esc, "/") {
		u, err := url.PathUnescape(s)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid", "bad escape")
			return
		}
		seg = append(seg, u)
	}
	f.route(w, r, seg, body)
}

func (f *fakeTFP) route(w http.ResponseWriter, r *http.Request, seg []string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(seg) == 1 && seg[0] == "user":
		writeJSON(w, 200, domain.User{ID: "u1", Username: "alice", Name: "Alice"})
	case len(seg) == 1 && seg[0] == "summary":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.summary)
	case len(seg) == 1 && seg[0] == "namespaces":
		paginate(f, w, r, f.namespaces)
	case len(seg) == 2 && seg[0] == "namespaces":
		for _, n := range f.namespaces {
			if n.FullPath == seg[1] {
				writeJSON(w, 200, n)
				return
			}
		}
		writeErr(w, 404, "not_found", "namespace not found")
	case len(seg) == 1 && seg[0] == "repositories" && r.Method == http.MethodGet:
		q := r.URL.Query()
		var out []domain.Repository
		for _, repo := range f.repos {
			ns := repo.Namespace
			if ns == "" {
				ns = nestedRepo[:strings.LastIndex(nestedRepo, "/")]
			}
			if want := q.Get("namespace"); want != "" && ns != want && !strings.HasPrefix(ns, want+"/") {
				continue
			}
			if repo.Archived && q.Get("include_archived") != "true" {
				continue
			}
			out = append(out, repo)
		}
		paginate(f, w, r, out)
	case len(seg) == 1 && seg[0] == "repositories" && r.Method == http.MethodPost:
		var req struct {
			Name, Namespace, Visibility string
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Name == "" {
			writeErr(w, 422, "invalid", "name is required")
			return
		}
		if f.findRepo(req.Namespace+"/"+req.Name) >= 0 {
			writeErr(w, 409, "conflict", "repository already exists")
			return
		}
		repo := domain.Repository{ID: "new", Name: req.Name, Namespace: req.Namespace, Visibility: domain.Visibility(req.Visibility)}
		f.repos = append(f.repos, repo)
		writeJSON(w, 201, repo)
	case seg[0] == "repositories" && len(seg) >= 2:
		f.routeRepo(w, r, seg[1], seg[2:], body)
	default:
		writeErr(w, 404, "not_found", "no such endpoint")
	}
}

func (f *fakeTFP) findRepo(full string) int {
	for i, r := range f.repos {
		name := r.FullName
		if r.Namespace != "" {
			name = r.Namespace + "/" + r.Name
		}
		if name == full {
			return i
		}
	}
	return -1
}

func (f *fakeTFP) routeRepo(w http.ResponseWriter, r *http.Request, full string, rest []string, body []byte) {
	idx := f.findRepo(full)
	if idx < 0 {
		writeErr(w, 404, "not_found", "repository "+full+" not found")
		return
	}
	q := r.URL.Query()
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeJSON(w, 200, f.repos[idx])
	case len(rest) == 0 && r.Method == http.MethodDelete:
		f.repos = append(f.repos[:idx], f.repos[idx+1:]...)
		w.WriteHeader(204)
	case len(rest) == 1 && rest[0] == "pull-requests" && r.Method == http.MethodGet:
		var out []domain.PullRequest
		for _, pr := range f.pulls[full] {
			if s := q.Get("state"); s != "" && s != "all" && string(pr.State) != s {
				continue
			}
			out = append(out, pr)
		}
		paginate(f, w, r, out)
	case len(rest) == 1 && rest[0] == "pull-requests" && r.Method == http.MethodPost:
		var pr domain.PullRequest
		_ = json.Unmarshal(body, &pr)
		pr.Number = len(f.pulls[full]) + 1
		pr.ID = "new"
		pr.State = domain.PullRequestOpen
		f.pulls[full] = append(f.pulls[full], pr)
		writeJSON(w, 201, pr)
	case len(rest) >= 2 && rest[0] == "pull-requests":
		n, _ := strconv.Atoi(rest[1])
		for _, pr := range f.pulls[full] {
			if pr.Number != n {
				continue
			}
			switch {
			case len(rest) == 2 && r.Method == http.MethodGet:
				writeJSON(w, 200, pr)
			case len(rest) == 3 && (rest[2] == "merge" || rest[2] == "close") && r.Method == http.MethodPost:
				w.WriteHeader(204)
			default:
				writeErr(w, 404, "not_found", "no such endpoint")
			}
			return
		}
		writeErr(w, 404, "not_found", "pull request not found")
	case len(rest) == 1 && rest[0] == "issues" && r.Method == http.MethodGet:
		var out []domain.Issue
		for _, is := range f.issues[full] {
			if s := q.Get("state"); s != "" && s != "all" && string(is.State) != s {
				continue
			}
			out = append(out, is) // labels deliberately not filtered: the client must.
		}
		paginate(f, w, r, out)
	case len(rest) == 1 && rest[0] == "issues" && r.Method == http.MethodPost:
		var is domain.Issue
		_ = json.Unmarshal(body, &is)
		is.Number, is.ID, is.State = 10, "new", domain.IssueOpen
		writeJSON(w, 201, is)
	case len(rest) == 2 && rest[0] == "issues":
		n, _ := strconv.Atoi(rest[1])
		for i, is := range f.issues[full] {
			if is.Number != n {
				continue
			}
			if r.Method == http.MethodPatch {
				var req struct{ State domain.IssueState }
				_ = json.Unmarshal(body, &req)
				f.issues[full][i].State = req.State
				is = f.issues[full][i]
			}
			writeJSON(w, 200, is)
			return
		}
		writeErr(w, 404, "not_found", "issue not found")
	case len(rest) == 1 && rest[0] == "pipelines":
		var out []domain.Pipeline
		for _, pl := range f.pipelines[full] {
			pl.Jobs = nil
			out = append(out, pl)
		}
		paginate(f, w, r, out)
	case len(rest) == 2 && rest[0] == "pipelines":
		for _, pl := range f.pipelines[full] {
			if pl.ID == rest[1] {
				writeJSON(w, 200, pl)
				return
			}
		}
		writeErr(w, 404, "not_found", "pipeline not found")
	case len(rest) == 1 && rest[0] == "releases":
		paginate(f, w, r, f.releases[full])
	case len(rest) == 2 && rest[0] == "releases":
		for _, rel := range f.releases[full] {
			if rel.Tag == rest[1] {
				writeJSON(w, 200, rel)
				return
			}
		}
		writeErr(w, 404, "not_found", "release not found")
	default:
		writeErr(w, 404, "not_found", "no such endpoint")
	}
}

// paginate serves items using f.strategy. Called with f.mu held.
func paginate[T any](f *fakeTFP, w http.ResponseWriter, r *http.Request, items []T) {
	if items == nil {
		items = []T{}
	}
	q := r.URL.Query()
	perPage := 30
	if v, err := strconv.Atoi(q.Get("per_page")); err == nil && v > 0 {
		perPage = min(v, 100)
	}
	window := func(start int) ([]T, int) {
		start = min(start, len(items))
		end := min(start+perPage, len(items))
		return items[start:end], end
	}
	switch f.strategy {
	case paginateLink, paginatePage:
		page := 1
		if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
			page = v
		}
		out, end := window((page - 1) * perPage)
		if f.strategy == paginateLink && end < len(items) {
			next := f.linkNext
			if next == "" {
				nq := r.URL.Query()
				nq.Set("page", strconv.Itoa(page+1))
				u := *r.URL
				u.RawQuery = nq.Encode()
				next = "http://" + r.Host + u.RequestURI()
			}
			w.Header().Add("Link", fmt.Sprintf(`<%s>; rel="next", <http://%s/api/v1/first>; rel="first"`, next, r.Host))
		}
		if f.strategy == paginatePage && f.nextPageHeader {
			if end < len(items) {
				w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
			} else {
				w.Header()["X-Next-Page"] = []string{""}
			}
		}
		writeJSON(w, 200, out)
	case paginateCursor:
		start := 0
		if c := q.Get("cursor"); c != "" {
			n, err := strconv.Atoi(strings.TrimPrefix(c, "off-"))
			if err != nil {
				writeErr(w, 400, "invalid", "bad cursor")
				return
			}
			start = n
		}
		out, end := window(start)
		next := ""
		if end < len(items) {
			next = "off-" + strconv.Itoa(end)
		}
		writeJSON(w, 200, map[string]any{"items": out, "next_cursor": next})
	default:
		writeJSON(w, 200, items)
	}
}
