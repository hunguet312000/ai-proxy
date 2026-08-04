package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
)

type CatalogModel struct {
	Provider      string `json:"provider"`
	ID            string `json:"id"`
	Label         string `json:"label,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	// MaxOutputTokens is 0 until an upstream rejection reveals the model's cap.
	MaxOutputTokens int       `json:"max_output_tokens,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

func normalizeCatalogProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "grok":
		return "xai"
	default:
		return provider
	}
}

func inferCatalogProvider(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	switch {
	case strings.HasPrefix(id, "claude"), strings.HasPrefix(id, "anthropic"):
		return "claude"
	case strings.HasPrefix(id, "grok"), strings.HasPrefix(id, "xai/") || id == "xai":
		return "xai"
	case strings.HasPrefix(id, "gpt"), strings.HasPrefix(id, "o1"), strings.HasPrefix(id, "o3"), strings.HasPrefix(id, "o4"), strings.HasPrefix(id, "codex"), strings.HasPrefix(id, "cx/"):
		return "codex"
	default:
		return "codex"
	}
}

// PrettyModelLabel turns ids like "cx/gpt-5.6-sol-review" into "GPT 5.6 Sol Review".
func PrettyModelLabel(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	name := id
	if i := strings.IndexByte(name, '/'); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	name = strings.ReplaceAll(name, "_", "-")
	parts := strings.Split(name, "-")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		switch lower {
		case "gpt":
			out = append(out, "GPT")
		case "claude":
			out = append(out, "Claude")
		case "opus", "sonnet", "haiku", "codex", "spark", "luna", "sol", "terra", "mini", "review", "fast", "reasoning", "code", "grok":
			out = append(out, strings.ToUpper(lower[:1])+lower[1:])
		default:
			// keep version-like tokens (5.6, 4.1, 4-8 already split)
			if isVersionToken(part) {
				out = append(out, part)
			} else {
				out = append(out, titleWord(part))
			}
		}
	}
	if len(out) == 0 {
		return id
	}
	return strings.Join(out, " ")
}

func isVersionToken(s string) bool {
	hasDigit := false
	for _, r := range s {
		if unicode.IsDigit(r) {
			hasDigit = true
			continue
		}
		if r == '.' {
			continue
		}
		return false
	}
	return hasDigit
}

