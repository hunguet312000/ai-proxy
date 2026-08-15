package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/translator"
)

// productionShapedEffortService mirrors the deployed configuration, because the pieces
// that could drop an effort only run in that configuration: with the context pipeline on,
// every streaming candidate is rebuilt from the unified request rather than sent as the
// caller's OpenAI form, and a rebuild is exactly where a field goes missing.
func productionShapedEffortService(oauth OAuthInference, events chan UsageEvent) *Service {
	service := New(Options{
		OAuthInference: oauth,
		ContextEnabled: true, ContextMode: ContextModeAggressive, SummarizeMode: SummarizeModeTrim,
		ContextLimits: contextguard.Limits{Default: 400_000},
		OnUsage:       func(event UsageEvent) { events <- event },
	})
	service.ReplaceModelEfforts(map[string]string{"cx/gpt-5.6-luna": "max", "cx/gpt-5.6-sol": "high"})
	return service
}

func awaitUsage(t *testing.T, events chan UsageEvent) UsageEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("no usage event was recorded")
		return UsageEvent{}
	}
}

// The whole point of the per-model setting: the level the operator chose has to be on the
// request the upstream receives, and it has to be visible afterwards on the usage row.
//
// Both halves were needed. The effort did reach the Codex payload, but the usage bridge
// dropped the field on the way to storage, so every row read empty — and an empty column
// is indistinguishable from "the setting never applied", which is how a working override
// came to look like a broken one.
func TestCatalogEffortReachesTheUpstreamAndTheUsageRow(t *testing.T) {
	oauth := &effortRecordingInference{}
	events := make(chan UsageEvent, 4)
	service := productionShapedEffortService(oauth, events)

	recorder := postAnthropic(t, service, `{"model":"cx/gpt-5.6-luna","max_tokens":64,"stream":true,`+
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if len(oauth.efforts) != 1 || oauth.efforts[0] != "max" {
		t.Fatalf("upstream efforts = %v, want [max] — the configured level never reached the wire", oauth.efforts)
	}
	if event := awaitUsage(t, events); event.Effort != "max" {
		t.Fatalf("usage Effort = %q, want max", event.Effort)
	}
}

// The lookup is per candidate, so two models configured differently must not bleed into
// each other, and a model with no override must still leave the client in control.
func TestEffortIsResolvedPerModel(t *testing.T) {
	for _, testCase := range []struct {
		model string
		want  string
	}{
		{model: "cx/gpt-5.6-luna", want: "max"},
		{model: "cx/gpt-5.6-sol", want: "high"},
		{model: "cx/gpt-5.6-terra", want: ""},
	} {
		t.Run(testCase.model, func(t *testing.T) {
			oauth := &effortRecordingInference{}
			events := make(chan UsageEvent, 4)
			service := productionShapedEffortService(oauth, events)

			recorder := postAnthropic(t, service, `{"model":"`+testCase.model+`","max_tokens":64,"stream":true,`+
				`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
			}
			if len(oauth.efforts) != 1 || oauth.efforts[0] != testCase.want {
				t.Fatalf("upstream efforts = %v, want [%q]", oauth.efforts, testCase.want)
			}
			if event := awaitUsage(t, events); event.Effort != testCase.want {
				t.Fatalf("usage Effort = %q, want %q", event.Effort, testCase.want)
			}
		})
	}
}

// The non-streaming /v1/messages path had no Effort on its usage event at all — the field
// was simply absent from the struct literal, so this endpoint could never report one.
func TestNonStreamingMessagesRecordsTheEffortItSent(t *testing.T) {
	oauth := &fakeOAuthInference{}
	events := make(chan UsageEvent, 4)
	service := productionShapedEffortService(oauth, events)

	recorder := postAnthropic(t, service, `{"model":"cx/gpt-5.6-luna","max_tokens":64,`+
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if oauth.last.Effort != "max" {
		t.Fatalf("upstream Effort = %q, want max", oauth.last.Effort)
	}
	if event := awaitUsage(t, events); event.Effort != "max" {
		t.Fatalf("usage Effort = %q, want max", event.Effort)
	}
}

// Same gap on /v1/chat/completions, which shares the chat() candidate loop.
func TestChatCompletionsRecordsTheEffortItSent(t *testing.T) {
	client := &fakeClient{}
	events := make(chan UsageEvent, 4)
	service := New(Options{OpenAI: client, OnUsage: func(event UsageEvent) { events <- event }})
	service.ReplaceModelEfforts(map[string]string{"cx/gpt-5.6-luna": "max"})

	if _, err := service.Chat(context.Background(), translator.OpenAIRequest{
		Model:    "cx/gpt-5.6-luna",
		Messages: []translator.OpenAIMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	if client.last.Effort != "max" {
		t.Fatalf("upstream Effort = %q, want max", client.last.Effort)
	}
	if event := awaitUsage(t, events); event.Effort != "max" {
		t.Fatalf("usage Effort = %q, want max", event.Effort)
	}
}

// The "off" override strips reasoning_effort from the payload entirely, even
// when the route would force a level (plan/compact turns) — an upstream that
// rejects effort must never receive one.
func TestEffortOffStripsEffortFromTheWire(t *testing.T) {
	oauth := &effortRecordingInference{}
	events := make(chan UsageEvent, 4)
	service := productionShapedEffortService(oauth, events)
	service.ReplaceModelEfforts(map[string]string{"cx/gpt-5.6-luna": "off"})
	// A forced route effort must not resurrect the value the operator turned off.
	service.SetCompactModel("cx/gpt-5.6-luna")

	recorder := postAnthropic(t, service, compactBody(t))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if len(oauth.efforts) != 1 {
		t.Fatalf("upstream efforts = %v, want a single attempt", oauth.efforts)
	}
	if oauth.efforts[0] != "off" {
		t.Fatalf("upstream Effort = %q, want off", oauth.efforts[0])
	}
	if !oauth.noEffort[0] {
		t.Fatal("the payload still carried reasoning_effort despite the off override")
	}
	if event := awaitUsage(t, events); event.Effort != "off" {
		t.Fatalf("usage Effort = %q, want off", event.Effort)
	}
}

// A route-forced effort still wins over the per-model override, and the row must show the
// level that actually went up rather than the one the catalog would have chosen.
func TestForcedRouteEffortIsTheOneRecorded(t *testing.T) {
	oauth := &effortRecordingInference{}
	events := make(chan UsageEvent, 4)
	service := productionShapedEffortService(oauth, events)
	service.SetCompactModel("cx/gpt-5.6-luna")

	recorder := postAnthropic(t, service, compactBody(t))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if len(oauth.efforts) != 1 || oauth.efforts[0] != compactEffort {
		t.Fatalf("upstream efforts = %v, want [%q]", oauth.efforts, compactEffort)
	}
	if event := awaitUsage(t, events); event.Effort != compactEffort {
		t.Fatalf("usage Effort = %q, want %q", event.Effort, compactEffort)
	}
}

// prepareStreamCandidate rebuilds the candidate from the unified request whenever context
// work is enabled. That rebuild is the single point where a resolved effort is most easily
// lost, so it is pinned directly rather than only through a handler.
func TestPreparingAStreamCandidateKeepsTheResolvedEffort(t *testing.T) {
	service := New(Options{
		ContextEnabled: true, ContextMode: ContextModeAggressive,
		ContextLimits: contextguard.Limits{Default: 400_000},
	})
	unified := provider.Request{
		Model: "cx/gpt-5.6-luna", MaxTokens: 64,
		Messages: []provider.Message{{Role: "user", Content: []provider.Content{{Type: "text", Text: "hi"}}}},
	}
	candidate := translator.OpenAIRequest{
		Model: "cx/gpt-5.6-luna", Effort: "max", Stream: true,
		Messages: []translator.OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	prepared, _, err := service.prepareStreamCandidate(context.Background(), candidate, &unified)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Effort != "max" {
		t.Fatalf("prepared Effort = %q, want max", prepared.Effort)
	}
}

// A stream that breaks after delivering bytes still files a row, and that row is the only
// record of what the turn was sent at.
func TestBrokenStreamStillReportsTheEffort(t *testing.T) {
	oauth := &truncatingEffortInference{}
	events := make(chan UsageEvent, 4)
	service := productionShapedEffortService(oauth, events)

	recorder := postAnthropic(t, service, `{"model":"cx/gpt-5.6-luna","max_tokens":64,"stream":true,`+
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if event := awaitUsage(t, events); event.Effort != "max" {
		t.Fatalf("usage Effort = %q, want max on a truncated turn", event.Effort)
	}
}

// Delivers a first token and then ends without a finish reason, which is the truncated
// turn closeBrokenStream exists for.
type truncatingEffortInference struct{}

func (t *truncatingEffortInference) DoStream(context.Context, translator.OpenAIRequest, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(
		"data: {\"id\":\"one\",\"model\":\"served\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")), nil
}

func (t *truncatingEffortInference) DoJSON(context.Context, translator.OpenAIRequest, string) (translator.OpenAIResponse, error) {
	return translator.OpenAIResponse{}, io.EOF
}

func (t *truncatingEffortInference) SupportsAnthropicPassthrough(string) bool { return false }

func (t *truncatingEffortInference) DoAnthropicStream(context.Context, []byte, string, string, string) (io.ReadCloser, error) {
	return nil, io.EOF
}
