package ui

import (
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

func TestSetupFormOpensASectionHoldingConfiguredValues(t *testing.T) {
	// Folding rarely-used fields away is only safe if a section that holds something
	// the operator set opens itself. A collapsed section hiding a live override is how
	// people lose an afternoon to "why is this model being used".
	page := renderCLITab(t, viewData{
		ClaudeSetup: clisetup.Request{Model: "cx/a", SubagentModel: "cx/b"},
	})
	if !strings.Contains(page, `class="form-section" open`) {
		t.Error("a section holding a configured role must render open")
	}
}

func TestSetupFormCollapsesSectionsThatHoldNothing(t *testing.T) {
	page := renderCLITab(t, viewData{ClaudeSetup: clisetup.Request{Model: "cx/a"}})
	if strings.Contains(page, `class="form-section" open`) {
		t.Error("an empty section must stay folded away")
	}
	// The two fields that matter on first setup stay in the open.
	for _, field := range []string{`name="base_url"`, `name="model"`} {
		if !strings.Contains(page, field) {
			t.Errorf("field %s must remain visible without expanding a section", field)
		}
	}
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
