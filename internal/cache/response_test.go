package cache

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildResponseKey(t *testing.T) {
	input := ResponseKey{Model: "model", Temperature: json.RawMessage(`0`), MaxTokens: 100, Messages: json.RawMessage(`[{"role":"user","content":"hello"}]`)}
	first, err := BuildResponseKey(input)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := BuildResponseKey(input)
	input.MaxTokens++
	third, _ := BuildResponseKey(input)
	if first != second || first == third || len(first) != 64 {
		t.Fatalf("keys = %q, %q, %q", first, second, third)
	}
}

func TestBuildResponseKeyPreservesLargeJSONNumbers(t *testing.T) {
	first, err := BuildResponseKey(ResponseKey{Model: "model", Messages: json.RawMessage(`[{"value":9007199254740992}]`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildResponseKey(ResponseKey{Model: "model", Messages: json.RawMessage(`[{"value":9007199254740993}]`)})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("distinct JSON integers produced the same key")
	}
}

func TestBuildResponseKeyCanonicalJSON(t *testing.T) {
	first, err := BuildResponseKey(ResponseKey{
		Model: "model", Temperature: json.RawMessage(`0.70`),
		Messages: json.RawMessage(`[{"role":"user","content":"hello"}]`),
		Tools:    json.RawMessage(`[{"function":{"name":"tool","parameters":{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"number"}}}}}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildResponseKey(ResponseKey{
		Model: "model", Temperature: json.RawMessage(`0.7`),
		Messages: json.RawMessage(`[ { "content": "hello", "role": "user" } ]`),
		Tools:    json.RawMessage(`[{"function":{"parameters":{"properties":{"a":{"type":"number"},"b":{"type":"string"}},"type":"object"},"name":"tool"}}]`),
	})
	if err != nil || first != second {
		t.Fatalf("keys = %q, %q; error = %v", first, second, err)
	}
}

func TestResponseCacheLRUTTLAndAdmission(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	cache := NewResponseCache(2, time.Minute)
	cache.now = func() time.Time { return now }
	if cache.Put("stream", []byte("x"), true, false) || cache.Put("tools", []byte("x"), false, true) {
		t.Fatal("ineligible response cached")
	}
	cache.Put("a", []byte("a"), false, false)
	cache.Put("b", []byte("b"), false, false)
	if value, ok := cache.Get("a"); !ok || string(value) != "a" {
		t.Fatalf("Get(a) = %q, %v", value, ok)
	}
	cache.Put("c", []byte("c"), false, false)
	if _, ok := cache.Get("b"); ok {
		t.Fatal("least-recent entry was not evicted")
	}
	now = now.Add(time.Minute)
	if _, ok := cache.Get("a"); ok {
		t.Fatal("expired entry returned")
	}
	metrics := cache.Metrics()
	if metrics.Hits != 1 || metrics.Misses != 2 || metrics.HitRatio != 1.0/3.0 {
		t.Fatalf("Metrics() = %#v", metrics)
	}
}

func BenchmarkBuildResponseKey(b *testing.B) {
	input := ResponseKey{
		Model: "gpt-4.1", Temperature: json.RawMessage(`0`), MaxTokens: 1024,
		Messages: json.RawMessage(`[{"role":"user","content":"` + strings.Repeat("prompt ", 1000) + `"}]`),
		Tools:    json.RawMessage(`[{"type":"function","function":{"name":"read","parameters":{"type":"object"}}}]`),
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(input.Messages)))
	for b.Loop() {
		if _, err := BuildResponseKey(input); err != nil {
			b.Fatal(err)
		}
	}
}

func TestResponseCacheCopiesValuesAndIsConcurrent(t *testing.T) {
	cache := NewResponseCache(100, time.Minute)
	value := []byte("value")
	cache.Put("key", value, false, false)
	value[0] = 'X'
	cached, _ := cache.Get("key")
	cached[0] = 'Y'
	again, _ := cache.Get("key")
	if string(again) != "value" {
		t.Fatalf("cached value = %q", again)
	}
	var wait sync.WaitGroup
	for i := range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			cache.Put(string(rune(i)), []byte("x"), false, false)
			cache.Get("key")
		}()
	}
	wait.Wait()
}
