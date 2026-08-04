package oauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClaudeExchangeAndRefreshUseJSON(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if calls == 1 && (payload["code"] != "code" || payload["state"] != "state" || payload["code_verifier"] != "verifier") {
			t.Errorf("exchange payload = %#v", payload)
		}
		if calls == 2 && payload["refresh_token"] != "refresh" {
			t.Errorf("refresh payload = %#v", payload)
		}
		_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"next","expires_in":3600}`)
	}))
	defer server.Close()
	provider := NewClaudeProvider(server.Client())
	provider.tokenURL = server.URL
	if _, err := provider.Exchange(context.Background(), "code#state", "verifier"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Refresh(context.Background(), "refresh"); err != nil {
		t.Fatal(err)
	}
}

func TestAntigravityOnboardsWhenProjectMissing(t *testing.T) {
	var onboardCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/load":
			_, _ = io.WriteString(w, `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`)
		case "/onboard":
			onboardCalls++
			if onboardCalls == 1 {
				_, _ = io.WriteString(w, `{"done":false}`)
				return
			}
			_, _ = io.WriteString(w, `{"done":true,"response":{"cloudaicompanionProject":{"id":"project-1"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider := NewAntigravityProvider(server.Client())
	provider.loadCodeAssistURL = server.URL + "/load"
	provider.onboardUserURL = server.URL + "/onboard"
	provider.onboardPollDelay = time.Millisecond
	projectID, err := provider.loadProject(context.Background(), "token")
	if err != nil || projectID != "project-1" || onboardCalls != 2 {
		t.Fatalf("loadProject() = %q, %v; calls = %d", projectID, err, onboardCalls)
	}
}

func TestAntigravityLoadProjectUsesExistingProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-existing"}`)
	}))
	defer server.Close()
	provider := NewAntigravityProvider(server.Client())
	provider.loadCodeAssistURL = server.URL
	provider.onboardUserURL = server.URL + "/unexpected"
	projectID, err := provider.loadProject(context.Background(), "token")
	if err != nil || projectID != "project-existing" {
		t.Fatalf("loadProject() = %q, %v", projectID, err)
	}
}

func TestGrokAuthorizeFlow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth2/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "auth-code" || r.Form.Get("code_verifier") != "verifier" {
			t.Fatalf("form = %#v", r.Form)
		}
		if r.Form.Get("redirect_uri") != "http://127.0.0.1:1456/callback" {
			t.Fatalf("redirect_uri = %q", r.Form.Get("redirect_uri"))
		}
		_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","id_token":"`+jwt(t, map[string]any{"sub": "grok-user", "email": "user@x.ai"})+`","expires_in":3600}`)
	}))
	defer server.Close()

	provider := NewGrokProvider(server.Client())
	provider.issuer = server.URL
	authURL := provider.AuthURL("state", "challenge")
	if !strings.Contains(authURL, "/oauth2/authorize?") || !strings.Contains(authURL, "code_challenge=challenge") || !strings.Contains(authURL, "client_id="+grokClientID) {
		t.Fatalf("AuthURL = %q", authURL)
	}
	token, err := provider.Exchange(context.Background(), "auth-code", "verifier")
	if err != nil || token.AccessToken != "access" {
		t.Fatalf("Exchange() = %#v, %v", token, err)
	}
	info, err := provider.AccountInfo(context.Background(), token)
	if err != nil || info.ID != "grok-user" || info.Email != "user@x.ai" || info.Plan != "xAI" {
		t.Fatalf("AccountInfo() = %#v, %v", info, err)
	}
}
