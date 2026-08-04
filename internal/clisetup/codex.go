package clisetup

func codexApply(request Request) []byte {
	if request.SubagentModel == "" {
		request.SubagentModel = request.Model
	}
	data := map[string]string{
		"base_url": normalizeBase(request.BaseURL, true), "token": request.Token,
		"model": request.Model, "subagent_model": request.SubagentModel,
	}
	return script(`import base64, json, os, pathlib, re, sys
payload = json.loads(base64.b64decode(sys.argv[1]))
root = pathlib.Path(os.environ.get("CODEX_HOME", pathlib.Path.home() / ".codex"))
config = root / "config.toml"
auth = root / "auth.json"
root.mkdir(parents=True, exist_ok=True)
for path in (config, auth):
    backup = pathlib.Path(str(path) + ".literouter.bak")
    if path.exists() and not backup.exists(): backup.write_bytes(path.read_bytes())
text = config.read_text() if config.exists() else ""
text = re.sub(r"(?ms)^# BEGIN LITEROUTER\n.*?^# END LITEROUTER\n?", "", text)
block = f'''# BEGIN LITEROUTER
model = {json.dumps(payload["model"])}
model_provider = "literouter"

[model_providers.literouter]
name = "LiteRouter"
base_url = {json.dumps(payload["base_url"])}
wire_api = "responses"

[agents.subagent]
model = {json.dumps(payload["subagent_model"])}
# END LITEROUTER
'''
tmp = root / "config.toml.literouter.tmp"
tmp.write_text(text.rstrip() + ("\n\n" if text.strip() else "") + block)
os.chmod(tmp, 0o600)
os.replace(tmp, config)
current = json.loads(auth.read_text() or "{}") if auth.exists() else {}
current["auth_mode"] = "apikey"
current["OPENAI_API_KEY"] = payload["token"]
tmp = root / "auth.json.literouter.tmp"
tmp.write_text(json.dumps(current, indent=2) + "\n")
os.chmod(tmp, 0o600)
os.replace(tmp, auth)
print(f"LiteRouter configured: {config}")`, data)
}

func codexReset() []byte {
	return script(`import base64, json, os, pathlib, re, sys
root = pathlib.Path(os.environ.get("CODEX_HOME", pathlib.Path.home() / ".codex"))
for name in ("config.toml", "auth.json"):
    path = root / name
    backup = pathlib.Path(str(path) + ".literouter.bak")
    if not path.exists():
        backup.unlink(missing_ok=True)
        continue
    if name == "config.toml":
        # Remove exactly the fenced block; the rest of the file is the user's.
        text = re.sub(r"(?ms)^# BEGIN LITEROUTER\n.*?^# END LITEROUTER\n?", "", path.read_text())
        tmp = root / "config.toml.literouter.tmp"
        tmp.write_text(text.rstrip() + ("\n" if text.strip() else ""))
    else:
        current = json.loads(path.read_text() or "{}")
        # auth_mode is restored, not dropped, so a ChatGPT OAuth login does not come
        # back as a half-configured apikey setup.
        original = {}
        if backup.exists():
            try: original = json.loads(backup.read_text() or "{}")
            except ValueError: original = {}
        for key in ("OPENAI_API_KEY", "auth_mode"):
            if key in original: current[key] = original[key]
            else: current.pop(key, None)
        tmp = root / "auth.json.literouter.tmp"
        tmp.write_text(json.dumps(current, indent=2) + "\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)
    backup.unlink(missing_ok=True)
print("Codex restored to its own configuration")`, map[string]string{})
}
