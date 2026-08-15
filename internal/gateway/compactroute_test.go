package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/toolstore"
	"literouter/internal/translator"
)

func compactUserMessage() translator.AnthropicMessage {
	return userText("Context is running low.", compactRequestPrefix+" the conversation so far, paying close attention to the user's explicit requests.")
}

func compactRecentUserMessage() translator.AnthropicMessage {
	return userText("Context is running low.", compactRequestPrefix+" the RECENT portion of the conversation — the messages that follow earlier retained context.")
}

func compactThisConversationUserMessage() translator.AnthropicMessage {
	return userText("Context is running low.", compactRequestPrefix+" this conversation. This summary will be placed at the start of a continuing session.")
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
				{Type: "tool_result", ToolUseID: "t1", Content: compactRequestPrefix + " the conversation so far"},
			}}},
			rawBytes: compactMinBytes,
		},
		{
			name:     "marker echoed by the assistant only",
			messages: []translator.AnthropicMessage{padding, assistantText(compactRequestPrefix + " the conversation so far"), userText("go on")},
			rawBytes: compactMinBytes,
		},
		{
			name:     "recent-portion variant",
			messages: []translator.AnthropicMessage{padding, compactRecentUserMessage()},
			rawBytes: compactMinBytes,
			want:     true,
		},
		{
			name:     "this-conversation variant",
			messages: []translator.AnthropicMessage{padding, compactThisConversationUserMessage()},
			rawBytes: compactMinBytes,
			want:     true,
		},
		{
			// The post-compact continuation quotes the summary text in its FIRST user
			// message; that summary carries the proxy's own marker, and this turn must
			// not be re-detected as a fresh compact (it would re-route to the compact
			// model and summarize a summary).
			name: "post-compact continuation quoting a proxy summary is not a compact",
			messages: []translator.AnthropicMessage{
				userText(contextguard.ProxySummaryMarker + " Historical context from earlier turns."),
				assistantText("understood"),
				userText("continue the task"),
			},
			rawBytes: compactMinBytes,
		},
		{
			name:     "prefix without a known target is not a compact",
			messages: []translator.AnthropicMessage{padding, userText("Your task is to create a detailed summary of the repo for onboarding.")},
			rawBytes: compactMinBytes,
		},
		{
			name:     "prefix embedded in prose is not a compact",
			messages: []translator.AnthropicMessage{padding, userText("Per the instruction, your task is to create a detailed summary of the conversation so far, then continue.")},
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
	// noEffort flags record whether the translated chat payload carried
	// reasoning_effort at all, for the "off" override test.
	noEffort []bool
}

func (f *effortRecordingInference) DoJSON(context.Context, translator.OpenAIRequest, string) (translator.OpenAIResponse, error) {
	return translator.OpenAIResponse{}, errors.New("unused")
}

