package oauth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"literouter/internal/pool"
	"literouter/internal/storage"
)

const flowTTL = 3 * time.Minute

type Manager struct {
	store  *storage.Store
	pool   *pool.Pool
	signer *StateSigner
	logger *slog.Logger

	mu                 sync.Mutex
	servers            map[string]*http.Server
	listeners          map[*http.Server]net.Listener
	onAccountConnected func(context.Context, string)
}

func (m *Manager) SetOnAccountConnected(fn func(context.Context, string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onAccountConnected = fn
}

type StartResult struct {
	Provider                string `json:"provider"`
	Flow                    string `json:"flow"`
	AuthURL                 string `json:"auth_url,omitempty"`
	UserCode                string `json:"user_code,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresAt               string `json:"expires_at"`
}

func NewManager(store *storage.Store, accountPool *pool.Pool, masterKey []byte, logger *slog.Logger) (*Manager, error) {
	signingKey := sha256.Sum256(append(append([]byte(nil), masterKey...), []byte("literouter-oauth-state-v1")...))
	signer, err := NewStateSigner(signingKey[:])
	if err != nil {
		return nil, err
	}
	return &Manager{
		store: store, pool: accountPool, signer: signer, logger: logger,
		servers: make(map[string]*http.Server), listeners: make(map[*http.Server]net.Listener),
	}, nil
}

func (m *Manager) StartCodex(ctx context.Context) (StartResult, error) {
	provider := NewCodexProvider(nil)
	return m.startBrowser(ctx, provider, []int{provider.PreferredPort(), provider.FallbackPort()})
}

func (m *Manager) StartClaude(ctx context.Context) (StartResult, error) {
	provider := NewClaudeProvider(nil)
	return m.startBrowser(ctx, provider, []int{provider.PreferredPort()})
}

func (m *Manager) StartGrok(ctx context.Context) (StartResult, error) {
	provider := NewGrokProvider(nil)
	return m.startBrowser(ctx, provider, []int{provider.PreferredPort()})
}

func (m *Manager) StartAntigravity(ctx context.Context) (StartResult, error) {
	provider := NewAntigravityProvider(nil)
	return m.startBrowser(ctx, provider, []int{provider.PreferredPort()})
}

func (m *Manager) startBrowser(ctx context.Context, provider OAuthProvider, ports []int) (StartResult, error) {
	port, server, err := m.listen(provider.Name(), ports, provider.CallbackPath())
	if err != nil {
		return StartResult{}, err
	}
	redirectURL := fmt.Sprintf("http://%s:%d%s", callbackPublicHost(provider.Name()), port, provider.CallbackPath())
	provider = provider.WithRedirectURL(redirectURL)

	pkce, err := GeneratePKCE()
	if provider.Name() == "grok" {
		pkce, err = GeneratePKCEBytes(96)
	}
	if err != nil {
		m.stopServer(provider.Name(), server)
		return StartResult{}, err
	}
	state, err := m.signer.Sign(provider.Name(), flowTTL)
	if err != nil {
		m.stopServer(provider.Name(), server)
		return StartResult{}, err
	}
	expiresAt := time.Now().Add(flowTTL).UTC()
	if err := m.store.PutOAuthSession(ctx, storage.OAuthSession{
		State: state, Provider: provider.Name(), Verifier: pkce.Verifier, ExpiresAt: expiresAt,
	}); err != nil {
		m.stopServer(provider.Name(), server)
		return StartResult{}, err
	}

	m.mu.Lock()
	m.servers[provider.Name()] = server
	m.mu.Unlock()
	go m.serveCallback(provider, server)
	go m.expireFlow(provider.Name(), server, expiresAt)

	return StartResult{
		Provider: provider.Name(), Flow: "authorization_code_pkce",
		AuthURL: provider.AuthURL(state, pkce.Challenge), ExpiresAt: expiresAt.Format(time.RFC3339),
	}, nil
}

func (m *Manager) pollDevice(provider DeviceOAuthProvider, device *DeviceAuthorization, expiresAt time.Time) {
	interval := device.Interval
	for time.Now().Before(expiresAt) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		token, err := provider.PollDeviceToken(ctx, device.DeviceCode)
		cancel()
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, saveErr := m.saveAccount(ctx, provider, token)
			cancel()
			if saveErr != nil {
				m.logger.Error("save device OAuth account", "provider", provider.Name(), "error", saveErr)
			}
			return
		}
		if errors.Is(err, ErrSlowDown) {
			interval += 5 * time.Second
		} else if !errors.Is(err, ErrAuthorizationPending) {
			m.logger.Warn("device OAuth polling failed", "provider", provider.Name(), "error", err)
			return
		}
		time.Sleep(interval)
	}
	m.logger.Warn("device OAuth expired", "provider", provider.Name())
}

func (m *Manager) ImportCodex(ctx context.Context, token TokenSet) (pool.Account, error) {
	provider := NewCodexProvider(nil)
	return m.saveAccount(ctx, provider, &token)
}

func (m *Manager) ImportCodexCLI(ctx context.Context, path string) (pool.Account, error) {
	file, err := os.Open(path)
	if err != nil {
		return pool.Account{}, fmt.Errorf("open Codex auth file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return pool.Account{}, fmt.Errorf("stat Codex auth file: %w", err)
	}
	if info.Size() > 1<<20 {
		return pool.Account{}, fmt.Errorf("Codex auth file exceeds 1 MiB")
	}
	var auth struct {
		Tokens struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
		LastRefresh time.Time `json:"last_refresh"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	if err := decoder.Decode(&auth); err != nil {
		return pool.Account{}, fmt.Errorf("decode Codex auth file: %w", err)
	}
	if auth.Tokens.AccessToken == "" || auth.Tokens.IDToken == "" {
		return pool.Account{}, fmt.Errorf("Codex auth file has no usable tokens")
	}
	return m.ImportCodex(ctx, TokenSet{
		AccessToken: auth.Tokens.AccessToken, RefreshToken: auth.Tokens.RefreshToken,
		IDToken: auth.Tokens.IDToken, LastRefreshAt: auth.LastRefresh,
		ExpiresAt: tokenExpiry(auth.Tokens.AccessToken, 0, time.Now()),
	})
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	servers := make(map[string]*http.Server, len(m.servers))
	for provider, server := range m.servers {
		servers[provider] = server
	}
	m.mu.Unlock()

	var firstErr error
	for provider, server := range servers {
		m.mu.Lock()
		listener := m.listeners[server]
		m.mu.Unlock()
		if listener != nil {
			_ = listener.Close()
		}
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && firstErr == nil {
			firstErr = err
		}
		m.stopServer(provider, server)
	}
	return firstErr
}

func callbackListenHost() string {
	if os.Getenv("LITEROUTER_OAUTH_CALLBACK_CONTAINER") == "true" {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// callbackPublicHost is the host advertised in redirect_uri.
// xAI expects 127.0.0.1 (same as 9Router); Codex/Claude keep localhost.
func callbackPublicHost(provider string) string {
	if provider == "grok" || provider == "xai" {
		return "127.0.0.1"
	}
	return "localhost"
}

func (m *Manager) listen(provider string, ports []int, callbackPath string) (int, *http.Server, error) {
	m.mu.Lock()
	existing := m.servers[provider]
	m.mu.Unlock()
	if existing != nil {
		m.stopServer(provider, existing)
	}

	var lastErr error
	for _, port := range ports {
		listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", callbackListenHost(), port))
		if err != nil {
			lastErr = err
			continue
		}
		mux := http.NewServeMux()
		server := &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
			m.handleCallback(provider, server, w, r)
		})
		server.RegisterOnShutdown(func() { _ = listener.Close() })
		m.mu.Lock()
		m.listeners[server] = listener
		m.mu.Unlock()
		return port, server, nil
	}
	return 0, nil, fmt.Errorf("start %s OAuth callback server: %w", provider, lastErr)
}

func (m *Manager) serveCallback(provider OAuthProvider, server *http.Server) {
	m.mu.Lock()
	listener := m.listeners[server]
	m.mu.Unlock()
	if listener == nil {
		m.logger.Error("OAuth callback listener missing", "provider", provider.Name())
		return
	}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		m.logger.Error("OAuth callback server failed", "provider", provider.Name(), "error", err)
	}
}

