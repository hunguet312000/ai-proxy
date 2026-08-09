package oauth

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"literouter/internal/provider"
)

// resolveCursorModel maps the routed name onto what the service accepts. The prefix
// is LiteRouter addressing and must not leave.
func resolveCursorModel(model string) string {
	model = strings.TrimSpace(model)
	for _, prefix := range []string{"cursor/", "cu/"} {
		if rest, found := strings.CutPrefix(strings.ToLower(model), prefix); found {
			return model[len(model)-len(rest):]
		}
	}
	if model == "" {
		return "default"
	}
	return model
}

// readCursorFrames walks the Connect envelope stream. A frame is a flag byte, a
// big-endian length, then that many bytes.
func readCursorFrames(reader io.Reader, emit func(flags byte, payload []byte) error) error {
	header := make([]byte, 5)
	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			if err == io.EOF {
				return nil
			}
			if err == io.ErrUnexpectedEOF {
				return fmt.Errorf("cursor stream ended mid-frame")
			}
			return err
		}
		length := binary.BigEndian.Uint32(header[1:5])
		if length > cursorMaxFrameBytes {
			return fmt.Errorf("cursor frame of %d bytes exceeds the %d byte limit", length, cursorMaxFrameBytes)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return fmt.Errorf("cursor frame truncated: %w", err)
		}
		if header[0]&connectFlagCompressed != 0 {
			decompressed, err := gunzipBytes(payload)
			if err != nil {
				return fmt.Errorf("cursor frame is not valid gzip: %w", err)
			}
			payload = decompressed
		}
		if err := emit(header[0], payload); err != nil {
			return err
		}
		if header[0]&connectFlagEndStream != 0 {
			return nil
		}
	}
}

const cursorMaxFrameBytes = 32 << 20

// gunzipBytes expands a compressed frame. Connect marks compression per frame, so a
// stream can mix compressed and plain envelopes.
func gunzipBytes(payload []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, cursorMaxFrameBytes))
}

// decodeCursorHTTPError turns a non-2xx response into the typed error the gateway
// already classifies, so a Cursor failure is retried or reported like any other.
func decodeCursorHTTPError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = response.Status
	}
	code := ""
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Message != "" {
		code, message = envelope.Code, envelope.Message
	}
	return &provider.ProviderError{
		Provider: "cursor", StatusCode: response.StatusCode, Code: code, Message: message,
	}
}
