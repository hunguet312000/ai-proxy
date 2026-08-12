package clisetup

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ApplyResult is returned after writing host client config in-process.
type ApplyResult struct {
	Tool    string `json:"tool"`
	Action  string `json:"action"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

func LoadClaude() (Request, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return Request{}, fmt.Errorf("cannot resolve host home directory")
	}
	raw, err := os.ReadFile(filepath.Join(claudeRoot(home), "settings.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return Request{}, nil
		}
		return Request{}, err
	}
	var current struct {
		Env map[string]any `json:"env"`
		// effortLevel is a top-level setting, not an env var — the same key /effort writes.
		EffortLevel string `json:"effortLevel"`
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		return Request{}, fmt.Errorf("decode Claude settings: %w", err)
	}
	value := func(key string) string {
		text, _ := current.Env[key].(string)
		return text
	}
	return Request{
		Tool: ToolClaude, BaseURL: value("ANTHROPIC_BASE_URL"), Model: value("ANTHROPIC_MODEL"),
		SubagentModel: value("CLAUDE_CODE_SUBAGENT_MODEL"),
		FableModel:    value("ANTHROPIC_DEFAULT_FABLE_MODEL"), OpusModel: value("ANTHROPIC_DEFAULT_OPUS_MODEL"),
		SonnetModel: value("ANTHROPIC_DEFAULT_SONNET_MODEL"), HaikuModel: value("ANTHROPIC_DEFAULT_HAIKU_MODEL"),
		Effort: current.EffortLevel,
		// Read back as a literal count, not as "auto". What is on disk is a number, and
		// whether it came from auto-resolution is not recoverable from it — the draft is
		// where "auto" is remembered.
		MaxContext: value("CLAUDE_CODE_MAX_CONTEXT_TOKENS"),
	}, nil
}

// ApplyDirect writes Claude/Codex host config immediately (9router-style),
// without generating a downloadable shell script.
func ApplyDirect(request Request) (ApplyResult, error) {
	if err := request.validate(); err != nil {
		return ApplyResult{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ApplyResult{}, fmt.Errorf("cannot resolve host home directory")
	}
	switch request.Tool {
	case ToolClaude:
		if request.Action == Apply {
			path, err := applyClaude(home, request)
			if err != nil {
				return ApplyResult{}, err
			}
			return ApplyResult{Tool: "claude", Action: "apply", Message: "Claude Code configured", Path: path}, nil
		}
		path, err := resetClaude(home)
		if err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Tool: "claude", Action: "reset", Message: "Claude Code reset", Path: path}, nil
	case ToolCodex:
		if request.Action == Apply {
			path, err := applyCodex(home, request)
			if err != nil {
				return ApplyResult{}, err
			}
			return ApplyResult{Tool: "codex", Action: "apply", Message: "Codex configured", Path: path}, nil
		}
		path, err := resetCodex(home)
		if err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Tool: "codex", Action: "reset", Message: "Codex reset", Path: path}, nil
	default:
		return ApplyResult{}, fmt.Errorf("unsupported CLI tool %q", request.Tool)
	}
}

func claudeRoot(home string) string {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); v != "" {
		return v
	}
	return filepath.Join(home, ".claude")
}

func codexRoot(home string) string {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v
	}
	return filepath.Join(home, ".codex")
}

func applyClaude(home string, request Request) (string, error) {
	root := claudeRoot(home)
	path := filepath.Join(root, "settings.json")
	backup := filepath.Join(root, "settings.json.literouter.bak")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create claude config dir: %w", err)
	}
	current := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &current)
		if _, err := os.Stat(backup); err != nil {
			_ = os.WriteFile(backup, raw, 0o600)
		}
	}
	env, _ := current["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	// Same map the downloadable script writes — see claudeManagedEnv, which is the one
	// place that decides these. An empty value means the key is removed.
	for key, value := range claudeEnvFor(request) {
		if strings.TrimSpace(value) != "" {
			env[key] = value
		} else {
			delete(env, key)
		}
	}
	current["env"] = env
	// effortLevel sits at the top level, beside `model` — not in env. Written only when
	// a level was chosen: an empty field means "leave whatever the user set via /effort
	// alone", so this never clears a preference LiteRouter did not put there.
	//
	// CLAUDE_CODE_EFFORT_LEVEL is deliberately not written instead. That env var pins
	// effort for the session and makes /effort refuse to change it, so it would remove a
	// control rather than expose one.
	if effort := strings.TrimSpace(request.Effort); effort != "" {
		current["effortLevel"] = effort
	}
	if err := writeJSONAtomic(path, current); err != nil {
		return "", err
	}
	return path, nil
}

// claudeManagedEnv lists the settings.json env keys LiteRouter owns. Reset touches
// only these; anything else in the file belongs to the user.
//
// Derived from the map apply writes rather than repeated, so a key cannot be added to
// one and forgotten in the other — which would leave reset silently stranding whatever
// the new key configured.
func claudeManagedEnv() []string {
	keys := make([]string, 0, len(claudeEnvFor(Request{})))
	for key := range claudeEnvFor(Request{}) {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// resetClaude puts Claude Code back on stock behaviour: the LiteRouter env keys are
// removed, or restored to whatever they held before LiteRouter first touched them.
//
// effortLevel is intentionally left in place. Reset exists to stop Claude Code routing
// through LiteRouter, and an effort level does not route anything — it is a preference,
// like permissions and hooks. Reverting it would also mean guessing: apply writes it
// only when the field is filled in, so a value found here may be one the user set with
// /effort after the snapshot was taken, and deleting that would be data loss.
//
// It deliberately does NOT restore the backup over settings.json. That backup is a
// snapshot from the first ever apply, so renaming it into place discarded every
// permission, hook and model preference added since — the user's own configuration,
// not LiteRouter's. Only the keys LiteRouter writes are reverted.
func resetClaude(home string) (string, error) {
	root := claudeRoot(home)
	path := filepath.Join(root, "settings.json")
	backup := filepath.Join(root, "settings.json.literouter.bak")

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			_ = os.Remove(backup)
			return path, nil
		}
		return "", err
	}
	current := map[string]any{}
	if err := json.Unmarshal(raw, &current); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}

	// Pre-LiteRouter values, when a snapshot exists, so a base URL the user had set
	// themselves is put back rather than deleted.
	originalEnv := map[string]any{}
	if snapshot, err := os.ReadFile(backup); err == nil {
		previous := map[string]any{}
		if json.Unmarshal(snapshot, &previous) == nil {
			if env, ok := previous["env"].(map[string]any); ok {
				originalEnv = env
			}
		}
	}

	if env, _ := current["env"].(map[string]any); env != nil {
		for _, key := range claudeManagedEnv() {
			if value, existed := originalEnv[key]; existed {
				env[key] = value
			} else {
				delete(env, key)
			}
		}
		if len(env) == 0 {
			delete(current, "env")
		} else {
			current["env"] = env
		}
	}
	if err := writeJSONAtomic(path, current); err != nil {
		return "", err
	}
	// The snapshot has served its purpose. Leaving it would make the next apply skip
	// re-snapshotting and a later reset restore stale values.
	_ = os.Remove(backup)
	return path, nil
}

// literouterBlock matches the fenced section apply writes into config.toml, so
// reset can remove exactly that and leave the rest of the file untouched.
var literouterBlock = regexp.MustCompile(`(?ms)^# BEGIN LITEROUTER\n.*?^# END LITEROUTER\n?`)

func applyCodex(home string, request Request) (string, error) {
	root := codexRoot(home)
	configPath := filepath.Join(root, "config.toml")
	authPath := filepath.Join(root, "auth.json")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create codex config dir: %w", err)
	}
	subagent := request.SubagentModel
	if subagent == "" {
		subagent = request.Model
	}
	// backup once
	for _, path := range []string{configPath, authPath} {
		backup := path + ".literouter.bak"
		if raw, err := os.ReadFile(path); err == nil {
			if _, err := os.Stat(backup); err != nil {
				_ = os.WriteFile(backup, raw, 0o600)
			}
		}
	}
	text := ""
	if raw, err := os.ReadFile(configPath); err == nil {
		text = string(raw)
	}
	text = literouterBlock.ReplaceAllString(text, "")
	block := fmt.Sprintf(`# BEGIN LITEROUTER
model = %s
model_provider = "literouter"

[model_providers.literouter]
name = "LiteRouter"
base_url = %s
wire_api = "responses"

[agents.subagent]
model = %s
# END LITEROUTER
`, jsonString(request.Model), jsonString(normalizeBase(request.BaseURL, true)), jsonString(subagent))
	out := strings.TrimRight(text, "\n")
	if strings.TrimSpace(out) != "" {
		out += "\n\n"
	}
	out += block
	if err := writeFileAtomic(configPath, []byte(out)); err != nil {
		return "", err
	}

	auth := map[string]any{}
	if raw, err := os.ReadFile(authPath); err == nil {
		_ = json.Unmarshal(raw, &auth)
	}
	auth["auth_mode"] = "apikey"
	auth["OPENAI_API_KEY"] = request.Token
	if err := writeJSONAtomic(authPath, auth); err != nil {
		return "", err
	}
	return configPath, nil
}

