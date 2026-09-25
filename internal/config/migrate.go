package config

import "fmt"

// migration upgrades a raw document from version From to From+1.
type migration struct {
	From  int
	Apply func(raw map[string]any) error
}

// migrations is the ordered upgrade chain. To introduce schema version N+1,
// append a migration{From: N} and bump CurrentVersion.
var migrations = []migration{
	{
		// Version 0 (unversioned documents written by hand): add the version
		// field; the layout is otherwise identical to version 1.
		From:  0,
		Apply: func(raw map[string]any) error { return nil },
	},
}

// Migrate upgrades a raw configuration document to CurrentVersion.
func Migrate(raw map[string]any) (map[string]any, error) {
	v, err := versionOf(raw)
	if err != nil {
		return nil, err
	}
	if v > CurrentVersion {
		return nil, fmt.Errorf("config version %d is newer than this build of trove supports (%d); upgrade trove", v, CurrentVersion)
	}
	for _, m := range migrations {
		if m.From < v {
			continue
		}
		if m.From != v {
			return nil, fmt.Errorf("no migration path from config version %d", v)
		}
		if err := m.Apply(raw); err != nil {
			return nil, fmt.Errorf("migrate config v%d: %w", v, err)
		}
		v++
		raw["version"] = v
	}
	raw["version"] = CurrentVersion
	return raw, nil
}

func versionOf(raw map[string]any) (int, error) {
	switch v := raw["version"].(type) {
	case nil:
		return 0, nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("config version must be an integer, got %v", v)
	}
}
