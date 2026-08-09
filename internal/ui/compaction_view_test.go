package ui

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"literouter/internal/pool"
	"literouter/internal/storage"
	"literouter/internal/usage"
)

// renderAdvicePanel runs the real template, so a guard that exists only in Go cannot
// pass while the page still renders the button.
func renderAdvicePanel(t *testing.T, advice []usage.CompactionAdvice) string {
	t.Helper()
	service, err := New(pool.New(nil), "token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, ModelHooks{}, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	var out bytes.Buffer
	data := viewData{Advice: advice, AdviceGroups: groupAdvice(advice)}
	if err := service.tab.ExecuteTemplate(&out, "compaction-advice", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	return out.String()
}

func adviceOf(provider, model string, reason usage.Reason, requests, reported int, status string) usage.CompactionAdvice {
	return usage.CompactionAdvice{
		Provider: provider, Model: model, Reason: reason,
		Requests: requests, ReportedRequests: reported, Status: status,
	}
}

func TestGroupAdviceCollapsesOneSharedReasonPerProvider(t *testing.T) {
	// The flat list repeated "upstream does not report cache usage" once per model,
	// which buried the rows that actually said something different.
	groups := groupAdvice([]usage.CompactionAdvice{
		adviceOf("antigravity", "gemini-a", usage.ReasonNoCacheData, 786, 0, "long sentence"),
		adviceOf("antigravity", "gemini-b", usage.ReasonNoCacheData, 306, 16, "long sentence"),
		adviceOf("antigravity", "gemini-c", usage.ReasonNoCacheData, 901, 0, "long sentence"),
	})
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	group := groups[0]
	if group.Note == "" {
		t.Fatal("a group where every model shares a verdict must state it once")
	}
	if !strings.Contains(group.Note, "1993 requests seen") || !strings.Contains(group.Note, "only 16") {
		t.Errorf("note = %q, want the totals pooled and the missing figures named", group.Note)
	}
	for _, row := range group.Models {
		if row.Detail != "" {
			t.Errorf("model %s repeats the group's note", row.Model)
		}
		// The number is what differs down the column, so it stays on the line.
		if row.Metric == "" {
			t.Errorf("model %s lost the figure that distinguishes it", row.Model)
		}
		if row.Title == "" {
			t.Errorf("model %s must keep its full verdict on hover", row.Model)
		}
	}
}

func TestGroupAdviceKeepsPerModelDetailWhenVerdictsDiffer(t *testing.T) {
	groups := groupAdvice([]usage.CompactionAdvice{
		adviceOf("codex", "a", usage.ReasonApplied, 100, 100, "already compacting near 150k"),
		adviceOf("codex", "b", usage.ReasonLearning, 50, 50, "learning: 20/120"),
	})
	if len(groups) != 1 || groups[0].Note != "" {
		t.Fatalf("a mixed group must not claim one shared reason: %+v", groups)
	}
	for _, row := range groups[0].Models {
		if row.Detail == "" {
			t.Errorf("model %s lost its own explanation", row.Model)
		}
		// Short, because the metric beside it already carries the number.
		if len(row.Detail) > 24 {
			t.Errorf("row detail %q is prose, not a verdict", row.Detail)
		}
		if row.Title == "" {
			t.Errorf("model %s lost the full sentence from its tooltip", row.Model)
		}
	}
}

func TestGroupAdviceExcludesModelsThatHaveARecommendation(t *testing.T) {
	// Those belong in the actionable table above, not in the collapsed list.
	groups := groupAdvice([]usage.CompactionAdvice{
		{Provider: "codex", Model: "a", RecommendedWindow: 150_000},
		adviceOf("codex", "b", usage.ReasonLearning, 10, 10, "learning"),
	})
	if len(groups) != 1 || len(groups[0].Models) != 1 || groups[0].Models[0].Model != "b" {
		t.Fatalf("groups = %+v, want only the model without a recommendation", groups)
	}
}

func TestGroupAdviceOrdersBiggestProviderFirstAndLabelsIt(t *testing.T) {
	groups := groupAdvice([]usage.CompactionAdvice{
		adviceOf("cursor", "c1", usage.ReasonLearning, 1, 1, "x"),
		adviceOf("codex", "a1", usage.ReasonLearning, 1, 1, "x"),
		adviceOf("codex", "a2", usage.ReasonLearning, 1, 1, "x"),
	})
	if groups[0].Provider != "codex" || groups[0].Count != 2 {
		t.Fatalf("first group = %+v, want codex with 2 models", groups[0])
	}
	if groups[1].Label != "Cursor" || groups[1].Logo != "cursor" {
		t.Errorf("second group = %q/%q, want the Cursor label and mark", groups[1].Label, groups[1].Logo)
	}
}

func TestGroupAdviceDoesNotCollapseASingleModel(t *testing.T) {
	// With one model there is nothing to deduplicate, and hiding its status behind a
	// group note would lose detail rather than save space.
	groups := groupAdvice([]usage.CompactionAdvice{
		adviceOf("xai", "grok", usage.ReasonLearning, 10, 10, "learning: 39/120"),
	})
	if groups[0].Note != "" || groups[0].Models[0].Detail == "" {
		t.Fatalf("single-model group = %+v, want its own status kept", groups[0])
	}
}

func TestGroupAdviceMergesProviderIdsThatShareALabel(t *testing.T) {
	// Two ids that genuinely name one upstream belong under one heading; showing the
	// same label twice reads as a bug.
	groups := groupAdvice([]usage.CompactionAdvice{
		adviceOf("claude", "a", usage.ReasonLearning, 1, 1, "x"),
		adviceOf("anthropic", "b", usage.ReasonLearning, 1, 1, "x"),
	})
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want the shared label merged into one", len(groups))
	}
	if groups[0].Count != 2 || groups[0].Label != "Claude" {
		t.Errorf("group = %+v, want both models under one Claude heading", groups[0])
	}
}

func TestGroupAdviceKeepsCodexAndOpenAIApart(t *testing.T) {
	// They are different upstreams, and the `openai` bucket also holds traffic recorded
	// before attribution was fixed. Merging them filed Gemini and Llama models under
	// "OpenAI Codex", which is a claim about where traffic went.
	groups := groupAdvice([]usage.CompactionAdvice{
		adviceOf("codex", "cx/gpt", usage.ReasonLearning, 1, 1, "x"),
		adviceOf("openai", "gemini-3.6-flash", usage.ReasonLearning, 1, 1, "x"),
	})
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want codex and openai kept apart", len(groups))
	}
	labels := map[string]bool{groups[0].Label: true, groups[1].Label: true}
	if !labels["OpenAI Codex"] || !labels["OpenAI"] {
		t.Errorf("labels = %v, want distinct headings", labels)
	}
}

