package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"literouter/internal/provider"
	"literouter/internal/translator"
)

func rejection(status int, message string) error {
	return &provider.ProviderError{Provider: "test", StatusCode: status, Message: message}
}

func TestParseOutputLimitRejection(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		err       error
		want      int
	}{
		{
			name: "anthropic phrasing", requested: 64000, want: 32000,
			err: rejection(400, "max_tokens: 64000 > 32000, which is the maximum allowed number of output tokens for claude-opus-4-8"),
		},
		{
			name: "openai phrasing", requested: 32000, want: 16384,
			err: rejection(400, "max_tokens is too large: 32000. This model supports at most 16384 completion tokens"),
		},
		{
			name: "range phrasing", requested: 32000, want: 8192,
			err: rejection(400, "max_tokens must be at least 1 and at most 8192"),
		},
		{
			name: "max_completion_tokens naming", requested: 32000, want: 4096,
			err: rejection(422, "invalid max_completion_tokens: model accepts up to 4096"),
		},
		{
			// The dated id and the version fragments must not be mistaken for caps.
			name: "digits in the model id are ignored", requested: 32000, want: 16384,
			err: rejection(400, "max_tokens 32000 exceeds the output tokens limit of 16384 for gpt-5.6-sol-20251001"),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := parseOutputLimitRejection(testCase.err, testCase.requested)
			if !ok || got != testCase.want {
				t.Fatalf("parseOutputLimitRejection = %d, %v; want %d, true", got, ok, testCase.want)
			}
		})
	}
}

func TestParseOutputLimitRejectionRefuses(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		err       error
	}{
		{
			// The single most dangerous confusion: clamping max_tokens would not shorten
			// the prompt, and the client would stop compacting because overflow stopped
			// being reported.
			name: "context overflow is not an output cap", requested: 64000,
			err: rejection(400, "prompt is too long: 500000 tokens > 200000 tokens"),
		},
		{
			name: "context overflow by code", requested: 64000,
			err: &provider.ProviderError{StatusCode: 400, Code: "context_length_exceeded",
				Message: "max_tokens 64000 is over the 8192 limit"},
		},
		{
			name: "unrelated bad request", requested: 64000,
			err: rejection(400, "tool 'Read' has an invalid input schema"),
		},
		{
			name: "no number below the request", requested: 64000,
			err: rejection(400, "max_tokens is too large: 64000"),
		},
		{
			name: "only implausibly small numbers", requested: 64000,
			err: rejection(400, "max_tokens must be at most 8 output tokens"),
		},
		{
			name: "server error carries no cap", requested: 64000,
			err: rejection(500, "max_tokens: internal output tokens failure 4096"),
		},
		{
			name: "not a provider error", requested: 64000, err: errors.New("max_tokens at most 4096"),
		},
		{
			name: "request already at or below the floor", requested: 128,
			err: rejection(400, "max_tokens must be at most 64 completion tokens"),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got, ok := parseOutputLimitRejection(testCase.err, testCase.requested); ok {
				t.Fatalf("parseOutputLimitRejection = %d, true; want refusal", got)
			}
		})
	}
}

func TestIntegersIn(t *testing.T) {
	got := integersIn("gpt-5.6-sol max_tokens 64000 > 32000")
	want := []int{5, 6, 64000, 32000}
	if len(got) != len(want) {
		t.Fatalf("integersIn = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("integersIn = %v, want %v", got, want)
		}
	}
}

func TestRecordOutputLimitOnlyLowers(t *testing.T) {
	var persisted []int
	service := New(Options{OnOutputLimit: func(_ string, limit int) {
		persisted = append(persisted, limit)
	}})

	if !service.recordOutputLimit("cheap", 8192) {
		t.Fatal("first observation was not recorded")
	}
	if got := service.outputLimit("cheap"); got != 8192 {
		t.Fatalf("outputLimit = %d, want 8192", got)
	}
	// A higher cap means something else answered under this name; believing it walks
	// straight back into rejections.
	if service.recordOutputLimit("cheap", 16384) {
		t.Fatal("a higher observation was accepted")
	}
	if got := service.outputLimit("cheap"); got != 8192 {
		t.Fatalf("outputLimit = %d, want 8192", got)
	}
	if !service.recordOutputLimit("cheap", 4096) {
		t.Fatal("a lower observation was rejected")
	}
	if got := service.outputLimit("cheap"); got != 4096 {
		t.Fatalf("outputLimit = %d, want 4096", got)
	}
	if len(persisted) != 2 || persisted[0] != 8192 || persisted[1] != 4096 {
		t.Fatalf("persisted = %v, want [8192 4096]", persisted)
	}
}

