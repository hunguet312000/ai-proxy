package gateway

import (
	"strings"
	"testing"

	"literouter/internal/translator"
)

func reminder(text string) string {
	return "<system-reminder>" + text + "</system-reminder>"
}

// Pins that the current turn is what decides, with a quotation of the opposite marker
// sitting in history. The previous backward scan reached this same answer — it met the last
// user message first — so this is a guard against a future change reordering the tiers,
// not a bug being fixed.
func TestCurrentTurnBeatsAStaleQuotationInHistory(t *testing.T) {
	summary := "Summary of earlier work: the assistant noted " + planExitedMarker +
		" and then implemented the change."
	request := translator.AnthropicRequest{
		Model: "cx/gpt-5.6-luna",
		Messages: []translator.AnthropicMessage{
			userText("start the task"),
			userText(summary),
			assistantText("understood"),
			userText("now plan the migration", planActiveMarker+" — do not edit files."),
		},
	}
	active, evidence := planModeState(request)
	if !active {
		t.Fatalf("plan mode was not detected; evidence = %q", evidence)
	}
	if !strings.Contains(evidence, "current turn") {
		t.Fatalf("evidence = %q, want the current turn to have decided", evidence)
	}
}

// The other direction, same kind of guard: approval on the current turn ends plan mode even
// though the full instruction block stays in history forever.
func TestCurrentTurnApprovalEndsPlanModeDespiteHistory(t *testing.T) {
	request := translator.AnthropicRequest{
		Model: "cx/gpt-5.6-luna",
		Messages: []translator.AnthropicMessage{
			userText(planEnteredMarker + " Research first, then write a plan."),
			assistantText("here is the plan"),
			userText("go ahead", planExitedMarker+" You may now edit files."),
		},
	}
	active, evidence := planModeState(request)
	if active {
		t.Fatalf("plan mode should have ended; evidence = %q", evidence)
	}
	if !strings.Contains(evidence, "current turn") {
		t.Fatalf("evidence = %q, want the current turn to have decided", evidence)
	}
}

// A wrapped marker outranks bare prose in the same turn. The prose is placed last on
// purpose: the previous rule took the last marker in block order, so it read the quotation
// and reported plan mode over. Only a preference — nothing depends on the wrapper
// existing, because no transcript can prove that it does.
func TestAWrappedMarkerOutranksProseInTheSameTurn(t *testing.T) {
	request := translator.AnthropicRequest{
		Model: "cx/gpt-5.6-luna",
		Messages: []translator.AnthropicMessage{
			userText(
				reminder(planActiveMarker+" Do not make edits yet."),
				"For reference, the approval message reads: "+planExitedMarker+".",
			),
		},
	}
	active, evidence := planModeState(request)
	if !active {
		t.Fatalf("the wrapped marker should have decided; evidence = %q", evidence)
	}
	if evidence != "system-reminder on the current turn" {
		t.Fatalf("evidence = %q, want the reminder tier", evidence)
	}
}

// An unterminated tag is prose, not a reminder — otherwise quoting the opening tag alone
// would promote whatever followed it.
func TestAnUnterminatedReminderTagIsNotTrusted(t *testing.T) {
	request := translator.AnthropicRequest{
		Model: "cx/gpt-5.6-luna",
		Messages: []translator.AnthropicMessage{
			userText("docs mention <system-reminder> and " + planActiveMarker + " in passing"),
		},
	}
	_, evidence := planModeState(request)
	if evidence == "system-reminder on the current turn" {
		t.Fatal("an unterminated tag was treated as a reminder")
	}
}

// With nothing on the current turn the previous rule still applies, so a client that does
// not restate plan mode every turn behaves exactly as before.
func TestHistoryStillDecidesWhenTheCurrentTurnIsSilent(t *testing.T) {
	request := translator.AnthropicRequest{
		Model: "cx/gpt-5.6-luna",
		Messages: []translator.AnthropicMessage{
			userText(planEnteredMarker + " Research first."),
			assistantText("investigating"),
			translator.AnthropicMessage{Role: "user", Content: []translator.AnthropicContent{
				{Type: "tool_result", ToolUseID: "call_1", Text: "file contents"}}},
		},
	}
	active, evidence := planModeState(request)
	if !active {
		t.Fatalf("history should still decide; evidence = %q", evidence)
	}
	if evidence != "marker in older history" {
		t.Fatalf("evidence = %q, want the history tier", evidence)
	}
}

// The reminder has appeared in the top-level system block across client versions, and that
// block describes the current request, so it counts as the current turn.
func TestSystemBlockCountsAsTheCurrentTurn(t *testing.T) {
	request := translator.AnthropicRequest{
		Model:  "cx/gpt-5.6-luna",
		System: []translator.AnthropicContent{{Type: "text", Text: reminder(planActiveMarker)}},
		Messages: []translator.AnthropicMessage{
			userText("Earlier: " + planExitedMarker),
			assistantText("done"),
			userText("keep planning"),
		},
	}
	active, evidence := planModeState(request)
	if !active {
		t.Fatalf("the system block should have decided; evidence = %q", evidence)
	}
	if evidence != "system-reminder on the current turn" {
		t.Fatalf("evidence = %q, want the reminder tier", evidence)
	}
}

func TestSystemRoleMessageCountsAsTheCurrentTurn(t *testing.T) {
	request := translator.AnthropicRequest{
		Model: "cx/gpt-5.6-luna",
		Messages: []translator.AnthropicMessage{
			userText("Earlier: " + planExitedMarker),
			{Role: "system", Content: []translator.AnthropicContent{{
				Type: "text", Text: planEnteredMarker + " The user indicated that they do not want to execute yet.",
			}}},
			userText("make a plan"),
		},
	}
	active, evidence := planModeState(request)
	if !active {
		t.Fatalf("the system-role marker should have decided; evidence = %q", evidence)
	}
	if evidence != "marker in system instructions on the current turn" {
		t.Fatalf("evidence = %q, want the system-instructions tier", evidence)
	}
}

func TestNoMarkerAnywhereReportsSo(t *testing.T) {
	request := translator.AnthropicRequest{
		Model:    "cx/gpt-5.6-luna",
		Messages: []translator.AnthropicMessage{userText("just fix the bug")},
	}
	active, evidence := planModeState(request)
	if active || evidence != "no marker" {
		t.Fatalf("active = %v, evidence = %q", active, evidence)
	}
}
