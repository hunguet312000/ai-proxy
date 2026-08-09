package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"literouter/internal/pool"
	"literouter/internal/storage"
)

func newCustomProviderUI(t *testing.T, hooks CustomProviderHooks) (*echo.Echo, *Service) {
	t.Helper()
	service, err := New(pool.New(nil), "token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	service.SetCustomProviderHooks(hooks)
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	return e, service
}

func htmxPost(target string, form url.Values) *http.Request {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	request.Header.Set("Origin", "http://127.0.0.1:8317")
	request.Host = "127.0.0.1:8317"
	request.Header.Set("HX-Request", "true")
	return request
}

func TestCustomProviderPageRendersWithoutKeyMaterial(t *testing.T) {
	stored := []storage.CustomProvider{{
		ID: "cp_1", Name: "AI Telegram", Prefix: "ai-tele", Kind: storage.CustomKindOpenAI,
		APIType: storage.CustomAPITypeChat, BaseURL: "https://apikey.click/v1", Enabled: true,
		Keys: []storage.CustomProviderKey{
			{ID: "cpk_1", ProviderID: "cp_1", Label: "primary", Enabled: true, Weight: 1},
			{ID: "cpk_2", ProviderID: "cp_1", Label: "backup", Enabled: false, Weight: 1},
		},
	}}
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return stored, nil },
	})
	request := httptest.NewRequest(http.MethodGet, "/ui/providers/custom", nil)
	request.Header.Set("HX-Request", "true")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{"AI Telegram", "ai-tele/", "https://apikey.click/v1",
		"OpenAI · Chat", "primary", "backup", "Add key"} {
		if !strings.Contains(body, want) {
			t.Fatalf("fragment missing %q: %s", want, body)
		}
	}
	// A rendered page must never be able to leak a credential.
	if strings.Contains(body, "secret") || strings.Contains(strings.ToLower(body), "sk-") {
		t.Fatalf("fragment appears to contain key material: %s", body)
	}
	if !strings.Contains(body, `type="password"`) {
		t.Fatal("the key input is not masked")
	}
}

func TestCustomProviderPageShowsEmptyState(t *testing.T) {
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return nil, nil },
	})
	request := httptest.NewRequest(http.MethodGet, "/ui/providers/custom", nil)
	request.Header.Set("HX-Request", "true")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), "No custom providers") {
		t.Fatalf("empty state missing: %s", recorder.Body.String())
	}
}

func TestCustomProviderCreateFromFormRerendersList(t *testing.T) {
	var created storage.CustomProvider
	var addedKey string
	var reloaded int
	stored := []storage.CustomProvider{}
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return stored, nil },
		Create: func(_ context.Context, provider storage.CustomProvider) (storage.CustomProvider, error) {
			provider.ID = "cp_new"
			created = provider
			stored = append(stored, provider)
			return provider, nil
		},
		AddKey: func(_ context.Context, providerID, label, apiKey string) (storage.CustomProviderKey, error) {
			addedKey = apiKey
			return storage.CustomProviderKey{ID: "cpk_new", ProviderID: providerID, Label: label, Enabled: true}, nil
		},
		Reload: func() { reloaded++ },
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, htmxPost("/ui/providers/custom", url.Values{
		"name": {"Acme"}, "prefix": {"acme"}, "kind": {"openai"}, "api_type": {"chat"},
		"base_url": {"https://acme.example.com/v1"}, "api_key": {"sk-live-value"},
	}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if created.Prefix != "acme" || created.BaseURL != "https://acme.example.com/v1" {
		t.Fatalf("created = %#v", created)
	}
	if addedKey != "sk-live-value" {
		t.Fatalf("key not forwarded to storage: %q", addedKey)
	}
	if reloaded != 1 {
		t.Fatalf("registry reloads = %d, want 1", reloaded)
	}
	// htmx swaps the response in, so it has to be the list again.
	if !strings.Contains(recorder.Body.String(), "Acme") {
		t.Fatalf("response is not the refreshed list: %s", recorder.Body.String())
	}
	// The submitted key must not come back in the HTML.
	if strings.Contains(recorder.Body.String(), "sk-live-value") {
		t.Fatal("the submitted key was echoed into the page")
	}
}