// resetCodex removes the LiteRouter block from config.toml and undoes the two
// auth.json keys apply writes. As with Claude, the backups are not renamed back into
// place: doing so threw away every unrelated setting — and, for auth.json, whatever
// OAuth state the user had — that arrived after the first apply.
func resetCodex(home string) (string, error) {
	root := codexRoot(home)
	configPath := filepath.Join(root, "config.toml")
	authPath := filepath.Join(root, "auth.json")

	if raw, err := os.ReadFile(configPath); err == nil {
		text := literouterBlock.ReplaceAllString(string(raw), "")
		text = strings.TrimRight(text, "\n")
		if text != "" {
			text += "\n"
		}
		if err := writeFileAtomic(configPath, []byte(text)); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	if raw, err := os.ReadFile(authPath); err == nil {
		auth := map[string]any{}
		if err := json.Unmarshal(raw, &auth); err != nil {
			return "", fmt.Errorf("parse %s: %w", authPath, err)
		}
		original := map[string]any{}
		if snapshot, err := os.ReadFile(authPath + ".literouter.bak"); err == nil {
			previous := map[string]any{}
			if json.Unmarshal(snapshot, &previous) == nil {
				original = previous
			}
		}
		// auth_mode is restored rather than dropped, so an account that was on ChatGPT
		// OAuth before does not come back as a broken apikey setup.
		for _, key := range []string{"OPENAI_API_KEY", "auth_mode"} {
			if value, existed := original[key]; existed {
				auth[key] = value
			} else {
				delete(auth, key)
			}
		}
		if err := writeJSONAtomic(authPath, auth); err != nil {
			return "", err
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}

	for _, path := range []string{configPath, authPath} {
		_ = os.Remove(path + ".literouter.bak")
	}
	return configPath, nil
}

func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func writeJSONAtomic(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeFileAtomic(path, raw)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".literouter.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
