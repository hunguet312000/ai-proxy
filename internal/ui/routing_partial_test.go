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
)

// A POST that carries one field must leave every other override alone.
//
// It did not: each model override was written unconditionally from FormValue, which cannot
// tell "the user cleared this box" from "this field was not submitted". A one-field POST
// setting the summarize mode wiped the plan, compact and fallback models on a live install.
func TestRoutingPostDoesNotWipeFieldsItDoesNotCarry(t *testing.T) {
	stored := map[string]string{
		"plan": "cx/gpt-5.6-sol", "compact": "cx/gpt-5.6-luna", "fallback": "cx/gpt-5.6-luna",
	}
	hooks := SettingsHooks{
		GetPlanModel:     func(context.Context) (string, error) { return stored["plan"], nil },
		SetPlanModel:     func(_ context.Context, v string) error { stored["plan"] = v; return nil },
		GetCompactModel:  func(context.Context) (string, error) { return stored["compact"], nil },
		SetCompactModel:  func(_ context.Context, v string) error { stored["compact"] = v; return nil },
		GetFallbackModel: func(context.Context) (string, error) { return stored["fallback"], nil },
		SetFallbackModel: func(_ context.Context, v string) error { stored["fallback"] = v; return nil },
		SetSummarizeMode: func(_ context.Context, v string) error { stored["summarize"] = v; return nil },
		GetSummarizeMode: func(context.Context) (string, error) { return stored["summarize"], nil },
	}
	service, err := New(pool.New(nil), "token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, ModelHooks{}, hooks, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	service.Register(e)

	post := func(form url.Values) {
		request := httptest.NewRequest(http.MethodPost, "/ui/routing", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
		}
	}

	post(url.Values{"summarize_mode": {"trim"}})
	if stored["summarize"] != "trim" {
		t.Fatalf("summarize mode = %q, want trim", stored["summarize"])
	}
	for field, want := range map[string]string{
		"plan": "cx/gpt-5.6-sol", "compact": "cx/gpt-5.6-luna", "fallback": "cx/gpt-5.6-luna",
	} {
		if stored[field] != want {
			t.Errorf("%s model = %q after a POST that never mentioned it, want %q", field, stored[field], want)
		}
	}

	// And a field that IS submitted empty still clears — that is the user emptying the box.
	post(url.Values{"plan_model": {""}, "compact_model": {"cx/gpt-5.6-luna"}})
	if stored["plan"] != "" {
		t.Errorf("plan model = %q, want it cleared by an explicit empty value", stored["plan"])
	}
	if stored["fallback"] != "cx/gpt-5.6-luna" {
		t.Errorf("fallback model = %q, want it untouched", stored["fallback"])
	}
}

// The build-image-prompt checkbox follows the same presence semantics: a partial POST
// that never mentions it must not flip it, and an explicit checked/unchecked writes it.
func TestRoutingBuildImagePromptPresenceSemantics(t *testing.T) {
	buildPrompt := false
	hooks := SettingsHooks{
		// routingHandler requires SetPlanModel to be wired before it does anything.
		SetPlanModel:        func(context.Context, string) error { return nil },
		GetBuildImagePrompt: func(context.Context) (bool, error) { return buildPrompt, nil },
		SetBuildImagePrompt: func(_ context.Context, v bool) error { buildPrompt = v; return nil },
	}
	service, err := New(pool.New(nil), "token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, ModelHooks{}, hooks, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	service.Register(e)

	post := func(form url.Values) {
		request := httptest.NewRequest(http.MethodPost, "/ui/routing", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
		}
	}

	// A POST that never mentions the checkbox leaves it alone.
	post(url.Values{"plan_model": {"cx/gpt-5.6-sol"}})
	if buildPrompt {
		t.Fatal("build_image_prompt flipped by a POST that did not carry it")
	}

	// Checking it writes true.
	post(url.Values{"build_image_prompt": {"on"}})
	if !buildPrompt {
		t.Fatal("checked checkbox did not set build_image_prompt")
	}

	// Unchecking it (present with empty value) writes false.
	post(url.Values{"build_image_prompt": {""}})
	if buildPrompt {
		t.Fatal("unchecked checkbox did not clear build_image_prompt")
	}
}
