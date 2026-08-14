package oauth

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"literouter/internal/translator"
)

// Cursor's agent keeps no conversation state of its own: it streams the state out to
// the client as blobs and expects it back on the next turn. Holding that state is what
// turns a proxy from "resend the whole transcript every turn" into "send the new turn
// only", which is the entire token saving available here.
//
// The shape was read off the IDE's own stored state (composerData.conversationState):
// after a turn, root_prompt_messages_json carries the turn's messages *in addition* to
// the prompt prefix, and turns carries the turn structure's blob id. Appending only the
// turn — the obvious reading of the schema — produces a model that answers as if the
// conversation never happened, silently.
//
// Disabled with LITEROUTER_CURSOR_CACHE=0. It is on by default because the fallback is
// safe: any history the proxy cannot prove it already sent starts a fresh conversation.
const (
	cursorCacheTTL      = 2 * time.Hour
	cursorCacheMaxEntry = 256
	cursorCacheMaxBlobs = 512
	// The blobs are the conversation itself, so they are large: a folded transcript
	// becomes one blob, and a long coding session produces hundreds of kilobytes per
	// turn. Counting entries alone bounds nothing — these two caps bound the memory.
	cursorCacheMaxConversationBytes = 8 << 20   // 8 MB: past this, replaying is no longer the cheap path
	cursorCacheMaxTotalBytes        = 128 << 20 // 128 MB across every conversation
	cursorStateAgentType            = "ide"
)

func cursorCacheEnabled() bool {
	return strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_CACHE")) != "0"
}

// cursorConversation is everything needed to continue one conversation upstream.
type cursorConversation struct {
	id string
	// state is the ConversationStateStructure to replay, already carrying the roots
	// and turns of every completed turn.
	state []byte
	// blobs is the store the service reads through: it asks for ids it wrote earlier
	// and the turn cannot be reconstructed without them.
	blobs map[string][]byte
	// history fingerprints the client messages already folded into this conversation.
	// A request whose history is not an extension of this cannot reuse the state.
	history []string
	// seenToolCalls persists the tool-call signatures already emitted in this
	// conversation, so the repeated-call suppression survives across turns.
	seenToolCalls map[string]struct{}
	// bytes is the resident size of state plus blobs, kept so eviction can bound memory
	// rather than conversation count.
	bytes   int
	updated time.Time
}

func (c *cursorConversation) size() int {
	total := len(c.state)
	for id, data := range c.blobs {
		total += len(id) + len(data)
	}
	return total
}

type cursorConversationCache struct {
	mu      sync.Mutex
	entries map[string]*cursorConversation
	// bytes is the sum of every entry's size, maintained on insert and eviction so the
	// cap costs nothing to enforce.
	bytes int
}

var cursorConversations = &cursorConversationCache{entries: map[string]*cursorConversation{}}

// messageFingerprint identifies one client message. Content is hashed rather than kept
// so a long transcript does not pin megabytes per conversation.
//
// Tool calls are part of the identity, not decoration. An assistant turn that only calls
// a tool carries no text at all, and a coding transcript is mostly those — hashing text
// alone made every one of them the same fingerprint, which left isPrefix unable to tell
// two different branches apart. isPrefix is the only thing standing between a request
// and someone else's upstream conversation, so it has to see what actually differs.
func messageFingerprint(message translator.OpenAIMessage) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(message.Role + "\x00" + openAIContentText(message.Content)))
	_, _ = hash.Write([]byte("\x00" + message.ToolCallID + "\x00" + message.Name))
	for _, call := range message.ToolCalls {
		_, _ = hash.Write([]byte("\x00" + call.ID + "\x00" + call.Type +
			"\x00" + call.Function.Name + "\x00" + call.Function.Arguments))
	}
	return hex.EncodeToString(hash.Sum(nil)[:8])
}

// cursorCacheKey scopes a conversation to the account that owns it.
//
// The model is deliberately left out: the upstream conversation id and its
// replayable state belong to one account, but they are model-agnostic — switching
// the model mid-session (default → grok) must continue the same conversation, not
// start a fresh one that re-sends the whole transcript. Scoping by model made
// every model switch a cache miss and a full resend, which is exactly the slow
// path. The session key is derived from the transcript (the first user message
// when the client sends no X-Conversation-ID), so two sessions that open the same
// way still collide safely: the history prefix check rejects a mismatched one.
//
// An empty session stays empty: that is the signal that disables the cache.
func cursorCacheKey(session, accountID, model string) string {
	if session == "" {
		return ""
	}
	return session + "\x00" + accountID
}

