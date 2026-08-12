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
	// Telling the client the real window is the whole fix for a non-Anthropic model: left
	// to itself, Claude Code 2.1.228 enforces 200k for a model id it does not recognize, so
	// a model measured at 370k would be compacted at 187k for no reason.
	//
	// Only for non-claude-* ids, and not by choice: the client consults
	// CLAUDE_CODE_MAX_CONTEXT_TOKENS only when the model id does not start with "claude-",
	// so writing it for one of those is a no-op.
	maxContext := ""
	if window := request.ContextWindow(); window > 0 && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(request.Model)), "claude-") {
		maxContext = strconv.Itoa(window)
	}
	return map[string]string{
		"ANTHROPIC_BASE_URL":   normalizeBase(request.BaseURL, false),
		"ANTHROPIC_AUTH_TOKEN": request.Token,
		"ANTHROPIC_MODEL":      request.Model,
		// Subagent model is deliberately not defaulted to request.Model the way Codex
		// does: Claude Code treats CLAUDE_CODE_SUBAGENT_MODEL as the highest-priority
		// override, so writing it unconditionally would silently void every agent's own
		// `model:` frontmatter.
		"CLAUDE_CODE_SUBAGENT_MODEL":     request.SubagentModel,
		"ANTHROPIC_DEFAULT_FABLE_MODEL":  request.FableModel,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   request.OpusModel,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": request.SonnetModel,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  request.HaikuModel,
		// The only window key written, and deliberately: raising the window can only move
		// compaction later.
		"CLAUDE_CODE_MAX_CONTEXT_TOKENS": maxContext,
		// Always empty, which means "remove this key" — so an apply or a reset cleans these
		// out of a settings.json that an earlier version of this function wrote them into.
		//
		// They forced the client to compact early: PCT_OVERRIDE=45 made it compact at 45% of
		// the window, AUTO_COMPACT_WINDOW pinned the window it measured that against, and
		// DISABLE_1M stopped a [1m] model id from inflating it. The reason was that a
		// compaction request re-sends the whole conversation on top of the summarization
		// prompt — about 2.06x what it compacts — so the client had to start well under half
		// the window or the compaction request itself could not fit.
		//
		// That reasoning held only while the client was the only thing that could shrink a
		// prompt. The proxy now compresses oversized prompts itself (internal/contextguard),
		// so a client-side compaction at 45% bought nothing and cost minutes per session —
		// and because the client compacts against a size the proxy reports, any error in that
		// number moved the real trigger lower still. Compaction is left to the client's own
		// judgement and to an explicit /compact.
		"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE": "",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "",
		"CLAUDE_CODE_DISABLE_1M_CONTEXT":  "",
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
