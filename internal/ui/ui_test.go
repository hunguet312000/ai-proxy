package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/clisetup"
	"literouter/internal/pool"
	"literouter/internal/storage"
)

// refreshRecorder captures the account ids handed to the quota refresh hook.
//
// A plain variable is not enough: rendering a provider page calls queueQuotaRefresh,
// which invokes the same hook from a background goroutine, so the hook really is
// called from two goroutines during this test. Guarding it here rather than
// serialising the production path keeps the lazy refresh doing what it is for.
type refreshRecorder struct {
	mu   sync.Mutex
	last string
}

func (r *refreshRecorder) record(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last = id
}

func (r *refreshRecorder) latest() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func TestUIRoutesAndActions(t *testing.T) {
	accountPool := pool.New([]pool.Account{{ID: "codex:account", Provider: "codex", Label: "User", Enabled: true, Weight: 1}})
	refreshed := &refreshRecorder{}
	service, err := New(accountPool, "api-token", func(context.Context, string) (OAuthResult, error) {
		return OAuthResult{AuthURL: "https://example.com/auth"}, nil
	}, func(_ context.Context, id string) error {
		refreshed.record(id)
		return nil
	}, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}

	index := httptest.NewRecorder()
	e.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/ui", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "LiteRouter") {
		t.Fatalf("index = %d %q", index.Code, index.Body.String())
	}
	for _, marker := range []string{`class="shell"`, `/ui/endpoint`, `/ui/providers`, `/ui/quota`, `/ui/cli`, `/ui/usage`, `Local gateway endpoints`, `nav-item`, `Workspace`, `System`, `Usage`} {
		if !strings.Contains(index.Body.String(), marker) {
			t.Fatalf("index missing %q in %s", marker, index.Body.String())
		}
	}
	providers := httptest.NewRecorder()
	e.ServeHTTP(providers, httptest.NewRequest(http.MethodGet, "/ui/providers", nil))
	if providers.Code != http.StatusOK || !strings.Contains(providers.Body.String(), "Codex") || !strings.Contains(providers.Body.String(), "/ui/providers/codex") {
		t.Fatalf("providers = %d %q", providers.Code, providers.Body.String())
	}
	detail := httptest.NewRecorder()
	e.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/ui/providers/codex", nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), "User") || !strings.Contains(detail.Body.String(), "Connections") || !strings.Contains(detail.Body.String(), "Risk notice") || !strings.Contains(detail.Body.String(), "Algorithm") || !strings.Contains(detail.Body.String(), "Models") || !strings.Contains(detail.Body.String(), "Add Model") {
		t.Fatalf("provider detail = %d %q", detail.Code, detail.Body.String())
	}
	if !strings.Contains(index.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("CSP = %q", index.Header().Get("Content-Security-Policy"))
	}

	form := url.Values{"provider": {"codex"}}
	oauth := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/ui/oauth/start", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	e.ServeHTTP(oauth, request)
	if oauth.Code != http.StatusOK || !strings.Contains(oauth.Body.String(), "https://example.com/auth") || !strings.Contains(oauth.Body.String(), "data-oauth-popup") || strings.Contains(oauth.Body.String(), "noopener") {
		t.Fatalf("OAuth = %d %q", oauth.Code, oauth.Body.String())
	}

	quota := httptest.NewRecorder()
	quotaRequest := httptest.NewRequest(http.MethodPost, "/ui/accounts/codex%3Aaccount/quota", nil)
	quotaRequest.Header.Set("Origin", "http://example.com")
	quotaRequest.Host = "example.com"
	e.ServeHTTP(quota, quotaRequest)
	// quotaHandler calls the hook synchronously, so this is settled by the time
	// ServeHTTP returns — unlike the background refresh, which only ever passes the
	// same id in this fixture.
	if latest := refreshed.latest(); quota.Code != http.StatusOK || latest != "codex:account" || !strings.Contains(quota.Body.String(), "User") {
		t.Fatalf("quota = %d %q, refreshed = %q", quota.Code, quota.Body.String(), latest)
	}
}

