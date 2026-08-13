package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const (
	antigravityAuthorizeURL      = "https://accounts.google.com/o/oauth2/v2/auth"
	antigravityTokenURL          = "https://oauth2.googleapis.com/token"
	antigravityUserInfoURL       = "https://www.googleapis.com/oauth2/v1/userinfo"
	antigravityLoadCodeAssistURL = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	antigravityOnboardUserURL    = "https://cloudcode-pa.googleapis.com/v1internal:onboardUser"
	antigravityCallbackPort      = 1458
	antigravityCallbackPath      = "/callback"
)

// antigravityCredentials is the OAuth app identity, supplied from LiteRouter's own
// settings (never hardcoded, never env): the client id and secret Google requires for
// the authorize and token endpoints.
type antigravityCredentials struct {
	clientID     string
	clientSecret string
}

type AntigravityProvider struct {
	client            *http.Client
	credentials       atomic.Pointer[antigravityCredentials]
	redirectURL       string
	loadCodeAssistURL string
	onboardUserURL    string
	onboardPollDelay  time.Duration
}

func NewAntigravityProvider(client *http.Client) *AntigravityProvider {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &AntigravityProvider{
		client:            client,
		redirectURL:       fmt.Sprintf("http://127.0.0.1:%d%s", antigravityCallbackPort, antigravityCallbackPath),
		loadCodeAssistURL: antigravityLoadCodeAssistURL, onboardUserURL: antigravityOnboardUserURL, onboardPollDelay: 2 * time.Second,
	}
}

// SetCredentials updates the OAuth app identity on a running provider, so the dashboard
// can change it without a restart. Empty clientID leaves the provider unconfigured —
// AuthURL then yields the Google "missing client_id" error, which the UI surfaces as
// "set the Antigravity client id".
func (p *AntigravityProvider) SetCredentials(clientID, clientSecret string) {
	p.credentials.Store(&antigravityCredentials{clientID: strings.TrimSpace(clientID), clientSecret: strings.TrimSpace(clientSecret)})
}

// Credentials reports the current OAuth app identity.
func (p *AntigravityProvider) Credentials() (string, string) {
	if creds := p.credentials.Load(); creds != nil {
		return creds.clientID, creds.clientSecret
	}
	return "", ""
}
func (p *AntigravityProvider) Name() string         { return "antigravity" }
func (p *AntigravityProvider) PreferredPort() int   { return antigravityCallbackPort }
func (p *AntigravityProvider) CallbackPath() string { return antigravityCallbackPath }
func (p *AntigravityProvider) RedirectURL() string  { return p.redirectURL }
func (p *AntigravityProvider) WithRedirectURL(redirectURL string) OAuthProvider {
	cp := &AntigravityProvider{
		client: p.client, redirectURL: redirectURL,
		loadCodeAssistURL: p.loadCodeAssistURL, onboardUserURL: p.onboardUserURL, onboardPollDelay: p.onboardPollDelay,
	}
	// The credentials pointer is shared, not copied: the noCopy in atomic.Pointer makes
	// copying the struct a vet error, and sharing keeps a later SetCredentials visible.
	cp.credentials.Store(p.credentials.Load())
	return cp
}
func (p *AntigravityProvider) AuthURL(state, challenge string) string {
	clientID, _ := p.Credentials()
	q := url.Values{"client_id": {clientID}, "redirect_uri": {p.redirectURL}, "response_type": {"code"}, "access_type": {"offline"}, "prompt": {"consent"}, "state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {strings.Join([]string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile", "https://www.googleapis.com/auth/cclog", "https://www.googleapis.com/auth/experimentsandconfigs"}, " ")}}
	return antigravityAuthorizeURL + "?" + q.Encode()
}
func (p *AntigravityProvider) Exchange(ctx context.Context, code, verifier string) (*TokenSet, error) {
	clientID, clientSecret := p.Credentials()
	return p.tokenRequest(ctx, url.Values{"code": {code}, "client_id": {clientID}, "client_secret": {clientSecret}, "redirect_uri": {p.redirectURL}, "grant_type": {"authorization_code"}, "code_verifier": {verifier}}, "exchange")
}
func (p *AntigravityProvider) Refresh(ctx context.Context, refreshToken string) (*TokenSet, error) {
	clientID, clientSecret := p.Credentials()
	return p.tokenRequest(ctx, url.Values{"refresh_token": {refreshToken}, "client_id": {clientID}, "client_secret": {clientSecret}, "grant_type": {"refresh_token"}}, "refresh")
}
func (p *AntigravityProvider) AccountInfo(ctx context.Context, token *TokenSet) (*AccountInfo, error) {
	if token == nil || token.AccessToken == "" {
		return nil, fmt.Errorf("Antigravity account has no access token")
	}
	if token.ProjectID == "" {
		projectID, err := p.loadProject(ctx, token.AccessToken)
		if err != nil {
			return nil, err
		}
		token.ProjectID = projectID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, antigravityUserInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	var info struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	response, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Antigravity user info: %w", err)
	}
	if err := decodeJSONResponse(response, &info); err != nil {
		return nil, fmt.Errorf("Antigravity user info: %w", err)
	}
	if info.ID == "" {
		info.ID = info.Email
	}
	if info.ID == "" {
		return nil, fmt.Errorf("Antigravity user info has no identity")
	}
	return &AccountInfo{ID: info.ID, Email: info.Email, Plan: "Google Cloud"}, nil
}
func (p *AntigravityProvider) loadProject(ctx context.Context, accessToken string) (string, error) {
	result, err := p.loadCodeAssist(ctx, accessToken)
	if err != nil {
		return "", err
	}
	if projectID := antigravityProjectID(result.Project); projectID != "" {
		return projectID, nil
	}
	// No project from loadCodeAssist: fall back to onboarding with a tier. 9router uses
	// "legacy-tier" when no default tier is advertised; keep that fallback instead of
	// failing. And if onboarding completes without a project, the connection still works —
	// the project id is metadata, not a prerequisite — so log and continue rather than
	// rejecting the account.
	tierID := "legacy-tier"
	if advertised := antigravityTierID(result.AllowedTiers); advertised != "" {
		tierID = advertised
	}
	for attempt := 0; attempt < 5; attempt++ {
		project, done, err := p.onboardUser(ctx, accessToken, tierID)
		if err != nil {
			return "", err
		}
		if project != "" {
			return project, nil
		}
		if done {
			slog.Warn("antigravity onboarding finished without a project; continuing without one",
				"tier", tierID)
			return "", nil
		}
		if attempt < 4 {
			timer := time.NewTimer(p.onboardPollDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}
	}
	slog.Warn("antigravity onboarding did not complete after 5 attempts; continuing without a project")
	return "", nil
}

type antigravityCodeAssistResult struct {
	Project      json.RawMessage   `json:"cloudaicompanionProject"`
	AllowedTiers []json.RawMessage `json:"allowedTiers"`
}

func (p *AntigravityProvider) loadCodeAssist(ctx context.Context, accessToken string) (antigravityCodeAssistResult, error) {
	var result antigravityCodeAssistResult
	err := p.antigravityJSON(ctx, accessToken, p.loadCodeAssistURL, map[string]any{
		"metadata": antigravityMetadata(), "mode": 1,
	}, &result, "load Code Assist")
	return result, err
}

func (p *AntigravityProvider) onboardUser(ctx context.Context, accessToken, tierID string) (string, bool, error) {
	var result struct {
		Done     bool `json:"done"`
		Response struct {
			Project json.RawMessage `json:"cloudaicompanionProject"`
		} `json:"response"`
		Project json.RawMessage `json:"cloudaicompanionProject"`
	}
	err := p.antigravityJSON(ctx, accessToken, p.onboardUserURL, map[string]any{
		"tierId": tierID, "metadata": antigravityMetadata(),
	}, &result, "onboard user")
	if err != nil {
		return "", false, err
	}
	project := antigravityProjectID(result.Response.Project)
	if project == "" {
		project = antigravityProjectID(result.Project)
	}
	slog.Debug("antigravity onboard response", "done", result.Done,
		"response_project", string(result.Response.Project), "project", string(result.Project), "resolved", project)
	return project, result.Done, nil
}

func (p *AntigravityProvider) antigravityJSON(ctx context.Context, accessToken, endpoint string, payload any, target any, operation string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "google-api-nodejs-client/9.15.1")
	req.Header.Set("X-Goog-Api-Client", "google-cloud-sdk vscode-antigravity/1.107.0")
	req.Header.Set("Client-Metadata", `{"ideType":9,"platform":`+fmt.Sprint(antigravityPlatform())+`,"pluginType":2}`)
	req.Header.Set("x-request-source", "local")
	response, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("Antigravity %s: %w", operation, err)
	}
	if err := decodeJSONResponse(response, target); err != nil {
		return fmt.Errorf("Antigravity %s: %w", operation, err)
	}
	return nil
}

