package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
	"literouter/internal/toolstore"
)

// bigBashExchange is one tool call whose result is large enough to be truncated by
// aggressive mode at the small window these tests use.
func bigBashExchange(id, command, result string) []provider.Message {
	return []provider.Message{
		{Role: "assistant", Content: []provider.Content{
			{Type: "tool_use", ToolUseID: id, Name: "Bash", Input: []byte(fmt.Sprintf(`{"command":%q}`, command))},
		}},
		{Role: "user", Content: []provider.Content{
			{Type: "tool_result", ToolUseID: id, Text: result},
		}},
	}
}

func TestPrepareContextCapturesTruncatedResultsIntoTheStore(t *testing.T) {
	store := toolstore.New(0)
	service := New(Options{
		ContextEnabled: true, ContextMode: ContextModeAggressive,
		ContextLimits: contextguard.Limits{Default: 30_000},
		// SoftRatio tiny so the compact gate opens immediately; the window is small
		// enough that truncation must run to fit.
		ContextPolicy: contextguard.Policy{SoftRatio: 0.05, TruncateRatio: 0.055, SummarizeRatio: 0.06, HardRatio: 0.95, KeepRecentTurns: 1},
		ToolStore:     store,
	})
	var messages []provider.Message
	messages = append(messages, bigBashExchange("call-1", "make build",
		strings.Repeat("builder output line\n", 700))...)
	messages = append(messages, provider.Message{Role: "user", Content: []provider.Content{{Type: "text", Text: "latest instruction"}}})

	prepared, err := service.prepareContext(context.Background(), provider.Request{Model: "model", Messages: messages, MaxTokens: 100})
	if err != nil {
		t.Fatalf("prepareContext() = %v", err)
	}
	var marker string
	for _, message := range prepared.Messages {
		for _, block := range message.Content {
			if strings.HasPrefix(block.Text, "[literouter:truncate-v1") {
				marker = block.Text
			}
		}
	}
	if marker == "" {
		t.Fatal("aggressive mode truncated nothing on an over-budget request")
	}
	// The full body must have been captured, keyed by the hash the marker references.
	hash := toolstore.ID(strings.Repeat("builder output line\n", 700))
	if _, ok := store.Get(hash); !ok {
		t.Fatal("the truncated result body was not captured into the reference store")
	}
	if !strings.Contains(marker, "/ref/"+hash) {
		t.Fatalf("marker does not reference the captured body: %q", marker[:min(len(marker), 200)])
	}
}

func TestRefHandlerServesTheCapturedBodyAnd404sOtherwise(t *testing.T) {
	store := toolstore.New(0)
	body := strings.Repeat("recover me\n", 800)
	store.Put(toolstore.ID(body), "Bash", "call-9", body)
	service := New(Options{ToolStore: store})

	e := echo.New()
	service.Register(e)

	// Hit: the full body comes back as text/plain.
	hit := httptest.NewRequest(http.MethodGet, "/ref/"+toolstore.ID(body), nil)
	hitRecorder := httptest.NewRecorder()
	e.ServeHTTP(hitRecorder, hit)
	if hitRecorder.Code != http.StatusOK {
		t.Fatalf("GET /ref/<hash> = %d, want 200", hitRecorder.Code)
	}
	if hitRecorder.Body.String() != body {
		t.Fatal("ref endpoint did not return the full stored body")
	}
	if !strings.Contains(hitRecorder.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", hitRecorder.Header().Get("Content-Type"))
	}

	// Miss: unknown hash → 404, and the marker can point somewhere harmless.
	miss := httptest.NewRequest(http.MethodGet, "/ref/deadbeef", nil)
	missRecorder := httptest.NewRecorder()
	e.ServeHTTP(missRecorder, miss)
	if missRecorder.Code != http.StatusNotFound {
		t.Fatalf("GET /ref/<unknown> = %d, want 404", missRecorder.Code)
	}

	// Disabled: no store wired → 404, and the endpoint must not exist as a data path.
	disabled := New(Options{})
	disabledEcho := echo.New()
	disabled.Register(disabledEcho)
	req := httptest.NewRequest(http.MethodGet, "/ref/"+toolstore.ID(body), nil)
	rec := httptest.NewRecorder()
	disabledEcho.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ref endpoint with no store = %d, want 404", rec.Code)
	}
}
