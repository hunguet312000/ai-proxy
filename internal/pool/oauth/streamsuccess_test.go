package oauth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"literouter/internal/pool"
	"literouter/internal/secret"
	"literouter/internal/storage"
	"literouter/internal/translator"
)

// servedStreamTransport answers any upstream call with a complete, valid stream, so the
// only thing under test is what the selector is told about the account afterwards.
type servedStreamTransport struct{ calls int }

func (t *servedStreamTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		Request:    request,
	}, nil
}

func singleAccountInference(t *testing.T) (*Inference, *pool.Selector, *servedStreamTransport) {
	t.Helper()
	box, err := secret.New(make([]byte, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "accounts.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(context.Background(), storage.Account{
		ID: "codex:live", Provider: "codex", Label: "live@example.com",
		Credentials: []byte(`{"access_token":"live"}`), Enabled: true, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// One account only: if the selector rules it out there is nothing to fall back to,
	// which is exactly the situation a Claude CLI session lands in.
	accountPool := pool.New([]pool.Account{{ID: "codex:live", Provider: "codex", Enabled: true, Weight: 1}})
	// No OAuth providers registered, so LoadFresh serves the stored token without refreshing.
	credentials := NewCredentialManager(store, accountPool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	selector := pool.NewSelector(accountPool, pool.StrategyRoundRobin, nil)
	transport := &servedStreamTransport{}
	return &Inference{
		credentials: credentials,
		selector:    selector,
		client:      &http.Client{Transport: transport},
	}, selector, transport
}

// Claude CLI is streaming end to end, and the streaming path used to report only failures:
// ReportError on every fault, ReportSuccess never. The error streak therefore survived any
// number of served turns in between, so five faults scattered across an entire session —
// a 502, a dropped connection, a transient refusal — eventually tripped the circuit breaker
// on the one account that had been working the whole time.
func TestStreamingSuccessClearsTheAccountFailureStreak(t *testing.T) {
	inference, selector, transport := singleAccountInference(t)
	request := translator.OpenAIRequest{Model: "cx/gpt-5.6-luna"}

	// Four faults: one short of the breaker, which trips at five.
	for range 4 {
		selector.ReportError("codex:live")
	}
	body, err := inference.DoStream(context.Background(), request, "conversation-1")
	if err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	_ = body.Close()
	if transport.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", transport.calls)
	}
	// Four more. With the streak cleared by the served turn this is four; without it the
	// fifth fault of the session trips the breaker for three minutes.
	for range 4 {
		selector.ReportError("codex:live")
	}

	if _, err := selector.Select(pool.SelectRequest{Provider: "codex"}); err != nil {
		t.Fatalf("the account that just served a stream is unusable: %v", err)
	}
}
