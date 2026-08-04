package gateway

import (
	"strings"

	"literouter/internal/translator"
)

// Claude Code announces plan mode transitions as meta ("system-reminder") text
// blocks on user messages, not through any field on the request. These are the
// three markers it emits, verified against the 2.1.x client:
//
//   - planEnteredMarker  the full instruction block, sent once on entering plan mode
//   - planActiveMarker   the sparse per-turn restatement that references it
//   - planExitedMarker   the "## Exited Plan Mode" block, sent once on approval
//
// Matching on prose is unpleasant, but the alternative signals are all worse: the
// permission mode never reaches the wire, and the tool set does not discriminate
// because plan mode still exposes Write/Edit for the plan file itself.
const (
	planEnteredMarker = "Plan mode is active."
	planActiveMarker  = "Plan mode still active"
	planExitedMarker  = "You have exited plan mode"
)

// planModeActive reports whether the newest plan-mode marker in the transcript is
// an enter rather than an exit.
//
// Presence alone is not enough: the full instruction block stays in history after
// the plan is approved — the sparse reminder exists precisely to point back at it —
// so a plain "does the history mention plan mode" test would pin every subsequent
// implementation turn to the plan model for the rest of the session. Scanning
// backwards and letting the first marker decide handles that, and re-entering plan
// mode later in the same session, without the gateway holding any state.
//
// Only text blocks on user messages can carry a reminder, so tool results — which
// are the bulk of a coding transcript — are skipped rather than searched.
func planModeActive(request translator.AnthropicRequest) bool {
	for index := len(request.Messages) - 1; index >= 0; index-- {
		message := request.Messages[index]
		if message.Role != "user" {
			continue
		}
		for block := len(message.Content) - 1; block >= 0; block-- {
			content := message.Content[block]
			if content.Type != "text" || content.Text == "" {
				continue
			}
			switch {
			case strings.Contains(content.Text, planExitedMarker):
				return false
			case strings.Contains(content.Text, planActiveMarker),
				strings.Contains(content.Text, planEnteredMarker):
				return true
			}
		}
	}
	return false
}

// planModeModel reports the model that should serve this turn when plan mode is
// active and a plan model is configured.
//
// This exists because Claude Code's own opusplan setting gives up above 200k
// prompt tokens: its plan-mode upgrade is gated on !exceeds200kTokens, so on a
// large context the client silently plans with the cheap resting model — exactly
// the sessions where a good plan matters most. Deciding here is not subject to
// that gate.
func (s *Service) planModeModel(request translator.AnthropicRequest) (string, bool) {
	planModel := s.PlanModel()
	if planModel == "" || planModel == request.Model {
		return "", false
	}
	if !planModeActive(request) {
		return "", false
	}
	return planModel, true
}

// PlanModel reports the model plan-mode turns are routed to, or empty when the
// override is off.
func (s *Service) PlanModel() string {
	if value := s.planModel.Load(); value != nil {
		return *value
	}
	return ""
}

// SetPlanModel changes the plan-mode override on a running gateway, so the dashboard
// does not need a restart to take effect. Empty turns the override off.
//
// It stores a pointer rather than guarding a string field because planModeModel reads
// it on every /v1/messages request: an atomic swap keeps that read free, where a mutex
// would put a lock on the hot path for a value that changes once in a session.
func (s *Service) SetPlanModel(model string) {
	model = strings.TrimSpace(model)
	s.planModel.Store(&model)
}
