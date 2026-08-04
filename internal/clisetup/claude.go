package clisetup

import "strings"

func claudeApply(request Request) []byte {
	env := map[string]string{
		"ANTHROPIC_BASE_URL":   normalizeBase(request.BaseURL, false),
		"ANTHROPIC_AUTH_TOKEN": request.Token,
		"ANTHROPIC_MODEL":      request.Model,
	}
	for key, value := range map[string]string{
		"CLAUDE_CODE_SUBAGENT_MODEL":     request.SubagentModel,
		"ANTHROPIC_DEFAULT_FABLE_MODEL":  request.FableModel,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   request.OpusModel,
		"ANTHROPIC_DEFAULT_SONNET_MODEL": request.SonnetModel,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  request.HaikuModel,
	} {
		if value != "" {
			env[key] = value
		}
	}
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
env.update(payload["env"])
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
for key in [
    "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL",
    "CLAUDE_CODE_SUBAGENT_MODEL",
    "ANTHROPIC_DEFAULT_FABLE_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
    "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
]:
    if key in original_env: env[key] = original_env[key]
    else: env.pop(key, None)
if not env: current.pop("env", None)
else: current["env"] = env
tmp = root / "settings.json.literouter.tmp"
tmp.write_text(json.dumps(current, indent=2) + "\n")
os.chmod(tmp, 0o600)
os.replace(tmp, path)
backup.unlink(missing_ok=True)
print(f"Claude Code restored to its own configuration: {path}")`, map[string]string{})
}
