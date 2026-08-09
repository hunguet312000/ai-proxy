package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"literouter/internal/pool"
	"literouter/internal/storage"
)

// Cursor is reached through the same private endpoint its IDE uses. There is no
// OAuth flow to run: the session is imported from the desktop app's local state,
// exactly as 9router does it, because Cursor issues no refresh token and exposes no
// public authorization endpoint.
const (
	cursorBaseURL   = "https://api2.cursor.sh"
	cursorChatPath  = "/aiserver.v1.ChatService/StreamUnifiedChatWithTools"
	cursorUserAgent = "connect-es/1.6.1"

	// Defaults for the client identity. Cursor refuses builds it considers outdated
	// with "Your version of Cursor is no longer supported", so these go stale on their
	// schedule, not ours — hence the environment overrides below rather than a rebuild.
	defaultCursorClientVersion = "3.12.17"
	defaultCursorClientCommit  = "0fb762053c34788bb7760d5673f8a6d4c8589d50"
)

// cursorClientVersion and cursorClientCommit are read once at startup. Both must come
// from the same IDE build: they are validated as a pair.
var (
	cursorClientVersion = envOrDefault("LITEROUTER_CURSOR_CLIENT_VERSION", defaultCursorClientVersion)
	cursorClientCommit  = envOrDefault("LITEROUTER_CURSOR_CLIENT_COMMIT", defaultCursorClientCommit)
)

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// CursorCredentials is what gets stored for a Cursor account. Both values come out
// of the IDE's state.vscdb; neither can be derived from the other.
type CursorCredentials struct {
	AccessToken string `json:"access_token"`
	MachineID   string `json:"machine_id"`
	Email       string `json:"email,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	// ExpiresAt is read from the token's own exp claim. Cursor issues no refresh
	// token, so an expired session can only be replaced by importing a fresh one.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// ClientVersion and ClientCommit come from the installed IDE's product.json.
	ClientVersion string `json:"client_version,omitempty"`
	ClientCommit  string `json:"client_commit,omitempty"`
}

func (c CursorCredentials) encode() ([]byte, error) { return json.Marshal(c) }

func decodeCursorCredentials(raw []byte) (CursorCredentials, error) {
	var credentials CursorCredentials
	if err := json.Unmarshal(raw, &credentials); err != nil {
		return CursorCredentials{}, fmt.Errorf("decode cursor credentials: %w", err)
	}
	return credentials, nil
}

// cursorAuthToken strips the "user_xxx::" prefix the IDE stores in front of the JWT.
// Everything downstream — the bearer header, the client key, the session id — is
// derived from the bare token, so normalising once here keeps them consistent.
func cursorAuthToken(token string) string {
	token = strings.TrimSpace(token)
	if _, rest, found := strings.Cut(token, "::"); found {
		return rest
	}
	return token
}

// ParseCursorToken validates an imported session and pulls out what the UI shows.
// It deliberately does not contact Cursor: the only offline check available is the
// token's own shape and expiry.
func ParseCursorToken(accessToken, machineID string) (CursorCredentials, error) {
	token := cursorAuthToken(accessToken)
	if len(token) < 50 {
		return CursorCredentials{}, fmt.Errorf("access token looks too short to be a Cursor session")
	}
	machineID = strings.TrimSpace(machineID)
	if len(strings.ReplaceAll(machineID, "-", "")) < 32 {
		return CursorCredentials{}, fmt.Errorf("machine id must be the UUID from storage.serviceMachineId")
	}
	credentials := CursorCredentials{AccessToken: token, MachineID: machineID}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return CursorCredentials{}, fmt.Errorf("access token is not a JWT; copy the value of cursorAuth/accessToken")
	}
	payload, err := base64URLDecodeSegment(parts[1])
	if err != nil {
		return CursorCredentials{}, fmt.Errorf("access token payload is not readable: %w", err)
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Exp   int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return CursorCredentials{}, fmt.Errorf("access token payload is not JSON: %w", err)
	}
	credentials.UserID, credentials.Email = claims.Sub, claims.Email
	if claims.Exp > 0 {
		credentials.ExpiresAt = time.Unix(claims.Exp, 0).UTC()
		if time.Now().After(credentials.ExpiresAt) {
			// Reported rather than accepted: every request with this token would fail
			// with an opaque auth error, and there is no refresh path to recover.
			return credentials, fmt.Errorf("this Cursor session expired on %s; open Cursor and import a fresh token",
				credentials.ExpiresAt.Format("2006-01-02"))
		}
	}
	return credentials, nil
}

func base64URLDecodeSegment(segment string) ([]byte, error) {
	segment = strings.NewReplacer("-", "+", "_", "/").Replace(segment)
	for len(segment)%4 != 0 {
		segment += "="
	}
	return base64.StdEncoding.DecodeString(segment)
}

// cursorHeaders reproduces the header set the IDE sends. The endpoint validates the
// combination, so an omitted or reordered value is rejected as an unknown client.
func cursorHeaders(credentials CursorCredentials, ghostMode bool) (map[string]string, error) {
	token := cursorAuthToken(credentials.AccessToken)
	// The build detected at import wins: it is the one this token actually belongs to.
	// The package defaults are only a fallback for a session imported before build
	// detection existed, or pasted by hand.
	version, commit := credentials.ClientVersion, credentials.ClientCommit
	if strings.TrimSpace(version) == "" || strings.TrimSpace(commit) == "" {
		version, commit = cursorClientVersion, cursorClientCommit
	}
	machineID := strings.TrimSpace(credentials.MachineID)
	if machineID == "" {
		// The IDE always has one; this mirrors the fallback so a partial import still
		// produces a stable, self-consistent identity.
		machineID = sha256Hex(token + "machineId")
	}
	traceID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	configVersion, err := randomUUID()
	if err != nil {
		return nil, err
	}
	requestID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"authorization":               "Bearer " + token,
		"connect-accept-encoding":     "gzip",
		"connect-protocol-version":    "1",
		"content-type":                "application/connect+proto",
		"user-agent":                  cursorUserAgent,
		"x-amzn-trace-id":             "Root=" + traceID,
		"x-client-key":                sha256Hex(token),
		"x-cursor-checksum":           cursorChecksum(machineID, time.Now()),
		"x-cursor-client-version":     version,
		"x-cursor-client-commit":      commit,
		"x-cursor-client-type":        "ide",
		"x-cursor-client-os":          cursorOS(),
		"x-cursor-client-arch":        cursorArch(),
		"x-cursor-client-device-type": "desktop",
		"x-cursor-config-version":     configVersion,
		"x-cursor-timezone":           cursorTimezone(),
		"x-ghost-mode":                fmt.Sprintf("%t", ghostMode),
		"x-request-id":                requestID,
		"x-session-id":                uuidV5DNS(token),
	}, nil
}

// cursorChecksum builds the x-cursor-checksum value: six bytes of a coarse timestamp
// put through a rolling XOR, encoded with Cursor's own base64 alphabet, with the
// machine id appended. The timestamp is divided by 1e6, so the value only changes
// every ~17 minutes — it identifies the client, it is not a nonce.
func cursorChecksum(machineID string, now time.Time) string {
	stamp := now.UnixMilli() / 1e6
	bytes := []byte{
		byte(stamp >> 40), byte(stamp >> 32), byte(stamp >> 24),
		byte(stamp >> 16), byte(stamp >> 8), byte(stamp),
	}
	previous := byte(165)
	for index := range bytes {
		bytes[index] = (bytes[index] ^ previous) + byte(index%256)
		previous = bytes[index]
	}
	return cursorBase64(bytes) + machineID
}

// cursorBase64 is base64 over Cursor's alphabet with no padding. It is not the
// standard URL alphabet: the last two characters differ in order from RFC 4648, so
// encoding/base64 cannot be substituted without changing the output.
func cursorBase64(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out strings.Builder
	for index := 0; index < len(data); index += 3 {
		first := data[index]
		var second, third byte
		if index+1 < len(data) {
			second = data[index+1]
		}
		if index+2 < len(data) {
			third = data[index+2]
		}
		out.WriteByte(alphabet[first>>2])
		out.WriteByte(alphabet[(first&3)<<4|second>>4])
		if index+1 < len(data) {
			out.WriteByte(alphabet[(second&15)<<2|third>>6])
		}
		if index+2 < len(data) {
			out.WriteByte(alphabet[third&63])
		}
	}
	return out.String()
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func cursorOS() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

func cursorArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x64"
}

func cursorTimezone() string {
	if zone, _ := time.Now().Zone(); zone != "" {
		if location := time.Local.String(); location != "" && location != "Local" {
			return location
		}
	}
	return "UTC"
}

// randomUUID returns a v4 UUID. The project keeps its dependency list to three
// direct modules, so this is implemented here rather than pulling in a UUID package
// for a handful of header values.
func randomUUID() (string, error) {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", err
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return formatUUID(buffer), nil
}

// uuidV5DNS derives the stable session id Cursor expects: UUIDv5 of the token under
// the DNS namespace. It must be deterministic — the same session has to present the
// same id across requests.
func uuidV5DNS(name string) string {
	namespace := [16]byte{
		0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1,
		0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
	}
	hash := sha1.New()
	hash.Write(namespace[:])
	hash.Write([]byte(name))
	sum := hash.Sum(nil)
	var buffer [16]byte
	copy(buffer[:], sum[:16])
	buffer[6] = (buffer[6] & 0x0f) | 0x50
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	return formatUUID(buffer)
}

func formatUUID(buffer [16]byte) string {
	encoded := hex.EncodeToString(buffer[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// ImportCursorAccount stores a session copied out of the Cursor IDE. There is no
// authorization flow to run and no refresh token to store: the pool holds the
// imported session until it expires, after which it has to be imported again.
func (m *Manager) ImportCursorAccount(ctx context.Context, accessToken, machineID string) (pool.Account, error) {
	parsed, err := ParseCursorToken(accessToken, machineID)
	if err != nil {
		return pool.Account{}, err
	}
	// Capture the installed build alongside the session. A missing install is not
	// fatal here — the pasted-token path has no IDE to read — so the defaults stand in.
	version, commit := parsed.ClientVersion, parsed.ClientCommit
	if version == "" || commit == "" {
		if build, buildErr := DetectCursorBuild(); buildErr == nil {
			version, commit = build.Version, build.Commit
			m.logger.Info("Cursor build detected", "version", version, "commit", commit, "path", build.Path)
		} else {
			m.logger.Warn("Cursor build not detected; falling back to the configured client identity",
				"version", cursorClientVersion, "error", buildErr)
		}
	}
	credentials, err := json.Marshal(TokenSet{
		AccessToken:   parsed.AccessToken,
		MachineID:     parsed.MachineID,
		ExpiresAt:     parsed.ExpiresAt,
		TokenType:     "Bearer",
		ClientVersion: version,
		ClientCommit:  commit,
	})
	if err != nil {
		return pool.Account{}, fmt.Errorf("encode cursor credentials: %w", err)
	}
	identity := parsed.UserID
	if identity == "" {
		// Fall back to the machine id so two imports from one machine replace each
		// other instead of accumulating duplicate accounts.
		identity = parsed.MachineID
	}
	accountID := "cursor:" + identity
	label := parsed.Email
	if label == "" {
		label = accountID
	}
	weight := 1
	if existing, getErr := m.store.GetAccount(ctx, accountID); getErr == nil {
		weight = existing.Weight
	} else if !errors.Is(getErr, sql.ErrNoRows) {
		return pool.Account{}, getErr
	}
	if err := m.store.UpsertAccount(ctx, storage.Account{
		ID: accountID, Provider: "cursor", Label: label, Credentials: credentials,
		Enabled: true, Weight: weight,
	}); err != nil {
		return pool.Account{}, err
	}
	if err := m.store.UpdateAccountRouting(ctx, accountID, true, weight); err != nil {
		return pool.Account{}, err
	}
	account := pool.Account{ID: accountID, Provider: "cursor", Label: label, Enabled: true, Weight: weight}
	m.pool.Upsert(account)
	m.logger.Info("Cursor session imported", "account_id", accountID, "expires_at", parsed.ExpiresAt)
	return account, nil
}
