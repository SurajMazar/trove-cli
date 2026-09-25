package github

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/domain"
	"github.com/SurajMazar/trove-cli/internal/errs"
)

// profileSettings are the account profile fields PATCH /user accepts.
var profileSettings = []struct {
	key, desc string
}{
	{"name", "Display name"},
	{"email", "Publicly visible email address (must be a verified email of the account)"},
	{"blog", "Website URL"},
	{"company", "Company"},
	{"location", "Location"},
	{"bio", "Short biography"},
	{"twitter_username", "X (Twitter) username"},
	{"hireable", "Available for hire (true/false)"},
}

func settingKeys() []string {
	out := make([]string, len(profileSettings))
	for i, s := range profileSettings {
		out[i] = s.key
	}
	return out
}

func settingDesc(key string) (string, bool) {
	for _, s := range profileSettings {
		if s.key == key {
			return s.desc, true
		}
	}
	return "", false
}

func (u apiUser) settingValue(key string) string {
	switch key {
	case "name":
		return str(u.Name)
	case "email":
		return str(u.Email)
	case "blog":
		return str(u.Blog)
	case "company":
		return str(u.Company)
	case "location":
		return str(u.Location)
	case "bio":
		return str(u.Bio)
	case "twitter_username":
		return str(u.TwitterUsername)
	case "hireable":
		if u.Hireable == nil {
			return ""
		}
		return strconv.FormatBool(*u.Hireable)
	}
	return ""
}

func (p *Provider) checkSettingKey(op, key string) (string, string, error) {
	k := strings.ToLower(strings.TrimSpace(key))
	desc, ok := settingDesc(k)
	if !ok {
		return "", "", p.errorf(errs.ErrInvalidArgument, op, 0, "unknown GitHub setting %q (valid: %s)", key, strings.Join(settingKeys(), ", "))
	}
	return k, desc, nil
}

func (p *Provider) profile(ctx context.Context, op string) (*apiUser, error) {
	var u apiUser
	if _, err := p.do(ctx, request{op: op, method: http.MethodGet, path: "/user", out: &u}); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListSettings implements forge.SettingsProvider (account profile fields).
func (p *Provider) ListSettings(ctx context.Context) ([]domain.Setting, error) {
	u, err := p.profile(ctx, "list settings")
	if err != nil {
		return nil, err
	}
	out := make([]domain.Setting, 0, len(profileSettings))
	for _, s := range profileSettings {
		out = append(out, domain.Setting{Key: s.key, Value: u.settingValue(s.key), Description: s.desc, Writable: true})
	}
	return out, nil
}

// GetSetting implements forge.SettingsProvider.
func (p *Provider) GetSetting(ctx context.Context, key string) (*domain.Setting, error) {
	const op = "get setting"
	k, desc, err := p.checkSettingKey(op, key)
	if err != nil {
		return nil, err
	}
	u, err := p.profile(ctx, op)
	if err != nil {
		return nil, err
	}
	return &domain.Setting{Key: k, Value: u.settingValue(k), Description: desc, Writable: true}, nil
}

// SetSetting implements forge.SettingsProvider via PATCH /user (needs the
// "user" scope for classic tokens).
func (p *Provider) SetSetting(ctx context.Context, key, value string) (*domain.Setting, error) {
	const op = "set setting"
	k, desc, err := p.checkSettingKey(op, key)
	if err != nil {
		return nil, err
	}
	var v any = value
	if k == "hireable" {
		b, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return nil, p.errorf(errs.ErrInvalidArgument, op, 0, "hireable must be true or false, got %q", value)
		}
		v = b
	}
	var u apiUser
	if _, err := p.do(ctx, request{op: op, method: http.MethodPatch, path: "/user", body: map[string]any{k: v}, out: &u}); err != nil {
		return nil, err
	}
	return &domain.Setting{Key: k, Value: u.settingValue(k), Description: desc, Writable: true}, nil
}