func TestGroupNoteAccountsForUnmeasurableRequests(t *testing.T) {
	// The group total has to agree with the usage table, which counts every request
	// whether or not its prompt size was recorded.
	first := adviceOf("cursor", "a", usage.ReasonNoCacheData, 6, 0, "x")
	first.UnsizedRequests = 12
	second := adviceOf("cursor", "b", usage.ReasonNoCacheData, 4, 0, "x")
	second.UnsizedRequests = 9

	groups := groupAdvice([]usage.CompactionAdvice{first, second})
	if !strings.Contains(groups[0].Note, "10 requests seen") {
		t.Errorf("note = %q, want the measurable requests pooled", groups[0].Note)
	}
	if !strings.Contains(groups[0].Note, "+21 with no recorded prompt size") {
		t.Errorf("note = %q, want the unmeasurable ones accounted for", groups[0].Note)
	}
}

func TestAdviceTableGuardsApplyForModelsOutsideTheCatalog(t *testing.T) {
	// A recommendation is computed from usage, which includes models nobody registered.
	// Offering Apply there produces "model not found" on click, so the row has to say
	// what is missing instead.
	page := renderAdvicePanel(t, []usage.CompactionAdvice{
		{Provider: "antigravity", Model: "gemini-x", RecommendedWindow: 250_000,
			CurrentWindow: 0, EstimatedSaving: 0.2, SavingIsUpperBound: true},
	})
	// Match the form, not the word: the panel's footnote also begins with "Applying".
	if strings.Contains(page, `hx-post="/ui/models/advice/apply"`) {
		t.Error("a model with no catalog entry must not offer plain Apply: it would fail")
	}
	// A dead end is not an answer. The model has traffic, so the row offers to register
	// it and set the window in one step.
	if !strings.Contains(page, `hx-post="/ui/models/advice/register"`) {
		t.Error("the row must offer to add the model and apply the window")
	}
	if !strings.Contains(page, "not reported") {
		t.Error("cache reuse must read as unreported, not as zero")
	}
}

