package gitlab

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

type glRelease struct {
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	CreatedAt   *time.Time `json:"created_at"`
	ReleasedAt  *time.Time `json:"released_at"`
	Author      *glUserRef `json:"author"`
	Assets      struct {
		Links []struct {
			Name           string `json:"name"`
			URL            string `json:"url"`
			DirectAssetURL string `json:"direct_asset_url"`
		} `json:"links"`
	} `json:"assets"`
	Links struct {
		Self string `json:"self"`
	} `json:"_links"`
}

// toDomain maps a release. GitLab releases have no numeric ID in the API;
// the tag name identifies them. GitLab has neither drafts nor a prerelease
// flag, so both are always false.
func (r glRelease) toDomain() domain.Release {
	out := domain.Release{
		ID: r.TagName, Tag: r.TagName, Name: r.Name, Body: r.Description,
		Author: r.Author.name(), WebURL: r.Links.Self,
	}
	setTime(&out.CreatedAt, r.CreatedAt)
	setTime(&out.PublishedAt, r.ReleasedAt)
	for _, l := range r.Assets.Links {
		u := l.DirectAssetURL
		if u == "" {
			u = l.URL
		}
		out.Assets = append(out.Assets, domain.Asset{Name: l.Name, URL: u})
	}
	return out
}

func (p *provider) ListReleases(ctx context.Context, ref domain.RepositoryRef, opts forge.ListOptions) ([]domain.Release, error) {
	op := "list releases of " + ref.FullName()
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	rels, err := list[glRelease](ctx, p, op, path+"/releases", nil, opts.Limit, errs.ErrRepositoryNotFound)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Release, 0, len(rels))
	for _, r := range rels {
		out = append(out, r.toDomain())
	}
	return out, nil
}

func (p *provider) releasePath(op string, ref domain.RepositoryRef, tag string) (string, error) {
	path, err := projectPath(ref)
	if err != nil {
		return "", withProvider(err, p.acct.Name, op)
	}
	if strings.TrimSpace(tag) == "" {
		return "", p.invalid(op, "a tag name is required")
	}
	// Tags may contain "/", which must be escaped as %2F.
	return path + "/releases/" + url.PathEscape(tag), nil
}

func (p *provider) GetRelease(ctx context.Context, ref domain.RepositoryRef, tag string) (*domain.Release, error) {
	op := "get release " + tag + " of " + ref.FullName()
	path, err := p.releasePath(op, ref, tag)
	if err != nil {
		return nil, err
	}
	var r glRelease
	if err := p.get(ctx, op, path, nil, &r); err != nil {
		return nil, err
	}
	d := r.toDomain()
	return &d, nil
}

// CreateRelease creates a release, creating the tag from req.Target when it
// does not exist yet. GitLab has no draft releases and no prerelease flag
// (its "upcoming release" is merely a future released_at date), so those
// requests are rejected instead of being emulated.
func (p *provider) CreateRelease(ctx context.Context, ref domain.RepositoryRef, req forge.CreateReleaseRequest) (*domain.Release, error) {
	op := "create release in " + ref.FullName()
	if req.Draft {
		return nil, p.invalid(op, "GitLab does not support draft releases; a release is published when it is created")
	}
	if req.Prerelease {
		return nil, p.invalid(op, "GitLab releases have no prerelease flag; mark the release as a prerelease in its name or description instead")
	}
	if strings.TrimSpace(req.Tag) == "" {
		return nil, p.invalid(op, "a tag name is required")
	}
	path, err := projectPath(ref)
	if err != nil {
		return nil, withProvider(err, p.acct.Name, op)
	}
	body := map[string]any{"tag_name": req.Tag}
	if req.Name != "" {
		body["name"] = req.Name
	}
	if req.Body != "" {
		body["description"] = req.Body
	}
	if req.Target != "" {
		// Required by GitLab when the tag does not exist yet.
		body["ref"] = req.Target
	}
	var r glRelease
	if _, err := p.do(ctx, call{method: http.MethodPost, path: path + "/releases", op: op, body: body, notFound: errs.ErrRepositoryNotFound}, &r); err != nil {
		return nil, err
	}
	d := r.toDomain()
	return &d, nil
}

// DeleteRelease deletes the release; the Git tag itself is kept.
func (p *provider) DeleteRelease(ctx context.Context, ref domain.RepositoryRef, tag string) error {
	op := "delete release " + tag + " of " + ref.FullName()
	path, err := p.releasePath(op, ref, tag)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, call{method: http.MethodDelete, path: path, op: op}, nil)
	return err
}
