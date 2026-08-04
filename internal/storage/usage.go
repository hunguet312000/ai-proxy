package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type UsageEvent struct {
	ID                        int64
	Timestamp                 time.Time
	Provider                  string
	Model                     string
	Endpoint                  string
	Status                    string
	PromptTokens              int
	CompletionTokens          int
	CachedTokens              int
	TotalTokens               int
	CostUSD                   float64
	PromptTokensEstimated     bool
	CompletionTokensEstimated bool
	CachedTokensReported      bool
}

type UsageSummary struct {
	Requests         int
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	CostUSD          float64
	ByProvider       []UsageProviderStat
	ByModel          []UsageModelStat
	Recent           []UsageEvent
}

type UsageProviderStat struct {
	Provider string
	Requests int
	Tokens   int64
	CostUSD  float64
}

type UsageModelStat struct {
	Model     string
	Requests  int
	InTokens  int64
	OutTokens int64
	CostUSD   float64
}

func (s *Store) InsertUsageEvent(ctx context.Context, event UsageEvent) error {
	return s.InsertUsageEvents(ctx, []UsageEvent{event})
}

func (s *Store) InsertUsageEvents(ctx context.Context, events []UsageEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage batch: %w", err)
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `
	INSERT INTO usage_events (
	  ts, provider, model, endpoint, status,
	  prompt_tokens, completion_tokens, cached_tokens, total_tokens, cost_usd,
	  prompt_estimated, completion_estimated, cached_reported
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare usage batch: %w", err)
	}
	defer statement.Close()
	for _, event := range events {
		if event.Timestamp.IsZero() {
			event.Timestamp = time.Now().UTC()
		}
		if event.Status == "" {
			event.Status = "ok"
		}
		if event.TotalTokens == 0 {
			event.TotalTokens = event.PromptTokens + event.CompletionTokens
		}
		if _, err := statement.ExecContext(ctx,
			event.Timestamp.UnixMilli(),
			strings.TrimSpace(event.Provider),
			strings.TrimSpace(event.Model),
			strings.TrimSpace(event.Endpoint),
			strings.TrimSpace(event.Status),
			event.PromptTokens,
			event.CompletionTokens,
			event.CachedTokens,
			event.TotalTokens,
			event.CostUSD,
			event.PromptTokensEstimated,
			event.CompletionTokensEstimated,
			event.CachedTokensReported,
		); err != nil {
			return fmt.Errorf("insert usage event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit usage batch: %w", err)
	}
	return nil
}

func (s *Store) DeleteUsageEventsBefore(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = 1000
	}
	var deleted int64
	for {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		result, err := s.db.ExecContext(ctx, `
DELETE FROM usage_events
WHERE id IN (
  SELECT id FROM usage_events
  WHERE ts < ?
  ORDER BY ts ASC
  LIMIT ?
)`, cutoff.UTC().UnixMilli(), batchSize)
		if err != nil {
			return deleted, fmt.Errorf("delete expired usage events: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("count deleted usage events: %w", err)
		}
		deleted += count
		if count < int64(batchSize) {
			break
		}
	}
	if deleted > 0 {
		if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
			return deleted, fmt.Errorf("checkpoint usage retention WAL: %w", err)
		}
	}
	return deleted, nil
}

func (s *Store) UsageSummary(ctx context.Context, since time.Time, recentLimit int) (UsageSummary, error) {
	if recentLimit <= 0 {
		recentLimit = 30
	}
	var out UsageSummary
	sinceMs := since.UTC().UnixMilli()

	row := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(prompt_tokens),0),
       COALESCE(SUM(completion_tokens),0),
       COALESCE(SUM(cached_tokens),0),
       COALESCE(SUM(cost_usd),0)
FROM usage_events WHERE ts >= ?`, sinceMs)
	if err := row.Scan(&out.Requests, &out.PromptTokens, &out.CompletionTokens, &out.CachedTokens, &out.CostUSD); err != nil {
		return out, fmt.Errorf("usage summary totals: %w", err)
	}

	prows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(provider,''), COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0)
FROM usage_events WHERE ts >= ?
GROUP BY provider
ORDER BY COUNT(*) DESC, provider ASC`, sinceMs)
	if err != nil {
		return out, fmt.Errorf("usage by provider: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var st UsageProviderStat
		if err := prows.Scan(&st.Provider, &st.Requests, &st.Tokens, &st.CostUSD); err != nil {
			return out, err
		}
		if st.Provider == "" {
			st.Provider = "unknown"
		}
		out.ByProvider = append(out.ByProvider, st)
	}

	mrows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(model,''), COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cost_usd),0)
FROM usage_events WHERE ts >= ?
GROUP BY model
ORDER BY COUNT(*) DESC, model ASC
LIMIT 20`, sinceMs)
	if err != nil {
		return out, fmt.Errorf("usage by model: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var st UsageModelStat
		if err := mrows.Scan(&st.Model, &st.Requests, &st.InTokens, &st.OutTokens, &st.CostUSD); err != nil {
			return out, err
		}
		if st.Model == "" {
			st.Model = "unknown"
		}
		out.ByModel = append(out.ByModel, st)
	}

	rrows, err := s.db.QueryContext(ctx, `
SELECT id, ts, provider, model, endpoint, status, prompt_tokens, completion_tokens, cached_tokens, total_tokens, cost_usd,
       prompt_estimated, completion_estimated, cached_reported
FROM usage_events
WHERE ts >= ?
ORDER BY ts DESC
LIMIT ?`, sinceMs, recentLimit)
	if err != nil {
		return out, fmt.Errorf("usage recent: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var e UsageEvent
		var ts int64
		if err := rrows.Scan(&e.ID, &ts, &e.Provider, &e.Model, &e.Endpoint, &e.Status,
			&e.PromptTokens, &e.CompletionTokens, &e.CachedTokens, &e.TotalTokens, &e.CostUSD,
			&e.PromptTokensEstimated, &e.CompletionTokensEstimated, &e.CachedTokensReported); err != nil {
			return out, err
		}
		e.Timestamp = time.UnixMilli(ts).UTC()
		out.Recent = append(out.Recent, e)
	}
	return out, nil
}
