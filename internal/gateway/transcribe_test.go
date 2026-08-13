package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"literouter/internal/provider"
	"literouter/internal/translator"
)

// imageRequest is an Anthropic request carrying one top-level image and one nested
// image inside a tool result — the two shapes transcribe must handle. Model defaults
// to a text-only id so transcription has something to fire on.
func imageRequest() translator.AnthropicRequest {
	return translator.AnthropicRequest{
		Model: "text-model",
		Messages: []translator.AnthropicMessage{
			{Role: "user", Content: []translator.AnthropicContent{
				{Type: "text", Text: "look at this"},
				{Type: "image", Source: &translator.AnthropicSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}},
			}},
			{Role: "user", Content: []translator.AnthropicContent{
				{Type: "tool_result", ToolUseID: "t1", Content: []any{
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "BBBB"}},
				}},
			}},
		},
	}
}

// countImages counts image blocks (top-level + nested) left in a request.
func countImages(request translator.AnthropicRequest) int {
	count := 0
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "image" {
				count++
			}
			for range nestedImageIndexes(block) {
				count++
			}
		}
	}
	return count
}

// visionTestServer is a fake chat/completions endpoint that returns a fixed
// transcription and counts calls per unique image.
func visionTestServer(t *testing.T, calls *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"[transcribed: a diagram showing the flow]"}}]}`)
	}))
}

// visionService builds a Service with a vision client pointed at the test server
// and the image rule wired.
func visionService(t *testing.T, textOnly []string, buildPrompt bool) (*Service, *httptest.Server, *int) {
	t.Helper()
	calls := new(int)
	server := visionTestServer(t, calls)
	client := &staticJSONClient{server: server}
	service := New(Options{
		OpenAI:           client,
		ImageModel:       "vision-model",
		TextOnlyModels:   textOnly,
		BuildImagePrompt: buildPrompt,
	})
	return service, server, calls
}

type staticJSONClient struct {
	server *httptest.Server
}

func (c *staticJSONClient) DoJSON(ctx context.Context, path string, requestBody, responseBody any) error {
	var payload map[string]any
	_ = json.NewDecoder(strings.NewReader(mustJSON(requestBody))).Decode(&payload)
	// Route to the fake server via the same payload; the server ignores it and
	// returns the canned transcription.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server.URL, strings.NewReader(mustJSON(requestBody)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.server.Client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(responseBody)
}

func mustJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestTranscribeImagesReplacesEveryImageWhenToggleOn(t *testing.T) {
	service, server, calls := visionService(t, []string{"text-model"}, true)
	defer server.Close()
	request := imageRequest()
	if before := countImages(request); before != 2 {
		t.Fatalf("setup: want 2 images, got %d", before)
	}
	result, transcribed := service.transcribeImages(context.Background(), request)
	if !transcribed {
		t.Fatal("transcribeImages reported nothing transcribed")
	}
	if got := countImages(result); got != 0 {
		t.Fatalf("images remaining after transcription: %d", got)
	}
	if *calls != 2 {
		t.Fatalf("vision calls = %d, want 2 (one per unique image)", *calls)
	}
	// The top-level image became a text block.
	if result.Messages[0].Content[1].Type != "text" || !strings.Contains(result.Messages[0].Content[1].Text, "[transcribed:") {
		t.Fatalf("top-level image not transcribed: %+v", result.Messages[0].Content[1])
	}
}

func TestTranscribeImagesCachesRepeatedImage(t *testing.T) {
	service, server, calls := visionService(t, []string{"text-model"}, true)
	defer server.Close()
	first := imageRequest()
	second := imageRequest() // same two images again
	_, ok1 := service.transcribeImages(context.Background(), first)
	_, ok2 := service.transcribeImages(context.Background(), second)
	if !ok1 || !ok2 {
		t.Fatal("transcription did not run")
	}
	// First call paid both; second call hits the cache.
	if *calls != 2 {
		t.Fatalf("vision calls = %d, want 2 (cache should serve the second request)", *calls)
	}
}

func TestTranscribeImagesNoopWithoutToggle(t *testing.T) {
	service, server, _ := visionService(t, []string{"text-model"}, false)
	defer server.Close()
	request := imageRequest()
	result, transcribed := service.transcribeImages(context.Background(), request)
	if transcribed {
		t.Fatal("transcription ran with the toggle off")
	}
	if countImages(result) != 2 {
		t.Fatal("request was modified with the toggle off")
	}
}

func TestTranscribeImagesNoopForVisionModel(t *testing.T) {
	// A vision-capable serving model: even with the toggle on, nothing to transcribe.
	// textOnly declares "text-model" but the serving model here is "vision-model",
	// which is not in the text-only list.
	service, server, _ := visionService(t, []string{"text-model"}, true)
	defer server.Close()
	request := imageRequest()
	request.Model = "vision-model"
	result, transcribed := service.transcribeImages(context.Background(), request)
	if transcribed || countImages(result) != 2 {
		t.Fatal("transcription ran for a vision-capable model")
	}
}

// A vision call that fails must still replace the image with a placeholder — a
// text-only model handed the raw image cannot read it, and leaving it in is worse
// than the degraded-but-honest placeholder.
func TestTranscribeImagesFallsBackToPlaceholderOnVisionFailure(t *testing.T) {
	// A fake client that always errors.
	failing := &failingJSONClient{}
	service := New(Options{
		OpenAI: failing, ImageModel: "vision-model", TextOnlyModels: []string{"text-model"},
		BuildImagePrompt: true,
	})
	request := imageRequest()
	result, transcribed := service.transcribeImages(context.Background(), request)
	if !transcribed {
		t.Fatal("transcription should still report progress (placeholders count)")
	}
	// Every image is gone; the top-level one became the placeholder text.
	if got := countImages(result); got != 0 {
		t.Fatalf("images remaining after failed transcription: %d", got)
	}
	if result.Messages[0].Content[1].Type != "text" || !strings.Contains(result.Messages[0].Content[1].Text, "an image was here") {
		t.Fatalf("top-level image not replaced with placeholder: %+v", result.Messages[0].Content[1])
	}
}

type failingJSONClient struct{}

func (c *failingJSONClient) DoJSON(ctx context.Context, path string, requestBody, responseBody any) error {
	return fmt.Errorf("vision model unavailable")
}

func TestTranscribeImagesFallsBackWithoutVisionModel(t *testing.T) {
	service, server, _ := visionService(t, []string{"text-model"}, true)
	defer server.Close()
	// Clear the image model — no vision model to transcribe with.
	service.SetImageRoute("", []string{"text-model"})
	request := imageRequest()
	result, transcribed := service.transcribeImages(context.Background(), request)
	if transcribed {
		t.Fatal("transcription reported success without a vision model")
	}
	if countImages(result) != 2 {
		t.Fatal("request was modified without a vision model")
	}
}

// TestRouteModelPrefersTranscriptionWhenToggleOn pins the routing decision: with the
// toggle on and a text-only serving model, the turn stays on that model and flags
// transcription instead of rerouting to the vision model or stripping.
func TestRouteModelPrefersTranscriptionWhenToggleOn(t *testing.T) {
	service := New(Options{
		ImageModel: "vision-model", TextOnlyModels: []string{"text-model"},
		BuildImagePrompt: true,
	})
	request := imageRequest()
	request.Model = "text-model"
	decision, err := service.routeModel(context.Background(), request, len(mustJSON(request)))
	if err != nil {
		t.Fatalf("routeModel() = %v", err)
	}
	if !decision.TranscribeImages {
		t.Fatalf("decision = %+v, want TranscribeImages=true", decision)
	}
	if decision.Model != "" {
		t.Fatalf("decision.Model = %q, want empty (stay on text-only serving model)", decision.Model)
	}
	if decision.StripImages {
		t.Fatal("decision strips instead of transcribing")
	}
}

func TestRouteModelKeepsRerouteWhenToggleOff(t *testing.T) {
	service := New(Options{
		ImageModel: "vision-model", TextOnlyModels: []string{"text-model"},
		BuildImagePrompt: false,
	})
	request := imageRequest()
	request.Model = "text-model"
	decision, err := service.routeModel(context.Background(), request, len(mustJSON(request)))
	if err != nil {
		t.Fatalf("routeModel() = %v", err)
	}
	if decision.TranscribeImages {
		t.Fatal("decision transcribes with the toggle off")
	}
	if decision.Model != "vision-model" {
		t.Fatalf("decision.Model = %q, want vision-model (reroute)", decision.Model)
	}
}

// A request asked-for as a vision-capable model can fall back to a text-only model
// (fallback_model appended to the chain). The routing decision was made on the
// asked-for model, so the image was left in place — the fallback candidate must strip
// it per-candidate or it carries an image_url a text-only model cannot read. This is
// the exact shape of the compaction 400: a claude-* id falling back to opencode.
func TestCompleteStripsImagesForTextOnlyFallbackCandidate(t *testing.T) {
	service := New(Options{
		ImageModel: "vision-model", TextOnlyModels: []string{"opencode/deepseek-v4-flash"},
		FallbackModel: "opencode/deepseek-v4-flash",
	})
	// A vision-capable asked-for model: no text-only route fires on it, image stays.
	request := provider.Request{
		Model:     "vision-model",
		MaxTokens: 100,
		Messages: []provider.Message{{
			Role: "user",
			Content: []provider.Content{
				{Type: "text", Text: "describe"},
				{Type: "image", MediaType: "image/png", Data: "iVBOR"},
			},
		}},
	}
	// Walk the chain the way complete() does; the fallback (text-only) candidate must
	// have its image stripped before ToOpenAIRequest.
	chain := service.modelChain("vision-model")
	found := false
	for _, model := range chain {
		if model != "opencode/deepseek-v4-flash" {
			continue
		}
		found = true
		prepared := cloneProviderRequest(request)
		prepared.Model = model
		if route := service.imageRoute.Load(); route != nil && route.isTextOnly(model) {
			service.stripProviderImages(&prepared)
		}
		for _, message := range prepared.Messages {
			for _, block := range message.Content {
				if block.Type == "image" {
					t.Fatal("text-only fallback candidate still carries an image")
				}
				if block.Type == "text" && strings.Contains(block.Text, "an image was here") {
					// good — stripped to placeholder
				}
			}
		}
	}
	if !found {
		t.Fatal("chain did not include the text-only fallback")
	}
}

func TestSetBuildImagePromptRoundTrip(t *testing.T) {
	service := New(Options{})
	if service.BuildImagePrompt() {
		t.Fatal("default toggle should be off")
	}
	service.SetBuildImagePrompt(true)
	if !service.BuildImagePrompt() {
		t.Fatal("toggle not set")
	}
	service.SetBuildImagePrompt(false)
	if service.BuildImagePrompt() {
		t.Fatal("toggle not cleared")
	}
}
