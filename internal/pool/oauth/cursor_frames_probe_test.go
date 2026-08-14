package oauth

// Temporary live probe: logs every decoded agent frame in order, to answer whether
// the token-delta frame is truly terminal or can arrive mid-turn (which would make
// "end on token" cut off long-thinking turns).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"literouter/internal/translator"
)

func TestCursorLiveFrameOrder(t *testing.T) {
	if os.Getenv("LITEROUTER_CURSOR_LIVE") != "1" {
		t.Skip("set LITEROUTER_CURSOR_LIVE=1 to run the live frame-order probe")
	}
	credentials, _, err := DetectCursorSession(context.Background())
	if err != nil {
		t.Fatalf("detect session: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_PROBE_AGENT_MODELS"))
	if model == "" {
		model = "composer-2.5"
	}
	prompt := "Plan out, step by step, how you would build a log anomaly detection pipeline. " +
		"Think for a long time about the architecture, then write out the plan in detail."
	frame, _, _, err := cursorAgentRequestBody(translator.OpenAIRequest{
		Model:    model,
		Messages: []translator.OpenAIMessage{{Role: "user", Content: prompt}},
	}, model, nil, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	headers, err := cursorHeaders(credentials, false)
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	reader, writer := io.Pipe()
	go func() { _, _ = writer.Write(frame); _ = writer.Close() }()
	request, err := http.NewRequest(http.MethodPost, cursorAgentBaseURL+cursorAgentRunPath, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.ContentLength = -1
	response, err := (&http.Client{Timeout: 120 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	defer writer.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		t.Fatalf("http %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	start := time.Now()
	frameCount := 0
	err = readCursorFrames(response.Body, func(flags byte, payload []byte) error {
		frameCount++
		if flags&connectFlagEndStream != 0 {
			t.Logf("t=%s END-STREAM: %s", time.Since(start).Round(time.Millisecond), strings.TrimSpace(string(payload)))
			return nil
		}
		update := decodeAgentServerMessage(payload)
		elapsed := time.Since(start).Round(time.Millisecond).String()
		switch {
		case update.Ended:
			t.Logf("t=%s ENDED", elapsed)
		case update.Idle:
			t.Logf("t=%s IDLE", elapsed)
		case update.ToolCall != nil:
			t.Logf("t=%s TOOL name=%q args=%q", elapsed, update.ToolCall.Name, truncate64(update.ToolCall.Arguments, 50))
		case update.Text != "":
			t.Logf("t=%s TEXT %q", elapsed, truncate64(update.Text, 50))
		case update.Thinking != "":
			t.Logf("t=%s THINK %q", elapsed, truncate64(update.Thinking, 50))
		case update.Tokens > 0:
			t.Logf("t=%s TOKENS +%d", elapsed, update.Tokens)
		default:
			t.Logf("t=%s OTHER flags=%d len=%d", elapsed, flags, len(payload))
		}
		return nil
	})
	t.Logf("read ended: %v, %d frames total", err, frameCount)
}

func truncate64(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s...(%d)", s[:n], len(s))
}
