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
	claudeClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeAuthorize   = "https://claude.ai/oauth/authorize"
	claudeTokenURL    = "https://api.anthropic.com/v1/oauth/token"
	claudeCallbackURL = "http://localhost:1457/callback"
)

type ClaudeProvider struct {
	client       *http.Client
	authorizeURL string
	tokenURL     string
	redirectURL  string
}

func NewClaudeProvider(client *http.Client) *ClaudeProvider {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &ClaudeProvider{client: client, authorizeURL: claudeAuthorize, tokenURL: claudeTokenURL, redirectURL: claudeCallbackURL}
}

func (p *ClaudeProvider) Name() string         { return "claude" }
func (p *ClaudeProvider) PreferredPort() int   { return 1457 }
func (p *ClaudeProvider) CallbackPath() string { return "/callback" }
func (p *ClaudeProvider) RedirectURL() string  { return p.redirectURL }

func (p *ClaudeProvider) WithRedirectURL(redirectURL string) OAuthProvider {
	copy := *p
	copy.redirectURL = redirectURL
	return &copy
}

func (p *ClaudeProvider) AuthURL(state, codeChallenge string) string {
	query := url.Values{
		"code":                  {"true"},
		"client_id":             {claudeClientID},
		"response_type":         {"code"},
		"redirect_uri":          {p.redirectURL},
		"scope":                 {"org:create_api_key user:profile user:inference"},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return p.authorizeURL + "?" + query.Encode()
}

func (p *ClaudeProvider) Exchange(ctx context.Context, code, verifier string) (*TokenSet, error) {
	state := ""
	if before, after, ok := strings.Cut(code, "#"); ok {
		code, state = before, after
	}
	body := map[string]string{
		"code": code, "state": state, "grant_type": "authorization_code",
		"client_id": claudeClientID, "redirect_uri": p.redirectURL, "code_verifier": verifier,
	}
	return p.tokenRequest(ctx, body, "exchange")
}

func (p *ClaudeProvider) Refresh(ctx context.Context, refreshToken string) (*TokenSet, error) {
	return p.tokenRequest(ctx, map[string]string{
		"grant_type": "refresh_token", "refresh_token": refreshToken, "client_id": claudeClientID,
	}, "refresh")
}

func (p *ClaudeProvider) AccountInfo(_ context.Context, token *TokenSet) (*AccountInfo, error) {
	claims, err := ParseJWTClaims(token.IDToken)
	if err != nil {
		claims, err = ParseJWTClaims(token.AccessToken)
	}
	if err != nil {
		return nil, fmt.Errorf("parse Claude identity: %w", err)
	}
	id := claims.Subject
	if id == "" {
		id = claims.Email
	}
	if id == "" {
		return nil, fmt.Errorf("Claude token does not contain an account ID")
	}
	return &AccountInfo{ID: id, Email: claims.Email, Plan: "Claude Code"}, nil
}

func (p *ClaudeProvider) tokenRequest(ctx context.Context, payload map[string]string, operation string) (*TokenSet, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create Claude token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Claude token %s: %w", operation, err)
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := decodeJSONResponse(response, &result); err != nil {
		return nil, fmt.Errorf("Claude token %s: %w", operation, err)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("Claude token %s returned no access token", operation)
	}
	now := time.Now().UTC()
	return &TokenSet{
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken,
		TokenType: result.TokenType, Scope: result.Scope,
		ExpiresAt: tokenExpiry(result.AccessToken, result.ExpiresIn, now), LastRefreshAt: now,
	}, nil
}
