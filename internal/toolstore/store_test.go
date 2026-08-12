package toolstore

import (
	"testing"
)

func TestPutGetRoundTripsByContentHash(t *testing.T) {
	store := New(0)
	body := "line one\nline two\nline three\n"
	id := ID(body)
	if id == "" || len(id) != 64 {
		t.Fatalf("ID() = %q, want a 64-hex-char sha256", id)
	}
	store.Put(id, "Read", "toolu_1", body)
	got, ok := store.Get(id)
	if !ok || got != body {
		t.Fatalf("Get(%q) = %q, %v; want the original body", id, got, ok)
	}
}

func TestGetIsARefreshNotADuplicate(t *testing.T) {
	store := New(0)
	body := "same body"
	id := ID(body)
	store.Put(id, "Bash", "toolu_1", body)
	store.Put(id, "Bash", "toolu_2", body)
	if store.Size() != 1 {
		t.Fatalf("Size() = %d, want 1 (dedupe by hash)", store.Size())
	}
}

func TestEvictionDropsOldestByFetch(t *testing.T) {
	store := New(2) // cap: 2 bytes — keep at most two single-byte bodies
	a, b, c := "a", "b", "c"
	store.Put(ID(a), "", "t1", a)
	store.Put(ID(b), "", "t2", b)
	if got, ok := store.Get(ID(a)); !ok || got != a {
		t.Fatal("first entry should be present before the cap is hit")
	}
	// Touch a (refresh LRU), then add c — b is now the LRU tail and must go.
	store.Get(ID(a))
	store.Put(ID(c), "", "t3", c)
	if _, ok := store.Get(ID(b)); ok {
		t.Fatal("least-recently-fetched entry survived eviction")
	}
	if got, ok := store.Get(ID(a)); !ok || got != a {
		t.Fatal("recently-touched entry was evicted")
	}
	if got, ok := store.Get(ID(c)); !ok || got != c {
		t.Fatal("newest entry was evicted")
	}
}

func TestEvictionCapsBytesAcrossManyEntries(t *testing.T) {
	store := New(100)
	for index := 0; index < 50; index++ {
		body := string(rune('a' + index%26))
		store.Put(ID(body), "", "t", body)
	}
	if store.Size() > 100 {
		t.Fatalf("store grew past its byte cap: %d entries", store.Size())
	}
}

func TestMissReturnsFalse(t *testing.T) {
	store := New(0)
	if _, ok := store.Get("deadbeef"); ok {
		t.Fatal("Get on an unknown id reported a hit")
	}
}
