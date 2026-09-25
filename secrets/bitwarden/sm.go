package bitwarden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/secrets"
)

const (
	bwsInstallURL = "https://bitwarden.com/help/secrets-manager-cli/"
	bwsTokenHint  = "export BWS_ACCESS_TOKEN=..."
	bwsTokenEnv   = "BWS_ACCESS_TOKEN"
)

// smBackend talks to Bitwarden Secrets Manager through bws.
//
// bws accepts secret values only as command-line arguments, so writes expose
// the value to the local process list for the lifetime of the bws process.
// See the package documentation.
type smBackend struct {
	path      string
	projectID string
	run       Runner
	env       func(string) string
	sem       semaphore
}

func newSMBackend(opts Options) *smBackend {
	return &smBackend{
		path:      opts.CLIPath,
		projectID: opts.ProjectID,
		run:       opts.Runner,
		env:       opts.Env,
		sem:       newSemaphore(),
	}
}

func (b *smBackend) description() string {
	if b.projectID != "" {
		return fmt.Sprintf("Bitwarden Secrets Manager (bws CLI, project %s)", b.projectID)
	}
	return "Bitwarden Secrets Manager (bws CLI)"
}

type bwsSecret struct {
	ID        string  `json:"id"`
	Key       string  `json:"key"`
	Value     string  `json:"value"`
	ProjectID *string `json:"projectId"`
}

// bws runs a bws subcommand. The access token reaches bws through the
// inherited BWS_ACCESS_TOKEN variable, never through --access-token.
func (b *smBackend) bws(ctx context.Context, args ...string) ([]byte, error) {
	out, _, err := b.run.Run(ctx, nil, b.path, args...)
	return out, err
}

func (b *smBackend) cmdErr(op string, err error, secret string) error {
	return commandError(op, "Bitwarden Secrets Manager CLI (bws)", bwsInstallURL, err, secret, classifyBWS)
}

// classifyBWS maps authentication failures reported by bws.
func classifyBWS(re *RunError) error {
	s := strings.ToLower(re.Stderr)
	for _, marker := range []string{"401", "unauthorized", "access token", "access_token", "invalid_client"} {
		if strings.Contains(s, marker) {
			return &errs.Error{
				Kind:    errs.ErrSecretProviderLocked,
				Message: "Bitwarden Secrets Manager rejected the access token in BWS_ACCESS_TOKEN",
				Hint:    bwsTokenHint,
			}
		}
	}
	return nil
}

// lock acquires the semaphore after checking that a token is configured.
func (b *smBackend) lock(ctx context.Context) (func(), error) {
	if b.env(bwsTokenEnv) == "" {
		return nil, &errs.Error{
			Kind:    errs.ErrSecretProviderLocked,
			Message: "Bitwarden Secrets Manager access token is not set (BWS_ACCESS_TOKEN)",
			Hint:    bwsTokenHint,
		}
	}
	if err := b.sem.acquire(ctx); err != nil {
		return nil, err
	}
	return b.sem.release, nil
}

func (b *smBackend) check(ctx context.Context) error {
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	args := []string{"project", "list", "--output", "json"}
	if b.projectID != "" {
		args = []string{"project", "get", b.projectID, "--output", "json"}
	}
	if _, err := b.bws(ctx, args...); err != nil {
		return b.cmdErr("check Bitwarden Secrets Manager access", err, "")
	}
	return nil
}

// find returns the secret with the given key (scoped to the project when one
// is configured), or nil. Callers must hold b.sem.
func (b *smBackend) find(ctx context.Context, key string) (*bwsSecret, error) {
	args := []string{"secret", "list"}
	if b.projectID != "" {
		args = append(args, b.projectID)
	}
	out, err := b.bws(ctx, append(args, "--output", "json")...)
	if err != nil {
		return nil, b.cmdErr("list Bitwarden Secrets Manager secrets", err, "")
	}
	var list []bwsSecret
	if err := json.Unmarshal(bytes.TrimSpace(out), &list); err != nil {
		return nil, errs.New(errs.ErrProviderAPI, "bitwarden: could not parse `bws secret list` output")
	}
	var matches []*bwsSecret
	for i := range list {
		if list[i].Key == key {
			matches = append(matches, &list[i])
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	}
	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = m.ID
	}
	return nil, errs.New(errs.ErrConflict,
		"bitwarden: found %d Secrets Manager secrets with key %q (ids %s); set secrets.bitwarden.project_id or remove the duplicates",
		len(matches), key, strings.Join(ids, ", "))
}

func (b *smBackend) get(ctx context.Context, key string) (string, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	s, err := b.find(ctx, key)
	if err != nil {
		return "", err
	}
	if s == nil {
		return "", secrets.NotFound(providerName, key)
	}
	return s.Value, nil
}

func (b *smBackend) exists(ctx context.Context, key string) (bool, error) {
	unlock, err := b.lock(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	s, err := b.find(ctx, key)
	return s != nil, err
}

func (b *smBackend) set(ctx context.Context, key, value string) error {
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := b.find(ctx, key)
	if err != nil {
		return err
	}
	// --output none keeps bws from echoing the secret (value included) back.
	if s != nil {
		// --value=<v> keeps values that start with "-" (PEM keys) from being
		// parsed as flags.
		if _, err := b.bws(ctx, "secret", "edit", s.ID, "--value="+value, "--output", "none"); err != nil {
			return b.cmdErr(fmt.Sprintf("update Bitwarden Secrets Manager secret %q", key), err, value)
		}
		return nil
	}
	if b.projectID == "" {
		return errs.New(errs.ErrInvalidConfiguration,
			"bitwarden: secrets.bitwarden.project_id is required to create secrets with the secrets-manager backend")
	}
	// "--" ends option parsing so keys and values are always positional.
	if _, err := b.bws(ctx, "secret", "create", "--note", managedNote, "--output", "none", "--", key, value, b.projectID); err != nil {
		return b.cmdErr(fmt.Sprintf("create Bitwarden Secrets Manager secret %q", key), err, value)
	}
	return nil
}

func (b *smBackend) del(ctx context.Context, key string) error {
	unlock, err := b.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := b.find(ctx, key)
	if err != nil || s == nil {
		return err
	}
	if _, err := b.bws(ctx, "secret", "delete", s.ID, "--output", "none"); err != nil {
		return b.cmdErr(fmt.Sprintf("delete Bitwarden Secrets Manager secret %q", key), err, "")
	}
	return nil
}
