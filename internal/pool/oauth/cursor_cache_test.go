package oauth

import (
	"testing"
	"time"

	"literouter/internal/translator"
)

func newCacheForTest(t *testing.T) *cursorConversationCache {
	t.Helper()
	t.Setenv("LITEROUTER_CURSOR_CACHE", "1")
	return &cursorConversationCache{entries: map[string]*cursorConversation{}}
}

func requestOf(messages ...translator.OpenAIMessage) translator.OpenAIRequest {
	return translator.OpenAIRequest{Messages: messages}
}

func TestCursorCacheContinuesOnlyTheNewMessages(t *testing.T) {
	cache := newCacheForTest(t)
	first := requestOf(
		translator.OpenAIMessage{Role: "system", Content: "be terse"},
		translator.OpenAIMessage{Role: "user", Content: "remember 8317"})
	cache.store("k", &cursorConversation{
		id: "c", state: []byte{1}, history: requestFingerprints(first),
		blobs: map[string][]byte{},
	})

	second := requestOf(
		translator.OpenAIMessage{Role: "system", Content: "be terse"},
		translator.OpenAIMessage{Role: "user", Content: "remember 8317"},
		translator.OpenAIMessage{Role: "assistant", Content: "noted"},
		translator.OpenAIMessage{Role: "user", Content: "which number?"})

	conversation, pending := cache.lookup("k", second)
	if conversation == nil {
		t.Fatal("an extended history must continue the conversation")
	}
	// The assistant turn is already in the replayed state; resending it would show the
	// model its own answer twice.
	if len(pending) != 1 || pending[0].Role != "user" {
		t.Fatalf("pending = %+v, want only the new user message", pending)
	}
}

func TestCursorCacheKeepsToolResultsInTheDelta(t *testing.T) {
	cache := newCacheForTest(t)
	first := requestOf(translator.OpenAIMessage{Role: "user", Content: "weather?"})
	cache.store("k", &cursorConversation{id: "c", state: []byte{1}, history: requestFingerprints(first)})

	second := requestOf(
		translator.OpenAIMessage{Role: "user", Content: "weather?"},
		translator.OpenAIMessage{Role: "assistant", Content: ""},
		translator.OpenAIMessage{Role: "tool", Content: "22C"})

	_, pending := cache.lookup("k", second)
	if len(pending) != 1 || pending[0].Role != "tool" {
		t.Fatalf("pending = %+v, want the tool result kept", pending)
	}
}

func TestCursorCacheRefusesADivergedHistory(t *testing.T) {
	// Compaction, an edited turn or a branch all change the transcript's prefix. Reusing
	// state then would answer from a history the user can no longer see, so the entry is
	// dropped and the next request rebuilds from scratch.
	cache := newCacheForTest(t)
	first := requestOf(
		translator.OpenAIMessage{Role: "user", Content: "remember 8317"},
		translator.OpenAIMessage{Role: "user", Content: "and 42"})
	cache.store("k", &cursorConversation{id: "c", state: []byte{1}, history: requestFingerprints(first)})

	compacted := requestOf(
		translator.OpenAIMessage{Role: "user", Content: "summary of earlier turns"},
		translator.OpenAIMessage{Role: "user", Content: "which number?"})

	if conversation, _ := cache.lookup("k", compacted); conversation != nil {
		t.Fatal("a diverged history must not reuse the stored state")
	}
	if _, ok := cache.entries["k"]; ok {
		t.Error("the diverged entry must be dropped, not left to match again later")
	}
}

func TestCursorCacheExpiresAndCanBeDisabled(t *testing.T) {
	cache := newCacheForTest(t)
	request := requestOf(translator.OpenAIMessage{Role: "user", Content: "hi"})
	stale := &cursorConversation{id: "c", state: []byte{1}, history: requestFingerprints(request)}
	cache.store("k", stale)
	stale.updated = time.Now().Add(-2 * cursorCacheTTL)

	next := requestOf(
		translator.OpenAIMessage{Role: "user", Content: "hi"},
		translator.OpenAIMessage{Role: "user", Content: "again"})
	if conversation, _ := cache.lookup("k", next); conversation != nil {
		t.Error("an expired conversation must not be continued")
	}

	cache.store("k2", &cursorConversation{id: "c", state: []byte{1}, history: requestFingerprints(request)})
	t.Setenv("LITEROUTER_CURSOR_CACHE", "0")
	if conversation, _ := cache.lookup("k2", next); conversation != nil {
		t.Error("LITEROUTER_CURSOR_CACHE=0 must switch the cache off")
	}
}