func TestConfiguredLimitBeatsLearned(t *testing.T) {
	service := New(Options{
		MaxOutputTokens:     map[string]int{"pinned": 20000},
		LearnedOutputTokens: map[string]int{"pinned": 4096, "observed": 8192},
	})
	// Configuration is an explicit human decision; learning must not override it.
	if got := service.outputLimit("pinned"); got != 20000 {
		t.Fatalf("outputLimit(pinned) = %d, want 20000", got)
	}
	if got := service.outputLimit("observed"); got != 8192 {
		t.Fatalf("outputLimit(observed) = %d, want 8192", got)
	}
}

func TestStreamOpenerLearnsAndRewindsWithinBudget(t *testing.T) {
	service := New(Options{})
	opener := &streamOpener{service: service, models: []string{"cheap"}, index: 1}

	if !opener.retryAfterLearningOutputLimit("cheap", 32000, rejection(http.StatusBadRequest,
		"max_tokens is too large: 32000. This model supports at most 8192 completion tokens")) {
		t.Fatal("first rejection did not produce a re-attempt")
	}
	if opener.index != 0 {
		t.Fatalf("index = %d, want 0 so the same candidate is retried", opener.index)
	}
	if got := service.outputLimit("cheap"); got != 8192 {
		t.Fatalf("outputLimit = %d, want 8192", got)
	}

	opener.index = 1
	if !opener.retryAfterLearningOutputLimit("cheap", 8192,
		rejection(http.StatusBadRequest, "max_tokens is too large: 8192, at most 4096 completion tokens")) {
		t.Fatal("second rejection did not produce a re-attempt")
	}
	if got := service.outputLimit("cheap"); got != 4096 {
		t.Fatalf("outputLimit = %d, want 4096", got)
	}

	// Budget spent. Past this the cap being derived is wrong, and each further attempt
	// re-uploads the whole conversation.
	opener.index = 1
	if opener.retryAfterLearningOutputLimit("cheap", 4096,
		rejection(http.StatusBadRequest, "max_tokens is too large: 4096, at most 2048 completion tokens")) {
		t.Fatal("re-attempts continued past the budget")
	}
	if opener.index != 1 {
		t.Fatalf("index = %d, want 1 left untouched", opener.index)
	}
}

func TestLearnOutputLimitStepsDownWhenNoNumberIsNamed(t *testing.T) {
	// The case the ladder exists for: the upstream says the budget is the problem but
	// never says what it accepts, so there is nothing to parse.
	vague := rejection(http.StatusBadRequest, "max_tokens exceeds the limit for this model")

	service := New(Options{})
	if !service.learnOutputLimit("mystery", 32000, 0, vague) {
		t.Fatal("first step down was refused")
	}
	if got := service.outputLimit("mystery"); got != 8192 {
		t.Fatalf("outputLimit = %d, want the first ladder rung 8192", got)
	}
	if !service.learnOutputLimit("mystery", 8192, 1, vague) {
		t.Fatal("second step down was refused")
	}
	if got := service.outputLimit("mystery"); got != 2048 {
		t.Fatalf("outputLimit = %d, want the second ladder rung 2048", got)
	}
	if service.learnOutputLimit("mystery", 2048, 2, vague) {
		t.Fatal("the ladder continued past its last rung")
	}

	// Rungs at or above the request are skipped: re-sending the same budget is not a
	// different request.
	small := New(Options{})
	if !small.learnOutputLimit("tiny", 4096, 0, vague) {
		t.Fatal("step down was refused for a small request")
	}
	if got := small.outputLimit("tiny"); got != 2048 {
		t.Fatalf("outputLimit = %d, want 2048 with 8192 skipped as too high", got)
	}

	// A rejection that is not about the output budget must never trigger the ladder.
	unrelated := New(Options{})
	if unrelated.learnOutputLimit("tiny", 32000, 0,
		rejection(http.StatusBadRequest, "tool 'Read' has an invalid input schema")) {
		t.Fatal("an unrelated rejection triggered a step down")
	}
}

