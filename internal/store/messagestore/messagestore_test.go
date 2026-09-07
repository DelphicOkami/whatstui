package messagestore_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/delphicokami/whatstui/internal/engine"
	"github.com/delphicokami/whatstui/internal/store/messagestore"
)

func openTestStore(t *testing.T) *messagestore.Store {
	t.Helper()
	s, err := messagestore.Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func msg(id engine.MessageID, chat engine.ChatID, ts int64, body string) engine.Message {
	return engine.Message{
		ID: id, ChatID: chat, SenderID: "x",
		Body: body, Timestamp: time.Unix(ts, 0),
		Status: engine.StatusSent,
	}
}

func TestHistoryReturnsAscending(t *testing.T) {
	s := openTestStore(t)
	if err := s.Upsert(msg("3", "c", 300, "c")); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(msg("1", "c", 100, "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(msg("2", "c", 200, "b")); err != nil {
		t.Fatal(err)
	}
	got, err := s.History("c", time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "1" || got[2].ID != "3" {
		t.Fatalf("order = %v", got)
	}
}

func TestHistoryPaginationAndLimit(t *testing.T) {
	s := openTestStore(t)
	for i := int64(1); i <= 5; i++ {
		if err := s.Upsert(msg(engine.MessageID(string(rune('0'+i))), "c", i*100, "x")); err != nil {
			t.Fatal(err)
		}
	}
	// Most recent 2: ids "4","5".
	page1, err := s.History("c", time.Time{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || page1[0].ID != "4" || page1[1].ID != "5" {
		t.Fatalf("page1 = %v", page1)
	}
	// Older than oldest of page1: ids "2","3".
	page2, err := s.History("c", page1[0].Timestamp, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID != "2" || page2[1].ID != "3" {
		t.Fatalf("page2 = %v", page2)
	}
}

func TestHistoryFiltersByChat(t *testing.T) {
	s := openTestStore(t)
	_ = s.Upsert(msg("a", "c1", 100, "a"))
	_ = s.Upsert(msg("b", "c2", 200, "b"))
	got, err := s.History("c1", time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("got %v", got)
	}
}

func TestUpsertReplaces(t *testing.T) {
	s := openTestStore(t)
	_ = s.Upsert(msg("a", "c", 100, "first"))
	_ = s.Upsert(msg("a", "c", 100, "second"))
	got, _ := s.History("c", time.Time{}, 10)
	if len(got) != 1 || got[0].Body != "second" {
		t.Fatalf("got %v", got)
	}
}

func TestSetStatusDoesNotDowngrade(t *testing.T) {
	s := openTestStore(t)
	m := msg("a", "c", 100, "x")
	m.Status = engine.StatusRead
	if err := s.Upsert(m); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("a", engine.StatusSent); err != nil {
		t.Fatal(err)
	}
	got, _ := s.History("c", time.Time{}, 1)
	if got[0].Status != engine.StatusRead {
		t.Fatalf("status = %v, want Read", got[0].Status)
	}
}

func TestSetStatusUpgrades(t *testing.T) {
	s := openTestStore(t)
	m := msg("a", "c", 100, "x")
	m.Status = engine.StatusSent
	_ = s.Upsert(m)
	if err := s.SetStatus("a", engine.StatusRead); err != nil {
		t.Fatal(err)
	}
	got, _ := s.History("c", time.Time{}, 1)
	if got[0].Status != engine.StatusRead {
		t.Fatalf("status = %v, want Read", got[0].Status)
	}
}
