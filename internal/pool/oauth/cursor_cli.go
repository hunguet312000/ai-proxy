package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Cursor CLI login is a deep-link + polling flow, not the browser-callback OAuth
// the other providers use. The CLI builds a challenge from a verifier, shows the
// user `cursor.com/loginDeepControl?challenge=…&uuid=…&mode=login&redirectTarget=cli`,
// and polls `api2.cursor.sh/auth/poll?uuid=…&verifier=…` until the web login
// completes, then receives {accessToken, refreshToken} — a JWT session.
//
// LiteRouter replicates that flow so a Cursor account can be connected the same
// way `agent login` does it, and stores the resulting token set encrypted in the
// account row (the same secret.Box every other provider uses), instead of
// importing a session out of the IDE.
//
// The token set carries a refresh token, unlike the IDE session, so the account
// stays usable after the access token expires.
type CursorCLIProvider struct {
	client *http.Client
	// apiBase is where /auth/poll lives. api2.cursor.sh is the production value;
	// tests override it.
	apiBase string
	// websiteBase is where the deep link points. cursor.com is production; tests
	// override it.
	websiteBase string
}

// NewCursorCLIProvider returns the provider with production endpoints.
func NewCursorCLIProvider(client *http.Client) *CursorCLIProvider {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &CursorCLIProvider{client: client, apiBase: "https://api2.cursor.sh", websiteBase: "https://cursor.com"}
}

// cursorCLIDeepLink is the shape the web login returns the token through.
type cursorCLIDeepLink struct {
	UUID      string
	Verifier  string
	Challenge string
	AuthURL   string
}

// newCursorCLIDeepLink builds the challenge + deep link, mirroring the CLI bundle:
// verifier is 32 random bytes, challenge is base64url(SHA-256(verifier)), and the
// link carries both uuid and challenge to the web login.
func newCursorCLIDeepLink(websiteBase string) (cursorCLIDeepLink, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return cursorCLIDeepLink{}, fmt.Errorf("generate cursor cli verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	uuid, err := randomUUID()
	if err != nil {
		return cursorCLIDeepLink{}, err
	}
	link := fmt.Sprintf("%s/loginDeepControl?challenge=%s&uuid=%s&mode=login&redirectTarget=cli",
		strings.TrimRight(websiteBase, "/"), challenge, url.QueryEscape(uuid))
	return cursorCLIDeepLink{UUID: uuid, Verifier: verifier, Challenge: challenge, AuthURL: link}, nil
}

// OAuthProvider interface compliance. The deep-link flow does not use a browser
// callback, so these return empty / error — the Manager routes cursor through
// StartCursorCLI / CompleteCursorCLI instead.
func (p *CursorCLIProvider) Name() string           { return "cursor" }
func (p *CursorCLIProvider) PreferredPort() int     { return 1458 }
func (p *CursorCLIProvider) CallbackPath() string   { return "/auth/callback" }
func (p *CursorCLIProvider) RedirectURL() string    { return "" }
func (p *CursorCLIProvider) WithRedirectURL(string) OAuthProvider { return p }
func (p *CursorCLIProvider) AuthURL(_, _ string) string { return "" }
func (p *CursorCLIProvider) Exchange(context.Context, string, string) (*TokenSet, error) {
	return nil, errors.New("cursor cli login uses deep-link polling, not an authorization code exchange")
}

// Refresh exchanges the refresh token for a new access token. The endpoint shape
// is inferred from the CLI bundle; the poll response carries both tokens.
func (p *CursorCLIProvider) Refresh(ctx context.Context, refreshToken string) (*TokenSet, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Cursor CLI refresh request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.apiBase, "/")+"/auth/refresh", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create Cursor CLI refresh request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeJSONResponse(p.do(request), &payload); err != nil {
		return nil, err
	}
	if payload.AccessToken == "" {
		return nil, fmt.Errorf("Cursor CLI refresh returned no access token")
	}
	return &TokenSet{
		AccessToken:   payload.AccessToken,
		RefreshToken:  payload.RefreshToken,
		TokenType:     "Bearer",
		ExpiresAt:     tokenExpiry(payload.AccessToken, 0, time.Now().UTC()),
		LastRefreshAt: time.Now().UTC(),
	}, nil
}

func (p *CursorCLIProvider) AccountInfo(_ context.Context, token *TokenSet) (*AccountInfo, error) {
	claims, err := ParseJWTClaims(token.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("parse Cursor CLI identity: %w", err)
	}
	id := claims.Subject
	if id == "" {
		id = claims.Email
	}
	if id == "" {
		return nil, fmt.Errorf("Cursor CLI token does not contain an account ID")
	}
	return &AccountInfo{ID: id, Email: claims.Email, Plan: "Cursor"}, nil
}

func (p *CursorCLIProvider) ShouldRefresh(token *TokenSet, now time.Time) bool {
	if token.RefreshToken == "" {
		return false
	}
	if !token.ExpiresAt.IsZero() {
		return !token.ExpiresAt.After(now.Add(5 * time.Minute))
	}
	return !token.LastRefreshAt.IsZero() && token.LastRefreshAt.Before(now.Add(-8*24*time.Hour))
}

func (p *CursorCLIProvider) do(request *http.Request) *http.Response {
	response, err := p.client.Do(request)
	if err != nil {
		return nil
	}
	return response
}

// pollToken implements the CLI's waitForResult: poll /auth/poll with the verifier
// until the web login completes or the flow expires. 404 means "not yet";
// anything else is parsed as the token response.
func (p *CursorCLIProvider) pollToken(ctx context.Context, verifier string, expiresAt time.Time) (*TokenSet, error) {
	endpoint := fmt.Sprintf("%s/auth/poll?verifier=%s", strings.TrimRight(p.apiBase, "/"), url.QueryEscape(verifier))
	backoff := time.Second
	const maxBackoff = 10 * time.Second
	for {
		if time.Now().After(expiresAt) {
			return nil, fmt.Errorf("Cursor CLI login expired; click Connect and try again")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("create Cursor CLI poll request: %w", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		response, err := p.client.Do(request)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff += time.Second
			}
			continue
		}
		if response.StatusCode == http.StatusNotFound {
			_ = response.Body.Close()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff += time.Second
			}
			continue
		}
		var payload struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		}
		if err := decodeJSONResponse(response, &payload); err != nil {
			return nil, fmt.Errorf("Cursor CLI poll: %w", err)
		}
		if payload.AccessToken == "" {
			return nil, fmt.Errorf("Cursor CLI login returned no access token")
		}
		now := time.Now().UTC()
		return &TokenSet{
			AccessToken:   payload.AccessToken,
			RefreshToken:  payload.RefreshToken,
			TokenType:     "Bearer",
			ExpiresAt:     tokenExpiry(payload.AccessToken, 0, now),
			LastRefreshAt: now,
		}, nil
	}
}
