package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	grokClientID     = "b1a00492-073a-47ea-816f-4c329264a828"
	grokIssuer       = "https://auth.x.ai"
	grokCallbackPort = 1456
	grokCallbackPath = "/callback"
	grokScope        = "openid profile email offline_access grok-cli:access api:access"
	grokUserAgent    = "grok-cli/literouter"
)

type GrokProvider struct {
	client      *http.Client
	clientID    string
	issuer      string
	redirectURL string
}

func NewGrokProvider(client *http.Client) *GrokProvider {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GrokProvider{
		client:      client,
		clientID:    grokClientID,
		issuer:      grokIssuer,
		redirectURL: fmt.Sprintf("http://127.0.0.1:%d%s", grokCallbackPort, grokCallbackPath),
	}
}

func (p *GrokProvider) Name() string       { return "grok" }
func (p *GrokProvider) PreferredPort() int { return grokCallbackPort }
func (p *GrokProvider) FallbackPort() int  { return 0 }
func (p *GrokProvider) CallbackPath() string {
	return grokCallbackPath
}
func (p *GrokProvider) RedirectURL() string { return p.redirectURL }

func (p *GrokProvider) WithRedirectURL(redirectURL string) OAuthProvider {
	copy := *p
	copy.redirectURL = redirectURL
	return &copy
}

func (p *GrokProvider) AuthURL(state, codeChallenge string) string {
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.clientID},
		"redirect_uri":          {p.redirectURL},
		"scope":                 {grokScope},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"plan":                  {"generic"},
		"referrer":              {"cli-proxy-api"},
	}
	return strings.TrimRight(p.issuer, "/") + "/oauth2/authorize?" + query.Encode()
}

func (p *GrokProvider) Exchange(ctx context.Context, code, verifier string) (*TokenSet, error) {
	return p.tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {p.clientID},
		"code":          {code},
		"redirect_uri":  {p.redirectURL},
		"code_verifier": {verifier},
	}, "exchange")
}

func (p *GrokProvider) Refresh(ctx context.Context, refreshToken string) (*TokenSet, error) {
	return p.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {p.clientID},
	}, "refresh")
}

func (p *GrokProvider) AccountInfo(_ context.Context, token *TokenSet) (*AccountInfo, error) {
	claims, err := ParseJWTClaims(token.IDToken)
	if err != nil {
		claims, err = ParseJWTClaims(token.AccessToken)
	}
	if err != nil {
		return nil, fmt.Errorf("parse xAI identity: %w", err)
	}
	id := claims.Subject
	if id == "" {
		id = claims.Email
	}
	if id == "" {
		return nil, fmt.Errorf("xAI token does not contain an account ID")
	}
	return &AccountInfo{ID: id, Email: claims.Email, Plan: "xAI"}, nil
}

func (p *GrokProvider) tokenRequest(ctx context.Context, form url.Values, operation string) (*TokenSet, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.issuer, "/")+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create xAI token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", grokUserAgent)
	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("xAI token %s: %w", operation, err)
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
		return nil, fmt.Errorf("xAI token %s: %w", operation, err)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("xAI token %s returned no access token", operation)
	}
	now := time.Now().UTC()
	return &TokenSet{
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken,
		TokenType: result.TokenType, Scope: result.Scope,
		ExpiresAt: tokenExpiry(result.AccessToken, result.ExpiresIn, now), LastRefreshAt: now,
	}, nil
}
