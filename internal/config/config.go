// Package config loads, validates, migrates and saves Trove's YAML
// configuration. The configuration never contains secret values: credentials
// are referenced through secret_ref URIs resolved by a secret provider.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/SurajMazar/trove-cli/internal/errs"
)

// CurrentVersion is the configuration schema version this build writes.
const CurrentVersion = 1

// Config is the root configuration document.
type Config struct {
	Version         int                        `yaml:"version"`
	DefaultProvider string                     `yaml:"default_provider,omitempty"`
	Providers       map[string]*ProviderConfig `yaml:"providers"`
	Secrets         SecretsConfig              `yaml:"secrets"`
	Clone           CloneConfig                `yaml:"clone"`
	Cache           CacheConfig                `yaml:"cache"`
	Output          OutputConfig               `yaml:"output,omitempty"`

	path string
}

// ProviderConfig configures one forge account. The map key in Config.Providers
// is the user's local alias.
type ProviderConfig struct {
	Type         string     `yaml:"type"`
	Host         string     `yaml:"host,omitempty"`
	Name         string     `yaml:"name,omitempty"` // display label, e.g. "GitHub Personal"
	APIBaseURL   string     `yaml:"api_base_url,omitempty"`
	WebBaseURL   string     `yaml:"web_base_url,omitempty"`
	CloneBaseURL string     `yaml:"clone_base_url,omitempty"`
	SSHHost      string     `yaml:"ssh_host,omitempty"`
	Protocol     string     `yaml:"protocol,omitempty"` // default clone protocol for this account
	Auth         AuthConfig `yaml:"auth,omitempty"`
	// Extra holds provider-specific settings (e.g. Bitbucket `workspace`,
	// custom provider `custom:` block). Drivers decode them themselves.
	Extra map[string]any `yaml:",inline"`
}

// AuthConfig describes how to authenticate. It never holds secret values.
type AuthConfig struct {
	Type           string   `yaml:"type,omitempty"`
	SecretRef      string   `yaml:"secret_ref,omitempty"`
	Username       string   `yaml:"username,omitempty"`
	ClientID       string   `yaml:"client_id,omitempty"`
	AppID          string   `yaml:"app_id,omitempty"`
	InstallationID string   `yaml:"installation_id,omitempty"`
	Scopes         []string `yaml:"scopes,omitempty"`
}

// SecretsConfig selects and configures secret providers.
type SecretsConfig struct {
	// Provider is the default secret provider for newly stored credentials.
	Provider      string              `yaml:"provider,omitempty"`
	Bitwarden     BitwardenConfig     `yaml:"bitwarden,omitempty"`
	Keychain      KeychainConfig      `yaml:"keychain,omitempty"`
	SecretService SecretServiceConfig `yaml:"secretservice,omitempty"`
}

// BitwardenConfig configures the Bitwarden secret provider.
type BitwardenConfig struct {
	Backend        string `yaml:"backend,omitempty"` // cli | secrets-manager
	CLIPath        string `yaml:"cli_path,omitempty"`
	Folder         string `yaml:"folder,omitempty"`
	NoFolder       bool   `yaml:"no_folder,omitempty"` // store items outside any folder
	OrganizationID string `yaml:"organization_id,omitempty"`
	CollectionID   string `yaml:"collection_id,omitempty"`
	ProjectID      string `yaml:"project_id,omitempty"`
	SyncOnStart    bool   `yaml:"sync_on_start,omitempty"`
}

// KeychainConfig configures the macOS Keychain provider.
type KeychainConfig struct {
	Service string `yaml:"service,omitempty"`
}

// SecretServiceConfig configures the Linux Secret Service provider.
type SecretServiceConfig struct {
	Service string `yaml:"service,omitempty"`
}

// CloneConfig configures cloning.
type CloneConfig struct {
	Concurrency int    `yaml:"concurrency,omitempty"`
	Protocol    string `yaml:"protocol,omitempty"`  // https | ssh
	Directory   string `yaml:"directory,omitempty"` // base directory; default: current directory
	// Layout: "flat" (<dir>/<name>), "namespace" (<dir>/<namespace>/<name>)
	// or "host" (<dir>/<host>/<namespace>/<name>).
	Layout  string `yaml:"layout,omitempty"`
	Retries int    `yaml:"retries,omitempty"`
}

// CacheConfig configures the lightweight response cache.
type CacheConfig struct {
	Enabled *bool  `yaml:"enabled,omitempty"`
	TTL     string `yaml:"ttl,omitempty"`
}

// OutputConfig configures default output.
type OutputConfig struct {
	Format string `yaml:"format,omitempty"` // table | json
	Color  string `yaml:"color,omitempty"`  // auto | always | never
}

// Defaults.
const (
	DefaultConcurrency = 4
	DefaultCacheTTL    = 2 * time.Minute
	MaxConcurrency     = 32
)

