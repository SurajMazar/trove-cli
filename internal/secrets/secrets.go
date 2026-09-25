// Package secrets defines the Secret Provider contract.
//
// Secret providers (Bitwarden, macOS Keychain, Linux Secret Service, ...) are
// completely independent from forge providers. The application refers to a
// secret by a SecretReference such as
//
//	bitwarden://trove/github/personal/token
//	keychain://trove/github/personal/token
//	secretservice://trove/gitlab/work/token
//
// and the Resolver dispatches to the provider named by the scheme.
package secrets

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/SurajMazar/trove-cli/internal/errs"
)

// SecretProvider stores and retrieves secret strings by key.
//
// Implementations must never log or print secret values, must return
// errs.ErrSecretNotFound (wrapped) from Get for missing keys, and must treat
// Delete of a missing key as success (Delete is idempotent).
type SecretProvider interface {
	Name() string
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
}

// Checker is implemented by providers that can report whether they are
// usable in the current environment (installed, unlocked, reachable).
type Checker interface {
	Check(ctx context.Context) error
}

// Describer is implemented by providers that can describe themselves for
// `trove doctor`.
type Describer interface {
	Description() string
}

// SecretReference identifies a secret: provider scheme plus key.
type SecretReference struct {
	Provider string
	Key      string
}

// String renders the reference as provider://key.
func (r SecretReference) String() string { return r.Provider + "://" + r.Key }

// IsZero reports whether the reference is empty.
func (r SecretReference) IsZero() bool { return r.Provider == "" && r.Key == "" }

// ParseReference parses "provider://key/path".
func ParseReference(s string) (SecretReference, error) {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "://")
	if i <= 0 {
		return SecretReference{}, errs.New(errs.ErrInvalidConfiguration, "invalid secret reference %q (want provider://key)", s)
	}
	ref := SecretReference{Provider: strings.ToLower(s[:i]), Key: strings.Trim(s[i+3:], "/")}
	if ref.Key == "" {
		return SecretReference{}, errs.New(errs.ErrInvalidConfiguration, "secret reference %q has an empty key", s)
	}
	if k, err := url.PathUnescape(ref.Key); err == nil {
		ref.Key = k
	}
	for _, r := range ref.Provider {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return SecretReference{}, errs.New(errs.ErrInvalidConfiguration, "invalid secret provider name %q", ref.Provider)
		}
	}
	return ref, nil
}

// DefaultKey returns Trove's conventional key for a provider account secret:
// trove/<type>/<alias>/token.
func DefaultKey(forgeType, alias, name string) string {
	if name == "" {
		name = "token"
	}
	return fmt.Sprintf("trove/%s/%s/%s", forgeType, alias, name)
}

// Resolver resolves secret references against registered providers.
type SecretResolver interface {
	Resolve(ctx context.Context, ref SecretReference) (string, error)
}

// Resolver is a registry of secret providers keyed by scheme.
type Resolver struct {
	mu        sync.RWMutex
	providers map[string]SecretProvider
	factories map[string]func() (SecretProvider, error)
}

// NewResolver returns an empty resolver.
func NewResolver() *Resolver {
	return &Resolver{providers: map[string]SecretProvider{}, factories: map[string]func() (SecretProvider, error){}}
}

// Register adds an already-constructed provider under its Name().
func (r *Resolver) Register(p SecretProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
}

// RegisterFactory adds a lazily-constructed provider. Construction happens on
// first use so that commands that never touch secrets never start e.g. the
// Bitwarden CLI.
func (r *Resolver) RegisterFactory(name string, f func() (SecretProvider, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = f
}

// Provider returns the provider for a scheme.
func (r *Resolver) Provider(name string) (SecretProvider, error) {
	r.mu.RLock()
	p, ok := r.providers[name]
	f, fok := r.factories[name]
	r.mu.RUnlock()
	if ok {
		return p, nil
	}
	if !fok {
		return nil, errs.New(errs.ErrInvalidConfiguration, "unknown secret provider %q (available: %s)", name, strings.Join(r.Names(), ", "))
	}
	p, err := f()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.providers[name] = p
	r.mu.Unlock()
	return p, nil
}

// Names lists registered provider names.
func (r *Resolver) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]bool{}
	for n := range r.providers {
		seen[n] = true
	}
	for n := range r.factories {
		seen[n] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Resolve implements SecretResolver.
func (r *Resolver) Resolve(ctx context.Context, ref SecretReference) (string, error) {
	p, err := r.Provider(ref.Provider)
	if err != nil {
		return "", err
	}
	return p.Get(ctx, ref.Key)
}

// Store writes a secret.
func (r *Resolver) Store(ctx context.Context, ref SecretReference, value string) error {
	p, err := r.Provider(ref.Provider)
	if err != nil {
		return err
	}
	return p.Set(ctx, ref.Key, value)
}

// Remove deletes a secret. Missing secrets are not an error.
func (r *Resolver) Remove(ctx context.Context, ref SecretReference) error {
	p, err := r.Provider(ref.Provider)
	if err != nil {
		return err
	}
	return p.Delete(ctx, ref.Key)
}

// NotFound builds a standard ErrSecretNotFound error that names the key but
// never a value.
func NotFound(provider, key string) error {
	return &errs.Error{Kind: errs.ErrSecretNotFound, Message: fmt.Sprintf("secret %q not found in %s", key, provider)}
}
