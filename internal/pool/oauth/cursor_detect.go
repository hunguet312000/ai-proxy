package oauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"literouter/internal/pool"
)

// ErrCursorSessionNotFound means no Cursor install with a stored session was located.
var ErrCursorSessionNotFound = errors.New("no Cursor session found on this machine")

// cursorStateKeys are the two rows the IDE keeps its session in.
const (
	cursorTokenKey   = "cursorAuth/accessToken"
	cursorMachineKey = "storage.serviceMachineId"
)

// cursorStatePaths lists where the IDE keeps state.vscdb, newest layout first.
//
// LiteRouter usually runs in a container, so the host home is consulted through the
// bind mount as well: without that, auto-detect would only ever work when the binary
// runs directly on the desktop.
func cursorStatePaths() []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, home)
	}
	// Set by the container image so CLI apply/reset can reach the real home.
	if hostHome := strings.TrimSpace(os.Getenv("HOME")); hostHome != "" && !contains(roots, hostHome) {
		roots = append(roots, hostHome)
	}
	if _, err := os.Stat("/host-home"); err == nil && !contains(roots, "/host-home") {
		roots = append(roots, "/host-home")
	}

	suffixes := [][]string{
		{"Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb"},
		{".config", "Cursor", "User", "globalStorage", "state.vscdb"},
		{"AppData", "Roaming", "Cursor", "User", "globalStorage", "state.vscdb"},
	}
	if runtime.GOOS == "darwin" {
		// Keep the platform's own layout first so a Linux container reading a macOS
		// home still finds it, but a native run does not stat two wrong paths first.
		suffixes = suffixes[:1]
		suffixes = append(suffixes,
			[]string{".config", "Cursor", "User", "globalStorage", "state.vscdb"},
			[]string{"AppData", "Roaming", "Cursor", "User", "globalStorage", "state.vscdb"})
	}

	var paths []string
	for _, root := range roots {
		for _, suffix := range suffixes {
			paths = append(paths, filepath.Join(append([]string{root}, suffix...)...))
		}
	}
	if explicit := strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_STATE_DB")); explicit != "" {
		// An explicit override wins: a non-standard install cannot be guessed.
		paths = append([]string{explicit}, paths...)
	}
	return paths
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// DetectCursorSession reads the session straight out of the IDE's local state, which
// is what makes the import a single click instead of two sqlite queries pasted by
// hand. The database is opened read-only: it belongs to a running application and
// must not be locked or written.
func DetectCursorSession(ctx context.Context) (CursorCredentials, string, error) {
	var lastErr error
	for _, path := range cursorStatePaths() {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		credentials, err := readCursorState(ctx, path)
		if err != nil {
			lastErr = err
			continue
		}
		return credentials, path, nil
	}
	if lastErr != nil {
		return CursorCredentials{}, "", lastErr
	}
	return CursorCredentials{}, "", ErrCursorSessionNotFound
}

// readCursorState reads the session out of a copy of the IDE's database.
//
// Opening the original in place is not safe enough even read-only: SQLite may
// checkpoint a pending WAL on open, which rewrites the main file of an application
// LiteRouter does not own. Copying first makes the read provably side-effect free,
// and the WAL is copied too so a session written moments ago is still visible.
func readCursorState(ctx context.Context, path string) (CursorCredentials, error) {
	scratch, err := os.MkdirTemp("", "literouter-cursor-*")
	if err != nil {
		return CursorCredentials{}, fmt.Errorf("prepare Cursor state copy: %w", err)
	}
	defer os.RemoveAll(scratch)
	copied := filepath.Join(scratch, "state.vscdb")
	if err := copyFile(path, copied); err != nil {
		return CursorCredentials{}, fmt.Errorf("copy Cursor state: %w", err)
	}
	// Sidecars are best effort: a database with no pending WAL simply has none.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = copyFile(path+suffix, copied+suffix)
	}
	return readCursorStateFile(ctx, copied)
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func readCursorStateFile(ctx context.Context, path string) (CursorCredentials, error) {
	// The copy is still opened read-only, so a bug here cannot corrupt it either.
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(3000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return CursorCredentials{}, fmt.Errorf("open Cursor state: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	read := func(key string) (string, error) {
		var value string
		err := db.QueryRowContext(ctx, `SELECT value FROM itemTable WHERE key = ?`, key).Scan(&value)
		return strings.TrimSpace(value), err
	}
	token, err := read(cursorTokenKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CursorCredentials{}, fmt.Errorf("%s holds no signed-in session; open Cursor and log in first", filepath.Base(path))
		}
		return CursorCredentials{}, fmt.Errorf("read Cursor session: %w", err)
	}
	machineID, err := read(cursorMachineKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return CursorCredentials{}, fmt.Errorf("read Cursor machine id: %w", err)
	}
	return ParseCursorToken(token, machineID)
}

