package ui

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"literouter/internal/clisetup"
	"literouter/internal/pool"
	"literouter/internal/storage"
	"literouter/internal/usage"
)

//go:embed assets/*
var assets embed.FS

var oauthModalTemplate = template.Must(template.New("oauth-modal").Parse(`
<div class="oauth-modal" data-oauth-modal data-provider="{{.Provider}}" data-auth-url="{{.AuthURL}}">
  <div class="oauth-modal-card">
    <header class="oauth-modal-head">
      <div class="oauth-traffic" aria-hidden="true"><i></i><i></i><i></i></div>
      <strong>{{.Title}}</strong>
      <button type="button" class="icon-btn" data-oauth-close aria-label="Close">✕</button>
    </header>
    <div class="oauth-waiting">
      <span class="oauth-spinner" aria-hidden="true"></span>
      <span>{{.Waiting}}</span>
    </div>
    <div class="oauth-divider"><span>OR PASTE CALLBACK URL MANUALLY</span></div>
    <div class="oauth-step">
      <div class="oauth-step-label">{{.Step1}}</div>
      <div class="oauth-url-row">
        <code class="oauth-url" id="oauth-auth-url">{{.AuthURL}}</code>
        <button type="button" class="btn secondary sm" data-copy="#oauth-auth-url">Copy</button>
      </div>
      {{if .UserCode}}<p class="oauth-usercode">Device code: <strong>{{.UserCode}}</strong></p>{{end}}
      <a class="btn primary block" href="{{.AuthURL}}" target="literouter_oauth" rel="noreferrer" data-oauth-popup>Open browser</a>
    </div>
    <div class="oauth-step">
      <div class="oauth-step-label">{{.Step2}}</div>
      <p class="oauth-hint">{{.Step2Hint}}</p>
      <form class="oauth-complete-form" hx-post="/ui/oauth/complete" hx-target="#oauth-complete-result" hx-swap="innerHTML" hx-disabled-elt="find button[type=submit]" data-oauth-complete-form>
        <input type="hidden" name="provider" value="{{.Provider}}">
        <input class="field" name="callback" placeholder="{{.Placeholder}}" autocomplete="off" spellcheck="false" required data-oauth-callback-input>
        <div id="oauth-complete-result" class="oauth-complete-result" aria-live="polite"></div>
        <div class="oauth-actions">
          <button class="btn primary" type="submit">Connect</button>
          <button class="btn ghost" type="button" data-oauth-close>Cancel</button>
        </div>
      </form>
    </div>
  </div>
</div>
`))

type OAuthResult struct {
	AuthURL                 string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
}

type APIKeyHooks struct {
	List       func(context.Context) ([]storage.APIKey, error)
	Create     func(context.Context, string) (storage.APIKey, error)
	SetEnabled func(context.Context, string, bool) error
	Delete     func(context.Context, string) error
	Valid      func(string) bool
}

type ModelHooks struct {
	List             func(context.Context, string) ([]storage.CatalogModel, error)
	Add              func(context.Context, string, string, string, int) (storage.CatalogModel, error)
	SetContextWindow func(context.Context, string, string, int) error
	SetEffort        func(context.Context, string, string, string) error
	Delete           func(context.Context, string, string) error
	Test             func(context.Context, string) (ModelTestResult, error)
}

type ModelTestResult struct {
	OK       bool   `json:"ok"`
	Model    string `json:"model"`
	Latency  string `json:"latency,omitempty"`
	Preview  string `json:"preview,omitempty"`
	Error    string `json:"error,omitempty"`
	Provider string `json:"provider,omitempty"`
}

type SettingsHooks struct {
	GetStrategy func(context.Context) (string, error)
	SetStrategy func(context.Context, string) error
	// GetPlanModel and SetPlanModel expose the router's plan-mode override. Unlike the
	// model fields on the CLI card, which write the host client's own settings.json,
	// this is LiteRouter's own routing decision and applies to a running gateway.
	GetPlanModel func(context.Context) (string, error)
	SetPlanModel func(context.Context, string) error
	// GetLongContext and SetLongContext expose the router's long-context rule: the model
	// a turn is handed to, and the share of the serving model's window that triggers it.
	// Also LiteRouter's own routing, applied to a running gateway.
	GetLongContext func(context.Context) (string, int, error)
	SetLongContext func(context.Context, string, int) error
	// GetImageRoute and SetImageRoute expose the vision fallback and the models declared
	// unable to read images. Also LiteRouter's own routing, applied to a running gateway.
	GetImageRoute func(context.Context) (string, string, error)
	SetImageRoute func(context.Context, string, string) error
	// GetCLIDraft and SetCLIDraft remember the CLI model selection independently of the
	// host client's config, so Reset can return that client to stock without discarding
	// what to re-apply.
	GetCLIDraft func(context.Context) (clisetup.Draft, error)
	SetCLIDraft func(context.Context, clisetup.Draft) error
	// ContextCeiling resolves what the CLI card's "auto" max-context setting stands for:
	// the window the client can safely be told it has, given the models being configured
	// and the long-context rule. A hook rather than a catalog read because the gateway
	// folds in windows learned from upstream rejections since boot.
	ContextCeiling func(context.Context, []string) int
}

type UsageHooks struct {
	Summary func(context.Context, time.Time, int) (storage.UsageSummary, error)
	// Compaction reports where each model's prompt cache stops paying for itself, so
	// the operator can see the recommendation and the evidence behind it rather than
	// having it applied silently.
	Compaction func(context.Context) ([]usage.CompactionAdvice, error)
}

type ProviderInfo struct {
	ID          string
	Name        string
	Description string
	Icon        string
	OAuthValue  string
	Total       int
	Enabled     int
	Exhausted   int
}

type Service struct {
	pool            *pool.Pool
	startOAuth      func(context.Context, string) (OAuthResult, error)
	completeOAuth   func(context.Context, string, string) error
	refreshQuota    func(context.Context, string) error
	resetCodex      func(context.Context, string) error
	listSnapshots   func(context.Context, string) ([]storage.QuotaSnapshot, error)
	updateAccount   func(context.Context, string, bool, int) error
	deleteAccount   func(context.Context, string) error
	apiKeys         APIKeyHooks
	modelsHook      ModelHooks
	customProviders CustomProviderHooks
	importCursor    func(context.Context, string, string) error
	detectCursor    func(context.Context) (string, error)
	settings        SettingsHooks
	usage           UsageHooks
	apiToken        string
	models          []string
	index           *template.Template
	accounts        *template.Template
	account         *template.Template
	tab             *template.Template
	refreshMu       sync.Mutex
	refreshing      map[string]struct{}
}

type QuotaWindowView struct {
	Key              string
	Label            string
	Used             float64
	Total            float64
	Remaining        float64
	RemainingPercent float64
	ResetAt          time.Time
	FetchedAt        time.Time
	Stale            bool
	Exhausted        bool
	Unlimited        bool
}

type AccountView struct {
	pool.Account
	Windows []QuotaWindowView
}

type QuotaProviderGroup struct {
	ID        string
	Name      string
	Accounts  []AccountView
	Total     int
	Enabled   int
	Disabled  int
	Exhausted int
}

type ModelGroup struct {
	Provider ProviderInfo
	Models   []storage.CatalogModel
}

type MapProviderNode struct {
	ID       string
	Name     string
	Icon     string
	Requests int
	Tokens   int64
	Calling  bool
	Used     bool
}

type UsageLive struct {
	// Calling now (newest request within live window).
	Codex   bool
	Claude  bool
	XAI     bool
	Any     bool
	Last    string
	Seconds int // seconds since last request; -1 if none
	// Used in selected range (history), even if not currently calling.
	UsedCodex  bool
	UsedClaude bool
	UsedXAI    bool
	UsedAny    bool
}

type viewData struct {
	Accounts             []AccountView
	QuotaGroups          []QuotaProviderGroup
	APIKeys              []storage.APIKey
	CreatedKey           *storage.APIKey
	CatalogModels        []storage.CatalogModel
	ModelGroups          []ModelGroup
	Providers            []ProviderInfo
	Provider             *ProviderInfo
	CustomProviders      []storage.CustomProvider
	CustomProvider       *storage.CustomProvider
	CustomProviderModels map[string][]storage.CatalogModel
	CustomError          string
	Advice               []usage.CompactionAdvice
	AdviceGroups         []AdviceGroup
	AdviceNote           string
	Total                int
	Enabled              int
	Exhausted            int
	Models               []string
	BaseURL              string
	ClaudeSetup          clisetup.Request
	ClaudeApplied        bool
	PlanModel            string
	LongContextModel     string
	LongContextPercent   int
	ImageModel           string
	TextOnlyModels       string
	Tab                  string
	TabTitle             string
	TabHeading           string
	View                 string
	Strategy             string
	FilterProvider       string
	FilterStatus         string
	Sort                 string
	AutoRefresh          bool
	Usage                storage.UsageSummary
	UsageRange           string
	UsageSince           time.Time
	UsageLive            UsageLive
	UsageMap             []MapProviderNode
}

func newViewData(accounts []AccountView) viewData {
	data := viewData{Accounts: accounts, Total: len(accounts), Tab: "endpoint"}
	for _, account := range accounts {
		if account.Enabled {
			data.Enabled++
		}
		if account.QuotaExhausted {
			data.Exhausted++
		}
	}
	data.QuotaGroups = groupQuotaAccounts(accounts)
	return data
}

func groupQuotaAccounts(accounts []AccountView) []QuotaProviderGroup {
	groups := make([]QuotaProviderGroup, 0)
	indexes := make(map[string]int)
	for _, account := range accounts {
		id := normalizeProviderID(account.Provider)
		index, ok := indexes[id]
		if !ok {
			info := providerByID(id)
			index = len(groups)
			indexes[id] = index
			groups = append(groups, QuotaProviderGroup{ID: id, Name: info.Name})
		}
		group := &groups[index]
		group.Accounts = append(group.Accounts, account)
		group.Total++
		if account.Enabled {
			group.Enabled++
		} else {
			group.Disabled++
		}
		if account.QuotaExhausted {
			group.Exhausted++
		}
	}
	return groups
}

