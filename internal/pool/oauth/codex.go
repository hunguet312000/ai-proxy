package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	codexClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexIssuer          = "https://auth.openai.com"
	codexCallbackURL     = "http://localhost:1455/auth/callback"
	codexFallbackPort    = 1457
	codexScope           = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	codexRefreshInterval = 8 * 24 * time.Hour
)

type CodexProvider struct {
	client      *http.Client
	clientID    string
	issuer      string
	redirectURL string
}

func NewCodexProvider(client *http.Client) *CodexProvider {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &CodexProvider{
		client:      client,
		clientID:    codexClientID,
		issuer:      codexIssuer,
		redirectURL: codexCallbackURL,
	}
}

func (p *CodexProvider) Name() string { return "codex" }

func (p *CodexProvider) PreferredPort() int { return 1455 }

func (p *CodexProvider) FallbackPort() int { return codexFallbackPort }

func (p *CodexProvider) CallbackPath() string { return "/auth/callback" }

func (p *CodexProvider) RedirectURL() string { return p.redirectURL }

func (p *CodexProvider) WithRedirectURL(redirectURL string) OAuthProvider {
	copy := *p
	copy.redirectURL = redirectURL
	return &copy
}

func (p *CodexProvider) withRedirectURL(redirectURL string) *CodexProvider {
	return p.WithRedirectURL(redirectURL).(*CodexProvider)
}

func (p *CodexProvider) AuthURL(state, codeChallenge string) string {
	query := url.Values{
		"response_type":              {"code"},
		"client_id":                  {p.clientID},
		"redirect_uri":               {p.redirectURL},
		"scope":                      {codexScope},
		"code_challenge":             {codeChallenge},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"state":                      {state},
		"originator":                 {"codex_cli_rs"},
	}
	return strings.TrimRight(p.issuer, "/") + "/oauth/authorize?" + query.Encode()
}

func (p *CodexProvider) Exchange(ctx context.Context, code, verifier string) (*TokenSet, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {p.clientID},
		"code":          {code},
		"redirect_uri":  {p.redirectURL},
		"code_verifier": {verifier},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.issuer, "/")+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create Codex token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	return p.doTokenRequest(request, "exchange")
}

func (p *CodexProvider) Refresh(ctx context.Context, refreshToken string) (*TokenSet, error) {
	body, err := json.Marshal(map[string]string{
		"client_id":     p.clientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Codex refresh request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.issuer, "/")+"/oauth/token", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create Codex refresh request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	return p.doTokenRequest(request, "refresh")
}

func (p *CodexProvider) AccountInfo(_ context.Context, token *TokenSet) (*AccountInfo, error) {
	claims, err := ParseJWTClaims(token.IDToken)
	if err != nil {
		return nil, fmt.Errorf("parse Codex identity: %w", err)
	}
	id := claims.OpenAI.AccountID
	if id == "" {
		id = claims.Subject
	}
	if id == "" {
		return nil, fmt.Errorf("Codex token does not contain an account ID")
	}
	return &AccountInfo{ID: id, Email: claims.Email, Plan: claims.OpenAI.PlanType}, nil
}

func (p *CodexProvider) ShouldRefresh(token *TokenSet, now time.Time) bool {
	if token.RefreshToken == "" {
		return false
	}
	if !token.ExpiresAt.IsZero() {
		return !token.ExpiresAt.After(now.Add(5 * time.Minute))
	}
	return !token.LastRefreshAt.IsZero() && token.LastRefreshAt.Before(now.Add(-codexRefreshInterval))
}

func (p *CodexProvider) doTokenRequest(request *http.Request, operation string) (*TokenSet, error) {
	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Codex token %s: %w", operation, err)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := decodeJSONResponse(response, &payload); err != nil {
		return nil, fmt.Errorf("Codex token %s: %w", operation, err)
	}
	if payload.AccessToken == "" {
		return nil, fmt.Errorf("Codex token %s returned no access token", operation)
	}
	now := time.Now().UTC()
	return &TokenSet{
		AccessToken:   payload.AccessToken,
		RefreshToken:  payload.RefreshToken,
		IDToken:       payload.IDToken,
		TokenType:     payload.TokenType,
		Scope:         payload.Scope,
		ExpiresAt:     tokenExpiry(payload.AccessToken, payload.ExpiresIn, now),
		LastRefreshAt: now,
	}, nil
}

func tokenExpiry(accessToken string, expiresIn int64, now time.Time) time.Time {
	if expiresIn > 0 {
		return now.Add(time.Duration(expiresIn) * time.Second)
	}
	claims, err := ParseJWTClaims(accessToken)
	if err == nil && claims.ExpiresAt > 0 {
		return time.Unix(claims.ExpiresAt, 0).UTC()
	}
	return time.Time{}
}
