package clisetup

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type Tool string
type Action string

const (
	ToolClaude Tool   = "claude"
	ToolCodex  Tool   = "codex"
	ToolOMP    Tool   = "omp"
	Apply      Action = "apply"
	Reset      Action = "reset"
)

type Request struct {
	Tool          Tool
	Action        Action
	BaseURL       string
	Token         string
	Model         string
	SubagentModel string
	FableModel    string
	OpusModel     string
	SonnetModel   string
	HaikuModel    string
	// Effort is Claude Code's own `effortLevel` setting, not an env var. The
	// CLAUDE_CODE_EFFORT_LEVEL env var is deliberately not written: it pins effort for
	// the session and locks /effort out of changing it, which would take a control away
	// from the user rather than give them one.
	Effort string
	// MaxContext is the model's real context window as the form supplies it: empty to
	// leave Claude Code's own belief alone, "auto" to use what LiteRouter knows, or an
	// explicit token count.
	MaxContext string
	// CatalogContextWindow is what "auto" resolves to — the smallest window among the
	// models this request configures, since the client setting it feeds is global while
	// the window is per-model, and only the smallest is safe for every turn. Supplied by
	// the caller because resolving it needs the catalog. Ignored unless MaxContext is
	// "auto".
	CatalogContextWindow int
	// OMPModels is the catalog of models to declare in omp's models.yml. omp does not
	// discover a custom provider's models — it must be given a hand-declared list — so
	// the card writes the whole LiteRouter catalog rather than a single selected model.
	// Supplied by the caller because only the dashboard can reach the catalog. Used only
	// when Tool is OMP.
	OMPModels []OMPModel
}

// OMPModel is one model declared in the `models:` list omp writes for a custom provider.
// contextWindow defaults to 128000 and maxTokens to 16384 in omp when omitted, so both
// are filled from the catalog when known to keep omp's auto-compaction budgeting honest.
type OMPModel struct {
	ID            string
	Name          string
	ContextWindow int
	MaxTokens     int
	Reasoning     bool
}

// Draft is the model selection a user last applied, remembered by LiteRouter itself.
//
// It exists because Reset deliberately strips LiteRouter's keys out of the host
// client's config, which is also the only place those values were stored — so a reset
// used to erase the selection along with it, and re-applying meant retyping every
// field. The draft is LiteRouter's own copy, untouched by Reset.
//
// The auth token is not part of it. That value comes from the running instance, and a
// copy on disk would be a credential stored for no reason.
type Draft struct {
	BaseURL       string `json:"base_url,omitempty"`
	Model         string `json:"model,omitempty"`
	SubagentModel string `json:"subagent_model,omitempty"`
	FableModel    string `json:"fable_model,omitempty"`
	OpusModel     string `json:"opus_model,omitempty"`
	SonnetModel   string `json:"sonnet_model,omitempty"`
	HaikuModel    string `json:"haiku_model,omitempty"`
	Effort        string `json:"effort,omitempty"`
	// MaxContext is kept unresolved — "auto", a number, or empty — rather than as the
	// integer that was applied. "auto" has to stay "auto": the window it stands for is
	// learned at runtime, so a draft holding last week's number would re-apply a stale
	// one instead of picking up what has been learned since.
	MaxContext string `json:"max_context,omitempty"`
}

// Draft captures the reusable part of a request.
func (r Request) Draft() Draft {
	return Draft{
		BaseURL: r.BaseURL, Model: r.Model, SubagentModel: r.SubagentModel,
		FableModel: r.FableModel, OpusModel: r.OpusModel,
		SonnetModel: r.SonnetModel, HaikuModel: r.HaikuModel,
		Effort: r.Effort, MaxContext: r.MaxContext,
	}
}

// Request rebuilds a form-shaped request from a draft, for rendering only. Tool,
// action and token are supplied by whoever is about to act on it.
func (d Draft) Request() Request {
	return Request{
		BaseURL: d.BaseURL, Model: d.Model, SubagentModel: d.SubagentModel,
		FableModel: d.FableModel, OpusModel: d.OpusModel,
		SonnetModel: d.SonnetModel, HaikuModel: d.HaikuModel,
		Effort: d.Effort, MaxContext: d.MaxContext,
	}
}