// New returns an empty configuration with defaults applied.
func New() *Config {
	c := &Config{Version: CurrentVersion, Providers: map[string]*ProviderConfig{}}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.Providers == nil {
		c.Providers = map[string]*ProviderConfig{}
	}
	if c.Clone.Concurrency == 0 {
		c.Clone.Concurrency = DefaultConcurrency
	}
	if c.Clone.Layout == "" {
		c.Clone.Layout = "namespace"
	}
	if c.Clone.Protocol == "" {
		c.Clone.Protocol = "https"
	}
	if c.Secrets.Provider == "" {
		c.Secrets.Provider = DefaultSecretProvider()
	}
}

// DefaultSecretProvider picks the platform's native secret store.
func DefaultSecretProvider() string {
	switch runtime.GOOS {
	case "darwin":
		return "keychain"
	case "linux":
		return "secretservice"
	default:
		return "bitwarden"
	}
}

// Path returns the file this configuration was loaded from.
func (c *Config) Path() string { return c.path }

// SetPath sets the file path used by Save.
func (c *Config) SetPath(p string) { c.path = p }

// CacheEnabled reports whether caching is on (default true).
func (c *Config) CacheEnabled() bool { return c.Cache.Enabled == nil || *c.Cache.Enabled }

// CacheTTL returns the configured cache TTL.
func (c *Config) CacheTTL() time.Duration {
	if c.Cache.TTL == "" {
		return DefaultCacheTTL
	}
	d, err := time.ParseDuration(c.Cache.TTL)
	if err != nil || d < 0 {
		return DefaultCacheTTL
	}
	return d
}

// ProviderNames returns provider aliases sorted.
func (c *Config) ProviderNames() []string {
	out := make([]string, 0, len(c.Providers))
	for k := range c.Providers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Provider returns a provider config by alias.
func (c *Config) Provider(name string) (*ProviderConfig, error) {
	p, ok := c.Providers[name]
	if !ok || p == nil {
		hint := "trove provider list"
		if len(c.Providers) == 0 {
			hint = "trove provider add"
		}
		return nil, &errs.Error{Kind: errs.ErrProviderNotFound, Message: fmt.Sprintf("provider %q is not configured", name), Hint: hint}
	}
	return p, nil
}

// DecodeInto decodes the provider block (including Extra keys) into v; used
// by drivers to read provider-specific settings.
func (p *ProviderConfig) DecodeInto(v any) error {
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(b, v)
}

// Label returns a human label for the account.
func (p *ProviderConfig) Label(alias string) string {
	if p.Name != "" {
		return p.Name
	}
	return alias
}

// DefaultPath returns the default configuration file path:
// $TROVE_CONFIG, else $XDG_CONFIG_HOME/trove/config.yaml, else
// ~/.config/trove/config.yaml (macOS and Linux), else the OS config dir.
func DefaultPath() (string, error) {
	if p := os.Getenv("TROVE_CONFIG"); p != "" {
		return filepath.Abs(p)
	}
	return filepath.Join(Dir(), "config.yaml"), nil
}

// Dir returns Trove's configuration directory.
func Dir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "trove")
	}
	if runtime.GOOS != "windows" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".config", "trove")
		}
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "trove")
	}
	return ".trove"
}

// Load reads the configuration at path. A missing file yields an empty
// configuration (not an error) so first-run commands work.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c := New()
			c.path = path
			return c, nil
		}
		return nil, errs.Wrap(errs.ErrInvalidConfiguration, err, "read config %s", path)
	}
	c, err := Parse(b)
	if err != nil {
		return nil, errs.Wrap(errs.ErrInvalidConfiguration, err, "parse config %s", path)
	}
	c.path = path
	return c, nil
}

// Parse decodes configuration bytes, applying migrations and defaults.
func Parse(b []byte) (*Config, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return New(), nil
	}
	migrated, err := Migrate(raw)
	if err != nil {
		return nil, err
	}
	mb, err := yaml.Marshal(migrated)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(mb))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	for name, p := range c.Providers {
		if p == nil {
			return nil, fmt.Errorf("provider %q has an empty configuration", name)
		}
	}
	c.applyDefaults()
	return &c, nil
}

// Marshal renders the configuration as YAML.
func (c *Config) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Save writes the configuration atomically with 0600 permissions after
// verifying it contains no secret-looking values.
func (c *Config) Save() error {
	if c.path == "" {
		return errs.New(errs.ErrInvalidConfiguration, "config path not set")
	}
	if problems := c.secretProblems(); len(problems) > 0 {
		return errs.New(errs.ErrInvalidConfiguration, "refusing to save configuration: %s", strings.Join(problems, "; "))
	}
	c.Version = CurrentVersion
	b, err := c.Marshal()
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	header := "# Trove configuration. Secrets are never stored here; see secret_ref.\n"
	if _, err := tmp.WriteString(header); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}
