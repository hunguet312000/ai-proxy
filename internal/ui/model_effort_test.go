package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"literouter/internal/pool"
	"literouter/internal/storage"
)

func effortUI(t *testing.T, hooks ModelHooks) *echo.Echo {
	t.Helper()
	service, err := New(pool.New(nil), "token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, hooks, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	return e
}

func postEffort(t *testing.T, e *echo.Echo, id, provider, effort string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"provider": {provider}, "effort": {effort}}
	request := httptest.NewRequest(http.MethodPost, "/ui/models/"+url.PathEscape(id)+"/effort",
		strings.NewReader(form.Encode()))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	request.Header.Set("Origin", "http://example.test")
	request.Host = "example.test"
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	return recorder
}

func TestSetModelEffortReachesTheStoreWithTheModelId(t *testing.T) {
	var gotProvider, gotID, gotEffort string
	e := effortUI(t, ModelHooks{
		List: func(context.Context, string) ([]storage.CatalogModel, error) { return nil, nil },
		SetEffort: func(_ context.Context, provider, id, effort string) error {
			gotProvider, gotID, gotEffort = provider, id, effort
			return nil
		},
	})
	if code := postEffort(t, e, "cx/gpt-5.6-sol", "codex", "xhigh").Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if gotProvider != "codex" || gotID != "cx/gpt-5.6-sol" || gotEffort != "xhigh" {
		t.Errorf("got %q/%q/%q, want codex/cx/gpt-5.6-sol/xhigh", gotProvider, gotID, gotEffort)
	}
}

func TestSetModelEffortRejectsAnUnknownLevel(t *testing.T) {
	// Silently dropping a bad value would look like the setting saved and was ignored.
	e := effortUI(t, ModelHooks{
		List: func(context.Context, string) ([]storage.CatalogModel, error) { return nil, nil },
		SetEffort: func(_ context.Context, _, _, effort string) error {
			if _, err := storage.NormalizeEffort(effort); err != nil {
				return err
			}
			return nil
		},
	})
	if code := postEffort(t, e, "cx/m", "codex", "turbo").Code; code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown effort", code)
	}
}

func TestSetModelEffortAcceptsEmptyToFollowTheRequest(t *testing.T) {
	var gotEffort = "unset"
	e := effortUI(t, ModelHooks{
		List:      func(context.Context, string) ([]storage.CatalogModel, error) { return nil, nil },
		SetEffort: func(_ context.Context, _, _, effort string) error { gotEffort = effort; return nil },
	})
	if code := postEffort(t, e, "cx/m", "codex", "").Code; code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if gotEffort != "" {
		t.Errorf("effort = %q, want empty so the client keeps control", gotEffort)
	}
}

func TestModelChipShowsAnActiveEffortOverride(t *testing.T) {
	// An override that is only visible after expanding an editor is one people forget
	// they set, then spend an afternoon wondering about.
	service, err := New(pool.New(nil), "token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	data := viewData{CatalogModels: []storage.CatalogModel{
		{Provider: "codex", ID: "cx/a", ContextWindow: 400_000, Effort: "xhigh"},
		{Provider: "codex", ID: "cx/b", ContextWindow: 400_000},
	}}
	if err := service.tab.ExecuteTemplate(&out, "model-catalog", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	page := out.String()
	if strings.Count(page, "model-effort-chip") != 1 {
		t.Errorf("chips = %d, want exactly the overridden model to show one",
			strings.Count(page, "model-effort-chip"))
	}
	if !strings.Contains(page, ">xhigh<") {
		t.Error("the chip must name the level in force")
	}
}

func TestEffortControlAppearsForEveryProvider(t *testing.T) {
	// Every route carries effort today — Codex as reasoning.effort, chat
	// upstreams as reasoning_effort — so the control belongs on every model,
	// including custom providers, and "off" is offered for those that must not
	// receive it.
	service, err := New(pool.New(nil), "token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	render := func(models []storage.CatalogModel) string {
		var out strings.Builder
		if err := service.tab.ExecuteTemplate(&out, "model-catalog", viewData{CatalogModels: models}); err != nil {
			t.Fatalf("render: %v", err)
		}
		return out.String()
	}

	local := render([]storage.CatalogModel{
		{Provider: "local", ID: "local/qwen3.8-27B", ContextWindow: 122_049, Effort: "high"},
	})
	if !strings.Contains(local, "/effort") {
		t.Error("a custom provider must offer the effort control")
	}
	if !strings.Contains(local, "model-effort-chip") {
		t.Error("an override on a custom provider must be visible")
	}
	if !strings.Contains(local, `value="off"`) {
		t.Error("the control must offer Off — don't send effort")
	}

	codex := render([]storage.CatalogModel{
		{Provider: "codex", ID: "cx/a", ContextWindow: 400_000, Effort: "high"},
	})
	if !strings.Contains(codex, "/effort") {
		t.Error("Codex carries effort, so the control belongs there")
	}
	if !strings.Contains(codex, "model-effort-chip") {
		t.Error("an override that does reach the upstream must be visible")
	}
}

func TestProviderHonoursEffortMatchesEveryRoute(t *testing.T) {
	// Every route carries a reasoning effort today, and the "off" override
	// strips it on all of them — so the control is offered everywhere.
	for _, provider := range []string{"codex", "cx", "openai", "opencode", "local", "fpt-ai", "custom:fpt-ai", "antigravity", "xai", "claude", ""} {
		if !providerHonoursEffort(provider) {
			t.Errorf("providerHonoursEffort(%q) = false, want true", provider)
		}
	}
}
