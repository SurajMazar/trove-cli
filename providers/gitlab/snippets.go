package gitlab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

// maxSnippetFile bounds how much of one snippet file is read into memory.
const maxSnippetFile = 10 << 20

type glSnippet struct {
	ID          int64      `json:"id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Visibility  string     `json:"visibility"`
	FileName    string     `json:"file_name"` // single-file snippets / pre-13.x
	WebURL      string     `json:"web_url"`
	RawURL      string     `json:"raw_url"`
	Author      *glUserRef `json:"author"`
	CreatedAt   *time.Time `json:"created_at"`
	UpdatedAt   *time.Time `json:"updated_at"`
	Files       []struct {
		Path   string `json:"path"`
		RawURL string `json:"raw_url"`
	} `json:"files"`
}

func (s glSnippet) toDomain() domain.Snippet {
	out := domain.Snippet{
		ID: strconv.FormatInt(s.ID, 10), Title: s.Title, Description: s.Description,
		Visibility: visibility(s.Visibility), Owner: s.Author.name(), WebURL: s.WebURL, Term: "Snippet",
	}
	for _, f := range s.Files {
		out.Files = append(out.Files, domain.SnippetFile{Name: f.Path, RawURL: f.RawURL})
	}
	if len(out.Files) == 0 && s.FileName != "" {
		out.Files = []domain.SnippetFile{{Name: s.FileName, RawURL: s.RawURL}}
	}
	setTime(&out.CreatedAt, s.CreatedAt)
	setTime(&out.UpdatedAt, s.UpdatedAt)
	return out
}

// ListSnippets lists the authenticated user's personal snippets.
func (p *provider) ListSnippets(ctx context.Context, opts forge.ListOptions) ([]domain.Snippet, error) {
	snips, err := list[glSnippet](ctx, p, "list snippets", "/snippets", nil, opts.Limit, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Snippet, 0, len(snips))
	for _, s := range snips {
		out = append(out, s.toDomain())
	}
	return out, nil
}

// GetSnippet returns a personal snippet with its file contents. Snippets are
// Git repositories: each file is read with
// GET /snippets/:id/files/:ref/:file_path/raw, where ref is taken from the
// file's raw_url (".../raw/<ref>/<path>") and falls back to HEAD. Instances
// that predate multi-file snippets are read with GET /snippets/:id/raw.
func (p *provider) GetSnippet(ctx context.Context, id string) (*domain.Snippet, error) {
	op := "get snippet " + id
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return nil, p.invalid(op, "snippet ID %q is not numeric", id)
	}
	var s glSnippet
	if err := p.get(ctx, op, "/snippets/"+id, nil, &s); err != nil {
		return nil, err
	}
	out := s.toDomain()
	if len(s.Files) == 0 {
		content, err := p.raw(ctx, op, "/snippets/"+id+"/raw")
		if err != nil {
			return nil, err
		}
		if len(out.Files) == 0 {
			out.Files = []domain.SnippetFile{{Name: s.Title}}
		}
		out.Files[0].Content = content
		return &out, nil
	}
	for i, f := range s.Files {
		ref := snippetRef(f.RawURL, f.Path)
		content, err := p.raw(ctx, op, "/snippets/"+id+"/files/"+url.PathEscape(ref)+"/"+url.PathEscape(f.Path)+"/raw")
		if errors.Is(err, errs.ErrNotFound) && len(s.Files) == 1 {
			content, err = p.raw(ctx, op, "/snippets/"+id+"/raw")
		}
		if err != nil {
			return nil, err
		}
		out.Files[i].Content = content
	}
	return &out, nil
}

// snippetRef extracts <ref> from a raw URL ending in "/raw/<ref>/<path>".
func snippetRef(rawURL, path string) string {
	u, err := url.Parse(rawURL)
	if err == nil {
		p := u.Path
		if i := strings.LastIndex(p, "/raw/"); i >= 0 {
			rest := p[i+len("/raw/"):]
			if ref, file, ok := strings.Cut(rest, "/"); ok && ref != "" && file == path {
				return ref
			}
		}
	}
	return "HEAD"
}

// raw fetches a plain-text body.
func (p *provider) raw(ctx context.Context, op, path string) (string, error) {
	resp, err := p.send(ctx, call{method: http.MethodGet, path: path, op: op, accept: "text/plain"})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSnippetFile+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("%s: %w", op, ctxErr)
		}
		return "", err
	}
	if len(b) > maxSnippetFile {
		return "", &errs.Error{Kind: errs.ErrProviderAPI, Provider: p.acct.Name, Op: op,
			Message: fmt.Sprintf("snippet file is larger than %d MiB", maxSnippetFile>>20)}
	}
	return string(b), nil
}

// CreateSnippet creates a personal snippet. GitLab requires a title; when
// none is given the first file name is used. Visibility defaults to private.
func (p *provider) CreateSnippet(ctx context.Context, req forge.CreateSnippetRequest) (*domain.Snippet, error) {
	const op = "create snippet"
	if len(req.Files) == 0 {
		return nil, p.invalid(op, "a snippet needs at least one file")
	}
	files := make([]map[string]string, 0, len(req.Files))
	for _, f := range req.Files {
		if strings.TrimSpace(f.Name) == "" {
			return nil, p.invalid(op, "every snippet file needs a name")
		}
		files = append(files, map[string]string{"file_path": f.Name, "content": f.Content})
	}
	title := req.Title
	if strings.TrimSpace(title) == "" {
		title = req.Files[0].Name
	}
	vis := req.Visibility
	if vis == "" {
		vis = domain.VisibilityPrivate
	}
	body := map[string]any{"title": title, "visibility": string(vis), "files": files}
	if req.Description != "" {
		body["description"] = req.Description
	}
	var s glSnippet
	if _, err := p.do(ctx, call{method: http.MethodPost, path: "/snippets", op: op, body: body}, &s); err != nil {
		return nil, err
	}
	out := s.toDomain()
	return &out, nil
}
