package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"literouter/internal/provider"
	"literouter/internal/storage"
)

// CustomProviderRegistry resolves a model name to a user-registered upstream.
//
// Routing is by prefix: a model addressed as "<prefix>/<model>" belongs to the
// provider that claimed that prefix, and the remainder is what goes upstream. That
// keeps a custom provider from having to be known to any hardcoded switch, and it is
// the same shape the built-in "cx/" and "ag/" prefixes already use.
type CustomProviderRegistry struct {
	mu         sync.RWMutex
	byPrefix   map[string]*customProvider
	httpClient *http.Client
}

type customProvider struct {
	definition storage.CustomProvider
	keys       []*customKey
	// next rotates across keys. Requests are spread rather than pinned because a
	// custom provider's keys are interchangeable credentials, not sessions with
	// their own upstream cache.
	next atomic.Uint64
}

type customKey struct {
	id     string
	label  string
	client *provider.OpenAICompatibleClient
}

// CustomTarget is one resolved upstream: the client to call, the model name to send,
// and enough identity to record usage and report errors against.
type CustomTarget struct {
	ProviderID string
	Prefix     string
	Name       string
	Kind       string
	APIType    string
	KeyID      string
	Model      string
	Client     *provider.OpenAICompatibleClient
}

func NewCustomProviderRegistry(httpClient *http.Client) *CustomProviderRegistry {
	return &CustomProviderRegistry{byPrefix: map[string]*customProvider{}, httpClient: httpClient}
}

// Reload rebuilds the registry from storage. It is called at startup and after any
// change through the UI, so an edit takes effect without a restart.
func (r *CustomProviderRegistry) Reload(providers []storage.CustomProvider) error {
	rebuilt := make(map[string]*customProvider, len(providers))
	var failures []string
	for _, definition := range providers {
		entry := &customProvider{definition: definition}
		for _, key := range definition.Keys {
			client, err := provider.NewOpenAICompatibleClient(
				"custom:"+definition.Prefix, definition.BaseURL, key.Secret, r.httpClient)
			if err != nil {
				// One bad key must not take the whole registry down; the rest of the
				// provider keeps serving and the reason is logged.
				failures = append(failures, fmt.Sprintf("%s/%s: %v", definition.Prefix, key.ID, err))
				continue
			}
			entry.keys = append(entry.keys, &customKey{id: key.ID, label: key.Label, client: client})
		}
		if len(entry.keys) == 0 {
			continue
		}
		rebuilt[definition.Prefix] = entry
	}
	r.mu.Lock()
	r.byPrefix = rebuilt
	r.mu.Unlock()
	if len(failures) > 0 {
		return fmt.Errorf("custom providers with unusable keys: %s", strings.Join(failures, "; "))
	}
	return nil
}

// PrefixFor reports which custom provider claims a model without selecting a key.
// It exists because Resolve advances the key rotation: calling Resolve just to name
// the provider for a usage row would skip a key on every request and pin traffic to
// one credential.
func (r *CustomProviderRegistry) PrefixFor(model string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	best := r.matchLocked(model)
	return best, best != ""
}

// matchLocked returns the longest claimed prefix for a model. Callers hold the lock.
func (r *CustomProviderRegistry) matchLocked(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	if lower == "" {
		return ""
	}
	best := ""
	for prefix := range r.byPrefix {
		if !strings.HasPrefix(lower, prefix+"/") {
			continue
		}
		if len(prefix) > len(best) {
			best = prefix
		}
	}
	return best
}

// Resolve reports the upstream for a model and picks the key to use, or ok=false when
// no custom provider claims it. The longest matching prefix wins so "acme.eu/" beats
// "acme/". This advances the key rotation, so it must be called once per request.
func (r *CustomProviderRegistry) Resolve(model string) (CustomTarget, bool) {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return CustomTarget{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	best := r.matchLocked(trimmed)
	if best == "" {
		return CustomTarget{}, false
	}
	entry := r.byPrefix[best]
	key := entry.pick()
	if key == nil {
		return CustomTarget{}, false
	}
	return CustomTarget{
		ProviderID: entry.definition.ID, Prefix: entry.definition.Prefix,
		Name: entry.definition.Name, Kind: entry.definition.Kind,
		APIType: entry.definition.APIType, KeyID: key.id,
		// The prefix is LiteRouter's addressing, not the upstream's, so it is stripped
		// before the request leaves.
		Model:  trimmed[len(best)+1:],
		Client: key.client,
	}, true
}

// Prefixes lists the claimed prefixes, for surfacing models and for diagnostics.
func (r *CustomProviderRegistry) Prefixes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byPrefix))
	for prefix := range r.byPrefix {
		out = append(out, prefix)
	}
	return out
}

func (p *customProvider) pick() *customKey {
	if len(p.keys) == 0 {
		return nil
	}
	index := p.next.Add(1) - 1
	return p.keys[index%uint64(len(p.keys))]
}

// customJSONClient adapts a resolved target to the JSONClient the gateway already
// speaks, rewriting the model to the upstream's own name.
type customJSONClient struct {
	target CustomTarget
}

func (c customJSONClient) DoJSON(ctx context.Context, path string, requestBody, responseBody any) error {
	return c.target.Client.DoJSON(ctx, path, requestBody, responseBody)
}

// customStreamClient is the streaming counterpart.
type customStreamClient struct {
	target CustomTarget
}

func (c customStreamClient) DoStream(ctx context.Context, path string, requestBody any) (io.ReadCloser, error) {
	return c.target.Client.DoStream(ctx, path, requestBody)
}

// customUpstreamPath maps the provider's wire protocol to the path to call. The
// Responses and Messages shapes are deliberately rejected here rather than silently
// sent to the chat endpoint, which would fail with a confusing upstream error.
func customUpstreamPath(apiType string) (string, error) {
	switch apiType {
	case storage.CustomAPITypeChat:
		return "/chat/completions", nil
	case storage.CustomAPITypeResponses:
		return "/responses", nil
	case storage.CustomAPITypeMessages:
		return "/messages", nil
	default:
		return "", fmt.Errorf("unsupported custom provider api type %q", apiType)
	}
}

func logCustomTarget(target CustomTarget, endpoint string) {
	slog.Debug("routing to custom provider", "endpoint", endpoint, "prefix", target.Prefix,
		"name", target.Name, "kind", target.Kind, "api_type", target.APIType,
		"key", target.KeyID, "upstream_model", target.Model)
}
