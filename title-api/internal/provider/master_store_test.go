package provider

import (
	"testing"
	"time"
)

func TestMasterStoreExpiresAndEvicts(t *testing.T) {
	store := newMasterStore(20*time.Millisecond, 1)
	first := store.put([]byte("first"))
	second := store.put([]byte("second"))
	if _, ok := store.get(first); ok {
		t.Fatal("oldest master was not evicted")
	}
	if body, ok := store.get(second); !ok || string(body) != "second" {
		t.Fatalf("second master = %q, %v", body, ok)
	}
	time.Sleep(30 * time.Millisecond)
	if _, ok := store.get(second); ok {
		t.Fatal("master did not expire")
	}
}