func (f *effortRecordingInference) DoStream(_ context.Context, request translator.OpenAIRequest, _ string) (io.ReadCloser, error) {
	f.models = append(f.models, request.Model)
	f.efforts = append(f.efforts, request.Effort)
	f.noEffort = append(f.noEffort, request.ForceNoEffort && request.ReasoningEffort == "")
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
	instruction, err := json.Marshal(compactRequestPrefix + " the conversation so far, paying close attention to the user's requests.")
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
	body := strings.Replace(compactBody(t), compactRequestPrefix+" the conversation so far", "please summarize my day", 1)
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

// A compaction request must not be LLM-summarized on the way through: the model it is
// being sent to is about to summarize it. Measured on one real turn, the summarize pass
// cost 35.2s (645,471 -> 63,059) where the deterministic trim cost 1.1s (-> 154,651), and
// the client retried the compaction twice before giving up at 95%.
func TestCompactTurnSkipsTheSummarizerAndTrimsInstead(t *testing.T) {
	summarizer := &countingSummarizer{}
	// The window has to hold the backlog plus the summary, or the summarizer is skipped for
	// a reason that has nothing to do with this change and the test proves nothing.
	newService := func() *Service {
		return New(Options{
			ContextEnabled: true,
			ContextLimits:  contextguard.Limits{Default: 12_000},
			ContextPolicy: contextguard.Policy{
				SoftRatio: 0.50, SummarizeRatio: 0.60, HardRatio: 0.95, KeepRecentTurns: 1,
			},
			Summarizer: summarizer, SummaryMaxTokens: 321, SummaryTimeout: time.Second,
		})
	}
	request := provider.Request{Model: "model", Messages: []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("older fact ", 1500)}}},
		{Role: "assistant", Content: []provider.Content{{Type: "text", Text: strings.Repeat("older response ", 400)}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest instruction"}}},
	}}

	// An ordinary turn still gets the summarizer.
	if _, outcome, err := newService().prepareContextStages(context.Background(), request); err != nil {
		t.Fatalf("ordinary turn: %v", err)
	} else if outcome.stage != "summarize" {
		t.Fatalf("ordinary turn stage = %q, want summarize — the test proves nothing otherwise", outcome.stage)
	}
	if summarizer.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1 for the ordinary turn", summarizer.calls)
	}

	// The same turn marked as a compaction must not. It also must not need a trim here:
	// the cheap stages already brought it under the hard limit, so the correct outcome is
	// "forward it" — trimming would throw away history for nothing.
	_, outcome, err := newService().prepareContextStages(withCompactTurn(context.Background()), request)
	if err != nil {
		t.Fatalf("compact turn: %v", err)
	}
	if summarizer.calls != 1 {
		t.Fatalf("summarizer ran %d extra times for a compaction request", summarizer.calls-1)
	}
	if outcome.stage == "summarize" || outcome.stage == "summarize+trim" {
		t.Fatalf("compact turn stage = %q, want no summarize stage", outcome.stage)
	}
}

// And when skipping the summarizer leaves the turn still too large — the real case, where
// 645k arrived against a 370k window — it falls through to the deterministic trim rather
// than being forwarded oversized.
func TestCompactTurnTooLargeForTheWindowTrims(t *testing.T) {
	summarizer := &countingSummarizer{}
	service := New(Options{
		ContextEnabled: true,
		ContextLimits:  contextguard.Limits{Default: 12_000},
		// A hard limit the payload cannot meet without dropping turns.
		ContextPolicy: contextguard.Policy{
			SoftRatio: 0.20, SummarizeRatio: 0.25, HardRatio: 0.30, KeepRecentTurns: 1,
		},
		Summarizer: summarizer, SummaryMaxTokens: 321, SummaryTimeout: time.Second,
	})
	request := provider.Request{Model: "model", Messages: []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("older fact ", 1500)}}},
		{Role: "assistant", Content: []provider.Content{{Type: "text", Text: strings.Repeat("older response ", 400)}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest instruction"}}},
	}}
	_, outcome, err := service.prepareContextStages(withCompactTurn(context.Background()), request)
	if err != nil {
		t.Fatalf("compact turn: %v", err)
	}
	if summarizer.calls != 0 {
		t.Fatalf("summarizer ran %d times for a compaction request", summarizer.calls)
	}
	if outcome.stage != "trim" {
		t.Fatalf("stage = %q, want trim", outcome.stage)
	}
}

