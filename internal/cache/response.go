package cache

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultResponseTTL     = 15 * time.Minute
	DefaultResponseEntries = 10_000
)

type ResponseKey struct {
	Model               string          `json:"model"`
	Temperature         json.RawMessage `json:"temperature"`
	TopP                json.RawMessage `json:"top_p"`
	Seed                json.RawMessage `json:"seed"`
	Stop                json.RawMessage `json:"stop"`
	ResponseFormat      json.RawMessage `json:"response_format"`
	N                   int             `json:"n"`
	PresencePenalty     json.RawMessage `json:"presence_penalty"`
	FrequencyPenalty    json.RawMessage `json:"frequency_penalty"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Messages            json.RawMessage `json:"messages"`
	Tools               json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
}

func HashResponseKey(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func BuildResponseKey(input ResponseKey) (string, error) {
	canonical := struct {
		Model               string `json:"model"`
		Temperature         any    `json:"temperature"`
		TopP                any    `json:"top_p"`
		Seed                any    `json:"seed"`
		Stop                any    `json:"stop"`
		ResponseFormat      any    `json:"response_format"`
		N                   int    `json:"n"`
		PresencePenalty     any    `json:"presence_penalty"`
		FrequencyPenalty    any    `json:"frequency_penalty"`
		MaxTokens           int    `json:"max_tokens"`
		MaxCompletionTokens int    `json:"max_completion_tokens"`
		Messages            any    `json:"messages"`
		Tools               any    `json:"tools"`
		ToolChoice          any    `json:"tool_choice"`
	}{Model: input.Model, N: input.N, MaxTokens: input.MaxTokens, MaxCompletionTokens: input.MaxCompletionTokens}
	fields := []struct {
		raw    json.RawMessage
		target *any
	}{
		{input.Temperature, &canonical.Temperature}, {input.TopP, &canonical.TopP},
		{input.Seed, &canonical.Seed}, {input.Stop, &canonical.Stop},
		{input.ResponseFormat, &canonical.ResponseFormat},
		{input.PresencePenalty, &canonical.PresencePenalty}, {input.FrequencyPenalty, &canonical.FrequencyPenalty},
		{input.Messages, &canonical.Messages}, {input.Tools, &canonical.Tools}, {input.ToolChoice, &canonical.ToolChoice},
	}
	for _, field := range fields {
		if len(field.raw) == 0 || string(field.raw) == "null" {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(field.raw))
		decoder.UseNumber()
		if err := decoder.Decode(field.target); err != nil {
			return "", err
		}
		*field.target = normalizeJSONNumbers(*field.target)
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func normalizeJSONNumbers(value any) any {
	switch value := value.(type) {
	case json.Number:
		text := string(value)
		if strings.ContainsAny(text, ".eE") {
			if number, err := strconv.ParseFloat(text, 64); err == nil {
				return number
			}
		}
		return value
	case []any:
		for index := range value {
			value[index] = normalizeJSONNumbers(value[index])
		}
	case map[string]any:
		for key := range value {
			value[key] = normalizeJSONNumbers(value[key])
		}
	}
	return value
}

type ResponseCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	lru        list.List
	maxEntries int
	ttl        time.Duration
	now        func() time.Time
	hits       atomic.Uint64
	misses     atomic.Uint64
}

type responseEntry struct {
	key       string
	value     []byte
	expiresAt time.Time
}

type ResponseMetrics struct {
	Hits     uint64  `json:"hits"`
	Misses   uint64  `json:"misses"`
	HitRatio float64 `json:"hit_ratio"`
	Entries  int     `json:"entries"`
}

func NewResponseCache(maxEntries int, ttl time.Duration) *ResponseCache {
	if maxEntries <= 0 {
		maxEntries = DefaultResponseEntries
	}
	if ttl <= 0 {
		ttl = DefaultResponseTTL
	}
	return &ResponseCache{entries: make(map[string]*list.Element, maxEntries), maxEntries: maxEntries, ttl: ttl, now: time.Now}
}

func (c *ResponseCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element := c.entries[key]
	if element == nil {
		c.misses.Add(1)
		return nil, false
	}
	entry := element.Value.(*responseEntry)
	if !entry.expiresAt.After(c.now()) {
		c.remove(element)
		c.misses.Add(1)
		return nil, false
	}
	c.lru.MoveToFront(element)
	c.hits.Add(1)
	return append([]byte(nil), entry.value...), true
}

func (c *ResponseCache) Put(key string, value []byte, stream, hasToolCalls bool) bool {
	if stream || hasToolCalls || key == "" || value == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		entry := element.Value.(*responseEntry)
		entry.value = append(entry.value[:0], value...)
		entry.expiresAt = c.now().Add(c.ttl)
		c.lru.MoveToFront(element)
		return true
	}
	entry := &responseEntry{key: key, value: append([]byte(nil), value...), expiresAt: c.now().Add(c.ttl)}
	c.entries[key] = c.lru.PushFront(entry)
	if c.lru.Len() > c.maxEntries {
		c.remove(c.lru.Back())
	}
	return true
}

func (c *ResponseCache) Metrics() ResponseMetrics {
	hits, misses := c.hits.Load(), c.misses.Load()
	ratio := 0.0
	if total := hits + misses; total > 0 {
		ratio = float64(hits) / float64(total)
	}
	c.mu.Lock()
	entries := len(c.entries)
	c.mu.Unlock()
	return ResponseMetrics{Hits: hits, Misses: misses, HitRatio: ratio, Entries: entries}
}

func (c *ResponseCache) remove(element *list.Element) {
	if element == nil {
		return
	}
	entry := element.Value.(*responseEntry)
	delete(c.entries, entry.key)
	c.lru.Remove(element)
}
