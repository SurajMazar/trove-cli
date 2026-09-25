package secretservice

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	dbus "github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/redact"
	"github.com/SurajMazar/trove-cli/internal/secrets"
)

const (
	providerName = "secretservice"
	// DefaultService is the service attribute used when Options.Service is empty.
	DefaultService = "trove"

	installHint = "install and start gnome-keyring or KeePassXC with Secret Service integration"
	unlockHint  = "unlock your default keyring (for example with Seahorse, KeePassXC, or by logging in to a desktop session) and retry"
)

// Backend stores secrets by service and user attributes. Missing items are
// reported with keyring.ErrNotFound. The default backend is go-keyring's
// Secret Service implementation; tests inject an in-memory one.
type Backend interface {
	Set(service, user, pass string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

// Prober is optionally implemented by backends that can check availability
// without touching items. Check uses it when present.
type Prober interface {
	Probe() error
}

// Options configures the provider.
type Options struct {
	// Service is the value of the "service" attribute (default "trove").
	Service string
	// Backend overrides the Secret Service access layer. When set, the Linux
	// platform check is skipped.
	Backend Backend
}

// Provider stores secrets through the Secret Service API. It is safe for
// concurrent use.
type Provider struct {
	service string
	backend Backend // nil when the platform is unsupported
}

var (
	_ secrets.SecretProvider = (*Provider)(nil)
	_ secrets.Checker        = (*Provider)(nil)
	_ secrets.Describer      = (*Provider)(nil)
)

// New returns a Secret Service provider. It does not connect to D-Bus.
func New(opts Options) (*Provider, error) {
	service := strings.TrimSpace(opts.Service)
	if service == "" {
		service = DefaultService
	}
	if strings.ContainsRune(service, 0) {
		return nil, errs.New(errs.ErrInvalidConfiguration, "secretservice: service %q contains a NUL character", service)
	}
	p := &Provider{service: service, backend: opts.Backend}
	if p.backend == nil && runtime.GOOS == "linux" {
		p.backend = newDBusBackend()
	}
	return p, nil
}

// Name implements secrets.SecretProvider.
func (p *Provider) Name() string { return providerName }

// Description implements secrets.Describer.
func (p *Provider) Description() string {
	return fmt.Sprintf("Linux Secret Service / libsecret (service %q)", p.service)
}

// Check reports whether a Secret Service is reachable.
func (p *Provider) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.backend == nil {
		return unavailable()
	}
	pr, ok := p.backend.(Prober)
	if !ok {
		return nil
	}
	if _, err := call(ctx, func() (struct{}, error) { return struct{}{}, pr.Probe() }); err != nil {
		return classify("check", "", err, "")
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
		return "", classify("read", key, err, "")
	}
	return v, nil
}

// Set creates or replaces the value stored under key.
func (p *Provider) Set(ctx context.Context, key, value string) error {
	key, err := p.prepare(ctx, key)
	if err != nil {
		return err
	}
	if _, err := call(ctx, func() (struct{}, error) { return struct{}{}, p.backend.Set(p.service, key, value) }); err != nil {
		return classify("write", key, err, value)
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
	if err = classify("delete", key, err, ""); errors.Is(err, errs.ErrSecretNotFound) {
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
		return "", errs.New(errs.ErrInvalidArgument, "secretservice: secret key is empty")
	}
	if strings.ContainsRune(key, 0) {
		return "", errs.New(errs.ErrInvalidArgument, "secretservice: secret key %q contains a NUL character", key)
	}
	return key, nil
}

func unavailable() error {
	return &errs.Error{
		Kind:    errs.ErrSecretUnavailable,
		Message: "the Linux Secret Service is only available on Linux",
		Hint:    "use the keychain or bitwarden secret provider on this system",
	}
}

// classify maps Secret Service and D-Bus failures to Trove errors. value,
// when non-empty, is scrubbed from any message.
func classify(op, key string, err error, value string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	what := op
	if key != "" {
		what = fmt.Sprintf("%s %q", op, key)
	}
	if errors.Is(err, keyring.ErrNotFound) || errors.Is(err, errs.ErrSecretNotFound) {
		return secrets.NotFound(providerName, key)
	}
	if errors.Is(err, errNoSessionBus) {
		return &errs.Error{
			Kind:    errs.ErrSecretUnavailable,
			Message: fmt.Sprintf("secretservice: cannot %s: no D-Bus session bus (DBUS_SESSION_BUS_ADDRESS is not set)", what),
			Hint:    "run trove inside a desktop session (or under `dbus-run-session`) and " + installHint,
		}
	}
	unavail := func(detail, hint string) error {
		return &errs.Error{Kind: errs.ErrSecretUnavailable, Message: fmt.Sprintf("secretservice: cannot %s: %s", what, detail), Hint: hint}
	}
	locked := func(detail string) error {
		return &errs.Error{Kind: errs.ErrSecretProviderLocked, Message: fmt.Sprintf("secretservice: cannot %s: %s", what, detail), Hint: unlockHint}
	}

	switch dbusErrorName(err) {
	case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner",
		"org.freedesktop.DBus.Error.Spawn.ChildExited", "org.freedesktop.DBus.Error.Spawn.ExecFailed",
		"org.freedesktop.DBus.Error.Spawn.ServiceNotFound":
		return unavail("no Secret Service provider (org.freedesktop.secrets) is running on the session bus", installHint)
	case "org.freedesktop.Secret.Error.IsLocked":
		return locked("the keyring is locked")
	case "org.freedesktop.Secret.Error.NoSuchObject", "org.freedesktop.DBus.Error.UnknownObject":
		return unavail("the default keyring collection does not exist", "create a default keyring (for example with Seahorse or KeePassXC) and retry")
	case "org.freedesktop.Secret.Error.NoSession":
		return unavail("the Secret Service session was closed", "retry the command")
	case "org.freedesktop.DBus.Error.NoReply", "org.freedesktop.DBus.Error.Timeout", "org.freedesktop.DBus.Error.TimedOut":
		return unavail("the Secret Service did not respond", "make sure your keyring daemon is running and responsive")
	case "org.freedesktop.DBus.Error.AccessDenied":
		return &errs.Error{Kind: errs.ErrPermissionDenied, Message: fmt.Sprintf("secretservice: cannot %s: access denied by the Secret Service", what)}
	}

	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "failed to unlock correct collection"), strings.Contains(lower, "dismissed"):
		// go-keyring reports a dismissed or impossible unlock prompt this way.
		return locked("the keyring is locked and was not unlocked (the prompt was dismissed or could not be shown)")
	case strings.Contains(lower, "org.freedesktop.secrets"), strings.Contains(lower, "not provided by any .service files"):
		return unavail("no Secret Service provider (org.freedesktop.secrets) is running on the session bus", installHint)
	case strings.Contains(lower, "couldn't determine address of session bus"), strings.Contains(lower, "dbus-launch"),
		strings.Contains(lower, "connection refused"), strings.Contains(lower, "no such file or directory"):
		return unavail("cannot connect to the D-Bus session bus", "run trove inside a desktop session (or under `dbus-run-session`) and "+installHint)
	}
	msg = redact.String(msg)
	if value != "" {
		msg = strings.ReplaceAll(msg, value, redact.Placeholder)
	}
	return unavail("Secret Service error: "+msg, installHint)
}

// dbusErrorName returns the D-Bus error name carried by err, if any. godbus
// returns both dbus.Error values and pointers.
func dbusErrorName(err error) string {
	var v dbus.Error
	if errors.As(err, &v) {
		return v.Name
	}
	var p *dbus.Error
	if errors.As(err, &p) && p != nil {
		return p.Name
	}
	return ""
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
		return zero, fmt.Errorf("secretservice: stopped waiting for the Secret Service (the operation may still complete): %w", ctx.Err())
	}
}
