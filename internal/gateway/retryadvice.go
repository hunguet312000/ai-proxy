package gateway

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/contextguard"
	"literouter/internal/provider"
)

// Claude Code retries through the Anthropic SDK, whose decision is:
//
//	x-should-retry: "true"  -> retry
//	x-should-retry: "false" -> do not retry
//	otherwise               -> retry on 408, 409, 429 and any 5xx
//
// and whose delay is read from retry-after-ms (float milliseconds), else
// retry-after (float seconds or an HTTP date), else its own exponential backoff.
//
// Both matter more through a proxy than they do against the real API. A retry
// re-uploads the entire conversation, so on a large session one pointless retry
// costs more input tokens than the turn itself — and by the time an error reaches
// the client, LiteRouter has already walked its whole candidate chain and taken its
// own backoff waves. What the client can still usefully retry is a narrower set
// than the status code alone suggests.
const (
	headerShouldRetry = "x-should-retry"
	headerRetryAfter  = "retry-after-ms"
)

// applyRetryAdvice annotates an error response with the client's retry decision.
//
// It only ever narrows: "false" is set where re-sending the identical request
// cannot change the outcome. Transient failures — timeouts, empty upstream
// responses, upstream 5xx — are deliberately left to the client's own defaults,
// because LiteRouter cannot tell a blip that outlasted its retry waves from one
// that did not, and losing a recoverable turn is worse than paying for a retry.
func applyRetryAdvice(c echo.Context, status int, err error) {
	if delay, ok := upstreamRetryAfter(err); ok {
		c.Response().Header().Set(headerRetryAfter, strconv.FormatInt(delay.Milliseconds(), 10))
	}
	if !retryIsFutile(status, err) {
		return
	}
	c.Response().Header().Set(headerShouldRetry, "false")
}

// retryIsFutile reports whether an identical retry is provably wasted.
func retryIsFutile(status int, err error) bool {
	switch {
	case errors.Is(err, context.Canceled):
		// The client hung up; nothing is listening for a second attempt.
		return true
	case errors.Is(err, contextguard.ErrBudgetExceeded), isContextOverflow(err):
		// Deterministic in the request itself. The client must compact, not retry.
		return true
	case errors.Is(err, ErrProviderUnavailable):
		// No upstream is configured for this model. Retrying waits for a config change
		// that a retry cannot cause, and 503 would otherwise be retried by default.
		return true
	}
	// Deterministic client-side statuses. 408, 409 and 429 are excluded: those are the
	// 4xx the SDK retries on purpose, and it is right to.
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired,
		http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// upstreamRetryAfter reports the cooldown the upstream itself asked for, so a
// rate-limited account is not hammered on the client's generic backoff schedule.
func upstreamRetryAfter(err error) (time.Duration, bool) {
	var providerError *provider.ProviderError
	if !errors.As(err, &providerError) || providerError.RetryAfter <= 0 {
		return 0, false
	}
	return providerError.RetryAfter, true
}
