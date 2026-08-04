package storage

import (
	"context"
	"fmt"
	"time"
)

type QuotaSnapshot struct {
	AccountID        string    `json:"account_id"`
	Key              string    `json:"key"`
	Used             float64   `json:"used"`
	Total            float64   `json:"total"`
	Remaining        float64   `json:"remaining"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
	Unlimited        bool      `json:"unlimited"`
	Exhausted        bool      `json:"exhausted"`
	Source           string    `json:"source"`
	FetchedAt        time.Time `json:"fetched_at"`
}

func (s *Store) ReplaceQuotaSnapshots(ctx context.Context, accountID string, snapshots []QuotaSnapshot) error {
	if accountID == "" {
		return fmt.Errorf("quota account ID is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin quota transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM quota_snapshots WHERE account_id = ?", accountID); err != nil {
		return fmt.Errorf("delete old quota snapshots: %w", err)
	}
	for _, snapshot := range snapshots {
		if snapshot.Key == "" || snapshot.Source == "" {
			return fmt.Errorf("quota key and source are required")
		}
		fetchedAt := snapshot.FetchedAt
		if fetchedAt.IsZero() {
			fetchedAt = time.Now().UTC()
		}
		var resetAt any
		if !snapshot.ResetAt.IsZero() {
			resetAt = snapshot.ResetAt.UnixMilli()
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO quota_snapshots (
  account_id, quota_key, used, total, remaining, remaining_percent,
  reset_at, unlimited, exhausted, source, fetched_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			accountID, snapshot.Key, snapshot.Used, snapshot.Total, snapshot.Remaining,
			snapshot.RemainingPercent, resetAt, snapshot.Unlimited, snapshot.Exhausted,
			snapshot.Source, fetchedAt.UnixMilli(),
		); err != nil {
			return fmt.Errorf("insert quota snapshot %q: %w", snapshot.Key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit quota snapshots: %w", err)
	}
	return nil
}

func (s *Store) ListQuotaSnapshots(ctx context.Context, accountID string) ([]QuotaSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT account_id, quota_key, used, total, remaining, remaining_percent,
       reset_at, unlimited, exhausted, source, fetched_at
FROM quota_snapshots WHERE account_id = ? ORDER BY quota_key`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list quota snapshots: %w", err)
	}
	defer rows.Close()

	var snapshots []QuotaSnapshot
	for rows.Next() {
		var snapshot QuotaSnapshot
		var resetAt *int64
		var fetchedAt int64
		if err := rows.Scan(
			&snapshot.AccountID, &snapshot.Key, &snapshot.Used, &snapshot.Total,
			&snapshot.Remaining, &snapshot.RemainingPercent, &resetAt,
			&snapshot.Unlimited, &snapshot.Exhausted, &snapshot.Source, &fetchedAt,
		); err != nil {
			return nil, fmt.Errorf("scan quota snapshot: %w", err)
		}
		if resetAt != nil {
			snapshot.ResetAt = time.UnixMilli(*resetAt).UTC()
		}
		snapshot.FetchedAt = time.UnixMilli(fetchedAt).UTC()
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate quota snapshots: %w", err)
	}
	return snapshots, nil
}
