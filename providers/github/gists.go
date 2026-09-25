package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
	"github.com/SurajMazar/trove-cli/internal/httpx"
)

type apiGistFile struct {
	Filename  string `json:"filename"`
	RawURL    string `json:"raw_url"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
	Size      int64  `json:"size"`
}

type apiGist struct {
	ID          string                 `json:"id"`
	Description *string                `json:"description"`
	Public      bool                   `json:"public"`
	Owner       *apiOwner              `json:"owner"`
	HTMLURL     string                 `json:"html_url"`
	Files       map[string]apiGistFile `json:"files"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
}

var gistIDRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

func (p *Provider) toSnippet(g apiGist) domain.Snippet {
	vis := domain.VisibilityPrivate // secret gists are unlisted, reachable by URL
	if g.Public {
		vis = domain.VisibilityPublic
	}
	names := make([]string, 0, len(g.Files))
	for n := range g.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	out := domain.Snippet{
		ID: g.ID, Description: str(g.Description), Visibility: vis, WebURL: g.HTMLURL,
		CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt, Term: p.meta.Terms.Snippet,
	}
	if len(names) > 0 {
		// Gists have no title; GitHub displays the first file name.
		out.Title = names[0]
	}
	if g.Owner != nil {
		out.Owner = g.Owner.Login
	}
	for _, n := range names {
		f := g.Files[n]
		out.Files = append(out.Files, domain.SnippetFile{Name: orDefault(f.Filename, n), Content: f.Content, RawURL: f.RawURL})
	}
	return out
}

// ListSnippets implements forge.SnippetProvider (the user's gists; file
// contents are not included in listings).
func (p *Provider) ListSnippets(ctx context.Context, opts forge.ListOptions) ([]domain.Snippet, error) {
	return paginate(ctx, p, listSpec[apiGist]{
		request: request{op: "list gists", path: "/gists"}, perPage: maxPerPage, limit: opts.Limit,
	}, func(g apiGist) (domain.Snippet, bool) {
		s := p.toSnippet(g)
		for i := range s.Files {
			s.Files[i].Content = ""
		}
		return s, true
	})
}

// GetSnippet implements forge.SnippetProvider. The API truncates file
// content beyond ~1 MB; truncated files are fetched from raw_url, which is
// served from a separate host and needs no credentials.
func (p *Provider) GetSnippet(ctx context.Context, id string) (*domain.Snippet, error) {
	const op = "get gist"
	id = strings.TrimSpace(id)
	if !gistIDRe.MatchString(id) {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "invalid gist ID %q", id)
	}
	var g apiGist
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/gists/" + id, out: &g,
		notFoundMsg: fmt.Sprintf("gist %s not found", id)}); err != nil {
		return nil, err
	}
	for name, f := range g.Files {
		if !f.Truncated || f.RawURL == "" {
			continue
		}
		content, err := p.fetchRaw(ctx, op, f.RawURL)
		if err != nil {
			return nil, err
		}
		f.Content = content
		g.Files[name] = f
	}
	s := p.toSnippet(g)
	return &s, nil
}

// fetchRaw downloads a raw gist file without credentials.
func (p *Provider) fetchRaw(ctx context.Context, op, raw string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", p.errorf(errs.ErrProviderAPI, op, 0, "invalid raw file URL")
	}
	req.Header.Set("User-Agent", httpx.UserAgent())
	resp, err := p.stream.Do(req)
	if err != nil {
		return "", p.transportError(ctx, op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", p.errorf(errs.ErrProviderAPI, op, resp.StatusCode, "downloading gist file failed with HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		if ctxErr := p.ctxErr(ctx, op); ctxErr != nil {
			return "", ctxErr
		}
		return "", p.errorf(errs.ErrProviderAPI, op, 0, "reading gist file failed: %v", err)
	}
	return string(b), nil
}

// CreateSnippet implements forge.SnippetCreator. Gists have a description
// but no title: a title is used as the description, or prefixed to it.
func (p *Provider) CreateSnippet(ctx context.Context, req forge.CreateSnippetRequest) (*domain.Snippet, error) {
	const op = "create gist"
	if len(req.Files) == 0 {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "a gist needs at least one file")
	}
	var public bool
	switch req.Visibility {
	case domain.VisibilityPublic:
		public = true
	case "", domain.VisibilityPrivate:
		public = false // secret gist
	default:
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "gists are public or secret (private); %q is not supported", req.Visibility)
	}
	files := map[string]map[string]string{}
	for _, f := range req.Files {
		name := strings.TrimSpace(f.Name)
		if name == "" || strings.Contains(name, "/") {
			return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "invalid gist file name %q", f.Name)
		}
		if _, dup := files[name]; dup {
			return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "duplicate gist file name %q", name)
		}
		if strings.TrimSpace(f.Content) == "" {
			return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "gist file %q is empty; GitHub rejects empty files", name)
		}
		files[name] = map[string]string{"content": f.Content}
	}
	desc := req.Description
	switch {
	case req.Title != "" && desc == "":
		desc = req.Title
	case req.Title != "" && desc != "":
		desc = req.Title + " - " + desc
	}
	body := map[string]any{"public": public, "files": files}
	if desc != "" {
		body["description"] = desc
	}
	var g apiGist
	if _, err := p.do(ctx, request{op: op, method: http.MethodPost, path: "/gists", body: body, out: &g}); err != nil {
		return nil, err
	}
	s := p.toSnippet(g)
	return &s, nil
}