func TestAdviceTableOffersApplyForACatalogModel(t *testing.T) {
	page := renderAdvicePanel(t, []usage.CompactionAdvice{
		{Provider: "codex", Model: "cx/m", RecommendedWindow: 150_000,
			CurrentWindow: 400_000, EstimatedSaving: 0.33, BestHitRate: 0.3, CliffHitRate: 0.14},
	})
	if !strings.Contains(page, `hx-post="/ui/models/advice/apply"`) {
		t.Error("a catalog model with a recommendation must be applicable")
	}
	if strings.Contains(page, "not reported") {
		t.Error("a measured model must show its reuse figures")
	}
}

func TestRegisterKeepsTheModelIdTheClientActuallyUses(t *testing.T) {
	// Context windows resolve by the id the client asks for. The Add-model form prefixes
	// a typed name with its provider, which is right when a human types "gemini-3.6" but
	// wrong here: storing "ag/gemini-3.6-flash-high" would never match the traffic that
	// produced the recommendation, and the row would look applied while changing nothing.
	var gotProvider, gotID string
	var gotWindow int
	service := newAdviceService(t, ModelHooks{
		Add: func(_ context.Context, provider, id, label string, window int) (storage.CatalogModel, error) {
			gotProvider, gotID, gotWindow = provider, id, window
			return storage.CatalogModel{Provider: provider, ID: id}, nil
		},
	})

	form := url.Values{"provider": {"antigravity"}, "model": {"gemini-3.6-flash-high"}, "window": {"250000"}}
	request := httptest.NewRequest(http.MethodPost, "/ui/models/advice/register", strings.NewReader(form.Encode()))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	request.Header.Set("Origin", "http://example.test")
	request.Host = "example.test"
	recorder := httptest.NewRecorder()

	e := echo.New()
	if err := service.Register(e); err != nil {
		t.Fatalf("register routes: %v", err)
	}
	e.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if gotID != "gemini-3.6-flash-high" {
		t.Errorf("catalog id = %q, want the id verbatim from usage", gotID)
	}
	if gotProvider != "antigravity" || gotWindow != 250_000 {
		t.Errorf("provider/window = %q/%d, want antigravity/250000", gotProvider, gotWindow)
	}
}

func newAdviceService(t *testing.T, hooks ModelHooks) *Service {
	t.Helper()
	service, err := New(pool.New(nil), "token", nil, nil, nil, nil, nil,
		APIKeyHooks{}, hooks, SettingsHooks{}, UsageHooks{})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestGroupAdviceCollapsesTheCommonestVerdict(t *testing.T) {
	// "little history to compact" is the verdict most models land on. Left per-row it
	// repeated the same forty words nine times and buried the two rows that differed.
	groups := groupAdvice([]usage.CompactionAdvice{
		adviceOf("openai", "a", usage.ReasonCacheHolds, 180, 0, "no window would cut even 15% of its input, so there is little history to compact"),
		adviceOf("openai", "b", usage.ReasonCacheHolds, 261, 0, "no window would cut even 15% of its input, so there is little history to compact"),
	})
	if groups[0].Note == "" {
		t.Fatal("a group where every model reached the same verdict must state it once")
	}
	for _, row := range groups[0].Models {
		if row.Detail != "" {
			t.Errorf("row %s still repeats the shared sentence", row.Model)
		}
	}
	if groups[0].Models[0].Metric != "180 req" {
		t.Errorf("metric = %q, want the request count", groups[0].Models[0].Metric)
	}
}

func TestRowMetricNamesWhatEachVerdictTurnsOn(t *testing.T) {
	learning := adviceOf("codex", "m", usage.ReasonLearning, 500, 500, "learning")
	learning.LargeRequests = 39
	if got := rowMetric(learning); got != "39/120 large" {
		t.Errorf("learning metric = %q, want the evidence still missing", got)
	}
	applied := adviceOf("codex", "m", usage.ReasonApplied, 5, 5, "applied")
	applied.CurrentWindow = 150_000
	if got := rowMetric(applied); got != "at 150k" {
		t.Errorf("applied metric = %q, want where it now compacts", got)
	}
	unsized := adviceOf("cursor", "m", usage.ReasonNoCacheData, 10, 0, "x")
	unsized.UnsizedRequests = 16
	if got := rowMetric(unsized); got != "26 req" {
		t.Errorf("metric = %q, want every request counted, measurable or not", got)
	}
}
