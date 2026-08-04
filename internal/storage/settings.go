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
