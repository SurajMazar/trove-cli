// Package redact removes secret material from strings, headers and URLs
// before they reach logs, errors or terminal output.
package redact

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Placeholder replaces redacted values.
const Placeholder = "[REDACTED]"

var sensitiveHeaders = map[string]bool{
	"authorization":         true,
	"proxy-authorization":   true,
	"cookie":                true,
	"set-cookie":            true,
	"private-token":         true,
	"job-token":             true,
	"x-api-key":             true,
	"x-auth-token":          true,
	"token":                 true,
	"x-gitlab-token":        true,
	"x-hub-signature":       true,
	"x-hub-signature-256":   true,
	"bws-access-token":      true,
	"x-bitwarden-api-token": true,
}

// IsSensitiveHeader reports whether a header carries credentials.
func IsSensitiveHeader(name string) bool {
	n := strings.ToLower(name)
	if sensitiveHeaders[n] {
		return true
	}
	return strings.Contains(n, "token") || strings.Contains(n, "secret") || strings.Contains(n, "password")
}

// Headers returns a copy of h with sensitive values replaced.
func Headers(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		if IsSensitiveHeader(k) {
			out[k] = []string{Placeholder}
			continue
		}
		cp := make([]string, len(v))
		for i, s := range v {
			cp[i] = String(s)
		}
		out[k] = cp
	}
	return out
}

var sensitiveParams = []string{
	"access_token", "private_token", "token", "refresh_token", "client_secret",
	"password", "code", "device_code", "key", "secret", "signature", "sig", "jwt",
}

// URL returns u with userinfo and sensitive query parameters redacted.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return String(raw)
	}
	if u.User != nil {
		u.User = url.User(Placeholder)
	}
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			lk := strings.ToLower(k)
			for _, p := range sensitiveParams {
				if lk == p || strings.Contains(lk, "token") || strings.Contains(lk, "secret") {
					q.Set(k, Placeholder)
					break
				}
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// Known token formats. Matching is best-effort defense in depth: code paths
// should never put secrets into strings in the first place.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{20,}`),                                   // GitHub classic tokens
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),                                  // GitHub fine-grained
	regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{16,}`),                                     // GitLab PAT
	regexp.MustCompile(`gl(?:oas|dt|rt|cbt|ptt|ft|imt|agent|soat)-[A-Za-z0-9_\-]{16,}`), // other GitLab tokens
	regexp.MustCompile(`ATBB[A-Za-z0-9_\-=]{20,}`),                                      // Bitbucket app password
	regexp.MustCompile(`ATAT[A-Za-z0-9_\-=]{20,}`),                                      // Atlassian API token
	regexp.MustCompile(`ATCTT[A-Za-z0-9_\-=]{20,}`),                                     // Bitbucket access token
	regexp.MustCompile(`(?i)(bearer|token|basic)\s+[A-Za-z0-9._~+/\-=]{8,}`),            // auth header values
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`0\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.[A-Za-z0-9+/=:]{20,}`), // bws access token
}

// String masks known token formats inside s.
func String(s string) string {
	for _, re := range tokenPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			lower := strings.ToLower(m)
			for _, p := range []string{"bearer", "token", "basic"} {
				if strings.HasPrefix(lower, p) {
					rest := strings.TrimLeft(m[len(p):], " \t")
					if !looksLikeSecret(rest) {
						return m // ordinary prose such as "token authentication"
					}
					return m[:len(m)-len(rest)] + Placeholder
				}
			}
			return Placeholder
		})
	}
	return s
}

// looksLikeSecret distinguishes credential-like values from English words.
func looksLikeSecret(v string) bool {
	if len(v) >= 24 {
		return true
	}
	hasDigit, hasLetter := false, false
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			hasLetter = true
		}
	}
	return hasDigit && hasLetter
}

// Value masks a whole value, keeping only a hint of its length class.
func Value(s string) string {
	if s == "" {
		return ""
	}
	return Placeholder
}