// HasSelection reports whether a draft carries anything worth remembering.
//
// BaseURL deliberately does not count. The handler defaults it to the instance
// serving the request, so it is always set and a "is this struct zero" test would
// call every submission meaningful — including a reset from an empty form, which
// would then overwrite a good draft with nothing.
func (d Draft) HasSelection() bool {
	for _, value := range []string{
		d.Model, d.SubagentModel, d.FableModel, d.OpusModel, d.SonnetModel, d.HaikuModel,
		d.Effort, d.MaxContext,
	} {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// EffortLevels are the values Claude Code's settings parser accepts for effortLevel.
// Empty means "leave the user's own effortLevel alone", which is why it is not in this
// list either. `ultracode` is intentionally absent: it is a session-only /effort mode
// that combines xhigh with dynamic workflow orchestration, not a persisted effortLevel.
var EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// Ultracode is a Claude Code session-only mode, not a settings.json effortLevel. It
// cannot be reproduced by LiteRouter's persistent CLI setup form because it also enables
// dynamic workflow orchestration in the client.
const Ultracode = "ultracode"

// MaxContextAuto is the MaxContext value meaning "use the window LiteRouter resolved".
const MaxContextAuto = "auto"

// minManagedContextWindow rejects a window too small to run an agent in. Below this a
// single tool result can exceed the compact threshold, so Claude Code would compact on
// every turn and never make progress — a worse failure than the one this setting fixes.
const minManagedContextWindow = 32768

// ContextWindow resolves MaxContext to the window to hand the client, or zero to leave
// the client's own belief alone.
func (r Request) ContextWindow() int {
	switch value := strings.TrimSpace(strings.ToLower(r.MaxContext)); value {
	case "":
		return 0
	case MaxContextAuto:
		if r.CatalogContextWindow < minManagedContextWindow {
			return 0
		}
		return r.CatalogContextWindow
	default:
		window, err := strconv.Atoi(value)
		if err != nil || window < minManagedContextWindow {
			return 0
		}
		return window
	}
}

// validMaxContext reports whether a form value is one this package can act on.
func validMaxContext(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" || value == MaxContextAuto {
		return true
	}
	window, err := strconv.Atoi(value)
	return err == nil && window >= minManagedContextWindow
}

func validEffort(effort string) bool {
	for _, level := range EffortLevels {
		if effort == level {
			return true
		}
	}
	return false
}

type Artifact struct {
	Filename string
	Content  []byte
}

func Generate(request Request) (Artifact, error) {
	if err := request.validate(); err != nil {
		return Artifact{}, err
	}
	switch request.Tool {
	case ToolClaude:
		if request.Action == Apply {
			return Artifact{Filename: "literouter-claude-apply.sh", Content: claudeApply(request)}, nil
		}
		return Artifact{Filename: "literouter-claude-reset.sh", Content: claudeReset()}, nil
	case ToolCodex:
		if request.Action == Apply {
			return Artifact{Filename: "literouter-codex-apply.sh", Content: codexApply(request)}, nil
		}
		return Artifact{Filename: "literouter-codex-reset.sh", Content: codexReset()}, nil
	case ToolOMP:
		if request.Action == Apply {
			return Artifact{Filename: "literouter-omp-apply.sh", Content: ompApply(request)}, nil
		}
		return Artifact{Filename: "literouter-omp-reset.sh", Content: ompReset()}, nil
	default:
		return Artifact{}, fmt.Errorf("unsupported CLI tool %q", request.Tool)
	}
}

func (request Request) validate() error {
	if request.Tool != ToolClaude && request.Tool != ToolCodex && request.Tool != ToolOMP {
		return fmt.Errorf("tool must be claude, codex, or omp")
	}
	if request.Action != Apply && request.Action != Reset {
		return fmt.Errorf("action must be apply or reset")
	}
	if request.Action == Reset {
		return nil
	}
	parsed, err := url.Parse(request.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("base URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" && parsed.Path != "/v1" {
		return fmt.Errorf("base URL path must be empty or /v1")
	}
	for name, value := range map[string]string{
		"token": request.Token, "model": request.Model, "subagent model": request.SubagentModel,
		"Fable model": request.FableModel, "Opus model": request.OpusModel,
		"Sonnet model": request.SonnetModel, "Haiku model": request.HaikuModel,
		"effort": request.Effort, "max context": request.MaxContext,
	} {
		if strings.ContainsAny(value, "\x00\r\n") || len(value) > 512 {
			return fmt.Errorf("%s contains invalid characters or is too long", name)
		}
	}
	if request.Token == "" {
		return fmt.Errorf("token is required")
	}
	// omp is configured as a plain provider: the catalog is written wholesale, and the
	// user picks the model themselves inside omp, so a single "model" selection is not
	// part of the contract. Claude and Codex still need one.
	if request.Model == "" && request.Tool != ToolOMP {
		return fmt.Errorf("model is required")
	}
	// Empty is allowed and means "do not manage effortLevel"; anything else must be a
	// level the client will actually persist, or the write is silently ignored.
	if request.Effort != "" && !validEffort(request.Effort) {
		return fmt.Errorf("effort must be empty or one of %s", strings.Join(EffortLevels, ", "))
	}
	if !validMaxContext(request.MaxContext) {
		return fmt.Errorf("max context must be empty, %s, or a token count of at least %d",
			MaxContextAuto, minManagedContextWindow)
	}
	return nil
}

func payload(value any) string {
	encoded, _ := json.Marshal(value)
	return base64.StdEncoding.EncodeToString(encoded)
}

func normalizeBase(raw string, withV1 bool) string {
	base := strings.TrimRight(raw, "/")
	base = strings.TrimSuffix(base, "/v1")
	if withV1 {
		return base + "/v1"
	}
	return base
}

func script(python string, data any) []byte {
	return []byte("#!/bin/sh\nset -eu\numask 077\ncommand -v python3 >/dev/null 2>&1 || { echo 'Python 3 is required' >&2; exit 1; }\nPAYLOAD='" + payload(data) + "'\npython3 - \"$PAYLOAD\" <<'PY'\n" + python + "\nPY\n")
}