func requestFingerprints(request translator.OpenAIRequest) []string {
	out := make([]string, 0, len(request.Messages))
	for _, message := range request.Messages {
		out = append(out, messageFingerprint(message))
	}
	return out
}

// isPrefix reports whether stored is the leading part of current. Anything else — an
// edited turn, a compaction, a branch — means the upstream conversation no longer
// matches the client's, and continuing it would answer from a history the user cannot
// see.
func isPrefix(stored, current []string) bool {
	if len(stored) == 0 || len(stored) > len(current) {
		return false
	}
	for index, value := range stored {
		if current[index] != value {
			return false
		}
	}
	return true
}

// lookup returns a conversation that can be continued for this request, along with the
// messages that still have to be sent.
func (c *cursorConversationCache) lookup(key string, request translator.OpenAIRequest) (*cursorConversation, []translator.OpenAIMessage) {
	if !cursorCacheEnabled() || key == "" {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		slog.Debug("cursor cache miss", "key", shortKey(key), "entries", len(c.entries))
		return nil, nil
	}
	if time.Since(entry.updated) > cursorCacheTTL || len(entry.state) == 0 {
		c.dropLocked(key)
		return nil, nil
	}
	current := requestFingerprints(request)
	if !isPrefix(entry.history, current) {
		// Diverged: drop it rather than answer from a stale history.
		c.dropLocked(key)
		return nil, nil
	}
	pending := request.Messages[len(entry.history):]
	// The service recorded its own reply in the state it handed back, so replaying the
	// assistant turn would show the model its previous answer twice. Tool results are
	// never dropped: those come from the client and the service has never seen them.
	for len(pending) > 0 && pending[0].Role == "assistant" {
		pending = pending[1:]
	}
	if len(pending) == 0 {
		return nil, nil
	}
	entry.updated = time.Now()
	return entry, pending
}

func (c *cursorConversationCache) store(key string, entry *cursorConversation) {
	if !cursorCacheEnabled() || key == "" || entry == nil {
		return
	}
	entry.bytes = entry.size()
	if entry.bytes > cursorCacheMaxConversationBytes {
		// Replaying a conversation this large is no longer the cheap path, and holding
		// it would let one session dominate the cache.
		slog.Debug("cursor conversation too large to cache", "bytes", entry.bytes)
		c.drop(key)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[key]; ok {
		c.bytes -= existing.bytes
	}
	entry.updated = time.Now()
	c.entries[key] = entry
	c.bytes += entry.bytes
	for len(c.entries) > cursorCacheMaxEntry || c.bytes > cursorCacheMaxTotalBytes {
		if !c.evictOldestLocked() {
			break
		}
	}
}

func (c *cursorConversationCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(key)
}

func (c *cursorConversationCache) dropLocked(key string) {
	if entry, ok := c.entries[key]; ok {
		c.bytes -= entry.bytes
		delete(c.entries, key)
	}
}

// evictOldestLocked removes the least recently used entry, reporting whether there was
// one to remove so a caller can loop without spinning on an empty cache.
func (c *cursorConversationCache) evictOldestLocked() bool {
	var oldestKey string
	var oldest time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.updated.Before(oldest) {
			oldestKey, oldest = key, entry.updated
		}
	}
	if oldestKey == "" {
		return false
	}
	c.dropLocked(oldestKey)
	return true
}

// cursorRunSession owns one upstream run. The request body stays open for the whole
// turn because the service reads its own state back through it: a half-closed stream
// cannot answer a get_blob, and the turn then silently loses its history.
type cursorRunSession struct {
	io.ReadCloser
	writer *io.PipeWriter

	key          string
	conversation *cursorConversation
	// pendingHistory is the client-side transcript this turn brings the conversation
	// up to, recorded only once the turn completes.
	pendingHistory []string
	// conversationID is the id sent upstream, kept so a continued conversation keeps
	// addressing the same one.
	conversationID string

	// sentPromptTokens is the proxy's estimate of the prompt it actually put on the
	// wire, which is the delta when a conversation is replayed.
	sentPromptTokens int

	mu sync.Mutex
	// written records blobs in the order the service wrote them, which is the order
	// root_prompt_messages_json expects.
	written []blobEntry
	roots   int

	// seenToolCalls remembers every tool call this conversation has emitted, keyed
	// by name+arguments. It persists across turns through the conversation object,
	// so the "survey the pipeline" loop — the agent calling the same tool with the
	// same arguments on every turn — is suppressed even when it spans turns.
	seenToolCalls map[string]struct{}
	// turnBlobSeen marks that the turn blob for this run has been received; the
	// streaming path waits for it after ENDED before committing.
	turnBlobSeen bool
}

type blobEntry struct {
	id   string
	data []byte
}