func withTab(data viewData, tab string) viewData {
	data.Tab = tab
	switch {
	case strings.HasPrefix(tab, "provider:"):
		id := strings.TrimPrefix(tab, "provider:")
		info := providerByID(id)
		data.Tab = "provider-detail"
		data.TabTitle = "Providers"
		data.TabHeading = info.Name
		data.Provider = &info
	case tab == "providers":
		data.TabTitle, data.TabHeading = "Providers", "OAuth providers & pool"
	case tab == "quota":
		data.TabTitle, data.TabHeading = "Quota Tracker", "Session & weekly limits"
	case tab == "usage":
		data.TabTitle, data.TabHeading = "Usage & Analytics", "Traffic, cost & routing"
	case tab == "cli":
		data.TabTitle, data.TabHeading = "CLI Tools", "Apply host client configs"
	default:
		data.Tab, data.TabTitle, data.TabHeading = "endpoint", "Endpoint & Key", "Local gateway endpoints"
	}
	return data
}

func knownProviders() []ProviderInfo {
	return []ProviderInfo{
		{ID: "codex", Name: "Codex", Description: "OpenAI ChatGPT OAuth", Icon: "codex", OAuthValue: "codex"},
		{ID: "claude", Name: "Claude", Description: "Anthropic OAuth", Icon: "claude", OAuthValue: "claude"},
		{ID: "xai", Name: "xAI (Grok)", Description: "Browser OAuth · auth.x.ai", Icon: "xai", OAuthValue: "xai"},
		{ID: "antigravity", Name: "Antigravity", Description: "Google OAuth · Cloud Code", Icon: "antigravity", OAuthValue: "antigravity"},
		// Cursor has no authorization endpoint: the session is imported from the IDE,
		// so OAuthValue stays empty and the detail page offers an import form instead.
		{ID: "cursor", Name: "Cursor", Description: "Imported IDE session · agent.api5.cursor.sh", Icon: "cursor"},
	}
}

func providerByID(id string) ProviderInfo {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "grok" {
		id = "xai"
	}
	for _, provider := range knownProviders() {
		if provider.ID == id {
			return provider
		}
	}
	// The fallback used to hand every unknown provider OpenAI's mark, which put that
	// logo on custom upstreams that have nothing to do with OpenAI.
	return ProviderInfo{ID: id, Name: providerLabel(id), Description: "Provider", Icon: providerLogo(id), OAuthValue: id}
}

func providerMatches(accountProvider, providerID string) bool {
	accountProvider = strings.ToLower(accountProvider)
	providerID = strings.ToLower(providerID)
	if providerID == "xai" {
		return accountProvider == "xai" || accountProvider == "grok"
	}
	return accountProvider == providerID
}

func quotaBucket(remaining float64) int {
	remaining = min(max(remaining, 0), 100)
	return int(remaining+9) / 10 * 10
}

func formatPlan(plan string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return ""
	}
	lower := strings.ToLower(plan)
	switch lower {
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	case "free":
		return "Free"
	case "team":
		return "Team"
	case "enterprise":
		return "Enterprise"
	case "claude code":
		return "Claude Code"
	case "xai":
		return "xAI"
	case "supergrok", "super_grok", "super-grok":
		return "SuperGrok"
	case "supergrok heavy", "super_grok_heavy":
		return "SuperGrok Heavy"
	case "grokpro", "grok_pro", "grok-pro", "grok pro":
		// Consumer app brands this tier as SuperGrok.
		return "SuperGrok"
	case "grok":
		return "Grok"
	default:
		// Title-case words for values like "super_grok"
		parts := strings.FieldsFunc(plan, func(r rune) bool { return r == '_' || r == '-' || r == ' ' })
		for i, part := range parts {
			if part == "" {
				continue
			}
			low := strings.ToLower(part)
			if low == "xai" {
				parts[i] = "xAI"
				continue
			}
			if low == "gpt" {
				parts[i] = "GPT"
				continue
			}
			parts[i] = strings.ToUpper(low[:1]) + low[1:]
		}
		return strings.Join(parts, " ")
	}
}

func planClass(plan string) string {
	p := strings.ToLower(strings.TrimSpace(plan))
	switch {
	case p == "plus", strings.Contains(p, "plus"):
		return "plus"
	case strings.Contains(p, "supergrok"), strings.Contains(p, "super_grok"), strings.Contains(p, "super-grok"), p == "grokpro", strings.Contains(p, "grok_pro"), strings.Contains(p, "grok-pro"), strings.Contains(p, "grok pro"):
		return "pro"
	case p == "pro", strings.Contains(p, "pro"):
		return "pro"
	case p == "team", strings.Contains(p, "team"):
		return "team"
	case p == "enterprise", strings.Contains(p, "enterprise"):
		return "enterprise"
	case p == "free", strings.Contains(p, "free"):
		return "free"
	case strings.Contains(p, "claude"):
		return "claude"
	case p == "xai", strings.Contains(p, "grok"):
		return "xai"
	default:
		return "default"
	}
}

func formatReset(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	remaining := time.Until(value)
	if remaining <= 0 {
		return "reset due"
	}
	days := int(remaining.Hours()) / 24
	hours := int(remaining.Hours()) % 24
	minutes := int(remaining.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("in %dd %dh %dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("in %dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("in %dm", max(minutes, 1))
	}
}

func formatFetched(value time.Time) string {
	if value.IsZero() {
		return "Not refreshed yet"
	}
	elapsed := time.Since(value)
	switch {
	case elapsed < time.Minute:
		return "Updated just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("Updated %dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("Updated %dh ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("Updated %dd ago", int(elapsed.Hours())/24)
	}
}

func windowLabel(key string) string {
	switch key {
	case "session":
		return "Session"
	case "weekly":
		return "Weekly"
	case "weekly_api":
		return "Weekly API"
	case "weekly_chat":
		return "Weekly Chat"
	case "monthly":
		return "Monthly"
	case "on_demand":
		return "On-demand"
	case "credits":
		return "Credits"
	case "prepaid":
		return "Prepaid"
	case "subscription":
		return "Subscription"
	case "review_session":
		return "Review session"
	case "review_weekly":
		return "Review weekly"
	default:
		return strings.ReplaceAll(key, "_", " ")
	}
}

func pct(part, total int) int {
	if total <= 0 || part <= 0 {
		return 0
	}
	v := (part*100 + total/2) / total
	if v > 100 {
		return 100
	}
	return v
}

func pct64(part, total int64) int {
	if total <= 0 || part <= 0 {
		return 0
	}
	v := int((part*100 + total/2) / total)
	if v > 100 {
		return 100
	}
	return v
}

// providerHonoursEffort reports whether a reasoning-effort override reaches the wire.
//
// Only the Codex path sends it — as `reasoning.effort` on the Responses payload. Every
// other upstream drops it: Cursor's agent request has no such field, Antigravity's
// envelope has none, and for OpenAI-compatible upstreams the field is `json:"-"` and is
// never serialised. Offering the control everywhere let the dashboard claim an override
// was in force on a model that never saw it.
//
// If another provider learns to carry effort, this is the list to extend — it is the
// only thing standing between the setting and a promise the proxy cannot keep.
func providerHonoursEffort(provider string) bool {
	switch normalizeProviderID(provider) {
	case "codex", "cx", "openai":
		return true
	default:
		return false
	}
}

// providerLogo picks the asset for a provider id.
//
// One helper rather than a template chain per surface: there were three, and they had
// already drifted — the usage breakdown knew nothing about Antigravity or Cursor and
// handed both OpenAI's mark, which is not a cosmetic error when the whole point of the
// panel is telling providers apart.
func providerLogo(p string) string {
	id := strings.ToLower(strings.TrimSpace(p))
	if strings.HasPrefix(id, customProviderUsagePrefix) {
		// A user-registered upstream has no logo of its own, and borrowing a vendor's
		// would misattribute it.
		return "literouter"
	}
	switch id {
	case "codex", "cx":
		return "codex"
	case "openai":
		return "openai"
	case "claude", "anthropic":
		return "claude"
	case "xai", "grok":
		return "xai"
	case "antigravity", "gemini":
		return "antigravity"
	case "cursor", "cu":
		return "cursor"
	default:
		return "literouter"
	}
}

const customProviderUsagePrefix = "custom:"

func providerLabel(p string) string {
	id := strings.ToLower(strings.TrimSpace(p))
	if name, ok := strings.CutPrefix(id, customProviderUsagePrefix); ok && name != "" {
		return name + " (custom)"
	}
	switch id {
	case "codex":
		return "OpenAI Codex"
	case "openai":
		// Not folded into Codex. They are different upstreams, and this bucket also
		// holds traffic recorded before provider attribution was fixed — calling it
		// "OpenAI Codex" put Gemini and Llama models under the Codex heading.
		return "OpenAI"
	case "claude", "anthropic":
		return "Claude"
	case "xai", "grok":
		return "xAI (Grok)"
	case "antigravity", "gemini":
		return "Google Antigravity"
	case "cursor", "cu":
		return "Cursor"
	case "", "unknown":
		return "Unknown"
	default:
		return p
	}
}

func endpointLabel(ep string) string {
	ep = strings.TrimSpace(ep)
	switch {
	case ep == "/ui/models/test":
		return "Model test"
	case strings.Contains(ep, "/messages"):
		return "Messages"
	case strings.Contains(ep, "/chat/completions"):
		return "Chat"
	case strings.Contains(ep, "/responses"):
		return "Responses"
	case ep == "":
		return "—"
	default:
		return ep
	}
}

func formatClock(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(usageLocation).Format("15:04:05")
}

func formatDayTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	// Always render usage timestamps in UTC+7 so Docker/UTC hosts match the
	// analytics calendar range (Asia/Ho_Chi_Minh), not container local time.
	now := time.Now().In(usageLocation)
	local := t.In(usageLocation)
	if now.Year() == local.Year() && now.YearDay() == local.YearDay() {
		return local.Format("15:04:05")
	}
	return local.Format("01-02 15:04")
}

func formatUSD(v float64) string {
	if v <= 0 {
		return "$0.00"
	}
	if v < 0.01 {
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func formatInt(v int64) string {
	n := v
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	pad := len(s) % 3
	if pad == 0 {
		pad = 3
	}
	b.WriteString(s[:pad])
	for i := pad; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	out := b.String()
	if neg {
		return "-" + out
	}
	return out
}

func formatUsageInt(v int) string {
	return formatInt(int64(v))
}

func formatTokens(v int64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(v)/1_000_000)
	case v >= 10_000:
		return fmt.Sprintf("%.1fK", float64(v)/1_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fK", float64(v)/1_000)
	default:
		return formatInt(v)
	}
}

var usageLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		return time.FixedZone("UTC+7", 7*60*60)
	}
	return location
}()