// The reference-store capture must survive the trip through the real production shape:
// aggressive mode, summarize=trim, a window big enough that compact (with truncation)
// fits the request without dropping turns — so the truncate marker that carries /ref/
// is exactly what is forwarded upstream, not a casualty of a later trim.
func TestAggressiveTruncateCaptureSurvivesTrimModeForwarding(t *testing.T) {
	store := toolstore.New(0)
	service := New(Options{
		ContextEnabled: true, ContextMode: ContextModeAggressive, SummarizeMode: SummarizeModeTrim,
		ContextLimits: contextguard.Limits{Default: 30_000},
		ContextPolicy: contextguard.Policy{
			SoftRatio: 0.20, TruncateRatio: 0.25, SummarizeRatio: 0.30, HardRatio: 0.95, KeepRecentTurns: 1,
		},
		ToolStore: store,
	})
	body := strings.Repeat("old tool result line that should be preserved\n", 900)
	var messages []provider.Message
	messages = append(messages,
		provider.Message{Role: "assistant", Content: []provider.Content{{Type: "tool_use", ToolUseID: "call-old", Name: "Bash", Input: []byte(`{"command":"make"}`)}}},
		provider.Message{Role: "user", Content: []provider.Content{{Type: "tool_result", ToolUseID: "call-old", Text: body}}},
		provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest instruction"}}},
	)
	request := provider.Request{Model: "model", Messages: messages, MaxTokens: 100}

	prepared, outcome, err := service.prepareContextStages(context.Background(), request)
	if err != nil {
		t.Fatalf("prepareContextStages() = %v", err)
	}
	// The request must be forwarded (compact enough to fit), not trimmed — trimming would
	// drop the very turn whose truncation we are trying to prove.
	if outcome.stage == "trim" {
		t.Fatal("a compactable request was trimmed; the truncate capture is moot")
	}
	before := contextguard.EstimateRequest(request)
	after := contextguard.EstimateRequest(prepared)
	if after >= before {
		t.Fatalf("aggressive compact did not shrink the request: %d -> %d", before, after)
	}
	// The marker with the /ref/ reference must be what is actually forwarded.
	var marker string
	for _, message := range prepared.Messages {
		for _, block := range message.Content {
			if strings.HasPrefix(block.Text, "[literouter:truncate-v1") {
				marker = block.Text
			}
		}
	}
	if marker == "" {
		t.Fatal("the forwarded request carries no truncate marker")
	}
	hash := toolstore.ID(body)
	if _, ok := store.Get(hash); !ok {
		t.Fatal("the truncated body was not captured into the reference store")
	}
	if !strings.Contains(marker, "/ref/"+hash) {
		t.Fatalf("forwarded marker lacks the fetch reference: %q", marker[:min(len(marker), 200)])
	}
}

type countingSummarizer struct{ calls int }

func (c *countingSummarizer) Summarize(_ context.Context, _ contextguard.SummaryInput) (string, error) {
	c.calls++
	return "tóm tắt", nil
}

