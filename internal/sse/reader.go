package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

var (
	ErrLineTooLarge  = errors.New("SSE line exceeds configured limit")
	ErrEventTooLarge = errors.New("SSE event exceeds configured limit")
)

type Limits struct {
	LineBytes  int
	EventBytes int
}

func Read(reader io.Reader, limits Limits, emit func([]byte) error) error {
	if limits.LineBytes <= 0 || limits.EventBytes <= 0 {
		return fmt.Errorf("SSE limits must be positive")
	}
	buffered := bufio.NewReaderSize(reader, limits.LineBytes)
	data := make([]byte, 0, min(limits.LineBytes, limits.EventBytes))
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := append([]byte(nil), data...)
		data = data[:0]
		return emit(payload)
	}
	for {
		line, err := buffered.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return ErrLineTooLarge
		}
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if flushErr := flush(); flushErr != nil {
					return flushErr
				}
			case bytes.HasPrefix(line, []byte("data:")):
				payload := bytes.TrimSpace(line[len("data:"):])
				extra := len(payload)
				if len(data) > 0 {
					extra++
				}
				if len(data)+extra > limits.EventBytes {
					return ErrEventTooLarge
				}
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, payload...)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return flush()
			}
			return fmt.Errorf("read SSE: %w", err)
		}
	}
}
