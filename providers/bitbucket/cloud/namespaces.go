package cloud

import (
	"context"
	"net/http"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/auth"
	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

func (p *Provider) toNamespace(w bbWorkspace) domain.Namespace {
	ns := domain.Namespace{
		ID:        w.UUID,
		Name:      firstNonEmpty(w.Name, w.Slug),
		FullPath:  w.Slug,
		Type:      domain.NamespaceWorkspace,
		WebURL:    firstNonEmpty(w.Links.HTML.Href, p.webURL+"/"+w.Slug),
		AvatarURL: w.Links.Avatar.Href,
		Label:     "Bitbucket Workspace",
	}
	return ns
}

// ListNamespaces implements forge.NamespaceProvider by listing the caller's
// workspaces (GET /user/workspaces). Access tokens cannot enumerate
// workspaces; for them the configured workspace is returned.
func (p *Provider) ListNamespaces(ctx context.Context, opts forge.ListOptions) ([]domain.Namespace, error) {
	const op = "list workspaces"
	cred, err := p.credential(ctx)
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	if p.authMethodFor(cred) == auth.MethodAccessToken {
		if p.workspace == "" {
			return nil, p.needWorkspace(op, "Bitbucket access tokens cannot list workspaces; configure the token's workspace")
		}
		ns, err := p.GetNamespace(ctx, p.workspace)
		if err != nil {
			return nil, err
		}
		return []domain.Namespace{*ns}, nil
	}
	was, err := p.userWorkspaces(ctx, opts.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Namespace, 0, len(was))
	for _, wa := range was {
		out = append(out, p.toNamespace(wa.Workspace))
	}
	return out, nil
}

type bbProject struct {
	UUID        string  `json:"uuid"`
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Links       bbLinks `json:"links"`
}

// GetNamespace implements forge.NamespaceProvider. path is a workspace slug
// ("acme") or a project inside a workspace ("acme/PROJ", by project key).
func (p *Provider) GetNamespace(ctx context.Context, path string) (*domain.Namespace, error) {
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path == "" {
		return nil, p.invalidArg("get workspace", "a workspace slug is required")
	}
	if ws, key, ok := strings.Cut(path, "/"); ok {
		const op = "get project"
		if strings.Contains(key, "/") || key == "" {
			return nil, p.invalidArg(op, "Bitbucket namespaces are <workspace> or <workspace>/<PROJECT-KEY>")
		}
		var pr bbProject
		if err := p.do(ctx, request{op: op, method: http.MethodGet,
			url:         "/workspaces/" + pathEscape(ws) + "/projects/" + pathEscape(key),
			notFoundMsg: "project " + key + " not found in workspace " + ws}, &pr); err != nil {
			return nil, err
		}
		return &domain.Namespace{
			ID: pr.UUID, Name: firstNonEmpty(pr.Name, pr.Key), FullPath: ws + "/" + pr.Key,
			Type: domain.NamespaceProject, ParentPath: ws, Description: pr.Description,
			WebURL: pr.Links.HTML.Href, AvatarURL: pr.Links.Avatar.Href, Label: "Bitbucket Project",
		}, nil
	}
	const op = "get workspace"
	var w bbWorkspace
	if err := p.do(ctx, request{op: op, method: http.MethodGet, url: "/workspaces/" + pathEscape(path),
		notFoundMsg: "workspace " + path + " not found"}, &w); err != nil {
		return nil, err
	}
	ns := p.toNamespace(w)
	return &ns, nil
}