func titleWord(s string) string {
	s = strings.ToLower(s)
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// DefaultContextWindow returns a researched/default context window for known models.
// Values are input+output total context where vendors publish one number.
// Unknown models return 0 so callers can apply hybrid fallback.
func DefaultContextWindow(id string) int {
	id = strings.ToLower(strings.TrimSpace(id))
	if i := strings.IndexByte(id, '/'); i >= 0 && i+1 < len(id) {
		id = id[i+1:]
	}
	// strip review / effort suffixes for lookup
	if strings.HasSuffix(id, "-review") {
		id = strings.TrimSuffix(id, "-review")
	}
	if j := strings.LastIndex(id, " ("); j >= 0 && strings.HasSuffix(id, ")") {
		id = strings.TrimSpace(id[:j])
	}
	// exact
	if n, ok := knownContextWindows[id]; ok {
		return n
	}
	// longest prefix match
	best, bestLen := 0, 0
	for prefix, n := range knownContextWindows {
		if strings.HasPrefix(id, prefix) && len(prefix) > bestLen {
			best, bestLen = n, len(prefix)
		}
	}
	return best
}

// knownContextWindows — curated defaults (tokens), synced from 9router capabilities/models.dev on 2026-07-22.
// Unknown models stay 0 in SQLite and use the conservative hybrid fallback at runtime.
var knownContextWindows = map[string]int{
	// OpenAI / Codex family (ChatGPT backend / API-compatible ids)
	"gpt-5.6-sol":         400_000,
	"gpt-5.6-terra":       400_000,
	"gpt-5.6-luna":        400_000,
	"gpt-5.6":             400_000,
	"gpt-5.5":             272_000,
	"gpt-5.4":             400_000,
	"gpt-5.4-mini":        400_000,
	"gpt-5.3-codex-spark": 400_000,
	"gpt-5.3-codex":       200_000,
	"gpt-5.3":             200_000,
	"gpt-5.2":             200_000,
	"gpt-5.1":             200_000,
	"gpt-5":               200_000,
	"gpt-4.1":             1_000_000,
	"gpt-4o":              128_000,
	"o1":                  200_000,
	"o3":                  200_000,
	"o4-mini":             200_000,

	// Anthropic Claude
	"claude-opus-4-8":           1_000_000,
	"claude-opus-4-6":           1_000_000,
	"claude-opus-4-5":           200_000,
	"claude-sonnet-5":           1_000_000,
	"claude-sonnet-4-6":         1_000_000,
	"claude-sonnet-4-5":         200_000,
	"claude-haiku-4-5":          200_000,
	"claude-haiku-4-5-20251001": 200_000,
	"claude-fable-5":            1_000_000,
	"claude-opus-4":             200_000,
	"claude-sonnet-4":           200_000,
	"claude-3-5-sonnet":         200_000,
	"claude-3-opus":             200_000,
	"claude-3-haiku":            200_000,

	// xAI Grok
	"grok-4.5":              500_000,
	"grok-4":                256_000,
	"grok-4-fast-reasoning": 256_000,
	"grok-3":                131_072,
	"grok-code-fast-1":      256_000,
	"grok-2":                131_072,
}

func normalizeContextWindow(n int) int {
	if n < 0 {
		return 0
	}
	// hard ceiling guard — 10M tokens is beyond any current product window
	if n > 10_000_000 {
		return 10_000_000
	}
	return n
}

func resolveContextWindow(id string, explicit int) int {
	explicit = normalizeContextWindow(explicit)
	if explicit > 0 {
		return explicit
	}
	return DefaultContextWindow(id)
}

func FormatContextWindow(n int) string {
	n = normalizeContextWindow(n)
	if n <= 0 {
		return "hybrid"
	}
	if n%1_000_000 == 0 {
		return fmt.Sprintf("%dM", n/1_000_000)
	}
	if n >= 1000 && n%1000 == 0 {
		k := n / 1000
		if k >= 1000 && k%1000 == 0 {
			return fmt.Sprintf("%dM", k/1000)
		}
		return fmt.Sprintf("%dk", k)
	}
	return fmt.Sprintf("%d", n)
}

func (s *Store) ListCatalogModels(ctx context.Context, provider string) ([]CatalogModel, error) {
	provider = normalizeCatalogProvider(provider)
	var (
		query string
		args  []any
	)
	if provider == "" {
		query = `SELECT provider, id, label, context_window, max_output_tokens, created_at FROM catalog_models ORDER BY provider COLLATE NOCASE, id COLLATE NOCASE`
	} else {
		query = `SELECT provider, id, label, context_window, max_output_tokens, created_at FROM catalog_models WHERE provider = ? ORDER BY id COLLATE NOCASE`
		args = []any{provider}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list catalog models: %w", err)
	}
	defer rows.Close()
	var models []CatalogModel
	for rows.Next() {
		var model CatalogModel
		var created int64
		if err := rows.Scan(&model.Provider, &model.ID, &model.Label, &model.ContextWindow, &model.MaxOutputTokens, &created); err != nil {
			return nil, fmt.Errorf("scan catalog model: %w", err)
		}
		if model.Label == "" || model.Label == model.ID {
			model.Label = PrettyModelLabel(model.ID)
		}
		if model.ContextWindow <= 0 {
			model.ContextWindow = DefaultContextWindow(model.ID)
		}
		model.CreatedAt = time.UnixMilli(created).UTC()
		models = append(models, model)
	}
	return models, rows.Err()
}

func (s *Store) AddCatalogModel(ctx context.Context, provider, id, label string, contextWindow ...int) (CatalogModel, error) {
	provider = normalizeCatalogProvider(provider)
	id = strings.TrimSpace(id)
	label = strings.TrimSpace(label)
	if provider == "" {
		return CatalogModel{}, fmt.Errorf("provider is required")
	}
	if id == "" {
		return CatalogModel{}, fmt.Errorf("model id is required")
	}
	if len(id) > 128 {
		return CatalogModel{}, fmt.Errorf("model id must be 128 characters or fewer")
	}
	if strings.ContainsAny(id, "\x00\r\n") {
		return CatalogModel{}, fmt.Errorf("model id contains invalid characters")
	}
	if label == "" || label == id {
		label = PrettyModelLabel(id)
	}
	explicit := 0
	if len(contextWindow) > 0 {
		explicit = contextWindow[0]
	}
	window := resolveContextWindow(id, explicit)
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO catalog_models (provider, id, label, context_window, created_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(provider, id) DO UPDATE SET
  label = CASE
    WHEN excluded.label != '' AND excluded.label != excluded.id THEN excluded.label
    WHEN catalog_models.label = '' OR catalog_models.label = catalog_models.id THEN excluded.label
    ELSE catalog_models.label
  END,
  context_window = CASE
    WHEN ? > 0 THEN ?
    WHEN catalog_models.context_window > 0 THEN catalog_models.context_window
    ELSE excluded.context_window
  END`,
		provider, id, label, window, now.UnixMilli(), explicit, explicit,
	); err != nil {
		return CatalogModel{}, fmt.Errorf("upsert catalog model: %w", err)
	}
	return CatalogModel{Provider: provider, ID: id, Label: label, ContextWindow: window, CreatedAt: now}, nil
}

func (s *Store) DeleteCatalogModel(ctx context.Context, provider, id string) error {
	provider = normalizeCatalogProvider(provider)
	id = strings.TrimSpace(id)
	if provider == "" {
		return fmt.Errorf("provider is required")
	}
	if id == "" {
		return fmt.Errorf("model id is required")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM catalog_models WHERE provider = ? AND id = ?`, provider, id)
	if err != nil {
		return fmt.Errorf("delete catalog model: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model not found")
	}
	return nil
}

func (s *Store) EnsureCatalogModels(ctx context.Context, provider string, ids []string) error {
	provider = normalizeCatalogProvider(provider)
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		label := PrettyModelLabel(id)
		window := DefaultContextWindow(id)
		now := time.Now().UTC().UnixMilli()
		// Startup seeding must never overwrite a user-configured context window.
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO catalog_models (provider, id, label, context_window, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(provider, id) DO UPDATE SET
  label = CASE
    WHEN catalog_models.label = '' OR catalog_models.label = catalog_models.id THEN excluded.label
    ELSE catalog_models.label
  END,
  context_window = CASE
    WHEN catalog_models.context_window > 0 THEN catalog_models.context_window
    ELSE excluded.context_window
  END`, provider, id, label, window, now); err != nil {
			return fmt.Errorf("ensure catalog model %q: %w", id, err)
		}
	}
	return nil
}

// CatalogContextWindow resolves a model window from SQLite. Exact id wins;
// normalized base id handles provider prefixes, review variants, and effort suffixes.
func (s *Store) CatalogContextWindow(ctx context.Context, model string) (int, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, context_window FROM catalog_models WHERE context_window > 0`)
	if err != nil {
		return 0, fmt.Errorf("resolve catalog context window: %w", err)
	}
	defer rows.Close()
	normalized := normalizeContextModelID(model)
	best, bestScore := 0, -1
	for rows.Next() {
		var id string
		var window int
		if err := rows.Scan(&id, &window); err != nil {
			return 0, err
		}
		score := -1
		switch {
		case strings.EqualFold(id, model):
			score = 10_000 + len(id)
		case normalizeContextModelID(id) == normalized:
			score = 5_000 + len(id)
		case normalized != "" && strings.HasPrefix(normalized, normalizeContextModelID(id)):
			score = len(normalizeContextModelID(id))
		}
		if score > bestScore {
			best, bestScore = window, score
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return best, nil
}

func normalizeContextModelID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if i := strings.IndexByte(id, '/'); i >= 0 && i+1 < len(id) {
		id = id[i+1:]
	}
	if strings.HasSuffix(id, "-review") {
		id = strings.TrimSuffix(id, "-review")
	}
	if i := strings.LastIndex(id, " ("); i >= 0 && strings.HasSuffix(id, ")") {
		id = strings.TrimSpace(id[:i])
	}
	return id
}

// CatalogContextWindows returns id -> context_window for all catalog models (window>0 only).
func (s *Store) CatalogContextWindows(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, context_window FROM catalog_models`)
	if err != nil {
		return nil, fmt.Errorf("list catalog context windows: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var window int
		if err := rows.Scan(&id, &window); err != nil {
			return nil, err
		}
		if window <= 0 {
			window = DefaultContextWindow(id)
		}
		if window > 0 {
			out[id] = window
		}
	}
	return out, rows.Err()
}

// CatalogMaxOutputTokens returns the observed output-token caps, keyed by model id.
//
// Unlike context windows there is no fallback table: an unobserved model is absent
// rather than guessed. Forwarding a max_tokens the model rejects costs one request
// that the gateway retries transparently; clamping a model that would have complied
// silently shortens every answer it gives, which is far harder to notice.
func (s *Store) CatalogMaxOutputTokens(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, max_output_tokens FROM catalog_models WHERE max_output_tokens > 0`)
	if err != nil {
		return nil, fmt.Errorf("list catalog output limits: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var limit int
		if err := rows.Scan(&id, &limit); err != nil {
			return nil, err
		}
		if limit > 0 {
			out[id] = limit
		}
	}
	return out, rows.Err()
}

// RecordCatalogMaxOutputTokens persists an observed output-token cap, creating the
// catalog row when the model was never seeded — custom-provider models reach the
// gateway without ever being enumerated.
//
// It only ever lowers a recorded cap. A higher observation means the upstream
// changed its mind or a different model answered under the same alias, and taking
// the larger number would put the gateway straight back into rejections.
func (s *Store) RecordCatalogMaxOutputTokens(ctx context.Context, id string, limit int) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("model id is required")
	}
	if limit <= 0 {
		return fmt.Errorf("output limit must be positive")
	}
	// Updating by id alone, rather than upserting on (provider, id): the provider a
	// model was seeded under is not always what inferCatalogProvider would guess for
	// it, and an ON CONFLICT that misses would insert a second row for the same model.
	result, err := s.db.ExecContext(ctx, `
UPDATE catalog_models SET max_output_tokens = ?
WHERE id = ? AND (max_output_tokens <= 0 OR ? < max_output_tokens)`, limit, id, limit)
	if err != nil {
		return fmt.Errorf("record catalog output limit: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected > 0 {
		return nil
	}
	// Nothing was updated, which means either the model is not catalogued at all or a
	// lower cap is already recorded. Only the first case warrants an insert.
	var existing int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM catalog_models WHERE id = ?`, id).Scan(&existing); err != nil {
		return fmt.Errorf("record catalog output limit: %w", err)
	}
	if existing > 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO catalog_models (provider, id, label, context_window, max_output_tokens, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		inferCatalogProvider(id), id, PrettyModelLabel(id), DefaultContextWindow(id), limit,
		time.Now().UTC().UnixMilli(),
	); err != nil {
		return fmt.Errorf("record catalog output limit: %w", err)
	}
	return nil
}

// RecordCatalogContextWindow persists a context window the gateway learned from an
// upstream rejection, keyed by model id alone.
//
// It overwrites whatever was there, in either direction, which is the difference from
// SetCatalogContextWindow. The value being replaced is a curated default or a
// hand-entered number — a guess either way — while this one came from the upstream
// naming its own limit. Preferring the guess would defeat the point of learning.
//
// Keyed by id rather than upserted on (provider, id) for the same reason
// RecordCatalogMaxOutputTokens is: the provider a model was seeded under is not
// always what inferCatalogProvider guesses for it, and an ON CONFLICT that misses
// inserts a second row for the same model.
func (s *Store) RecordCatalogContextWindow(ctx context.Context, id string, window int) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("model id is required")
	}
	window = normalizeContextWindow(window)
	if window <= 0 {
		return fmt.Errorf("context window must be positive")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE catalog_models SET context_window = ?
WHERE id = ? AND context_window <> ?`, window, id, window)
	if err != nil {
		return fmt.Errorf("record catalog context window: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected > 0 {
		return nil
	}
	// Either the value is already recorded or the model was never catalogued. Only the
	// second case warrants an insert.
	var existing int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM catalog_models WHERE id = ?`, id).Scan(&existing); err != nil {
		return fmt.Errorf("record catalog context window: %w", err)
	}
	if existing > 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO catalog_models (provider, id, label, context_window, created_at)
VALUES (?, ?, ?, ?, ?)`,
		inferCatalogProvider(id), id, PrettyModelLabel(id), window, time.Now().UTC().UnixMilli(),
	); err != nil {
		return fmt.Errorf("record catalog context window: %w", err)
	}
	return nil
}

