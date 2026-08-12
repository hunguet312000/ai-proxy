package gateway

import (
	"strings"
)

// The fallback model is the last candidate in every model chain: whatever the client
// asked for, whatever the routing rules picked, if nothing in the chain could be served
// the turn goes here instead of failing.
//
// It exists because "no eligible account" is not a downstream outage, but was reported as
// one. Every 502 in a day of logs was the same shape — a `claude-*` model id resolving to
// the `claude` provider, which had no account configured at all, so `accounts_tried` was
// zero and the turn had never left the process:
//
//	model=claude-opus-5 provider=claude waves=1 accounts_tried=0 error="no eligible account"
//
// The client sends those ids without being asked to: Claude Code resolves its opus,
// sonnet and haiku tiers to `claude-*` whenever the matching ANTHROPIC_DEFAULT_*_MODEL is
// unset, and the model catalogue advertises them, so a request for one is expected rather
// than a misconfiguration to be punished with a 502.
//
// Appended to the chain rather than swapped in at the front, and deliberately: a chain
// that can be served must still be served by what the routing rules chose. This only
// catches the case where the alternative is failing — which also covers the case that has
// nothing to do with `claude-*`, an upstream whose accounts are all disabled or
// circuit-broken at the moment the turn arrives. Empty turns it off and restores the 502.
func (s *Service) withFallbackModel(chain []string) []string {
	fallback := s.FallbackModel()
	if fallback == "" {
		return chain
	}
	// Already reachable is the common case — the fallback is usually the session model —
	// and appending it twice would spend a second attempt proving the same thing.
	for _, model := range chain {
		if strings.EqualFold(strings.TrimSpace(model), fallback) {
			return chain
		}
	}
	return append(chain, fallback)
}

// FallbackModel reports the model that serves a turn no other candidate could, or empty
// when the fallback is off.
func (s *Service) FallbackModel() string {
	if value := s.fallbackModel.Load(); value != nil {
		return *value
	}
	return ""
}

// SetFallbackModel changes the fallback on a running gateway, so the dashboard does not
// need a restart to take effect. Same atomic-pointer contract as SetPlanModel: read on
// every request, written once in a session. Empty turns it off.
func (s *Service) SetFallbackModel(model string) {
	model = strings.TrimSpace(model)
	s.fallbackModel.Store(&model)
}
