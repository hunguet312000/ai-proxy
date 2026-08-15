package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewCursorCLIDeepLink(t *testing.T) {
	link, err := newCursorCLIDeepLink("https://cursor.com")
	if err != nil {
		t.Fatalf("newCursorCLIDeepLink: %v", err)
	}
	if link.UUID == "" || link.Verifier == "" || link.Challenge == "" {
		t.Fatalf("deep link incomplete: %+v", link)
	}
	if !strings.HasPrefix(link.AuthURL, "https://cursor.com/loginDeepControl?challenge=") {
		t.Fatalf("AuthURL = %q, want loginDeepControl prefix", link.AuthURL)
	}
	if !strings.Contains(link.AuthURL, "mode=login") || !strings.Contains(link.AuthURL, "redirectTarget=cli") {
		t.Fatalf("AuthURL missing mode/redirectTarget: %q", link.AuthURL)
	}
	// Challenge must equal base64url(SHA-256(verifier)).
	digest := sha256.Sum256([]byte(link.Verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if link.Challenge != wantChallenge {
		t.Fatalf("challenge = %q, want %q", link.Challenge, wantChallenge)
	}
}

func TestCursorCLIPollToken(t *testing.T) {
	var polled int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/poll" {
			t.Fatalf("path = %q, want /auth/poll", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawQuery, "verifier=test-verifier") {
			t.Fatalf("query = %q, want verifier", r.URL.RawQuery)
		}
		polled++
		if polled == 1 {
			// First poll: not done yet.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"accessToken":  "access-token-123",
			"refreshToken": "refresh-token-456",
		})
	}))
	defer server.Close()

	provider := NewCursorCLIProvider(server.Client())
	provider.apiBase = server.URL

	// 404 then success. The first poll should be answered with the token.
	token, err := provider.pollToken(context.Background(), "test-verifier", time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("pollToken: %v", err)
	}
	if token.AccessToken != "access-token-123" || token.RefreshToken != "refresh-token-456" {
		t.Fatalf("token = %+v", token)
	}
	if token.TokenType != "Bearer" || token.LastRefreshAt.IsZero() {
		t.Fatalf("token metadata = %+v", token)
	}
}

func TestCursorCLIPollTokenExpires(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	provider := NewCursorCLIProvider(server.Client())
	provider.apiBase = server.URL

	// The flow expired before the first poll completes.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err := provider.pollToken(ctx, "v", time.Now().Add(-time.Second))
	if err == nil {
		t.Fatal("expected an expiry error")
	}
}

func TestCursorCLIAccountInfo(t *testing.T) {
	// A JWT whose payload has the Cursor identity claims.
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(
		`{"sub":"google-oauth2|user_01KYEAREQ5S3PD54NG8EBP74N0","email":"xuanhung312000@gmail.com","exp":1791944273}`,
	)) + ".signature"
	info, err := NewCursorCLIProvider(nil).AccountInfo(context.Background(), &TokenSet{AccessToken: token})
	if err != nil {
		t.Fatalf("AccountInfo: %v", err)
	}
	if info.ID != "google-oauth2|user_01KYEAREQ5S3PD54NG8EBP74N0" {
		t.Fatalf("ID = %q", info.ID)
	}
	if info.Email != "xuanhung312000@gmail.com" {
		t.Fatalf("Email = %q", info.Email)
	}
	if info.Plan != "Cursor" {
		t.Fatalf("Plan = %q", info.Plan)
	}
}

func TestCursorCLIAccountInfoInvalid(t *testing.T) {
	if _, err := NewCursorCLIProvider(nil).AccountInfo(context.Background(), &TokenSet{AccessToken: "not-a-jwt"}); err == nil {
		t.Fatal("expected an error for a non-JWT token")
	}
}

func TestCursorCLIRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/refresh" {
			t.Fatalf("path = %q, want /auth/refresh", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["grant_type"] != "refresh_token" || body["refresh_token"] != "old-refresh" {
			t.Fatalf("body = %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
		})
	}))
	defer server.Close()

	provider := NewCursorCLIProvider(server.Client())
	provider.apiBase = server.URL

	token, err := provider.Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if token.AccessToken != "new-access" || token.RefreshToken != "new-refresh" {
		t.Fatalf("token = %+v", token)
	}
}

func TestCursorCLIExchangeUnsupported(t *testing.T) {
	_, err := NewCursorCLIProvider(nil).Exchange(context.Background(), "code", "verifier")
	if err == nil {
		t.Fatal("expected Exchange to be unsupported")
	}
}
