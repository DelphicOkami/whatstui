package chatstore_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/delphicokami/whatstui/internal/engine"
	"github.com/delphicokami/whatstui/internal/store/chatstore"
)

func openTestStore(t *testing.T) *chatstore.Store {
	t.Helper()
	s, err := chatstore.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpsertAndListSortsByActivity(t *testing.T) {
	s := openTestStore(t)

	older := engine.Chat{ID: "a", Title: "Alice", LastActivity: time.Unix(100, 0), LastSnippet: "hi"}
	newer := engine.Chat{ID: "b", Title: "Bob", IsGroup: true, LastActivity: time.Unix(200, 0), LastSnippet: "hey", UnreadCount: 2}
	if err := s.Upsert(older); err != nil {
		t.Fatalf("Upsert older: %v", err)
	}
	if err := s.Upsert(newer); err != nil {
		t.Fatalf("Upsert newer: %v", err)
	}

	got, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "b" || got[1].ID != "a" {
		t.Fatalf("order = %v, want [b a]", []engine.ChatID{got[0].ID, got[1].ID})
	}
	if !got[0].IsGroup || got[0].UnreadCount != 2 || got[0].LastSnippet != "hey" {
		t.Fatalf("group row not preserved: %+v", got[0])
	}
}

func TestUpsertReplacesExisting(t *testing.T) {
	s := openTestStore(t)
	c := engine.Chat{ID: "a", Title: "Alice", LastActivity: time.Unix(100, 0), LastSnippet: "hi"}
	if err := s.Upsert(c); err != nil {
		t.Fatal(err)
	}
	c.Title = "Alice 2"
	c.LastSnippet = "yo"
	c.UnreadCount = 3
	if err := s.Upsert(c); err != nil {
		t.Fatal(err)
	}

	got, _, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Alice 2" || got.LastSnippet != "yo" || got.UnreadCount != 3 {
		t.Fatalf("not replaced: %+v", got)
	}
}

func TestSetTitleAndClearUnread(t *testing.T) {
	s := openTestStore(t)
	if err := s.Upsert(engine.Chat{ID: "a", Title: "raw", LastActivity: time.Unix(1, 0), UnreadCount: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTitle("a", "Pretty Name"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearUnread("a"); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Pretty Name" || got.UnreadCount != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestGetMissing(t *testing.T) {
	s := openTestStore(t)
	_, ok, err := s.Get("nope")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("ok should be false for missing chat")
	}
}
