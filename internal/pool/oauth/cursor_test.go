package oauth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"literouter/internal/provider"
)

// The wire format is reverse-engineered and cannot be checked against a schema, so
// these tests pin the exact bytes. A change in output is either an intentional
// protocol update or a regression; either way it must be visible here.

func TestPutUvarintGoldenBytes(t *testing.T) {
	for _, test := range []struct {
		value uint64
		want  []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
		{16384, []byte{0x80, 0x80, 0x01}},
	} {
		if got := putUvarint(test.value); !bytes.Equal(got, test.want) {
			t.Fatalf("putUvarint(%d) = %x, want %x", test.value, got, test.want)
		}
	}
}

func TestProtoFieldGoldenBytes(t *testing.T) {
	// tag = field<<3 | wireType
	if got := protoField(1, protoVarint, 1); !bytes.Equal(got, []byte{0x08, 0x01}) {
		t.Fatalf("field 1 varint = %x", got)
	}
	if got := protoField(2, protoBytes, "hi"); !bytes.Equal(got, []byte{0x12, 0x02, 'h', 'i'}) {
		t.Fatalf("field 2 bytes = %x", got)
	}
	// Field 13 crosses the single-byte tag boundary, which is where a hand-written
	// encoder is most likely to be wrong.
	if got := protoField(13, protoBytes, "x"); !bytes.Equal(got, []byte{0x6a, 0x01, 'x'}) {
		t.Fatalf("field 13 bytes = %x", got)
	}
	if got := protoField(16, protoVarint, 0); !bytes.Equal(got, []byte{0x80, 0x01, 0x00}) {
		t.Fatalf("field 16 varint = %x", got)
	}
	if protoField(1, 5, "unsupported") != nil {
		t.Fatal("an unsupported wire type must not silently encode")
	}
}

func TestConnectFrameGoldenBytes(t *testing.T) {
	frame := connectFrame([]byte{0xde, 0xad}, false)
	want := []byte{0x00, 0x00, 0x00, 0x00, 0x02, 0xde, 0xad}
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame = %x, want %x", frame, want)
	}
	if compressed := connectFrame([]byte{0x01}, true); compressed[0] != 0x01 {
		t.Fatalf("compression flag not set: %x", compressed)
	}
}

func TestCursorBase64UsesItsOwnAlphabet(t *testing.T) {
	// Bytes chosen so the last two alphabet positions (62, 63) are exercised: standard
	// base64 would emit "+/" there and the checksum would be rejected.
	data := []byte{0xfb, 0xf0, 0xff}
	got := cursorBase64(data)
	if strings.ContainsAny(got, "+/=") {
		t.Fatalf("cursorBase64(%x) = %q, which is not Cursor's alphabet", data, got)
	}
	if got != "-_D_" {
		t.Fatalf("cursorBase64(%x) = %q, want %q", data, got, "-_D_")
	}
	// Trailing partial groups must not be padded.
	if out := cursorBase64([]byte{0x00}); out != "AA" {
		t.Fatalf("one byte encoded as %q, want %q", out, "AA")
	}
}

func TestCursorChecksumIsStableWithinItsWindow(t *testing.T) {
	machineID := "11111111-2222-3333-4444-555555555555"
	base := time.Unix(1750000000, 0)
	first := cursorChecksum(machineID, base)
	// The timestamp is divided by 1e6 milliseconds, so a minute apart is the same
	// window: the value identifies the client rather than acting as a nonce.
	if second := cursorChecksum(machineID, base.Add(time.Minute)); first != second {
		t.Fatalf("checksum changed within one window: %q vs %q", first, second)
	}
	if !strings.HasSuffix(first, machineID) {
		t.Fatalf("checksum does not end with the machine id: %q", first)
	}
	// A different machine must produce a different value.
	if other := cursorChecksum("99999999-2222-3333-4444-555555555555", base); other == first {
		t.Fatal("checksum ignores the machine id")
	}
	// And a far later window must move it.
	if later := cursorChecksum(machineID, base.Add(48*time.Hour)); later == first {
		t.Fatal("checksum never changes across windows")
	}
}

