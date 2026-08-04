package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

type JWTClaims struct {
	Subject   string `json:"sub"`
	Email     string `json:"email"`
	ExpiresAt int64  `json:"exp"`
	OpenAI    struct {
		AccountID string `json:"chatgpt_account_id"`
		PlanType  string `json:"chatgpt_plan_type"`
	} `json:"https://api.openai.com/auth"`
}

func ParseJWTClaims(raw string) (JWTClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return JWTClaims{}, fmt.Errorf("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return JWTClaims{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims JWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return JWTClaims{}, fmt.Errorf("decode JWT claims: %w", err)
	}
	return claims, nil
}
