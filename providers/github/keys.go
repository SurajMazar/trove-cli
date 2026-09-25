package github

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
)

type apiSSHKey struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

type apiGPGKey struct {
	ID     int64  `json:"id"`
	KeyID  string `json:"key_id"`
	Emails []struct {
		Email string `json:"email"`
	} `json:"emails"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// sshFingerprint computes the OpenSSH SHA256 fingerprint of an
// authorized_keys line; GitHub's API does not return one.
func sshFingerprint(key string) string {
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func toSSHKey(k apiSSHKey) domain.SSHKey {
	return domain.SSHKey{ID: strconv.FormatInt(k.ID, 10), Title: k.Title, Key: k.Key,
		Fingerprint: sshFingerprint(k.Key), CreatedAt: k.CreatedAt}
}

// ListSSHKeys implements forge.SSHKeyProvider.
func (p *Provider) ListSSHKeys(ctx context.Context) ([]domain.SSHKey, error) {
	return paginate(ctx, p, listSpec[apiSSHKey]{
		request: request{op: "list SSH keys", path: "/user/keys"}, perPage: maxPerPage,
	}, func(k apiSSHKey) (domain.SSHKey, bool) { return toSSHKey(k), true })
}

// AddSSHKey implements forge.SSHKeyProvider (needs write:public_key or
// admin:public_key for classic tokens).
func (p *Provider) AddSSHKey(ctx context.Context, title, key string) (*domain.SSHKey, error) {
	const op = "add SSH key"
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "an SSH public key is required")
	}
	if strings.Contains(key, "PRIVATE KEY") {
		return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "that is a private key; add the public key (.pub) instead")
	}
	body := map[string]string{"key": key}
	if title = strings.TrimSpace(title); title != "" {
		body["title"] = title
	}
	var k apiSSHKey
	if _, err := p.do(ctx, request{op: op, method: http.MethodPost, path: "/user/keys", body: body, out: &k}); err != nil {
		return nil, err
	}
	out := toSSHKey(k)
	return &out, nil
}

// RemoveSSHKey implements forge.SSHKeyProvider.
func (p *Provider) RemoveSSHKey(ctx context.Context, id string) error {
	const op = "remove SSH key"
	n, err := p.parseNumericID(op, "SSH key ID", id)
	if err != nil {
		return err
	}
	_, err = p.do(ctx, request{op: op, method: http.MethodDelete, path: "/user/keys/" + strconv.FormatInt(n, 10),
		notFoundMsg: fmt.Sprintf("SSH key %s not found", id)})
	return err
}

// ListGPGKeys implements forge.GPGKeyProvider.
func (p *Provider) ListGPGKeys(ctx context.Context) ([]domain.GPGKey, error) {
	return paginate(ctx, p, listSpec[apiGPGKey]{
		request: request{op: "list GPG keys", path: "/user/gpg_keys"}, perPage: maxPerPage,
	}, func(k apiGPGKey) (domain.GPGKey, bool) {
		out := domain.GPGKey{ID: strconv.FormatInt(k.ID, 10), KeyID: k.KeyID, CreatedAt: k.CreatedAt, ExpiresAt: timeOf(k.ExpiresAt)}
		for _, e := range k.Emails {
			out.Emails = append(out.Emails, e.Email)
		}
		return out, true
	})
}