// DetectAndImportCursorAccount finds the local session and the installed build, then
// stores them together. It reports what it found so the result can name both.
func (m *Manager) DetectAndImportCursorAccount(ctx context.Context) (pool.Account, string, error) {
	credentials, path, err := DetectCursorSession(ctx)
	if err != nil {
		return pool.Account{}, "", err
	}
	summary := path
	if build, buildErr := DetectCursorBuild(); buildErr == nil {
		summary = fmt.Sprintf("%s (Cursor %s)", path, build.Version)
	} else {
		// Worth saying plainly: without a build, requests go out claiming whatever
		// version is configured, and a stale one is refused outright.
		summary = fmt.Sprintf("%s — no Cursor install found, using the configured client version %s",
			path, cursorClientVersion)
	}
	account, err := m.ImportCursorAccount(ctx, credentials.AccessToken, credentials.MachineID)
	return account, summary, err
}

// CursorBuild is the IDE build a session belongs to. Cursor validates the version and
// commit as a pair and refuses anything it considers outdated, so both have to come
// from the same install as the token.
type CursorBuild struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Path    string `json:"-"`
}

// cursorAppPaths lists where the IDE keeps product.json, which is the only place the
// build is recorded. As with the session, the host filesystem is consulted through
// the container's bind mount as well.
func cursorAppPaths() []string {
	suffixes := []string{
		"/Applications/Cursor.app/Contents/Resources/app/product.json",
		"/usr/share/cursor/resources/app/product.json",
		"/opt/Cursor/resources/app/product.json",
	}
	var paths []string
	if explicit := strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_PRODUCT_JSON")); explicit != "" {
		paths = append(paths, explicit)
	}
	paths = append(paths, suffixes...)
	// /host-apps is where the compose file mounts the host's /Applications read-only;
	// without it a containerised LiteRouter can read the session but not the build.
	paths = append(paths, "/host-apps/Cursor.app/Contents/Resources/app/product.json")
	for _, root := range []string{"/host-root", "/host-home"} {
		for _, suffix := range suffixes {
			paths = append(paths, filepath.Join(root, suffix))
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths,
			filepath.Join(home, "Applications/Cursor.app/Contents/Resources/app/product.json"),
			filepath.Join(home, "AppData/Local/Programs/cursor/resources/app/product.json"))
	}
	return paths
}

// DetectCursorBuild reads the installed IDE's version and commit. Without it the
// operator has to copy both out of product.json by hand, and a stale pair is the one
// failure that makes every request fail with "Your version of Cursor is no longer
// supported".
func DetectCursorBuild() (CursorBuild, error) {
	for _, path := range cursorAppPaths() {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var product struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
		}
		if err := json.Unmarshal(raw, &product); err != nil {
			continue
		}
		if strings.TrimSpace(product.Version) == "" || strings.TrimSpace(product.Commit) == "" {
			continue
		}
		return CursorBuild{Version: product.Version, Commit: product.Commit, Path: path}, nil
	}
	return CursorBuild{}, errors.New("no Cursor installation found; set LITEROUTER_CURSOR_CLIENT_VERSION and _COMMIT by hand")
}
