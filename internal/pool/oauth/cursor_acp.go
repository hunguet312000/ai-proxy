package oauth

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"
)

// The Cursor CLI exposes its agent as an ACP (Agent Client Protocol) server over
// stdio. LiteRouter spawns `agent acp`, speaks newline-delimited JSON-RPC 2.0,
// and streams the agent's tool loop back to the client. This is the Phase 2
// inference transport replacing the private IDE protobuf endpoint.
//
// ACP method flow (verified live against the CLI):
//
//	initialize            -> agent capabilities
//	authenticate          -> {methodId:"cursor_login"} -> {} (uses stored token)
//	session/new           -> {cwd, mcpServers:[]} -> {sessionId, modes...}
//	session/prompt        -> {sessionId, prompt:[{type:"text",text}]} -> {stopReason}
//	notif session/update  -> {sessionUpdate:"agent_message_chunk", content:[{type:"text",text}]}...
//	req  session/request_permission -> {kind:"allow_once", name, optionId} -> reply {outcome:{...}}
//
// The CLI authenticates through the token LiteRouter stores in the DB (Phase 1),
// so no keychain access is needed inside the container.

// cursorACPAgent wraps one spawned `agent acp` process (or one TCP connection to
// the host bridge) and speaks JSON-RPC to it.
type cursorACPAgent struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// conn is set only in bridge mode (dialCursorACPAgent); closing it ends the
	// host-side agent.
	conn   net.Conn
	reader *bufio.Reader
	// responses keyed by id
	mu        sync.Mutex
	pending   map[uint64]chan json.RawMessage
	nextID    uint64
	onNotify  func(method string, params json.RawMessage)
	onRequest func(method string, params json.RawMessage, reply func(result any, err error))
}

// cursorACPClient is the high-level ACP client used by inference.
type cursorACPClient struct {
	agent *cursorACPAgent
}

func newCursorACPAgent(cwd string, onNotify func(string, json.RawMessage), onRequest func(string, json.RawMessage, func(any, error))) (*cursorACPAgent, error) {
	// The CLI ships a macOS binary, so inside a Linux container it cannot be
	// spawned directly. When LITEROUTER_CURSOR_ACP_HOST is set, LiteRouter talks
	// to a host-side bridge (scripts/cursor-acp-bridge.js) over TCP instead:
	// each connection there spawns a fresh `agent acp` and relays stdio.
	if host := os.Getenv("LITEROUTER_CURSOR_ACP_HOST"); host != "" {
		return dialCursorACPAgent(host, onNotify, onRequest)
	}
	binary := cursorACPBinary()
	if binary == "" {
		return nil, fmt.Errorf("cursor CLI agent not found (no agent on PATH, no /host-cursor-cli/versions, and LITEROUTER_CURSOR_ACP_HOST unset)")
	}
	cmd := exec.Command(binary, "acp")
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("cursor acp stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cursor acp stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("cursor acp stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cursor acp: %w", err)
	}
	agent := &cursorACPAgent{
		cmd: cmd, stdin: stdin, reader: bufio.NewReader(stdout),
		pending: make(map[uint64]chan json.RawMessage),
		onNotify: onNotify, onRequest: onRequest,
	}
	// Drain stderr to a buffer; the CLI logs there and it must not block.
	go func() {
		_, _ = io.Copy(io.Discard, stderr)
	}()
	go agent.readLoop()
	return agent, nil
}

// dialCursorACPAgent connects to the host bridge over TCP. Each dial maps to one
// `agent acp` process on the host; the connection is per turn, matching the
// spawn-per-turn lifecycle of the local path.
func dialCursorACPAgent(host string, onNotify func(string, json.RawMessage), onRequest func(string, json.RawMessage, func(any, error))) (*cursorACPAgent, error) {
	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect cursor acp bridge %s: %w", host, err)
	}
	agent := &cursorACPAgent{
		stdin: conn, reader: bufio.NewReader(conn),
		pending:  make(map[uint64]chan json.RawMessage),
		onNotify: onNotify, onRequest: onRequest,
		conn: conn,
	}
	go agent.readLoop()
	return agent, nil
}

