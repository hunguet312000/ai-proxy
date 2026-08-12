package clisetup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateValidatesInput(t *testing.T) {
	_, err := Generate(Request{Tool: ToolClaude, Action: Apply, BaseURL: "javascript:bad", Token: "token", Model: "model"})
	if err == nil {
		t.Fatal("invalid URL accepted")
	}
	_, err = Generate(Request{Tool: ToolClaude, Action: Apply, BaseURL: "http://localhost:8317", Token: "bad\nkey", Model: "model"})
	if err == nil {
		t.Fatal("token newline accepted")
	}
}

func TestClaudeApplyAndResetPreserveSettings(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	home := t.TempDir()
	root := filepath.Join(home, ".claude")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings.json"), []byte(`{"permissions":{"allow":["Read"]},"env":{"KEEP":"yes"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	apply, err := Generate(Request{Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317/v1", Token: "secret", Model: "model", OpusModel: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, home, apply.Content)
	var settings map[string]any
	encoded, _ := os.ReadFile(filepath.Join(root, "settings.json"))
	if err := json.Unmarshal(encoded, &settings); err != nil {
		t.Fatal(err)
	}
	env := settings["env"].(map[string]any)
	if env["KEEP"] != "yes" || env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8317" || env["ANTHROPIC_AUTH_TOKEN"] != "secret" {
		t.Fatalf("settings = %#v", settings)
	}
	reset, _ := Generate(Request{Tool: ToolClaude, Action: Reset})
	runScript(t, home, reset.Content)
	encoded, _ = os.ReadFile(filepath.Join(root, "settings.json"))
	// Parsed rather than substring-matched: reset now rewrites the file instead of
	// restoring the original bytes, so its formatting is not part of the contract.
	settings = map[string]any{}
	if err := json.Unmarshal(encoded, &settings); err != nil {
		t.Fatal(err)
	}
	env, _ = settings["env"].(map[string]any)
	if env["KEEP"] != "yes" {
		t.Fatalf("unrelated env key lost: %s", encoded)
	}
	for key := range env {
		if strings.HasPrefix(key, "ANTHROPIC_") {
			t.Fatalf("LiteRouter env survived reset: %s", encoded)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "settings.json.literouter.bak")); !os.IsNotExist(err) {
		t.Fatal("reset left the snapshot behind")
	}
}

func TestCodexApplyAndResetPreserveFiles(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	_ = os.MkdirAll(root, 0o700)
	_ = os.WriteFile(filepath.Join(root, "config.toml"), []byte("approval_policy = \"on-request\"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(root, "auth.json"), []byte(`{"tokens":{"access_token":"keep"}}`), 0o600)
	apply, err := Generate(Request{Tool: ToolCodex, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "secret", Model: "model", SubagentModel: "small"})
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, home, apply.Content)
	config, _ := os.ReadFile(filepath.Join(root, "config.toml"))
	auth, _ := os.ReadFile(filepath.Join(root, "auth.json"))
	if !strings.Contains(string(config), `wire_api = "responses"`) || !strings.Contains(string(config), `base_url = "http://127.0.0.1:8317/v1"`) || !strings.Contains(string(auth), `"access_token": "keep"`) {
		t.Fatalf("config = %s, auth = %s", config, auth)
	}
	reset, _ := Generate(Request{Tool: ToolCodex, Action: Reset})
	runScript(t, home, reset.Content)
	config, _ = os.ReadFile(filepath.Join(root, "config.toml"))
	auth, _ = os.ReadFile(filepath.Join(root, "auth.json"))
	if string(config) != "approval_policy = \"on-request\"\n" || strings.Contains(string(auth), "OPENAI_API_KEY") {
		t.Fatalf("reset config = %s, auth = %s", config, auth)
	}
}

func runScript(t *testing.T, home string, script []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "setup.sh")
	if err := os.WriteFile(path, script, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", path)
	command.Env = append(os.Environ(), "HOME="+home)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, output)
	}
}

