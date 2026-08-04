package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestGeneratePKCE(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE() error = %v", err)
	}
	if len(pkce.Verifier) < 43 {
		t.Fatalf("verifier length = %d", len(pkce.Verifier))
	}
	digest := sha256.Sum256([]byte(pkce.Verifier))
	want := base64.RawURLEncoding.EncodeToString(digest[:])
	if pkce.Challenge != want {
		t.Fatalf("Challenge = %q, want %q", pkce.Challenge, want)
	}
}

func TestStateSigner(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	signer, err := NewStateSigner(make([]byte, 32))
	if err != nil {
		t.Fatalf("NewStateSigner() error = %v", err)
	}
	signer.now = func() time.Time { return now }

	state, err := signer.Sign("codex", time.Minute)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if err := signer.Verify(state, "codex"); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if err := signer.Verify(state, "other"); err == nil {
		t.Fatal("Verify() accepted the wrong provider")
	}

	parts := strings.Split(state, ".")
	parts[0] = "A" + parts[0][1:]
	if err := signer.Verify(strings.Join(parts, "."), "codex"); err == nil {
		t.Fatal("Verify() accepted tampered state")
	}

	signer.now = func() time.Time { return now.Add(time.Minute) }
	if err := signer.Verify(state, "codex"); err == nil {
		t.Fatal("Verify() accepted expired state")
	}
}