func antigravityMetadata() map[string]any {
	return map[string]any{"ideType": 9, "platform": antigravityPlatform(), "pluginType": 2}
}

func antigravityProjectID(raw json.RawMessage) string {
	var projectID string
	if json.Unmarshal(raw, &projectID) != nil {
		var project struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
		}
		_ = json.Unmarshal(raw, &project)
		projectID = project.ID
		if projectID == "" {
			projectID = project.ProjectID
		}
	}
	return strings.TrimSpace(projectID)
}

func antigravityTierID(tiers []json.RawMessage) string {
	var fallback string
	for _, raw := range tiers {
		var tier struct {
			ID        string `json:"id"`
			TierID    string `json:"tierId"`
			IsDefault bool   `json:"isDefault"`
			Default   bool   `json:"default"`
		}
		if json.Unmarshal(raw, &tier) != nil {
			continue
		}
		id := strings.TrimSpace(tier.ID)
		if id == "" {
			id = strings.TrimSpace(tier.TierID)
		}
		if id != "" && (tier.IsDefault || tier.Default) {
			return id
		}
		if fallback == "" {
			fallback = id
		}
	}
	return fallback
}

func antigravityPlatform() int {
	switch runtime.GOOS {
	case "darwin":
		return 1
	case "windows":
		return 2
	default:
		return 3
	}
}

func (p *AntigravityProvider) tokenRequest(ctx context.Context, form url.Values, operation string) (*TokenSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, antigravityTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	response, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Antigravity token %s: %w", operation, err)
	}
	if err := decodeJSONResponse(response, &result); err != nil {
		return nil, fmt.Errorf("Antigravity token %s: %w", operation, err)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("Antigravity token %s returned no access token", operation)
	}
	now := time.Now().UTC()
	return &TokenSet{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, TokenType: result.TokenType, Scope: result.Scope, ExpiresAt: tokenExpiry(result.AccessToken, result.ExpiresIn, now), LastRefreshAt: now}, nil
}
func (p *AntigravityProvider) ShouldRefresh(token *TokenSet, now time.Time) bool {
	return token.RefreshToken != "" && !token.ExpiresAt.After(now.Add(5*time.Minute))
}
