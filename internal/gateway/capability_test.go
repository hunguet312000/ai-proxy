package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"literouter/internal/storage"
	"literouter/internal/translator"
)

// qwenUpstream mimics a vLLM server started without
// --enable-auto-tool-choice: it refuses tool_choice:"auto" and reasoning
// effort "high" exactly the way Qwen3's chat template does. Every request
// records what it saw, so the tests can prove the recovery shaped the retry.
type qwenUpstream struct {
	mu                  sync.Mutex
	rejectAuto          bool
	rejectEfforts       map[string]bool
	rejectDefaultEffort bool // template default for an absent reasoning_effort = "high"
	rejectAlways        bool
	sawToolChoice       []string
	sawEffort           []string
	servedWithNone      bool
}

func newQwenUpstream() *qwenUpstream {
	return &qwenUpstream{rejectAuto: true, rejectEfforts: map[string]bool{"high": true}}
}

func (u *qwenUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		toolChoice, _ := body["tool_choice"].(string)
		effort, _ := body["reasoning_effort"].(string)
		stream, _ := body["stream"].(bool)
		u.mu.Lock()
		u.sawToolChoice = append(u.sawToolChoice, toolChoice)
		u.sawEffort = append(u.sawEffort, effort)
		reject := u.rejectAlways || (u.rejectAuto && toolChoice == "auto") ||
			(effort != "" && u.rejectEfforts[effort]) ||
			(u.rejectDefaultEffort && effort == "")
		u.mu.Unlock()
		if reject {
			// rejectAlways keeps returning the same marker: a genuinely broken
			// upstream names the same fault every time, so no new fact is ever
			// learned and the retry budget is exhausted after the first one.
			message := `"auto" tool choice requires --enable-auto-tool-choice and --tool-call-parser to be set`
			if !u.rejectAlways && toolChoice != "auto" {
				message = "Unexpected reasoning effort " + effort + ". Supported types are xhigh (default), medium, and low."
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"` + message + `"}}`))
			return
		}
		if toolChoice == "none" {
			u.mu.Lock()
			u.servedWithNone = true
			u.mu.Unlock()
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: " + fmt.Sprintf(
				`{"id":"1","model":"qwen","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`) + "\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","model":"qwen","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	})
}

func (u *qwenUpstream) snapshot() (toolChoices, efforts []string, servedWithNone bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.sawToolChoice...), append([]string(nil), u.sawEffort...), u.servedWithNone
}

// qwenRegistry stands up the fake vLLM upstream as a custom provider.
func qwenRegistry(t *testing.T, upstream *qwenUpstream) *CustomProviderRegistry {
	t.Helper()
	server := httptest.NewServer(upstream.handler())
	t.Cleanup(server.Close)
	registry := NewCustomProviderRegistry(nil)
	if err := registry.Reload([]storage.CustomProvider{{
		ID: "cp_local", Name: "Qwen", Prefix: "local",
		Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
		BaseURL: server.URL, Enabled: true,
		Keys: []storage.CustomProviderKey{{ID: "cpk_1", Enabled: true, Weight: 1, Secret: "sk-local"}},
	}}); err != nil {
		t.Fatal(err)
	}
	return registry
}

// qwenChatRequest is a non-streaming request with tools and auto tool choice,
// the shape Claude Code sends and vLLM refuses without the enable flag.
func qwenChatRequest(model string) translator.OpenAIRequest {
	request := openAIRequestFor(model)
	request.Tools = []translator.OpenAITool{{
		Type: "function", Function: translator.OpenAIFunction{
			Name: "bash", Description: "run a command", Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}}
	request.ToolChoice = "auto"
	return request
}

func TestCustomProviderRecoversFromAutoToolChoiceRejection(t *testing.T) {
	upstream := newQwenUpstream()
	service := New(Options{CustomProviders: qwenRegistry(t, upstream)})

	// Claude Code's default is "auto". The upstream refuses it; the recovery
	// must remember and retry with tool_choice:"none" on the same turn.
	response, err := service.Chat(context.Background(), qwenChatRequest("local/qwen3.8-27B"))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) == 0 || response.Choices[0].Message.Content != "hi" {
		t.Fatalf("response = %#v", response)
	}
	toolChoices, _, servedWithNone := upstream.snapshot()
	if len(toolChoices) < 2 || toolChoices[0] != "auto" || toolChoices[len(toolChoices)-1] != "none" {
		t.Fatalf("tool_choice progression = %v; want auto then none", toolChoices)
	}
	if !servedWithNone {
		t.Fatal("the retry did not carry tool_choice=none")
	}

	// A second turn must be shaped pre-emptively: no refusal, one request.
	before := len(toolChoices)
	if _, err := service.Chat(context.Background(), qwenChatRequest("local/qwen3.8-27B")); err != nil {
		t.Fatal(err)
	}
	toolChoices, _, _ = upstream.snapshot()
	if len(toolChoices) != before+1 || toolChoices[len(toolChoices)-1] != "none" {
		t.Fatalf("second turn = %v; want one request with tool_choice=none", toolChoices[before:])
	}
}