func usageSince(rangeKey string) (time.Time, string) {
	return usageSinceAt(time.Now(), rangeKey)
}

func usageSinceAt(now time.Time, rangeKey string) (time.Time, string) {
	local := now.In(usageLocation)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, usageLocation)
	switch strings.ToLower(strings.TrimSpace(rangeKey)) {
	case "7d", "week":
		return start.AddDate(0, 0, -6), "7d"
	case "30d", "month":
		return start.AddDate(0, 0, -29), "30d"
	default:
		return start, "24h"
	}
}

// usageLiveWindow: only the single newest request stays "calling now".
// Older providers go idle immediately so the map never lights multiple paths at once.
const usageLiveWindow = 90 * time.Second

func buildUsageMap(providers []ProviderInfo, summary storage.UsageSummary, live UsageLive) []MapProviderNode {
	// Only providers with at least one configured account appear on the topology.
	out := make([]MapProviderNode, 0, len(providers))
	reqBy := map[string]int{}
	tokBy := map[string]int64{}
	for _, st := range summary.ByProvider {
		id := normalizeLiveProvider(st.Provider, "")
		if id == "" {
			continue
		}
		reqBy[id] += st.Requests
		tokBy[id] += st.Tokens
	}
	for _, p := range providers {
		if p.Total <= 0 {
			continue
		}
		id := p.ID
		if id == "grok" {
			id = "xai"
		}
		node := MapProviderNode{
			ID:       id,
			Name:     p.Name,
			Icon:     p.Icon,
			Requests: reqBy[id],
			Tokens:   tokBy[id],
		}
		// Pretty display names for map
		switch id {
		case "codex":
			node.Name = "OpenAI Codex"
			node.Calling = live.Codex
			node.Used = live.UsedCodex
			if node.Icon == "" {
				node.Icon = "codex"
			}
		case "claude":
			node.Name = "Claude"
			node.Calling = live.Claude
			node.Used = live.UsedClaude
			if node.Icon == "" {
				node.Icon = "claude"
			}
		case "xai":
			node.Name = "xAI (Grok)"
			node.Calling = live.XAI
			node.Used = live.UsedXAI
			if node.Icon == "" {
				node.Icon = "xai"
			}
		default:
			node.Calling = live.Last == id
			node.Used = reqBy[id] > 0
		}
		out = append(out, node)
	}
	return out
}

func computeUsageLive(summary storage.UsageSummary) UsageLive {
	var live UsageLive
	live.Seconds = -1

	// Historical usage in the selected analytics window.
	for _, st := range summary.ByProvider {
		if st.Requests <= 0 {
			continue
		}
		switch normalizeLiveProvider(st.Provider, "") {
		case "codex":
			live.UsedCodex = true
		case "claude":
			live.UsedClaude = true
		case "xai":
			live.UsedXAI = true
		}
	}
	// Fallback: derive used from recent events if ByProvider empty.
	if !live.UsedCodex && !live.UsedClaude && !live.UsedXAI {
		for _, ev := range summary.Recent {
			switch normalizeLiveProvider(ev.Provider, ev.Model) {
			case "codex":
				live.UsedCodex = true
			case "claude":
				live.UsedClaude = true
			case "xai":
				live.UsedXAI = true
			}
		}
	}
	live.UsedAny = live.UsedCodex || live.UsedClaude || live.UsedXAI

	if len(summary.Recent) == 0 {
		return live
	}
	// Pick absolute newest event (defensive if order ever changes).
	newest := summary.Recent[0]
	for _, ev := range summary.Recent[1:] {
		if ev.Timestamp.After(newest.Timestamp) {
			newest = ev
		}
	}
	if newest.Timestamp.IsZero() {
		return live
	}
	age := time.Since(newest.Timestamp)
	if age < 0 {
		age = 0
	}
	live.Seconds = int(age.Seconds())
	live.Last = normalizeLiveProvider(newest.Provider, newest.Model)
	if age > usageLiveWindow || live.Last == "" {
		return live
	}
	// Only the latest provider path is hot.
	switch live.Last {
	case "codex":
		live.Codex = true
	case "claude":
		live.Claude = true
	case "xai":
		live.XAI = true
	default:
		return live
	}
	live.Any = true
	return live
}

func normalizeLiveProvider(provider, model string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	// Usage from a custom provider is recorded as "custom:<prefix>"; the routing map
	// and the provider grid key on the bare prefix.
	if rest, ok := strings.CutPrefix(p, "custom:"); ok {
		return rest
	}
	switch p {
	case "codex", "openai", "cx":
		return "codex"
	case "antigravity", "gemini":
		return "antigravity"
	case "claude", "anthropic":
		return "claude"
	case "xai", "grok":
		return "xai"
	}
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "ag/"), strings.HasPrefix(m, "antigravity/"), strings.HasPrefix(m, "gemini-"), strings.HasPrefix(m, "gemini/"):
		return "antigravity"
	case strings.HasPrefix(m, "claude"), strings.HasPrefix(m, "anthropic"):
		return "claude"
	case strings.HasPrefix(m, "grok"), strings.HasPrefix(m, "xai/"):
		return "xai"
	case strings.HasPrefix(m, "cx/"), strings.Contains(m, "codex"), strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "codex"
	default:
		return p
	}
}

func (s *Service) loadUsage(ctx context.Context, since time.Time) storage.UsageSummary {
	if s == nil || s.usage.Summary == nil {
		return storage.UsageSummary{}
	}
	summary, err := s.usage.Summary(ctx, since, 40)
	if err != nil {
		return storage.UsageSummary{}
	}
	return summary
}

// SetCustomProviderHooks wires the custom-provider storage operations. It is a
// setter rather than another positional argument to New, whose parameter list is
// already long enough to make call sites easy to get wrong.
func (s *Service) SetCustomProviderHooks(hooks CustomProviderHooks) {
	s.customProviders = hooks
}

// SetImportCursor wires the Cursor session import. Cursor has no authorization
// endpoint, so this replaces the OAuth start hook the other providers use.
func (s *Service) SetImportCursor(fn func(context.Context, string, string) error) {
	s.importCursor = fn
}

// SetDetectCursor wires local session discovery. It returns the path it read so the
// result names the install it found, which is the only way to tell one Cursor
// profile from another.
func (s *Service) SetDetectCursor(fn func(context.Context) (string, error)) {
	s.detectCursor = fn
}

func (s *Service) detectCursorHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.detectCursor == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "cursor detection unavailable")
	}
	path, err := s.detectCursor(c.Request().Context())
	if err != nil {
		return s.flash(c, http.StatusBadRequest, err.Error())
	}
	return s.flash(c, http.StatusOK, "Imported the session from "+path)
}

func (s *Service) importCursorHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.importCursor == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "cursor import unavailable")
	}
	accessToken := strings.TrimSpace(c.FormValue("access_token"))
	machineID := strings.TrimSpace(c.FormValue("machine_id"))
	if accessToken == "" || machineID == "" {
		return s.flash(c, http.StatusBadRequest, "Both the access token and the machine id are required.")
	}
	if err := s.importCursor(c.Request().Context(), accessToken, machineID); err != nil {
		return s.flash(c, http.StatusBadRequest, err.Error())
	}
	return s.flash(c, http.StatusOK, "Cursor session imported. It expires with the token; re-import when it does.")
}

// flash renders a one-line result into the slot the import form targets.
func (s *Service) flash(c echo.Context, status int, message string) error {
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	c.Response().WriteHeader(status)
	class := "flash-ok"
	if status >= 400 {
		class = "flash-error"
	}
	_, err := c.Response().Write([]byte(`<p class="` + class + `">` + template.HTMLEscapeString(message) + `</p>`))
	return err
}

func New(
	accountPool *pool.Pool,
	apiToken string,
	startOAuth func(context.Context, string) (OAuthResult, error),
	refreshQuota func(context.Context, string) error,
	listSnapshots func(context.Context, string) ([]storage.QuotaSnapshot, error),
	updateAccount func(context.Context, string, bool, int) error,
	deleteAccount func(context.Context, string) error,
	apiKeys APIKeyHooks,
	modelHooks ModelHooks,
	settings SettingsHooks,
	usageHooks UsageHooks,
	models ...string,
) (*Service, error) {
	funcs := template.FuncMap{
		"urlquery": url.QueryEscape, "quotaBucket": quotaBucket, "modelIDExample": modelIDExample,
		"formatReset": formatReset, "formatFetched": formatFetched, "formatPlan": formatPlan, "planClass": planClass,
		"formatUSD": formatUSD, "formatInt": formatInt, "formatUsageInt": formatUsageInt, "formatTokens": formatTokens, "prettyModel": storage.PrettyModelLabel, "formatContextWindow": storage.FormatContextWindow, "pct": pct, "pct64": pct64, "div": divInt, "pctf": pctFraction, "effortLevels": func() []string { return storage.EffortLevels }, "honoursEffort": providerHonoursEffort, "providerLabel": providerLabel, "providerLogo": providerLogo, "endpointLabel": endpointLabel, "formatClock": formatClock, "formatDayTime": formatDayTime,
	}
	index, err := template.New("index.html").Funcs(funcs).ParseFS(assets, "assets/index.html", "assets/tabs.html", "assets/accounts.html")
	if err != nil {
		return nil, err
	}
	accounts, err := template.New("accounts.html").Funcs(funcs).ParseFS(assets, "assets/accounts.html")
	if err != nil {
		return nil, err
	}
	tab, err := template.New("tab").Funcs(funcs).ParseFS(assets, "assets/tabs.html", "assets/accounts.html")
	if err != nil {
		return nil, err
	}
	account, err := template.New("account").Funcs(funcs).Parse(`{{template "account" .}}`)
	if err != nil {
		return nil, err
	}
	if _, err := account.ParseFS(assets, "assets/accounts.html"); err != nil {
		return nil, err
	}
	return &Service{
		pool: accountPool, apiToken: apiToken, models: append([]string(nil), models...), startOAuth: startOAuth, refreshQuota: refreshQuota,
		listSnapshots: listSnapshots, updateAccount: updateAccount, deleteAccount: deleteAccount,
		apiKeys: apiKeys, modelsHook: modelHooks, settings: settings, usage: usageHooks,
		index: index, accounts: accounts, account: account, tab: tab, refreshing: map[string]struct{}{},
	}, nil
}

