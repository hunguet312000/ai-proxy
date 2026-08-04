package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
)

func adviceHeaders(t *testing.T, status int, err error) http.Header {
	t.Helper()
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), recorder)
	if writeErr := anthropicError(c, status, "boom", "api_error", err); writeErr != nil {
		t.Fatalf("anthropicError: %v", writeErr)
	}
	return recorder.Header()
}

func TestRetryAdviceSuppressesFutileRetries(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    error
	}{
		{"provider unavailable", http.StatusServiceUnavailable, ErrProviderUnavailable},
		{"context budget exceeded", http.StatusBadRequest, contextguard.ErrBudgetExceeded},
		{"upstream context rejection", http.StatusBadRequest,
			&provider.ProviderError{StatusCode: 400, Code: "context_length_exceeded"}},
		{"client cancelled", 499, context.Canceled},
		{"unauthorized", http.StatusUnauthorized, nil},
		{"not found", http.StatusNotFound, nil},
		{"unprocessable", http.StatusUnprocessableEntity, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := adviceHeaders(t, testCase.status, testCase.err).Get(headerShouldRetry); got != "false" {
				t.Fatalf("%s = %q, want false", headerShouldRetry, got)
			}
		})
	}
}

func TestRetryAdviceLeavesTransientFailuresToTheClient(t *testing.T) {
	// Anything LiteRouter cannot prove is deterministic keeps the SDK's own defaults:
	// suppressing a retry that would have worked loses the turn outright.
	cases := []struct {
		name   string
		status int
		err    error
	}{
		{"upstream bad gateway", http.StatusBadGateway, errors.New("upstream request failed")},
		{"stream idle timeout", http.StatusGatewayTimeout, errUpstreamStreamIdle},
		{"rate limited", http.StatusTooManyRequests, &provider.ProviderError{StatusCode: 429}},
		{"conflict", http.StatusConflict, nil},
		{"request timeout", http.StatusRequestTimeout, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := adviceHeaders(t, testCase.status, testCase.err).Get(headerShouldRetry); got != "" {
				t.Fatalf("%s = %q, want unset", headerShouldRetry, got)
			}
		})
	}
}

func TestRetryAdviceForwardsUpstreamCooldown(t *testing.T) {
	header := adviceHeaders(t, http.StatusTooManyRequests,
		&provider.ProviderError{StatusCode: 429, RetryAfter: 3 * time.Second})
	if got := header.Get(headerRetryAfter); got != "3000" {
		t.Fatalf("%s = %q, want 3000", headerRetryAfter, got)
	}

	// No hint from the upstream means no header: an invented delay is worse than the
	// client's own backoff, which at least widens on repeat failures.
	header = adviceHeaders(t, http.StatusTooManyRequests, &provider.ProviderError{StatusCode: 429})
	if got := header.Get(headerRetryAfter); got != "" {
		t.Fatalf("%s = %q, want unset", headerRetryAfter, got)
	}
}