func (m *Manager) handleCallback(providerName string, server *http.Server, w http.ResponseWriter, r *http.Request) {
	finish := func(success bool, message string) {
		writeResultPage(w, success, providerName, message)
		m.scheduleStop(providerName, server)
	}
	query := r.URL.Query()
	state := query.Get("state")
	if err := m.signer.Verify(state, providerName); err != nil {
		finish(false, "Invalid or expired OAuth state")
		return
	}
	if upstreamError := query.Get("error"); upstreamError != "" {
		message := query.Get("error_description")
		if message == "" {
			message = upstreamError
		}
		finish(false, message)
		return
	}
	code := query.Get("code")
	if code == "" {
		finish(false, "Missing authorization code")
		return
	}
	session, err := m.store.TakeOAuthSession(r.Context(), state, providerName)
	if err != nil {
		finish(false, "OAuth session expired or already used")
		return
	}

	var provider OAuthProvider
	switch providerName {
	case "codex":
		provider = NewCodexProvider(nil)
	case "claude":
		provider = NewClaudeProvider(nil)
	case "grok", "xai":
		provider = NewGrokProvider(nil)
	case "antigravity":
		provider = NewAntigravityProvider(nil)
	default:
		finish(false, "Unsupported OAuth provider")
		return
	}
	m.mu.Lock()
	listener := m.listeners[server]
	m.mu.Unlock()
	if listener == nil {
		finish(false, "OAuth callback unavailable")
		return
	}
	port := listener.Addr().(*net.TCPAddr).Port
	provider = provider.WithRedirectURL(fmt.Sprintf("http://%s:%d%s", callbackPublicHost(provider.Name()), port, provider.CallbackPath()))
	if providerName == "claude" {
		code += "#" + state
	}
	exchangeContext, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	token, err := provider.Exchange(exchangeContext, code, session.Verifier)
	if err == nil {
		_, err = m.saveAccount(exchangeContext, provider, token)
	}
	if err != nil {
		m.logger.Warn("OAuth callback failed", "provider", providerName, "error", err)
		finish(false, "Authentication failed. Return to LiteRouter and retry.")
		return
	}
	finish(true, "Account connected. This window will close automatically.")
}