func (s *Service) SetResetCodex(fn func(context.Context, string) error) {
	s.resetCodex = fn
}

func (s *Service) SetCompleteOAuth(fn func(context.Context, string, string) error) {
	s.completeOAuth = fn
}

func (s *Service) Register(e *echo.Echo) error {
	static, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	e.GET("/ui", s.pageHandler("endpoint"))
	e.GET("/ui/", s.pageHandler("endpoint"))
	e.GET("/ui/endpoint", s.pageHandler("endpoint"))
	e.GET("/ui/providers", s.pageHandler("providers"))
	e.GET("/ui/providers/:id", s.providerDetailHandler)
	e.GET("/ui/quota", s.pageHandler("quota"))
	e.GET("/ui/cli", s.pageHandler("cli"))
	e.GET("/ui/usage", s.pageHandler("usage"))
	e.GET("/ui/tabs/:tab", s.tabHandler)
	e.GET("/ui/tabs/providers/:id", s.providerTabHandler)
	e.GET("/ui/accounts", s.accountsHandler)
	e.POST("/ui/session", s.sessionHandler)
	e.POST("/ui/oauth/start", s.oauthHandler)
	e.POST("/ui/oauth/complete", s.oauthCompleteHandler)
	e.POST("/ui/accounts/:id/quota", s.quotaHandler)
	e.POST("/ui/accounts/:id/reset-credits", s.resetCreditsHandler)
	e.POST("/ui/accounts/:id/priority", s.priorityHandler)
	e.POST("/ui/accounts/bulk", s.bulkAccountsHandler)
	e.POST("/ui/accounts/:id", s.updateHandler)
	e.DELETE("/ui/accounts/:id", s.deleteHandler)
	e.POST("/ui/strategy", s.strategyHandler)
	e.POST("/ui/routing", s.routingHandler)
	e.GET("/ui/keys", s.keysHandler)
	e.POST("/ui/keys", s.createKeyHandler)
	e.POST("/ui/keys/:id/toggle", s.toggleKeyHandler)
	e.DELETE("/ui/keys/:id", s.deleteKeyHandler)
	e.GET("/ui/models", s.modelsHandler)
	e.POST("/ui/models", s.addModelHandler)
	e.POST("/ui/models/:id/context", s.updateModelContextHandler)
	e.POST("/ui/models/:id/effort", s.updateModelEffortHandler)
	e.GET("/ui/models/advice", s.compactionAdviceHandler)
	e.POST("/ui/models/advice/apply", s.applyCompactionAdviceHandler)
	e.POST("/ui/models/advice/register", s.registerCompactionAdviceHandler)
	e.POST("/ui/models/test", s.testModelHandler)
	e.DELETE("/ui/models/:id", s.deleteModelHandler)
	e.POST("/ui/setup/:tool/:action", s.cliSetupHandler)
	s.registerCustomProviderRoutes(e)
	e.POST("/ui/oauth/cursor/import", s.importCursorHandler)
	e.POST("/ui/oauth/cursor/detect", s.detectCursorHandler)
	// Every dashboard response is a live reading and none of it may be cached.
	//
	// The mutation handlers each said so individually while the pages and fragments said
	// nothing at all — no Cache-Control, no ETag, no Last-Modified — which leaves a browser
	// free to apply heuristic freshness and serve a reload from its cache. That is what made
	// a disabled account come back on after F5: the toggle had persisted, the server was
	// right, and the browser redrew the page it already had.
	//
	// Assets are exempt and keep their own header: they are versioned in the URL and are the
	// one part of this that should be cached.
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			path := c.Request().URL.Path
			if !strings.HasPrefix(path, "/ui") || strings.HasPrefix(path, "/ui/assets/") {
				return next(c)
			}
			c.Response().Header().Set("Cache-Control", "no-store, must-revalidate")
			method := c.Request().Method
			if method == http.MethodGet || method == http.MethodHead {
				return next(c)
			}
			// Mutations only, and this is why: twice now a control appeared to do nothing and
			// there was no way to tell whether the click had even reached the router. Without
			// that one fact the diagnosis is guesswork — a rejected request, a stale cached
			// page and a browser that never sent anything all look identical from the outside.
			err := next(c)
			status := c.Response().Status
			if err != nil {
				var httpError *echo.HTTPError
				if errors.As(err, &httpError) {
					status = httpError.Code
				}
			}
			slog.Info("dashboard action", "method", method, "path", path, "status", status,
				"origin", c.Request().Header.Get("Origin"), "error", err)
			return err
		}
	})
	fileServer := http.FileServer(http.FS(static))
	e.GET("/ui/assets/*", func(c echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-cache")
		http.StripPrefix("/ui/assets/", fileServer).ServeHTTP(c.Response(), c.Request())
		return nil
	})
	return nil
}

func (s *Service) pageHandler(tab string) echo.HandlerFunc {
	return func(c echo.Context) error { return s.renderPage(c, tab) }
}

func (s *Service) tabHandler(c echo.Context) error {
	tab := normalizeTab(c.Param("tab"))
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.tab.ExecuteTemplate(c.Response(), "tab", s.pageData(c, tab))
}

func (s *Service) renderPage(c echo.Context, tab string) error {
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	c.Response().Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	return s.index.Execute(c.Response(), s.pageData(c, tab))
}

func (s *Service) providerDetailHandler(c echo.Context) error {
	return s.renderPage(c, "provider:"+normalizeProviderID(c.Param("id")))
}

func (s *Service) providerTabHandler(c echo.Context) error {
	id := normalizeProviderID(c.Param("id"))
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.tab.ExecuteTemplate(c.Response(), "tab", s.pageData(c, "provider:"+id))
}

func (s *Service) pageData(c echo.Context, tab string) viewData {
	accounts := s.accountViews(c.Request().Context())
	data := withTab(newViewData(accounts), tab)
	data.Models = s.modelIDs(c.Request().Context())
	data.BaseURL = defaultLiteRouterBaseURL
	data.Providers = s.providerSummaries(accounts)
	// Loaded for every tab because the provider grid and the routing map both read it.
	custom := s.loadCustomProviders(c.Request().Context())
	data.CustomProviders = custom
	data.Providers = append(data.Providers, customProviderInfos(custom)...)
	if data.Tab == "providers" {
		data.CustomProviderModels = s.customProviderModels(c.Request().Context(), custom)
	}
	if data.Tab == "provider-detail" && data.Provider != nil {
		for index := range custom {
			if custom[index].Prefix != data.Provider.ID {
				continue
			}
			data.CustomProvider = &custom[index]
			// withTab could only build a placeholder from the id, so the hero showed the
			// bare prefix, an empty description and a connection count taken from the
			// OAuth pool. Replace it with the provider's own summary, where "total"
			// means its keys.
			info := customProviderInfos(custom[index : index+1])[0]
			data.Provider = &info
			data.Total, data.Enabled, data.Exhausted = info.Total, info.Enabled, 0
			data.TabHeading = info.Name
			data.CustomProviderModels = s.customProviderModels(c.Request().Context(),
				[]storage.CustomProvider{custom[index]})
			break
		}
	}
	if data.Tab == "endpoint" {
		data.APIKeys = s.loadAPIKeys(c.Request().Context())
	}
	// CLI picker uses full catalog grouped by provider; provider detail only shows that provider's models.
	if data.Tab == "cli" {
		data.CatalogModels = s.loadCatalogModels(c.Request().Context(), "")
		data.ModelGroups = groupCatalogModels(data.CatalogModels)
		// Two sources, in this order. What the host client currently has is the truth
		// about what is live, so it wins. When that is stock — nothing applied, or Reset
		// was just pressed — LiteRouter's own draft fills the form so the selection can
		// be re-applied without retyping it. ClaudeApplied says which of the two it is,
		// because a form showing values that are not in effect must not look active.
		if setup, setupErr := clisetup.LoadClaude(); setupErr == nil && setup.Model != "" {
			data.ClaudeSetup = setup
			data.ClaudeApplied = true
		} else if s.settings.GetCLIDraft != nil {
			if draft, draftErr := s.settings.GetCLIDraft(c.Request().Context()); draftErr == nil {
				data.ClaudeSetup = draft.Request()
			}
		}
		// The plan model is LiteRouter's own routing decision, not host CLI config, so
		// it is read from settings rather than from ~/.claude/settings.json.
		if s.settings.GetLongContext != nil {
			if model, percent, longErr := s.settings.GetLongContext(c.Request().Context()); longErr == nil {
				data.LongContextModel, data.LongContextPercent = model, percent
			}
		}
		if s.settings.GetImageRoute != nil {
			if model, textOnly, imageErr := s.settings.GetImageRoute(c.Request().Context()); imageErr == nil {
				data.ImageModel, data.TextOnlyModels = model, textOnly
			}
		}
		if s.settings.GetPlanModel != nil {
			if planModel, planErr := s.settings.GetPlanModel(c.Request().Context()); planErr == nil {
				data.PlanModel = planModel
			}
		}
		// Endpoint defaults to the LiteRouter instance serving this UI. Persisted
		// CLI config may point at an old installation and must not override it.
	}
	if data.Tab == "usage" {
		rangeKey := strings.TrimSpace(c.QueryParam("range"))
		if rangeKey == "" {
			rangeKey = "24h"
		}
		since, rangeKey := usageSince(rangeKey)
		data.UsageRange = rangeKey
		data.UsageSince = since
		data.Usage = s.loadUsage(c.Request().Context(), since)
		data.UsageLive = computeUsageLive(data.Usage)
		data.UsageMap = buildUsageMap(data.Providers, data.Usage, data.UsageLive)
		data.AutoRefresh = true
	}
	if data.Tab == "quota" {
		data.View = "quota"
		data.Sort = "expiring"
		data.AutoRefresh = true
		sortQuotaAccounts(data.Accounts, data.Sort)
	}
	data.Strategy = s.loadStrategy(c.Request().Context())
	if data.Tab == "provider-detail" && data.Provider != nil {
		filtered := make([]AccountView, 0, len(accounts))
		for _, account := range accounts {
			if providerMatches(account.Provider, data.Provider.ID) {
				filtered = append(filtered, account)
			}
		}
		sortAccountConnections(filtered)
		data.Accounts = filtered
		data.View = "connections"
		// A custom provider has no accounts in the pool, so counting them would reset
		// the key totals the block above established and the hero would read
		// "0 connections" next to a keys list that says otherwise.
		if data.CustomProvider == nil {
			data.Total = len(filtered)
			data.Enabled, data.Exhausted = 0, 0
			for _, account := range filtered {
				if account.Enabled {
					data.Enabled++
				}
				if account.QuotaExhausted {
					data.Exhausted++
				}
				// Only kick background quota refresh while the provider detail is open.
				if account.QuotaUpdatedAt.IsZero() || time.Since(account.QuotaUpdatedAt) > 2*time.Minute {
					s.queueQuotaRefresh(account.ID)
				}
			}
			info := *data.Provider
			info.Total, info.Enabled, info.Exhausted = data.Total, data.Enabled, data.Exhausted
			data.Provider = &info
		}
		data.CatalogModels = s.loadCatalogModels(c.Request().Context(), data.Provider.ID)
	}
	return data
}

