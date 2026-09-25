// Package auth holds provider-neutral authentication types.
//
// Each forge driver implements its own authentication flows (GitHub device
// flow, GitLab PAT, Bitbucket API tokens, ...). This package only defines the
// vocabulary shared between drivers and the application core, plus reusable
// building blocks such as the RFC 8628 device flow client.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Method is an authentication method a provider may support.
type Method string

const (
	MethodToken       Method = "token"        // personal / project / group access token
	MethodOAuth       Method = "oauth"        // OAuth 2.0 (device flow or pre-issued access token)
	MethodApp         Method = "app"          // GitHub App installation
	MethodBasic       Method = "basic"        // username + password-like token (e.g. Bitbucket API token)
	MethodAccessToken Method = "access-token" // Bitbucket repository/project/workspace access token
)

// CredentialKind describes what a stored credential contains.
type CredentialKind string

const (
	KindToken CredentialKind = "token"
	KindOAuth CredentialKind = "oauth"
	KindBasic CredentialKind = "basic"
	KindApp   CredentialKind = "app"
)

// Credential is a secret used to authenticate against a provider.
//
// Credential deliberately implements fmt.Stringer, fmt.GoStringer,
// json.Marshaler and slog.LogValuer to redact itself, so that accidentally
// printing, logging or serializing it never leaks the secret. Use Encode to
// obtain the storable representation.
type Credential struct {
	Kind         CredentialKind
	Token        string // access token, API token or app private key (PEM)
	RefreshToken string
	TokenType    string
	Username     string // for basic auth (e.g. Bitbucket email) or OAuth client ID
	ClientSecret string // OAuth client secret when the flow needs it to refresh
	Expiry       time.Time
	Scopes       []string
}

const redacted = "[REDACTED]"

func (c Credential) String() string   { return redacted }
func (c Credential) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (c Credential) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON prevents credentials from being serialized into output.
func (c Credential) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// Expired reports whether the credential has a known expiry in the past (with
// a small leeway).
func (c Credential) Expired() bool {
	return !c.Expiry.IsZero() && time.Now().Add(30*time.Second).After(c.Expiry)
}

// Refreshable reports whether the credential can be refreshed.
func (c Credential) Refreshable() bool { return c.RefreshToken != "" }

type storedCredential struct {
	Version      int            `json:"v"`
	Kind         CredentialKind `json:"kind"`
	Token        string         `json:"token,omitempty"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	TokenType    string         `json:"token_type,omitempty"`
	Username     string         `json:"username,omitempty"`
	ClientSecret string         `json:"client_secret,omitempty"`
	Expiry       *time.Time     `json:"expiry,omitempty"`
	Scopes       []string       `json:"scopes,omitempty"`
}

// Encode returns the value to place in a secret store. Plain tokens without
// metadata are stored verbatim so that users can populate secret stores by
// hand (e.g. paste a PAT into Bitwarden); richer credentials are stored as a
// small JSON document.
func (c Credential) Encode() string {
	if (c.Kind == KindToken || c.Kind == "") && c.RefreshToken == "" && c.Username == "" && c.ClientSecret == "" && c.Expiry.IsZero() && len(c.Scopes) == 0 {
		return c.Token
	}
	s := storedCredential{
		Version: 1, Kind: c.Kind, Token: c.Token, RefreshToken: c.RefreshToken,
		TokenType: c.TokenType, Username: c.Username, ClientSecret: c.ClientSecret, Scopes: c.Scopes,
	}
	if !c.Expiry.IsZero() {
		t := c.Expiry.UTC()
		s.Expiry = &t
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// DecodeCredential parses a stored secret value produced by Encode or entered
// by hand. A value that is not a Trove JSON document is treated as a token.
func DecodeCredential(v string) (Credential, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Credential{}, fmt.Errorf("stored credential is empty")
	}
	if strings.HasPrefix(v, "{") {
		var s storedCredential
		if err := json.Unmarshal([]byte(v), &s); err == nil && s.Version >= 1 {
			c := Credential{
				Kind: s.Kind, Token: s.Token, RefreshToken: s.RefreshToken,
				TokenType: s.TokenType, Username: s.Username, ClientSecret: s.ClientSecret, Scopes: s.Scopes,
			}
			if s.Expiry != nil {
				c.Expiry = *s.Expiry
			}
			if c.Kind == "" {
				c.Kind = KindToken
			}
			return c, nil
		}
		// Not ours: fall through and treat as an opaque token.
	}
	return Credential{Kind: KindToken, Token: v}, nil
}

// Store persists a single account's credential. The application core backs
// it with the configured secret provider; drivers use it to persist refreshed
// OAuth tokens without knowing where they are stored.
type Store interface {
	Load(ctx context.Context) (Credential, error)
	Save(ctx context.Context, c Credential) error
	Delete(ctx context.Context) error
}

// Request is the input to a provider login flow.
type Request struct {
	Method Method
	// Token is a pre-issued token supplied by the user (never logged).
	Token string
	// Username is required by some basic-auth methods.
	Username string
	// Scopes requested for OAuth flows.
	Scopes []string
	// ClientID / ClientSecret for OAuth; AppID / InstallationID for GitHub Apps.
	ClientID       string
	ClientSecret   string
	AppID          string
	InstallationID string
	// Prompter displays device codes and verification URLs.
	Prompter Prompter
}

// Prompter is how a login flow communicates with the user.
type Prompter interface {
	// DeviceCode is called when the user must visit a URL and enter a code.
	DeviceCode(verificationURI, userCode string, expiresIn time.Duration)
	// Info displays an informational message.
	Info(msg string)
}

// Result is what a successful login produced.
type Result struct {
	Credential Credential
	User       string // username the credential authenticates as
}

// Status describes the state of an account's authentication.
type Status struct {
	Authenticated bool      `json:"authenticated"`
	User          string    `json:"user,omitempty"`
	Method        Method    `json:"method,omitempty"`
	Scopes        []string  `json:"scopes,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitzero"`
	Source        string    `json:"source,omitempty"` // "secret", "env"
	Detail        string    `json:"detail,omitempty"`
}