// CompleteManualCallback finishes a browser OAuth flow from a pasted callback URL or bare code.
// Used by the dashboard connect modal (9router-style) when the loopback popup cannot complete.
func (m *Manager) CompleteManualCallback(ctx context.Context, providerName, raw string) (pool.Account, error) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	if providerName == "xai" {
		providerName = "grok"
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return pool.Account{}, fmt.Errorf("callback URL or code is required")
	}

	code, state, err := parseManualOAuthInput(raw)
	if err != nil {
		return pool.Account{}, err
	}
	if code == "" {
		return pool.Account{}, fmt.Errorf("missing authorization code")
	}

	var session storage.OAuthSession
	if state != "" {
		if err := m.signer.Verify(state, providerName); err != nil {
			// also try alias
			if providerName == "grok" {
				if err2 := m.signer.Verify(state, "xai"); err2 == nil {
					err = nil
				}
			}
			if err != nil {
				return pool.Account{}, fmt.Errorf("invalid or expired OAuth state")
			}
		}
		session, err = m.store.TakeOAuthSession(ctx, state, providerName)
		if err != nil && providerName == "grok" {
			session, err = m.store.TakeOAuthSession(ctx, state, "xai")
		}
		if err != nil {
			return pool.Account{}, fmt.Errorf("OAuth session expired or already used — click Connect again, finish login, then paste immediately")
		}
	} else {
		// Bare authorization code: pair with the newest live session for this provider.
		// Common when the browser fails to open 127.0.0.1:1456 and the user copies only `code`.
		session, err = m.store.TakeLatestOAuthSession(ctx, providerName)
		if err != nil && providerName == "grok" {
			session, err = m.store.TakeLatestOAuthSession(ctx, "xai")
		}
		if err != nil {
			return pool.Account{}, fmt.Errorf("no active OAuth session — click Connect again, authorize in browser, then paste the code/URL right away")
		}
		state = session.State
	}

	provider, err := browserProvider(providerName)
	if err != nil {
		return pool.Account{}, err
	}

	// Prefer the redirect URL from the live callback server if still running.
	m.mu.Lock()
	server := m.servers[providerName]
	if server == nil && providerName == "grok" {
		server = m.servers["xai"]
	}
	var port int
	if server != nil {
		if ln := m.listeners[server]; ln != nil {
			if ta, ok := ln.Addr().(*net.TCPAddr); ok {
				port = ta.Port
			}
		}
	}
	m.mu.Unlock()
	if port > 0 {
		provider = provider.WithRedirectURL(fmt.Sprintf("http://%s:%d%s", callbackPublicHost(provider.Name()), port, provider.CallbackPath()))
	}

	exchangeCode := code
	if providerName == "claude" {
		exchangeCode = code + "#" + state
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	token, err := provider.Exchange(exchangeCtx, exchangeCode, session.Verifier)
	if err != nil {
		return pool.Account{}, fmt.Errorf("token exchange failed: %w", err)
	}
	account, err := m.saveAccount(exchangeCtx, provider, token)
	if err != nil {
		return pool.Account{}, err
	}
	// Stop callback server after successful manual completion.
	if server != nil {
		m.scheduleStop(providerName, server)
	}
	return account, nil
}