func normalizeTab(tab string) string {
	if strings.HasPrefix(tab, "provider:") {
		return "provider:" + normalizeProviderID(strings.TrimPrefix(tab, "provider:"))
	}
	switch tab {
	case "providers", "quota", "cli", "endpoint", "usage":
		return tab
	default:
		return "endpoint"
	}
}

func normalizeProviderID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "grok" {
		return "xai"
	}
	return id
}

func (s *Service) accountsHandler(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	accounts := s.accountViews(c.Request().Context())
	provider := normalizeProviderID(c.QueryParam("provider"))
	view := c.QueryParam("view")
	status := strings.ToLower(strings.TrimSpace(c.QueryParam("status")))
	sortKey := strings.ToLower(strings.TrimSpace(c.QueryParam("sort")))
	if sortKey == "" {
		sortKey = "expiring"
	}

	if provider != "" {
		filtered := make([]AccountView, 0, len(accounts))
		for _, account := range accounts {
			if providerMatches(account.Provider, provider) {
				filtered = append(filtered, account)
			}
		}
		accounts = filtered
	}

	if view == "quota" || view == "" && status != "" {
		// status filter only on quota board
		if status != "" {
			filtered := make([]AccountView, 0, len(accounts))
			for _, account := range accounts {
				switch status {
				case "available":
					if account.Enabled && !account.QuotaExhausted {
						filtered = append(filtered, account)
					}
				case "empty":
					if account.QuotaExhausted {
						filtered = append(filtered, account)
					}
				case "disabled":
					if !account.Enabled {
						filtered = append(filtered, account)
					}
				default:
					filtered = append(filtered, account)
				}
			}
			accounts = filtered
		}
		sortQuotaAccounts(accounts, sortKey)
	} else if provider != "" {
		sortAccountConnections(accounts)
	}

	data := newViewData(accounts)
	switch {
	case view == "connections" || (provider != "" && view != "quota"):
		data.View = "connections"
	case view == "quota":
		data.View = "quota"
		data.FilterProvider = provider
		data.FilterStatus = status
		data.Sort = sortKey
		data.AutoRefresh = c.QueryParam("auto") != "0"
	}
	return s.accounts.ExecuteTemplate(c.Response(), "accounts", data)
}

func sortAccountConnections(accounts []AccountView) {
	sort.SliceStable(accounts, func(i, j int) bool {
		a, b := accounts[i], accounts[j]
		if a.Enabled != b.Enabled {
			return a.Enabled
		}
		if a.Weight == b.Weight {
			return a.ID < b.ID
		}
		return a.Weight > b.Weight
	})
}

func sortQuotaAccounts(accounts []AccountView, sortKey string) {
	sort.SliceStable(accounts, func(i, j int) bool {
		a, b := accounts[i], accounts[j]
		if a.Enabled != b.Enabled {
			return a.Enabled
		}
		switch sortKey {
		case "name":
			la, lb := a.Label, b.Label
			if la == "" {
				la = a.ID
			}
			if lb == "" {
				lb = b.ID
			}
			return strings.ToLower(la) < strings.ToLower(lb)
		case "provider":
			if a.Provider == b.Provider {
				return a.QuotaRemainingPercent < b.QuotaRemainingPercent
			}
			return a.Provider < b.Provider
		case "remaining":
			return a.QuotaRemainingPercent < b.QuotaRemainingPercent
		default: // expiring
			ar, br := a.QuotaResetAt, b.QuotaResetAt
			if ar.IsZero() && br.IsZero() {
				return a.QuotaRemainingPercent < b.QuotaRemainingPercent
			}
			if ar.IsZero() {
				return false
			}
			if br.IsZero() {
				return true
			}
			return ar.Before(br)
		}
	})
}

func (s *Service) sessionHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	provided := strings.TrimSpace(c.FormValue("token"))
	valid := subtle.ConstantTimeCompare([]byte(provided), []byte(s.apiToken)) == 1
	if !valid && s.apiKeys.Valid != nil {
		valid = s.apiKeys.Valid(provided)
	}
	if !valid {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid API token")
	}
	c.SetCookie(&http.Cookie{
		Name: "literouter_session", Value: provided, Path: "/ui", MaxAge: 8 * 60 * 60,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	_, err := c.Response().Write([]byte(`<span class="success">Dashboard actions unlocked for 8 hours.</span>`))
	return err
}

func (s *Service) keysHandler(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.tab.ExecuteTemplate(c.Response(), "api-keys", viewData{APIKeys: s.loadAPIKeys(c.Request().Context())})
}

func (s *Service) createKeyHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.apiKeys.Create == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "API keys unavailable")
	}
	created, err := s.apiKeys.Create(c.Request().Context(), c.FormValue("name"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	data := viewData{APIKeys: s.loadAPIKeys(c.Request().Context()), CreatedKey: &created}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.tab.ExecuteTemplate(c.Response(), "api-keys", data)
}

func (s *Service) toggleKeyHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.apiKeys.SetEnabled == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "API keys unavailable")
	}
	enabled := c.FormValue("enabled") == "true"
	if err := s.apiKeys.SetEnabled(c.Request().Context(), c.Param("id"), enabled); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.tab.ExecuteTemplate(c.Response(), "api-keys", viewData{APIKeys: s.loadAPIKeys(c.Request().Context())})
}

func (s *Service) deleteKeyHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.apiKeys.Delete == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "API keys unavailable")
	}
	if err := s.apiKeys.Delete(c.Request().Context(), c.Param("id")); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.tab.ExecuteTemplate(c.Response(), "api-keys", viewData{APIKeys: s.loadAPIKeys(c.Request().Context())})
}

func (s *Service) loadAPIKeys(ctx context.Context) []storage.APIKey {
	if s.apiKeys.List == nil {
		return nil
	}
	keys, err := s.apiKeys.List(ctx)
	if err != nil {
		return nil
	}
	return keys
}

func (s *Service) modelsHandler(c echo.Context) error {
	provider := normalizeProviderID(c.QueryParam("provider"))
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.renderModelCatalog(c, provider)
}

func (s *Service) addModelHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.modelsHook.Add == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "model catalog unavailable")
	}
	provider := normalizeProviderID(c.FormValue("provider"))
	if provider == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "provider is required")
	}
	window := 0
	if raw := strings.TrimSpace(c.FormValue("context_window")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1_000 || parsed > 10_000_000 {
			return echo.NewHTTPError(http.StatusBadRequest, "context window must be between 1,000 and 10,000,000 tokens")
		}
		window = parsed
	}
	// The prefix is applied here for every provider, so only the model name is ever
	// typed. Both the Providers tab and the provider detail page post to this handler
	// and only one of them sends target=custom, so correctness must not depend on it.
	modelID, normErr := normalizeCatalogModelID(provider, c.FormValue("id"))
	if normErr != nil {
		if c.FormValue("target") == "custom" {
			return s.renderCustomProviders(c, http.StatusBadRequest, normErr.Error())
		}
		return echo.NewHTTPError(http.StatusBadRequest, normErr.Error())
	}
	if _, err := s.modelsHook.Add(c.Request().Context(), provider, modelID, c.FormValue("label"), window); err != nil {
		// The custom-provider form lives inside the providers list, so its errors have
		// to come back as that list or htmx would swap the list away.
		if c.FormValue("target") == "custom" {
			return s.renderCustomProviders(c, http.StatusBadRequest, err.Error())
		}
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if c.FormValue("target") == "custom" {
		return s.renderCustomProviders(c, http.StatusOK, "")
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.renderModelCatalog(c, provider)
}

func (s *Service) updateModelContextHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.modelsHook.SetContextWindow == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "model context configuration unavailable")
	}
	provider := normalizeProviderID(c.FormValue("provider"))
	id, err := url.PathUnescape(c.Param("id"))
	if err != nil || provider == "" || strings.TrimSpace(id) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "provider and model id are required")
	}
	window, err := strconv.Atoi(strings.TrimSpace(c.FormValue("context_window")))
	if err != nil || window < 1_000 || window > 10_000_000 {
		return echo.NewHTTPError(http.StatusBadRequest, "context window must be between 1,000 and 10,000,000 tokens")
	}
	if err := s.modelsHook.SetContextWindow(c.Request().Context(), provider, id, window); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.renderModelCatalog(c, provider)
}

