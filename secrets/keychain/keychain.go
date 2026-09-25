package keychain

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/redact"
	"github.com/SurajMazar/trove-cli/internal/secrets"
)

const (
	providerName = "keychain"
	// DefaultService is the Keychain service used when Options.Service is empty.
	DefaultService = "trove"
	securityPath   = "/usr/bin/security"
	unlockHint     = "security unlock-keychain login.keychain-db"
)

// Backend stores generic passwords. Missing items are reported with
// keyring.ErrNotFound. The default backend is go-keyring's macOS
// implementation; tests inject an in-memory one.
type Backend interface {
	Set(service, user, pass string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

// Options configures the provider.
type Options struct {
	// Service is the Keychain item service (default "trove").
	Service string
	// Backend overrides the Keychain access layer. When set, the macOS
	// platform check is skipped.
	Backend Backend
}

// Provider stores secrets in the macOS Keychain. It is safe for concurrent use.
type Provider struct {
	service string
	backend Backend // nil when the platform is unsupported
	custom  bool
}

var (
	_ secrets.SecretProvider = (*Provider)(nil)
	_ secrets.Checker        = (*Provider)(nil)
	_ secrets.Describer      = (*Provider)(nil)
)

// keyringBackend is go-keyring's platform provider (macOS: /usr/bin/security).
type keyringBackend struct{}

func (keyringBackend) Set(service, user, pass string) error     { return keyring.Set(service, user, pass) }
func (keyringBackend) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (keyringBackend) Delete(service, user string) error        { return keyring.Delete(service, user) }

// New returns a Keychain provider. It does not touch the Keychain.
func New(opts Options) (*Provider, error) {
	service := strings.TrimSpace(opts.Service)
	if service == "" {
		service = DefaultService
	}
	if err := validName("service", service); err != nil {
		return nil, errs.New(errs.ErrInvalidConfiguration, "%s", err.Error())
	}
	p := &Provider{service: service, backend: opts.Backend, custom: opts.Backend != nil}
	if p.backend == nil && runtime.GOOS == "darwin" {
		p.backend = keyringBackend{}
	}
	return p, nil
}

// Name implements secrets.SecretProvider.
func (p *Provider) Name() string { return providerName }

// Description implements secrets.Describer.
func (p *Provider) Description() string {
	return fmt.Sprintf("macOS Keychain (generic passwords, service %q)", p.service)
}

// Check reports whether the Keychain can be used on this system.
func (p *Provider) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.backend == nil {
		return unavailable()
	}
	if p.custom {
		return nil
	}
	if _, err := os.Stat(securityPath); err != nil {
		return &errs.Error{Kind: errs.ErrSecretUnavailable, Message: fmt.Sprintf("macOS Keychain tool %s not found", securityPath)}
	}
	return nil
}

// Get returns the value stored under key.
func (p *Provider) Get(ctx context.Context, key string) (string, error) {
	key, err := p.prepare(ctx, key)
	if err != nil {
		return "", err
	}
	v, err := call(ctx, func() (string, error) { return p.backend.Get(p.service, key) })
	if err != nil {
		return "", p.mapErr("read", key, err, "")
	}
	return v, nil
}

// Set creates or replaces the value stored under key.
func (p *Provider) Set(ctx context.Context, key, value string) error {
	key, err := p.prepare(ctx, key)
	if err != nil {
		return err
	}
	_, err = call(ctx, func() (struct{}, error) { return struct{}{}, p.backend.Set(p.service, key, value) })
	if err != nil {
		return p.mapErr("write", key, err, value)
	}
	return nil
}

// Delete removes key. Deleting a missing key succeeds.
func (p *Provider) Delete(ctx context.Context, key string) error {
	key, err := p.prepare(ctx, key)
	if err != nil {
		return err
	}
	_, err = call(ctx, func() (struct{}, error) { return struct{}{}, p.backend.Delete(p.service, key) })
	if err == nil {
		return nil
	}
	if err = p.mapErr("delete", key, err, ""); errors.Is(err, errs.ErrSecretNotFound) {
		return nil
	}
	return err
}

// Exists reports whether key is stored.
func (p *Provider) Exists(ctx context.Context, key string) (bool, error) {
	if _, err := p.Get(ctx, key); err != nil {
		if errors.Is(err, errs.ErrSecretNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (p *Provider) prepare(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.backend == nil {
		return "", unavailable()
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errs.New(errs.ErrInvalidArgument, "keychain: secret key is empty")
	}
	if err := validName("key", key); err != nil {
		return "", errs.New(errs.ErrInvalidArgument, "%s", err.Error())
	}
	return key, nil
}

// validName rejects control characters, which would break the line-oriented
// `security -i` protocol.
func validName(what, s string) error {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("keychain: %s %q contains control characters", what, s)
		}
	}
	return nil
}

func unavailable() error {
	return &errs.Error{Kind: errs.ErrSecretUnavailable, Message: "macOS Keychain is only available on macOS", Hint: "use the bitwarden or secretservice secret provider on this system"}
}

// security(1) exits with the low byte of the Security framework OSStatus.
const (
	exitInteractionNotAllowed = 36  // errSecInteractionNotAllowed (-25308): locked, cannot prompt
	exitItemNotFound          = 44  // errSecItemNotFound (-25300)
	exitAuthFailed            = 51  // errSecAuthFailed (-25293)
	exitUserCanceled          = 128 // errSecUserCanceled (-128)
)

// mapErr converts backend errors into Trove errors. value, when non-empty, is
// scrubbed from any message.
func (p *Provider) mapErr(op, key string, err error, value string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch {
	case errors.Is(err, keyring.ErrNotFound), errors.Is(err, errs.ErrSecretNotFound):
		return secrets.NotFound(providerName, key)
	case errors.Is(err, keyring.ErrSetDataTooBig):
		return errs.New(errs.ErrInvalidArgument,
			"keychain: the value for %q is too large for the macOS Keychain command line tool (about 3000 bytes of service, account and value combined); store it with the bitwarden provider instead", key)
	case errors.Is(err, keyring.ErrUnsupportedPlatform):
		return unavailable()
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return &errs.Error{Kind: errs.ErrSecretUnavailable, Message: fmt.Sprintf("macOS Keychain tool %s not found", securityPath)}
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch code := ee.ExitCode(); code {
		case exitItemNotFound:
			return secrets.NotFound(providerName, key)
		case exitInteractionNotAllowed, exitAuthFailed:
			return &errs.Error{
				Kind:    errs.ErrSecretProviderLocked,
				Message: fmt.Sprintf("keychain: cannot %s %q: the keychain is locked and cannot be unlocked without user interaction", op, key),
				Hint:    unlockHint,
			}
		case exitUserCanceled:
			return &errs.Error{
				Kind:    errs.ErrPermissionDenied,
				Message: fmt.Sprintf("keychain: access to %q was denied or the Keychain prompt was canceled", key),
				Hint:    "run the command again and choose Allow (or Always Allow) in the Keychain prompt",
			}
		default:
			return &errs.Error{Kind: errs.ErrSecretUnavailable, Message: fmt.Sprintf("keychain: cannot %s %q: security exited with status %d", op, key, code)}
		}
	}
	msg := redact.String(err.Error())
	if value != "" {
		msg = strings.ReplaceAll(msg, value, redact.Placeholder)
	}
	return &errs.Error{Kind: errs.ErrSecretUnavailable, Message: fmt.Sprintf("keychain: cannot %s %q: %s", op, key, msg)}
}

// call runs fn, which cannot be interrupted, but stops waiting for it when
// ctx is done. A call abandoned this way may still complete later.
func call[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return zero, fmt.Errorf("keychain: stopped waiting for the Keychain (the operation may still complete): %w", ctx.Err())
	}
}