func TestCustomProviderFailureKeepsListInPage(t *testing.T) {
	// htmx replaces #custom-providers with whatever comes back. A bare error body
	// would wipe the list off the page, so the message rides along with it.
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Prefix: "acme", Kind: "openai",
				APIType: "chat", BaseURL: "https://acme.example.com/v1", Enabled: true}}, nil
		},
		Create: func(context.Context, storage.CustomProvider) (storage.CustomProvider, error) {
			return storage.CustomProvider{}, storage.ErrCustomPrefixTaken
		},
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, htmxPost("/ui/providers/custom", url.Values{
		"prefix": {"acme"}, "kind": {"openai"}, "base_url": {"https://acme.example.com/v1"},
	}))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "already used") {
		t.Fatalf("error message missing: %s", body)
	}
	if !strings.Contains(body, "acme/") {
		t.Fatalf("the existing list was dropped from the page: %s", body)
	}
}

func TestCustomProviderMutationsRequireSameOrigin(t *testing.T) {
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return nil, nil },
		Create: func(context.Context, storage.CustomProvider) (storage.CustomProvider, error) {
			t.Fatal("a cross-origin request reached storage")
			return storage.CustomProvider{}, nil
		},
	})
	request := httptest.NewRequest(http.MethodPost, "/ui/providers/custom",
		strings.NewReader("prefix=x&kind=openai&base_url=https://x.example.com/v1"))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	request.Header.Set("Origin", "https://evil.example.com")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
}