func (a *cursorACPAgent) readLoop() {
	for {
		line, err := a.reader.ReadBytes('\n')
		if err != nil {
			// A nil cmd means bridge mode (dialCursorACPAgent); there is no process
			// to inspect. EOF there just means the host closed the connection.
			if !errors.Is(err, io.EOF) && a.cmd != nil && a.cmd.ProcessState == nil {
				// process died unexpectedly
			}
			return
		}
		var msg struct {
			ID     uint64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.ID != 0 && msg.Method == "" {
			// A response to one of our requests: carries result/error and no method.
			a.mu.Lock()
			ch := a.pending[msg.ID]
			delete(a.pending, msg.ID)
			a.mu.Unlock()
			if ch != nil {
				if msg.Error != nil {
					ch <- json.RawMessage(fmt.Sprintf(`{"__error__":%q}`, msg.Error.Message))
				} else {
					ch <- msg.Result
				}
				close(ch)
			}
			continue
		}
		if msg.Method != "" && msg.Result == nil && msg.Error == nil {
			// A request from the agent (session/request_permission) or a notification
			// (session/update). Requests carry an id — often 0 — and the reply must
			// echo that id. Notifications have no id at all.
			if len(msg.Params) == 0 {
				msg.Params = json.RawMessage("{}")
			}
			if a.onRequest != nil && (msg.ID != 0 || msg.Method != "session/update") {
				a.onRequest(msg.Method, msg.Params, func(result any, err error) {
					if err != nil {
						return
					}
					encoded, marshalErr := json.Marshal(map[string]any{
						"jsonrpc": "2.0", "id": msg.ID, "result": result,
					})
					if marshalErr == nil {
						_, _ = a.stdin.Write(append(encoded, '\n'))
					}
				})
				continue
			}
			if a.onNotify != nil {
				a.onNotify(msg.Method, msg.Params)
				continue
			}
		}
	}
}

// send writes one request and waits for its response.
func (a *cursorACPAgent) send(ctx context.Context, method string, params any) (json.RawMessage, error) {
	a.mu.Lock()
	a.nextID++
	id := a.nextID
	ch := make(chan json.RawMessage, 1)
	a.pending[id] = ch
	a.mu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	if _, err := a.stdin.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("cursor acp send %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case raw := <-ch:
		if len(raw) > 0 && raw[0] == '{' && json.Valid(raw) {
			var errMsg struct {
				Error string `json:"__error__"`
			}
			if json.Unmarshal(raw, &errMsg) == nil && errMsg.Error != "" {
				return nil, fmt.Errorf("cursor acp %s: %s", method, errMsg.Error)
			}
		}
		return raw, nil
	}
}

// close terminates the agent process.
func (a *cursorACPAgent) close() {
	_ = a.stdin.Close()
	if a.conn != nil {
		_ = a.conn.Close()
	}
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
		_, _ = a.cmd.Process.Wait()
	}
}

// initialize handshakes and returns agent capabilities.
func (c *cursorACPClient) initialize(ctx context.Context) error {
	raw, err := c.agent.send(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": "literouter", "version": "0.1"},
	})
	if err != nil {
		return err
	}
	var caps struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	_ = json.Unmarshal(raw, &caps)
	return nil
}

// authenticate performs the cursor_login handshake using the stored token.
func (c *cursorACPClient) authenticate(ctx context.Context) error {
	raw, err := c.agent.send(ctx, "authenticate", map[string]any{"methodId": "cursor_login"})
	if err != nil {
		return err
	}
	_ = raw
	return nil
}

// newSession creates an agent session in cwd.
func (c *cursorACPClient) newSession(ctx context.Context, cwd string) (string, error) {
	raw, err := c.agent.send(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return "", err
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("cursor acp session/new: %w", err)
	}
	if result.SessionID == "" {
		return "", fmt.Errorf("cursor acp session/new returned no session id")
	}
	return result.SessionID, nil
}

// prompt sends a text prompt and waits for the agent's final turn result.
func (c *cursorACPClient) prompt(ctx context.Context, sessionID, text string) (json.RawMessage, error) {
	raw, err := c.agent.send(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": text}},
	})
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// setModel switches the session's model (unstable ACP method, may be missing).
func (c *cursorACPClient) setModel(ctx context.Context, sessionID, model string) error {
	raw, err := c.agent.send(ctx, "session/set_model", map[string]any{"sessionId": sessionID, "model": model})
	if err != nil {
		return err
	}
	_ = raw
	return nil
}

var _ = time.Second
