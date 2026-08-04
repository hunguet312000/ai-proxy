package oauth

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMergeAnthropicBetasKeepsClientFlags(t *testing.T) {
	// The OAuth flag is mandatory, but dropping the client's own betas would
	// silently disable features Claude Code negotiated for the turn.
	merged := mergeAnthropicBetas("claude-code-20250219, fine-grained-tool-streaming-2025-05-14")
	if !strings.HasPrefix(merged, anthropicOAuthBeta) {
		t.Fatalf("oauth beta missing: %q", merged)
	}
	for _, expected := range []string{"claude-code-20250219", "fine-grained-tool-streaming-2025-05-14"} {
		if !strings.Contains(merged, expected) {
			t.Fatalf("client beta %q dropped: %q", expected, merged)
		}
	}
	if strings.Count(merged, anthropicOAuthBeta) != 1 {
		t.Fatalf("oauth beta duplicated: %q", merged)
	}
}

func TestRewriteAnthropicModelKeepsEverythingElse(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-4-5","system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":"u"},"stream":true}`)
	rewritten, err := rewriteAnthropicModel(payload, "claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rewritten, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["model"]) != `"claude-sonnet-4-5"` {
		t.Fatalf("model = %s", fields["model"])
	}
	for _, key := range []string{"system", "metadata", "stream"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("field %q was dropped: %s", key, rewritten)
		}
	}
	if !strings.Contains(string(fields["system"]), "ephemeral") {
		t.Fatalf("cache_control dropped: %s", fields["system"])
	}
}
