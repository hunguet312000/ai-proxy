package clisetup

import (
	"strconv"
	"strings"
)

// claudeEnvFor is every environment key LiteRouter manages in Claude Code's
// settings.json, and the only place that decides them. An empty value means the key must
// be absent, so a setting that is turned off is removed rather than left behind.
//
// It exists because there are two ways to apply a configuration — the downloadable script
// and the direct write — and they were each building this map themselves. They drifted:
// the script never wrote CLAUDE_CODE_MAX_CONTEXT_TOKENS or CLAUDE_AUTOCOMPACT_PCT_OVERRIDE
// at all, so anyone who used it got a client that knew nothing about the window LiteRouter
// had resolved and compacted on its own 200k assumption. Nothing caught it, because the
// test for those keys only exercised the direct path.
func claudeEnvFor(request Request) map[string]string {
	// Telling the client the real window is the whole fix for a non-Anthropic model:
	// left to itself it either assumes 200k, or — with the [1m] suffix, which is a
	// client-side marker the proxy never sees — assumes 1M, and compacts far too late
	// either way. Both keys move together because the window alone is not enough: at
	// window − 13,000 the compaction request is already twice the window. See
	// autoCompactPercent and CompactRequestFits.
	//
	// Only for non-claude-* ids, and not by choice: the client consults
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS only when the model id does not start with
	// "claude-", so writing it for one of those is a no-op that would still leave the
	// percentage override in force against a window it never applied to.
	maxContext, autoCompactPct := "", ""
	if window := request.ContextWindow(); window > 0 && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(request.Model)), "claude-") {
		maxContext = strconv.Itoa(window)
		autoCompactPct = strconv.Itoa(autoCompactPercent)
	}
	return map[string]string{
		"ANTHROPIC_BASE_URL":   normalizeBase(request.BaseURL, false),
		"ANTHROPIC_AUTH_TOKEN": request.Token,
		"ANTHROPIC_MODEL":      request.Model,
		// Subagent model is deliberately not defaulted to request.Model the way Codex
		// does: Claude Code treats CLAUDE_CODE_SUBAGENT_MODEL as the highest-priority
		// override, so writing it unconditionally would silently void every agent's own
		// `model:` frontmatter.
		"CLAUDE_CODE_SUBAGENT_MODEL":      request.SubagentModel,
		"ANTHROPIC_DEFAULT_FABLE_MODEL":   request.FableModel,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":    request.OpusModel,
		"ANTHROPIC_DEFAULT_SONNET_MODEL":  request.SonnetModel,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":   request.HaikuModel,
		"CLAUDE_CODE_MAX_CONTEXT_TOKENS":  maxContext,
		"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": autoCompactPct,
	}
}

func claudeApply(request Request) []byte {
	env := claudeEnvFor(request)
	// The payload is nested rather than a flat env map because effortLevel is a
	// top-level setting, not an env var — the same key /effort writes.
	payload := map[string]any{"env": env}
	if effort := strings.TrimSpace(request.Effort); effort != "" {
		payload["effortLevel"] = effort
	}
	return script(`import base64, json, os, pathlib, sys
payload = json.loads(base64.b64decode(sys.argv[1]))
root = pathlib.Path(os.environ.get("CLAUDE_CONFIG_DIR", pathlib.Path.home() / ".claude"))
path = root / "settings.json"
backup = root / "settings.json.literouter.bak"
root.mkdir(parents=True, exist_ok=True)
current = {}
if path.exists():
    current = json.loads(path.read_text() or "{}")
    if not backup.exists():
        backup.write_bytes(path.read_bytes())
env = current.setdefault("env", {})
# An empty value means the setting is off, so the key is removed rather than left
# behind pointing at a window or a model that is no longer configured.
for key, value in payload["env"].items():
    if value:
        env[key] = value
    else:
        env.pop(key, None)
# Only when a level was chosen: an empty field must not clear an effortLevel the
# user set themselves with /effort.
if payload.get("effortLevel"):
    current["effortLevel"] = payload["effortLevel"]
tmp = root / "settings.json.literouter.tmp"
tmp.write_text(json.dumps(current, indent=2) + "\n")
os.chmod(tmp, 0o600)
os.replace(tmp, path)
print(f"LiteRouter configured: {path}")`, payload)
}

// claudeReset takes the keys to revert from claudeManagedEnv rather than listing them,
// which is the same reason claudeEnvFor exists. Its own copy of the list had fallen two
// keys behind: a reset left CLAUDE_CODE_MAX_CONTEXT_TOKENS and
// CLAUDE_AUTOCOMPACT_PCT_OVERRIDE in settings.json, so a client the user believed was
// back on stock behaviour went on compacting against a window LiteRouter had chosen for
// it, with nothing left in the file to explain where the number came from.
func claudeReset() []byte {
	return script(`import base64, json, os, pathlib, sys
root = pathlib.Path(os.environ.get("CLAUDE_CONFIG_DIR", pathlib.Path.home() / ".claude"))
path = root / "settings.json"
backup = root / "settings.json.literouter.bak"
if not path.exists():
    backup.unlink(missing_ok=True)
    print("Claude Code settings do not exist")
    raise SystemExit(0)
current = json.loads(path.read_text() or "{}")
# The backup is only consulted for the keys LiteRouter owns. Restoring the whole
# file would discard every permission, hook and preference added since the apply.
original_env = {}
if backup.exists():
    try:
        original_env = json.loads(backup.read_text() or "{}").get("env", {}) or {}
    except ValueError:
        original_env = {}
env = current.get("env", {})
for key in json.loads(base64.b64decode(sys.argv[1]))["keys"]:
    if key in original_env: env[key] = original_env[key]
    else: env.pop(key, None)
if not env: current.pop("env", None)
else: current["env"] = env
tmp = root / "settings.json.literouter.tmp"
tmp.write_text(json.dumps(current, indent=2) + "\n")
os.chmod(tmp, 0o600)
os.replace(tmp, path)
backup.unlink(missing_ok=True)
print(f"Claude Code restored to its own configuration: {path}")`, map[string][]string{"keys": claudeManagedEnv()})
}
