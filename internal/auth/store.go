package auth

import (
	"context"
	"sync"

	"github.com/SurajMazar/trove-cli/internal/errs"
)

// MemoryStore is an in-process Store. It is used for environment-variable
// credentials in CI (which must never be persisted) and in tests.
type MemoryStore struct {
	mu       sync.Mutex
	cred     *Credential
	ReadOnly bool // Save/Delete become no-ops (env credentials)
}

// NewMemoryStore returns a store holding c.
func NewMemoryStore(c Credential) *MemoryStore { return &MemoryStore{cred: &c} }

// TokenStore returns a store holding a plain token.
func TokenStore(token string) *MemoryStore {
	return NewMemoryStore(Credential{Kind: KindToken, Token: token})
}

// Load implements Store.
func (m *MemoryStore) Load(ctx context.Context) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cred == nil {
		return Credential{}, errs.New(errs.ErrNotAuthenticated, "no credential stored")
	}
	return *m.cred, nil
}

// Save implements Store.
func (m *MemoryStore) Save(ctx context.Context, c Credential) error {
	if m.ReadOnly {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cred = &c
	return nil
}

// Delete implements Store.
func (m *MemoryStore) Delete(ctx context.Context) error {
	if m.ReadOnly {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cred = nil
	return nil
}
