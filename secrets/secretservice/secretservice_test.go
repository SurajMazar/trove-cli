package secretservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	dbus "github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"

	"github.com/SurajMazar/trove-cli/internal/errs"
	"github.com/SurajMazar/trove-cli/internal/secrets/secrettest"
)

// memBackend is an in-memory Backend keyed by service and user attributes.
type memBackend struct {
	mu       sync.Mutex
	items    map[[2]string]string
	err      error
	probeErr error
	block    chan struct{}
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

// probingMem also implements Prober.
type probingMem struct{ *memBackend }

func (p probingMem) Probe() error { return p.probeErr }

func TestContract(t *testing.T) {
	p, err := New(Options{Backend: newMem()})
	if err != nil {
		t.Fatal(err)
	}
	secrettest.RunSecretProviderContractTests(t, p)
}

func TestAttributesAndExactValues(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	p, _ := New(Options{Backend: m})
	values := []string{
		"-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----\n",
		`{"a":"b","n":[1,2]}`,
		"  padded  ",
	}
	for _, v := range values {
		if err := p.Set(ctx, "trove/gitlab/work/token", v); err != nil {
			t.Fatal(err)
		}
		if got, err := p.Get(ctx, "trove/gitlab/work/token"); err != nil || got != v {
			t.Fatalf("round trip failed: %q %v", got, err)
		}
	}
	if _, ok := m.items[[2]string{"trove", "trove/gitlab/work/token"}]; !ok {
		t.Fatalf("unexpected attributes: %v", m.items)
	}
	other, _ := New(Options{Service: "other", Backend: m})
	if ok, _ := other.Exists(ctx, "trove/gitlab/work/token"); ok {
		t.Fatal("services are not isolated")
	}
	if other.Name() != "secretservice" || !strings.Contains(other.Description(), `"other"`) {
		t.Fatalf("name/description: %q %q", other.Name(), other.Description())
	}
}

func TestUnsupportedPlatform(t *testing.T) {
	ctx := context.Background()
	p := &Provider{service: DefaultService} // what New builds off Linux
	for _, err := range []error{
		p.Check(ctx),
		p.Set(ctx, "k", "v"),
		p.Delete(ctx, "k"),
		func() error { _, e := p.Get(ctx, "k"); return e }(),
		func() error { _, e := p.Exists(ctx, "k"); return e }(),
	} {
		if !errors.Is(err, errs.ErrSecretUnavailable) || !strings.Contains(err.Error(), "only available on Linux") {
			t.Fatalf("want ErrSecretUnavailable, got %v", err)
		}
	}
	if runtime.GOOS != "linux" {
		real, _ := New(Options{})
		if err := real.Check(ctx); !errors.Is(err, errs.ErrSecretUnavailable) {
			t.Fatalf("Check on %s: %v", runtime.GOOS, err)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		kind     error
		contains string
		hint     string
	}{
		{"not found", keyring.ErrNotFound, errs.ErrSecretNotFound, "not found", ""},
		{"no session bus preflight", errNoSessionBus, errs.ErrSecretUnavailable, "no D-Bus session bus", installHint},
		{"no address", errors.New("dbus: couldn't determine address of session bus"), errs.ErrSecretUnavailable, "session bus", installHint},
		{"autolaunch failed", errors.New("dbus: couldn't determine address of session bus: exec: \"dbus-launch\": executable file not found in $PATH"), errs.ErrSecretUnavailable, "session bus", installHint},
		{"socket gone", errors.New("dial unix /run/user/1000/bus: connect: no such file or directory"), errs.ErrSecretUnavailable, "session bus", installHint},
		{"service unknown value", dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown", Body: []any{"The name org.freedesktop.secrets was not provided by any .service files"}}, errs.ErrSecretUnavailable, "org.freedesktop.secrets", installHint},
		{"service unknown pointer", &dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}, errs.ErrSecretUnavailable, "org.freedesktop.secrets", installHint},
		{"service unknown wrapped", fmt.Errorf("call: %w", dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}), errs.ErrSecretUnavailable, "org.freedesktop.secrets", installHint},
		{"service unknown text", errors.New("The name org.freedesktop.secrets was not provided by any .service files"), errs.ErrSecretUnavailable, "org.freedesktop.secrets", installHint},
		{"spawn failed", dbus.Error{Name: "org.freedesktop.DBus.Error.Spawn.ChildExited"}, errs.ErrSecretUnavailable, "org.freedesktop.secrets", installHint},
		{"prompt dismissed", errors.New("failed to unlock correct collection '/org/freedesktop/secrets/aliases/default'"), errs.ErrSecretProviderLocked, "locked", unlockHint},
		{"is locked", dbus.Error{Name: "org.freedesktop.Secret.Error.IsLocked", Body: []any{"Cannot get secret of a locked object"}}, errs.ErrSecretProviderLocked, "locked", unlockHint},
		{"no collection", dbus.Error{Name: "org.freedesktop.Secret.Error.NoSuchObject", Body: []any{"The collection does not exist"}}, errs.ErrSecretUnavailable, "default keyring collection", ""},
		{"unknown object", dbus.Error{Name: "org.freedesktop.DBus.Error.UnknownObject"}, errs.ErrSecretUnavailable, "default keyring collection", ""},
		{"no reply", dbus.Error{Name: "org.freedesktop.DBus.Error.NoReply"}, errs.ErrSecretUnavailable, "did not respond", ""},
		{"access denied", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}, errs.ErrPermissionDenied, "access denied", ""},
		{"other", errors.New("something odd happened"), errs.ErrSecretUnavailable, "something odd happened", installHint},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMem()
			m.err = tc.err
			p, _ := New(Options{Backend: m})
			_, err := p.Get(context.Background(), "trove/gitlab/work/token")
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err, tc.contains)
			}
			if tc.hint != "" && errs.HintOf(err) != tc.hint && !strings.HasSuffix(errs.HintOf(err), tc.hint) {
				t.Errorf("hint = %q, want %q", errs.HintOf(err), tc.hint)
			}
		})
	}
}

