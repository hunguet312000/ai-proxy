package oauth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"literouter/internal/pool"
	"literouter/internal/secret"
	"literouter/internal/storage"
)

func TestManagerStartCodexUsesFallbackPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		t.Skipf("port 1455 unavailable before test: %v", err)
	}
	defer listener.Close()
	fallback, err := net.Listen("tcp", "127.0.0.1:1457")
	if err != nil {
		t.Skipf("port 1457 unavailable: %v", err)
	}
	fallback.Close()

	manager, _ := testManager(t)
	result, err := manager.StartCodex(context.Background())
	if err != nil {
		t.Fatalf("StartCodex() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})
	authURL, _ := url.Parse(result.AuthURL)
	if got := authURL.Query().Get("redirect_uri"); got != "http://localhost:1457/auth/callback" {
		t.Fatalf("redirect_uri = %q", got)
	}
}

func TestManagerImportCodex(t *testing.T) {
	manager, store := testManager(t)
	idToken := jwt(t, map[string]any{
		"email":                       "user@example.com",
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "account", "chatgpt_plan_type": "plus"},
	})
	account, err := manager.ImportCodex(context.Background(), TokenSet{AccessToken: "access", IDToken: idToken})
	if err != nil {
		t.Fatalf("ImportCodex() error = %v", err)
	}
	if account.ID != "codex:account" {
		t.Fatalf("account = %#v", account)
	}
	stored, err := store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var token TokenSet
	if err := json.Unmarshal(stored.Credentials, &token); err != nil || token.AccessToken != "access" {
		t.Fatalf("stored token = %#v, error = %v", token, err)
	}
}

func testManager(t *testing.T) (*Manager, *storage.Store) {
	t.Helper()
	key := make([]byte, secret.KeySize)
	box, _ := secret.New(key)
	store, err := storage.Open(filepath.Join(t.TempDir(), "oauth.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store, pool.New(nil), key, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return manager, store
}

func TestManagerCallbackRejectsBadState(t *testing.T) {
	manager, _ := testManager(t)
	request := httptest.NewRequest(http.MethodGet, "http://localhost/auth/callback?state=bad", nil)
	recorder := httptest.NewRecorder()
	manager.handleCallback("codex", &http.Server{}, recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Invalid or expired OAuth state") || !strings.Contains(recorder.Body.String(), "literouter-oauth") {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestManagerSaveAccountInvokesConnectedHook(t *testing.T) {
	manager, _ := testManager(t)
	done := make(chan string, 1)
	manager.SetOnAccountConnected(func(_ context.Context, id string) { done <- id })
	idToken := jwt(t, map[string]any{
		"email":                       "user@example.com",
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "account", "chatgpt_plan_type": "plus"},
	})
	if _, err := manager.ImportCodex(context.Background(), TokenSet{AccessToken: "access", IDToken: idToken}); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-done:
		if id != "codex:account" {
			t.Fatalf("hook id = %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connected hook not called")
	}
}

func TestManagerCloseReleasesCallbackPort(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		t.Skipf("port 1455 unavailable before test: %v", err)
	}
	probe.Close()

	manager, _ := testManager(t)
	if _, err := manager.StartCodex(context.Background()); err != nil {
		t.Fatalf("StartCodex() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		listener, err := net.Listen("tcp", "127.0.0.1:1455")
		if err == nil {
			listener.Close()
			return
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("callback port remains bound: %v", last)
}