func browserProvider(name string) (OAuthProvider, error) {
	switch name {
	case "codex":
		return NewCodexProvider(nil), nil
	case "claude":
		return NewClaudeProvider(nil), nil
	case "grok", "xai":
		return NewGrokProvider(nil), nil
	case "antigravity":
		return NewAntigravityProvider(nil), nil
	default:
		return nil, fmt.Errorf("unsupported OAuth provider %q", name)
	}
}

func parseManualOAuthInput(raw string) (code, state string, err error) {
	raw = strings.TrimSpace(raw)
	raw = strings.Trim(raw, `"'`)
	if raw == "" {
		return "", "", fmt.Errorf("callback URL or code is required")
	}

	// Prefer structured URL / query parsing whenever the paste looks like a callback.
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "http") || strings.Contains(raw, "callback?") ||
		strings.Contains(raw, "code=") || strings.Contains(raw, "state=") || strings.Contains(raw, "127.0.0.1") ||
		strings.Contains(raw, "localhost") {
		candidate := raw
		// allow missing scheme
		if !strings.Contains(candidate, "://") {
			if strings.HasPrefix(candidate, "localhost") || strings.HasPrefix(candidate, "127.0.0.1") || strings.HasPrefix(candidate, "/") {
				candidate = "http://" + strings.TrimPrefix(candidate, "/")
				if strings.HasPrefix(raw, "/") {
					candidate = "http://127.0.0.1" + raw
				}
			} else if strings.Contains(candidate, "code=") || strings.Contains(candidate, "state=") {
				// pasted querystring only: code=...&state=...
				candidate = "http://127.0.0.1/callback?" + strings.TrimPrefix(candidate, "?")
			}
		}
		u, perr := url.Parse(candidate)
		if perr != nil {
			return "", "", fmt.Errorf("invalid callback URL")
		}
		q := u.Query()
		if e := q.Get("error"); e != "" {
			msg := q.Get("error_description")
			if msg == "" {
				msg = e
			}
			return "", "", fmt.Errorf("%s", msg)
		}
		code = q.Get("code")
		state = q.Get("state")
		// sometimes pasted as fragment
		if (code == "" || state == "") && u.Fragment != "" {
			fq, _ := url.ParseQuery(u.Fragment)
			if code == "" {
				code = fq.Get("code")
			}
			if state == "" {
				state = fq.Get("state")
			}
		}
		// Last-resort regex extraction for mangled pastes (line breaks, extra text).
		if code == "" || state == "" {
			if code == "" {
				code = firstQueryValue(raw, "code")
			}
			if state == "" {
				state = firstQueryValue(raw, "state")
			}
		}
		if code == "" {
			return "", "", fmt.Errorf("missing authorization code in callback URL")
		}
		if state == "" {
			return "", "", fmt.Errorf("missing OAuth state — paste the full callback URL including state=")
		}
		return code, state, nil
	}

	// Bare authorization code — CompleteManualCallback pairs it with the latest live session.
	ws := " " + "\t" + "\n" + "\r"
	if strings.ContainsAny(raw, ws) || strings.Contains(raw, "/") {
		return "", "", fmt.Errorf("unrecognized paste — use the full callback URL or the bare code value")
	}
	if len(raw) < 16 {
		return "", "", fmt.Errorf("authorization code looks too short")
	}
	return raw, "", nil
}
func firstQueryValue(raw, key string) string {
	// Match key=value in messy pasted text without requiring a full URL parse.
	needle := key + "="
	lower := strings.ToLower(raw)
	idx := strings.Index(lower, needle)
	if idx < 0 {
		return ""
	}
	value := raw[idx+len(needle):]
	cutset := "&" + "\n" + "\r" + "\t" + " #"
	if end := strings.IndexAny(value, cutset); end >= 0 {
		value = value[:end]
	}
	decoded, err := url.QueryUnescape(strings.TrimSpace(value))
	if err == nil {
		return decoded
	}
	return strings.TrimSpace(value)
}