func TestLoadClaudeReturnsAppliedFieldsWithoutToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	if _, err := ApplyDirect(Request{
		Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "secret", Model: "default",
		FableModel: "fable", OpusModel: "opus", SonnetModel: "sonnet", HaikuModel: "haiku",
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadClaude()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BaseURL != "http://127.0.0.1:8317" || loaded.Model != "default" || loaded.FableModel != "fable" || loaded.OpusModel != "opus" || loaded.SonnetModel != "sonnet" || loaded.HaikuModel != "haiku" {
		t.Fatalf("LoadClaude() = %#v", loaded)
	}
	if loaded.Token != "" {
		t.Fatal("LoadClaude exposed auth token")
	}
}

func TestApplyDirectClaudeAndCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))

	// seed existing files
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"env":{"KEEP":"yes"}}`), 0o600)
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("approval_policy = \"on-request\"\n"), 0o600)

	if _, err := ApplyDirect(Request{Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "secret", Model: "m1", OpusModel: "opus"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(string(raw), "ANTHROPIC_BASE_URL") || !strings.Contains(string(raw), `"KEEP": "yes"`) {
		t.Fatalf("claude settings = %s", raw)
	}
	if _, err := ApplyDirect(Request{Tool: ToolClaude, Action: Reset}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if strings.Contains(string(raw), "ANTHROPIC_") {
		t.Fatalf("claude reset = %s", raw)
	}

	if _, err := ApplyDirect(Request{Tool: ToolCodex, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "secret", Model: "m1", SubagentModel: "s1"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	auth, _ := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if !strings.Contains(string(cfg), "literouter") || !strings.Contains(string(auth), "OPENAI_API_KEY") {
		t.Fatalf("codex apply cfg=%s auth=%s", cfg, auth)
	}
	if _, err := ApplyDirect(Request{Tool: ToolCodex, Action: Reset}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	auth, _ = os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if strings.Contains(string(cfg), "BEGIN LITEROUTER") || strings.Contains(string(auth), "OPENAI_API_KEY") {
		t.Fatalf("codex reset cfg=%s auth=%s", cfg, auth)
	}
}

func TestResetClaudePreservesUserSettings(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "settings.json")
	// What the user had before LiteRouter was ever applied.
	before := `{"model":"opus","permissions":{"allow":["Read(**)"]}}`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyClaude(home, Request{
		BaseURL: "http://127.0.0.1:8317", Token: "secret", Model: "ag/claude-opus-4-6-thinking",
	}); err != nil {
		t.Fatal(err)
	}
	// Everything below arrived after the apply: hooks, extra permissions, a new model.
	current := map[string]any{}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		t.Fatal(err)
	}
	current["hooks"] = map[string]any{"PreToolUse": []any{"script.sh"}}
	current["effortLevel"] = "high"
	current["model"] = "opus[1m]"
	current["permissions"] = map[string]any{"allow": []any{"Read(**)", "Bash(go *:*)"}}
	if err := writeJSONAtomic(path, current); err != nil {
		t.Fatal(err)
	}

	if _, err := resetClaude(home); err != nil {
		t.Fatal(err)
	}

	after := map[string]any{}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	// The point of the test: post-apply settings must survive the reset.
	if after["effortLevel"] != "high" {
		t.Fatalf("effortLevel lost: %#v", after)
	}
	if after["model"] != "opus[1m]" {
		t.Fatalf("model reverted to a stale snapshot: %#v", after["model"])
	}
	if _, ok := after["hooks"]; !ok {
		t.Fatalf("hooks lost: %#v", after)
	}
	allow, _ := after["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 2 {
		t.Fatalf("permissions reverted: %#v", after["permissions"])
	}
	// And Claude Code must be back on stock behaviour.
	if env, present := after["env"]; present {
		t.Fatalf("LiteRouter env survived the reset: %#v", env)
	}
	if _, err := os.Stat(filepath.Join(root, "settings.json.literouter.bak")); !os.IsNotExist(err) {
		t.Fatal("the snapshot was left behind, so a later reset would restore stale values")
	}
}

func TestResetClaudeRestoresAPreexistingBaseURL(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "settings.json")
	// The user already pointed Claude Code somewhere of their own choosing.
	before := `{"env":{"ANTHROPIC_BASE_URL":"https://gateway.internal","OTHER":"keep"}}`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyClaude(home, Request{
		BaseURL: "http://127.0.0.1:8317", Token: "secret", Model: "m",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := resetClaude(home); err != nil {
		t.Fatal(err)
	}
	after := map[string]any{}
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	env, _ := after["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "https://gateway.internal" {
		t.Fatalf("a pre-existing base URL was not restored: %#v", env)
	}
	if env["OTHER"] != "keep" {
		t.Fatalf("unrelated env key lost: %#v", env)
	}
	if _, present := env["ANTHROPIC_AUTH_TOKEN"]; present {
		t.Fatalf("LiteRouter token survived: %#v", env)
	}
}

func TestResetCodexKeepsUnrelatedConfigAndOAuth(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	authPath := filepath.Join(root, "auth.json")
	if err := os.WriteFile(configPath, []byte("approval_policy = \"on-request\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A ChatGPT OAuth login, which reset must not turn into a broken apikey setup.
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"chatgpt","tokens":{"refresh":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := applyCodex(home, Request{
		BaseURL: "http://127.0.0.1:8317", Token: "secret", Model: "cx/gpt-5.6-luna",
	}); err != nil {
		t.Fatal(err)
	}
	// Added after the apply.
	raw, _ := os.ReadFile(configPath)
	if err := os.WriteFile(configPath, append([]byte("sandbox_mode = \"workspace-write\"\n"), raw...), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := resetCodex(home); err != nil {
		t.Fatal(err)
	}

	text, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "LITEROUTER") || strings.Contains(string(text), "model_provider") {
		t.Fatalf("LiteRouter block survived: %q", text)
	}
	for _, want := range []string{"approval_policy", "sandbox_mode"} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("unrelated setting %s lost: %q", want, text)
		}
	}
	auth := map[string]any{}
	rawAuth, _ := os.ReadFile(authPath)
	if err := json.Unmarshal(rawAuth, &auth); err != nil {
		t.Fatal(err)
	}
	if auth["auth_mode"] != "chatgpt" {
		t.Fatalf("OAuth auth_mode not restored: %#v", auth)
	}
	if _, present := auth["OPENAI_API_KEY"]; present {
		t.Fatalf("injected API key survived: %#v", auth)
	}
	if _, ok := auth["tokens"]; !ok {
		t.Fatalf("OAuth tokens lost: %#v", auth)
	}
}

func TestDraftHasSelection(t *testing.T) {
	// BaseURL alone must not count: the handler always fills it in, so treating it as a
	// selection would let a reset from an empty form overwrite a real draft.
	if (Draft{BaseURL: "http://127.0.0.1:8317"}).HasSelection() {
		t.Fatal("a draft with only a base URL was treated as a selection")
	}
	if (Draft{}).HasSelection() {
		t.Fatal("an empty draft was treated as a selection")
	}
	if (Draft{HaikuModel: "  "}).HasSelection() {
		t.Fatal("a whitespace-only model was treated as a selection")
	}
	for _, draft := range []Draft{
		{Model: "cc/cheap"}, {SubagentModel: "cc/mid"}, {FableModel: "f"},
		{OpusModel: "o"}, {SonnetModel: "s"}, {HaikuModel: "h"},
	} {
		if !draft.HasSelection() {
			t.Fatalf("draft %#v was not treated as a selection", draft)
		}
	}
}

func TestDraftRoundTripsThroughRequest(t *testing.T) {
	request := Request{
		Tool: ToolClaude, Action: Apply, Token: "secret", BaseURL: "http://127.0.0.1:8317",
		Model: "cc/cheap", SubagentModel: "cc/mid", FableModel: "f",
		OpusModel: "o", SonnetModel: "s", HaikuModel: "h",
	}
	draft := request.Draft()
	// The token is the one field that must not survive: it belongs to the running
	// instance, and persisting it would store a credential for no reason.
	rebuilt := draft.Request()
	if rebuilt.Token != "" || rebuilt.Tool != "" || rebuilt.Action != "" {
		t.Fatalf("draft carried tool/action/token: %#v", rebuilt)
	}
	rebuilt.Tool, rebuilt.Action, rebuilt.Token = request.Tool, request.Action, request.Token
	if rebuilt != request {
		t.Fatalf("round trip lost fields:\n got %#v\nwant %#v", rebuilt, request)
	}
}

func TestEffortValidation(t *testing.T) {
	base := Request{Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "secret", Model: "m"}
	// Empty is the "don't manage effortLevel" case and must stay legal.
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		request := base
		request.Effort = effort
		if err := request.validate(); err != nil {
			t.Fatalf("effort %q rejected: %v", effort, err)
		}
	}
	// ultracode is a session-only mode that also enables dynamic workflows; it is
	// not a persistable effortLevel and must not be written by this setup form.
	for _, effort := range []string{"MAX", "High", "ultracode", "auto", "7"} {
		request := base
		request.Effort = effort
		if err := request.validate(); err == nil {
			t.Fatalf("effort %q was accepted", effort)
		}
	}
}

func TestEffortLevelsIncludeClaudeMaxButNotUltracode(t *testing.T) {
	if !contains(EffortLevels, "max") {
		t.Fatal("session effort levels omit max")
	}
	if contains(EffortLevels, Ultracode) {
		t.Fatal("session effort levels must not persist ultracode")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestEffortRoundTripsThroughDraft(t *testing.T) {
	request := Request{Tool: ToolClaude, Action: Apply, Token: "secret", Model: "m", Effort: "xhigh"}
	if got := request.Draft().Effort; got != "xhigh" {
		t.Fatalf("draft effort = %q, want xhigh", got)
	}
	if got := request.Draft().Request().Effort; got != "xhigh" {
		t.Fatalf("rebuilt effort = %q, want xhigh", got)
	}
	// An effort-only draft is still worth remembering — HasSelection is not models-only.
	if !(Draft{Effort: "low"}).HasSelection() {
		t.Fatal("an effort-only draft was treated as empty")
	}
}

func TestApplyWritesEffortLevelWithoutClearingIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	// The user's own effortLevel, set via /effort before LiteRouter ever ran.
	if err := os.WriteFile(path, []byte(`{"effortLevel":"high","permissions":{"defaultMode":"auto"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	apply := func(effort string) map[string]any {
		if _, err := ApplyDirect(Request{
			Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317",
			Token: "secret", Model: "cx/cheap", Effort: effort,
		}); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]any{}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Empty means "leave it alone" — never clear a preference LiteRouter didn't set.
	if got := apply("")["effortLevel"]; got != "high" {
		t.Fatalf("effortLevel = %v after an empty apply, want high untouched", got)
	}
	if got := apply("xhigh")["effortLevel"]; got != "xhigh" {
		t.Fatalf("effortLevel = %v, want xhigh", got)
	}
	// It is a top-level key beside `model`, not an env var — writing it into env would
	// make Claude Code ignore it entirely.
	after := apply("xhigh")
	if env, _ := after["env"].(map[string]any); env["effortLevel"] != nil || env["CLAUDE_CODE_EFFORT_LEVEL"] != nil {
		t.Fatalf("effort leaked into env: %v", env)
	}
	if loaded, err := LoadClaude(); err != nil || loaded.Effort != "xhigh" {
		t.Fatalf("LoadClaude effort = %q, %v; want xhigh", loaded.Effort, err)
	}

	// Reset restores routing but leaves effort in place: apply only writes it when the
	// field is filled, so a value here may be one /effort set after the snapshot.
	if _, err := ApplyDirect(Request{Tool: ToolClaude, Action: Reset}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["effortLevel"] != "xhigh" {
		t.Fatalf("effortLevel = %v after reset, want it left at xhigh", out["effortLevel"])
	}
	if env, _ := out["env"].(map[string]any); env["ANTHROPIC_BASE_URL"] != nil {
		t.Fatalf("reset left routing keys behind: %v", env)
	}
}

func TestRequestContextWindow(t *testing.T) {
	cases := []struct {
		name    string
		request Request
		want    int
	}{
		{name: "empty leaves the client alone", request: Request{}, want: 0},
		{
			name:    "auto uses the resolved window",
			request: Request{MaxContext: "auto", CatalogContextWindow: 400_000},
			want:    400_000,
		},
		{
			// Nothing known means nothing written. A guess here would be a claim about the
			// window, and a wrong one is what this setting exists to prevent.
			name:    "auto with no resolved window writes nothing",
			request: Request{MaxContext: "AUTO"},
			want:    0,
		},
		{
			name:    "explicit count wins over the resolved one",
			request: Request{MaxContext: "200000", CatalogContextWindow: 400_000},
			want:    200_000,
		},
		{
			name:    "a window too small to work in is refused",
			request: Request{MaxContext: "1000"},
			want:    0,
		},
		{name: "garbage is refused", request: Request{MaxContext: "lots"}, want: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.request.ContextWindow(); got != testCase.want {
				t.Fatalf("got %d, want %d", got, testCase.want)
			}
		})
	}
}

// The window is the only thing the client is told, and the keys that used to force it to
// compact early are written as empty so that an apply also strips them from a
// settings.json an older version wrote them into.
func TestApplyClaudeWritesTheWindowAndForcesNoCompaction(t *testing.T) {
	home := t.TempDir()
	request := Request{
		Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "t",
		Model: "cx/gpt-5.6-sol", MaxContext: "auto", CatalogContextWindow: 400_000,
	}
	if _, err := applyClaude(home, request); err != nil {
		t.Fatalf("applyClaude: %v", err)
	}
	env := claudeEnv(t, home)
	if env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "400000" {
		t.Fatalf("max context: got %q, want 400000", env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"])
	}
	for _, key := range compactForcingKeys {
		if value, ok := env[key]; ok {
			t.Fatalf("%s = %q: compaction must be left to the client, not forced", key, value)
		}
	}
}

// The migration half of the same rule. Anyone who applied an earlier version has the
// forcing keys sitting in settings.json; applying again has to take them out, because
// nothing else will and a stale PCT_OVERRIDE=45 keeps compacting at 45% of the window
// forever.
func TestApplyClaudeStripsCompactForcingKeysLeftByAnEarlierApply(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := `{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"45","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"370578","CLAUDE_CODE_DISABLE_1M_CONTEXT":"1","KEEP_ME":"yes"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	request := Request{
		Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "t",
		Model: "cx/gpt-5.6-sol", MaxContext: "auto", CatalogContextWindow: 400_000,
	}
	if _, err := applyClaude(home, request); err != nil {
		t.Fatalf("applyClaude: %v", err)
	}
	env := claudeEnv(t, home)
	for _, key := range compactForcingKeys {
		if value, ok := env[key]; ok {
			t.Fatalf("%s = %q survived the apply", key, value)
		}
	}
	if env["KEEP_ME"] != "yes" {
		t.Fatalf("apply removed a key it does not own: %#v", env)
	}
}

var compactForcingKeys = []string{
	"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE",
	"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
	"CLAUDE_CODE_DISABLE_1M_CONTEXT",
}

// The client consults CLAUDE_CODE_MAX_CONTEXT_TOKENS only for ids that do not start
// with "claude-", so writing it for one of those would be inert.
func TestApplyClaudeSkipsMaxContextForAnthropicIDs(t *testing.T) {
	home := t.TempDir()
	request := Request{
		Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "t",
		Model: "claude-opus-5", MaxContext: "auto", CatalogContextWindow: 1_000_000,
	}
	if _, err := applyClaude(home, request); err != nil {
		t.Fatalf("applyClaude: %v", err)
	}
	env := claudeEnv(t, home)
	for _, key := range []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"} {
		if _, ok := env[key]; ok {
			t.Fatalf("%s was written for an anthropic model id", key)
		}
	}
}

func claudeEnv(t *testing.T, home string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	return settings.Env
}

// A reset has to strip the forcing keys too, for the same reason an apply does: they are
// only removable by something that knows it owns them.
func TestResetClaudeStripsCompactForcingKeys(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := `{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"45","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"370578","CLAUDE_CODE_DISABLE_1M_CONTEXT":"1","KEEP_ME":"yes"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resetClaude(home); err != nil {
		t.Fatalf("resetClaude: %v", err)
	}
	env := claudeEnv(t, home)
	for _, key := range compactForcingKeys {
		if value, ok := env[key]; ok {
			t.Fatalf("%s = %q survived the reset", key, value)
		}
	}
	if env["KEEP_ME"] != "yes" {
		t.Fatalf("reset removed a key it does not own: %#v", env)
	}
}

// There are four ways a Claude Code configuration is written or reverted — the
// downloadable apply script, the direct apply, the downloadable reset script, the direct
// reset — and they have to agree about which keys LiteRouter owns. They did not: the
// apply script wrote no context keys at all, and the reset script reverted every key
// except those two, so a user who applied and then reset by script was left with a
// window and a compact percentage that nothing in the file explained.
func TestEveryClaudePathAgreesOnTheManagedKeys(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	request := Request{
		Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "secret",
		Model: "cx/gpt-5.6-luna", MaxContext: "auto", CatalogContextWindow: 372_860,
	}

	scripted := t.TempDir()
	apply, err := Generate(request)
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, scripted, apply.Content)
	scriptEnv := claudeEnv(t, scripted)

	direct := t.TempDir()
	if _, err := applyClaude(direct, request); err != nil {
		t.Fatal(err)
	}
	directEnv := claudeEnv(t, direct)

	// The window has to reach the client whichever way the configuration was applied — and
	// nothing that forces compaction may, by either route.
	for _, env := range []map[string]string{scriptEnv, directEnv} {
		if env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "372860" {
			t.Fatalf("max context = %q, want 372860", env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"])
		}
		for _, key := range compactForcingKeys {
			if value, ok := env[key]; ok {
				t.Fatalf("%s = %q: no path may force compaction", key, value)
			}
		}
	}
	for key, value := range directEnv {
		if scriptEnv[key] != value {
			t.Fatalf("key %q: script wrote %v, direct apply wrote %v", key, scriptEnv[key], value)
		}
	}
	for key := range scriptEnv {
		if _, ok := directEnv[key]; !ok {
			t.Fatalf("script wrote %q, direct apply did not", key)
		}
	}

	// And reset has to take all of them back out, by either route.
	reset, err := Generate(Request{Tool: ToolClaude, Action: Reset})
	if err != nil {
		t.Fatal(err)
	}
	runScript(t, scripted, reset.Content)
	if _, err := resetClaude(direct); err != nil {
		t.Fatal(err)
	}
	for _, env := range []map[string]string{claudeEnv(t, scripted), claudeEnv(t, direct)} {
		for _, key := range claudeManagedEnv() {
			if _, ok := env[key]; ok {
				t.Fatalf("reset left %q behind: %v", key, env)
			}
		}
	}
}

// Claude Code consults CLAUDE_CODE_MAX_CONTEXT_TOKENS only for ids that do not start with
// "claude-". Writing the window for one of those is a no-op, but writing the keys that act
// on it is not: the percentage and the 1M switch would bind against a window the client
// never read. All four move together or none does.
func TestClaudeModelsGetNoneOfTheWindowKeys(t *testing.T) {
	env := claudeEnvFor(Request{
		Tool: ToolClaude, Action: Apply, BaseURL: "http://127.0.0.1:8317", Token: "t",
		Model: "claude-opus-4-5", MaxContext: "auto", CatalogContextWindow: 400_000,
	})
	for _, key := range []string{
		"CLAUDE_CODE_MAX_CONTEXT_TOKENS", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_CODE_DISABLE_1M_CONTEXT",
	} {
		if env[key] != "" {
			t.Fatalf("%s = %q for a claude-* model, want empty", key, env[key])
		}
	}
}
