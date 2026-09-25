// Package secretservice implements a Trove secret provider backed by the
// freedesktop.org Secret Service API (org.freedesktop.secrets over the D-Bus
// session bus), which is what libsecret, GNOME Keyring, KDE Wallet (5.97+)
// and KeePassXC's Secret Service integration provide on Linux.
//
// References look like
//
//	secretservice://trove/gitlab/work/token
//
// Each key is stored as an item in the default collection (the "login"
// keyring when present) with the attributes service = Options.Service
// (default "trove") and username = key, the layout used by
// github.com/zalando/go-keyring. Items can be inspected with
//
//	secret-tool search service trove username trove/gitlab/work/token
//
// Values are transferred over D-Bus (never via argv or temporary files) and
// round-trip byte for byte, so multi-line PEM keys and JSON are fine.
//
// # Configuration
//
//	secrets:
//	  provider: secretservice
//	  secretservice:
//	    service: trove
//
// # Usage
//
//	p, err := secretservice.New(secretservice.Options{Service: "trove"})
//	if err != nil { ... }
//	if err := p.Check(ctx); err != nil { ... } // no session bus, no keyring daemon, ...
//	err = p.Set(ctx, "trove/gitlab/work/token", token)
//
// # Environments
//
// On systems other than Linux every operation returns
// errs.ErrSecretUnavailable. On Linux the provider first makes sure a session
// bus can be found (DBUS_SESSION_BUS_ADDRESS or /run/user/<uid>/bus) instead
// of letting the D-Bus library try to autolaunch one, then maps typical
// failures to actionable errors:
//
//   - no session bus, or nothing providing org.freedesktop.secrets:
//     errs.ErrSecretUnavailable ("install and start gnome-keyring or KeePassXC
//     with Secret Service integration");
//   - a locked collection whose unlock prompt was dismissed or could not be
//     shown: errs.ErrSecretProviderLocked.
//
// Check only verifies that the service is reachable (it opens and closes a
// Secret Service session); it never triggers an unlock prompt, so a locked
// keyring is reported by the first real operation.
//
// # Cancellation
//
// The D-Bus calls, and in particular an unlock prompt waiting for the user,
// cannot be canceled. The provider checks the context before each call and
// stops waiting when it is done, but the abandoned call may still complete in
// the background.
package secretservice