// updateModelEffortHandler pins the reasoning effort for one model.
//
// Claude Code carries a single session-wide effortLevel and drives it with /effort, so
// per-model effort cannot come from the client. It is applied here instead, on the model
// actually being called — which also means a fallback candidate uses its own setting
// rather than inheriting one meant for a different model.
func (s *Service) updateModelEffortHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.modelsHook.SetEffort == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "model effort configuration unavailable")
	}
	provider := normalizeProviderID(c.FormValue("provider"))
	id, err := url.PathUnescape(c.Param("id"))
	if err != nil || provider == "" || strings.TrimSpace(id) == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "provider and model id are required")
	}
	if err := s.modelsHook.SetEffort(c.Request().Context(), provider, id, c.FormValue("effort")); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.renderModelCatalog(c, provider)
}

// compactionAdviceHandler renders the per-model compaction recommendations.
func (s *Service) compactionAdviceHandler(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.tab.ExecuteTemplate(c.Response(), "compaction-advice", s.compactionView(c))
}

// applyCompactionAdviceHandler writes one recommendation into the catalog. It is a
// deliberate, per-model click: lowering a context window makes the client compact
// earlier, which discards context, so it is never applied on the proxy's own initiative.
func (s *Service) applyCompactionAdviceHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.modelsHook.SetContextWindow == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "model context configuration unavailable")
	}
	provider := normalizeProviderID(c.FormValue("provider"))
	model := strings.TrimSpace(c.FormValue("model"))
	window, err := strconv.Atoi(strings.TrimSpace(c.FormValue("window")))
	if provider == "" || model == "" || err != nil || window < 1_000 {
		return echo.NewHTTPError(http.StatusBadRequest, "provider, model and window are required")
	}
	view := s.compactionView(c)
	if err := s.modelsHook.SetContextWindow(c.Request().Context(), provider, model, window); err != nil {
		view.AdviceNote = err.Error()
	} else {
		// Re-read so the row reflects the change and drops out of the recommendation
		// list on its own, rather than being hidden client-side.
		view = s.compactionView(c)
		view.AdviceNote = fmt.Sprintf("%s now compacts near %dk.", model, window/1000)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.tab.ExecuteTemplate(c.Response(), "compaction-advice", view)
}

// divInt and pctFraction keep the advice table readable without pushing formatting
// decisions into the handler.
func divInt(value, by int) int {
	if by == 0 {
		return 0
	}
	return value / by
}

func pctFraction(value float64) string {
	return strconv.FormatFloat(value*100, 'f', 0, 64)
}

// registerCompactionAdviceHandler adds a model the proxy has served but nobody
// registered, then sets the recommended window on it.
//
// The id is taken verbatim from usage rather than through the Add-model form's prefix
// normalisation. Windows are resolved by the id the client actually asks for, so a
// catalog entry stored as "ag/gemini-3.6-flash-high" would never apply to the traffic
// that produced the recommendation — the row would look applied and change nothing.
func (s *Service) registerCompactionAdviceHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.modelsHook.Add == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "model catalog unavailable")
	}
	provider := normalizeProviderID(c.FormValue("provider"))
	model := strings.TrimSpace(c.FormValue("model"))
	window, err := strconv.Atoi(strings.TrimSpace(c.FormValue("window")))
	if provider == "" || model == "" || err != nil || window < 1_000 {
		return echo.NewHTTPError(http.StatusBadRequest, "provider, model and window are required")
	}
	view := s.compactionView(c)
	if _, addErr := s.modelsHook.Add(c.Request().Context(), provider, model, "", window); addErr != nil {
		view.AdviceNote = addErr.Error()
	} else {
		view = s.compactionView(c)
		view.AdviceNote = fmt.Sprintf("%s added to the catalog and set to compact near %dk.", model, window/1000)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.tab.ExecuteTemplate(c.Response(), "compaction-advice", view)
}

func (s *Service) compactionView(c echo.Context) viewData {
	data := viewData{}
	if s.usage.Compaction == nil {
		data.AdviceNote = "usage analytics unavailable"
		return data
	}
	advice, err := s.usage.Compaction(c.Request().Context())
	if err != nil {
		data.AdviceNote = err.Error()
		return data
	}
	data.Advice = advice
	data.AdviceGroups = groupAdvice(advice)
	return data
}

func (s *Service) testModelHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.modelsHook.Test == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "model test unavailable")
	}
	modelID := strings.TrimSpace(c.FormValue("id"))
	if modelID == "" {
		modelID = strings.TrimSpace(c.FormValue("model"))
	}
	if modelID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "model id is required")
	}
	result, err := s.modelsHook.Test(c.Request().Context(), modelID)
	if err != nil {
		result = ModelTestResult{OK: false, Model: modelID, Error: err.Error()}
	}
	if result.Model == "" {
		result.Model = modelID
	}
	// Prefer JSON for fetch-based UI; fall back to tiny HTML flash for hx.
	accept := c.Request().Header.Get("Accept")
	if strings.Contains(accept, "application/json") || c.QueryParam("format") == "json" {
		status := http.StatusOK
		if !result.OK {
			status = http.StatusBadGateway
		}
		return c.JSON(status, result)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	if result.OK {
		_, err = fmt.Fprintf(c.Response(), `<div class="model-test ok"><strong>OK</strong> <span>%s</span> <code>%s</code></div>`, template.HTMLEscapeString(result.Latency), template.HTMLEscapeString(result.Preview))
		return err
	}
	_, err = fmt.Fprintf(c.Response(), `<div class="model-test bad"><strong>Failed</strong> <span>%s</span></div>`, template.HTMLEscapeString(result.Error))
	return err
}

