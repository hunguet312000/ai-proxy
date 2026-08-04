package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const stateNonceSize = 32

type PKCE struct {
	Verifier  string
	Challenge string
}

type StateSigner struct {
	key []byte
	now func() time.Time
}

type statePayload struct {
	Provider  string `json:"provider"`
	ExpiresAt int64  `json:"expires_at"`
	Nonce     string `json:"nonce"`
}

func GeneratePKCE() (PKCE, error) {
	return GeneratePKCEBytes(64)
}

func GeneratePKCEBytes(size int) (PKCE, error) {
	if size < 32 || size > 96 {
		return PKCE{}, fmt.Errorf("PKCE entropy must be between 32 and 96 bytes")
	}
	verifierBytes := make([]byte, size)
	if _, err := rand.Read(verifierBytes); err != nil {
		return PKCE{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	digest := sha256.Sum256([]byte(verifier))
	return PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(digest[:]),
	}, nil
}

func NewStateSigner(key []byte) (*StateSigner, error) {
	if len(key) < sha256.Size {
		return nil, fmt.Errorf("state signing key must be at least %d bytes", sha256.Size)
	}
	return &StateSigner{key: append([]byte(nil), key...), now: time.Now}, nil
}

func (s *StateSigner) Sign(provider string, ttl time.Duration) (string, error) {
	if provider == "" {
		return "", fmt.Errorf("OAuth provider is required")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("state TTL must be greater than zero")
	}
	nonce := make([]byte, stateNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate OAuth state nonce: %w", err)
	}
	payload, err := json.Marshal(statePayload{
		Provider:  provider,
		ExpiresAt: s.now().Add(ttl).Unix(),
		Nonce:     base64.RawURLEncoding.EncodeToString(nonce),
	})
	if err != nil {
		return "", fmt.Errorf("encode OAuth state: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	return encodedPayload + "." + s.signature(encodedPayload), nil
}

func (s *StateSigner) Verify(state, provider string) error {
	parts := strings.Split(state, ".")
	if len(parts) != 2 {
		return fmt.Errorf("invalid OAuth state format")
	}
	wantSignature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("invalid OAuth state signature: %w", err)
	}
	gotSignature, _ := base64.RawURLEncoding.DecodeString(s.signature(parts[0]))
	if !hmac.Equal(gotSignature, wantSignature) {
		return fmt.Errorf("invalid OAuth state signature")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("decode OAuth state: %w", err)
	}
	var payload statePayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return fmt.Errorf("decode OAuth state payload: %w", err)
	}
	if payload.Provider != provider {
		return fmt.Errorf("OAuth state provider mismatch")
	}
	if payload.Nonce == "" {
		return fmt.Errorf("OAuth state nonce is missing")
	}
	if s.now().Unix() >= payload.ExpiresAt {
		return fmt.Errorf("OAuth state expired at %s", strconv.FormatInt(payload.ExpiresAt, 10))
	}
	return nil
}

func (s *StateSigner) signature(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