func newCursorRunSession(key string, conversation *cursorConversation, writer *io.PipeWriter) *cursorRunSession {
	session := &cursorRunSession{key: key, conversation: conversation, writer: writer}
	if conversation != nil {
		// Seed the suppression set from what this conversation has already called,
		// so a tool call that repeated across turns is suppressed on the very first
		// frame of this turn rather than rediscovered mid-stream.
		session.seenToolCalls = conversation.seenToolCalls
	}
	return session
}

// promptTokens reports what this run sent upstream. Nil-safe: the offline decoder
// tests run without a session.
func (s *cursorRunSession) promptTokens() int {
	if s == nil {
		return 0
	}
	return s.sentPromptTokens
}

// handleKV answers the service's blob traffic. Returns true when the frame was a blob
// message and carried no model output.
func (s *cursorRunSession) handleKV(payload []byte) bool {
	fields := parseProtoFields(payload)
	values, ok := fields[agentServerKvField]
	if !ok || len(values) == 0 {
		return false
	}
	message := parseProtoFields(values[0].Bytes)
	var id uint64
	if ids, ok := message[1]; ok && len(ids) > 0 {
		id = ids[0].Varint
	}
	if set, ok := message[3]; ok && len(set) > 0 {
		args := parseProtoFields(set[0].Bytes)
		blobID, _ := agentString(args, 1)
		data, _ := agentString(args, 2)
		s.putBlob(blobID, []byte(data))
		s.send(protoConcat(
			protoField(1, protoVarint, id),
			protoField(3, protoBytes, []byte{}),
		))
	}
	if get, ok := message[2]; ok && len(get) > 0 {
		blobID, _ := agentString(parseProtoFields(get[0].Bytes), 1)
		data, found := s.getBlob(blobID)
		if !found {
			// Answering with nothing is better than stalling: the service then rebuilds
			// what it needs instead of waiting on a reply that will never come.
			slog.Warn("cursor asked for a blob this proxy does not hold", "blob", blobID[:min(8, len(blobID))])
		}
		s.send(protoConcat(
			protoField(1, protoVarint, id),
			protoField(2, protoBytes, protoField(1, protoBytes, data)),
		))
	}
	return true
}

const agentServerKvField = 4

func (s *cursorRunSession) send(kv []byte) {
	if s.writer == nil {
		return
	}
	_, _ = s.writer.Write(connectFrame(protoField(agentClientKvField, protoBytes, kv), false))
}

const agentClientKvField = 3 // AgentClientMessage.kv_client_message

func (s *cursorRunSession) putBlob(id string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A turn blob is new on every turn and must always be recorded, even when its
	// id repeats a stored blob: commit() needs the turn record of THIS turn to fold
	// into the conversation, and skipping it on the id match left written without a
	// turn — which dropped every continuation. Other blobs (state, roots) are
	// deduplicated against the conversation store.
	isTurn := turnBlobID(blobEntry{id: id, data: data}) != nil
	if isTurn {
		s.turnBlobSeen = true
	}
	if !isTurn && s.conversation != nil {
		if _, seen := s.conversation.blobs[id]; seen {
			return
		}
	}
	slog.Debug("cursor blob received", "id", id[:min(16, len(id))], "bytes", len(data),
		"kind", blobKind(data), "turn", isTurn)
	s.written = append(s.written, blobEntry{id: id, data: data})
}

// blobKind names what a blob looks like, for the cache debugging logs.
func blobKind(data []byte) string {
	if kind, ok := agentString(parseProtoFields(data), cursorStateAgentTypeField); ok && kind == cursorStateAgentType {
		return "state"
	}
	if strings.HasPrefix(string(data), `{"role":`) {
		return "root"
	}
	return "other"
}

func (s *cursorRunSession) getBlob(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conversation != nil {
		if data, ok := s.conversation.blobs[id]; ok {
			return data, true
		}
	}
	for _, blob := range s.written {
		if blob.id == id {
			return blob.data, true
		}
	}
	return nil, false
}

// finish closes the request stream and records the turn. It is safe on a nil session:
// the offline decoder tests read plain frames with no upstream behind them.
func (s *cursorRunSession) finish() {
	if s == nil {
		return
	}
	if s.writer != nil {
		_ = s.writer.Close()
	}
	s.commit(s.pendingHistory)
}

// turnPending reports whether this session is still waiting for its turn blob —
// the record commit() needs to fold the turn into the stored conversation.
func (s *cursorRunSession) turnPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnBlobSeen {
		return false
	}
	return true
}

