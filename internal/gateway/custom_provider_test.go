package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/storage"
	"literouter/internal/translator"
)

// fakeUpstream stands in for a user-registered provider and records exactly what
// arrived, which is the only way to prove the prefix was stripped and the key sent.
type fakeUpstream struct {
	mu     sync.Mutex
	paths  []string
	models []string
	auths  []string
	stream bool
	server *httptest.Server
}

func newFakeUpstream(t *testing.T, stream bool) *fakeUpstream {
	t.Helper()
	upstream := &fakeUpstream{stream: stream}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string)
		upstream.mu.Lock()
		upstream.paths = append(upstream.paths, r.URL.Path)
		upstream.models = append(upstream.models, model)
		upstream.auths = append(upstream.auths, r.Header.Get("Authorization"))
		upstream.mu.Unlock()
		if upstream.stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"id":"1","model":"` + model + `","choices":[{"index":0,"delta":{"content":"hi from custom"},"finish_reason":"stop"}]}` +
				"\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","model":"` + model + `","choices":[{"index":0,"message":{"role":"assistant","content":"hi from custom"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *fakeUpstream) snapshot() ([]string, []string, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...), append([]string(nil), u.models...), append([]string(nil), u.auths...)
}

func registryFor(t *testing.T, baseURL string, keys ...string) *CustomProviderRegistry {
	t.Helper()
	definition := storage.CustomProvider{
		ID: "cp_1", Name: "AI Telegram", Prefix: "ai-tele",
		Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
		BaseURL: baseURL, Enabled: true,
	}
	for i, secret := range keys {
		definition.Keys = append(definition.Keys, storage.CustomProviderKey{
			ID: "cpk_" + string(rune('a'+i)), Enabled: true, Weight: 1, Secret: secret,
		})
	}
	registry := NewCustomProviderRegistry(nil)
	if err := registry.Reload([]storage.CustomProvider{definition}); err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestCustomProviderServesStreamingAndStripsPrefix(t *testing.T) {
	upstream := newFakeUpstream(t, true)
	var touched []string
	service := New(Options{
		CustomProviders: registryFor(t, upstream.server.URL, "sk-custom-1"),
		TouchCustomKey:  func(id string) { touched = append(touched, id) },
	})
	e := echo.New()
	service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"ai-tele/gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "hi from custom") {
		t.Fatalf("custom provider response not delivered: %q", recorder.Body.String())
	}
	paths, models, auths := upstream.snapshot()
	if len(paths) != 1 || paths[0] != "/chat/completions" {
		t.Fatalf("upstream paths = %v", paths)
	}
	// The prefix is LiteRouter addressing and must not leak upstream.
	if models[0] != "gpt-5.6-sol" {
		t.Fatalf("upstream model = %q, want the prefix stripped", models[0])
	}
	if auths[0] != "Bearer sk-custom-1" {
		t.Fatalf("upstream authorization = %q", auths[0])
	}
	if len(touched) != 1 {
		t.Fatalf("key usage not recorded: %v", touched)
	}
}

func TestCustomProviderServesNonStreaming(t *testing.T) {
	upstream := newFakeUpstream(t, false)
	service := New(Options{CustomProviders: registryFor(t, upstream.server.URL, "sk-custom-1")})
	response, err := service.Chat(context.Background(), openAIRequestFor("ai-tele/gpt-5.6-luna"))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) == 0 || response.Choices[0].Message.Content != "hi from custom" {
		t.Fatalf("response = %#v", response)
	}
	paths, models, _ := upstream.snapshot()
	if paths[0] != "/chat/completions" || models[0] != "gpt-5.6-luna" {
		t.Fatalf("paths=%v models=%v", paths, models)
	}
}

func TestCustomProviderRotatesKeys(t *testing.T) {
	upstream := newFakeUpstream(t, false)
	service := New(Options{CustomProviders: registryFor(t, upstream.server.URL, "sk-a", "sk-b")})
	for range 4 {
		if _, err := service.Chat(context.Background(), openAIRequestFor("ai-tele/m")); err != nil {
			t.Fatal(err)
		}
	}
	_, _, auths := upstream.snapshot()
	seen := map[string]int{}
	for _, auth := range auths {
		seen[auth]++
	}
	if len(seen) != 2 || seen["Bearer sk-a"] != 2 || seen["Bearer sk-b"] != 2 {
		t.Fatalf("keys were not rotated evenly: %v", seen)
	}
}

func TestCustomProviderDoesNotHijackBuiltInModels(t *testing.T) {
	upstream := newFakeUpstream(t, false)
	client := &fakeClient{}
	service := New(Options{
		OpenAI:          client,
		CustomProviders: registryFor(t, upstream.server.URL, "sk-a"),
	})
	if _, err := service.Chat(context.Background(), openAIRequestFor("gpt-4.1")); err != nil {
		t.Fatal(err)
	}
	if paths, _, _ := upstream.snapshot(); len(paths) != 0 {
		t.Fatalf("a built-in model was sent to the custom provider: %v", paths)
	}
	if client.calls != 1 {
		t.Fatalf("built-in client calls = %d", client.calls)
	}
}

func TestCustomProviderLongestPrefixWins(t *testing.T) {
	registry := NewCustomProviderRegistry(nil)
	if err := registry.Reload([]storage.CustomProvider{
		{ID: "a", Prefix: "acme", Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
			BaseURL: "https://a.example.com/v1", Enabled: true,
			Keys: []storage.CustomProviderKey{{ID: "k1", Enabled: true, Secret: "s"}}},
		{ID: "b", Prefix: "acme.eu", Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
			BaseURL: "https://b.example.com/v1", Enabled: true,
			Keys: []storage.CustomProviderKey{{ID: "k2", Enabled: true, Secret: "s"}}},
	}); err != nil {
		t.Fatal(err)
	}
	target, ok := registry.Resolve("acme.eu/model-x")
	if !ok || target.ProviderID != "b" || target.Model != "model-x" {
		t.Fatalf("target = %#v ok=%v", target, ok)
	}
	if target, ok := registry.Resolve("acme/model-y"); !ok || target.ProviderID != "a" || target.Model != "model-y" {
		t.Fatalf("target = %#v ok=%v", target, ok)
	}
	if _, ok := registry.Resolve("acmex/model"); ok {
		t.Fatal("a prefix must only match on a full segment boundary")
	}
}

func TestCustomProviderUnsupportedAPITypeIsRejected(t *testing.T) {
	// Messages and Responses upstreams are stored but the gateway pipeline does not
	// translate to them yet; failing loudly beats sending a chat body to /messages.
	if _, err := customUpstreamPath("chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := customUpstreamPath("nonsense"); err == nil {
		t.Fatal("an unknown api type must be rejected")
	}
}

func openAIRequestFor(model string) translator.OpenAIRequest {
	return translator.OpenAIRequest{
		Model:    model,
		Messages: []translator.OpenAIMessage{{Role: "user", Content: "hi"}},
	}
}

func TestCustomProviderUsageIsAttributedToItsPrefix(t *testing.T) {
	// The upstream echoes back its own model name with the prefix already stripped,
	// so attributing on that reported traffic to the built-in fallback instead.
	upstream := newFakeUpstream(t, false)
	events := make(chan UsageEvent, 4)
	service := New(Options{
		CustomProviders: registryFor(t, upstream.server.URL, "sk-a"),
		OnUsage:         func(event UsageEvent) { events <- event },
	})
	if _, err := service.Chat(context.Background(), openAIRequestFor("ai-tele/gpt-5.6-sol")); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Provider != "custom:ai-tele" {
			t.Fatalf("usage provider = %q, want custom:ai-tele", event.Provider)
		}
	case <-time.After(time.Second):
		t.Fatal("no usage event recorded")
	}
}

func TestCustomProviderLookupDoesNotConsumeRotation(t *testing.T) {
	// Regression guard: attribution used to call Resolve, which advances the key
	// rotation, so every request skipped a key and traffic pinned to one credential.
	registry := registryFor(t, "https://x.example.com/v1", "sk-a", "sk-b")
	for range 10 {
		if _, ok := registry.PrefixFor("ai-tele/model"); !ok {
			t.Fatal("prefix lookup failed")
		}
	}
	first, _ := registry.Resolve("ai-tele/model")
	second, _ := registry.Resolve("ai-tele/model")
	if first.KeyID == second.KeyID {
		t.Fatalf("rotation did not advance across two Resolve calls: %q", first.KeyID)
	}
	if _, ok := registry.PrefixFor("other/model"); ok {
		t.Fatal("an unclaimed prefix was matched")
	}
}
