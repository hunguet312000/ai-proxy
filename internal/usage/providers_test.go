package usage

import "testing"

func TestParseClaudeQuota(t *testing.T) {
	quota, err := ParseClaudeQuota([]byte(`{
  "five_hour":{"utilization":25,"resets_at":"2026-08-01T00:00:00Z"},
  "seven_day":{"utilization":80,"resets_at":"2026-08-02T00:00:00Z"},
  "seven_day_opus":{"utilization":100,"resets_at":"2026-08-03T00:00:00Z"}
}`))
	if err != nil || len(quota.Windows) != 3 {
		t.Fatalf("ParseClaudeQuota() = %#v, %v", quota, err)
	}
	foundExhausted := false
	for _, window := range quota.Windows {
		if window.Key == "weekly_opus" && window.Exhausted {
			foundExhausted = true
		}
	}
	if !foundExhausted {
		t.Fatalf("windows = %#v", quota.Windows)
	}
}

func TestParseGrokQuota(t *testing.T) {
	quota, err := ParseGrokQuota(
		[]byte(`{"config":{"monthlyLimit":{"val":100},"used":{"val":40},"billingPeriodEnd":"2026-08-01T00:00:00Z"}}`),
		[]byte(`{"subscriptionTier":"supergrok"}`),
		[]byte(`{"config":{"onDemandCap":{"val":50},"onDemandUsed":{"val":50},"prepaidBalance":{"val":10},"billingPeriodEnd":"2026-08-01T00:00:00Z"}}`),
	)
	if err != nil || quota.Plan != "supergrok" || len(quota.Windows) != 3 {
		t.Fatalf("ParseGrokQuota() = %#v, %v", quota, err)
	}
	if quota.Windows[1].Key != "on_demand" || !quota.Windows[1].Exhausted {
		t.Fatalf("on-demand = %#v", quota.Windows[1])
	}
}

func TestParseGrokQuotaLiveShape(t *testing.T) {
	// Captured from cli-chat-proxy.grok.com for a Grok Pro / SuperGrok account.
	billing := []byte(`{"config":{"monthlyLimit":{"val":15000},"used":{"val":3770},"onDemandCap":{"val":0},"billingPeriodStart":"2026-07-01T00:00:00+00:00","billingPeriodEnd":"2026-08-01T00:00:00+00:00"}}`)
	user := []byte(`{"subscriptionTier":"GrokPro","hasGrokCodeAccess":true}`)
	credits := []byte(`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-07-16T18:33:15.465889+00:00","end":"2026-07-23T18:33:15.465889+00:00"},"creditUsagePercent":15.0,"productUsage":[{"product":"Api","usagePercent":14.0},{"product":"GrokChat","usagePercent":1.0}],"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"prepaidBalance":{"val":0},"billingPeriodEnd":"2026-07-23T18:33:15.465889+00:00"}}`)
	quota, err := ParseGrokQuota(billing, user, credits)
	if err != nil {
		t.Fatal(err)
	}
	if quota.Plan != "grokpro" {
		t.Fatalf("plan = %q", quota.Plan)
	}
	if len(quota.Windows) != 2 {
		t.Fatalf("windows = %#v", quota.Windows)
	}
	if quota.Windows[0].Key != "weekly" || quota.Windows[0].Used != 15 || quota.Windows[0].RemainingPercent != 85 || quota.Windows[0].ResetAt.IsZero() {
		t.Fatalf("weekly = %#v", quota.Windows[0])
	}
	w := quota.Windows[1]
	if w.Key != "monthly" || w.Used != 3770 || w.Total != 15000 || w.Remaining != 11230 {
		t.Fatalf("monthly window = %#v", w)
	}
}
