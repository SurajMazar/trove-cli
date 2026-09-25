package secrets

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/SurajMazar/trove-cli/internal/errs"
)

// EnvProvider reads secrets from environment variables: env://GITHUB_TOKEN.
// It is read-only by design; Trove never persists environment credentials.
type EnvProvider struct {
	lookup func(string) (string, bool)
}

// NewEnvProvider returns an EnvProvider backed by os.LookupEnv.
func NewEnvProvider() *EnvProvider { return &EnvProvider{lookup: os.LookupEnv} }

// NewEnvProviderWith returns an EnvProvider using a custom lookup (tests).
func NewEnvProviderWith(lookup func(string) (string, bool)) *EnvProvider {
	return &EnvProvider{lookup: lookup}
}

func (e *EnvProvider) Name() string        { return "env" }
func (e *EnvProvider) Description() string { return "Environment variables (read-only)" }

func (e *EnvProvider) Get(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	v, ok := e.lookup(envName(key))
	if !ok || v == "" {
		return "", NotFound("env", envName(key))
	}
	return v, nil
}

func (e *EnvProvider) Set(ctx context.Context, key, value string) error {
	return errs.New(errs.ErrInvalidArgument, "the env secret provider is read-only; export %s instead", envName(key))
}

func (e *EnvProvider) Delete(ctx context.Context, key string) error {
	return errs.New(errs.ErrInvalidArgument, "the env secret provider is read-only; unset %s instead", envName(key))
}

func (e *EnvProvider) Exists(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	v, ok := e.lookup(envName(key))
	return ok && v != "", nil
}

func envName(key string) string { return strings.TrimSpace(key) }

// MemoryProvider is an in-memory SecretProvider for tests.
type MemoryProvider struct {
	name string
	mu   sync.Mutex
	data map[string]string
}

// NewMemoryProvider returns an empty MemoryProvider named name.
func NewMemoryProvider(name string) *MemoryProvider {
	return &MemoryProvider{name: name, data: map[string]string{}}
}

func (m *MemoryProvider) Name() string { return m.name }

func (m *MemoryProvider) Get(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return "", NotFound(m.name, key)
	}
	return v, nil
}

func (m *MemoryProvider) Set(ctx context.Context, key, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

func (m *MemoryProvider) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *MemoryProvider) Exists(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[key]
	return ok, nil
}