// TestCustomProviderRecoversFromReasoningEffortRejection exercises the real
// user path: a streaming /v1/messages request, translated to chat form with
// reasoning_effort, refused by the upstream, retried with the safe value.
func TestCustomProviderRecoversFromReasoningEffortRejection(t *testing.T) {
	upstream := newQwenUpstream()
	service := New(Options{CustomProviders: qwenRegistry(t, upstream)})
	e := echo.New()
	service.Register(e)

	// Claude Code sends output_config.effort=high by default.
	body := `{"model":"local/qwen3.8-27B","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true,` +
		`"tools":[{"name":"bash","description":"run a command","input_schema":{"type":"object","properties":{}}}],` +
		`"output_config":{"effort":"high"}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "hi") {
		t.Fatalf("recovered stream did not deliver content: %q", recorder.Body.String())
	}
	_, efforts, _ := upstream.snapshot()
	if len(efforts) < 2 || efforts[0] != "high" || efforts[len(efforts)-1] != "low" {
		t.Fatalf("reasoning_effort progression = %v; want high then low", efforts)
	}
}

// TestCustomProviderRecoversFromDefaultEffortRejection covers the empty-effort
// case: the request carries no reasoning_effort, the template defaults it to
// "high", and that is what the upstream rejects. The proxy must learn the
// empty value and retry with the safe one.
func TestCustomProviderRecoversFromDefaultEffortRejection(t *testing.T) {
	upstream := newQwenUpstream()
	upstream.rejectDefaultEffort = true
	service := New(Options{CustomProviders: qwenRegistry(t, upstream)})

	request := qwenChatRequest("local/qwen3.8-27B")
	request.Effort = "" // no effort: the wire carries no reasoning_effort
	response, err := service.Chat(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) == 0 || response.Choices[0].Message.Content != "hi" {
		t.Fatalf("response = %#v", response)
	}
	_, efforts, _ := upstream.snapshot()
	if len(efforts) < 2 || efforts[0] != "" || efforts[len(efforts)-1] != "low" {
		t.Fatalf("reasoning_effort progression = %v; want empty then low", efforts)
	}
}

// TestCustomProviderEffortOffStripsEffort proves the "off" override on a
// custom provider: the wire must carry no reasoning_effort at all, in a single
// request, even though the client asked for "high" (which this upstream
// rejects).
func TestCustomProviderEffortOffStripsEffort(t *testing.T) {
	upstream := newQwenUpstream()
	service := New(Options{CustomProviders: qwenRegistry(t, upstream)})
	service.ReplaceModelEfforts(map[string]string{"local/qwen3.8-27B": "off"})

	request := qwenChatRequest("local/qwen3.8-27B")
	request.Effort = "high" // what the client would send; off must strip it
	response, err := service.Chat(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) == 0 || response.Choices[0].Message.Content != "hi" {
		t.Fatalf("response = %#v", response)
	}
	_, efforts, _ := upstream.snapshot()
	// Two attempts: the first carries tool_choice:"auto" (rejected, recovered),
	// the second is served — both must carry no reasoning_effort. The client
	// asked for "high"; off won over it and over the capability recovery.
	if len(efforts) != 2 || efforts[0] != "" || efforts[1] != "" {
		t.Fatalf("reasoning_effort = %v; want two attempts with no effort", efforts)
	}
}

// TestAutoToolChoiceRecoveryDoesNotLoop keeps a same-candidate retry from
// repeating once the fact is known: the first rejection earns one recovery
// retry, but a second refusal of the already-shaped request must surface the
// error instead of looping.
func TestAutoToolChoiceRecoveryDoesNotLoop(t *testing.T) {
	upstream := newQwenUpstream()
	upstream.rejectAlways = true // refuses every shape
	service := New(Options{CustomProviders: qwenRegistry(t, upstream)})

	if _, err := service.Chat(context.Background(), qwenChatRequest("local/qwen3.8-27B")); err == nil {
		t.Fatal("a genuinely broken upstream must surface its rejection")
	}
	toolChoices, _, _ := upstream.snapshot()
	if len(toolChoices) != 2 {
		t.Fatalf("attempts = %d; want exactly 2 (learn, then surface)", len(toolChoices))
	}
}

func TestStreamingCustomProviderRecoversFromAutoToolChoiceRejection(t *testing.T) {
	upstream := newQwenUpstream()
	service := New(Options{CustomProviders: qwenRegistry(t, upstream)})
	e := echo.New()
	service.Register(e)
	// The Anthropic wire carries tool_choice as an object; auto is what Claude
	// Code asks for and what vLLM refuses without --enable-auto-tool-choice.
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"local/qwen3.8-27B","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true,`+
			`"tools":[{"name":"bash","description":"run a command","input_schema":{"type":"object","properties":{}}}],`+
			`"tool_choice":{"type":"auto"}}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "hi") {
		t.Fatalf("recovered stream did not deliver content: %q", recorder.Body.String())
	}
	toolChoices, _, servedWithNone := upstream.snapshot()
	if len(toolChoices) < 2 || toolChoices[0] != "auto" || toolChoices[len(toolChoices)-1] != "none" {
		t.Fatalf("tool_choice progression = %v; want auto then none", toolChoices)
	}
	if !servedWithNone {
		t.Fatal("the retry did not carry tool_choice=none")
	}
}
