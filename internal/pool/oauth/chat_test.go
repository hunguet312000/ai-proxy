package oauth

import (
	"strings"
	"testing"

	"literouter/internal/pool"
)

func TestPickAccountPrefersEnabledWeighted(t *testing.T) {
	p := pool.New([]pool.Account{
		{ID: "codex:a", Provider: "codex", Enabled: false, Weight: 10},
		{ID: "codex:b", Provider: "codex", Enabled: true, Weight: 1},
		{ID: "codex:c", Provider: "codex", Enabled: true, Weight: 5},
		{ID: "claude:x", Provider: "claude", Enabled: true, Weight: 9},
	})
	got, ok := pickAccount(p, "codex")
	if !ok || got.ID != "codex:c" {
		t.Fatalf("pickAccount = %#v, %v", got, ok)
	}
}

func TestResolveUpstreamModelReview(t *testing.T) {
	tests := []struct {
		provider, in, want string
	}{
		{"codex", "cx/gpt-5.6-sol-review", "gpt-5.6-sol"},
		{"codex", "gpt-5.6-sol-review", "gpt-5.6-sol"},
		{"codex", "cx/gpt-5.6-sol", "gpt-5.6-sol"},
		{"codex", "cx/gpt-5.3-codex-spark-review", "gpt-5.3-codex-spark"},
		{"grok", "xai/grok-4", "grok-4"},
		{"codex", "gpt-5.6-sol-review (high)", "gpt-5.6-sol (high)"},
	}
	for _, tc := range tests {
		if got := resolveUpstreamModel(tc.provider, tc.in); got != tc.want {
			t.Fatalf("resolveUpstreamModel(%q,%q) = %q, want %q", tc.provider, tc.in, got, tc.want)
		}
	}
	_ = strings.TrimSpace
}

func TestPrettyTrimErr(t *testing.T) {
	if got := trimErr([]byte("  hello   world  ")); got != "hello world" {
		t.Fatalf("trimErr = %q", got)
	}
}
