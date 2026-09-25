package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/forge"
)

type apiRelease struct {
	ID          int64      `json:"id"`
	TagName     string     `json:"tag_name"`
	Name        *string    `json:"name"`
	Body        *string    `json:"body"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	Author      *apiOwner  `json:"author"`
	HTMLURL     string     `json:"html_url"`
	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt *time.Time `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

func toRelease(r apiRelease) domain.Release {
	out := domain.Release{
		ID: strconv.FormatInt(r.ID, 10), Tag: r.TagName, Name: str(r.Name), Body: str(r.Body),
		Draft: r.Draft, Prerelease: r.Prerelease, WebURL: r.HTMLURL, CreatedAt: r.CreatedAt,
		PublishedAt: timeOf(r.PublishedAt),
	}
	if r.Author != nil {
		out.Author = r.Author.Login
	}
	for _, a := range r.Assets {
		out.Assets = append(out.Assets, domain.Asset{Name: a.Name, URL: a.BrowserDownloadURL, Size: a.Size})
	}
	return out
}

// ListReleases implements forge.ReleaseProvider. Draft releases are only
// visible to users with push access.
func (p *Provider) ListReleases(ctx context.Context, ref domain.RepositoryRef, opts forge.ListOptions) ([]domain.Release, error) {
	return paginate(ctx, p, listSpec[apiRelease]{
		request: request{op: "list releases", path: repoPath(ref.Namespace, ref.Name, "releases"),
			notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref)},
		perPage: maxPerPage, limit: opts.Limit,
	}, func(r apiRelease) (domain.Release, bool) { return toRelease(r), true })
}

// findRelease looks a release up by tag. GET /releases/tags/{tag} only
// returns published releases, so drafts (which have no tag yet) are found by
// scanning the release list.
func (p *Provider) findRelease(ctx context.Context, op string, ref domain.RepositoryRef, tag string) (*apiRelease, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "release tag is required")
	}
	var r apiRelease
	_, err := p.do(ctx, request{op: op, method: http.MethodGet,
		path: repoPath(ref.Namespace, ref.Name, "releases", "tags", escRef(tag)), out: &r})
	if err == nil {
		return &r, nil
	}
	if !errors.Is(err, errs.ErrNotFound) {
		return nil, err
	}
	drafts, err := paginate(ctx, p, listSpec[apiRelease]{
		request: request{op: op, path: repoPath(ref.Namespace, ref.Name, "releases"),
			notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref)},
		perPage: maxPerPage, limit: 1,
	}, func(r apiRelease) (apiRelease, bool) { return r, r.Draft && r.TagName == tag })
	if err != nil {
		return nil, err
	}
	if len(drafts) == 0 {
		return nil, &errs.Error{Kind: errs.ErrNotFound, Provider: p.acct.Name, Op: op, Status: http.StatusNotFound,
			Message: fmt.Sprintf("release %q not found in %s", tag, ref.FullName())}
	}
	return &drafts[0], nil
}

// GetRelease implements forge.ReleaseProvider.
func (p *Provider) GetRelease(ctx context.Context, ref domain.RepositoryRef, tag string) (*domain.Release, error) {
	r, err := p.findRelease(ctx, "get release", ref, tag)
	if err != nil {
		return nil, err
	}
	out := toRelease(*r)
	return &out, nil
}

// CreateRelease implements forge.ReleaseCreator. GitHub creates the tag
// from Target (a branch or SHA; default branch when empty) if it is missing.
func (p *Provider) CreateRelease(ctx context.Context, ref domain.RepositoryRef, req forge.CreateReleaseRequest) (*domain.Release, error) {
	const op = "create release"
	if strings.TrimSpace(req.Tag) == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "release tag is required")
	}
	body := map[string]any{"tag_name": req.Tag, "draft": req.Draft, "prerelease": req.Prerelease}
	if req.Target != "" {
		body["target_commitish"] = req.Target
	}
	if req.Name != "" {
		body["name"] = req.Name
	}
	if req.Body != "" {
		body["body"] = req.Body
	}
	var r apiRelease
	_, err := p.do(ctx, request{op: op, method: http.MethodPost, path: repoPath(ref.Namespace, ref.Name, "releases"),
		body: body, out: &r, notFound: errs.ErrRepositoryNotFound, notFoundMsg: repoNotFound(ref)})
	if err != nil {
		return nil, err
	}
	out := toRelease(r)
	return &out, nil
}

// DeleteRelease implements forge.ReleaseDeleter. The git tag is kept, as on
// the GitHub web UI.
func (p *Provider) DeleteRelease(ctx context.Context, ref domain.RepositoryRef, tag string) error {
	const op = "delete release"
	r, err := p.findRelease(ctx, op, ref, tag)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, request{op: op, method: http.MethodDelete,
		path:        repoPath(ref.Namespace, ref.Name, "releases", strconv.FormatInt(r.ID, 10)),
		notFoundMsg: fmt.Sprintf("release %q not found in %s", tag, ref.FullName())})
	return err
}
