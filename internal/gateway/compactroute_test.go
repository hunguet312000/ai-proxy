package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"literouter/internal/contextguard"
	"literouter/internal/translator"
)

func compactUserMessage() translator.AnthropicMessage {
	return userText("Context is running low.", compactRequestMarker+" Focus on the user's requests.")
}

func TestIsCompactRequest(t *testing.T) {
	padding := userText(strings.Repeat("earlier conversation history ", 2000))
	cases := []struct {
		name     string
		messages []translator.AnthropicMessage
		tools    []translator.AnthropicTool
		rawBytes int
		want     bool
	}{
		{
			name:     "marker in the last user message of a large no-tools request",
			messages: []translator.AnthropicMessage{padding, compactUserMessage()},
			rawBytes: compactMinBytes,
			want:     true,
		},
		{
			name:     "tools present",
			messages: []translator.AnthropicMessage{padding, compactUserMessage()},
			tools:    []translator.AnthropicTool{{Name: "Read", InputSchema: json.RawMessage(`{}`)}},
			rawBytes: compactMinBytes,
		},
		{
			name:     "payload too small",
			messages: []translator.AnthropicMessage{compactUserMessage()},
			rawBytes: compactMinBytes - 1,
		},
		{
			// The post-compact continuation quotes summary content in an earlier
			// user message; the newest user message is an ordinary instruction.
			name: "marker only in an earlier user message",
			messages: []translator.AnthropicMessage{
				compactUserMessage(), assistantText("summary text"), userText("continue the task"),
			},
			rawBytes: compactMinBytes,
		},
		{
			name: "marker inside a non-text block",
			messages: []translator.AnthropicMessage{padding, {Role: "user", Content: []translator.AnthropicContent{
				{Type: "tool_result", ToolUseID: "t1", Content: compactRequestMarker},
			}}},
			rawBytes: compactMinBytes,
		},
		{
			name:     "marker echoed by the assistant only",
			messages: []translator.AnthropicMessage{padding, assistantText(compactRequestMarker), userText("go on")},
			rawBytes: compactMinBytes,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := translator.AnthropicRequest{Messages: testCase.messages, Tools: testCase.tools}
			if got := isCompactRequest(request, testCase.rawBytes); got != testCase.want {
				t.Fatalf("isCompactRequest = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestCompactRoutePrecedence(t *testing.T) {
	compactRequest := translator.AnthropicRequest{
		Model: "cheap",
		Messages: []translator.AnthropicMessage{
			userText("plan this", "<system-reminder>"+planEnteredMarker+"</system-reminder>"),
			compactUserMessage(),
		},
	}

	// No compact model configured: plan mode still owns the turn.
	planOnly := New(Options{PlanModel: "planner"})
	model, _, reason, overridden := planOnly.sizePlanAndCompactRoute(context.Background(), compactRequest, compactMinBytes)
	if !overridden || model != "planner" || reason != "plan mode" {
		t.Fatalf("without a compact model: %q %q %v", model, reason, overridden)
	}

	// Compact beats plan: a /compact issued mid-plan is a summarization task.
	both := New(Options{PlanModel: "planner", CompactModel: "fast"})
	model, effort, reason, overridden := both.sizePlanAndCompactRoute(context.Background(), compactRequest, compactMinBytes)
	if !overridden || model != "fast" || effort != compactEffort || reason != "compact request" {
		t.Fatalf("compact vs plan: %q %q %q %v", model, effort, reason, overridden)
	}

	// A compact request the compact model cannot hold goes to the long-context
	// model, still at the forced effort.
	rescued := New(Options{
		CompactModel: "fast", LongContextModel: "big", LongContextPercent: 60,
		ContextLimits: contextguard.Limits{Models: map[string]int{"fast": 10_000, "big": 400_000}},
	})
	model, effort, reason, overridden = rescued.sizePlanAndCompactRoute(context.Background(), compactRequest, 40_000)
	if !overridden || model != "big" || effort != compactEffort || !strings.Contains(reason, "compact request exceeds") {
		t.Fatalf("long-context rescue: %q %q %q %v", model, effort, reason, overridden)
	}

	// Compact model equal to the requested model still forces the effort — that is
	// the "keep the session model, just stop max-effort reasoning" configuration.
	same := New(Options{CompactModel: "cheap"})
	model, effort, _, overridden = same.sizePlanAndCompactRoute(context.Background(), compactRequest, compactMinBytes)
	if !overridden || model != "cheap" || effort != compactEffort {
		t.Fatalf("same-model compact: %q %q %v", model, effort, overridden)
	}
}

// effortRecordingInference records the translated stream requests, which is what
// proves the forced effort reached upstream selection.
type effortRecordingInference struct {
	models  []string
	efforts []string
}

func (f *effortRecordingInference) DoJSON(context.Context, translator.OpenAIRequest, string) (translator.OpenAIResponse, error) {
	return translator.OpenAIResponse{}, errors.New("unused")
}

func (f *effortRecordingInference) DoStream(_ context.Context, request translator.OpenAIRequest, _ string) (io.ReadCloser, error) {
	f.models = append(f.models, request.Model)
	f.efforts = append(f.efforts, request.Effort)
	return io.NopCloser(strings.NewReader(
		"data: {\"id\":\"one\",\"model\":\"served\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")), nil
}

func (f *effortRecordingInference) SupportsAnthropicPassthrough(string) bool { return false }

func (f *effortRecordingInference) DoAnthropicStream(context.Context, []byte, string, string, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func compactBody(t *testing.T) string {
	t.Helper()
	padding, err := json.Marshal(strings.Repeat("history line ", 3000))
	if err != nil {
		t.Fatal(err)
	}
	instruction, err := json.Marshal(compactRequestMarker + " Focus on the user's requests.")
	if err != nil {
		t.Fatal(err)
	}
	return `{"model":"claude-cheap","max_tokens":100,"stream":true,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":` + string(padding) + `}]},` +
		`{"role":"user","content":[{"type":"text","text":` + string(instruction) + `}]}]}`
}

func TestMessagesHandlerRoutesCompactTurnAtForcedEffort(t *testing.T) {
	oauth := &effortRecordingInference{}
	service := New(Options{OAuthInference: oauth, CompactModel: "fast-model"})
	// A catalog override on the compact model must lose to the route-forced effort:
	// the route describes the task, the catalog describes the model.
	service.ReplaceModelEfforts(map[string]string{"fast-model": "xhigh"})

	recorder := postAnthropic(t, service, compactBody(t))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if len(oauth.models) != 1 || oauth.models[0] != "fast-model" {
		t.Fatalf("upstream models = %v, want [fast-model]", oauth.models)
	}
	if oauth.efforts[0] != compactEffort {
		t.Fatalf("upstream effort = %q, want %q", oauth.efforts[0], compactEffort)
	}

	// An ordinary large no-tools turn without the marker routes unchanged.
	oauth.models, oauth.efforts = nil, nil
	body := strings.Replace(compactBody(t), compactRequestMarker, "please summarize my day", 1)
	recorder = postAnthropic(t, service, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if len(oauth.models) != 1 || oauth.models[0] != "claude-cheap" || oauth.efforts[0] != "" {
		t.Fatalf("normal turn routed: models=%v efforts=%v", oauth.models, oauth.efforts)
	}
}

func TestSetCompactModelAppliesToRunningService(t *testing.T) {
	service := New(Options{})
	if got := service.CompactModel(); got != "" {
		t.Fatalf("CompactModel = %q, want empty", got)
	}
	service.SetCompactModel("  fast  ")
	if got := service.CompactModel(); got != "fast" {
		t.Fatalf("CompactModel = %q, want fast", got)
	}
	service.SetCompactModel("")
	request := translator.AnthropicRequest{Messages: []translator.AnthropicMessage{compactUserMessage()}}
	if _, ok := service.compactRoute(request, compactMinBytes); ok {
		t.Fatal("compact route fired after being cleared")
	}
}
