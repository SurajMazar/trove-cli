// Package secrettest provides a contract test suite for SecretProvider
// implementations. It never uses real credentials.
package secrettest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/secrets"
)

// RunSecretProviderContractTests exercises Set/Get/Exists/Delete, missing
// secrets and context cancellation.
func RunSecretProviderContractTests(t *testing.T, p secrets.SecretProvider) {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("trove-contract/%d", time.Now().UnixNano())
	key := prefix + "/github/test/token"
	const value = "fake-value-for-contract-tests"

	t.Run("name", func(t *testing.T) {
		if strings.TrimSpace(p.Name()) == "" {
			t.Fatal("provider name is empty")
		}
	})

	t.Run("missing secret", func(t *testing.T) {
		_, err := p.Get(ctx, prefix+"/does-not-exist")
		if !errors.Is(err, errs.ErrSecretNotFound) {
			t.Fatalf("Get(missing): want ErrSecretNotFound, got %v", err)
		}
		ok, err := p.Exists(ctx, prefix+"/does-not-exist")
		if err != nil || ok {
			t.Fatalf("Exists(missing) = %v, %v; want false, nil", ok, err)
		}
		if err := p.Delete(ctx, prefix+"/does-not-exist"); err != nil {
			t.Fatalf("Delete(missing) should be idempotent, got %v", err)
		}
	})

	t.Run("set get exists delete", func(t *testing.T) {
		if err := p.Set(ctx, key, value); err != nil {
			t.Fatalf("Set: %v", err)
		}
		got, err := p.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != value {
			t.Fatalf("Get returned a different value (len %d, want %d)", len(got), len(value))
		}
		ok, err := p.Exists(ctx, key)
		if err != nil || !ok {
			t.Fatalf("Exists = %v, %v; want true, nil", ok, err)
		}
		// Overwrite.
		if err := p.Set(ctx, key, value+"-2"); err != nil {
			t.Fatalf("Set (overwrite): %v", err)
		}
		got, err = p.Get(ctx, key)
		if err != nil || got != value+"-2" {
			t.Fatalf("Get after overwrite failed: err=%v match=%v", err, got == value+"-2")
		}
		if err := p.Delete(ctx, key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		ok, err = p.Exists(ctx, key)
		if err != nil || ok {
			t.Fatalf("Exists after delete = %v, %v; want false, nil", ok, err)
		}
		if _, err := p.Get(ctx, key); !errors.Is(err, errs.ErrSecretNotFound) {
			t.Fatalf("Get after delete: want ErrSecretNotFound, got %v", err)
		}
	})

	t.Run("errors never contain the value", func(t *testing.T) {
		if err := p.Set(ctx, key, value); err != nil {
			t.Fatalf("Set: %v", err)
		}
		defer p.Delete(ctx, key)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		for _, err := range []error{
			func() error { _, e := p.Get(cctx, key); return e }(),
			p.Set(cctx, key, value),
		} {
			if err != nil && strings.Contains(err.Error(), value) {
				t.Fatalf("error leaks secret value: %v", err)
			}
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := p.Get(cctx, key); err == nil {
			t.Error("Get with canceled context succeeded")
		} else if !errors.Is(err, context.Canceled) {
			t.Errorf("Get: want context.Canceled, got %v", err)
		}
		if err := p.Set(cctx, key, value); err == nil {
			t.Error("Set with canceled context succeeded")
		}
		if _, err := p.Exists(cctx, key); err == nil {
			t.Error("Exists with canceled context succeeded")
		}
	})
}