func TestCursorCacheEvictsTheOldestEntry(t *testing.T) {
	cache := newCacheForTest(t)
	for index := 0; index < cursorCacheMaxEntry+5; index++ {
		cache.store(string(rune('a'+index%26))+time.Duration(index).String(),
			&cursorConversation{id: "c", state: []byte{1}})
	}
	if len(cache.entries) > cursorCacheMaxEntry {
		t.Errorf("cache holds %d entries, want at most %d", len(cache.entries), cursorCacheMaxEntry)
	}
}

func TestCursorSessionCommitBuildsReplayableState(t *testing.T) {
	// The state the service writes carries only the prompt prefix. A continuation needs
	// the turn's messages appended to root_prompt_messages_json as well as the turn id;
	// appending only the turn produces a model with no memory and no error.
	t.Setenv("LITEROUTER_CURSOR_CACHE", "1")
	cursorConversations = &cursorConversationCache{entries: map[string]*cursorConversation{}}

	state := protoConcat(
		protoField(cursorStateRootsField, protoBytes, make([]byte, 32)),
		protoField(cursorStateRootsField, protoBytes, make([]byte, 32)),
		protoField(cursorStateAgentTypeField, protoBytes, cursorStateAgentType),
	)
	turn := protoField(1, protoBytes, protoConcat(
		protoField(1, protoBytes, make([]byte, 32)),
		protoField(2, protoBytes, make([]byte, 32))))

	session := newCursorRunSession("key", nil, nil)
	session.written = []blobEntry{
		{id: "system", data: []byte(`{"role":"system","content":"x"}`)},
		{id: "userinfo", data: []byte(`{"role":"user","content":"<user_info>"}`)},
		{id: "state", data: state},
		{id: "userturn", data: []byte(`{"role":"user","content":"remember 8317"}`)},
		{id: "assistant", data: []byte(`{"role":"assistant","content":"noted"}`)},
		{id: "turn", data: turn},
	}
	session.commit([]string{"fingerprint"})

	stored, ok := cursorConversations.entries["key"]
	if !ok {
		t.Fatal("the turn was not stored")
	}
	fields := parseProtoFields(stored.state)
	if got := len(fields[cursorStateRootsField]); got != 4 {
		t.Errorf("root_prompt_messages_json has %d refs, want 4 (2 prefix + 2 from the turn)", got)
	}
	if got := len(fields[cursorStateTurnsField]); got != 1 {
		t.Errorf("turns has %d refs, want 1", got)
	}
	if len(stored.blobs) != len(session.written) {
		t.Errorf("stored %d blobs, want all %d: the service reads them back by id",
			len(stored.blobs), len(session.written))
	}
}

func TestCursorSessionCommitDropsATurnItCannotReplay(t *testing.T) {
	t.Setenv("LITEROUTER_CURSOR_CACHE", "1")
	cursorConversations = &cursorConversationCache{entries: map[string]*cursorConversation{}}
	cursorConversations.store("key", &cursorConversation{id: "c", state: []byte{1}})

	session := newCursorRunSession("key", nil, nil)
	session.written = []blobEntry{{id: "only", data: []byte(`{"role":"user","content":"x"}`)}}
	session.commit(nil)

	if _, ok := cursorConversations.entries["key"]; ok {
		t.Error("without a turn structure the entry must be dropped, not left stale")
	}
}

func TestCursorCacheBoundsMemoryByBytesNotEntryCount(t *testing.T) {
	// Entry count bounds nothing here: one folded transcript can be hundreds of
	// kilobytes, so a few hundred conversations would be gigabytes of resident memory.
	cache := newCacheForTest(t)
	blob := make([]byte, 1<<20) // 1 MB per conversation
	for index := 0; index < 300; index++ {
		cache.store(string(rune('a'+index%26))+time.Duration(index).String(), &cursorConversation{
			id: "c", state: []byte{1}, blobs: map[string][]byte{"b": blob},
		})
	}
	if cache.bytes > cursorCacheMaxTotalBytes {
		t.Errorf("cache holds %d bytes, want at most %d", cache.bytes, cursorCacheMaxTotalBytes)
	}
	if len(cache.entries) > cursorCacheMaxEntry {
		t.Errorf("cache holds %d entries, want at most %d", len(cache.entries), cursorCacheMaxEntry)
	}
}

