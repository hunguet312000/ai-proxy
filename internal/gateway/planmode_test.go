package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"literouter/internal/translator"
)

func userText(texts ...string) translator.AnthropicMessage {
	message := translator.AnthropicMessage{Role: "user"}
	for _, text := range texts {
		message.Content = append(message.Content, translator.AnthropicContent{Type: "text", Text: text})
	}
	return message
}

func assistantText(text string) translator.AnthropicMessage {
	return translator.AnthropicMessage{Role: "assistant",
		Content: []translator.AnthropicContent{{Type: "text", Text: text}}}
}

func TestPlanModeActive(t *testing.T) {
	enter := "<system-reminder>" + planEnteredMarker + " The user indicated that they do not want you to execute yet</system-reminder>"
	sparse := "<system-reminder>" + planActiveMarker + " (see full instructions earlier in conversation).</system-reminder>"
	exit := "<system-reminder>## Exited Plan Mode\n" + planExitedMarker + ". You can now make edits.</system-reminder>"

	cases := []struct {
		name     string
		messages []translator.AnthropicMessage
		want     bool
	}{
		{name: "no markers", messages: []translator.AnthropicMessage{userText("refactor the handler")}},
		{
			name:     "entering plan mode",
			messages: []translator.AnthropicMessage{userText("plan this", enter)},
			want:     true,
		},
		{
			name: "sparse reminder on a later plan turn",
			messages: []translator.AnthropicMessage{
				userText("plan this", enter), assistantText("reading files"), userText(sparse),
			},
			want: true,
		},
		{
			// The full instruction block stays in history after approval, so presence
			// alone must not keep the turn on the plan model.
			name: "approved plan leaves the entering marker behind",
			messages: []translator.AnthropicMessage{
				userText("plan this", enter), assistantText("here is the plan"), userText(exit),
				assistantText("editing"), userText("continue"),
			},
		},
		{
			name: "plan mode re-entered after an earlier exit",
			messages: []translator.AnthropicMessage{
				userText("plan this", enter), assistantText("plan"), userText(exit),
				assistantText("done"), userText("plan the next bit", enter),
			},
			want: true,
		},
		{
			// A tool result quoting the marker — reading this very file, for instance —
			// is not a mode transition.
			name: "marker inside a non-text block",
			messages: []translator.AnthropicMessage{{Role: "user", Content: []translator.AnthropicContent{
				{Type: "tool_result", ToolUseID: "t1", Content: enter},
			}}},
		},
		{
			name:     "marker echoed by the assistant",
			messages: []translator.AnthropicMessage{userText("hi"), assistantText(enter)},
		},
		{
			name:     "plan marker in the top-level system block",
			messages: []translator.AnthropicMessage{userText("continue")},
			want:     true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := translator.AnthropicRequest{Messages: testCase.messages}
			if testCase.name == "plan marker in the top-level system block" {
				request.System = []translator.AnthropicContent{{Type: "text", Text: enter}}
			}
			got := planModeActive(request)
			if got != testCase.want {
				t.Fatalf("planModeActive = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestPlanModeModelRequiresConfiguration(t *testing.T) {
	planning := translator.AnthropicRequest{Model: "cheap",
		Messages: []translator.AnthropicMessage{userText(planEnteredMarker)}}

	if _, ok := New(Options{}).planModeModel(planning); ok {
		t.Fatal("plan mode override fired with no plan model configured")
	}
	// Already on the plan model: overriding would only add log noise.
	if _, ok := New(Options{PlanModel: "cheap"}).planModeModel(planning); ok {
		t.Fatal("plan mode override fired for the model already requested")
	}
	// Whitespace-only config is the same as unset, not a model named " ".
	if _, ok := New(Options{PlanModel: "  "}).planModeModel(planning); ok {
		t.Fatal("plan mode override fired for a blank plan model")
	}
	model, ok := New(Options{PlanModel: "strong"}).planModeModel(planning)
	if !ok || model != "strong" {
		t.Fatalf("planModeModel = %q, %v; want strong, true", model, ok)
	}
}

// modelRecordingInference captures the model each passthrough attempt resolved to,
// which is what proves the override reached upstream selection rather than only the
// classifier.
type modelRecordingInference struct {
	models []string
}

func (f *modelRecordingInference) DoJSON(context.Context, translator.OpenAIRequest, string) (translator.OpenAIResponse, error) {
	return translator.OpenAIResponse{}, errors.New("unused")
}

func (f *modelRecordingInference) DoStream(context.Context, translator.OpenAIRequest, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func (f *modelRecordingInference) SupportsAnthropicPassthrough(model string) bool {
	return strings.HasPrefix(model, "claude")
}

func (f *modelRecordingInference) DoAnthropicStream(_ context.Context, _ []byte, model, _, _ string) (io.ReadCloser, error) {
	f.models = append(f.models, model)
	return io.NopCloser(strings.NewReader(anthropicUpstreamStream)), nil
}

func planModeBody(t *testing.T, reminder string) string {
	t.Helper()
	encoded, err := json.Marshal(reminder)
	if err != nil {
		t.Fatalf("encode reminder: %v", err)
	}
	return `{"model":"claude-cheap","max_tokens":100,"stream":true,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"add caching"},` +
		`{"type":"text","text":` + string(encoded) + `}]}]}`
}

func TestMessagesHandlerRoutesPlanTurnsToPlanModel(t *testing.T) {
	oauth := &modelRecordingInference{}
	service := New(Options{OAuthInference: oauth, PlanModel: "claude-strong"})

	recorder := postAnthropic(t, service, planModeBody(t, planEnteredMarker+" You MUST NOT make any edits"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if len(oauth.models) != 1 || oauth.models[0] != "claude-strong" {
		t.Fatalf("upstream models = %v, want [claude-strong]", oauth.models)
	}

	// The same session after approval must fall back to the model the client sent.
	oauth.models = nil
	recorder = postAnthropic(t, service, planModeBody(t, "## Exited Plan Mode\n"+planExitedMarker+"."))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if len(oauth.models) != 1 || oauth.models[0] != "claude-cheap" {
		t.Fatalf("upstream models = %v, want [claude-cheap]", oauth.models)
	}
}

func TestSetPlanModelAppliesToRunningService(t *testing.T) {
	planning := translator.AnthropicRequest{Model: "cheap",
		Messages: []translator.AnthropicMessage{userText(planEnteredMarker)}}

	service := New(Options{})
	if _, ok := service.planModeModel(planning); ok {
		t.Fatal("override fired before any plan model was set")
	}

	service.SetPlanModel("  strong  ")
	if got := service.PlanModel(); got != "strong" {
		t.Fatalf("PlanModel = %q, want %q", got, "strong")
	}
	model, ok := service.planModeModel(planning)
	if !ok || model != "strong" {
		t.Fatalf("planModeModel = %q, %v; want strong, true", model, ok)
	}

	// Clearing turns the override off rather than routing to a model named "".
	service.SetPlanModel("")
	if got := service.PlanModel(); got != "" {
		t.Fatalf("PlanModel = %q, want empty", got)
	}
	if _, ok := service.planModeModel(planning); ok {
		t.Fatal("override still fired after being cleared")
	}
}

func TestPlanModelIsSafeUnderConcurrentUse(t *testing.T) {
	// planModeModel reads the override on every /v1/messages request while the dashboard
	// can write it at any moment, so the swap has to be atomic rather than a plain field.
	service := New(Options{PlanModel: "strong"})
	planning := translator.AnthropicRequest{Model: "cheap",
		Messages: []translator.AnthropicMessage{userText(planEnteredMarker)}}

	var group sync.WaitGroup
	for writer := 0; writer < 4; writer++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			for round := 0; round < 200; round++ {
				service.SetPlanModel(fmt.Sprintf("strong-%d", index))
			}
		}(writer)
	}
	for reader := 0; reader < 4; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for round := 0; round < 200; round++ {
				if model, ok := service.planModeModel(planning); ok && model == "" {
					t.Error("planModeModel reported an empty model as an override")
				}
			}
		}()
	}
	group.Wait()
}

// A plan-mode turn whose last user message is a tool_result is the agent mid tool-loop
// (reading, searching, running commands) — it must route to the cheap default model, not
// the expensive plan model. Only a fresh instruction (last message is text) earns plan.
func TestPlanModeModelRoutesToolLoopToDefault(t *testing.T) {
	service := New(Options{PlanModel: "strong"})
	// Tool-loop turn: plan active marker in history, last user message is a tool_result.
	toolLoop := translator.AnthropicRequest{Model: "cheap", Messages: []translator.AnthropicMessage{
		userText(planEnteredMarker),
		{Role: "user", Content: []translator.AnthropicContent{{Type: "tool_result", ToolUseID: "t1", Text: "output"}}},
	}}
	if model, ok := service.planModeModel(toolLoop); ok {
		t.Fatalf("tool-loop turn routed to %q, want default (no override)", model)
	}
	// Fresh-instruction turn: same history plus a new text user message → plan model.
	analysis := translator.AnthropicRequest{Model: "cheap", Messages: []translator.AnthropicMessage{
		userText(planEnteredMarker),
		{Role: "user", Content: []translator.AnthropicContent{{Type: "tool_result", ToolUseID: "t1", Text: "output"}}},
		{Role: "user", Content: []translator.AnthropicContent{{Type: "text", Text: "what is the plan?"}}},
	}}
	model, ok := service.planModeModel(analysis)
	if !ok || model != "strong" {
		t.Fatalf("analysis turn = %q, %v; want strong, true", model, ok)
	}
}
