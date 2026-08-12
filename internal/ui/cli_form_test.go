package ui

import (
	"regexp"
	"strings"
	"testing"

	"literouter/internal/clisetup"
	"literouter/internal/pool"
)

func renderCLITab(t *testing.T, data viewData) string {
	t.Helper()
	service, err := New(pool.New(nil), "token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := service.tab.ExecuteTemplate(&out, "tab-cli", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return out.String()
}

// Folding rarely-used fields away is only safe if a section that holds something the
// operator set opens itself. A collapsed section hiding a live override is how people lose
// an afternoon to "why is this model being used".
//
// But "set" is not the same as "surprising", and treating them as the same made the folding
// useless: the roles are nearly always filled with the same model as Default, so the section
// was always open and the operator asked for the clutter to go away. A role equal to Default
// changes nothing and may be hidden; one that differs may not.
func TestSetupFormOpensASectionOnlyWhenItHidesSomethingSurprising(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup clisetup.Request
		open  bool
	}{
		{"nothing set", clisetup.Request{Model: "cx/a"}, false},
		{"roles echo the default", clisetup.Request{
			Model: "cx/a", FableModel: "cx/a", OpusModel: "cx/a", SonnetModel: "cx/a", HaikuModel: "cx/a",
		}, false},
		{"a role diverges", clisetup.Request{Model: "cx/a", HaikuModel: "cx/cheap"}, true},
		{"fable diverges", clisetup.Request{Model: "cx/a", FableModel: "cx/strong"}, true},
		// Subagent counts even when it equals Default: setting it at all overrides every
		// agent's own model: frontmatter, which is the surprise.
		{"subagent set to the default", clisetup.Request{Model: "cx/a", SubagentModel: "cx/a"}, true},
		{"session effort set", clisetup.Request{Model: "cx/a", Effort: "high"}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			page := renderCLITab(t, viewData{ClaudeSetup: testCase.setup})
			if got := strings.Contains(page, `class="form-section" open`); got != testCase.open {
				t.Errorf("section open = %v, want %v", got, testCase.open)
			}
		})
	}
}

// What stays in the open is what an operator has to decide: which model serves the turns,
// and how much context it can hold. The endpoint is not one of those — an empty value
// resolves to the address the dashboard was loaded from, which is the answer in every case
// except a CLI that reaches LiteRouter differently than the browser does.
func TestSetupFormLeavesOnlyTheDecisionsInTheOpen(t *testing.T) {
	page := renderCLITab(t, viewData{ClaudeSetup: clisetup.Request{Model: "cx/a"}})
	claude := claudeCardMarkup(t, page)
	visible, folded := fieldsByVisibility(claude)
	for _, field := range []string{"model", "max_context"} {
		if !visible[field] {
			t.Errorf("%s must be visible without expanding a section", field)
		}
	}
	for _, field := range []string{"base_url", "fable_model", "opus_model", "sonnet_model", "haiku_model", "subagent_model", "effort"} {
		if !folded[field] {
			t.Errorf("%s should be folded away; it is not a first-setup decision", field)
		}
	}
}

// A required field inside a collapsed <details> cannot be focused, so the browser refuses to
// submit the form and reports nothing the operator can act on. Endpoint lost its `required`
// when it was folded away, and this is what keeps the two facts tied together.
func TestSetupFormNeverRequiresAFoldedField(t *testing.T) {
	page := renderCLITab(t, viewData{ClaudeSetup: clisetup.Request{Model: "cx/a"}})
	_, folded := fieldsByVisibility(claudeCardMarkup(t, page))
	for field := range folded {
		if regexp.MustCompile(`name="` + field + `"[^>]*\brequired\b`).MatchString(page) {
			t.Errorf("%s is folded away but still required", field)
		}
	}
}

func claudeCardMarkup(t *testing.T, page string) string {
	t.Helper()
	end := strings.Index(page, `formaction="/ui/setup/claude/apply"`)
	if end < 0 {
		t.Fatal("claude apply button not found")
	}
	start := strings.LastIndex(page[:end], `<article class="cli-card">`)
	if start < 0 {
		t.Fatal("claude card not found")
	}
	return page[start:end]
}

// Splits the card's fields by whether they sit inside a <details>, by tracking nesting
// depth across the markup in order.
func fieldsByVisibility(card string) (visible, folded map[string]bool) {
	visible, folded = map[string]bool{}, map[string]bool{}
	depth := 0
	for _, match := range regexp.MustCompile(`<details|</details>|name="([a-z_]+)"`).FindAllStringSubmatch(card, -1) {
		switch {
		case match[0] == "<details":
			depth++
		case match[0] == "</details>":
			depth--
		case depth > 0:
			folded[match[1]] = true
		default:
			visible[match[1]] = true
		}
	}
	return visible, folded
}

func TestSetupFormKeepsEveryFieldItUsedToSubmit(t *testing.T) {
	// The grouping is presentational. Losing a field here would silently stop writing
	// part of the CLI configuration, which no test of the handler would catch.
	page := renderCLITab(t, viewData{ClaudeSetup: clisetup.Request{Model: "cx/a"}})
	for _, field := range []string{
		"base_url", "model", "fable_model", "opus_model", "sonnet_model", "haiku_model",
		"subagent_model", "effort", "max_context",
		"plan_model", "long_context_model", "long_context_percent", "text_only_models", "image_model",
	} {
		if !strings.Contains(page, `name="`+field+`"`) {
			t.Errorf("form no longer submits %q", field)
		}
	}
}

func TestSetupFormSaysEffortIsSessionWide(t *testing.T) {
	// One effort for the whole session is Claude Code's own model, not a limitation the
	// dashboard should leave unexplained now that per-model overrides exist.
	page := renderCLITab(t, viewData{ClaudeSetup: clisetup.Request{Model: "cx/a"}})
	if !strings.Contains(page, "Session effort") {
		t.Error("the label must say the setting is session-wide")
	}
	if !strings.Contains(page, "Reasoning effort under Models") {
		t.Error("it must point at where a single model can be forced")
	}
}
