package ui

import (
	"fmt"
	"sort"

	"literouter/internal/usage"
)

// AdviceGroup collects one provider's models that carry no recommendation.
//
// Flat, the list repeats the same sentence once per model — twenty lines of "upstream
// does not report cache usage" hide the three lines that actually differ. Grouping puts
// the shared reason on the provider and leaves only what varies on the model.
type AdviceGroup struct {
	Provider string
	Label    string
	Logo     string
	// Note is the shared explanation when every model in the group has the same
	// verdict; the per-model status is then redundant and is not repeated.
	Note   string
	Models []AdviceRow
	Count  int
}

// AdviceRow is one model inside a group.
type AdviceRow struct {
	Model string
	// Metric is the short figure that differs between models — the part worth reading
	// down a column. The prose stays in Title.
	Metric string
	// Detail is shown inline only when models in the group disagree; otherwise the
	// group's Note carries the explanation once.
	Detail string
	// Title is the full verdict, always available on hover.
	Title string
}

// reasonPhrase is the two-word verdict for a row in a group whose models disagree.
//
// The full sentence was being printed next to a metric that already said the same
// thing — "20/120 large" beside "learning: 20/120 large requests observed". The prose
// is kept on hover, where it costs nothing to read down the list.
func reasonPhrase(reason usage.Reason) string {
	switch reason {
	case usage.ReasonApplied:
		return "already applied"
	case usage.ReasonLearning:
		return "still learning"
	case usage.ReasonNoCacheData:
		return "no cache data"
	case usage.ReasonCacheHolds:
		return "little history"
	case usage.ReasonNoTraffic:
		return "no traffic"
	default:
		return ""
	}
}

// rowMetric is the one number that distinguishes a model from its neighbours. Repeating
// a forty-word sentence per row buried it; the sentence moves to the group note or a
// tooltip and this stays on the line.
func rowMetric(item usage.CompactionAdvice) string {
	total := item.Requests + item.UnsizedRequests
	switch item.Reason {
	case usage.ReasonLearning:
		return fmt.Sprintf("%d/%d large", item.LargeRequests, usage.MinLargeRequests)
	case usage.ReasonApplied:
		if item.CurrentWindow > 0 {
			return fmt.Sprintf("at %dk", item.CurrentWindow/1000)
		}
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%d req", total)
}

// groupAdvice builds the collapsed view of everything without a recommendation.
func groupAdvice(advice []usage.CompactionAdvice) []AdviceGroup {
	// Keyed on the label, not the raw id: `codex` and the legacy `openai` attribution
	// render as the same provider, and showing "OpenAI Codex" twice reads as a bug.
	byLabel := map[string][]usage.CompactionAdvice{}
	for _, item := range advice {
		if item.RecommendedWindow > 0 {
			continue
		}
		label := providerLabel(item.Provider)
		byLabel[label] = append(byLabel[label], item)
	}

	groups := make([]AdviceGroup, 0, len(byLabel))
	for label, items := range byLabel {
		group := AdviceGroup{
			Provider: items[0].Provider, Label: label,
			Logo: providerLogo(items[0].Provider), Count: len(items),
		}
		note, shared := sharedNote(items)
		group.Note = note
		for _, item := range items {
			row := AdviceRow{Model: item.Model, Metric: rowMetric(item), Title: item.Status}
			if !shared {
				row.Detail = reasonPhrase(item.Reason)
			}
			group.Models = append(group.Models, row)
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].Label < groups[j].Label
	})
	return groups
}

// sharedNote reports one explanation for the whole group when every model landed on the
// same verdict for the same reason.
func sharedNote(items []usage.CompactionAdvice) (string, bool) {
	if len(items) < 2 {
		return "", false
	}
	reason := items[0].Reason
	for _, item := range items {
		if item.Reason != reason {
			return "", false
		}
	}
	switch reason {
	case usage.ReasonNoCacheData:
		requests, reported, unsized := 0, 0, 0
		for _, item := range items {
			requests += item.Requests
			reported += item.ReportedRequests
			unsized += item.UnsizedRequests
		}
		suffix := ""
		if unsized > 0 {
			// Without this the group's total contradicts the usage table, which counts
			// every request whether or not its prompt size was recorded.
			suffix = fmt.Sprintf(" (+%d with no recorded prompt size)", unsized)
		}
		if reported == 0 {
			return fmt.Sprintf("%d requests seen, no cache figures from upstream%s — see each model for how much of its input is history",
				requests, suffix), true
		}
		return fmt.Sprintf("%d requests seen, only %d carrying a cache figure%s",
			requests, reported, suffix), true
	case usage.ReasonLearning:
		return fmt.Sprintf("still learning: none has reached %d large requests yet", usage.MinLargeRequests), true
	case usage.ReasonNoTraffic:
		return "no traffic yet", true
	case usage.ReasonCacheHolds:
		// The commonest verdict by far, and the one that filled the panel: every model
		// saying the same forty words about having little history to compact.
		return "conversations stay small — no window would cut enough input to be worth the context", true
	default:
		return "", false
	}
}
