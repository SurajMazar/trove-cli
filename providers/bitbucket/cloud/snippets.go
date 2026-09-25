package cloud

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

// Snippet IDs in Trove are "<workspace>/<encoded_id>" because every
// Bitbucket snippet endpoint is addressed by workspace and id. GetSnippet
// also accepts a bare id when the account has a configured workspace.

type bbSnippetID string

// UnmarshalJSON accepts both numeric and string ids (Bitbucket documents an
// integer but returns short string ids such as "kypj").
func (s *bbSnippetID) UnmarshalJSON(b []byte) error {
	str := strings.Trim(string(b), `"`)
	if str == "null" {
		str = ""
	}
	*s = bbSnippetID(str)
	return nil
}

type bbSnippet struct {
	ID        bbSnippetID `json:"id"`
	Title     string      `json:"title"`
	IsPrivate bool        `json:"is_private"`
	CreatedOn time.Time   `json:"created_on"`
	UpdatedOn time.Time   `json:"updated_on"`
	Owner     *bbAccount  `json:"owner"`
	Creator   *bbAccount  `json:"creator"`
	Links     bbLinks     `json:"links"`
	Files     map[string]struct {
		Links bbLinks `json:"links"`
	} `json:"files"`
}

// workspaceFromSelf extracts the workspace from a snippet self link
// ".../snippets/{workspace}/{id}".
func workspaceFromSelf(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "snippets" {
			ws, err := url.PathUnescape(parts[i+1])
			if err != nil {
				return ""
			}
			return ws
		}
	}
	return ""
}

func (p *Provider) toSnippet(ws string, s bbSnippet) domain.Snippet {
	if w := workspaceFromSelf(s.Links.Self.Href); w != "" {
		ws = w
	}
	out := domain.Snippet{
		ID:         ws + "/" + string(s.ID),
		Title:      s.Title,
		Visibility: domain.VisibilityPublic,
		WebURL:     s.Links.HTML.Href,
		CreatedAt:  s.CreatedOn,
		UpdatedAt:  s.UpdatedOn,
		Term:       "Snippet",
	}
	if s.IsPrivate {
		out.Visibility = domain.VisibilityPrivate
	}
	if s.Owner != nil {
		out.Owner = s.Owner.handle()
	} else if s.Creator != nil {
		out.Owner = s.Creator.handle()
	}
	names := make([]string, 0, len(s.Files))
	for name := range s.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out.Files = append(out.Files, domain.SnippetFile{Name: name, RawURL: s.Files[name].Links.Self.Href})
	}
	return out
}

// ListSnippets implements forge.SnippetProvider. The cross-workspace
// GET /snippets listing no longer exists, so each of the caller's workspaces
// is listed with GET /snippets/{workspace}?role=member.
func (p *Provider) ListSnippets(ctx context.Context, opts forge.ListOptions) ([]domain.Snippet, error) {
	const op = "list snippets"
	workspaces, err := p.listingWorkspaces(ctx, op)
	if err != nil {
		return nil, err
	}
	var out []domain.Snippet
	for _, ws := range workspaces {
		limit := 0
		if opts.Limit > 0 {
			limit = opts.Limit - len(out)
			if limit <= 0 {
				break
			}
		}
		raw, err := collectPages[bbSnippet](ctx, p, limit, []request{{op: op, url: "/snippets/" + pathEscape(ws),
			query: url.Values{"role": {"member"}, "pagelen": {itoa(forge.PageSize(limit, maxPageLen))}}}}, nil)
		if err != nil {
			return nil, err
		}
		for _, s := range raw {
			out = append(out, p.toSnippet(ws, s))
		}
	}
	return out, nil
}

