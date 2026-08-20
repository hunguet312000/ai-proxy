package clisetup

// providerName is the omp provider entry LiteRouter manages in models.yml. Deliberately
// generic ("literouter") rather than derived from the base URL: the whole point is a
// stable, discoverable name the user can select with `omp --model literouter/<id>`.
const providerName = "literouter"

// ompModel is the JSON shape shipped to the apply script. Kept separate from OMPModel
// because the script needs the fields verbatim plus the api marker omp requires on
// hand-declared models; input is always text here, which is safe for any LiteRouter model.
type ompModel struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int    `json:"contextWindow"`
	MaxTokens     int    `json:"maxTokens"`
	Reasoning     bool   `json:"reasoning"`
	Input         string `json:"input"`
}

// ompProvider is the provider block shipped to the script. baseUrl carries /v1 because
// omp posts to {baseUrl}/chat/completions verbatim with no path normalization.
type ompProvider struct {
	Provider  string     `json:"provider"`
	BaseURL   string     `json:"baseUrl"`
	APIKey    string     `json:"apiKey"`
	APIMode   string     `json:"api"`
	ModelList []ompModel `json:"models"`
}

func ompApply(request Request) []byte {
	models := request.OMPModels
	if len(models) == 0 && request.Model != "" {
		models = []OMPModel{{ID: request.Model}}
	}
	data := ompProvider{
		Provider:  providerName,
		BaseURL:   normalizeBase(request.BaseURL, true), // keep /v1 (see ompProvider doc)
		APIKey:    request.Token,
		APIMode:   "openai-completions",
		ModelList: ompModelList(models),
	}
	return script(`import base64, json, os, pathlib, re, sys
try:
    import yaml as pyyaml
except ImportError:
    pyyaml = None
def provider_value(p):
    value = {
        "baseUrl": p["baseUrl"],
        "apiKey": p["apiKey"],
        "api": "openai-completions",
    }
    models = p.get("models") or []
    if models:
        value["models"] = []
        for m in models:
            entry = {"id": m["id"]}
            if m.get("name"): entry["name"] = m["name"]
            if m.get("contextWindow"): entry["contextWindow"] = m["contextWindow"]
            if m.get("maxTokens"): entry["maxTokens"] = m["maxTokens"]
            if m.get("reasoning"): entry["reasoning"] = True
            entry["input"] = ["text"]
            value["models"].append(entry)
    return value
payload = json.loads(base64.b64decode(sys.argv[1]))
root = pathlib.Path(os.environ.get("PI_CODING_AGENT_DIR", pathlib.Path.home() / ".omp" / "agent"))
path = root / "models.yml"
root.mkdir(parents=True, exist_ok=True)
backup = pathlib.Path(str(path) + ".literouter.bak")
if path.exists() and not backup.exists():
    backup.write_bytes(path.read_bytes())
existing = path.read_text() if path.exists() else ""
if pyyaml is not None:
    doc = pyyaml.safe_load(existing) or {}
    doc.setdefault("providers", {})[payload["provider"]] = provider_value(payload)
    out = pyyaml.safe_dump(doc, sort_keys=False, default_flow_style=False, allow_unicode=True)
else:
    # No PyYAML: fall back to a fenced block, but only when the file has no
    # providers: key of its own — appending a second one would drop the user's
    # other providers. The dashboard Apply button always uses Go's YAML merge,
    # so this path is the last resort.
    if re.search(r"(?m)^providers:", existing):
        sys.exit("models.yml already has a providers: key and PyYAML is not installed; use the dashboard Apply button instead")
    existing = re.sub(r"(?ms)^# BEGIN LITEROUTER\n.*?^# END LITEROUTER\n?", "", existing)
    body = ["%s:" % payload["provider"]]
    def q(v): return json.dumps(v, ensure_ascii=False)
    body.append("  baseUrl: %s" % q(payload["baseUrl"]))
    body.append("  apiKey: %s" % q(payload["apiKey"]))
    body.append("  api: openai-completions")
    models = payload.get("models") or []
    if models:
        body.append("  models:")
        for m in models:
            body.append("    - id: %s" % q(m["id"]))
            if m.get("name"): body.append("      name: %s" % q(m["name"]))
            if m.get("contextWindow"): body.append("      contextWindow: %d" % m["contextWindow"])
            if m.get("maxTokens"): body.append("      maxTokens: %d" % m["maxTokens"])
            if m.get("reasoning"): body.append("      reasoning: true")
            body.append("      input: [text]")
    block = "# BEGIN LITEROUTER\nproviders:\n  " + "\n  ".join(body) + "\n# END LITEROUTER\n"
    out = existing.rstrip() + ("\n\n" if existing.strip() else "") + block
tmp = root / "models.yml.literouter.tmp"
tmp.write_text(out)
os.chmod(tmp, 0o600)
os.replace(tmp, path)
print("LiteRouter configured: %s" % path)`, data)
}

func ompReset() []byte {
	return script(`import base64, os, pathlib, re, sys
try:
    import yaml as pyyaml
except ImportError:
    pyyaml = None
root = pathlib.Path(os.environ.get("PI_CODING_AGENT_DIR", pathlib.Path.home() / ".omp" / "agent"))
path = root / "models.yml"
backup = pathlib.Path(str(path) + ".literouter.bak")
if not path.exists():
    backup.unlink(missing_ok=True)
    print("omp models.yml does not exist")
    raise SystemExit(0)
text = path.read_text()
if pyyaml is not None:
    doc = pyyaml.safe_load(text) or {}
    providers = doc.get("providers")
    if isinstance(providers, dict):
        providers.pop("literouter", None)
    out = pyyaml.safe_dump(doc, sort_keys=False, default_flow_style=False, allow_unicode=True)
else:
    # Without PyYAML, remove any fenced block the apply fallback wrote.
    out = re.sub(r"(?ms)^# BEGIN LITEROUTER\n.*?^# END LITEROUTER\n?", "", text).rstrip()
    if out.strip():
        out += "\n"
tmp = root / "models.yml.literouter.tmp"
tmp.write_text(out)
os.chmod(tmp, 0o600)
os.replace(tmp, path)
print("omp restored: %s" % path)`, map[string]string{})
}

// ompModelList converts a catalog of OMPModel into the JSON payload the apply script
// expects, sorted for a stable file (models.yml is diffed by hand and by users).
func ompModelList(models []OMPModel) []ompModel {
	out := make([]ompModel, 0, len(models))
	for _, m := range models {
		out = append(out, ompModel{
			ID: m.ID, Name: m.Name, ContextWindow: m.ContextWindow,
			MaxTokens: m.MaxTokens, Reasoning: m.Reasoning, Input: "text",
		})
	}
	return out
}