// The flag has to survive the trip from messagesHandler into the context pipeline. The
// stage tests call prepareContextStages directly, so they would still pass if the handler
// never set it — which is exactly the plumbing that decides whether any of this fires in
// production.
func TestMessagesHandlerMarksCompactTurnsForThePipeline(t *testing.T) {
	summarizer := &countingSummarizer{}
	stream := &fakeStreamClient{content: strings.Join([]string{
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{"content":"ok"}}]}`, "",
		`data: {"id":"one","model":"model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "",
		"data: [DONE]", "",
	}, "\n")}
	newService := func() *Service {
		return New(Options{
			OpenAIStream:   stream,
			ContextEnabled: true,
			// Sized so both constraints hold at once: the raw body clears
			// compactMinBytes (32KiB) so detection can fire, while the backlog still
			// fits inside the window so the summarizer is skipped for the reason under
			// test rather than because its own call could not fit.
			ContextLimits: contextguard.Limits{Default: 60_000},
			ContextPolicy: contextguard.Policy{
				SoftRatio: 0.15, SummarizeRatio: 0.20, HardRatio: 0.95, KeepRecentTurns: 1,
			},
			Summarizer: summarizer, SummaryMaxTokens: 321, SummaryTimeout: time.Second,
			// Detection is independent of this, but set it so the turn also takes the
			// compact route — the shape a real session has.
			CompactModel: "model",
		})
	}
	// Big enough to pass compactMinBytes and to overflow the window either way.
	filler := strings.Repeat("bối cảnh lịch sử cần nén ", 2400)
	body := func(lastText string) string {
		payload := map[string]any{
			"model": "model", "max_tokens": 64, "stream": true,
			"messages": []map[string]any{
				{"role": "user", "content": []map[string]string{{"type": "text", "text": filler}}},
				{"role": "assistant", "content": []map[string]string{{"type": "text", "text": filler}}},
				{"role": "user", "content": []map[string]string{{"type": "text", "text": lastText}}},
			},
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	send := func(service *Service, raw string) {
		e := echo.New()
		service.Register(e)
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
		}
	}

	// A normal oversized turn summarizes.
	send(newService(), body("tiếp tục công việc"))
	if summarizer.calls == 0 {
		t.Fatal("ordinary oversized turn did not summarize, so this test proves nothing")
	}
	ordinary := summarizer.calls

	// The same turn whose last user message carries the compaction instruction does not.
	send(newService(), body("Your task is to create a detailed summary of the conversation so far."))
	if summarizer.calls != ordinary {
		t.Fatalf("summarizer ran %d extra times for a compaction request; the handler flag is not reaching the pipeline",
			summarizer.calls-ordinary)
	}

	// A post-compact continuation whose first user message quotes the proxy's summary
	// marker is not detected as a fresh compact, but it is also not re-summarized: the
	// pipeline sees the requestAlreadySummarized guard and skips the LLM pass the same
	// way it skips a compaction request.
	bodyWithSummary := func(lastText string) string {
		payload := map[string]any{
			"model": "model", "max_tokens": 64, "stream": true,
			"messages": []map[string]any{
				{"role": "user", "content": []map[string]string{{"type": "text", "text": contextguard.ProxySummaryMarker + " Historical context from earlier turns. " + filler}}},
				{"role": "assistant", "content": []map[string]string{{"type": "text", "text": filler}}},
				{"role": "user", "content": []map[string]string{{"type": "text", "text": lastText}}},
			},
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	send(newService(), bodyWithSummary("tiếp tục công việc"))
	if summarizer.calls != ordinary {
		t.Fatalf("summarizer ran %d extra times for a post-compact continuation quoting the proxy summary; the already-summarized guard is not reaching the pipeline",
			summarizer.calls-ordinary)
	}
}

// The dial the operator sets: trim mode must keep the summarizer out of every turn, not
// just compaction requests, and llm mode must leave it in.
func TestSummarizeModeTrimKeepsTheSummarizerOutOfOrdinaryTurns(t *testing.T) {
	request := provider.Request{Model: "model", Messages: []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: strings.Repeat("older fact ", 1500)}}},
		{Role: "assistant", Content: []provider.Content{{Type: "text", Text: strings.Repeat("older response ", 400)}}},
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest instruction"}}},
	}}
	build := func(mode string) (*Service, *countingSummarizer) {
		summarizer := &countingSummarizer{}
		return New(Options{
			ContextEnabled: true,
			ContextLimits:  contextguard.Limits{Default: 12_000},
			ContextPolicy: contextguard.Policy{
				SoftRatio: 0.50, SummarizeRatio: 0.60, HardRatio: 0.95, KeepRecentTurns: 1,
			},
			Summarizer: summarizer, SummaryMaxTokens: 321, SummaryTimeout: time.Second,
			SummarizeMode: mode,
		}), summarizer
	}

	service, summarizer := build(SummarizeModeLLM)
	if _, outcome, err := service.prepareContextStages(context.Background(), request); err != nil {
		t.Fatalf("llm mode: %v", err)
	} else if outcome.stage != "summarize" {
		t.Fatalf("llm mode stage = %q, want summarize", outcome.stage)
	}
	if summarizer.calls != 1 {
		t.Fatalf("llm mode summarizer calls = %d, want 1", summarizer.calls)
	}

	service, summarizer = build(SummarizeModeTrim)
	if _, outcome, err := service.prepareContextStages(context.Background(), request); err != nil {
		t.Fatalf("trim mode: %v", err)
	} else if outcome.stage == "summarize" || outcome.stage == "summarize+trim" {
		t.Fatalf("trim mode stage = %q, want no summarize stage", outcome.stage)
	}
	if summarizer.calls != 0 {
		t.Fatalf("trim mode called the summarizer %d times", summarizer.calls)
	}

	// An unset mode must behave like it always did.
	service, summarizer = build("")
	if _, _, err := service.prepareContextStages(context.Background(), request); err != nil {
		t.Fatalf("default mode: %v", err)
	}
	if summarizer.calls != 1 {
		t.Fatalf("default mode summarizer calls = %d, want 1 — the default changed", summarizer.calls)
	}
	// An invalid mode falls back to the default rather than silently disabling summaries.
	service, summarizer = build("nonsense")
	if got := service.SummarizeMode(); got != SummarizeModeLLM {
		t.Fatalf("invalid mode resolved to %q, want the llm default", got)
	}
	if _, _, err := service.prepareContextStages(context.Background(), request); err != nil {
		t.Fatalf("invalid mode: %v", err)
	}
	if summarizer.calls != 1 {
		t.Fatalf("invalid mode summarizer calls = %d, want 1", summarizer.calls)
	}
}
