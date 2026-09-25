package config

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/SurajMazar/trove-cli/internal/errs"
)

// Get returns the value at a dotted key path such as "clone.concurrency" or
// "providers.github-personal.host". Maps and lists are rendered as YAML.
func (c *Config) Get(key string) (string, error) {
	node, err := c.toMap()
	if err != nil {
		return "", err
	}
	var cur any = node
	for _, part := range splitKey(key) {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", errs.New(errs.ErrNotFound, "config key %q not found", key)
		}
		cur, ok = m[part]
		if !ok {
			return "", errs.New(errs.ErrNotFound, "config key %q not found", key)
		}
	}
	switch v := cur.(type) {
	case string:
		return v, nil
	case nil:
		return "", nil
	case map[string]any, []any:
		b, err := yaml.Marshal(v)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\n"), nil
	default:
		return fmt.Sprint(v), nil
	}
}

// Set assigns value at a dotted key path. Values are parsed as YAML scalars
// (so "true", "4" and "[a, b]" become bool/int/list). Keys that look like
// secrets are rejected. The resulting configuration is re-parsed so that
// type errors surface immediately; Validate should be called by the caller.
func (c *Config) Set(key, value string) error {
	if IsSecretKey(key) {
		return &errs.Error{Kind: errs.ErrInvalidArgument,
			Message: fmt.Sprintf("refusing to store %q in the configuration file: it looks like a secret", key),
			Hint:    "use `trove auth login` to store credentials in a secret provider"}
	}
	parts := splitKey(key)
	if len(parts) == 0 {
		return errs.New(errs.ErrInvalidArgument, "empty config key")
	}
	root, err := c.toMap()
	if err != nil {
		return err
	}
	var parsed any
	if err := yaml.Unmarshal([]byte(value), &parsed); err != nil || parsed == nil {
		parsed = value
	}
	// Keep strings that merely look numeric for known string fields.
	if s, ok := parsed.(int); ok && isStringKey(parts) {
		parsed = strconv.Itoa(s)
	}
	cur := root
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			if _, exists := cur[part]; exists {
				return errs.New(errs.ErrInvalidArgument, "config key %q is not a map", part)
			}
			next = map[string]any{}
			cur[part] = next
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = parsed
	return c.replace(root)
}

// Unset removes a key.
func (c *Config) Unset(key string) error {
	parts := splitKey(key)
	root, err := c.toMap()
	if err != nil {
		return err
	}
	cur := root
	for _, part := range parts[:len(parts)-1] {
		next, ok := cur[part].(map[string]any)
		if !ok {
			return errs.New(errs.ErrNotFound, "config key %q not found", key)
		}
		cur = next
	}
	if _, ok := cur[parts[len(parts)-1]]; !ok {
		return errs.New(errs.ErrNotFound, "config key %q not found", key)
	}
	delete(cur, parts[len(parts)-1])
	return c.replace(root)
}

func isStringKey(parts []string) bool {
	last := parts[len(parts)-1]
	switch last {
	case "app_id", "installation_id", "client_id", "project_id", "organization_id", "collection_id", "username", "host", "name":
		return true
	}
	return false
}

func (c *Config) toMap() (map[string]any, error) {
	b, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *Config) replace(root map[string]any) error {
	b, err := yaml.Marshal(root)
	if err != nil {
		return err
	}
	nc, err := Parse(b)
	if err != nil {
		return errs.Wrap(errs.ErrInvalidArgument, err, "invalid value")
	}
	path := c.path
	*c = *nc
	c.path = path
	return nil
}

func splitKey(key string) []string {
	var out []string
	for _, p := range strings.Split(strings.TrimSpace(key), ".") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