func (s *Service) deleteModelHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.modelsHook.Delete == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "model catalog unavailable")
	}
	provider := normalizeProviderID(c.QueryParam("provider"))
	if provider == "" {
		provider = normalizeProviderID(c.FormValue("provider"))
	}
	if provider == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "provider is required")
	}
	id, err := url.PathUnescape(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid model id")
	}
	custom := c.QueryParam("target") == "custom" || c.FormValue("target") == "custom"
	if err := s.modelsHook.Delete(c.Request().Context(), provider, id); err != nil {
		if custom {
			return s.renderCustomProviders(c, http.StatusBadRequest, err.Error())
		}
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if custom {
		return s.renderCustomProviders(c, http.StatusOK, "")
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.renderModelCatalog(c, provider)
}

func (s *Service) renderModelCatalog(c echo.Context, provider string) error {
	provider = normalizeProviderID(provider)
	data := viewData{
		CatalogModels: s.loadCatalogModels(c.Request().Context(), provider),
		Models:        s.modelIDs(c.Request().Context()),
	}
	if provider != "" {
		info := providerInfoByID(provider)
		data.Provider = &info
	}
	return s.tab.ExecuteTemplate(c.Response(), "model-catalog", data)
}

func (s *Service) loadCatalogModels(ctx context.Context, provider string) []storage.CatalogModel {
	if s.modelsHook.List == nil {
		return nil
	}
	models, err := s.modelsHook.List(ctx, normalizeProviderID(provider))
	if err != nil {
		return nil
	}
	return models
}

func (s *Service) modelIDs(ctx context.Context) []string {
	seen := map[string]struct{}{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, id := range s.models {
		add(id)
	}
	for _, model := range s.loadCatalogModels(ctx, "") {
		add(model.ID)
	}
	return ids
}

func groupCatalogModels(models []storage.CatalogModel) []ModelGroup {
	order := []string{"codex", "claude", "xai"}
	buckets := map[string][]storage.CatalogModel{}
	extras := []string{}
	for _, model := range models {
		provider := normalizeProviderID(model.Provider)
		if provider == "" {
			provider = "codex"
		}
		if _, ok := buckets[provider]; !ok {
			known := false
			for _, id := range order {
				if id == provider {
					known = true
					break
				}
			}
			if !known {
				extras = append(extras, provider)
			}
		}
		buckets[provider] = append(buckets[provider], model)
	}
	groups := make([]ModelGroup, 0, len(order)+len(extras))
	add := func(id string) {
		models := buckets[id]
		if len(models) == 0 {
			return
		}
		groups = append(groups, ModelGroup{Provider: providerInfoByID(id), Models: models})
	}
	for _, id := range order {
		add(id)
	}
	for _, id := range extras {
		add(id)
	}
	return groups
}

// isCustomProviderPrefix reports whether a provider id belongs to a custom provider.
func (s *Service) isCustomProviderPrefix(ctx context.Context, provider string) bool {
	for _, definition := range s.loadCustomProviders(ctx) {
		if definition.Prefix == provider {
			return true
		}
	}
	return false
}

// modelIDExample is the placeholder for the add-model field. It has to name the
// provider being viewed: the catalog stores the callable id, so a generic example
// from another provider invites entering something that will never route.
func modelIDExample(providerID string) string {
	switch providerID {
	case "codex", "":
		return "e.g. gpt-5.6-sol"
	case "antigravity":
		return "e.g. gemini-3-flash"
	case "claude":
		return "e.g. claude-opus-4-5"
	case "xai":
		return "e.g. grok-4.5"
	default:
		return "e.g. model-name"
	}
}

// canonicalModelPrefix is the prefix a provider's catalog ids carry, so the operator
// never has to type it. It follows what the catalog actually holds: every codex id is
// "cx/…", every Anthropic id is bare, and a custom provider is addressed by the
// prefix it registered.
//
// Adding a prefix is always safe; removing one is not. "gemini-3-flash" reaches
// Antigravity on its own, but "claude-opus-4-6-thinking" only reaches it as
// "ag/claude-opus-4-6-thinking" — without the prefix that name belongs to Anthropic.
func canonicalModelPrefix(providerID string) string {
	switch providerID {
	case "codex":
		return "cx"
	case "antigravity":
		return "ag"
	case "xai":
		return "xai"
	case "claude", "":
		return ""
	default:
		return providerID
	}
}

// normalizeCatalogModelID turns a typed model name into the callable catalog id by
// applying the provider's prefix. Typing the prefix is accepted but never required.
func normalizeCatalogModelID(providerID, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("model id is required")
	}
	prefix := canonicalModelPrefix(providerID)
	if prefix == "" {
		return id, nil
	}
	before, after, found := strings.Cut(id, "/")
	if !found {
		return prefix + "/" + id, nil
	}
	if !strings.EqualFold(before, prefix) {
		// Another provider's prefix produces an id that routes elsewhere, or nowhere.
		// Refusing beats storing something that silently never works.
		return "", fmt.Errorf("%q is another provider's prefix; type just the model name and %q is added for you", before+"/", prefix+"/")
	}
	if strings.TrimSpace(after) == "" {
		return "", errors.New("model id is missing the model name after the prefix")
	}
	return prefix + "/" + strings.TrimSpace(after), nil
}

func providerInfoByID(id string) ProviderInfo {
	id = normalizeProviderID(id)
	for _, provider := range knownProviders() {
		if provider.ID == id {
			return provider
		}
	}
	// Icon: id would name a file that does not exist for anything unknown, which
	// renders as a broken image rather than as a neutral mark.
	return ProviderInfo{ID: id, Name: providerLabel(id), Icon: providerLogo(id), OAuthValue: id}
}

// customProviderInfos presents user-registered upstreams the same way built-in ones
// are presented, so they appear in the provider grid and the routing map without
// every consumer needing to learn about a second kind of provider. Their keys stand
// in for accounts, which is what "enabled" means for them.
func customProviderInfos(providers []storage.CustomProvider) []ProviderInfo {
	out := make([]ProviderInfo, 0, len(providers))
	for _, definition := range providers {
		name := definition.Name
		if name == "" {
			name = definition.Prefix
		}
		description := "Custom · OpenAI compatible"
		if definition.Kind == storage.CustomKindAnthropic {
			description = "Custom · Anthropic compatible"
		}
		// The wire protocol is not the vendor. Marking FPT AI with OpenAI's logo because
		// it speaks that API reads as "this traffic goes to OpenAI", which is the one
		// thing the routing map exists to answer. The description already says which
		// protocol it speaks.
		icon := providerLogo(definition.Prefix)
		info := ProviderInfo{
			ID: definition.Prefix, Name: name,
			Description: description + " · " + definition.BaseURL, Icon: icon,
		}
		for _, key := range definition.Keys {
			info.Total++
			if key.Enabled && definition.Enabled {
				info.Enabled++
			}
		}
		out = append(out, info)
	}
	return out
}

func (s *Service) providerSummaries(accounts []AccountView) []ProviderInfo {
	providers := knownProviders()
	for i := range providers {
		for _, account := range accounts {
			if !providerMatches(account.Provider, providers[i].ID) {
				continue
			}
			providers[i].Total++
			if account.Enabled {
				providers[i].Enabled++
			}
			if account.QuotaExhausted {
				providers[i].Exhausted++
			}
		}
	}
	return providers
}

func (s *Service) oauthHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.startOAuth == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "OAuth is unavailable")
	}
	provider := normalizeProviderID(c.FormValue("provider"))
	result, err := s.startOAuth(c.Request().Context(), c.FormValue("provider"))
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "OAuth start failed")
	}
	href := result.AuthURL
	if result.VerificationURIComplete != "" {
		href = result.VerificationURIComplete
	} else if result.VerificationURI != "" {
		href = result.VerificationURI
	}
	if href == "" {
		return echo.NewHTTPError(http.StatusBadGateway, "OAuth provider returned no verification URL")
	}
	if parsed, err := url.Parse(href); err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return echo.NewHTTPError(http.StatusBadGateway, "OAuth provider returned an invalid URL")
	}

	title := "Connect provider"
	waiting := "Waiting for authorization…"
	step1 := "Step 1: Open this URL in your browser"
	step2 := "Step 2: Paste the callback URL here"
	step2hint := "After authorization, copy the full URL from your browser."
	placeholder := "http://localhost/callback?code=…"
	switch provider {
	case "codex":
		title = "Connect OpenAI Codex"
		waiting = "Waiting for popup authorization…"
		placeholder = "http://localhost:1455/auth/callback?code=…"
	case "claude":
		title = "Connect Claude"
		waiting = "Waiting for Claude authorization…"
		placeholder = "http://localhost:54545/callback?code=…"
	case "xai", "grok":
		title = "Connect Grok Build OAuth"
		waiting = "Waiting for Grok Build OAuth…"
		step1 = "Step 1: Open this Grok Build OAuth URL in your browser"
		step2 = "Step 2: Paste the full callback URL here"
		step2hint = "Prefer the full localhost callback URL. If the browser only shows a broken page, paste the bare code= value and LiteRouter will pair it with the open session."
		placeholder = "http://127.0.0.1:1456/callback?code=…&state=…  or bare code"
	}

	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return oauthModalTemplate.Execute(c.Response(), map[string]string{
		"Provider": provider, "Title": title, "Waiting": waiting,
		"Step1": step1, "Step2": step2, "Step2Hint": step2hint,
		"Placeholder": placeholder, "AuthURL": href, "UserCode": result.UserCode,
	})
}

func (s *Service) oauthCompleteHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.completeOAuth == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "OAuth complete unavailable")
	}
	provider := strings.TrimSpace(c.FormValue("provider"))
	raw := strings.TrimSpace(c.FormValue("callback"))
	if raw == "" {
		raw = strings.TrimSpace(c.FormValue("code"))
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	if err := s.completeOAuth(c.Request().Context(), provider, raw); err != nil {
		// Return 200 HTML so HTMX swaps the error into the modal (4xx is not swapped by default).
		_, werr := fmt.Fprintf(c.Response(), `<div class="oauth-modal-error" role="alert"><strong>Connect failed</strong><p>%s</p><p class="oauth-error-hint">Best: full URL <code>http://127.0.0.1:1456/callback?code=…&amp;state=…</code>. Also OK: bare <code>code</code> right after clicking Connect (uses the open session).</p></div>`, template.HTMLEscapeString(err.Error()))
		return werr
	}
	_, err := c.Response().Write([]byte(`<div class="oauth-modal-success" data-oauth-done="1"><strong>Connected</strong><p>Account saved. You can close this dialog.</p></div>`))
	return err
}

func sameOrigin(request *http.Request) bool {
	source := request.Header.Get("Origin")
	if source == "" {
		source = request.Header.Get("Referer")
	}
	if source == "" {
		return false
	}
	parsed, err := url.Parse(source)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, request.Host) && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func accountID(c echo.Context) (string, error) {
	id, err := url.PathUnescape(c.Param("id"))
	if err != nil || id == "" {
		return "", echo.NewHTTPError(http.StatusBadRequest, "invalid account ID")
	}
	return id, nil
}

func (s *Service) bulkAccountsHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.updateAccount == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "account update is unavailable")
	}
	action := strings.TrimSpace(c.FormValue("action"))
	provider := normalizeProviderID(c.FormValue("provider"))
	accounts := s.accountViews(c.Request().Context())
	for _, account := range accounts {
		if provider != "" && !providerMatches(account.Provider, provider) {
			continue
		}
		switch action {
		case "disable-empty":
			if account.QuotaExhausted && account.Enabled {
				_ = s.updateAccount(c.Request().Context(), account.ID, false, account.Weight)
			}
		case "enable-available":
			if !account.QuotaExhausted && !account.Enabled {
				_ = s.updateAccount(c.Request().Context(), account.ID, true, account.Weight)
			}
		}
	}
	// Re-render quota board with current filters.
	c.Request().URL.RawQuery = "view=quota&provider=" + url.QueryEscape(provider) +
		"&status=" + url.QueryEscape(c.FormValue("status")) +
		"&sort=" + url.QueryEscape(c.FormValue("sort")) +
		"&auto=" + url.QueryEscape(c.FormValue("auto"))
	return s.accountsHandler(c)
}

func (s *Service) updateHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.updateAccount == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "account update is unavailable")
	}
	id, err := accountID(c)
	if err != nil {
		return err
	}
	weight, err := strconv.Atoi(c.FormValue("weight"))
	if err != nil || weight < 1 || weight > 100 {
		return echo.NewHTTPError(http.StatusBadRequest, "weight must be between 1 and 100")
	}
	enabled := c.FormValue("enabled") == "true"
	if err := s.updateAccount(c.Request().Context(), id, enabled, weight); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "account update failed")
	}
	view, ok := s.accountView(c.Request().Context(), id)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "account not found")
	}
	if c.FormValue("view") == "connections" {
		return s.renderConnections(c, view.Provider)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.account.ExecuteTemplate(c.Response(), "account", view)
}

func (s *Service) priorityHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.updateAccount == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "account update is unavailable")
	}
	id, err := accountID(c)
	if err != nil {
		return err
	}
	view, ok := s.accountView(c.Request().Context(), id)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "account not found")
	}
	weight := view.Weight
	if c.FormValue("dir") == "up" {
		weight = min(100, weight+1)
	} else {
		weight = max(1, weight-1)
	}
	if err := s.updateAccount(c.Request().Context(), id, view.Enabled, weight); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "account update failed")
	}
	provider := c.FormValue("provider")
	if provider == "" {
		provider = view.Provider
	}
	return s.renderConnections(c, provider)
}

func (s *Service) strategyHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	strategy := strings.ToLower(strings.TrimSpace(c.FormValue("strategy")))
	switch strategy {
	case "round_robin", "weighted", "least_used", "least_used_rpm", "sticky", "sticky_soft", "failover", "smart":
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "invalid strategy")
	}
	if s.settings.SetStrategy == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "strategy settings unavailable")
	}
	if err := s.settings.SetStrategy(c.Request().Context(), strategy); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	_, err := fmt.Fprintf(c.Response(), `<span class="pill ok">%s</span>`, template.HTMLEscapeString(strategyLabel(strategy)))
	return err
}

