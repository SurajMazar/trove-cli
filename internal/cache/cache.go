// Package cache is a small on-disk TTL cache for non-sensitive API results
// (repository lists, namespaces, user profiles). It must never be used for
// credentials or secret values.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cache stores JSON-encoded values in files under Dir.
type Cache struct {
	Dir     string
	TTL     time.Duration
	Enabled bool
	now     func() time.Time
}

// DefaultDir returns the cache directory ($XDG_CACHE_HOME/trove or the OS
// cache dir).
func DefaultDir() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "trove")
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "trove")
	}
	return filepath.Join(os.TempDir(), "trove-cache")
}

// New returns a cache.
func New(dir string, ttl time.Duration, enabled bool) *Cache {
	return &Cache{Dir: dir, TTL: ttl, Enabled: enabled && ttl > 0, now: time.Now}
}

type entry struct {
	Stored time.Time       `json:"stored"`
	Value  json.RawMessage `json:"value"`
}

// Key builds a cache key from parts (provider alias, operation, params).
func Key(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:16])
}

func (c *Cache) path(key string) string { return filepath.Join(c.Dir, key+".json") }

// Load reads a fresh value into v, reporting whether it was found.
func (c *Cache) Load(key string, v any) bool {
	if c == nil || !c.Enabled {
		return false
	}
	b, err := os.ReadFile(c.path(key))
	if err != nil {
		return false
	}
	var e entry
	if json.Unmarshal(b, &e) != nil || c.now().Sub(e.Stored) > c.TTL {
		return false
	}
	return json.Unmarshal(e.Value, v) == nil
}

// Store writes v under key. Errors are ignored: the cache is best-effort.
func (c *Cache) Store(key string, v any) {
	if c == nil || !c.Enabled {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	b, err := json.Marshal(entry{Stored: c.now(), Value: raw})
	if err != nil {
		return
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(c.Dir, ".tmp-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return
	}
	tmp.Close()
	_ = os.Rename(tmp.Name(), c.path(key))
}

// Invalidate removes every cached entry (e.g. after a mutation).
func (c *Cache) Invalidate() error {
	if c == nil || c.Dir == "" {
		return nil
	}
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			_ = os.Remove(filepath.Join(c.Dir, e.Name()))
		}
	}
	return nil
}

// Fetch returns a cached value or calls fetch and caches its result.
func Fetch[T any](ctx context.Context, c *Cache, key string, fetch func(context.Context) (T, error)) (T, error) {
	var v T
	if c.Load(key, &v) {
		return v, nil
	}
	v, err := fetch(ctx)
	if err != nil {
		return v, err
	}
	c.Store(key, v)
	return v, nil
}
