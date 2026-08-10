package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"literouter/internal/contextguard"
)

// subagentPayload is the shape Claude Code sends for a subagent: one real user turn
// carrying the task, then nothing but tool_use/tool_result pairs. It has no second
// user turn, so the whole thing is a single summary unit.
func subagentPayload(model string, chains, resultBytes, maxTokens int) string {
	parts := []string{`{"role":"user","content":[{"type":"text","text":"audit the inference flow"}]}`}
	for index := range chains {
		parts = append(parts,
			fmt.Sprintf(`{"role":"assistant","content":[{"type":"tool_use","id":"call_%d","name":"Read","input":{"file_path":"/repo/f%d.go"}}]}`, index, index),
			fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_%d","content":%q}]}`, index, strings.Repeat("x", resultBytes)),
		)
	}
	return fmt.Sprintf(`{"model":%q,"messages":[%s],"max_tokens":%d,"stream":true}`,
		model, strings.Join(parts, ","), maxTokens)
}

// The incident this exists for: every subagent died with "Prompt is too long" while
// ordinary sessions survived. summaryUnits only opens a unit on a user message that
// is not a tool_result, and a subagent never produces one after its opening task, so
// the transcript was a single unit that whole-turn trimming could not touch — and the
// proxy turned that into a 400 of its own before the upstream ever saw the request.
func TestSubagentTurnWithNoSecondUserTurnIsTrimmedAndServed(t *testing.T) {
	client := &overflowThenServeStreamClient{}
	service := New(Options{
		OpenAIStream:   client,
		ContextEnabled: true,
		ContextPolicy:  contextguard.DefaultPolicy(),
		ContextWindow:  func(context.Context, string) (int, error) { return 20_000, nil },
	})
	e := echo.New()
	service.Register(e)

	payload := subagentPayload("cx/gpt-5.6-luna", 2, 40_000, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "message_stop") {
		t.Fatalf("turn did not complete: %s", body)
	}
	if client.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 — the preflight trim should have made it servable", client.calls)
	}
	if client.sizes[0] >= len(payload) {
		t.Fatalf("request was not reduced: sent %d, original %d", client.sizes[0], len(payload))
	}
}

// The counterpart contract: with nothing droppable the proxy must still report the
// overflow, because Claude Code keys its own compaction off that error shape.
func TestSubagentTurnWithNothingDroppableStillReportsOverflow(t *testing.T) {
	service := New(Options{
		OpenAIStream:   &overflowThenServeStreamClient{},
		ContextEnabled: true,
		ContextPolicy:  contextguard.DefaultPolicy(),
		ContextWindow:  func(context.Context, string) (int, error) { return 20_000, nil },
	})
	e := echo.New()
	service.Register(e)

	payload := fmt.Sprintf(`{"model":"cx/gpt-5.6-luna","messages":[{"role":"user","content":%q}],"max_tokens":1024,"stream":true}`,
		strings.Repeat("x", 400_000))
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "prompt is too long") {
		t.Fatalf("overflow shape = %s", recorder.Body.String())
	}
}

// The summarize path is now skipped on the cheap keep=1 probe instead of the
// expensive keep-hunting one. A configured summarizer must still see the turn
// through, and the turn must still be served.
func TestSubagentTurnStillServedWithASummarizerConfigured(t *testing.T) {
	summarizer := &fakeSummarizer{text: "summary"}
	service := New(Options{
		OpenAIStream:     &overflowThenServeStreamClient{},
		ContextEnabled:   true,
		ContextPolicy:    contextguard.DefaultPolicy(),
		Summarizer:       summarizer,
		SummaryMaxTokens: 100,
		SummaryTimeout:   time.Minute,
		ContextWindow:    func(context.Context, string) (int, error) { return 20_000, nil },
	})
	e := echo.New()
	service.Register(e)

	payload := subagentPayload("cx/gpt-5.6-luna", 6, 40_000, 1024)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "message_stop") {
		t.Fatalf("turn did not complete: %s", recorder.Body.String())
	}
}

// The last escape: a client asking for an output budget the size of the window left
// no input budget, so the trim declined on the budget alone and the turn was rejected
// no matter how droppable its history was. The output ask is the upstream's business;
// the guard's job is to fit the input.
func TestOversizedOutputAskDoesNotStarveTheInputBudget(t *testing.T) {
	client := &overflowThenServeStreamClient{}
	service := New(Options{
		OpenAIStream:   client,
		ContextEnabled: true,
		ContextPolicy:  contextguard.DefaultPolicy(),
		ContextWindow:  func(context.Context, string) (int, error) { return 20_000, nil },
	})
	e := echo.New()
	service.Register(e)

	payload := subagentPayload("cx/gpt-5.6-luna", 4, 40_000, 20_000)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "message_stop") {
		t.Fatalf("turn did not complete: %s", body)
	}
	if client.sizes[0] >= len(payload) {
		t.Fatalf("request was not reduced: sent %d, original %d", client.sizes[0], len(payload))
	}
}