func TestCursorCacheRefusesAConversationLargerThanTheCap(t *testing.T) {
	cache := newCacheForTest(t)
	cache.store("k", &cursorConversation{
		id: "c", state: []byte{1},
		blobs: map[string][]byte{"huge": make([]byte, cursorCacheMaxConversationBytes+1)},
	})
	if _, ok := cache.entries["k"]; ok {
		t.Fatal("an oversized conversation must not be cached")
	}
	if cache.bytes != 0 {
		t.Errorf("cache accounts %d bytes after refusing the entry", cache.bytes)
	}
}

func TestCursorCacheAccountingStaysBalancedAcrossDropAndReplace(t *testing.T) {
	cache := newCacheForTest(t)
	entry := &cursorConversation{id: "c", state: []byte{1}, blobs: map[string][]byte{"b": make([]byte, 1000)}}
	cache.store("k", entry)
	first := cache.bytes
	// Replacing the same key must not double-count.
	cache.store("k", &cursorConversation{id: "c", state: []byte{1}, blobs: map[string][]byte{"b": make([]byte, 1000)}})
	if cache.bytes != first {
		t.Errorf("bytes = %d after replacing the same key, want %d", cache.bytes, first)
	}
	cache.drop("k")
	if cache.bytes != 0 {
		t.Errorf("bytes = %d after dropping the only entry, want 0", cache.bytes)
	}
}

// Two assistant turns that call different tools carry no text at all, which is the norm
// in a coding transcript. Hashing text alone made them the same fingerprint, so isPrefix
// — the only guard against continuing someone else's upstream conversation — could not
// tell two diverged branches apart and would replay the wrong history.
func TestCursorCacheFingerprintSeparatesDifferentToolCalls(t *testing.T) {
	call := func(name, args string) translator.OpenAIMessage {
		return translator.OpenAIMessage{Role: "assistant", ToolCalls: []translator.OpenAIToolCall{{
			ID: "call_1", Type: "function",
			Function: translator.OpenAIFunctionCall{Name: name, Arguments: args},
		}}}
	}
	readA := call("Read", `{"file_path":"/a.go"}`)
	readB := call("Read", `{"file_path":"/b.go"}`)
	if messageFingerprint(readA) == messageFingerprint(readB) {
		t.Fatal("two different tool calls must not share a fingerprint")
	}
	// And the guard has to act on it: a branch that called a different tool cannot reuse
	// the stored conversation.
	cache := newCacheForTest(t)
	stored := requestOf(translator.OpenAIMessage{Role: "user", Content: "read a file"}, readA)
	cache.store("k", &cursorConversation{id: "c", state: []byte{1}, history: requestFingerprints(stored)})

	diverged := requestOf(
		translator.OpenAIMessage{Role: "user", Content: "read a file"}, readB,
		translator.OpenAIMessage{Role: "tool", ToolCallID: "call_1", Content: "package b"})
	if conversation, _ := cache.lookup("k", diverged); conversation != nil {
		t.Fatal("a branch with a different tool call must not continue the stored conversation")
	}
}

func TestCursorCacheFingerprintSeparatesToolResultsByCallID(t *testing.T) {
	first := translator.OpenAIMessage{Role: "tool", ToolCallID: "call_1", Content: "same text"}
	second := translator.OpenAIMessage{Role: "tool", ToolCallID: "call_2", Content: "same text"}
	if messageFingerprint(first) == messageFingerprint(second) {
		t.Fatal("tool results answering different calls must not share a fingerprint")
	}
}

// The stored conversation id and state belong to one account and one model. The session
// key says nothing about either — without X-Conversation-ID it is derived from the first
// user message — so the key has to carry them.
func TestCursorCacheKeyScopesByAccountAndModel(t *testing.T) {
	base := cursorCacheKey("session", "acct-1", "gpt-5.6-luna")
	if base == cursorCacheKey("session", "acct-2", "gpt-5.6-luna") {
		t.Fatal("two accounts must not share a conversation entry")
	}
	if base == cursorCacheKey("session", "acct-1", "gpt-5.6-sol") {
		t.Fatal("two models must not share a conversation entry")
	}
	if base != cursorCacheKey("session", "acct-1", "gpt-5.6-luna") {
		t.Fatal("the key must be stable for the same session, account and model")
	}
	// An empty session is the signal that disables the cache; scoping must not turn it
	// into a usable key.
	if cursorCacheKey("", "acct-1", "gpt-5.6-luna") != "" {
		t.Fatal("an empty session must stay empty")
	}
}
