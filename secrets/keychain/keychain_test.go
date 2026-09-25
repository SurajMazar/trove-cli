package keychain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/secrets/secrettest"
)

// memBackend is an in-memory Backend keyed by service and account.
type memBackend struct {
	mu    sync.Mutex
	items map[[2]string]string
	err   error // returned by every call when set
	block chan struct{}
}

func newMem() *memBackend { return &memBackend{items: map[[2]string]string{}} }

func (m *memBackend) wait() {
	if m.block != nil {
		<-m.block
	}
}

func (m *memBackend) Set(service, user, pass string) error {
	m.wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.items[[2]string{service, user}] = pass
	return nil
}

func (m *memBackend) Get(service, user string) (string, error) {
	m.wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return "", m.err
	}
	v, ok := m.items[[2]string{service, user}]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (m *memBackend) Delete(service, user string) error {
	m.wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if _, ok := m.items[[2]string{service, user}]; !ok {
		return keyring.ErrNotFound
	}
	delete(m.items, [2]string{service, user})
	return nil
}

func TestContract(t *testing.T) {
	p, err := New(Options{Backend: newMem()})
	if err != nil {
		t.Fatal(err)
	}
	secrettest.RunSecretProviderContractTests(t, p)
}

func TestServiceAndAccount(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	p, _ := New(Options{Backend: m})
	pem := "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n"
	if err := p.Set(ctx, "trove/github/personal/token", pem); err != nil {
		t.Fatal(err)
	}
	if got := m.items[[2]string{"trove", "trove/github/personal/token"}]; got != pem {
		t.Fatalf("stored under unexpected service/account: %v", m.items)
	}
	custom, _ := New(Options{Service: "trove-work", Backend: m})
	if ok, _ := custom.Exists(ctx, "trove/github/personal/token"); ok {
		t.Fatal("services are not isolated")
	}
	if !strings.Contains(custom.Description(), "trove-work") || custom.Name() != "keychain" {
		t.Fatalf("name/description: %q %q", custom.Name(), custom.Description())
	}
}

func TestUnsupportedPlatform(t *testing.T) {
	ctx := context.Background()
	p := &Provider{service: DefaultService} // what New builds on non-darwin systems
	for _, err := range []error{
		p.Check(ctx),
		p.Set(ctx, "k", "v"),
		p.Delete(ctx, "k"),
		func() error { _, e := p.Get(ctx, "k"); return e }(),
		func() error { _, e := p.Exists(ctx, "k"); return e }(),
	} {
		if !errors.Is(err, errs.ErrSecretUnavailable) || !strings.Contains(err.Error(), "macOS Keychain is only available on macOS") {
			t.Fatalf("want ErrSecretUnavailable, got %v", err)
		}
	}
	real, _ := New(Options{})
	if runtime.GOOS != "darwin" {
		if err := real.Check(ctx); !errors.Is(err, errs.ErrSecretUnavailable) {
			t.Fatalf("Check on %s: %v", runtime.GOOS, err)
		}
	} else if err := real.Check(ctx); err != nil {
		t.Fatalf("Check on macOS: %v", err)
	}
}

// exitError produces a real *exec.ExitError with the given status.
func exitError(t *testing.T, code int) error {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected ExitError, got %v", err)
	}
	return err
}

func TestErrorMapping(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
		kind error
		hint string
	}{
		{"locked keychain", exitError(t, 36), errs.ErrSecretProviderLocked, unlockHint},
		{"auth failed", exitError(t, 51), errs.ErrSecretProviderLocked, unlockHint},
		{"user canceled", exitError(t, 128), errs.ErrPermissionDenied, ""},
		{"item not found status", exitError(t, 44), errs.ErrSecretNotFound, ""},
		{"other status", exitError(t, 9), errs.ErrSecretUnavailable, ""},
		{"security missing", &exec.Error{Name: securityPath, Err: exec.ErrNotFound}, errs.ErrSecretUnavailable, ""},
		{"too big", keyring.ErrSetDataTooBig, errs.ErrInvalidArgument, ""},
		{"unsupported platform", keyring.ErrUnsupportedPlatform, errs.ErrSecretUnavailable, ""},
		{"not found", keyring.ErrNotFound, errs.ErrSecretNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMem()
			m.err = tc.err
			p, _ := New(Options{Backend: m})
			err := p.Set(ctx, "trove/a/b/token", "value-that-must-not-leak")
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
			if tc.hint != "" && errs.HintOf(err) != tc.hint {
				t.Errorf("hint = %q", errs.HintOf(err))
			}
			if strings.Contains(err.Error(), "value-that-must-not-leak") {
				t.Fatal("error leaks value")
			}
		})
	}
}

func TestUnknownBackendErrorIsScrubbed(t *testing.T) {
	m := newMem()
	m.err = errors.New("backend echoed value-that-must-not-leak")
	p, _ := New(Options{Backend: m})
	err := p.Set(context.Background(), "k", "value-that-must-not-leak")
	if !errors.Is(err, errs.ErrSecretUnavailable) || strings.Contains(err.Error(), "value-that-must-not-leak") {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteMissingAndNotFoundStatus(t *testing.T) {
	m := newMem()
	m.err = exitError(t, 44)
	p, _ := New(Options{Backend: m})
	if err := p.Delete(context.Background(), "k"); err != nil {
		t.Fatalf("Delete(missing) = %v", err)
	}
	if ok, err := p.Exists(context.Background(), "k"); ok || err != nil {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
}

func TestRejectsControlCharacters(t *testing.T) {
	p, _ := New(Options{Backend: newMem()})
	if err := p.Set(context.Background(), "trove/a\nb", "v"); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("key with newline: %v", err)
	}
	if _, err := p.Get(context.Background(), " "); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("empty key: %v", err)
	}
	if _, err := New(Options{Service: "tro\x00ve"}); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("service with NUL: %v", err)
	}
}

func TestStopsWaitingWhenContextIsDone(t *testing.T) {
	m := newMem()
	m.block = make(chan struct{})
	defer close(m.block)
	p, _ := New(Options{Backend: m})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Get(ctx, "k")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("did not stop waiting")
	}
}

// TestRealKeychain exercises the real login keychain. It is opt-in because it
// writes to the user's keychain and may show a prompt.
func TestRealKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("TROVE_KEYCHAIN_INTEGRATION") != "1" {
		t.Skip("set TROVE_KEYCHAIN_INTEGRATION=1 on macOS to run against the real Keychain")
	}
	p, err := New(Options{Service: "trove-integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	secrettest.RunSecretProviderContractTests(t, p)
	ctx := context.Background()
	pem := "-----BEGIN PRIVATE KEY-----\nline1\nline2\n-----END PRIVATE KEY-----\n"
	key := fmt.Sprintf("trove-integration/%d/pem", time.Now().UnixNano())
	defer p.Delete(ctx, key)
	if err := p.Set(ctx, key, pem); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Get(ctx, key); err != nil || got != pem {
		t.Fatalf("PEM did not round-trip: %v", err)
	}
}