func TestStrategySelectionPersistsAcrossRender(t *testing.T) {
	strategy := "smart"
	service, err := New(
		pool.New([]pool.Account{{ID: "account", Provider: "codex", Enabled: true, Weight: 1}}),
		"api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{},
		SettingsHooks{
			GetStrategy: func(context.Context) (string, error) { return strategy, nil },
			SetStrategy: func(_ context.Context, value string) error { strategy = value; return nil },
		}, UsageHooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"strategy": {"round_robin"}}
	request := httptest.NewRequest(http.MethodPost, "/ui/strategy", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strategy != "round_robin" {
		t.Fatalf("strategy response = %d %q; strategy = %q", response.Code, response.Body.String(), strategy)
	}
	rendered := httptest.NewRecorder()
	e.ServeHTTP(rendered, httptest.NewRequest(http.MethodGet, "/ui/providers/codex", nil))
	if rendered.Code != http.StatusOK || !strings.Contains(rendered.Body.String(), `<option value="round_robin" selected>`) {
		t.Fatalf("rendered strategy = %d %q", rendered.Code, rendered.Body.String())
	}
	for _, marker := range []string{`hx-post="/ui/strategy"`, `hx-trigger="change"`, `hx-disabled-elt="this"`} {
		if !strings.Contains(rendered.Body.String(), marker) {
			t.Fatalf("rendered strategy missing %q", marker)
		}
	}
}

func TestQuotaAccountsGroupByProvider(t *testing.T) {
	groups := groupQuotaAccounts([]AccountView{
		{Account: pool.Account{ID: "codex:on", Provider: "codex", Enabled: true}},
		{Account: pool.Account{ID: "xai:off", Provider: "xai", Enabled: false, QuotaExhausted: true}},
		{Account: pool.Account{ID: "codex:off", Provider: "codex", Enabled: false}},
	})
	if len(groups) != 2 {
		t.Fatalf("groups = %#v", groups)
	}
	if groups[0].ID != "codex" || groups[0].Name != "Codex" || groups[0].Total != 2 || groups[0].Enabled != 1 || groups[0].Disabled != 1 {
		t.Fatalf("codex group = %#v", groups[0])
	}
	if groups[1].ID != "xai" || groups[1].Exhausted != 1 || groups[1].Disabled != 1 {
		t.Fatalf("xai group = %#v", groups[1])
	}
}

func TestAccountSortsPlaceDisabledAccountsLast(t *testing.T) {
	base := []AccountView{
		{Account: pool.Account{ID: "disabled", Enabled: false, Weight: 100, Label: "A", QuotaRemainingPercent: 0, QuotaResetAt: time.Now().Add(-time.Hour)}},
		{Account: pool.Account{ID: "enabled-low", Enabled: true, Weight: 1, Label: "Z", QuotaRemainingPercent: 90}},
		{Account: pool.Account{ID: "enabled-high", Enabled: true, Weight: 9, Label: "Y", QuotaRemainingPercent: 80}},
	}

	connections := append([]AccountView(nil), base...)
	sortAccountConnections(connections)
	if connections[0].ID != "enabled-high" || connections[1].ID != "enabled-low" || connections[2].ID != "disabled" {
		t.Fatalf("connections = %#v", connections)
	}

	for _, sortKey := range []string{"expiring", "remaining", "name", "provider"} {
		accounts := append([]AccountView(nil), base...)
		sortQuotaAccounts(accounts, sortKey)
		if !accounts[0].Enabled || !accounts[1].Enabled || accounts[2].Enabled {
			t.Fatalf("sort %q = %#v", sortKey, accounts)
		}
	}
}

func TestAccountTogglePersistsStateAndReorders(t *testing.T) {
	accountPool := pool.New([]pool.Account{
		{ID: "codex:first", Provider: "codex", Label: "First", Enabled: true, Weight: 2},
		{ID: "codex:second", Provider: "codex", Label: "Second", Enabled: true, Weight: 1},
	})
	var updatedID string
	var updatedEnabled bool
	service, err := New(accountPool, "api-token", nil, nil, nil, func(_ context.Context, id string, enabled bool, weight int) error {
		updatedID, updatedEnabled = id, enabled
		account, ok := accountPool.Get(id)
		if !ok {
			return errors.New("account not found")
		}
		account.Enabled = enabled
		account.Weight = weight
		accountPool.Upsert(account)
		return nil
	}, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"enabled": {"false"}, "weight": {"2"}, "view": {"connections"}}
	request := httptest.NewRequest(http.MethodPost, "/ui/accounts/codex%3Afirst", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	response := httptest.NewRecorder()
	e.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || updatedID != "codex:first" || updatedEnabled {
		t.Fatalf("response = %d %q, update = %q/%v", response.Code, body, updatedID, updatedEnabled)
	}
	if first, second := strings.Index(body, "Second"), strings.Index(body, "First"); first < 0 || second < 0 || first >= second {
		t.Fatalf("disabled account was not moved last: %s", body)
	}
}

func TestQuotaBucketClampsAndRoundsUp(t *testing.T) {
	tests := []struct {
		value float64
		want  int
	}{{-1, 0}, {0, 0}, {1, 10}, {42, 50}, {99, 100}, {101, 100}}
	for _, test := range tests {
		if got := quotaBucket(test.value); got != test.want {
			t.Fatalf("quotaBucket(%v) = %d, want %d", test.value, got, test.want)
		}
	}
}

func TestAccountViewRendersQuotaWindows(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(3*24*time.Hour + 17*time.Hour + 25*time.Minute)
	accountPool := pool.New([]pool.Account{{
		ID: "codex:account", Provider: "codex", Label: "User", Enabled: true, Weight: 1,
		QuotaRemainingPercent: 51, QuotaResetAt: reset, QuotaUpdatedAt: now,
	}})
	service, err := New(accountPool, "api-token", nil, nil, func(_ context.Context, id string) ([]storage.QuotaSnapshot, error) {
		return []storage.QuotaSnapshot{
			{AccountID: id, Key: "session", Used: 49, Total: 100, Remaining: 51, RemainingPercent: 51, ResetAt: reset, FetchedAt: now, Source: "codex"},
			{AccountID: id, Key: "weekly", Used: 20, Total: 100, Remaining: 80, RemainingPercent: 80, ResetAt: reset.Add(24 * time.Hour), FetchedAt: now, Source: "codex"},
		}, nil
	}, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/accounts", nil))
	body := recorder.Body.String()
	for _, marker := range []string{"Session", "Weekly", "49 / 100", "in 3d"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("accounts missing %q in %s", marker, body)
		}
	}
}

func TestUICLISetupDownloads(t *testing.T) {
	service, err := New(pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{}, "fast")
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)

	index := httptest.NewRecorder()
	indexRequest := httptest.NewRequest(http.MethodGet, "/ui", nil)
	indexRequest.Host = "127.0.0.1:8317"
	e.ServeHTTP(index, indexRequest)
	cli := httptest.NewRecorder()
	e.ServeHTTP(cli, httptest.NewRequest(http.MethodGet, "/ui/cli", nil))
	if !strings.Contains(cli.Body.String(), "CLI Tools") || !strings.Contains(cli.Body.String(), "Browse") || !strings.Contains(cli.Body.String(), "fast") || strings.Contains(cli.Body.String(), "Available Models") || strings.Contains(cli.Body.String(), `value="api-token"`) {
		t.Fatalf("cli = %s", cli.Body.String())
	}

	form := url.Values{"base_url": {"http://127.0.0.1:8317"}, "model": {"fast"}}
	request := httptest.NewRequest(http.MethodPost, "/ui/setup/codex/apply?download=1", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Content-Disposition"), "literouter-codex-apply.sh") || !strings.Contains(recorder.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("download = %d %#v %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "PAYLOAD=") || strings.Contains(recorder.Body.String(), "api-token") {
		t.Fatalf("script payload is not encoded: %s", recorder.Body.String())
	}
}

func TestUISession(t *testing.T) {
	service, err := New(pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)
	form := url.Values{"token": {"api-token"}}
	request := httptest.NewRequest(http.MethodPost, "/ui/session", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Set-Cookie"), "HttpOnly") || !strings.Contains(recorder.Header().Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("response = %d %#v %q", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestUIAPIKeyCreateListPauseDelete(t *testing.T) {
	var keys []storage.APIKey
	hooks := APIKeyHooks{
		List: func(context.Context) ([]storage.APIKey, error) {
			out := make([]storage.APIKey, len(keys))
			copy(out, keys)
			for i := range out {
				out[i].Token = ""
			}
			return out, nil
		},
		Create: func(_ context.Context, name string) (storage.APIKey, error) {
			key := storage.APIKey{ID: "key_test", Name: name, Prefix: "sk-lr-abc", Enabled: true, Token: "sk-lr-secret-once"}
			keys = append(keys, key)
			return key, nil
		},
		SetEnabled: func(_ context.Context, id string, enabled bool) error {
			for i := range keys {
				if keys[i].ID == id {
					keys[i].Enabled = enabled
					return nil
				}
			}
			return context.Canceled
		},
		Delete: func(_ context.Context, id string) error {
			filtered := keys[:0]
			for _, key := range keys {
				if key.ID != id {
					filtered = append(filtered, key)
				}
			}
			keys = filtered
			return nil
		},
		Valid: func(token string) bool { return token == "api-token" || token == "sk-lr-secret-once" },
	}
	service, err := New(pool.New(nil), "api-token", nil, nil, nil, nil, nil, hooks, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)

	create := httptest.NewRecorder()
	form := url.Values{"name": {"claude-code"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	e.ServeHTTP(create, req)
	if create.Code != http.StatusOK || !strings.Contains(create.Body.String(), "sk-lr-secret-once") || !strings.Contains(create.Body.String(), "claude-code") {
		t.Fatalf("create = %d %s", create.Code, create.Body.String())
	}

	list := httptest.NewRecorder()
	e.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/ui/keys", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "sk-lr-abc") || strings.Contains(list.Body.String(), "sk-lr-secret-once") {
		t.Fatalf("list leaked secret: %s", list.Body.String())
	}

	pause := httptest.NewRecorder()
	pauseForm := url.Values{"enabled": {"false"}}
	pauseReq := httptest.NewRequest(http.MethodPost, "/ui/keys/key_test/toggle", strings.NewReader(pauseForm.Encode()))
	pauseReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	pauseReq.Header.Set("Origin", "http://example.com")
	pauseReq.Host = "example.com"
	e.ServeHTTP(pause, pauseReq)
	if pause.Code != http.StatusOK || !strings.Contains(pause.Body.String(), "paused") {
		t.Fatalf("pause = %d %s", pause.Code, pause.Body.String())
	}

	del := httptest.NewRecorder()
	delReq := httptest.NewRequest(http.MethodDelete, "/ui/keys/key_test", nil)
	delReq.Header.Set("Origin", "http://example.com")
	delReq.Host = "example.com"
	e.ServeHTTP(del, delReq)
	if del.Code != http.StatusOK || !strings.Contains(del.Body.String(), "No managed keys yet") {
		t.Fatalf("delete = %d %s", del.Code, del.Body.String())
	}
}

func TestUIAccountUpdateAndDelete(t *testing.T) {
	accountPool := pool.New([]pool.Account{{ID: "codex:account", Provider: "codex", Enabled: true, Weight: 1}})
	var updated, deleted bool
	service, err := New(accountPool, "api-token", nil, nil, nil, func(_ context.Context, id string, enabled bool, weight int) error {
		updated = id == "codex:account" && !enabled && weight == 3
		account, _ := accountPool.Get(id)
		account.Enabled, account.Weight = enabled, weight
		accountPool.Upsert(account)
		return nil
	}, func(_ context.Context, id string) error {
		deleted = id == "codex:account"
		accountPool.Remove(id)
		return nil
	}, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)

	form := url.Values{"enabled": {"false"}, "weight": {"3"}}
	update := httptest.NewRequest(http.MethodPost, "/ui/accounts/codex%3Aaccount", strings.NewReader(form.Encode()))
	update.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	update.Header.Set("Origin", "http://example.com")
	update.Host = "example.com"
	updateRecorder := httptest.NewRecorder()
	e.ServeHTTP(updateRecorder, update)
	if updateRecorder.Code != http.StatusOK || !updated || (!strings.Contains(updateRecorder.Body.String(), "is-off") && !strings.Contains(updateRecorder.Body.String(), "disabled")) {
		t.Fatalf("update = %d %q, updated = %v", updateRecorder.Code, updateRecorder.Body.String(), updated)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/ui/accounts/codex%3Aaccount", nil)
	deleteRequest.Header.Set("Origin", "http://example.com")
	deleteRequest.Host = "example.com"
	deleteRecorder := httptest.NewRecorder()
	e.ServeHTTP(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusOK || !deleted || accountPool.Len() != 0 {
		t.Fatalf("delete = %d, deleted = %v, accounts = %d", deleteRecorder.Code, deleted, accountPool.Len())
	}
}

func TestUIRejectsCrossOriginMutation(t *testing.T) {
	service, err := New(pool.New(nil), "api-token", func(context.Context, string) (OAuthResult, error) {
		return OAuthResult{AuthURL: "https://example.com"}, nil
	}, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)
	request := httptest.NewRequest(http.MethodPost, "/ui/oauth/start", strings.NewReader("provider=codex"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://evil.example")
	request.Host = "127.0.0.1:8317"
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestUIRejectsUnsafeOAuthURL(t *testing.T) {
	service, err := New(pool.New(nil), "api-token", func(context.Context, string) (OAuthResult, error) {
		return OAuthResult{AuthURL: "javascript:alert(1)"}, nil
	}, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)
	form := url.Values{"provider": {"codex"}}
	request := httptest.NewRequest(http.MethodPost, "/ui/oauth/start", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
}

func TestUIModelTest(t *testing.T) {
	service, err := New(pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{
		Test: func(_ context.Context, model string) (ModelTestResult, error) {
			if model != "cx/gpt-5.6-sol" {
				t.Fatalf("model = %q", model)
			}
			return ModelTestResult{OK: true, Model: model, Latency: "12ms", Preview: "pong"}, nil
		},
	}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)
	form := url.Values{"id": {"cx/gpt-5.6-sol"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/models/test?format=json", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) || !strings.Contains(rec.Body.String(), "pong") {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
}

func TestUICLISetupDirectApply(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	service, err := New(pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{}, "fast")
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)
	form := url.Values{"base_url": {"http://127.0.0.1:8317"}, "model": {"fast"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/setup/codex/apply", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.com")
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Codex configured") {
		t.Fatalf("direct apply = %d %s", rec.Code, rec.Body.String())
	}
	cfg, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil || !strings.Contains(string(cfg), "literouter") {
		t.Fatalf("config missing: %v %s", err, cfg)
	}
}

func TestUICLISetupClaudeReloadsAppliedFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	service, err := New(pool.New(nil), "secret-api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)
	form := url.Values{
		"base_url":     {"http://127.0.0.1:8317"},
		"model":        {"default-model"},
		"fable_model":  {"fable-model"},
		"opus_model":   {"opus-model"},
		"sonnet_model": {"sonnet-model"},
		"haiku_model":  {"haiku-model"},
	}
	apply := httptest.NewRequest(http.MethodPost, "/ui/setup/claude/apply", strings.NewReader(form.Encode()))
	apply.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	apply.Header.Set("Origin", "http://example.com")
	apply.Host = "example.com"
	applied := httptest.NewRecorder()
	e.ServeHTTP(applied, apply)
	if applied.Code != http.StatusOK {
		t.Fatalf("apply = %d %s", applied.Code, applied.Body.String())
	}

	reloaded := httptest.NewRecorder()
	e.ServeHTTP(reloaded, httptest.NewRequest(http.MethodGet, "/ui/cli", nil))
	body := reloaded.Body.String()
	for _, field := range []string{
		`name="base_url" value="http://127.0.0.1:8317"`,
		`name="model" value="default-model"`,
		`name="fable_model" value="fable-model"`,
		`name="opus_model" value="opus-model"`,
		`name="sonnet_model" value="sonnet-model"`,
		`name="haiku_model" value="haiku-model"`,
	} {
		if !strings.Contains(body, field) {
			t.Fatalf("reloaded form missing %q: %s", field, body)
		}
	}
	if strings.Contains(body, "secret-api-token") {
		t.Fatal("reloaded form exposed auth token")
	}
}

func TestUsageSinceAtUsesUTC7CalendarBoundaries(t *testing.T) {
	now := time.Date(2026, time.July, 23, 10, 30, 0, 0, time.UTC)
	tests := []struct {
		name     string
		rangeKey string
		wantKey  string
		wantUTC  time.Time
	}{
		{name: "24h", rangeKey: "24h", wantKey: "24h", wantUTC: time.Date(2026, time.July, 22, 17, 0, 0, 0, time.UTC)},
		{name: "day alias", rangeKey: "day", wantKey: "24h", wantUTC: time.Date(2026, time.July, 22, 17, 0, 0, 0, time.UTC)},
		{name: "7d", rangeKey: "7d", wantKey: "7d", wantUTC: time.Date(2026, time.July, 16, 17, 0, 0, 0, time.UTC)},
		{name: "week alias", rangeKey: " WEEK ", wantKey: "7d", wantUTC: time.Date(2026, time.July, 16, 17, 0, 0, 0, time.UTC)},
		{name: "30d", rangeKey: "30d", wantKey: "30d", wantUTC: time.Date(2026, time.June, 23, 17, 0, 0, 0, time.UTC)},
		{name: "month alias", rangeKey: "month", wantKey: "30d", wantUTC: time.Date(2026, time.June, 23, 17, 0, 0, 0, time.UTC)},
		{name: "invalid", rangeKey: "invalid", wantKey: "24h", wantUTC: time.Date(2026, time.July, 22, 17, 0, 0, 0, time.UTC)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, gotKey := usageSinceAt(now, test.rangeKey)
			if gotKey != test.wantKey || !got.Equal(test.wantUTC) {
				t.Fatalf("usageSinceAt(%q) = %v, %q; want %v, %q", test.rangeKey, got, gotKey, test.wantUTC, test.wantKey)
			}
			if got.Hour() != 0 || got.Location() != usageLocation {
				t.Fatalf("local boundary = %v, want 00:00 in %s", got, usageLocation)
			}
		})
	}
}

func TestUsageSinceAtChangesAtUTC7Midnight(t *testing.T) {
	before := time.Date(2026, time.July, 22, 16, 59, 59, 999999999, time.UTC)
	after := time.Date(2026, time.July, 22, 17, 0, 0, 0, time.UTC)

	beforeSince, _ := usageSinceAt(before, "24h")
	afterSince, _ := usageSinceAt(after, "24h")
	if want := time.Date(2026, time.July, 21, 17, 0, 0, 0, time.UTC); !beforeSince.Equal(want) {
		t.Fatalf("before midnight since = %v, want %v", beforeSince, want)
	}
	if want := time.Date(2026, time.July, 22, 17, 0, 0, 0, time.UTC); !afterSince.Equal(want) {
		t.Fatalf("after midnight since = %v, want %v", afterSince, want)
	}
}

func TestFormatDayTimeUsesUTC7(t *testing.T) {
	// 16:05 UTC on Jul 21 == 23:05 UTC+7 same calendar day.
	old := time.Date(2026, time.July, 21, 16, 5, 0, 0, time.UTC)
	if got := formatDayTime(old); got != "07-21 23:05" {
		t.Fatalf("formatDayTime() = %q, want 07-21 23:05 (UTC+7)", got)
	}
	if got := formatClock(old); got != "23:05:00" {
		t.Fatalf("formatClock() = %q, want 23:05:00 (UTC+7)", got)
	}
}

func TestUIUsageAnalytics(t *testing.T) {
	now := time.Now().UTC()
	hooks := UsageHooks{
		Summary: func(_ context.Context, since time.Time, limit int) (storage.UsageSummary, error) {
			if since.After(now) {
				t.Fatalf("since in future: %v", since)
			}
			if limit <= 0 {
				t.Fatalf("limit = %d", limit)
			}
			return storage.UsageSummary{
				Requests: 2, PromptTokens: 1200, CompletionTokens: 340, CachedTokens: 100, CostUSD: 0.042,
				ByProvider: []storage.UsageProviderStat{{Provider: "codex", Requests: 2, Tokens: 1540, CostUSD: 0.042}},
				ByModel:    []storage.UsageModelStat{{Model: "cx/gpt-5.6-sol", Requests: 2, InTokens: 1200, OutTokens: 340, CostUSD: 0.042}},
				Recent: []storage.UsageEvent{{
					ID: 1, Timestamp: now, Provider: "codex", Model: "cx/gpt-5.6-sol", Endpoint: "/v1/chat/completions",
					Status: "ok", PromptTokens: 150100, CompletionTokens: 170, CachedTokens: 50, CostUSD: 0.021,
					Effort: "high", PromptTokensEstimated: true, CachedTokensReported: true,
				}},
			}, nil
		},
	}
	service, err := New(pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, hooks)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/usage?range=7d", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, marker := range []string{
		"Overview", "Routing map", "By provider", "By model", "Recent activity",
		"OpenAI Codex", "GPT 5.6 Sol", "$0.04", "range=7d", "is-active",
		"usage-map", "share-bar", "kpi-row", "Chat", "No providers configured", "used",
		"150,100", "usage-est", "50 cached input tokens reported by upstream",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("usage missing %q in %s", marker, body)
		}
	}
}

func TestComputeUsageLiveRecentOnly(t *testing.T) {
	now := time.Now()
	// Two providers both "recent" — only the newest path is hot; both marked used.
	live := computeUsageLive(storage.UsageSummary{
		Requests: 3,
		ByProvider: []storage.UsageProviderStat{
			{Provider: "codex", Requests: 2},
			{Provider: "xai", Requests: 1},
		},
		Recent: []storage.UsageEvent{
			{Provider: "xai", Model: "xai/grok-4.5", Timestamp: now.Add(-10 * time.Second)},
			{Provider: "codex", Model: "cx/gpt-5.6-sol", Timestamp: now.Add(-28 * time.Second)},
		},
	})
	if !live.XAI || live.Codex || !live.Any || live.Last != "xai" {
		t.Fatalf("live = %+v want only xai calling", live)
	}
	if !live.UsedXAI || !live.UsedCodex || live.UsedClaude {
		t.Fatalf("used flags = %+v", live)
	}
	// Newest flips to codex
	live2 := computeUsageLive(storage.UsageSummary{
		ByProvider: []storage.UsageProviderStat{{Provider: "codex", Requests: 1}, {Provider: "xai", Requests: 1}},
		Recent: []storage.UsageEvent{
			{Provider: "codex", Model: "cx/gpt-5.6-sol", Timestamp: now.Add(-5 * time.Second)},
			{Provider: "xai", Model: "xai/grok-4.5", Timestamp: now.Add(-15 * time.Second)},
		},
	})
	if !live2.Codex || live2.XAI || live2.Last != "codex" {
		t.Fatalf("live2 = %+v want only codex", live2)
	}
	// Stale call: not calling, but still used from ByProvider
	stale := computeUsageLive(storage.UsageSummary{
		ByProvider: []storage.UsageProviderStat{{Provider: "codex", Requests: 1}},
		Recent:     []storage.UsageEvent{{Provider: "codex", Timestamp: now.Add(-10 * time.Minute)}},
	})
	if stale.Any || stale.Codex {
		t.Fatalf("stale should not be calling: %+v", stale)
	}
	if !stale.UsedCodex || !stale.UsedAny {
		t.Fatalf("stale should be used: %+v", stale)
	}
	// Never used
	unused := computeUsageLive(storage.UsageSummary{})
	if unused.UsedAny || unused.Any {
		t.Fatalf("unused = %+v", unused)
	}
	// A custom provider (opencode) carrying the newest request must light the client
	// node too — previously only codex/claude/xai set live.Any, so the map looked idle
	// while OpenCode was actively serving.
	custom := computeUsageLive(storage.UsageSummary{
		Recent: []storage.UsageEvent{{Provider: "custom:opencode", Model: "opencode/deepseek-v4-flash", Timestamp: now.Add(-5 * time.Second)}},
	})
	if !custom.Any {
		t.Fatalf("custom provider traffic should be live: %+v", custom)
	}
	if custom.Codex || custom.Claude || custom.XAI {
		t.Fatalf("custom traffic must not light a named provider flag: %+v", custom)
	}
}

func TestBuildUsageMapConfiguredOnly(t *testing.T) {
	providers := []ProviderInfo{
		{ID: "codex", Name: "Codex", Icon: "codex", Total: 2},
		{ID: "claude", Name: "Claude", Icon: "claude", Total: 0},
		{ID: "xai", Name: "xAI", Icon: "xai", Total: 1},
	}
	live := UsageLive{UsedCodex: true, UsedXAI: true, UsedAny: true, XAI: true, Any: true, Last: "xai"}
	summary := storage.UsageSummary{
		ByProvider: []storage.UsageProviderStat{
			{Provider: "codex", Requests: 3, Tokens: 90},
			{Provider: "xai", Requests: 2, Tokens: 40},
			{Provider: "claude", Requests: 9, Tokens: 99}, // ignored — not configured
		},
	}
	nodes := buildUsageMap(providers, summary, live)
	if len(nodes) != 2 {
		t.Fatalf("nodes=%d want 2 (configured only): %+v", len(nodes), nodes)
	}
	if nodes[0].ID != "codex" || nodes[1].ID != "xai" {
		t.Fatalf("order/ids = %+v", nodes)
	}
	if nodes[1].Calling != true || nodes[0].Calling {
		t.Fatalf("calling flags = %+v", nodes)
	}
	if nodes[0].Requests != 3 || nodes[1].Tokens != 40 {
		t.Fatalf("stats = %+v", nodes)
	}
}

func TestUIModelContextWindowCreateUpdate(t *testing.T) {
	models := []storage.CatalogModel{{Provider: "codex", ID: "cx/gpt-5.6-sol", Label: "GPT 5.6 Sol", ContextWindow: 400_000}}
	var addedWindow, updatedWindow int
	hooks := ModelHooks{
		List: func(_ context.Context, provider string) ([]storage.CatalogModel, error) {
			return models, nil
		},
		Add: func(_ context.Context, provider, id, label string, window int) (storage.CatalogModel, error) {
			addedWindow = window
			m := storage.CatalogModel{Provider: provider, ID: id, Label: label, ContextWindow: window}
			models = append(models, m)
			return m, nil
		},
		SetContextWindow: func(_ context.Context, provider, id string, window int) error {
			updatedWindow = window
			for i := range models {
				if models[i].Provider == provider && models[i].ID == id {
					models[i].ContextWindow = window
				}
			}
			return nil
		},
	}
	service, err := New(pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, hooks, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	_ = service.Register(e)

	list := httptest.NewRecorder()
	e.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/ui/models?provider=codex", nil))
	for _, marker := range []string{"Context", "400k", `name="context_window"`, "400000"} {
		if !strings.Contains(list.Body.String(), marker) {
			t.Fatalf("list missing %q: %s", marker, list.Body.String())
		}
	}

	createForm := url.Values{"provider": {"codex"}, "id": {"cx/new-model"}, "label": {"New"}, "context_window": {"300000"}}
	createReq := httptest.NewRequest(http.MethodPost, "/ui/models", strings.NewReader(createForm.Encode()))
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createReq.Header.Set("Origin", "http://example.com")
	createReq.Host = "example.com"
	create := httptest.NewRecorder()
	e.ServeHTTP(create, createReq)
	if create.Code != http.StatusOK || addedWindow != 300_000 {
		t.Fatalf("create=%d addedWindow=%d body=%s", create.Code, addedWindow, create.Body.String())
	}

	updateForm := url.Values{"provider": {"codex"}, "context_window": {"512000"}}
	updateReq := httptest.NewRequest(http.MethodPost, "/ui/models/cx%2Fgpt-5.6-sol/context", strings.NewReader(updateForm.Encode()))
	updateReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	updateReq.Header.Set("Origin", "http://example.com")
	updateReq.Host = "example.com"
	update := httptest.NewRecorder()
	e.ServeHTTP(update, updateReq)
	if update.Code != http.StatusOK || updatedWindow != 512_000 || !strings.Contains(update.Body.String(), "512k") {
		t.Fatalf("update=%d updatedWindow=%d body=%s", update.Code, updatedWindow, update.Body.String())
	}
}

func TestRoutingHandlerSavesAndClears(t *testing.T) {
	var saved []string
	planModel := "cc/claude-opus-5"
	service, err := New(
		pool.New([]pool.Account{{ID: "account", Provider: "codex", Enabled: true, Weight: 1}}),
		"api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{},
		SettingsHooks{
			GetPlanModel: func(context.Context) (string, error) { return planModel, nil },
			SetPlanModel: func(_ context.Context, value string) error {
				saved = append(saved, value)
				planModel = value
				return nil
			},
		}, UsageHooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}

	post := func(value string) *httptest.ResponseRecorder {
		form := url.Values{"plan_model": {value}}
		request := httptest.NewRequest(http.MethodPost, "/ui/routing", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)
		return recorder
	}

	// An unlisted id must be accepted: custom provider prefixes are resolved at request
	// time, so validating against the catalog would reject ids that route fine.
	response := post("  ag/claude-opus-4-6-thinking  ")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ag/claude-opus-4-6-thinking") {
		t.Fatalf("save = %d %q", response.Code, response.Body.String())
	}
	if len(saved) != 1 || saved[0] != "ag/claude-opus-4-6-thinking" {
		t.Fatalf("saved = %v, want the trimmed id", saved)
	}

	// Empty turns the override off rather than being rejected as missing input.
	response = post("")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "plan turns use the requested model") {
		t.Fatalf("clear = %d %q", response.Code, response.Body.String())
	}
	if len(saved) != 2 || saved[1] != "" {
		t.Fatalf("saved = %v, want an empty second write", saved)
	}

	if response := post(strings.Repeat("m", 129)); response.Code != http.StatusBadRequest {
		t.Fatalf("oversized id = %d, want 400", response.Code)
	}
	if response := post("a\nb"); response.Code != http.StatusBadRequest {
		t.Fatalf("id with a newline = %d, want 400", response.Code)
	}

	// Cross-origin writes must be refused like every other mutation.
	form := url.Values{"plan_model": {"x"}}
	cross := httptest.NewRequest(http.MethodPost, "/ui/routing", strings.NewReader(form.Encode()))
	cross.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cross.Header.Set("Origin", "http://evil.example")
	cross.Host = "example.com"
	crossRecorder := httptest.NewRecorder()
	e.ServeHTTP(crossRecorder, cross)
	if crossRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d, want 403", crossRecorder.Code)
	}
}

func TestRoutingHandlerSavesCompactModelAndContextMode(t *testing.T) {
	var savedCompact []string
	var savedModes []string
	service, err := New(
		pool.New([]pool.Account{{ID: "account", Provider: "codex", Enabled: true, Weight: 1}}),
		"api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{},
		SettingsHooks{
			GetPlanModel: func(context.Context) (string, error) { return "", nil },
			SetPlanModel: func(context.Context, string) error { return nil },
			GetCompactModel: func(context.Context) (string, error) {
				if len(savedCompact) == 0 {
					return "", nil
				}
				return savedCompact[len(savedCompact)-1], nil
			},
			SetCompactModel: func(_ context.Context, value string) error {
				savedCompact = append(savedCompact, value)
				return nil
			},
			GetContextMode: func(context.Context) (string, error) { return "safe", nil },
			SetContextMode: func(_ context.Context, mode string) error {
				savedModes = append(savedModes, mode)
				return nil
			},
		}, UsageHooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	post := func(form url.Values) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/ui/routing", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", "http://example.com")
		request.Host = "example.com"
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)
		return recorder
	}

	response := post(url.Values{"compact_model": {" cx/fast-model "}, "context_mode": {"aggressive"}})
	if response.Code != http.StatusOK {
		t.Fatalf("save = %d %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "compact → cx/fast-model at medium effort") ||
		!strings.Contains(response.Body.String(), "proxy compaction aggressive") {
		t.Fatalf("flash = %q", response.Body.String())
	}
	if len(savedCompact) != 1 || savedCompact[0] != "cx/fast-model" {
		t.Fatalf("saved compact = %v", savedCompact)
	}
	if len(savedModes) != 1 || savedModes[0] != "aggressive" {
		t.Fatalf("saved modes = %v", savedModes)
	}

	// Clearing the model turns the route off; an absent mode field changes nothing.
	response = post(url.Values{"compact_model": {""}})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "compacts stay on the session model") {
		t.Fatalf("clear = %d %q", response.Code, response.Body.String())
	}
	if len(savedModes) != 1 {
		t.Fatalf("absent mode field still wrote a mode: %v", savedModes)
	}

	if response := post(url.Values{"context_mode": {"bogus"}}); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode = %d, want 400", response.Code)
	}
}

func TestRoutingUnavailableWithoutHook(t *testing.T) {
	service, err := New(
		pool.New([]pool.Account{{ID: "account", Provider: "codex", Enabled: true, Weight: 1}}),
		"api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"plan_model": {"x"}}
	request := httptest.NewRequest(http.MethodPost, "/ui/routing", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestCLITabRendersRoutingAndSubagentFields(t *testing.T) {
	service, err := New(
		pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{},
		SettingsHooks{
			GetPlanModel: func(context.Context) (string, error) { return "cc/claude-opus-5", nil },
			SetPlanModel: func(context.Context, string) error { return nil },
		}, UsageHooks{}, "fast",
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	cli := httptest.NewRecorder()
	e.ServeHTTP(cli, httptest.NewRequest(http.MethodGet, "/ui/cli", nil))
	body := cli.Body.String()
	if cli.Code != http.StatusOK {
		t.Fatalf("cli = %d", cli.Code)
	}
	for _, marker := range []string{
		// The plan model is LiteRouter's own routing, posted to its own endpoint rather
		// than bundled into the form that writes ~/.claude/settings.json.
		`hx-post="/ui/routing"`,
		`name="plan_model" value="cc/claude-opus-5"`,
		// Subagent writes CLAUDE_CODE_SUBAGENT_MODEL, and its picker key must not collide
		// with the Codex card's own subagent field.
		`name="subagent_model"`,
		`data-model-input="claude_subagent_model"`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("cli tab missing %q in %s", marker, body)
		}
	}
	// A nested form would be invalid HTML and browsers drop the inner one, so the plan
	// form has to close the setup form first.
	setupEnd := strings.Index(body, `</form>`)
	planStart := strings.Index(body, `hx-post="/ui/routing"`)
	if setupEnd < 0 || planStart < 0 || setupEnd > planStart {
		t.Fatalf("plan form is not a sibling of the setup form: setupEnd=%d planStart=%d", setupEnd, planStart)
	}
}

// cliDraftStore is the in-memory stand-in for the settings row main.go persists.
type cliDraftStore struct {
	draft clisetup.Draft
	saves int
}

func newDraftService(t *testing.T, store *cliDraftStore) (*Service, *echo.Echo) {
	t.Helper()
	service, err := New(
		pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{},
		SettingsHooks{
			GetCLIDraft: func(context.Context) (clisetup.Draft, error) { return store.draft, nil },
			SetCLIDraft: func(_ context.Context, draft clisetup.Draft) error {
				store.draft = draft
				store.saves++
				return nil
			},
		}, UsageHooks{}, "fast",
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	return service, e
}

func postCLISetup(t *testing.T, e *echo.Echo, action string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/ui/setup/claude/"+action, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	return recorder
}

func TestResetKeepsTheCLIDraft(t *testing.T) {
	// CLAUDE_CONFIG_DIR points the host writes at a temp dir so the real ~/.claude is
	// never touched, and so "stock config" is a state this test can actually produce.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	store := &cliDraftStore{}
	_, e := newDraftService(t, store)

	form := url.Values{
		"base_url": {"http://127.0.0.1:8317"}, "model": {"cc/cheap"},
		"subagent_model": {"cc/mid"}, "haiku_model": {"cc/tiny"},
	}
	if response := postCLISetup(t, e, "apply", form); response.Code != http.StatusOK {
		t.Fatalf("apply = %d %q", response.Code, response.Body.String())
	}
	if store.draft.Model != "cc/cheap" || store.draft.SubagentModel != "cc/mid" || store.draft.HaikuModel != "cc/tiny" {
		t.Fatalf("draft after apply = %#v", store.draft)
	}
	// The token must never reach the draft: it comes from the running instance, and a
	// copy on disk would be a credential stored for no reason.
	encoded, err := json.Marshal(store.draft)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "api-token") {
		t.Fatalf("draft carries the auth token: %s", encoded)
	}

	response := postCLISetup(t, e, "reset", form)
	if response.Code != http.StatusOK {
		t.Fatalf("reset = %d %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "selection kept here") {
		t.Fatalf("reset flash does not say the selection survived: %q", response.Body.String())
	}
	// The whole point: the host client is back to stock, the selection is still here.
	if store.draft.Model != "cc/cheap" || store.draft.SubagentModel != "cc/mid" {
		t.Fatalf("draft after reset = %#v", store.draft)
	}
}

func TestResetFromABlankFormKeepsTheStoredDraft(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	store := &cliDraftStore{draft: clisetup.Draft{Model: "cc/cheap"}}
	_, e := newDraftService(t, store)

	// An empty submission must not overwrite a good draft with nothing.
	if response := postCLISetup(t, e, "reset", url.Values{}); response.Code != http.StatusOK {
		t.Fatalf("reset = %d %q", response.Code, response.Body.String())
	}
	if store.draft.Model != "cc/cheap" {
		t.Fatalf("draft = %#v, want the stored selection untouched", store.draft)
	}
	if store.saves != 0 {
		t.Fatalf("saves = %d, want no write for an empty draft", store.saves)
	}
}

func TestCLITabFallsBackToDraftWhenHostConfigIsStock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	store := &cliDraftStore{draft: clisetup.Draft{Model: "cc/cheap", SubagentModel: "cc/mid"}}
	_, e := newDraftService(t, store)

	cli := httptest.NewRecorder()
	e.ServeHTTP(cli, httptest.NewRequest(http.MethodGet, "/ui/cli", nil))
	body := cli.Body.String()
	if cli.Code != http.StatusOK {
		t.Fatalf("cli = %d", cli.Code)
	}
	for _, marker := range []string{`value="cc/cheap"`, `value="cc/mid"`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("cli tab did not render the draft: missing %q", marker)
		}
	}
	// Values that are remembered but not in force must not read as active.
	if !strings.Contains(body, "saved · not applied") || strings.Contains(body, `<span class="tag ok">active</span>`) {
		t.Fatalf("cli tab claims the draft is applied: %s", body)
	}
}

func TestCLITabRendersEffortSelect(t *testing.T) {
	store := &cliDraftStore{draft: clisetup.Draft{Model: "cx/cheap", Effort: "xhigh"}}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, e := newDraftService(t, store)

	cli := httptest.NewRecorder()
	e.ServeHTTP(cli, httptest.NewRequest(http.MethodGet, "/ui/cli", nil))
	body := cli.Body.String()
	if cli.Code != http.StatusOK {
		t.Fatalf("cli = %d", cli.Code)
	}
	if !strings.Contains(body, `name="effort"`) {
		t.Fatalf("effort select missing: %s", body)
	}
	// The stored level must come back selected, and "keep current" must not be.
	if !strings.Contains(body, `<option value="xhigh" selected>`) {
		t.Fatalf("stored effort not selected: %s", body)
	}
	if strings.Contains(body, `<option value="" selected>`) {
		t.Fatalf(`"keep current" selected despite a stored effort: %s`, body)
	}
	if !strings.Contains(body, `<option value="max">max</option>`) {
		t.Fatal("effort select omits Claude Code's max level")
	}
	if strings.Contains(body, `<option value="ultracode"`) {
		t.Fatal("effort select must not persist the session-only ultracode mode")
	}
}

func TestApplyPersistsEffortIntoTheDraft(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	store := &cliDraftStore{}
	_, e := newDraftService(t, store)

	form := url.Values{
		"base_url": {"http://127.0.0.1:8317"}, "model": {"cx/cheap"}, "effort": {"xhigh"},
	}
	if response := postCLISetup(t, e, "apply", form); response.Code != http.StatusOK {
		t.Fatalf("apply = %d %q", response.Code, response.Body.String())
	}
	if store.draft.Effort != "xhigh" {
		t.Fatalf("draft effort = %q, want xhigh", store.draft.Effort)
	}

	// An invalid level must be rejected rather than written and silently ignored.
	bad := url.Values{"base_url": {"http://127.0.0.1:8317"}, "model": {"cx/cheap"}, "effort": {"ultracode"}}
	if response := postCLISetup(t, e, "apply", bad); response.Code != http.StatusBadRequest {
		t.Fatalf("effort=ultracode returned %d, want 400", response.Code)
	}
}

// The routing form carries LiteRouter's own overrides, including the image rule. All three
// post to one endpoint because they are one decision about how turns are routed.
func TestRoutingFormCarriesTheImageRule(t *testing.T) {
	var savedModel, savedTextOnly string
	service, err := New(
		pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{},
		SettingsHooks{
			GetPlanModel: func(context.Context) (string, error) { return "", nil },
			SetPlanModel: func(context.Context, string) error { return nil },
			GetImageRoute: func(context.Context) (string, string, error) {
				return "ag/gemini-3-flash", "fpt-ai", nil
			},
			SetImageRoute: func(_ context.Context, model, textOnly string) error {
				savedModel, savedTextOnly = model, textOnly
				return nil
			},
		}, UsageHooks{}, "fast",
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}

	cli := httptest.NewRecorder()
	e.ServeHTTP(cli, httptest.NewRequest(http.MethodGet, "/ui/cli", nil))
	body := cli.Body.String()
	for _, marker := range []string{
		`name="image_model" value="ag/gemini-3-flash"`,
		`name="text_only_models" value="fpt-ai"`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("cli tab missing %q", marker)
		}
	}

	form := url.Values{
		"plan_model": {""}, "long_context_model": {""}, "long_context_percent": {""},
		"image_model": {" ag/gemini-3-flash "}, "text_only_models": {" fpt-ai, some/text-model "},
	}
	request := httptest.NewRequest(http.MethodPost, "/ui/routing", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save = %d %q", recorder.Code, recorder.Body.String())
	}
	if savedModel != "ag/gemini-3-flash" || savedTextOnly != "fpt-ai, some/text-model" {
		t.Fatalf("saved = %q / %q", savedModel, savedTextOnly)
	}
	if !strings.Contains(recorder.Body.String(), "→ ag/gemini-3-flash") {
		t.Fatalf("flash did not report the image route: %q", recorder.Body.String())
	}

	// Declaring text-only models with no vision model refuses those turns rather than
	// routing them, and the flash has to say so.
	form.Set("image_model", "")
	request = httptest.NewRequest(http.MethodPost, "/ui/routing", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://example.com")
	request.Host = "example.com"
	recorder = httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), "will be refused") {
		t.Fatalf("flash did not warn about refusals: %q", recorder.Body.String())
	}
}

// A comma-separated field needs the same picker every single-model field has — nobody
// remembers a catalog of model ids — but it has to add to the list rather than replace it,
// so the input carries the marker the picker branches on.
func TestTextOnlyFieldHasABrowsePicker(t *testing.T) {
	service, err := New(
		pool.New(nil), "api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{},
		SettingsHooks{
			GetPlanModel:  func(context.Context) (string, error) { return "", nil },
			SetPlanModel:  func(context.Context, string) error { return nil },
			GetImageRoute: func(context.Context) (string, string, error) { return "", "fpt-ai", nil },
		}, UsageHooks{}, "fast",
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	cli := httptest.NewRecorder()
	e.ServeHTTP(cli, httptest.NewRequest(http.MethodGet, "/ui/cli", nil))
	body := cli.Body.String()
	for _, marker := range []string{
		`data-open-model-picker="text_only_models"`,
		`data-model-input="text_only_models"`,
		// The marker is what makes the picker append instead of overwrite, and what stops
		// "Use custom" from re-adding the whole list as one entry.
		"data-model-list",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("cli tab missing %q", marker)
		}
	}
}

// An account the gateway switched off used to appear as a dark card with no explanation
// anywhere the user would look — the reason was one line in a log. Show it on the card.
func TestDisabledAccountShowsTheReason(t *testing.T) {
	accounts := pool.New([]pool.Account{
		{
			ID: "codex:dead", Provider: "codex", Label: "dead@example.com", Weight: 1,
			Enabled: false, DisabledReason: "Your session has ended. Please log in again.",
		},
		{ID: "codex:off-by-hand", Provider: "codex", Label: "manual@example.com", Weight: 1, Enabled: false},
		{ID: "codex:live", Provider: "codex", Label: "live@example.com", Weight: 1, Enabled: true},
	})
	service, err := New(accounts, "api-token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	// Both layouts, because there are two: the card grid and the compact connections list.
	// Fixing only one left the reason invisible on the page users actually open.
	for _, view := range []struct{ path, class string }{
		{"/ui/accounts", "qcard-reason"},
		{"/ui/accounts?view=connections", "conn-reason"},
	} {
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, view.path, nil))
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s = %d", view.path, recorder.Code)
		}
		if !strings.Contains(body, "Your session has ended. Please log in again.") {
			t.Fatalf("%s: the reason is missing: %s", view.path, body)
		}
		// Exactly one: an account the user turned off themselves needs no explanation, and an
		// enabled one has nothing to explain.
		if count := strings.Count(body, `class="`+view.class+`"`); count != 1 {
			t.Fatalf("%s: %s blocks = %d, want 1", view.path, view.class, count)
		}
	}
}

// Reading every card is not how lost capacity gets noticed. Three accounts whose tokens the
// provider had invalidated read as three healthy ones on this page, while the whole load
// funnelled onto the one that still worked until it started returning 502s — so the count
// belongs in the summary line, and separately from the exhausted count: an exhausted account
// recovers by itself at its reset time, a refused credential does not.
func TestSummaryCountsAccountsThatNeedSignIn(t *testing.T) {
	accounts := pool.New([]pool.Account{
		{ID: "codex:dead1", Provider: "codex", Weight: 1, Enabled: false,
			DisabledReason: "Your authentication token has been invalidated."},
		{ID: "codex:dead2", Provider: "codex", Weight: 1, Enabled: false,
			DisabledReason: "Your authentication token has been invalidated."},
		// Turned off by hand: nothing for the user to do about it, so it must not be counted.
		{ID: "codex:off-by-hand", Provider: "codex", Weight: 1, Enabled: false},
		{ID: "codex:live", Provider: "codex", Weight: 1, Enabled: true},
	})
	service, err := New(accounts, "api-token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/quota", nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(body, "2 need sign-in") {
		t.Fatalf("summary does not report the retired accounts: %s", body)
	}
}

// A dashboard response is a live reading. With no Cache-Control, no ETag and no
// Last-Modified, a browser may apply heuristic freshness and serve a reload from its cache —
// which is what made a disabled account reappear as enabled after F5, with the toggle having
// persisted correctly all along.
func TestDashboardResponsesAreNotCacheable(t *testing.T) {
	service, err := New(
		pool.New([]pool.Account{{ID: "codex:a", Provider: "codex", Enabled: true, Weight: 1}}),
		"api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/ui", "/ui/quota", "/ui/cli", "/ui/accounts", "/ui/tabs/quota"} {
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Fatalf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}
	// Assets are versioned in the URL and are the one part that should stay cacheable.
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/assets/style.css", nil))
	if got := recorder.Header().Get("Cache-Control"); strings.Contains(got, "no-store") {
		t.Fatalf("assets got %q; they are versioned and should remain cacheable", got)
	}
}

// The switch has to submit its own state, not a hidden "opposite of what was rendered" value.
// The old shape encoded an absolute state at render time, so any stale page — a cached copy, a
// second tab, an account the gateway disabled while the page sat open — made a click send the
// opposite of what the switch showed.
func TestToggleSubmitsTheSwitchStateNotARenderTimeFlip(t *testing.T) {
	service, err := New(
		pool.New([]pool.Account{
			{ID: "codex:on", Provider: "codex", Enabled: true, Weight: 1},
			{ID: "codex:off", Provider: "codex", Enabled: false, Weight: 1},
		}),
		"api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/ui/accounts", "/ui/accounts?view=connections"} {
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		body := recorder.Body.String()
		// The control carries the name, so an unchecked box submits nothing and the handler
		// reads that as disabled.
		if count := strings.Count(body, `type="checkbox" name="enabled" value="true"`); count != 2 {
			t.Fatalf("%s: named checkboxes = %d, want 2", path, count)
		}
		// And no hidden field may carry it, or the flip semantics come back.
		if strings.Contains(body, `type="hidden" name="enabled"`) {
			t.Fatalf("%s: a hidden enabled field is still present", path)
		}
		// The enabled account renders checked, the disabled one does not.
		if count := strings.Count(body, `value="true" checked`); count != 1 {
			t.Fatalf("%s: checked switches = %d, want 1", path, count)
		}
	}
}

// The dashboard is served with `script-src 'self'`, which blocks inline event handlers
// outright. That failed silently and looked exactly like a broken control: clicking the
// account switch flipped the checkbox — native browser behaviour, no JavaScript needed — while
// the handler never ran, so no request was sent and a reload showed the unchanged state.
//
// Handlers therefore have to live in app.js. This guards the whole class, not just the one
// control that was found broken.
func TestNoInlineEventHandlersUnderTheContentSecurityPolicy(t *testing.T) {
	service, err := New(
		pool.New([]pool.Account{{ID: "codex:a", Provider: "codex", Enabled: true, Weight: 1}}),
		"api-token", nil, nil, nil, nil, nil, APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatal(err)
	}
	inline := regexp.MustCompile(`(?i)\son(change|click|input|submit|load|error|focus|blur)\s*=`)
	for _, path := range []string{"/ui", "/ui/quota", "/ui/cli", "/ui/usage", "/ui/providers", "/ui/accounts", "/ui/accounts?view=connections"} {
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if found := inline.FindString(recorder.Body.String()); found != "" {
			t.Fatalf("%s carries an inline handler (%q); the CSP blocks it — bind it in app.js instead",
				path, strings.TrimSpace(found))
		}
	}
	// And the policy that makes this a rule is actually being sent.
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/quota", nil))
	if policy := recorder.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "script-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q", policy)
	}
}
