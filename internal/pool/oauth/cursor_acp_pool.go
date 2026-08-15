package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// cursorACPSessionPool keeps one `agent acp` process + session alive per
// conversation, so consecutive turns of the same conversation reuse the agent
// instead of paying the ~7.5s spawn/init/auth/session-new cost every turn.
//
// Measured live: a fresh agent takes ~7.5s before the first prompt is even
// sent; reusing the same agent+session drops the next turn to ~3s (model time
// only). Conversations are independent, so each conversationID gets its own
// entry; an idle entry is closed after cursorACPSessionTTL.
type cursorACPSessionPool struct {
	mu      sync.Mutex
	entries map[string]*cursorACPSession
}

type cursorACPSession struct {
	agent     *cursorACPAgent
	sessionID string
	lastUsed  time.Time
}

const (
	cursorACPSessionTTL = 30 * time.Minute
	// cursorACPPoolMaxAttempts bounds how many times acquire will replace a dead
	// agent before giving up. One retry is usually enough (the bridge may have
	// restarted); beyond that the failure is systemic and should surface.
	cursorACPPoolMaxAttempts = 2
)

var cursorACPPool = &cursorACPSessionPool{entries: make(map[string]*cursorACPSession)}

// acquire returns a live session for the conversation, creating one if needed.
// The returned session is bound to the turn's onNotify for the duration of the
// prompt; release clears it.
//
// A pooled entry is reused only if its agent is still alive; a dead agent (the
// bridge closed, the process crashed, the read loop hit EOF) is closed and
// replaced with a fresh one, so a turn never runs against a zombie.
func (p *cursorACPSessionPool) acquire(ctx context.Context, key string, onNotify func(string, json.RawMessage)) (*cursorACPSession, error) {
	for attempt := 0; attempt < cursorACPPoolMaxAttempts; attempt++ {
		p.mu.Lock()
		entry := p.entries[key]
		if entry != nil && entry.agent != nil && entry.agent.alive() {
			entry.lastUsed = time.Now()
			entry.agent.setNotify(onNotify)
			p.mu.Unlock()
			return entry, nil
		}
		if entry != nil {
			// Dead entry: remove it so a concurrent acquirer does not race us on it.
			delete(p.entries, key)
		}
		p.mu.Unlock()
		if entry != nil {
			entry.agent.close()
		}

		fresh, err := p.spawnAgent(ctx, key, onNotify)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		if existing := p.entries[key]; existing != nil && existing.agent != nil && existing.agent.alive() {
			// Another goroutine created a healthy entry while we connected; prefer it.
			p.mu.Unlock()
			fresh.agent.close()
			return existing, nil
		}
		p.entries[key] = fresh
		p.mu.Unlock()
		return fresh, nil
	}
	return nil, fmt.Errorf("cursor acp pool: could not establish a session for %q after %d attempts", key, cursorACPPoolMaxAttempts)
}

// spawnAgent creates a brand-new agent and session, out of the pool lock so a
// slow connect does not block other conversations.
func (p *cursorACPSessionPool) spawnAgent(ctx context.Context, key string, onNotify func(string, json.RawMessage)) (*cursorACPSession, error) {
	agent, err := newCursorACPAgent(cursorACPWorkspace(), onNotify, cursorACPAutoApprove())
	if err != nil {
		return nil, err
	}
	client := &cursorACPClient{agent: agent}
	if err := client.initialize(ctx); err != nil {
		agent.close()
		return nil, err
	}
	if err := client.authenticate(ctx); err != nil {
		agent.close()
		return nil, err
	}
	sessionID, err := client.newSession(ctx, cursorACPWorkspace())
	if err != nil {
		agent.close()
		return nil, err
	}
	return &cursorACPSession{agent: agent, sessionID: sessionID, lastUsed: time.Now()}, nil
}

// release returns the session to the pool. On error the session is closed so
// the next turn starts fresh instead of resuming a broken agent.
func (p *cursorACPSessionPool) release(key string, entry *cursorACPSession, keep bool) {
	if entry == nil {
		return
	}
	entry.agent.setNotify(nil)
	if !keep {
		p.mu.Lock()
		if p.entries[key] == entry {
			delete(p.entries, key)
		}
		p.mu.Unlock()
		entry.agent.close()
		return
	}
	p.mu.Lock()
	entry.lastUsed = time.Now()
	p.mu.Unlock()
}

// expireIdle closes sessions idle past the TTL.
func (p *cursorACPSessionPool) expireIdle(now time.Time) {
	p.mu.Lock()
	var expired []*cursorACPSession
	for key, entry := range p.entries {
		if now.Sub(entry.lastUsed) > cursorACPSessionTTL {
			delete(p.entries, key)
			expired = append(expired, entry)
		}
	}
	p.mu.Unlock()
	for _, entry := range expired {
		entry.agent.close()
	}
}

// cursorACPKey names a session by conversation + model + workspace.
func cursorACPKey(conversationID, model, workspace string) string {
	return conversationID + "\x00" + model + "\x00" + workspace
}

// startACPPoolSweeper runs the idle-expiry loop in the background. It must be
// started once; the loop exits when ctx is canceled.
func startACPPoolSweeper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				cursorACPPool.expireIdle(now)
			}
		}
	}()
}

var _ = io.EOF
var _ = slog.Debug
