// Package errs defines Trove's typed errors.
//
// Every error that crosses a package boundary should be (or wrap) one of the
// sentinel kinds below so that the CLI can render consistent, actionable
// messages and pick the correct exit code. Errors must never contain secret
// material: tokens, passwords, Authorization headers or cookie values.
package errs

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel error kinds. Use errors.Is to test for them.
var (
	ErrProviderNotFound      = errors.New("provider not found")
	ErrDriverNotFound        = errors.New("provider type not supported")
	ErrUnsupportedCapability = errors.New("unsupported capability")
	ErrAuthenticationFailed  = errors.New("authentication failed")
	ErrNotAuthenticated      = errors.New("not authenticated")
	ErrSecretNotFound        = errors.New("secret not found")
	ErrSecretProviderLocked  = errors.New("secret provider locked")
	ErrSecretUnavailable     = errors.New("secret provider unavailable")
	ErrRepositoryNotFound    = errors.New("repository not found")
	ErrNotFound              = errors.New("not found")
	ErrPermissionDenied      = errors.New("permission denied")
	ErrRateLimited           = errors.New("rate limited")
	ErrInvalidConfiguration  = errors.New("invalid configuration")
	ErrInvalidArgument       = errors.New("invalid argument")
	ErrConflict              = errors.New("conflict")
	ErrCanceled              = errors.New("operation canceled")
	ErrInteractionRequired   = errors.New("interactive input required")
	ErrAborted               = errors.New("aborted by user")
	ErrProviderAPI           = errors.New("provider API error")
	ErrGit                   = errors.New("git command failed")
)

// Error is a structured error carrying context for display. The Kind is one
// of the sentinel errors above; Unwrap exposes both Kind and the cause so that
// errors.Is works against either.
type Error struct {
	Kind     error
	Provider string // local provider alias, e.g. "gitlab-work"
	Op       string // short operation description, e.g. "list repositories"
	Message  string // human readable detail (must not contain secrets)
	Hint     string // actionable suggestion, e.g. "trove auth login gitlab-work"
	Status   int    // HTTP status when applicable
	Cause    error
	// RetryAfter is set for rate limit errors when the provider told us.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	switch {
	case e.Message != "":
		b.WriteString(e.Message)
	case e.Kind != nil:
		b.WriteString(e.Kind.Error())
	case e.Cause != nil:
		b.WriteString(e.Cause.Error())
	default:
		b.WriteString("unknown error")
	}
	if e.Message != "" && e.Cause != nil && !strings.Contains(e.Message, e.Cause.Error()) {
		b.WriteString(": ")
		b.WriteString(e.Cause.Error())
	}
	return b.String()
}

// Unwrap returns the kind and cause so errors.Is/As see both.
func (e *Error) Unwrap() []error {
	out := make([]error, 0, 2)
	if e.Kind != nil {
		out = append(out, e.Kind)
	}
	if e.Cause != nil {
		out = append(out, e.Cause)
	}
	return out
}

// New constructs an *Error of the given kind.
func New(kind error, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// Wrap attaches a kind and message to a cause.
func Wrap(kind error, cause error, format string, args ...any) *Error {
	return &Error{Kind: kind, Cause: cause, Message: fmt.Sprintf(format, args...)}
}

// WithProvider returns err annotated with the provider alias. If err is not an
// *Error it is wrapped with ErrProviderAPI semantics preserved via errors.Is.
func WithProvider(err error, provider string) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		if e.Provider == "" {
			cp := *e
			cp.Provider = provider
			return &cp
		}
		return err
	}
	return &Error{Provider: provider, Cause: err}
}

// Unsupported returns an ErrUnsupportedCapability error with the canonical
// message: Provider "x" does not support <feature>.
func Unsupported(provider, feature string) *Error {
	return &Error{
		Kind:     ErrUnsupportedCapability,
		Provider: provider,
		Message:  fmt.Sprintf("provider %q does not support %s", provider, feature),
	}
}

// Hint returns the hint attached to err, if any.
func HintOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Hint
	}
	return ""
}

// ProviderOf returns the provider alias attached to err, if any.
func ProviderOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Provider
	}
	return ""
}

// ExitCode maps an error to a process exit code.
//
//	0 success, 1 generic failure, 2 usage/config, 3 auth, 4 not found,
//	5 unsupported, 6 rate limited, 130 canceled.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrCanceled), errors.Is(err, ErrAborted):
		return 130
	case errors.Is(err, ErrInvalidConfiguration), errors.Is(err, ErrInvalidArgument),
		errors.Is(err, ErrInteractionRequired):
		return 2
	case errors.Is(err, ErrAuthenticationFailed), errors.Is(err, ErrNotAuthenticated),
		errors.Is(err, ErrPermissionDenied), errors.Is(err, ErrSecretProviderLocked):
		return 3
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrRepositoryNotFound),
		errors.Is(err, ErrProviderNotFound), errors.Is(err, ErrSecretNotFound):
		return 4
	case errors.Is(err, ErrUnsupportedCapability), errors.Is(err, ErrDriverNotFound):
		return 5
	case errors.Is(err, ErrRateLimited):
		return 6
	default:
		return 1
	}
}