func TestCustomProviderJSONCallersStillGetJSON(t *testing.T) {
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Prefix: "acme"}}, nil
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/ui/providers/custom", nil)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if !strings.HasPrefix(recorder.Header().Get(echo.HeaderContentType), echo.MIMEApplicationJSON) {
		t.Fatalf("content type = %q", recorder.Header().Get(echo.HeaderContentType))
	}
	if !strings.Contains(recorder.Body.String(), `"prefix":"acme"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestCustomProviderRowShowsModelsAndAddForm(t *testing.T) {
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Name: "FPT AI", Prefix: "fpt-ai",
				Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
				BaseURL: "https://mkp-api.fptcloud.com/v1", Enabled: true,
				Keys: []storage.CustomProviderKey{{ID: "cpk_1", Enabled: true}}}}, nil
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/ui/providers/custom", nil)
	request.Header.Set("HX-Request", "true")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	// Without a model form there is no way to register what to call upstream.
	// The placeholder must ask for the bare model name; the prefix is applied for the
	// operator, and showing it read as an instruction to type it.
	for _, want := range []string{"Models (0)", "Add model", "No models registered",
		`placeholder="model name (no prefix)"`, "added automatically"} {
		if !strings.Contains(body, want) {
			t.Fatalf("fragment missing %q: %s", want, body)
		}
	}
}

func TestCustomProviderModelsAreListedFromCatalog(t *testing.T) {
	service, e := (*Service)(nil), (*echo.Echo)(nil)
	e, service = newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Prefix: "fpt-ai",
				Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
				BaseURL: "https://mkp-api.fptcloud.com/v1", Enabled: true,
				Keys: []storage.CustomProviderKey{{ID: "cpk_1", Enabled: true}}}}, nil
		},
	})
	service.modelsHook = ModelHooks{
		List: func(_ context.Context, provider string) ([]storage.CatalogModel, error) {
			if provider != "fpt-ai" {
				return nil, nil
			}
			// The catalog stores the callable id, prefix included, which is what the
			// database actually holds.
			return []storage.CatalogModel{{Provider: "fpt-ai", ID: "fpt-ai/Llama-3.3-70B", ContextWindow: 131072}}, nil
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/ui/providers/custom", nil)
	request.Header.Set("HX-Request", "true")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, "fpt-ai/Llama-3.3-70B") || !strings.Contains(body, "Models (1)") {
		t.Fatalf("catalog model not rendered: %s", body)
	}
}

func TestCustomProviderAppearsInProviderGridAndMap(t *testing.T) {
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Name: "FPT AI", Prefix: "fpt-ai",
				Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
				BaseURL: "https://mkp-api.fptcloud.com/v1", Enabled: true,
				Keys: []storage.CustomProviderKey{{ID: "cpk_1", Enabled: true}}}}, nil
		},
	})
	// Provider grid on the Providers tab.
	grid := httptest.NewRecorder()
	e.ServeHTTP(grid, httptest.NewRequest(http.MethodGet, "/ui/providers", nil))
	if !strings.Contains(grid.Body.String(), "FPT AI") {
		t.Fatalf("custom provider missing from the provider grid")
	}
	// Routing map on the Usage tab: a provider with a key counts as configured.
	usage := httptest.NewRecorder()
	e.ServeHTTP(usage, httptest.NewRequest(http.MethodGet, "/ui/usage", nil))
	body := usage.Body.String()
	if !strings.Contains(body, `data-p="fpt-ai"`) {
		t.Fatalf("custom provider missing from the routing map: %s", body[:min(len(body), 600)])
	}
}

func TestCustomProviderUsageAttributionNormalizes(t *testing.T) {
	// Usage rows are written as "custom:<prefix>"; the map and grid key on the prefix.
	if got := normalizeLiveProvider("custom:fpt-ai", ""); got != "fpt-ai" {
		t.Fatalf("normalizeLiveProvider = %q, want fpt-ai", got)
	}
	if got := normalizeLiveProvider("codex", ""); got != "codex" {
		t.Fatalf("built-in normalization regressed: %q", got)
	}
}

func TestNormalizeCatalogModelID(t *testing.T) {
	// Only the model name is typed; the provider's prefix is applied here.
	for _, test := range []struct{ prefix, in, want string }{
		{"fpt-ai", "GLM-5.2", "fpt-ai/GLM-5.2"},
		{"fpt-ai", "fpt-ai/GLM-5.2", "fpt-ai/GLM-5.2"},
		{"fpt-ai", "  fpt-ai/GLM-5.2  ", "fpt-ai/GLM-5.2"},
		{"fpt-ai", "FPT-AI/GLM-5.2", "fpt-ai/GLM-5.2"},
		// Built-in providers get the same treatment, from their own prefix.
		{"codex", "gpt-5.6-sol", "cx/gpt-5.6-sol"},
		{"codex", "cx/gpt-5.6-sol", "cx/gpt-5.6-sol"},
		{"antigravity", "gemini-3-flash", "ag/gemini-3-flash"},
		{"antigravity", "ag/claude-opus-4-6-thinking", "ag/claude-opus-4-6-thinking"},
		{"xai", "grok-4.5", "xai/grok-4.5"},
		// Anthropic ids are bare, so nothing is prepended.
		{"claude", "claude-opus-4-5", "claude-opus-4-5"},
	} {
		got, err := normalizeCatalogModelID(test.prefix, test.in)
		if err != nil || got != test.want {
			t.Fatalf("normalizeCatalogModelID(%q, %q) = %q, %v; want %q", test.prefix, test.in, got, err, test.want)
		}
	}
	// A mismatched prefix would create a model that never routes anywhere.
	if _, err := normalizeCatalogModelID("fpt-ai", "cx/gpt-5.6-sol"); err == nil {
		t.Fatal("a foreign prefix was accepted")
	}
	if _, err := normalizeCatalogModelID("codex", "ag/gemini-3-flash"); err == nil {
		t.Fatal("another built-in provider's prefix was accepted")
	}
	if _, err := normalizeCatalogModelID("fpt-ai", "fpt-ai/"); err == nil {
		t.Fatal("an empty model name was accepted")
	}
	if _, err := normalizeCatalogModelID("fpt-ai", "  "); err == nil {
		t.Fatal("an empty id was accepted")
	}
}

func TestCustomProviderDetailPageShowsKeysNotOAuth(t *testing.T) {
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Name: "FPT AI", Prefix: "fpt-ai",
				Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
				BaseURL: "https://mkp-api.fptcloud.com/v1", Enabled: true,
				Keys: []storage.CustomProviderKey{{ID: "cpk_1", Label: "primary", Enabled: true}}}}, nil
		},
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/providers/fpt-ai", nil))
	body := recorder.Body.String()
	// A custom provider has no OAuth, so offering it is a dead end.
	if strings.Contains(body, "OAuth") {
		t.Fatalf("the custom provider page still offers OAuth: %s", body)
	}
	for _, want := range []string{"API keys", "primary", "+ Add key", "mkp-api.fptcloud.com/v1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail page missing %q", want)
		}
	}
}

func TestBuiltInProviderDetailPageStillOffersOAuth(t *testing.T) {
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return nil, nil },
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/providers/codex", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, "Connections") || !strings.Contains(body, "OAuth") {
		t.Fatalf("the built-in provider page lost its connections block: %s", body[:min(len(body), 400)])
	}
}

func TestModelIDExampleShowsBareModelName(t *testing.T) {
	// The example names a real model for that provider but without the prefix, since
	// the prefix is added on save.
	for provider, want := range map[string]string{
		"codex":       "e.g. gpt-5.6-sol",
		"antigravity": "e.g. gemini-3-flash",
		"claude":      "e.g. claude-opus-4-5",
		"xai":         "e.g. grok-4.5",
		"fpt-ai":      "e.g. model-name",
	} {
		if got := modelIDExample(provider); got != want {
			t.Fatalf("modelIDExample(%q) = %q, want %q", provider, got, want)
		}
	}
}

func TestDetailPageAddModelFormAsksForBareName(t *testing.T) {
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Name: "FPT AI", Prefix: "fpt-ai",
				Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
				BaseURL: "https://mkp-api.fptcloud.com/v1", Enabled: true,
				Keys: []storage.CustomProviderKey{{ID: "cpk_1", Enabled: true}}}}, nil
		},
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/providers/fpt-ai", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, `placeholder="e.g. model-name"`) {
		t.Fatalf("the detail page add-model form does not ask for a bare name: %s", body)
	}
	// It must not suggest another provider's id, prefixed or not.
	if strings.Contains(body, "gpt-5.6-sol") {
		t.Fatal("the detail page suggests a model from a different provider")
	}
}

func TestDetailPageAddModelNormalizesWithoutTargetHint(t *testing.T) {
	// The detail form does not send target=custom, so normalization must be decided
	// from the provider itself or it would store an id with no routing prefix.
	var stored string
	service, e := (*Service)(nil), (*echo.Echo)(nil)
	e, service = newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Prefix: "fpt-ai",
				Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
				BaseURL: "https://mkp-api.fptcloud.com/v1", Enabled: true,
				Keys: []storage.CustomProviderKey{{ID: "cpk_1", Enabled: true}}}}, nil
		},
	})
	service.modelsHook = ModelHooks{
		List: func(context.Context, string) ([]storage.CatalogModel, error) { return nil, nil },
		Add: func(_ context.Context, provider, id, label string, window int) (storage.CatalogModel, error) {
			stored = id
			return storage.CatalogModel{Provider: provider, ID: id}, nil
		},
	}
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, htmxPost("/ui/models", url.Values{
		"provider": {"fpt-ai"}, "id": {"GLM-5.2"},
	}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if stored != "fpt-ai/GLM-5.2" {
		t.Fatalf("stored model id = %q, want the prefix added", stored)
	}
}

func TestCustomProviderModelIDIsNotDoubledInTheUI(t *testing.T) {
	// The catalog id already carries the prefix; rendering provider + id repeated it
	// as fpt-ai/fpt-ai/GLM-5.2, which is not a name anything can be called by.
	service, e := (*Service)(nil), (*echo.Echo)(nil)
	e, service = newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Prefix: "fpt-ai",
				Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
				BaseURL: "https://mkp-api.fptcloud.com/v1", Enabled: true,
				Keys: []storage.CustomProviderKey{{ID: "cpk_1", Enabled: true}}}}, nil
		},
	})
	service.modelsHook = ModelHooks{
		List: func(context.Context, string) ([]storage.CatalogModel, error) {
			return []storage.CatalogModel{{Provider: "fpt-ai", ID: "fpt-ai/GLM-5.2"}}, nil
		},
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ui/providers/custom", nil)
	request.Header.Set("HX-Request", "true")
	e.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if strings.Contains(body, "fpt-ai/fpt-ai/") {
		t.Fatalf("the model id was rendered twice: %s", body)
	}
	if !strings.Contains(body, "<code>fpt-ai/GLM-5.2</code>") {
		t.Fatalf("the callable id is not shown: %s", body)
	}
}

func TestCustomProviderHeroShowsNameAndKeyCount(t *testing.T) {
	// The hero used a placeholder built from the id, so it read "fpt-ai" with
	// "0 connections" while the keys block below said one key.
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) {
			return []storage.CustomProvider{{ID: "cp_1", Name: "FPT AI", Prefix: "fpt-ai",
				Kind: storage.CustomKindOpenAI, APIType: storage.CustomAPITypeChat,
				BaseURL: "https://mkp-api.fptcloud.com/v1", Enabled: true,
				Keys: []storage.CustomProviderKey{{ID: "cpk_1", Enabled: true}}}}, nil
		},
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/providers/fpt-ai", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, "<h1>FPT AI</h1>") {
		t.Fatalf("hero shows the prefix instead of the name: %s", body)
	}
	if !strings.Contains(body, ">1 key<") {
		t.Fatalf("hero does not count keys: %s", body)
	}
	if strings.Contains(body, "0 connections") || strings.Contains(body, "connection<") {
		t.Fatalf("hero still counts OAuth connections: %s", body)
	}
	// The description should name the upstream rather than being empty.
	if !strings.Contains(body, "mkp-api.fptcloud.com/v1") {
		t.Fatalf("hero description is missing the base URL: %s", body)
	}
}

func TestCursorDetailPageOffersImportNotOAuth(t *testing.T) {
	// Cursor has no authorization endpoint, so an OAuth button there is a dead end.
	e, _ := newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return nil, nil },
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/providers/cursor", nil))
	body := recorder.Body.String()
	if strings.Contains(body, "Add Cursor OAuth") {
		t.Fatalf("the Cursor page offers an OAuth flow that does not exist: %s", body)
	}
	// Auto-detect is the primary path; the paste form stays for a Cursor installed
	// where this server cannot read it.
	for _, want := range []string{"Detect local Cursor session", "Import pasted session",
		"cursorAuth/accessToken", "storage.serviceMachineId"} {
		if !strings.Contains(body, want) {
			t.Fatalf("import form missing %q", want)
		}
	}
}

func TestCursorImportRequiresBothValuesAndSameOrigin(t *testing.T) {
	var gotToken, gotMachine string
	service, e := (*Service)(nil), (*echo.Echo)(nil)
	e, service = newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return nil, nil },
	})
	service.SetImportCursor(func(_ context.Context, token, machine string) error {
		gotToken, gotMachine = token, machine
		return nil
	})

	// Cross-origin must never reach the importer: it writes a credential.
	cross := httptest.NewRequest(http.MethodPost, "/ui/oauth/cursor/import",
		strings.NewReader("access_token=t&machine_id=m"))
	cross.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	cross.Header.Set("Origin", "https://evil.example.com")
	denied := httptest.NewRecorder()
	e.ServeHTTP(denied, cross)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("cross-origin import status = %d", denied.Code)
	}
	if gotToken != "" {
		t.Fatal("a cross-origin request reached the importer")
	}

	// A half-filled form is refused rather than stored.
	partial := httptest.NewRecorder()
	e.ServeHTTP(partial, htmxPost("/ui/oauth/cursor/import", url.Values{"access_token": {"t"}}))
	if partial.Code != http.StatusBadRequest {
		t.Fatalf("partial import status = %d", partial.Code)
	}

	ok := httptest.NewRecorder()
	e.ServeHTTP(ok, htmxPost("/ui/oauth/cursor/import", url.Values{
		"access_token": {"token-value"}, "machine_id": {"machine-value"},
	}))
	if ok.Code != http.StatusOK {
		t.Fatalf("import status = %d, body = %s", ok.Code, ok.Body.String())
	}
	if gotToken != "token-value" || gotMachine != "machine-value" {
		t.Fatalf("importer got %q / %q", gotToken, gotMachine)
	}
	// The submitted token must not be echoed back into the page.
	if strings.Contains(ok.Body.String(), "token-value") {
		t.Fatal("the imported token was rendered back to the browser")
	}
}

func TestCursorImportSurfacesTheReason(t *testing.T) {
	service, e := (*Service)(nil), (*echo.Echo)(nil)
	e, service = newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return nil, nil },
	})
	service.SetImportCursor(func(context.Context, string, string) error {
		return errors.New("this Cursor session expired on 2026-05-07")
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, htmxPost("/ui/oauth/cursor/import", url.Values{
		"access_token": {"t"}, "machine_id": {"m"},
	}))
	// An expired session is the common case and the message has to say so.
	if !strings.Contains(recorder.Body.String(), "expired on 2026-05-07") {
		t.Fatalf("reason not shown: %s", recorder.Body.String())
	}
}

func TestCursorDetectReportsWhereItRead(t *testing.T) {
	service, e := (*Service)(nil), (*echo.Echo)(nil)
	e, service = newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return nil, nil },
	})
	service.SetDetectCursor(func(context.Context) (string, error) {
		return "/host-home/Library/Application Support/Cursor/User/globalStorage/state.vscdb", nil
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, htmxPost("/ui/oauth/cursor/detect", url.Values{}))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	// Naming the file is how one Cursor profile is told from another.
	if !strings.Contains(recorder.Body.String(), "state.vscdb") {
		t.Fatalf("result does not name the install: %s", recorder.Body.String())
	}
}

func TestCursorDetectSurfacesTheFailure(t *testing.T) {
	service, e := (*Service)(nil), (*echo.Echo)(nil)
	e, service = newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return nil, nil },
	})
	service.SetDetectCursor(func(context.Context) (string, error) {
		return "", errors.New("no Cursor session found on this machine")
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, htmxPost("/ui/oauth/cursor/detect", url.Values{}))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "no Cursor session found") {
		t.Fatalf("reason not shown: %s", recorder.Body.String())
	}
}

func TestCursorDetectRequiresSameOrigin(t *testing.T) {
	service, e := (*Service)(nil), (*echo.Echo)(nil)
	e, service = newCustomProviderUI(t, CustomProviderHooks{
		List: func(context.Context) ([]storage.CustomProvider, error) { return nil, nil },
	})
	service.SetDetectCursor(func(context.Context) (string, error) {
		t.Fatal("a cross-origin request reached local disk")
		return "", nil
	})
	request := httptest.NewRequest(http.MethodPost, "/ui/oauth/cursor/detect", nil)
	request.Header.Set("Origin", "https://evil.example.com")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
}
