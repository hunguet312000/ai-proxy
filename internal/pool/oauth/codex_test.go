package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseJWTClaimsReadsStandardClaims(t *testing.T) {
	token := jwt(t, map[string]any{"sub": "user", "email": "user@example.com", "exp": int64(2_000_000_000)})
	claims, err := ParseJWTClaims(token)
	if err != nil {
		t.Fatalf("ParseJWTClaims() error = %v", err)
	}
	if claims.Subject != "user" || claims.Email != "user@example.com" || claims.ExpiresAt != 2_000_000_000 {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestCodexAuthURL(t *testing.T) {
	provider := NewCodexProvider(nil)
	authURL, err := url.Parse(provider.AuthURL("state", "challenge"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	query := authURL.Query()
	checks := map[string]string{
		"client_id":                  codexClientID,
		"redirect_uri":               codexCallbackURL,
		"state":                      "state",
		"code_challenge":             "challenge",
		"code_challenge_method":      "S256",
		"codex_cli_simplified_flow":  "true",
		"id_token_add_organizations": "true",
		"originator":                 "codex_cli_rs",
	}
	for key, want := range checks {
		if got := query.Get(key); got != want {
			t.Errorf("query[%s] = %q, want %q", key, got, want)
		}
	}
}

func TestCodexExchangeAndRefresh(t *testing.T) {
	var exchanges, refreshes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.Header.Get("Content-Type") {
		case "application/x-www-form-urlencoded":
			exchanges++
			values, _ := url.ParseQuery(string(body))
			if values.Get("code") != "code" || values.Get("code_verifier") != "verifier" {
				t.Errorf("exchange form = %v", values)
			}
		case "application/json":
			refreshes++
			var values map[string]string
			_ = json.Unmarshal(body, &values)
			if values["refresh_token"] != "old-refresh" || values["grant_type"] != "refresh_token" {
				t.Errorf("refresh JSON = %v", values)
			}
		default:
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"new-refresh","id_token":"`+jwt(t, map[string]any{
			"sub": "user", "email": "user@example.com",
			"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "account", "chatgpt_plan_type": "plus"},
		})+`","expires_in":3600}`)
	}))
	defer server.Close()

	provider := NewCodexProvider(server.Client())
	provider.issuer = server.URL
	provider.redirectURL = "http://localhost:1455/auth/callback"

	token, err := provider.Exchange(context.Background(), "code", "verifier")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	info, err := provider.AccountInfo(context.Background(), token)
	if err != nil {
		t.Fatalf("AccountInfo() error = %v", err)
	}
	if info.ID != "account" || info.Email != "user@example.com" || info.Plan != "plus" {
		t.Fatalf("AccountInfo() = %#v", info)
	}
	if _, err := provider.Refresh(context.Background(), "old-refresh"); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if exchanges != 1 || refreshes != 1 {
		t.Fatalf("calls = exchange %d refresh %d", exchanges, refreshes)
	}
}

func TestCodexProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"refresh_token_reused","message":"sign in again"}}`)
	}))
	defer server.Close()
	provider := NewCodexProvider(server.Client())
	provider.issuer = server.URL

	_, err := provider.Refresh(context.Background(), "old")
	providerError, ok := errChainProvider(err)
	if !ok || !providerError.Permanent || providerError.Code != "refresh_token_reused" {
		t.Fatalf("Refresh() error = %#v", err)
	}
}

func TestCodexShouldRefresh(t *testing.T) {
	provider := NewCodexProvider(nil)
	now := time.Now().UTC()
	if !provider.ShouldRefresh(&TokenSet{RefreshToken: "x", ExpiresAt: now.Add(4 * time.Minute)}, now) {
		t.Fatal("ShouldRefresh() = false near expiry")
	}
	if provider.ShouldRefresh(&TokenSet{RefreshToken: "x", ExpiresAt: now.Add(time.Hour), LastRefreshAt: now.Add(-9 * 24 * time.Hour)}, now) {
		t.Fatal("ShouldRefresh() used last refresh despite JWT expiry")
	}
	if !provider.ShouldRefresh(&TokenSet{RefreshToken: "x", LastRefreshAt: now.Add(-9 * 24 * time.Hour)}, now) {
		t.Fatal("ShouldRefresh() = false with stale refresh and no expiry")
	}
}

func errChainProvider(err error) (*ProviderError, bool) {
	for err != nil {
		if providerError, ok := err.(*ProviderError); ok {
			return providerError, true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = unwrapper.Unwrap()
	}
	return nil, false
}

func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return strings.Join([]string{encode([]byte(`{"alg":"none"}`)), encode(payload), encode([]byte("sig"))}, ".")
}
