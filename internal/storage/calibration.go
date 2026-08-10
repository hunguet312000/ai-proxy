package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ModelCalibration is what one model's served traffic has taught about its
// tokenizer: how its real token counts relate to LiteRouter's byte and estimate
// heuristics, and how much evidence backs that measurement. Persisting it is what
// turns the calibration from a per-process warm-up into something that improves
// across restarts — every deployment starts from what previous runs measured
// instead of from the conventional guesses.
//
// Bounded by construction: one row per model ever served, a handful of scalars each.
type ModelCalibration struct {
	Model string `json:"model"`
	// BytesPerToken converts a raw request body length to tokens.
	BytesPerToken float64 `json:"bytes_per_token"`
	// EstimatePerToken converts a token budget into estimate units.
	EstimatePerToken float64 `json:"estimate_per_token"`
	// Spread is the smoothed relative deviation of recent samples — how much the
	// measurements disagree with each other. Low spread plus enough samples is
	// what lets the gateway trust the measurement over its conservative clamp.
	Spread  float64 `json:"spread"`
	Samples int     `json:"samples"`
	// UpdatedAt is when the calibration last moved materially, not when the last
	// sample arrived — converged models stop writing.
	UpdatedAt time.Time `json:"updated_at"`
}

// UpsertModelCalibration records one model's current calibration.
func (s *Store) UpsertModelCalibration(ctx context.Context, cal ModelCalibration) error {
	cal.Model = strings.TrimSpace(cal.Model)
	if cal.Model == "" {
		return fmt.Errorf("calibration model is required")
	}
	if cal.BytesPerToken <= 0 || cal.EstimatePerToken <= 0 {
		return fmt.Errorf("calibration ratios must be positive")
	}
	if cal.Samples < 0 || cal.Spread < 0 {
		return fmt.Errorf("calibration samples and spread cannot be negative")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO model_calibration (model, bytes_per_token, estimate_per_token, spread, samples, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(model) DO UPDATE SET
  bytes_per_token = excluded.bytes_per_token,
  estimate_per_token = excluded.estimate_per_token,
  spread = excluded.spread,
  samples = excluded.samples,
  updated_at = excluded.updated_at`,
		cal.Model, cal.BytesPerToken, cal.EstimatePerToken, cal.Spread, cal.Samples,
		time.Now().UTC().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("upsert model calibration: %w", err)
	}
	return nil
}

// ListModelCalibrations returns every stored calibration, for seeding a gateway
// at boot.
func (s *Store) ListModelCalibrations(ctx context.Context) ([]ModelCalibration, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model, bytes_per_token, estimate_per_token, spread, samples, updated_at FROM model_calibration`)
	if err != nil {
		return nil, fmt.Errorf("list model calibrations: %w", err)
	}
	defer rows.Close()
	var result []ModelCalibration
	for rows.Next() {
		var cal ModelCalibration
		var updated int64
		if err := rows.Scan(&cal.Model, &cal.BytesPerToken, &cal.EstimatePerToken, &cal.Spread, &cal.Samples, &updated); err != nil {
			return nil, fmt.Errorf("scan model calibration: %w", err)
		}
		cal.UpdatedAt = time.UnixMilli(updated).UTC()
		result = append(result, cal)
	}
	return result, rows.Err()
}
