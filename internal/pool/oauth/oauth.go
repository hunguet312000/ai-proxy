package oauth

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var (
	ErrAuthorizationPending = errors.New("authorization pending")
	ErrSlowDown             = errors.New("authorization polling must slow down")
)

type TokenSet struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	IDToken       string    `json:"id_token,omitempty"`
	TokenType     string    `json:"token_type,omitempty"`
	Scope         string    `json:"scope,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	LastRefreshAt time.Time `json:"last_refresh_at,omitempty"`
	ProjectID     string    `json:"project_id,omitempty"`
}

type AccountInfo struct {
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
	Plan  string `json:"plan,omitempty"`
	Quota string `json:"quota,omitempty"`
}

type OAuthProvider interface {
	Name() string
	PreferredPort() int
	CallbackPath() string
	RedirectURL() string
	WithRedirectURL(string) OAuthProvider
	AuthURL(state, codeChallenge string) string
	Exchange(ctx context.Context, code, verifier string) (*TokenSet, error)
	Refresh(ctx context.Context, refreshToken string) (*TokenSet, error)
	AccountInfo(ctx context.Context, token *TokenSet) (*AccountInfo, error)
}

type DeviceAuthorization struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               time.Duration
	Interval                time.Duration
}

type DeviceOAuthProvider interface {
	OAuthProvider
	RequestDeviceAuthorization(ctx context.Context) (*DeviceAuthorization, error)
	PollDeviceToken(ctx context.Context, deviceCode string) (*TokenSet, error)
}

type ProviderError struct {
	StatusCode int
	Code       string
	Message    string
	Permanent  bool
}

func (e *ProviderError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return http.StatusText(e.StatusCode)
}