func TestUUIDv5DNSMatchesTheKnownVector(t *testing.T) {
	// RFC 4122 DNS namespace with name "example.com" — a published vector, so this
	// catches a namespace or version-bit mistake.
	if got := uuidV5DNS("example.com"); got != "cfbff0d1-9375-5685-968c-48ce8b15ae17" {
		t.Fatalf("uuidV5DNS = %q, want the published vector", got)
	}
	// It must be deterministic: Cursor expects one session id per token.
	if uuidV5DNS("token") != uuidV5DNS("token") {
		t.Fatal("session id is not stable for the same token")
	}
}

func TestCursorHeadersCarryTheFullClientIdentity(t *testing.T) {
	headers, err := cursorHeaders(CursorCredentials{
		AccessToken: "user_abc::" + strings.Repeat("t", 60),
		MachineID:   "11111111-2222-3333-4444-555555555555",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	// The stored token carries a "user_x::" prefix that must not be sent.
	if strings.Contains(headers["authorization"], "user_abc::") {
		t.Fatalf("authorization still carries the storage prefix: %q", headers["authorization"])
	}
	for _, key := range []string{
		"x-cursor-checksum", "x-client-key", "x-session-id", "x-cursor-client-version",
		"x-cursor-client-commit", "x-cursor-client-type", "x-ghost-mode", "connect-protocol-version",
	} {
		if headers[key] == "" {
			t.Fatalf("header %q is missing; the endpoint validates the set", key)
		}
	}
	if headers["content-type"] != "application/connect+proto" {
		t.Fatalf("content type = %q", headers["content-type"])
	}
}

func TestParseCursorTokenRejectsAndReports(t *testing.T) {
	machineID := "11111111-2222-3333-4444-555555555555"
	if _, err := ParseCursorToken("short", machineID); err == nil {
		t.Fatal("a too-short token was accepted")
	}
	if _, err := ParseCursorToken(strings.Repeat("t", 60), "nope"); err == nil {
		t.Fatal("an invalid machine id was accepted")
	}
	// An expired session is reported rather than stored: there is no refresh path, so
	// every later request would fail with an opaque auth error.
	expired := makeCursorJWT(t, time.Now().Add(-time.Hour), "u1", "a@b.c")
	credentials, err := ParseCursorToken(expired, machineID)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token error = %v", err)
	}
	if credentials.Email != "a@b.c" {
		t.Fatalf("claims not extracted from an expired token: %#v", credentials)
	}
	valid := makeCursorJWT(t, time.Now().Add(time.Hour), "u2", "ok@b.c")
	parsed, err := ParseCursorToken(valid, machineID)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.UserID != "u2" || parsed.Email != "ok@b.c" {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func makeCursorJWT(t *testing.T, expiry time.Time, sub, email string) string {
	t.Helper()
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := encode(map[string]string{"alg": "none", "typ": "JWT"})
	payload := encode(map[string]any{"sub": sub, "email": email, "exp": expiry.Unix()})
	// Padded so the token clears the length check without depending on the claims.
	return header + "." + payload + "." + strings.Repeat("s", 40)
}

func TestResolveCursorModelStripsTheRoutingPrefix(t *testing.T) {
	for input, want := range map[string]string{
		"cursor/claude-4.5-sonnet": "claude-4.5-sonnet",
		"cu/default":               "default",
		"claude-4.5-opus-high":     "claude-4.5-opus-high",
		"":                         "default",
	} {
		if got := resolveCursorModel(input); got != want {
			t.Fatalf("resolveCursorModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestOAuthProviderForModelRoutesCursorBeforeClaude(t *testing.T) {
	// Cursor serves Claude models under its own subscription, so the prefix has to win
	// over the "claude" match or every Cursor request lands on the wrong pool.
	for model, want := range map[string]string{
		"cursor/claude-4.5-sonnet": "cursor",
		"cu/default":               "cursor",
		"claude-opus-4-5":          "claude",
		"cx/gpt-5.6-sol":           "codex",
		"gemini-3.1-pro-high":      "antigravity",
		"xai/grok-4.5":             "grok",
	} {
		if got := oauthProviderForModel(model); got != want {
			t.Fatalf("oauthProviderForModel(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestCursorClientIdentityIsOverridable(t *testing.T) {
	// Cursor rejects builds it considers outdated, and the version it accepts changes
	// on its schedule. Recovering from that must not require rebuilding LiteRouter.
	if envOrDefault("LITEROUTER_CURSOR_CLIENT_VERSION_UNSET_FOR_TEST", "fallback") != "fallback" {
		t.Fatal("an unset variable must fall back")
	}
	t.Setenv("PROBE_CURSOR_VERSION", "9.9.9")
	if got := envOrDefault("PROBE_CURSOR_VERSION", "fallback"); got != "9.9.9" {
		t.Fatalf("override = %q", got)
	}
	// The default pair still has to be present, or every request goes out unsigned.
	if cursorClientVersion == "" || cursorClientCommit == "" {
		t.Fatal("client version and commit must both be set")
	}
}

func TestCursorEndStreamErrorSurfacesTheActionableDetail(t *testing.T) {
	// Cursor puts the literal string "Error" in the top-level message and the useful
	// text in details[].debug — reporting only the code turns an answer the user can
	// act on into a generic upstream failure.
	payload := []byte(`{"error":{"code":"resource_exhausted","message":"Error","details":[{"debug":{"error":"ERROR_RATE_LIMITED_CHANGEABLE","details":{"title":"Named models unavailable","detail":"Free plans can only use Auto."}}}]}}`)
	err := cursorEndStreamError(payload)
	if err == nil {
		t.Fatal("no error returned")
	}
	var providerErr *provider.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("error type = %T", err)
	}
	if !strings.Contains(providerErr.Message, "Free plans can only use Auto") {
		t.Fatalf("message = %q", providerErr.Message)
	}
	if providerErr.Code != "ERROR_RATE_LIMITED_CHANGEABLE" {
		t.Fatalf("code = %q", providerErr.Code)
	}
	// A plan or version refusal is not fixed by another account, so it must not be
	// classified as a retryable rate limit.
	if providerErr.StatusCode != 400 {
		t.Fatalf("status = %d, want 400", providerErr.StatusCode)
	}
	if cursorEndStreamError([]byte("{}")) != nil {
		t.Fatal("an empty end frame is not an error")
	}
}

func TestCursorStatePathsCoverContainerAndHost(t *testing.T) {
	// The server normally runs in a container with the host home bind-mounted, so a
	// detector that only looks at its own home would never find anything.
	t.Setenv("LITEROUTER_CURSOR_STATE_DB", "")
	paths := cursorStatePaths()
	var sawMac, sawLinux, sawWindows bool
	for _, path := range paths {
		switch {
		case strings.Contains(path, "Library/Application Support/Cursor"):
			sawMac = true
		case strings.Contains(path, ".config/Cursor"):
			sawLinux = true
		case strings.Contains(path, "AppData/Roaming/Cursor") || strings.Contains(path, `AppData\Roaming\Cursor`):
			sawWindows = true
		}
	}
	if !sawMac || !sawLinux || !sawWindows {
		t.Fatalf("layouts covered: mac=%v linux=%v windows=%v (%v)", sawMac, sawLinux, sawWindows, paths)
	}
	// An explicit override has to win, since a non-standard install cannot be guessed.
	t.Setenv("LITEROUTER_CURSOR_STATE_DB", "/somewhere/else/state.vscdb")
	if first := cursorStatePaths()[0]; first != "/somewhere/else/state.vscdb" {
		t.Fatalf("override is not consulted first: %q", first)
	}
}

func TestDetectCursorSessionReportsWhenNothingIsInstalled(t *testing.T) {
	t.Setenv("LITEROUTER_CURSOR_STATE_DB", filepath.Join(t.TempDir(), "missing.vscdb"))
	t.Setenv("HOME", t.TempDir())
	_, _, err := DetectCursorSession(context.Background())
	if !errors.Is(err, ErrCursorSessionNotFound) {
		t.Fatalf("err = %v, want ErrCursorSessionNotFound", err)
	}
}

func TestReadCursorStateParsesAnIDEDatabase(t *testing.T) {
	// A stand-in for state.vscdb with the same table and keys, so the reader is
	// exercised end to end without touching a real install.
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE itemTable (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	token := makeCursorJWT(t, time.Now().Add(24*time.Hour), "user-1", "dev@example.com")
	machine := "11111111-2222-3333-4444-555555555555"
	if _, err := db.Exec(`INSERT INTO itemTable VALUES (?,?),(?,?)`,
		cursorTokenKey, token, cursorMachineKey, machine); err != nil {
		t.Fatal(err)
	}
	db.Close()

	credentials, err := readCursorStateFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.MachineID != machine || credentials.Email != "dev@example.com" {
		t.Fatalf("credentials = %#v", credentials)
	}

	// A database with no session must say what to do rather than fail obscurely.
	empty := filepath.Join(t.TempDir(), "state.vscdb")
	blank, err := sql.Open("sqlite", "file:"+empty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blank.Exec(`CREATE TABLE itemTable (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	blank.Close()
	if _, err := readCursorStateFile(context.Background(), empty); err == nil ||
		!strings.Contains(err.Error(), "log in") {
		t.Fatalf("empty database error = %v", err)
	}
}

func TestReadCursorStateLeavesTheOriginalUntouched(t *testing.T) {
	// The IDE owns this file. Opening it in place can checkpoint a pending WAL, which
	// rewrites the main database of an application LiteRouter has no business writing.
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE itemTable (key TEXT PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	token := makeCursorJWT(t, time.Now().Add(24*time.Hour), "u", "e@x.y")
	if _, err := db.Exec(`INSERT INTO itemTable VALUES (?,?),(?,?)`,
		cursorTokenKey, token, cursorMachineKey, "11111111-2222-3333-4444-555555555555"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readCursorState(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Fatalf("the IDE database was modified: %v/%d -> %v/%d",
			before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
}

func TestDetectCursorBuildReadsProductJSON(t *testing.T) {
	// Version and commit are validated as a pair, so both must be read from the same
	// product.json rather than assembled from different sources.
	dir := t.TempDir()
	path := filepath.Join(dir, "product.json")
	if err := os.WriteFile(path, []byte(`{"version":"3.15.6","commit":"a1f686545fd0ce8917bbd2449f733551a9bce420","nameShort":"Cursor"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LITEROUTER_CURSOR_PRODUCT_JSON", path)
	build, err := DetectCursorBuild()
	if err != nil {
		t.Fatal(err)
	}
	if build.Version != "3.15.6" || build.Commit != "a1f686545fd0ce8917bbd2449f733551a9bce420" {
		t.Fatalf("build = %#v", build)
	}

	// A product.json missing either half must be skipped rather than half-used:
	// sending a version without its commit fails the pair check with no useful
	// message. Detection may still succeed from a real install on this machine, so the
	// invariant under test is that a half pair is never returned.
	partial := filepath.Join(dir, "partial.json")
	if err := os.WriteFile(partial, []byte(`{"version":"9.9.9"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LITEROUTER_CURSOR_PRODUCT_JSON", partial)
	skipped, err := DetectCursorBuild()
	if err == nil {
		if skipped.Version == "9.9.9" || skipped.Commit == "" {
			t.Fatalf("a half-filled product.json was used: %#v", skipped)
		}
	}
}

func TestCursorHeadersPreferTheSessionsOwnBuild(t *testing.T) {
	token := strings.Repeat("t", 60)
	machine := "11111111-2222-3333-4444-555555555555"
	// A session that carries its build must send it, not the package default, or the
	// token and the client identity could come from different installs.
	headers, err := cursorHeaders(CursorCredentials{
		AccessToken: token, MachineID: machine,
		ClientVersion: "3.15.6", ClientCommit: "a1f686545fd0ce8917bbd2449f733551a9bce420",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if headers["x-cursor-client-version"] != "3.15.6" {
		t.Fatalf("version = %q", headers["x-cursor-client-version"])
	}
	if headers["x-cursor-client-commit"] != "a1f686545fd0ce8917bbd2449f733551a9bce420" {
		t.Fatalf("commit = %q", headers["x-cursor-client-commit"])
	}
	// A session imported before build detection existed still has to work.
	fallback, err := cursorHeaders(CursorCredentials{AccessToken: token, MachineID: machine}, true)
	if err != nil {
		t.Fatal(err)
	}
	if fallback["x-cursor-client-version"] != cursorClientVersion {
		t.Fatalf("fallback version = %q", fallback["x-cursor-client-version"])
	}
	// A half-filled pair must fall back rather than mix two builds.
	mixed, err := cursorHeaders(CursorCredentials{
		AccessToken: token, MachineID: machine, ClientVersion: "9.9.9",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if mixed["x-cursor-client-version"] != cursorClientVersion {
		t.Fatalf("a version with no commit was sent: %q", mixed["x-cursor-client-version"])
	}
}
