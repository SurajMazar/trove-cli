package errs

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestErrorIsKindAndCause(t *testing.T) {
	e := &Error{Kind: ErrRepositoryNotFound, Cause: ErrNotFound, Message: "repository acme/api not found"}
	if !errors.Is(e, ErrRepositoryNotFound) || !errors.Is(e, ErrNotFound) {
		t.Fatal("errors.Is should match kind and cause")
	}
	wrapped := fmt.Errorf("list: %w", Wrap(ErrProviderAPI, context.Canceled, "request failed"))
	if !errors.Is(wrapped, context.Canceled) || !errors.Is(wrapped, ErrProviderAPI) {
		t.Fatal("wrapped cause lost")
	}
}

func TestErrorMessage(t *testing.T) {
	e := &Error{Op: "list repositories", Kind: ErrRateLimited}
	if e.Error() != "list repositories: rate limited" {
		t.Fatalf("Error() = %q", e.Error())
	}
	e2 := Wrap(ErrGit, errors.New("exit 128"), "clone failed")
	if e2.Error() != "clone failed: exit 128" {
		t.Fatalf("Error() = %q", e2.Error())
	}
}

func TestWithProviderAndHelpers(t *testing.T) {
	e := WithProvider(New(ErrAuthenticationFailed, "bad token"), "gitlab-work")
	if ProviderOf(e) != "gitlab-work" {
		t.Fatalf("ProviderOf = %q", ProviderOf(e))
	}
	plain := WithProvider(errors.New("x"), "gh")
	if ProviderOf(plain) != "gh" {
		t.Fatal("plain error not annotated")
	}
	if WithProvider(nil, "gh") != nil {
		t.Fatal("nil should stay nil")
	}
	u := Unsupported("example", "pipelines")
	if u.Error() != `provider "example" does not support pipelines` || !errors.Is(u, ErrUnsupportedCapability) {
		t.Fatalf("Unsupported = %v", u)
	}
	h := &Error{Kind: ErrNotAuthenticated, Hint: "trove auth login gh"}
	if HintOf(fmt.Errorf("wrap: %w", h)) != "trove auth login gh" {
		t.Fatal("HintOf through wrapping failed")
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[error]int{
		nil: 0, ErrCanceled: 130, ErrAborted: 130, ErrInvalidArgument: 2, ErrInvalidConfiguration: 2,
		ErrAuthenticationFailed: 3, ErrNotAuthenticated: 3, ErrNotFound: 4, ErrRepositoryNotFound: 4,
		ErrUnsupportedCapability: 5, ErrRateLimited: 6, errors.New("x"): 1,
	}
	for err, want := range cases {
		if got := ExitCode(err); got != want {
			t.Errorf("ExitCode(%v) = %d, want %d", err, got, want)
		}
	}
}
