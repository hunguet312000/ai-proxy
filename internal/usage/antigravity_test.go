package usage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseAntigravityQuota(t *testing.T) {
	quota, err := ParseAntigravityQuota([]byte(`{
		"models": {
			"gemini-pro": {"displayName":"Gemini Pro","quotaInfo":{"remainingFraction":0.25,"resetTime":"2026-07-25T12:00:00Z"}},
			"gemini-flash": {"model":"gemini-3-flash","quotaInfo":{"remainingFraction":1}}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(quota.Windows) != 2 {
		t.Fatalf("windows = %#v", quota.Windows)
	}
	if quota.Windows[0].Key != "gemini-3-flash" || quota.Windows[0].RemainingPercent != 100 {
		t.Fatalf("flash window = %#v", quota.Windows[0])
	}
	if quota.Windows[1].Key != "gemini-pro" || quota.Windows[1].RemainingPercent != 25 {
		t.Fatalf("pro window = %#v", quota.Windows[1])
	}
	wantReset := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	if !quota.Windows[1].ResetAt.Equal(wantReset) {
		t.Fatalf("reset = %v, want %v", quota.Windows[1].ResetAt, wantReset)
	}
}

func TestAntigravityFetcher(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("request = %s, auth = %q", r.Method, r.Header.Get("Authorization"))
		}
		if r.Header.Get("Client-Metadata") == "" || r.Header.Get("x-request-source") != "local" {
			t.Fatalf("Antigravity headers missing: %#v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"project":"project-1"`) {
			t.Fatalf("body = %s", body)
		}
		_, _ = w.Write([]byte(`{"models":{"model-a":{"quotaInfo":{"remainingFraction":0}}}}`))
	}))
	defer server.Close()

	fetcher := NewAntigravityFetcher(server.Client())
	fetcher.url = server.URL
	quota, err := fetcher.Fetch(context.Background(), "token", "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(quota.Windows) != 1 || !quota.Windows[0].Exhausted {
		t.Fatalf("quota = %#v", quota)
	}
}
