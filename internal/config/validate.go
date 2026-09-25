package config

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Problem is a single validation finding.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (p Problem) String() string { return p.Path + ": " + p.Message }

// Rules supplies the knowledge validation needs from the rest of the app
// (registered drivers and secret providers) without importing them.
type Rules struct {
	// AuthMethods maps driver type -> supported auth types.
	AuthMethods map[string][]string
	// SecretProviders lists known secret provider schemes.
	SecretProviders []string
}

var aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidAlias reports whether a provider alias is acceptable.
func ValidAlias(s string) bool { return aliasRe.MatchString(s) }

// Validate checks the configuration and returns all problems found.
func (c *Config) Validate(r Rules) []Problem {
	var ps []Problem
	add := func(path, format string, args ...any) {
		ps = append(ps, Problem{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if c.Version != CurrentVersion {
		add("version", "unsupported version %d (want %d)", c.Version, CurrentVersion)
	}
	if c.DefaultProvider != "" {
		if _, ok := c.Providers[c.DefaultProvider]; !ok {
			add("default_provider", "%q is not a configured provider", c.DefaultProvider)
		}
	}
	secretSet := map[string]bool{}
	for _, s := range r.SecretProviders {
		secretSet[s] = true
	}
	names := c.ProviderNames()
	for _, name := range names {
		p := c.Providers[name]
		base := "providers." + name
		if !ValidAlias(name) {
			add(base, "alias must match %s", aliasRe.String())
		}
		methods, known := r.AuthMethods[p.Type]
		if p.Type == "" {
			add(base+".type", "required")
		} else if r.AuthMethods != nil && !known {
			add(base+".type", "unknown provider type %q (available: %s)", p.Type, strings.Join(sortedKeys(r.AuthMethods), ", "))
		}
		if p.Host == "" {
			add(base+".host", "required")
		} else if strings.Contains(p.Host, "/") || strings.Contains(p.Host, " ") {
			add(base+".host", "must be a hostname (optionally with :port), not a URL")
		}
		for field, v := range map[string]string{"api_base_url": p.APIBaseURL, "web_base_url": p.WebBaseURL, "clone_base_url": p.CloneBaseURL} {
			if v == "" {
				continue
			}
			u, err := url.Parse(v)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				add(base+"."+field, "must be an absolute http(s) URL")
			} else if u.User != nil {
				add(base+"."+field, "must not contain credentials")
			}
		}
		if strings.HasSuffix(p.SSHKey, ".pub") {
			add(base+".ssh_key", "must be the private key (drop the .pub suffix)")
		}
		if p.Protocol != "" && p.Protocol != "https" && p.Protocol != "ssh" {
			add(base+".protocol", "must be https or ssh")
		}
		if p.Auth.Type != "" && known && !contains(methods, p.Auth.Type) {
			add(base+".auth.type", "%q is not supported by %s (supported: %s)", p.Auth.Type, p.Type, strings.Join(methods, ", "))
		}
		if p.Auth.SecretRef != "" {
			scheme, key, ok := strings.Cut(p.Auth.SecretRef, "://")
			switch {
			case !ok || scheme == "" || key == "":
				add(base+".auth.secret_ref", "must look like provider://key")
			case len(secretSet) > 0 && !secretSet[strings.ToLower(scheme)]:
				add(base+".auth.secret_ref", "unknown secret provider %q (available: %s)", scheme, strings.Join(r.SecretProviders, ", "))
			}
		}
	}
	for _, sp := range c.secretProblems() {
		add("secrets", "%s", sp)
	}
	if c.Secrets.Provider != "" && len(secretSet) > 0 && !secretSet[c.Secrets.Provider] {
		add("secrets.provider", "unknown secret provider %q (available: %s)", c.Secrets.Provider, strings.Join(r.SecretProviders, ", "))
	}
	if b := c.Secrets.Bitwarden.Backend; b != "" && b != "cli" && b != "secrets-manager" {
		add("secrets.bitwarden.backend", "must be cli or secrets-manager")
	}
	if c.Clone.Concurrency < 1 || c.Clone.Concurrency > MaxConcurrency {
		add("clone.concurrency", "must be between 1 and %d", MaxConcurrency)
	}
	if c.Clone.Protocol != "https" && c.Clone.Protocol != "ssh" {
		add("clone.protocol", "must be https or ssh")
	}
	switch c.Clone.Layout {
	case "flat", "namespace", "host":
	default:
		add("clone.layout", "must be flat, namespace or host")
	}
	if c.Clone.Retries < 0 || c.Clone.Retries > 5 {
		add("clone.retries", "must be between 0 and 5")
	}
	if c.Cache.TTL != "" {
		if d, err := time.ParseDuration(c.Cache.TTL); err != nil || d < 0 {
			add("cache.ttl", "must be a duration such as 2m or 30s")
		}
	}
	switch c.Output.Format {
	case "", "table", "json":
	default:
		add("output.format", "must be table or json")
	}
	switch c.Output.Color {
	case "", "auto", "always", "never":
	default:
		add("output.color", "must be auto, always or never")
	}
	return ps
}

// secretKeyNames are field names that must never appear in the config.
var secretKeyNames = []string{"token", "password", "secret", "private_key", "client_secret", "api_key", "apikey", "passphrase", "access_token", "refresh_token"}

// IsSecretKey reports whether a config key looks like it would hold a secret.
func IsSecretKey(key string) bool {
	k := strings.ToLower(key)
	if k == "secret_ref" || strings.HasSuffix(k, ".secret_ref") {
		return false
	}
	last := k
	if i := strings.LastIndex(k, "."); i >= 0 {
		last = k[i+1:]
	}
	for _, s := range secretKeyNames {
		if last == s || strings.HasSuffix(last, "_"+s) {
			return true
		}
	}
	return false
}

// secretProblems finds secret-looking keys in provider Extra blocks.
func (c *Config) secretProblems() []string {
	var out []string
	for _, name := range c.ProviderNames() {
		walkKeys(c.Providers[name].Extra, "providers."+name, func(path string) {
			if IsSecretKey(path) {
				out = append(out, fmt.Sprintf("%s looks like a secret; store it in a secret provider and use auth.secret_ref instead", path))
			}
		})
	}
	return out
}

func walkKeys(m map[string]any, prefix string, fn func(string)) {
	for k, v := range m {
		p := prefix + "." + k
		fn(p)
		if sub, ok := v.(map[string]any); ok {
			walkKeys(sub, p, fn)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
