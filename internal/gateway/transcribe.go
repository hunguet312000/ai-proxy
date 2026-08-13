package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"

	"literouter/internal/provider"
	"literouter/internal/translator"
)

// transcriptionCache remembers, per image content hash, the text a vision model
// produced for it. Re-sending the same screenshot on the next turn (or the same
// image nested in a tool result again) must not re-pay a vision call: the whole
// point of transcription is that the image becomes cheap text, and paying a model
// to look at it twice defeats it. Bounded LRU; a turn never needs more than a few
// images and a session's screenshots are far below the cap.
type transcriptionCache struct {
	mu      sync.Mutex
	max     int
	entries map[string]string
	order   []string
}

func newTranscriptionCache(max int) *transcriptionCache {
	if max <= 0 {
		max = 512
	}
	return &transcriptionCache{max: max, entries: make(map[string]string)}
}

func (c *transcriptionCache) get(hash string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	text, ok := c.entries[hash]
	if !ok {
		return "", false
	}
	// Refresh LRU: a cache hit moves the entry to the front so a screenshot that
	// keeps reappearing is not evicted between turns.
	for index, key := range c.order {
		if key == hash {
			c.order = append(c.order[:index], c.order[index+1:]...)
			break
		}
	}
	c.order = append(c.order, hash)
	return text, true
}

