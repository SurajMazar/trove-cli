package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
)

// SSH and GPG keys live under /users/{selected_user}, where selected_user is
// the current user's UUID (with braces, percent-encoded in the path).

type bbSSHKey struct {
	UUID        string     `json:"uuid"`
	Key         string     `json:"key"`
	Label       string     `json:"label"`
	Comment     string     `json:"comment"`
	Fingerprint string     `json:"fingerprint"`
	CreatedOn   *time.Time `json:"created_on"`
}

type bbGPGKey struct {
	KeyID       string     `json:"key_id"`
	Fingerprint string     `json:"fingerprint"`
	Name        string     `json:"name"`
	CreatedOn   *time.Time `json:"created_on"`
	AddedOn     *time.Time `json:"added_on"`
	ExpiresOn   *time.Time `json:"expires_on"`
}

func toSSHKey(k bbSSHKey) domain.SSHKey {
	out := domain.SSHKey{ID: k.UUID, Title: firstNonEmpty(k.Label, k.Comment), Key: k.Key, Fingerprint: k.Fingerprint}
	if k.CreatedOn != nil {
		out.CreatedAt = *k.CreatedOn
	}
	return out
}

func (p *Provider) keysPath(ctx context.Context, feature, kind string) (string, error) {
	uuid, err := p.selectedUser(ctx, feature)
	if err != nil {
		return "", err
	}
	return "/users/" + pathEscape(uuid) + "/" + kind, nil
}

// ListSSHKeys implements forge.SSHKeyProvider.
func (p *Provider) ListSSHKeys(ctx context.Context) ([]domain.SSHKey, error) {
	const op = "list SSH keys"
	path, err := p.keysPath(ctx, "SSH keys", "ssh-keys")
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	raw, err := collectPages[bbSSHKey](ctx, p, 0, []request{{op: op, url: path, query: url.Values{"pagelen": {itoa(maxPageLen)}}}}, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SSHKey, 0, len(raw))
	for _, k := range raw {
		out = append(out, toSSHKey(k))
	}
	return out, nil
}

// AddSSHKey implements forge.SSHKeyProvider.
func (p *Provider) AddSSHKey(ctx context.Context, title, key string) (*domain.SSHKey, error) {
	const op = "add SSH key"
	if strings.TrimSpace(key) == "" {
		return nil, p.invalidArg(op, "an SSH public key is required")
	}
	path, err := p.keysPath(ctx, "SSH keys", "ssh-keys")
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	body := map[string]string{"key": strings.TrimSpace(key)}
	if title != "" {
		body["label"] = title
	}
	var k bbSSHKey
	if err := p.do(ctx, request{op: op, method: http.MethodPost, url: path, body: body}, &k); err != nil {
		return nil, err
	}
	out := toSSHKey(k)
	return &out, nil
}

// RemoveSSHKey implements forge.SSHKeyProvider. id is the key UUID.
func (p *Provider) RemoveSSHKey(ctx context.Context, id string) error {
	const op = "remove SSH key"
	if strings.TrimSpace(id) == "" {
		return p.invalidArg(op, "an SSH key id is required")
	}
	path, err := p.keysPath(ctx, "SSH keys", "ssh-keys")
	if err != nil {
		return p.wrapOp(op, err)
	}
	return p.do(ctx, request{op: op, method: http.MethodDelete, url: path + "/" + pathEscape(strings.TrimSpace(id)),
		notFoundMsg: "SSH key " + id + " not found"}, nil)
}

// ListGPGKeys implements forge.GPGKeyProvider. The key fingerprint is the
// identifier Bitbucket uses in /gpg-keys/{fingerprint}.
func (p *Provider) ListGPGKeys(ctx context.Context) ([]domain.GPGKey, error) {
	const op = "list GPG keys"
	path, err := p.keysPath(ctx, "GPG keys", "gpg-keys")
	if err != nil {
		return nil, p.wrapOp(op, err)
	}
	raw, err := collectPages[bbGPGKey](ctx, p, 0, []request{{op: op, url: path, query: url.Values{"pagelen": {itoa(maxPageLen)}}}}, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.GPGKey, 0, len(raw))
	for _, k := range raw {
		g := domain.GPGKey{ID: firstNonEmpty(k.Fingerprint, k.KeyID), KeyID: k.KeyID}
		switch {
		case k.CreatedOn != nil:
			g.CreatedAt = *k.CreatedOn
		case k.AddedOn != nil:
			g.CreatedAt = *k.AddedOn
		}
		if k.ExpiresOn != nil {
			g.ExpiresAt = *k.ExpiresOn
		}
		out = append(out, g)
	}
	return out, nil
}

// --- summary ---------------------------------------------------------------------

// Summary implements forge.SummaryProvider:
//
//   - Repositories: the sum of "size" of GET /repositories/{workspace}
//     ?role=member&pagelen=1 over the caller's workspaces (nil if Bitbucket
//     omits size for any of them).
//   - OpenPullRequests: the sum of "size" of
//     GET /workspaces/{workspace}/pullrequests/{user}?state=OPEN&pagelen=1
//     (pull requests authored by the caller; nil for access tokens or when
//     any count is unavailable).
//
// Issues, pipelines and notifications have no cheap account-wide count.
func (p *Provider) Summary(ctx context.Context) (*domain.AccountSummary, error) {
	const op = "account summary"
	workspaces, err := p.listingWorkspaces(ctx, op)
	if err != nil {
		return nil, err
	}
	sum := &domain.AccountSummary{}
	repos, ok := 0, true
	for _, ws := range workspaces {
		env, err := getPage[bbRepository](ctx, p, request{op: op, url: "/repositories/" + pathEscape(ws),
			query: url.Values{"role": {"member"}, "pagelen": {"1"}}})
		if err != nil {
			return nil, err
		}
		if env.Size == nil {
			ok = false
			break
		}
		repos += *env.Size
	}
	if ok {
		sum.Repositories = &repos
	}

	user, err := p.selectedUser(ctx, "pull request counts")
	if err != nil {
		if errors.Is(err, errs.ErrUnsupportedCapability) {
			return sum, nil
		}
		return nil, err
	}
	prs := 0
	for _, ws := range workspaces {
		env, err := getPage[bbPullRequest](ctx, p, request{op: op,
			url:   "/workspaces/" + pathEscape(ws) + "/pullrequests/" + pathEscape(user),
			query: url.Values{"state": {"OPEN"}, "pagelen": {"1"}}})
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			// Best effort: a workspace that refuses the query makes the
			// total unknown rather than wrong.
			return sum, nil
		}
		if env.Size == nil {
			return sum, nil
		}
		prs += *env.Size
	}
	sum.OpenPullRequests = &prs
	return sum, nil
}