// routingHandler sets the router's own model overrides: the plan-mode model and the
// long-context rule. Neither is written to the host client's config, so both take effect
// on the running gateway with no restart.
//
// Any model id is accepted, including one the catalog has never seen: a custom provider
// prefix is resolved at request time, so validating against the catalog here would reject
// ids that route perfectly well. An empty value turns that override off.
func (s *Service) routingHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.settings.SetPlanModel == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "routing settings unavailable")
	}
	planModel, err := routingModelValue(c, "plan_model")
	if err != nil {
		return err
	}
	longModel, err := routingModelValue(c, "long_context_model")
	if err != nil {
		return err
	}
	imageModel, err := routingModelValue(c, "image_model")
	if err != nil {
		return err
	}
	// A list, so it gets a longer allowance than a single id — but still bounded, since it
	// arrives from a form field.
	textOnly := strings.TrimSpace(c.FormValue("text_only_models"))
	if strings.ContainsAny(textOnly, "\x00\r\n") || len(textOnly) > 1024 {
		return echo.NewHTTPError(http.StatusBadRequest, "text-only model list contains invalid characters or is too long")
	}
	// Blank means "keep the gateway's default share" rather than zero, which would read as
	// "route every turn".
	percent := 0
	if raw := strings.TrimSpace(c.FormValue("long_context_percent")); raw != "" {
		percent, err = strconv.Atoi(raw)
		if err != nil || percent < 1 || percent > 99 {
			return echo.NewHTTPError(http.StatusBadRequest, "long context threshold must be between 1 and 99 percent")
		}
	}
	if err := s.settings.SetPlanModel(c.Request().Context(), planModel); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if s.settings.SetLongContext != nil {
		if err := s.settings.SetLongContext(c.Request().Context(), longModel, percent); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}
	if s.settings.SetImageRoute != nil {
		if err := s.settings.SetImageRoute(c.Request().Context(), imageModel, textOnly); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	c.Response().Header().Set("Cache-Control", "no-store")
	parts := make([]string, 0, 2)
	if planModel == "" {
		parts = append(parts, "plan turns use the requested model")
	} else {
		parts = append(parts, "plan → "+template.HTMLEscapeString(planModel))
	}
	switch {
	case longModel == "":
		parts = append(parts, "no long-context override")
	case percent == 0:
		// The effective share lives in the gateway. Naming a number here would mean
		// duplicating its default and going stale the moment that changes.
		parts = append(parts, "over the default share → "+template.HTMLEscapeString(longModel))
	default:
		parts = append(parts, fmt.Sprintf("over %d%% → %s", percent, template.HTMLEscapeString(longModel)))
	}
	switch {
	case textOnly == "":
		parts = append(parts, "no text-only models declared")
	case imageModel == "":
		// Worth saying out loud: this combination refuses image turns rather than routing
		// them, which is deliberate but not what someone half-configuring it expects.
		parts = append(parts, "image turns on "+template.HTMLEscapeString(textOnly)+" will be refused")
	default:
		parts = append(parts, "images from "+template.HTMLEscapeString(textOnly)+" → "+template.HTMLEscapeString(imageModel))
	}
	_, err = fmt.Fprintf(c.Response(),
		`<div class="cli-flash ok"><strong>Routing saved</strong><span> · %s</span></div>`,
		strings.Join(parts, " · "))
	return err
}

// routingModelValue reads and checks one model field from the routing form.
func routingModelValue(c echo.Context, field string) (string, error) {
	value := strings.TrimSpace(c.FormValue(field))
	if strings.ContainsAny(value, "\x00\r\n") || len(value) > 128 {
		return "", echo.NewHTTPError(http.StatusBadRequest, field+" contains invalid characters or is too long")
	}
	return value, nil
}

func (s *Service) deleteHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.deleteAccount == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "account deletion is unavailable")
	}
	id, err := accountID(c)
	if err != nil {
		return err
	}
	if err := s.deleteAccount(c.Request().Context(), id); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "account deletion failed")
	}
	return c.NoContent(http.StatusOK)
}

func (s *Service) resetCreditsHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.resetCodex == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "Codex reset credits unavailable")
	}
	id, err := accountID(c)
	if err != nil {
		return err
	}
	view, ok := s.accountView(c.Request().Context(), id)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "account not found")
	}
	if view.Provider != "codex" {
		return echo.NewHTTPError(http.StatusBadRequest, "reset credits are only for Codex")
	}
	if err := s.resetCodex(c.Request().Context(), id); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, err.Error())
	}
	// refresh view after reset
	if s.refreshQuota != nil {
		_ = s.refreshQuota(c.Request().Context(), id)
	}
	view, ok = s.accountView(c.Request().Context(), id)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "account not found")
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.account.ExecuteTemplate(c.Response(), "account", view)
}

func (s *Service) quotaHandler(c echo.Context) error {
	if !sameOrigin(c.Request()) {
		return echo.NewHTTPError(http.StatusForbidden, "cross-origin request denied")
	}
	if s.refreshQuota == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "quota refresh is unavailable")
	}
	id, err := accountID(c)
	if err != nil {
		return err
	}
	if err := s.refreshQuota(c.Request().Context(), id); err != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "quota refresh failed")
	}
	view, ok := s.accountView(c.Request().Context(), id)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "account not found")
	}
	if c.Request().Header.Get("HX-Target") == "accounts" {
		return s.renderConnections(c, view.Provider)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.account.ExecuteTemplate(c.Response(), "account", view)
}

func (s *Service) renderConnections(c echo.Context, provider string) error {
	accounts := s.accountViews(c.Request().Context())
	provider = normalizeProviderID(provider)
	filtered := make([]AccountView, 0, len(accounts))
	for _, account := range accounts {
		if providerMatches(account.Provider, provider) {
			filtered = append(filtered, account)
		}
	}
	sortAccountConnections(filtered)
	data := newViewData(filtered)
	data.View = "connections"
	c.Response().Header().Set(echo.HeaderContentType, "text/html; charset=utf-8")
	return s.accounts.ExecuteTemplate(c.Response(), "accounts", data)
}

func (s *Service) loadStrategy(ctx context.Context) string {
	if s.settings.GetStrategy == nil {
		return "smart"
	}
	value, err := s.settings.GetStrategy(ctx)
	if err != nil || value == "" {
		return "smart"
	}
	return value
}

func strategyLabel(strategy string) string {
	switch strategy {
	case "round_robin":
		return "Round Robin"
	case "weighted":
		return "Weighted"
	case "least_used":
		return "Least Used"
	case "least_used_rpm":
		return "Least Used RPM"
	case "sticky":
		return "Sticky"
	case "failover":
		return "Failover"
	default:
		return "Smart"
	}
}

func (s *Service) accountViews(ctx context.Context) []AccountView {
	accounts := s.pool.List()
	views := make([]AccountView, 0, len(accounts))
	for _, account := range accounts {
		views = append(views, s.buildAccountView(ctx, account))
	}
	return views
}

func (s *Service) accountView(ctx context.Context, id string) (AccountView, bool) {
	account, ok := s.pool.Get(id)
	if !ok {
		return AccountView{}, false
	}
	return s.buildAccountView(ctx, account), true
}

func (s *Service) buildAccountView(ctx context.Context, account pool.Account) AccountView {
	view := AccountView{Account: account}
	if s.listSnapshots == nil {
		return view
	}
	snapshots, err := s.listSnapshots(ctx, account.ID)
	if err != nil {
		return view
	}
	for _, snapshot := range snapshots {
		// Hide detail/noise windows on cards: review_*, weekly_api/chat breakdown.
		// Primary weekly already includes total SuperGrok usage percent.
		if snapshot.Unlimited || strings.HasPrefix(snapshot.Key, "review_") || strings.HasPrefix(snapshot.Key, "weekly_") {
			continue
		}
		view.Windows = append(view.Windows, QuotaWindowView{
			Key: snapshot.Key, Label: windowLabel(snapshot.Key),
			Used: snapshot.Used, Total: snapshot.Total, Remaining: snapshot.Remaining,
			RemainingPercent: snapshot.RemainingPercent, ResetAt: snapshot.ResetAt,
			FetchedAt: snapshot.FetchedAt, Stale: snapshot.FetchedAt.IsZero() || time.Since(snapshot.FetchedAt) > 2*time.Minute,
			Exhausted: snapshot.Exhausted, Unlimited: snapshot.Unlimited,
		})
	}
	// Shortest reset window first (session/weekly before monthly).
	sort.SliceStable(view.Windows, func(i, j int) bool {
		a, b := view.Windows[i], view.Windows[j]
		ar, br := a.ResetAt, b.ResetAt
		if !ar.IsZero() && !br.IsZero() && !ar.Equal(br) {
			return ar.Before(br)
		}
		if ar.IsZero() != br.IsZero() {
			return !ar.IsZero()
		}
		// Prefer known short windows by key when reset equal/missing.
		rank := func(key string) int {
			switch key {
			case "session":
				return 1
			case "weekly":
				return 2
			case "weekly_api":
				return 3
			case "weekly_chat":
				return 4
			case "monthly":
				return 5
			case "on_demand":
				return 6
			case "credits":
				return 7
			case "prepaid":
				return 8
			default:
				return 50
			}
		}
		if ri, rj := rank(a.Key), rank(b.Key); ri != rj {
			return ri < rj
		}
		return a.Key < b.Key
	})
	return view
}

func (s *Service) queueQuotaRefresh(accountID string) {
	if s.refreshQuota == nil || accountID == "" {
		return
	}
	s.refreshMu.Lock()
	if _, busy := s.refreshing[accountID]; busy {
		s.refreshMu.Unlock()
		return
	}
	s.refreshing[accountID] = struct{}{}
	s.refreshMu.Unlock()
	go func(id string) {
		defer func() {
			s.refreshMu.Lock()
			delete(s.refreshing, id)
			s.refreshMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.refreshQuota(ctx, id)
	}(accountID)
}