func TestNextFallbackOutputLimit(t *testing.T) {
	cases := []struct {
		requested, want int
		ok              bool
	}{
		{requested: 32000, want: 8192, ok: true},
		{requested: 8192, want: 2048, ok: true}, // 8192 is not strictly below 8192
		{requested: 4096, want: 2048, ok: true},
		{requested: 2048, ok: false}, // nothing on the ladder is lower
		{requested: 0, ok: false},
	}
	for _, testCase := range cases {
		got, ok := nextFallbackOutputLimit(testCase.requested)
		if ok != testCase.ok || (ok && got != testCase.want) {
			t.Errorf("nextFallbackOutputLimit(%d) = %d, %v; want %d, %v",
				testCase.requested, got, ok, testCase.want, testCase.ok)
		}
	}
}

// recordingPassthroughInference captures each Anthropic payload and can reject the
// first N attempts the way a real upstream does.
type recordingPassthroughInference struct {
	rejectUntil int
	limit       int
	payloads    [][]byte
}

func (f *recordingPassthroughInference) DoJSON(context.Context, translator.OpenAIRequest, string) (translator.OpenAIResponse, error) {
	return translator.OpenAIResponse{}, errors.New("unused")
}

func (f *recordingPassthroughInference) DoStream(context.Context, translator.OpenAIRequest, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func (f *recordingPassthroughInference) SupportsAnthropicPassthrough(model string) bool {
	return strings.HasPrefix(model, "claude")
}

func (f *recordingPassthroughInference) DoAnthropicStream(_ context.Context, payload []byte, _, _, _ string) (io.ReadCloser, error) {
	f.payloads = append(f.payloads, payload)
	if len(f.payloads) <= f.rejectUntil {
		return nil, rejection(http.StatusBadRequest, fmt.Sprintf(
			"max_tokens: 64000 > %d, which is the maximum allowed number of output tokens for claude-cheap", f.limit))
	}
	return io.NopCloser(strings.NewReader(anthropicUpstreamStream)), nil
}

func payloadMaxTokens(t *testing.T, payload []byte) int {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return decodeAnthropicMaxTokens(fields)
}

const passthroughBody = `{"model":"claude-cheap","max_tokens":64000,"stream":true,` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`

func TestPassthroughAppliesConfiguredOutputCap(t *testing.T) {
	oauth := &recordingPassthroughInference{}
	service := New(Options{OAuthInference: oauth, MaxOutputTokens: map[string]int{"claude-cheap": 8192}})

	if recorder := postAnthropic(t, service, passthroughBody); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	// A configured cap is a decision, and it has to hold on the byte-passthrough path
	// too rather than only on the translated one.
	if got := payloadMaxTokens(t, oauth.payloads[0]); got != 8192 {
		t.Fatalf("payload max_tokens = %d, want 8192", got)
	}
}

func TestPassthroughLearnsAndRetriesTransparently(t *testing.T) {
	oauth := &recordingPassthroughInference{rejectUntil: 1, limit: 32000}
	var persisted []int
	service := New(Options{OAuthInference: oauth,
		OnOutputLimit: func(_ string, limit int) { persisted = append(persisted, limit) }})

	recorder := postAnthropic(t, service, passthroughBody)
	// The client must never see the rejection: it was absorbed before any byte was
	// written, and the retry carried the cap the upstream just revealed.
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "event: message_stop") {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if len(oauth.payloads) != 2 {
		t.Fatalf("upstream attempts = %d, want 2", len(oauth.payloads))
	}
	if got := payloadMaxTokens(t, oauth.payloads[0]); got != 64000 {
		t.Fatalf("first attempt max_tokens = %d, want the caller's 64000", got)
	}
	if got := payloadMaxTokens(t, oauth.payloads[1]); got != 32000 {
		t.Fatalf("retry max_tokens = %d, want the learned 32000", got)
	}
	if len(persisted) != 1 || persisted[0] != 32000 {
		t.Fatalf("persisted = %v, want [32000]", persisted)
	}
}

func TestPassthroughStopsAfterItsRetryBudget(t *testing.T) {
	// An upstream that keeps rejecting whatever it is sent must surface the error rather
	// than re-upload the conversation indefinitely.
	oauth := &recordingPassthroughInference{rejectUntil: 99, limit: 32000}
	service := New(Options{OAuthInference: oauth})

	recorder := postAnthropic(t, service, passthroughBody)
	if recorder.Code == http.StatusOK {
		t.Fatalf("a permanently rejecting upstream produced a success: %q", recorder.Body.String())
	}
	if len(oauth.payloads) > 1+maxOutputLimitAttempts {
		t.Fatalf("upstream attempts = %d, want at most %d", len(oauth.payloads), 1+maxOutputLimitAttempts)
	}
}
