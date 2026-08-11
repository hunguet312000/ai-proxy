package gateway

import (
	"log/slog"
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
	active, _ := planModeState(request)
	return active
}

// planModeState is planModeActive with the evidence it used, so a turn that routed
// surprisingly can be explained without re-deriving this by hand.
//
// Three tiers, most trustworthy first. They exist because the markers are prose, and
// prose that merely quotes one is indistinguishable from the real thing: measured across
// every transcript in this project, all 12+ occurrences were quotations — compact
// summaries and analysis text fed back as user messages — and not one was a reminder the
// client had injected. A stale quoted "exited" in that history silently outvoted the
// current turn, which is how a plan turn ends up on the session model.
//
//  1. a marker inside a <system-reminder> on the current turn or in System
//  2. any marker on the current turn or in System
//  3. the newest marker anywhere in history — the original rule, kept as the floor
//
// Tier 1 is deliberately a preference and not a requirement. Reminders are injected at
// request time and never persisted, so no transcript can confirm the wire shape; if they
// turn out not to be wrapped, tier 1 simply never fires and nothing changes. Tier 2 is
// the substance: the client restates plan mode every turn it is active, so the current
// turn is authoritative and no older text may override it. Tier 3 keeps the previous
// behaviour for clients that do not restate.
func planModeState(request translator.AnthropicRequest) (active bool, evidence string) {
	current := currentTurnContent(request)
	if active, found := reminderMarkerState(current); found {
		return active, "system-reminder on the current turn"
	}
	if active, found := planMarkerState(current); found {
		return active, "marker on the current turn"
	}
	// System/developer messages are current request instructions too. Claude Code 2.1.227
	// places its plan marker in one of these message-shaped instruction blocks rather than
	// in AnthropicRequest.System. Tool results — which are the bulk of a coding transcript —
	// are still skipped rather than searched.
	if active, found := planMarkerState(currentTurnSystemContent(request)); found {
		return active, "marker in system instructions on the current turn"
	}
	for index := len(request.Messages) - 1; index >= 0; index-- {
		message := request.Messages[index]
		if message.Role != "user" {
			continue
		}
		if active, found := planMarkerState(message.Content); found {
			return active, "marker in older history"
		}
	}
	return false, "no marker"
}

// currentTurnSystemContent is the newest instruction-shaped message in the request.
//
// Measured against Claude Code 2.1.227: the plan reminder arrives as a system-role
// message inside Messages, not in the top-level System field, which is why the
// top-level check alone reported "no marker" on a real plan turn. The newest such
// message wins, because a reconstructed transcript can retain older ones.
func currentTurnSystemContent(request translator.AnthropicRequest) []translator.AnthropicContent {
	for index := len(request.Messages) - 1; index >= 0; index-- {
		message := request.Messages[index]
		if message.Role == "system" || message.Role == "developer" {
			return message.Content
		}
	}
	return nil
}

// currentTurnContent is what describes this request rather than its past: the top-level
// system block, plus the last user message. Claude Code has emitted the reminder in both
// across client versions, so both are read as one.
func currentTurnContent(request translator.AnthropicRequest) []translator.AnthropicContent {
	content := append([]translator.AnthropicContent(nil), request.System...)
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role != "user" {
			continue
		}
		return append(content, request.Messages[index].Content...)
	}
	return content
}

// reminderMarkerState reads only markers that sit inside a complete <system-reminder>
// pair, which prose quoting a marker will not usually reproduce.
func reminderMarkerState(contents []translator.AnthropicContent) (active, found bool) {
	reminders := make([]translator.AnthropicContent, 0, len(contents))
	for _, content := range contents {
		if content.Type != "text" {
			continue
		}
		for _, inner := range systemReminders(content.Text) {
			reminders = append(reminders, translator.AnthropicContent{Type: "text", Text: inner})
		}
	}
	return planMarkerState(reminders)
}

func systemReminders(text string) []string {
	const open, close = "<system-reminder>", "</system-reminder>"
	var out []string
	for {
		start := strings.Index(text, open)
		if start < 0 {
			return out
		}
		text = text[start+len(open):]
		end := strings.Index(text, close)
		if end < 0 {
			return out
		}
		out = append(out, text[:end])
		text = text[end+len(close):]
	}
}

// planMarkerState reports the last plan-mode marker in one content collection.
// Top-level system content describes the current request, while message content is
// scanned backwards by planModeActive so an approval can override an older reminder.
func planMarkerState(contents []translator.AnthropicContent) (active, found bool) {
	for index := len(contents) - 1; index >= 0; index-- {
		content := contents[index]
		if content.Type != "text" || content.Text == "" {
			continue
		}
		switch {
		case strings.Contains(content.Text, planExitedMarker):
			return false, true
		case strings.Contains(content.Text, planActiveMarker),
			strings.Contains(content.Text, planEnteredMarker):
			return true, true
		}
	}
	return false, false
}

// planModeModel reports the model that should serve this turn when plan mode is
// active and a plan model is configured.
//
// This exists because Claude Code's own opusplan setting gives up above 200k prompt tokens:
// its plan-mode upgrade is gated on !exceeds200kTokens, so on a large context the client
// silently plans with the cheap resting model — exactly the sessions where a good plan
// matters most. Deciding here is not subject to that gate.
func (s *Service) planModeModel(request translator.AnthropicRequest) (string, bool) {
	planModel := s.PlanModel()
	if planModel == "" || planModel == request.Model {
		return "", false
	}
	active, evidence := planModeState(request)
	// Logged either way, and only once a plan model is configured so it stays quiet for
	// everyone else. Which tier decided is the one question no transcript can answer —
	// reminders never reach disk — so served traffic is the only place to learn whether
	// the wrapped form exists, and a turn that routed surprisingly says why it did.
	slog.Debug("plan mode evidence", "active", active, "evidence", evidence,
		"plan_model", planModel, "request_model", request.Model)
	if !active {
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