func (c *transcriptionCache) put(hash, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[hash]; ok {
		return
	}
	c.entries[hash] = text
	c.order = append(c.order, hash)
	for len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

// transcriptionInstruction tells the vision model what to produce: a faithful
// text rendering of the image the text-only task model can act on. The output
// replaces the image in the prompt, so it must carry the picture's information,
// not describe the picture.
const transcriptionInstruction = "Describe everything visible in this image as dense text for a text-only model that " +
	"will act on it. Include every label, path, error message, diagram label, number, and layout detail " +
	"you can read. Do not comment on the image; transcribe its content."

// transcribeImages replaces every image in request with a text block produced by
// the vision model (ImageModel), so a text-only serving model can read the picture.
//
// It runs only when the build-image-prompt toggle is on and the serving model is
// declared text-only. Images already transcribed (content-hash cache hit) are
// replaced from the cache without a vision call. A vision call failure substitutes
// the image placeholder rather than leaving the image for a model that cannot read
// it — a failed transcription must never cost the user the request, but it must not
// hand the text-only model an unreadable image either.
func (s *Service) transcribeImages(ctx context.Context, request translator.AnthropicRequest) (translator.AnthropicRequest, bool) {
	route := s.imageRoute.Load()
	if !s.BuildImagePrompt() || route == nil || route.model == "" {
		// Toggle off, or no vision model to transcribe with. Leave the request untouched;
		// the caller falls back to its strip/refuse behavior.
		return request, false
	}
	if !route.isTextOnly(request.Model) {
		// The serving model reads images itself; no transcription needed.
		return request, false
	}
	cache := s.transcriptions()
	// System blocks can carry an image too; transcribe those as well.
	system, systemTranscribed, err := s.transcribeSystem(ctx, route.model, request.System, cache)
	if err != nil {
		return request, false
	}
	request.System = system
	transcribed := systemTranscribed
	messages := make([]translator.AnthropicMessage, len(request.Messages))
	copy(messages, request.Messages)
	for messageIndex, message := range messages {
		var content []translator.AnthropicContent
		ensure := func() {
			if content == nil {
				content = make([]translator.AnthropicContent, len(message.Content))
				copy(content, message.Content)
			}
		}
		for blockIndex, block := range message.Content {
			if block.Type == "image" {
				text, err := s.transcribeOne(ctx, route.model, block, cache)
				if err != nil {
					// Transcription failed; a placeholder is strictly better than leaving the
					// image for a text-only model that cannot read it.
					ensure()
					content[blockIndex] = translator.AnthropicContent{Type: "text", Text: imagePlaceholder}
					transcribed++
					continue
				}
				ensure()
				content[blockIndex] = translator.AnthropicContent{Type: "text", Text: text}
				transcribed++
				continue
			}
			indexes := nestedImageIndexes(block)
			if len(indexes) == 0 {
				continue
			}
			ensure()
			nested, _ := block.Content.([]any)
			replaced := make([]any, len(nested))
			copy(replaced, nested)
			changed := false
			for _, at := range indexes {
				entry, _ := replaced[at].(map[string]any)
				text, err := s.transcribeNested(ctx, route.model, entry, cache)
				if err != nil {
					replaced[at] = map[string]any{"type": "text", "text": imagePlaceholder}
					changed = true
					transcribed++
					continue
				}
				replaced[at] = map[string]any{"type": "text", "text": text}
				changed = true
				transcribed++
			}
			if changed {
				block.Content = replaced
				content[blockIndex] = block
			}
		}
		if content != nil {
			messages[messageIndex].Content = content
		}
	}
	if transcribed == 0 {
		return request, false
	}
	request.Messages = messages
	return request, true
}

// transcriptions returns the gateway's transcription cache, created lazily.
func (s *Service) transcriptions() *transcriptionCache {
	s.transcriptionMu.Lock()
	defer s.transcriptionMu.Unlock()
	if s.transcriptionCache == nil {
		s.transcriptionCache = newTranscriptionCache(0)
	}
	return s.transcriptionCache
}

// transcribeOne transcribes a top-level image block, using the cache when possible.
func (s *Service) transcribeOne(ctx context.Context, visionModel string, block translator.AnthropicContent, cache *transcriptionCache) (string, error) {
	hash := imageHash(block.Source)
	if text, ok := cache.get(hash); ok {
		return text, nil
	}
	text, err := s.callVisionModel(ctx, visionModel, block)
	if err != nil {
		slog.Warn("image transcription failed; leaving the image in place", "error", err)
		return "", err
	}
	cache.put(hash, text)
	return text, nil
}

// transcribeNested transcribes an image entry nested inside a tool result.
func (s *Service) transcribeNested(ctx context.Context, visionModel string, entry map[string]any, cache *transcriptionCache) (string, error) {
	block := translator.AnthropicContent{Type: "image", Source: anthropicSourceFromNested(entry)}
	hash := imageHash(block.Source)
	if text, ok := cache.get(hash); ok {
		return text, nil
	}
	text, err := s.callVisionModel(ctx, visionModel, block)
	if err != nil {
		slog.Warn("image transcription failed; leaving the image in place", "error", err)
		return "", err
	}
	cache.put(hash, text)
	return text, nil
}

// transcribeSystem transcribes any image blocks in the system preamble, mirroring
// the message pass. Returns the new system and how many images it transcribed.
func (s *Service) transcribeSystem(ctx context.Context, visionModel string, system []translator.AnthropicContent, cache *transcriptionCache) ([]translator.AnthropicContent, int, error) {
	changed := false
	cleaned := make([]translator.AnthropicContent, len(system))
	copy(cleaned, system)
	transcribed := 0
	for index, block := range system {
		if block.Type != "image" {
			continue
		}
		text, err := s.transcribeOne(ctx, visionModel, block, cache)
		if err != nil {
			cleaned[index] = translator.AnthropicContent{Type: "text", Text: imagePlaceholder}
			transcribed++
			changed = true
			continue
		}
		cleaned[index] = translator.AnthropicContent{Type: "text", Text: text}
		transcribed++
		changed = true
	}
	if !changed {
		return system, 0, nil
	}
	return cleaned, transcribed, nil
}

// callVisionModel sends one image to the vision model and returns its text
// transcription, reusing the same model-call machinery the summarizer uses
// (oauthInference.DoJSON or clientForModel + ToOpenAIRequest).
func (s *Service) callVisionModel(ctx context.Context, visionModel string, image translator.AnthropicContent) (string, error) {
	request := provider.Request{
		Model:     visionModel,
		MaxTokens: 1024,
		Messages: []provider.Message{{
			Role: "user",
			Content: []provider.Content{
				{Type: "text", Text: transcriptionInstruction},
				imageToProvider(image),
			},
		}},
	}
	upstreamRequest, err := translator.ToOpenAIRequest(request)
	if err != nil {
		return "", err
	}
	var lastErr error
	for _, candidate := range s.modelChain(visionModel) {
		upstreamRequest.Model = candidate
		var response translator.OpenAIResponse
		// Mirror the real turn's routing order (service.go complete()): a custom provider
		// owns this model outright, otherwise OAuth, otherwise the API-key client.
		custom, isCustom := s.resolveCustomProvider(candidate)
		if isCustom {
			path, pathErr := customUpstreamPath(custom.APIType)
			if pathErr != nil {
				lastErr = pathErr
				continue
			}
			upstreamRequest.Model = custom.Model
			if err = custom.Client.DoJSON(ctx, path, upstreamRequest, &response); err == nil {
				s.touchCustomProviderKey(custom.KeyID)
				return summaryResponseText(response)
			}
			lastErr = err
			if retryableProviderError(err) {
				continue
			}
			return "", err
		}
		if s.oauthInference != nil {
			response, err = s.oauthInference.DoJSON(ctx, upstreamRequest, conversationID(ctx))
			if err == nil {
				return summaryResponseText(response)
			}
			lastErr = err
		}
		upstream := s.clientForModel(candidate)
		if upstream == nil {
			continue
		}
		upstreamRequest.Model = upstreamModel(candidate)
		if err = upstream.DoJSON(ctx, "/chat/completions", upstreamRequest, &response); err != nil {
			lastErr = err
			if retryableProviderError(err) {
				continue
			}
			return "", err
		}
		return summaryResponseText(response)
	}
	if lastErr != nil {
		return "", lastErr
	}
	return "", ErrProviderUnavailable
}

// imageToProvider converts an Anthropic image block into the unified provider form.
func imageToProvider(block translator.AnthropicContent) provider.Content {
	if block.Source == nil {
		return provider.Content{Type: "image"}
	}
	return provider.Content{
		Type:      "image",
		MediaType: block.Source.MediaType,
		Data:      block.Source.Data,
		URL:       block.Source.URL,
	}
}

// anthropicSourceFromNested reads the source of an image entry nested inside a
// tool result's content (a map with "type":"image" and a "source" object).
func anthropicSourceFromNested(entry map[string]any) *translator.AnthropicSource {
	source, _ := entry["source"].(map[string]any)
	if source == nil {
		return nil
	}
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	url, _ := source["url"].(string)
	return &translator.AnthropicSource{Type: "base64", MediaType: mediaType, Data: data, URL: url}
}

// imageHash is the cache key: the content of the image regardless of which turn
// carries it. Empty for a source-less image (which transcription cannot read).
func imageHash(source *translator.AnthropicSource) string {
	if source == nil {
		return ""
	}
	// A source-less image gets a constant hash only so the cache never collides two
	// real images; transcription itself fails before the cache is consulted.
	sum := sha256.Sum256([]byte(source.Data + "\x00" + source.MediaType + "\x00" + source.URL))
	return hex.EncodeToString(sum[:])
}