func (p *Provider) splitSnippetID(id string) (ws, sid string, err error) {
	id = strings.Trim(strings.TrimSpace(id), "/")
	if w, s, ok := strings.Cut(id, "/"); ok && w != "" && s != "" && !strings.Contains(s, "/") {
		return w, s, nil
	}
	if id != "" && !strings.Contains(id, "/") && p.workspace != "" {
		return p.workspace, id, nil
	}
	return "", "", p.invalidArg("get snippet", "Bitbucket snippet ids have the form <workspace>/<id>")
}

// GetSnippet implements forge.SnippetProvider. File contents are fetched
// with GET /snippets/{workspace}/{id}/files/{path}, which redirects (on the
// API host) to the latest revision of each file.
func (p *Provider) GetSnippet(ctx context.Context, id string) (*domain.Snippet, error) {
	const op = "get snippet"
	ws, sid, err := p.splitSnippetID(id)
	if err != nil {
		return nil, err
	}
	base := "/snippets/" + pathEscape(ws) + "/" + pathEscape(sid)
	var s bbSnippet
	if err := p.do(ctx, request{op: op, method: http.MethodGet, url: base, notFoundMsg: "snippet " + ws + "/" + sid + " not found"}, &s); err != nil {
		return nil, err
	}
	out := p.toSnippet(ws, s)
	for i, f := range out.Files {
		content, err := p.snippetFile(ctx, base, f.Name)
		if err != nil {
			return nil, err
		}
		out.Files[i].Content = content
	}
	return &out, nil
}

func (p *Provider) snippetFile(ctx context.Context, base, name string) (string, error) {
	const op = "get snippet file"
	resp, err := p.send(ctx, request{op: op, method: http.MethodGet, url: base + "/files/" + pathEscapeAll(name),
		accept: "*/*", notFoundMsg: "snippet file " + name + " not found"})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return "", fmt.Errorf("%s: %w", op, cerr)
		}
		return "", fmt.Errorf("%s: %w", op, err)
	}
	return string(b), nil
}

// CreateSnippet implements forge.SnippetCreator with a multipart/form-data
// POST (title, is_private and one "file" part per file) to
// /snippets/{workspace}, or /snippets (the caller's own account) when no
// workspace is given or configured. Bitbucket snippets have a title but no
// separate description: a description alone is used as the title, and
// supplying both is rejected rather than dropping one silently.
func (p *Provider) CreateSnippet(ctx context.Context, req forge.CreateSnippetRequest) (*domain.Snippet, error) {
	const op = "create snippet"
	title := strings.TrimSpace(req.Title)
	desc := strings.TrimSpace(req.Description)
	switch {
	case title == "":
		title = desc
	case desc != "" && desc != title:
		return nil, p.invalidArg(op, "Bitbucket snippets have only a title; pass either a title or a description")
	}
	if len(req.Files) == 0 {
		return nil, p.invalidArg(op, "a snippet needs at least one file")
	}
	private := true
	switch req.Visibility {
	case "", domain.VisibilityPrivate:
	case domain.VisibilityPublic:
		private = false
	default:
		return nil, p.invalidArg(op, "Bitbucket snippets are either public or private")
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if title != "" {
		if err := mw.WriteField("title", title); err != nil {
			return nil, err
		}
	}
	if err := mw.WriteField("is_private", strconv.FormatBool(private)); err != nil {
		return nil, err
	}
	for _, f := range req.Files {
		if strings.TrimSpace(f.Name) == "" {
			return nil, p.invalidArg(op, "every snippet file needs a name")
		}
		fw, err := mw.CreateFormFile("file", f.Name)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(fw, f.Content); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	ws := firstNonEmpty(req.Namespace, p.workspace)
	path := "/snippets"
	if ws != "" {
		path += "/" + pathEscape(ws)
	}
	var s bbSnippet
	if err := p.do(ctx, request{op: op, method: http.MethodPost, url: path, body: buf.Bytes(),
		contentType: mw.FormDataContentType(), notFoundMsg: "workspace " + ws + " not found"}, &s); err != nil {
		return nil, err
	}
	out := p.toSnippet(ws, s)
	return &out, nil
}
