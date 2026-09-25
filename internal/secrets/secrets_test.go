package secrets_test

import (
	"context"
	"errors"
	"testing"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/secrets"
	"github.com/SurajMazar/trove-cli/internal/secrets/secrettest"
)

func TestMemoryProviderContract(t *testing.T) {
	secrettest.RunSecretProviderContractTests(t, secrets.NewMemoryProvider("mem"))
}

func TestParseReference(t *testing.T) {
	r, err := secrets.ParseReference("bitwarden://trove/github/personal/token")
	if err != nil || r.Provider != "bitwarden" || r.Key != "trove/github/personal/token" {
		t.Fatalf("ParseReference = %+v, %v", r, err)
	}
	if r.String() != "bitwarden://trove/github/personal/token" {
		t.Fatalf("String = %s", r.String())
	}
	if r, _ := secrets.ParseReference("KeyChain://a%2Fb"); r.Provider != "keychain" || r.Key != "a/b" {
		t.Fatalf("normalization: %+v", r)
	}
	for _, bad := range []string{"", "no-scheme", "://key", "bitwarden://", "bad scheme://k"} {
		if _, err := secrets.ParseReference(bad); !errors.Is(err, errs.ErrInvalidConfiguration) {
			t.Errorf("ParseReference(%q) = %v", bad, err)
		}
	}
}

func TestResolverDispatchAndLazyFactories(t *testing.T) {
	r := secrets.NewResolver()
	mem := secrets.NewMemoryProvider("mem")
	r.Register(mem)
	built := 0
	r.RegisterFactory("lazy", func() (secrets.SecretProvider, error) {
		built++
		return secrets.NewMemoryProvider("lazy"), nil
	})
	if built != 0 {
		t.Fatal("factory ran eagerly")
	}
	ctx := context.Background()
	ref := secrets.SecretReference{Provider: "lazy", Key: "k"}
	if err := r.Store(ctx, ref, "v"); err != nil {
		t.Fatal(err)
	}
	if v, err := r.Resolve(ctx, ref); err != nil || v != "v" {
		t.Fatalf("Resolve = %q %v", v, err)
	}
	if built != 1 {
		t.Fatalf("factory ran %d times", built)
	}
	if err := r.Remove(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, ref); !errors.Is(err, errs.ErrSecretNotFound) {
		t.Fatalf("after remove: %v", err)
	}
	if _, err := r.Resolve(ctx, secrets.SecretReference{Provider: "nope", Key: "k"}); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("unknown provider: %v", err)
	}
	if names := r.Names(); len(names) != 2 || names[0] != "lazy" || names[1] != "mem" {
		t.Fatalf("Names = %v", names)
	}
}

func TestEnvProviderIsReadOnly(t *testing.T) {
	env := map[string]string{"GITHUB_TOKEN": "abc"}
	p := secrets.NewEnvProviderWith(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	ctx := context.Background()
	if v, err := p.Get(ctx, "GITHUB_TOKEN"); err != nil || v != "abc" {
		t.Fatalf("Get = %q %v", v, err)
	}
	if _, err := p.Get(ctx, "MISSING"); !errors.Is(err, errs.ErrSecretNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if ok, _ := p.Exists(ctx, "GITHUB_TOKEN"); !ok {
		t.Fatal("Exists false")
	}
	if err := p.Set(ctx, "X", "y"); err == nil {
		t.Fatal("env provider accepted a write")
	}
	if err := p.Delete(ctx, "X"); err == nil {
		t.Fatal("env provider accepted a delete")
	}
}

func TestDefaultKey(t *testing.T) {
	if k := secrets.DefaultKey("github", "personal", ""); k != "trove/github/personal/token" {
		t.Fatalf("DefaultKey = %s", k)
	}
}
