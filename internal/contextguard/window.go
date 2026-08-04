package contextguard

import (
	"strings"
	"sync/atomic"
)

type windowSnapshot struct {
	configured map[string]int
	catalog    map[string]int
}

// WindowResolver serves immutable context-window snapshots without storage I/O.
type WindowResolver struct {
	snapshot atomic.Pointer[windowSnapshot]
}

func NewWindowResolver(configured, catalog map[string]int) *WindowResolver {
	resolver := &WindowResolver{}
	resolver.snapshot.Store(&windowSnapshot{configured: cloneWindows(configured), catalog: cloneWindows(catalog)})
	return resolver
}

func (resolver *WindowResolver) ReplaceCatalog(catalog map[string]int) {
	current := resolver.snapshot.Load()
	configured := map[string]int(nil)
	if current != nil {
		configured = current.configured
	}
	resolver.snapshot.Store(&windowSnapshot{configured: configured, catalog: cloneWindows(catalog)})
}

func (resolver *WindowResolver) Window(model string) int {
	snapshot := resolver.snapshot.Load()
	if snapshot == nil {
		return 0
	}
	if window := lookupWindow(snapshot.configured, model); window > 0 {
		return window
	}
	return lookupWindow(snapshot.catalog, model)
}

// LookupByModel resolves a per-model integer keyed by exact id or by the longest
// matching model prefix. It is exported so other per-model limits — output token
// caps, for one — key off the same matching rules the configuration documents for
// context.model_windows, rather than each growing its own near-miss variant.
func LookupByModel(values map[string]int, model string) int {
	return lookupWindow(values, model)
}

func lookupWindow(windows map[string]int, model string) int {
	for id, window := range windows {
		if strings.EqualFold(strings.TrimSpace(id), strings.TrimSpace(model)) && window > 0 {
			return window
		}
	}
	bestLength, bestWindow := 0, 0
	for prefix, window := range windows {
		if window > 0 && modelPrefixMatch(model, prefix) && len(prefix) > bestLength {
			bestLength, bestWindow = len(prefix), window
		}
	}
	return bestWindow
}

func cloneWindows(windows map[string]int) map[string]int {
	result := make(map[string]int, len(windows))
	for model, window := range windows {
		if window > 0 {
			result[model] = window
		}
	}
	return result
}
