package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"literouter/internal/translator"
)

// TestCursorLiveProbe talks to the real service and is skipped unless
// LITEROUTER_CURSOR_LIVE=1. It exists because the only way to learn which models a
// given Cursor plan may use is to ask the service: the id list is not published.
func TestCursorLiveProbe(t *testing.T) {
	if os.Getenv("LITEROUTER_CURSOR_LIVE") != "1" {
		t.Skip("set LITEROUTER_CURSOR_LIVE=1 to run the live Cursor probe")
	}
	credentials, path, err := DetectCursorSession(context.Background())
	if err != nil {
		t.Fatalf("detect session: %v", err)
	}
	// A probe override lets the same session be replayed under a different claimed
	// build, which is the only way to tell a version gate apart from a plan gate.
	credentials.ClientVersion = strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_PROBE_VERSION"))
	credentials.ClientCommit = strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_PROBE_COMMIT"))
	if credentials.ClientVersion == "" || credentials.ClientCommit == "" {
		if build, buildErr := DetectCursorBuild(); buildErr == nil {
			credentials.ClientVersion, credentials.ClientCommit = build.Version, build.Commit
		} else {
			t.Logf("build not detected: %v", buildErr)
		}
	}
	// The checksum appends the machine id verbatim, so a "machineId/macMachineId" pair
	// can be supplied whole to reproduce the macOS form the IDE uses.
	if machineID := strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_PROBE_MACHINE_ID")); machineID != "" {
		credentials.MachineID = machineID
	}
	t.Logf("session=%s claiming version=%s commit=%s machine=%s",
		path, credentials.ClientVersion, credentials.ClientCommit, credentials.MachineID)

	// AvailableModels is a unary call, so it takes a bare protobuf body rather than
	// the streaming envelope the chat endpoint uses.
	unary := []string{"/aiserver.v1.AiService/AvailableModels"}
	if override := strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_PROBE_UNARY")); override != "" {
		unary = strings.Split(override, ",")
	}
	for _, path := range unary {
		t.Run("unary "+path, func(t *testing.T) {
			body, err := postUnary(credentials, path, nil)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if want := strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_PROBE_MODEL_DETAIL")); want != "" {
				t.Logf("%s", modelDetail(body, want))
				return
			}
			t.Logf("%s", summariseUnary(body))
		})
	}

	for _, model := range strings.Split(os.Getenv("LITEROUTER_CURSOR_PROBE_AGENT_MODELS"), ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		t.Run("agent "+model, func(t *testing.T) {
			t.Logf("%s", probeAgentRun(credentials, model, "Reply with the single word: pong", nil))
		})
		for _, effort := range strings.Split(os.Getenv("LITEROUTER_CURSOR_PROBE_EFFORTS"), ",") {
			effort = strings.TrimSpace(effort)
			if effort == "" {
				continue
			}
			t.Run("agent-effort "+model+"/"+effort, func(t *testing.T) {
				t.Logf("%s", probeAgentRunWithEffort(credentials, model, effort))
			})
		}
		t.Run("agent-tools "+model, func(t *testing.T) {
			tools := []translator.OpenAITool{{
				Type: "function",
				Function: translator.OpenAIFunction{
					Name:        "get_weather",
					Description: "Get the current weather for a city",
					Parameters: json.RawMessage(
						`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
				},
			}}
			t.Logf("%s", probeAgentRun(credentials, model, "What is the weather in Hanoi? Use the get_weather tool.", tools))
		})
	}

}

// probeAgentRun opens the bidi Run stream and reports what comes back. The request
// body stays open, which is what makes it a bidi stream rather than a unary post.
func probeAgentRun(credentials CursorCredentials, model, prompt string, tools []translator.OpenAITool) string {
	return probeAgentRunWithEffortAndTools(credentials, model, "", prompt, tools)
}

func probeAgentRunWithEffort(credentials CursorCredentials, model, effort string) string {
	return probeAgentRunWithEffortAndTools(credentials, model, effort, "Reply with the single word: pong", nil)
}

func probeAgentRunWithEffortAndTools(credentials CursorCredentials, model, effort, prompt string, tools []translator.OpenAITool) string {
	headers, err := cursorHeaders(credentials, false)
	if err != nil {
		return "headers: " + err.Error()
	}
	// Build through the production encoder so the probe exercises what ships.
	frame, _, _, err := cursorAgentRequestBody(translator.OpenAIRequest{
		Model:    model,
		Messages: []translator.OpenAIMessage{{Role: "user", Content: prompt}},
		Tools:    tools,
		Effort:   effort,
	}, model, nil, nil)
	if err != nil {
		return "encode: " + err.Error()
	}

	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write(frame)
		if os.Getenv("LITEROUTER_CURSOR_PROBE_KEEP_OPEN") == "1" {
			return
		}
		// Half-closing is enough for a turn that sends no tool results, and it tells
		// HTTP/1.1 fallbacks that the body is complete.
		_ = writer.Close()
	}()
	base := cursorAgentBaseURL
	if override := strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_PROBE_BASE_URL")); override != "" {
		base = override
	}
	request, err := http.NewRequest(http.MethodPost, base+cursorAgentRunPath, reader)
	if err != nil {
		return "request: " + err.Error()
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.ContentLength = -1
	response, err := (&http.Client{Timeout: 120 * time.Second}).Do(request)
	if err != nil {
		return "post: " + err.Error()
	}
	defer response.Body.Close()
	defer writer.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return fmt.Sprintf("http %d proto=%s: %s", response.StatusCode, response.Proto, strings.TrimSpace(string(body)))
	}

	// Read through the production converter: the value of this probe is that it proves
	// the shipped translation, not a parallel one written for the test.
	var out strings.Builder
	fmt.Fprintf(&out, "proto=%s\n", response.Proto)
	converted, err := io.ReadAll(cursorAgentToChatStream(response.Body, model))
	out.Write(converted)
	if err != nil {
		fmt.Fprintf(&out, "\nSTREAM-ERROR: %v\n", err)
	}
	return out.String()
}

// sendUnary posts a unary call to an explicit host, for services that do not live on
// the chat endpoint's base URL.
func sendUnary(credentials CursorCredentials, base, path string) ([]byte, error) {
	previous := os.Getenv("LITEROUTER_CURSOR_PROBE_BASE_URL")
	_ = os.Setenv("LITEROUTER_CURSOR_PROBE_BASE_URL", base)
	defer os.Setenv("LITEROUTER_CURSOR_PROBE_BASE_URL", previous)
	return postUnary(credentials, path, nil)
}

func postUnary(credentials CursorCredentials, path string, payload []byte) ([]byte, error) {
	return send(credentials, path, payload, "application/proto")
}

func post(credentials CursorCredentials, path string, payload []byte) ([]byte, error) {
	return send(credentials, path, payload, "")
}

func send(credentials CursorCredentials, path string, payload []byte, contentType string) ([]byte, error) {
	headers, err := cursorHeaders(credentials, false)
	if err != nil {
		return nil, err
	}
	base := cursorBaseURL
	if override := strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_PROBE_BASE_URL")); override != "" {
		base = override
	}
	request, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	if contentType != "" {
		request.Header.Set("content-type", contentType)
	}
	// The IDE only sends the commit header for Anysphere staff, so the probe needs to
	// be able to leave it out.
	if os.Getenv("LITEROUTER_CURSOR_PROBE_NO_COMMIT") == "1" {
		request.Header.Del("x-cursor-client-commit")
	}
	for _, pair := range strings.Split(os.Getenv("LITEROUTER_CURSOR_PROBE_HEADERS"), ";") {
		key, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		if found && key != "" {
			request.Header.Set(key, value)
		}
	}
	client := &http.Client{Timeout: 60 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// summariseUnary pulls the printable strings out of a unary protobuf response. The
// schema is unpublished, so this walks every length-delimited field rather than
// pretending to know the message layout.
func summariseUnary(raw []byte) string {
	var names []string
	seen := map[string]bool{}
	collectModelNames(raw, seen, &names, 0)
	if len(names) == 0 {
		return fmt.Sprintf("no strings in %d bytes", len(raw))
	}
	return strings.Join(names, "\n")
}

// modelDetail prints every field of the entry whose first string field is `want`,
// which is how an account-specific availability flag can be spotted without the
// schema.
func modelDetail(raw []byte, want string) string {
	var out strings.Builder
	for _, occurrences := range parseProtoFields(raw) {
		for _, field := range occurrences {
			if field.WireType != 2 {
				continue
			}
			entry := parseProtoFields(field.Bytes)
			name := ""
			if values, ok := entry[1]; ok && len(values) > 0 && values[0].WireType == 2 {
				name = string(values[0].Bytes)
			}
			if name != want {
				continue
			}
			fmt.Fprintf(&out, "entry %q:\n", name)
			for number := 1; number <= 60; number++ {
				for _, value := range entry[number] {
					switch value.WireType {
					case 0:
						fmt.Fprintf(&out, "  %2d varint = %d\n", number, value.Varint)
					case 2:
						if printableModelID(string(value.Bytes)) {
							fmt.Fprintf(&out, "  %2d string = %q\n", number, value.Bytes)
						} else {
							fmt.Fprintf(&out, "  %2d bytes  = %x\n", number, value.Bytes)
						}
					default:
						fmt.Fprintf(&out, "  %2d wire%d = %x\n", number, value.WireType, value.Bytes)
					}
				}
			}
		}
	}
	if out.Len() == 0 {
		return "entry not found: " + want
	}
	return out.String()
}

func collectModelNames(payload []byte, seen map[string]bool, names *[]string, depth int) {
	if depth > 8 {
		return
	}
	for _, occurrences := range parseProtoFields(payload) {
		for _, field := range occurrences {
			if field.WireType != 2 {
				continue
			}
			if text := string(field.Bytes); printableModelID(text) {
				if !seen[text] {
					seen[text] = true
					*names = append(*names, text)
				}
				continue
			}
			collectModelNames(field.Bytes, seen, names, depth+1)
		}
	}
}

func printableModelID(text string) bool {
	if len(text) < 3 || len(text) > 60 {
		return false
	}
	for _, char := range text {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '-', char == '.', char == '_', char == ' ':
		default:
			return false
		}
	}
	return true
}
