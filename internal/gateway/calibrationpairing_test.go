package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"literouter/internal/contextguard"
	"literouter/internal/provider"
)

// reportingStreamClient serves one turn and reports a fixed prompt count, the way an
// upstream reports what it counted for the payload it was handed.
type reportingStreamClient struct {
	reported  int
	sentBytes int
}

func (c *reportingStreamClient) DoStream(_ context.Context, _ string, body any) (io.ReadCloser, error) {
	encoded, _ := json.Marshal(body)
	c.sentBytes = len(encoded)
	return io.NopCloser(strings.NewReader(fmt.Sprintf(
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],"+
			"\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n", c.reported))), nil
}

// The bug: the estimate fed to calibration described the payload the client sent, while
// the reported count described the smaller payload the pipeline sent after compacting it.
// The ratio between the two is the compaction factor, and learning it as a tokenizer
// ratio inflated the scale, inflated the budget the guard trusted, and held spread too
// high for the measurement to ever pass the confidence gate.
//
// The counts here are chosen so the poisoned sample would have been *accepted* rather
// than clamped away: that is the case that silently corrupts the ratio.
func TestCalibrationLearnsFromThePayloadThatWasSent(t *testing.T) {
	payload := subagentPayload("cx/gpt-5.6-luna", 30, 6_000, 1024)
	clientEstimate := contextguard.EstimateRequest(mustUnify(t, payload))
	// A quarter of the client's view: the pre-pipeline ratio lands at 4.0, inside the
	// [0.25, 6] band observeTokenScale accepts.
	reported := clientEstimate / 4
	client := &reportingStreamClient{reported: reported}
	service := New(Options{
		OpenAIStream:   client,
		ContextEnabled: true,
		ContextPolicy:  contextguard.AggressivePolicy(contextguard.DefaultPolicy()),
		ContextWindow:  func(context.Context, string) (int, error) { return 20_000, nil },
	})
	e := echo.New()
	service.Register(e)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if client.sentBytes == 0 || client.sentBytes >= len(payload) {
		t.Fatalf("pipeline did not rewrite the payload: sent %d of %d bytes", client.sentBytes, len(payload))
	}

	scale := service.tokenScaleFor("cx/gpt-5.6-luna")
	if scale.samples != 1 {
		t.Fatalf("samples = %d, want exactly one sample from this turn", scale.samples)
	}
	// 4.0 is what the client's own view of the turn would have taught.
	if scale.estimatePerToken > 2.5 {
		t.Fatalf("estimate_per_token = %.2f, too close to the 4.0 the pre-pipeline estimate would teach",
			scale.estimatePerToken)
	}
	// A rewritten payload makes the ingress byte count describe nothing that was counted,
	// so the bytes ratio must be left untaught rather than taught wrong.
	if scale.bytesPerToken != fallbackBytesPerToken {
		t.Fatalf("bytesPerToken = %v, want the untouched fallback %v", scale.bytesPerToken, fallbackBytesPerToken)
	}
}

// mustUnify rebuilds the unified form of an Anthropic test payload so the test can
// measure the estimate the client's own view carries.
func mustUnify(t *testing.T, payload string) provider.Request {
	t.Helper()
	var anthropic struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Content string `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(payload), &anthropic); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	unified := provider.Request{Model: anthropic.Model}
	for _, message := range anthropic.Messages {
		out := provider.Message{Role: message.Role}
		for _, block := range message.Content {
			text := block.Text
			if text == "" {
				text = block.Content
			}
			out.Content = append(out.Content, provider.Content{Type: block.Type, Text: text})
		}
		unified.Messages = append(unified.Messages, out)
	}
	return unified
}
