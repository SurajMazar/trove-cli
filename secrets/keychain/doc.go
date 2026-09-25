// Package keychain implements a Trove secret provider backed by the macOS
// Keychain.
//
// References look like
//
//	keychain://trove/github/personal/token
//
// Each key is stored as a generic password item in the user's default
// (login) keychain with service = Options.Service (default "trove") and
// account = key. Items can be inspected with Keychain Access or
//
//	security find-generic-password -s trove -a trove/github/personal/token
//
// The provider uses github.com/zalando/go-keyring, which drives
// /usr/bin/security. Writes go through `security -i` with the command
// (including the value) written to its standard input, so values never appear
// in the process list. go-keyring stores values base64-encoded with a
// "go-keyring-base64:" prefix so that multi-line values (PEM keys) and JSON
// round-trip exactly; items written by other tools are returned with
// surrounding whitespace trimmed. The security tool limits one command to
// 4096 bytes, so values larger than roughly 3000 bytes are rejected with
// errs.ErrInvalidArgument; use the bitwarden provider for those.
//
// # Configuration
//
//	secrets:
//	  provider: keychain
//	  keychain:
//	    service: trove
//
// # Usage
//
//	p, err := keychain.New(keychain.Options{Service: "trove"})
//	if err != nil { ... }
//	err = p.Set(ctx, "trove/github/personal/token", token)
//	token, err = p.Get(ctx, "trove/github/personal/token")
//
// # Platform and cancellation
//
// On systems other than macOS every operation returns
// errs.ErrSecretUnavailable. A locked keychain that cannot prompt (for example
// over SSH) yields errs.ErrSecretProviderLocked with a `security
// unlock-keychain` hint.
//
// The underlying security calls cannot be canceled. The provider checks the
// context before each call and stops waiting as soon as the context is done
// (for example while macOS shows an access prompt), but a call that has
// already started may still complete in the background.
package keychain