// waitForTurnBlob drains a few more frames from the upstream after ENDED so the
// kv channel delivers the turn blob, then marks it seen. commit() runs after this
// in the streaming path, so the turn is no longer lost.
func (s *cursorRunSession) waitForTurnBlob() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turnBlobSeen = true
}

// commit folds the turn the service just produced into the stored conversation, so the
// next request can continue instead of resending the transcript.
func (s *cursorRunSession) commit(history []string) {
	if s == nil || s.key == "" || !cursorCacheEnabled() {
		return
	}
	s.mu.Lock()
	written := s.written
	s.mu.Unlock()

	conversation := s.conversation
	if conversation == nil {
		conversation = &cursorConversation{id: s.conversationID, blobs: map[string][]byte{}}
	}
	var state []byte
	var roots [][]byte
	var turnID []byte
	priorRoots := 0
	for _, blob := range written {
		fields := parseProtoFields(blob.data)
		if kind, ok := agentString(fields, cursorStateAgentTypeField); ok && kind == cursorStateAgentType {
			state = blob.data
			priorRoots = len(fields[cursorStateRootsField])
			continue
		}
		if strings.HasPrefix(string(blob.data), `{"role":`) {
			roots = append(roots, []byte(blob.id))
			continue
		}
		if id := turnBlobID(blob); id != nil {
			turnID = id
		}
	}
	if turnID == nil {
		// A tool-call turn ends on an idle frame without a turn record — the service
		// sends state+roots but no turn blob. Continuing from state+roots alone is
		// still correct (the turn's messages are already folded into roots), so cache
		// the conversation with what arrived rather than dropping it. Only a turn
		// with neither state nor roots is un-cacheable.
		if state == nil && len(roots) == 0 {
			slog.Debug("cursor turn dropped: no state, roots or turn", "key", s.key != "", "blobs", len(written))
			cursorConversations.drop(s.key)
			return
		}
		slog.Debug("cursor turn cached without a turn blob", "key", s.key != "", "blobs", len(written),
			"state", state != nil, "roots", len(roots))
	}
	if state == nil {
		state = conversation.state
		priorRoots = 0
	}
	if state == nil {
		cursorConversations.drop(s.key)
		return
	}
	// The state already references the prompt prefix it was written with; appending
	// those again would duplicate the system prompt in the model's context.
	if priorRoots > 0 && len(roots) >= priorRoots {
		roots = roots[priorRoots:]
	}
	parts := [][]byte{state}
	for _, id := range roots {
		parts = append(parts, protoField(cursorStateRootsField, protoBytes, id))
	}
	if turnID != nil {
		parts = append(parts, protoField(cursorStateTurnsField, protoBytes, turnID))
	}

	conversation.state = protoConcat(parts...)
	conversation.history = history
	if len(s.seenToolCalls) > 0 {
		conversation.seenToolCalls = s.seenToolCalls
	}
	slog.Debug("cursor commit persisted", "key", s.key != "", "seen_tools", len(s.seenToolCalls),
		"stored_seen", len(conversation.seenToolCalls))
	if conversation.blobs == nil {
		conversation.blobs = map[string][]byte{}
	}
	for _, blob := range written {
		conversation.blobs[blob.id] = blob.data
	}
	if len(conversation.blobs) > cursorCacheMaxBlobs {
		// A conversation this large is no longer worth replaying; start clean rather
		// than grow without bound.
		cursorConversations.drop(s.key)
		return
	}
	cursorConversations.store(s.key, conversation)
}

const (
	cursorStateRootsField     = 1  // ConversationStateStructure.root_prompt_messages_json
	cursorStateTurnsField     = 8  // ConversationStateStructure.turns
	cursorStateAgentTypeField = 22 // ConversationStateStructure.agent_type
)

// turnBlobID reports the blob id when the blob is a ConversationTurnStructure: an
// agent_conversation_turn whose user_message is a blob reference.
func turnBlobID(blob blobEntry) []byte {
	fields := parseProtoFields(blob.data)
	turn, ok := fields[1]
	if !ok || len(turn) != 1 {
		return nil
	}
	inner := parseProtoFields(turn[0].Bytes)
	user, ok := inner[1]
	if !ok || len(user) != 1 || len(user[0].Bytes) != 32 {
		return nil
	}
	if _, hasSteps := inner[2]; !hasSteps {
		return nil
	}
	return []byte(blob.id)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func blobIDs(blobs []blobEntry) []string {
	out := make([]string, 0, len(blobs))
	for _, b := range blobs {
		id := b.id
		if len(id) > 16 {
			id = id[:16]
		}
		out = append(out, id)
	}
	return out
}

func shortKey(key string) string {
	if len(key) <= 24 {
		return key
	}
	return key[:24] + "..."
}
