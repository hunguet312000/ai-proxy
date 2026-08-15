package storage

import (
	"context"
	"fmt"
)

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("setting key is required")
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if key == "" {
		return fmt.Errorf("setting key is required")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO settings (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return nil
}

// DeleteSetting removes a setting row. Deleting a missing key is not an error.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("setting key is required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("delete setting %q: %w", key, err)
	}
	return nil
}

// SettingsByPrefix returns every setting whose key starts with prefix. Used to
// restore per-model overrides at boot.
func (s *Store) SettingsByPrefix(ctx context.Context, prefix string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key LIKE ?`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}
