package usage

import (
	"context"
	"time"

	"literouter/internal/storage"
)

// RecordGatewayUsage synchronously persists a completed gateway request.
func (s *Service) RecordGatewayUsage(ctx context.Context, event storage.UsageEvent) {
	if s == nil || s.tracker == nil || s.tracker.store == nil {
		return
	}
	event = normalizeUsageEvent(event)
	if err := s.tracker.store.InsertUsageEvent(ctx, event); err != nil && s.logger != nil {
		s.logger.Warn("record usage event", "error", err)
	}
}

// EnqueueGatewayUsage records analytics without blocking the request path.
func (s *Service) EnqueueGatewayUsage(event storage.UsageEvent) bool {
	if s == nil || s.writer == nil {
		return false
	}
	return s.writer.enqueue(normalizeUsageEvent(event))
}

func normalizeUsageEvent(event storage.UsageEvent) storage.UsageEvent {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	// Canonical OpenAI-style: prompt is cache-inclusive.
	canon := CanonicalizeUsage(event.PromptTokens, event.CompletionTokens, event.CachedTokens, 0, 0, false)
	event.PromptTokens = canon.PromptTokens
	event.CompletionTokens = canon.CompletionTokens
	event.CachedTokens = canon.CachedTokens
	event.TotalTokens = canon.PromptTokens + canon.CompletionTokens
	if event.CostUSD == 0 {
		event.CostUSD = CalculateCostUSD(event.Model, canon)
	}
	return event
}

func (s *Service) Close(ctx context.Context) error {
	if s == nil || s.writer == nil {
		return nil
	}
	return s.writer.close(ctx)
}

func (s *Service) UsageSummary(ctx context.Context, since time.Time, recentLimit int) (storage.UsageSummary, error) {
	if s == nil || s.tracker == nil || s.tracker.store == nil {
		return storage.UsageSummary{}, nil
	}
	return s.tracker.store.UsageSummary(ctx, since, recentLimit)
}