// SetCatalogContextWindow updates a model's context window.
func (s *Store) SetCatalogContextWindow(ctx context.Context, provider, id string, window int) error {
	provider = normalizeCatalogProvider(provider)
	id = strings.TrimSpace(id)
	window = normalizeContextWindow(window)
	if provider == "" || id == "" {
		return fmt.Errorf("provider and model id are required")
	}
	if window <= 0 {
		window = DefaultContextWindow(id)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE catalog_models SET context_window = ? WHERE provider = ? AND id = ?`, window, provider, id)
	if err != nil {
		return fmt.Errorf("set context window: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model not found")
	}
	return nil
}

func (s *Store) migrateCatalogModels(ctx context.Context) error {
	var tableSQL string
	err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'catalog_models'`).Scan(&tableSQL)
	if err != nil {
		return fmt.Errorf("inspect catalog_models: %w", err)
	}
	lower := strings.ToLower(tableSQL)
	if strings.Contains(lower, "provider") && strings.Contains(lower, "primary key (provider, id)") {
		if !strings.Contains(lower, "max_output_tokens") {
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE catalog_models ADD COLUMN max_output_tokens INTEGER NOT NULL DEFAULT 0`); err != nil {
				if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
					return fmt.Errorf("add catalog_models.max_output_tokens: %w", err)
				}
			}
		}
		if !strings.Contains(lower, "context_window") {
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE catalog_models ADD COLUMN context_window INTEGER NOT NULL DEFAULT 0`); err != nil {
				msg := strings.ToLower(err.Error())
				if !strings.Contains(msg, "duplicate column") {
					return fmt.Errorf("add catalog_models.context_window: %w", err)
				}
			}
		}
		// Backfill missing context windows from known defaults (do not overwrite custom >0).
		rowsW, err := s.db.QueryContext(ctx, `SELECT provider, id, context_window FROM catalog_models`)
		if err != nil {
			return err
		}
		type winRow struct {
			provider, id string
			window       int
		}
		var wins []winRow
		for rowsW.Next() {
			var r winRow
			if err := rowsW.Scan(&r.provider, &r.id, &r.window); err != nil {
				rowsW.Close()
				return err
			}
			wins = append(wins, r)
		}
		rowsW.Close()
		for _, r := range wins {
			if r.window > 0 {
				continue
			}
			def := DefaultContextWindow(r.id)
			if def <= 0 {
				continue
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE catalog_models SET context_window = ? WHERE provider = ? AND id = ? AND context_window <= 0`, def, r.provider, r.id); err != nil {
				return err
			}
		}
		// Backfill empty/equal labels to pretty names.
		rows, err := s.db.QueryContext(ctx, `SELECT provider, id, label FROM catalog_models`)
		if err != nil {
			return err
		}
		type row struct{ provider, id, label string }
		var all []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.provider, &r.id, &r.label); err != nil {
				rows.Close()
				return err
			}
			all = append(all, r)
		}
		rows.Close()
		for _, r := range all {
			if r.label != "" && r.label != r.id {
				continue
			}
			pretty := PrettyModelLabel(r.id)
			if pretty == "" || pretty == r.label {
				continue
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE catalog_models SET label = ? WHERE provider = ? AND id = ?`, pretty, r.provider, r.id); err != nil {
				return err
			}
		}
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
CREATE TABLE catalog_models_v2 (
    provider TEXT NOT NULL,
    id TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    context_window INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (provider, id)
)`); err != nil {
		return fmt.Errorf("create catalog_models_v2: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, label, created_at FROM catalog_models`)
	if err != nil {
		return fmt.Errorf("read legacy catalog_models: %w", err)
	}
	for rows.Next() {
		var id, label string
		var created int64
		if err := rows.Scan(&id, &label, &created); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy catalog model: %w", err)
		}
		provider := inferCatalogProvider(id)
		if label == "" || label == id {
			label = PrettyModelLabel(id)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO catalog_models_v2 (provider, id, label, context_window, created_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(provider, id) DO NOTHING`, provider, id, label, DefaultContextWindow(id), created); err != nil {
			rows.Close()
			return fmt.Errorf("migrate catalog model %q: %w", id, err)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if _, err := tx.ExecContext(ctx, `DROP TABLE catalog_models`); err != nil {
		return fmt.Errorf("drop legacy catalog_models: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE catalog_models_v2 RENAME TO catalog_models`); err != nil {
		return fmt.Errorf("rename catalog_models_v2: %w", err)
	}
	return tx.Commit()
}
