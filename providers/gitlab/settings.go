package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/domain"
)

// Settings exposes what a user may change about their own account through
// the API: preferences (GET/PUT /user/preferences) and status
// (GET/PUT /user/status). Profile fields are only writable by
// administrators via PUT /users/:id and are therefore not offered.

// preferenceSettings are the documented /user/preferences fields, all
// booleans and all writable.
var preferenceSettings = []struct{ key, description string }{
	{"view_diffs_file_by_file", "Show one file at a time on merge request diffs"},
	{"show_whitespace_in_diffs", "Show whitespace changes in diffs"},
	{"pass_user_identities_to_ci_jwt", "Pass external user identities to CI/CD job JWTs"},
}

var statusSettings = []struct{ key, field, description string }{
	{"status.message", "message", "Status message (up to 100 characters)"},
	{"status.emoji", "emoji", "Status emoji name, e.g. coffee"},
	{"status.availability", "availability", "Availability: busy or not_set"},
}

type glStatus struct {
	Message      string `json:"message"`
	Emoji        string `json:"emoji"`
	Availability string `json:"availability"`
}

func (s glStatus) field(name string) string {
	switch name {
	case "message":
		return s.Message
	case "emoji":
		return s.Emoji
	default:
		return s.Availability
	}
}

func (p *provider) preferences(ctx context.Context) (map[string]json.RawMessage, error) {
	var prefs map[string]json.RawMessage
	if err := p.get(ctx, "get user preferences", "/user/preferences", nil, &prefs); err != nil {
		return nil, err
	}
	return prefs, nil
}

func (p *provider) status(ctx context.Context) (glStatus, error) {
	var st glStatus
	err := p.get(ctx, "get user status", "/user/status", nil, &st)
	return st, err
}

func (p *provider) ListSettings(ctx context.Context) ([]domain.Setting, error) {
	prefs, err := p.preferences(ctx)
	if err != nil {
		return nil, err
	}
	st, err := p.status(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.Setting
	for _, s := range preferenceSettings {
		raw, ok := prefs[s.key]
		if !ok {
			continue // not offered by this GitLab version
		}
		out = append(out, domain.Setting{Key: s.key, Value: rawValue(raw), Description: s.description, Writable: true})
	}
	for _, s := range statusSettings {
		out = append(out, domain.Setting{Key: s.key, Value: st.field(s.field), Description: s.description, Writable: true})
	}
	return out, nil
}

func rawValue(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	default:
		return strings.TrimSpace(string(raw))
	}
}

func (p *provider) GetSetting(ctx context.Context, key string) (*domain.Setting, error) {
	all, err := p.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		if s.Key == key {
			return &s, nil
		}
	}
	return nil, p.notFound("get setting "+key, "unknown GitLab setting %q (available: %s)", key, strings.Join(settingKeys(), ", "))
}

func (p *provider) SetSetting(ctx context.Context, key, value string) (*domain.Setting, error) {
	op := "set " + key
	for _, s := range preferenceSettings {
		if s.key != key {
			continue
		}
		b, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return nil, p.invalid(op, "%s must be true or false", key)
		}
		prefs, err := p.preferences(ctx)
		if err != nil {
			return nil, err
		}
		if _, ok := prefs[key]; !ok {
			return nil, p.notFound(op, "this GitLab version does not offer the %q preference", key)
		}
		// Older GitLab versions require every preference on PUT, so the
		// current values are sent alongside the changed one.
		body := map[string]any{}
		for _, ps := range preferenceSettings {
			if raw, ok := prefs[ps.key]; ok {
				var cur bool
				if json.Unmarshal(raw, &cur) == nil {
					body[ps.key] = cur
				}
			}
		}
		body[key] = b
		var updated map[string]json.RawMessage
		if _, err := p.do(ctx, call{method: http.MethodPut, path: "/user/preferences", op: op, body: body}, &updated); err != nil {
			return nil, err
		}
		v := strconv.FormatBool(b)
		if raw, ok := updated[key]; ok {
			v = rawValue(raw)
		}
		return &domain.Setting{Key: key, Value: v, Description: s.description, Writable: true}, nil
	}
	for _, s := range statusSettings {
		if s.key != key {
			continue
		}
		if s.field == "availability" && value != "busy" && value != "not_set" {
			return nil, p.invalid(op, "availability must be busy or not_set")
		}
		if s.field == "message" && len([]rune(value)) > 100 {
			return nil, p.invalid(op, "the status message is limited to 100 characters")
		}
		cur, err := p.status(ctx)
		if err != nil {
			return nil, err
		}
		// PUT /user/status clears every field that is not sent, so the other
		// fields are sent with their current values. (An automatic
		// clear_status_at, if one was set, is reset by the update.)
		body := map[string]any{"message": cur.Message, "emoji": cur.Emoji, "availability": nonEmpty(cur.Availability, "not_set")}
		body[s.field] = value
		var updated glStatus
		if _, err := p.do(ctx, call{method: http.MethodPut, path: "/user/status", op: op, body: body}, &updated); err != nil {
			return nil, err
		}
		return &domain.Setting{Key: key, Value: updated.field(s.field), Description: s.description, Writable: true}, nil
	}
	return nil, p.notFound(op, "unknown GitLab setting %q (available: %s)", key, strings.Join(settingKeys(), ", "))
}

func settingKeys() []string {
	var keys []string
	for _, s := range preferenceSettings {
		keys = append(keys, s.key)
	}
	for _, s := range statusSettings {
		keys = append(keys, s.key)
	}
	return keys
}
