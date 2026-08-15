package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"literouter/internal/translator"
)

const (
	// Reasoning models routinely think for minutes before the first token, and a
	// coding turn can pause just as long between tool calls. Two minutes cut those
	// turns off mid-response; five leaves headroom while still catching dead peers.
	upstreamStreamIdleTimeout = 5 * time.Minute
	maxSSELineBytes           = 8 << 20
	maxSSEEventBytes          = 16 << 20
)

var (
	errUpstreamStreamIdle  = errors.New("upstream stream idle timeout")
	errEmptyUpstreamStream = errors.New("upstream returned an empty stream")
	errSSELineTooLarge     = errors.New("upstream SSE line exceeds 8 MiB")
	errSSEEventTooLarge    = errors.New("upstream SSE event exceeds 16 MiB")
)

type StreamClient interface {
	DoStream(ctx context.Context, path string, requestBody any) (io.ReadCloser, error)
}

type OpenAIStreamChunk struct {
	ID      string                  `json:"id"`
	Model   string                  `json:"model"`
	Choices []OpenAIStreamChoice    `json:"choices"`
	Usage   *translator.OpenAIUsage `json:"usage,omitempty"`
}

type OpenAIStreamChoice struct {
	Index        int               `json:"index"`
	Delta        OpenAIStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type OpenAIStreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// Reasoning is the deliberation text under the OpenAI spelling
	// (reasoning_content). vLLM 0.26 sends the same content as plain
	// "reasoning"; ReasoningAlt catches that spelling, so a turn where the
	// model only reasoned is still visible to the gateway instead of reading
	// as an empty stream.
	Reasoning    string                 `json:"reasoning_content,omitempty"`
	ReasoningAlt string                 `json:"reasoning,omitempty"`
	ToolCalls    []OpenAIStreamToolCall `json:"tool_calls,omitempty"`
}

// ReasoningText returns the deliberation text whichever spelling the upstream
// used.
func (d OpenAIStreamDelta) ReasoningText() string {
	if d.Reasoning != "" {
		return d.Reasoning
	}
	return d.ReasoningAlt
}

type OpenAIStreamToolCall struct {
	Index    int                           `json:"index"`
	ID       string                        `json:"id,omitempty"`
	Type     string                        `json:"type,omitempty"`
	Function translator.OpenAIFunctionCall `json:"function"`
}

func streamOpenAI(ctx context.Context, client StreamClient, request translator.OpenAIRequest, emit func(OpenAIStreamChunk) error) error {
	body, err := client.DoStream(ctx, "/chat/completions", request)
	if err != nil {
		return err
	}
	defer body.Close()
	return readOpenAIStreamWithIdleTimeout(ctx, body, upstreamStreamIdleTimeout, emit)
}

func readOpenAIStreamWithIdleTimeout(ctx context.Context, body io.ReadCloser, timeout time.Duration, emit func(OpenAIStreamChunk) error) error {
	return withStreamIdleTimeout(ctx, body, timeout, func(reader io.Reader) error {
		return readOpenAIStream(reader, emit)
	})
}

// withStreamIdleTimeout closes the upstream body when it stops producing bytes,
// so a dead peer surfaces as a timeout instead of hanging the caller forever.
func withStreamIdleTimeout(ctx context.Context, body io.ReadCloser, timeout time.Duration, read func(io.Reader) error) error {
	activity := make(chan struct{}, 1)
	done := make(chan struct{})
	var timedOut atomic.Bool
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(timeout)
			case <-ctx.Done():
				_ = body.Close()
				return
			case <-timer.C:
				timedOut.Store(true)
				_ = body.Close()
				return
			case <-done:
				return
			}
		}
	}()
	err := read(activityReader{reader: body, activity: activity})
	close(done)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if timedOut.Load() {
		return errUpstreamStreamIdle
	}
	return err
}

type activityReader struct {
	reader   io.Reader
	activity chan<- struct{}
}

func (r activityReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 {
		select {
		case r.activity <- struct{}{}:
		default:
		}
	}
	return n, err
}

func readOpenAIStream(body io.Reader, emit func(OpenAIStreamChunk) error) error {
	return readOpenAIStreamData(body, func(payload []byte) error {
		var chunk OpenAIStreamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return fmt.Errorf("decode upstream SSE event: %w", err)
		}
		return emit(chunk)
	})
}

func readOpenAIStreamData(body io.Reader, emit func([]byte) error) error {
	reader := bufio.NewReaderSize(body, maxSSELineBytes)
	var data []byte
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return errSSELineTooLarge
		}
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) == 0 {
				if len(data) > 0 {
					if bytes.Equal(data, []byte("[DONE]")) {
						return nil
					}
					if emitErr := emit(data); emitErr != nil {
						return emitErr
					}
					data = data[:0]
				}
			} else if bytes.HasPrefix(line, []byte("data:")) {
				payload := bytes.TrimSpace(line[len("data:"):])
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, payload...)
				if len(data) > maxSSEEventBytes {
					return errSSEEventTooLarge
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) {
					return emit(data)
				}
				return nil
			}
			return fmt.Errorf("read upstream SSE: %w", err)
		}
	}
}
