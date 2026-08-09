package oauth

import (
	"encoding/binary"
	"encoding/json"
	"strings"

	"literouter/internal/provider"
)

// Cursor's chat service speaks Connect with protobuf bodies against a schema that is
// not published. The field numbers below were read off the wire format its IDE uses;
// there is no generated code to fall back on, so the message is assembled by hand.
//
// Every offset here is load-bearing and unverifiable from documentation. Changing one
// does not produce a compile error — it produces a request the server rejects or,
// worse, silently misreads, so each group is written out explicitly rather than
// abstracted into a struct mapping.

const (
	protoVarint = 0
	protoBytes  = 2
)

// putUvarint encodes a protobuf base-128 varint.
func putUvarint(value uint64) []byte {
	var out []byte
	for value >= 128 {
		out = append(out, byte(value)|0x80)
		value >>= 7
	}
	return append(out, byte(value))
}

// protoField writes one field. Only the two wire types the schema uses are
// supported; anything else would be a silent encoding bug, so it is rejected.
func protoField(number, wireType int, value any) []byte {
	tag := putUvarint(uint64(number)<<3 | uint64(wireType))
	switch wireType {
	case protoVarint:
		var n uint64
		switch typed := value.(type) {
		case int:
			n = uint64(typed)
		case uint64:
			n = typed
		case bool:
			if typed {
				n = 1
			}
		default:
			return nil
		}
		return append(tag, putUvarint(n)...)
	case protoBytes:
		var payload []byte
		switch typed := value.(type) {
		case string:
			payload = []byte(typed)
		case []byte:
			payload = typed
		}
		out := append(tag, putUvarint(uint64(len(payload)))...)
		return append(out, payload...)
	default:
		return nil
	}
}

func protoConcat(parts ...[]byte) []byte {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]byte, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

// connectFrame wraps a payload in the Connect streaming envelope: one flag byte then
// a big-endian length. Flag bit 0 marks compression, bit 1 marks the end-of-stream
// frame whose payload is JSON rather than protobuf.
func connectFrame(payload []byte, compressed bool) []byte {
	frame := make([]byte, 5+len(payload))
	if compressed {
		frame[0] = 1
	}
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

const (
	connectFlagCompressed = 0x01
	connectFlagEndStream  = 0x02
)

// protoFieldValue is one decoded field occurrence.
type protoFieldValue struct {
	WireType int
	Varint   uint64
	Bytes    []byte
}

// parseProtoFields decodes a message into field number → occurrences. Unknown fields
// are kept rather than rejected: the schema is not ours and new fields appear without
// notice, so an unrecognised one must not abort a response.
func parseProtoFields(data []byte) map[int][]protoFieldValue {
	fields := map[int][]protoFieldValue{}
	offset := 0
	for offset < len(data) {
		tag, next, ok := readUvarint(data, offset)
		if !ok {
			break
		}
		number, wireType := int(tag>>3), int(tag&7)
		offset = next
		value := protoFieldValue{WireType: wireType}
		switch wireType {
		case protoVarint:
			parsed, after, ok := readUvarint(data, offset)
			if !ok {
				return fields
			}
			value.Varint, offset = parsed, after
		case protoBytes:
			length, after, ok := readUvarint(data, offset)
			if !ok || after+int(length) > len(data) {
				return fields
			}
			value.Bytes, offset = data[after:after+int(length)], after+int(length)
		case 1:
			if offset+8 > len(data) {
				return fields
			}
			value.Bytes, offset = data[offset:offset+8], offset+8
		case 5:
			if offset+4 > len(data) {
				return fields
			}
			value.Bytes, offset = data[offset:offset+4], offset+4
		default:
			return fields
		}
		fields[number] = append(fields[number], value)
	}
	return fields
}

func readUvarint(data []byte, offset int) (uint64, int, bool) {
	var result uint64
	var shift uint
	for offset < len(data) {
		current := data[offset]
		result |= uint64(current&0x7f) << shift
		offset++
		if current&0x80 == 0 {
			return result, offset, true
		}
		shift += 7
		if shift > 63 {
			return 0, offset, false
		}
	}
	return 0, offset, false
}

type CursorToolCall struct {
	ID        string
	Name      string
	Arguments string
	IsLast    bool
}

// cursorEndStreamError reads the JSON payload of an end-of-stream frame. Connect
// reports failures there rather than with an HTTP status, so a stream that looks
// successful can still be carrying an error.
func cursorEndStreamError(payload []byte) error {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Debug struct {
					Error   string `json:"error"`
					Details struct {
						Title  string `json:"title"`
						Detail string `json:"detail"`
					} `json:"details"`
				} `json:"debug"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return nil
	}
	if envelope.Error.Code == "" && envelope.Error.Message == "" {
		return nil
	}
	// The top-level message is almost always the literal string "Error"; everything a
	// user can act on — "Free plans can only use Auto", "Your version of Cursor is no
	// longer supported" — is buried in details[].debug. Surfacing the code alone would
	// turn an actionable answer into a generic upstream failure.
	message := envelope.Error.Message
	code := envelope.Error.Code
	for _, detail := range envelope.Error.Details {
		if text := strings.TrimSpace(detail.Debug.Details.Detail); text != "" {
			message = text
			if title := strings.TrimSpace(detail.Debug.Details.Title); title != "" {
				message = title + ": " + text
			}
			if debugCode := strings.TrimSpace(detail.Debug.Error); debugCode != "" {
				code = debugCode
			}
			break
		}
	}
	return &provider.ProviderError{
		Provider: "cursor", StatusCode: cursorErrorStatus(envelope.Error.Code), Code: code, Message: message,
	}
}

// cursorErrorStatus maps a Connect code onto the HTTP status the gateway classifies
// on, so a Cursor rate limit rotates accounts and an auth failure does not.
func cursorErrorStatus(code string) int {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "unauthenticated", "permission_denied":
		return 401
	case "resource_exhausted":
		// Plan and version refusals arrive under this code too, and none of them are
		// fixed by trying another account, so it is not reported as a rate limit.
		return 400
	case "invalid_argument", "failed_precondition", "out_of_range":
		return 400
	case "unavailable", "internal", "deadline_exceeded":
		return 503
	default:
		return 502
	}
}
