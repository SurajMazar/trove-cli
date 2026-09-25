package secretservice

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/zalando/go-keyring"
	ss "github.com/zalando/go-keyring/secret_service"
)

// errNoSessionBus reports that no D-Bus session bus address can be found.
var errNoSessionBus = errors.New("no D-Bus session bus address found")

// dbusBackend is the default Linux backend: go-keyring's Secret Service
// implementation, guarded by a session bus preflight.
type dbusBackend struct {
	getenv func(string) string
	exists func(string) bool
	uid    func() string
}

func newDBusBackend() *dbusBackend {
	return &dbusBackend{
		getenv: os.Getenv,
		exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		uid:    func() string { return strconv.Itoa(os.Getuid()) },
	}
}

// preflight mirrors how the D-Bus library locates the session bus
// (DBUS_SESSION_BUS_ADDRESS, then /run/user/<uid>/bus or
// /run/user/<uid>/dbus-session) and fails early instead of letting it
// autolaunch a private bus with dbus-launch, on which no keyring daemon could
// be running anyway.
func (d *dbusBackend) preflight() error {
	if a := d.getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" && a != "autolaunch:" {
		return nil
	}
	dir := filepath.Join("/run/user", d.uid())
	if d.exists(filepath.Join(dir, "bus")) || d.exists(filepath.Join(dir, "dbus-session")) {
		return nil
	}
	return errNoSessionBus
}

func (d *dbusBackend) Set(service, user, pass string) error {
	if err := d.preflight(); err != nil {
		return err
	}
	return keyring.Set(service, user, pass)
}

func (d *dbusBackend) Get(service, user string) (string, error) {
	if err := d.preflight(); err != nil {
		return "", err
	}
	return keyring.Get(service, user)
}

func (d *dbusBackend) Delete(service, user string) error {
	if err := d.preflight(); err != nil {
		return err
	}
	return keyring.Delete(service, user)
}

// Probe checks that a Secret Service is reachable by opening and closing a
// session. Unlike item operations it never unlocks a collection, so it cannot
// trigger a password prompt.
func (d *dbusBackend) Probe() error {
	if err := d.preflight(); err != nil {
		return err
	}
	svc, err := ss.NewSecretService()
	if err != nil {
		return err
	}
	session, err := svc.OpenSession()
	if err != nil {
		return err
	}
	return svc.Close(session)
}