func TestDeleteMissingAndErrors(t *testing.T) {
	ctx := context.Background()
	m := newMem()
	p, _ := New(Options{Backend: m})
	if err := p.Delete(ctx, "missing"); err != nil {
		t.Fatalf("Delete(missing) = %v", err)
	}
	m.err = errors.New("failed to unlock correct collection '/org/freedesktop/secrets/collection/login'")
	if err := p.Delete(ctx, "k"); !errors.Is(err, errs.ErrSecretProviderLocked) {
		t.Fatalf("Delete on locked keyring: %v", err)
	}
	if _, err := p.Exists(ctx, "k"); !errors.Is(err, errs.ErrSecretProviderLocked) {
		t.Fatalf("Exists on locked keyring: %v", err)
	}
}

func TestSetErrorScrubsValue(t *testing.T) {
	m := newMem()
	m.err = errors.New("daemon echoed value-that-must-not-leak")
	p, _ := New(Options{Backend: m})
	err := p.Set(context.Background(), "k", "value-that-must-not-leak")
	if err == nil || strings.Contains(err.Error(), "value-that-must-not-leak") {
		t.Fatalf("got %v", err)
	}
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	plain, _ := New(Options{Backend: newMem()})
	if err := plain.Check(ctx); err != nil {
		t.Fatalf("Check without prober: %v", err)
	}
	pm := probingMem{newMem()}
	p, _ := New(Options{Backend: pm})
	if err := p.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	pm.probeErr = dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}
	if err := p.Check(ctx); !errors.Is(err, errs.ErrSecretUnavailable) || errs.HintOf(err) != installHint {
		t.Fatalf("Check with no daemon: %v", err)
	}
	pm.probeErr = errNoSessionBus
	if err := p.Check(ctx); !errors.Is(err, errs.ErrSecretUnavailable) || !strings.Contains(err.Error(), "DBUS_SESSION_BUS_ADDRESS") {
		t.Fatalf("Check with no bus: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.Check(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Check canceled: %v", err)
	}
}

func TestPreflight(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		files map[string]bool
		ok    bool
	}{
		{"address set", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, nil, true},
		{"systemd user bus", nil, map[string]bool{"/run/user/1000/bus": true}, true},
		{"dbus-session file", nil, map[string]bool{"/run/user/1000/dbus-session": true}, true},
		{"autolaunch only", map[string]string{"DBUS_SESSION_BUS_ADDRESS": "autolaunch:"}, nil, false},
		{"nothing", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &dbusBackend{
				getenv: func(k string) string { return tc.env[k] },
				exists: func(p string) bool { return tc.files[p] },
				uid:    func() string { return "1000" },
			}
			err := d.preflight()
			if tc.ok != (err == nil) {
				t.Fatalf("preflight = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok {
				// Every entry point fails fast without touching D-Bus.
				for _, e := range []error{d.Set("s", "u", "p"), d.Delete("s", "u"), d.Probe(), func() error { _, e := d.Get("s", "u"); return e }()} {
					if !errors.Is(e, errNoSessionBus) {
						t.Fatalf("want errNoSessionBus, got %v", e)
					}
				}
			}
		})
	}
}

func TestStopsWaitingWhenContextIsDone(t *testing.T) {
	m := newMem()
	m.block = make(chan struct{})
	defer close(m.block)
	p, _ := New(Options{Backend: m})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.Set(ctx, "k", "v"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
}

func TestInvalidInput(t *testing.T) {
	p, _ := New(Options{Backend: newMem()})
	if _, err := p.Get(context.Background(), ""); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("empty key: %v", err)
	}
	if _, err := New(Options{Service: "a\x00b"}); !errors.Is(err, errs.ErrInvalidConfiguration) {
		t.Fatalf("NUL service: %v", err)
	}
}

// TestRealSecretService runs the contract suite against the session's Secret
// Service. It is opt-in because it writes to the user's keyring.
func TestRealSecretService(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("TROVE_SECRETSERVICE_INTEGRATION") != "1" {
		t.Skip("set TROVE_SECRETSERVICE_INTEGRATION=1 on Linux to run against the real Secret Service")
	}
	p, err := New(Options{Service: "trove-integration-test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	secrettest.RunSecretProviderContractTests(t, p)
}