func (m *Manager) saveAccount(ctx context.Context, provider OAuthProvider, token *TokenSet) (pool.Account, error) {
	info, err := provider.AccountInfo(ctx, token)
	if err != nil {
		return pool.Account{}, err
	}
	credentials, err := json.Marshal(token)
	if err != nil {
		return pool.Account{}, fmt.Errorf("encode OAuth credentials: %w", err)
	}
	accountID := provider.Name() + ":" + info.ID
	label := info.Email
	if label == "" {
		label = accountID
	}
	enabled, weight := true, 1
	if existing, getErr := m.store.GetAccount(ctx, accountID); getErr == nil {
		// A successful OAuth login replaces invalid credentials and must recover
		// accounts disabled after a permanent token refresh failure.
		weight = existing.Weight
	} else if !errors.Is(getErr, sql.ErrNoRows) {
		return pool.Account{}, getErr
	}
	if err := m.store.UpsertAccount(ctx, storage.Account{
		ID: accountID, Provider: provider.Name(), Label: label, Plan: info.Plan, Credentials: credentials, Enabled: enabled, Weight: weight,
	}); err != nil {
		return pool.Account{}, err
	}
	if err := m.store.UpdateAccountRouting(ctx, accountID, enabled, weight); err != nil {
		return pool.Account{}, err
	}
	account := pool.Account{ID: accountID, Provider: provider.Name(), Label: label, Plan: info.Plan, Enabled: enabled, Weight: weight}
	m.pool.Upsert(account)
	m.logger.Info("OAuth account connected", "provider", provider.Name(), "account_id", accountID, "plan", info.Plan)
	m.mu.Lock()
	hook := m.onAccountConnected
	m.mu.Unlock()
	if hook != nil {
		go func(id string) {
			hookCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			hook(hookCtx, id)
		}(accountID)
	}
	return account, nil
}

func (m *Manager) expireFlow(provider string, server *http.Server, expiresAt time.Time) {
	timer := time.NewTimer(time.Until(expiresAt))
	defer timer.Stop()
	<-timer.C
	m.stopServer(provider, server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.DeleteExpiredOAuthSessions(ctx, time.Now()); err != nil {
		m.logger.Warn("delete expired OAuth sessions", "error", err)
	}
}

func (m *Manager) scheduleStop(provider string, server *http.Server) {
	time.AfterFunc(750*time.Millisecond, func() { m.stopServer(provider, server) })
}

func (m *Manager) stopServer(provider string, server *http.Server) {
	m.mu.Lock()
	if m.servers[provider] == server {
		delete(m.servers, provider)
	}
	listener := m.listeners[server]
	delete(m.listeners, server)
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := server.Shutdown(ctx)
	cancel()
	if err != nil {
		_ = server.Close()
	}
	if listener != nil {
		_ = listener.Close()
	}
}

func writeResultPage(w http.ResponseWriter, success bool, provider, message string) {
	status := "Authentication failed"
	okJS := "false"
	if success {
		status = "Authentication successful"
		okJS = "true"
	}
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", `'`, "&#39;")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset=utf-8><title>%s</title>
<style>body{font-family:system-ui,sans-serif;background:#111827;color:#f9fafb;display:grid;place-items:center;min-height:100vh;margin:0}
main{max-width:28rem;padding:2rem;border:1px solid #374151;border-radius:1rem;background:#1f2937;text-align:center}
h1{font-size:1.25rem;margin:0 0 .75rem}p{color:#d1d5db;margin:0}</style></head>
<body><main><h1>%s</h1><p>%s</p></main>
<script>
try{if(window.opener){window.opener.postMessage({type:'literouter-oauth',ok:%s,provider:%q},'*');}}catch(e){}
setTimeout(function(){window.close();},1500);
</script></body></html>`, status, status, esc.Replace(message), okJS, provider)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func ValidateAuthURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("authorization URL must use HTTPS")
	}
	return nil
}
