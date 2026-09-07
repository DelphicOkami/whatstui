// Package chatstore persists the engine-neutral chat list so the TUI's left
// pane survives restarts. Schema is a single chats table keyed by ChatID.
package chatstore

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	// Blank import to allow interaction through std library's *sql.DB *sql.Tx and *sql.Rows
	_ "modernc.org/sqlite"

	"github.com/delphicokami/whatstui/internal/engine"
)

const schema = `
CREATE TABLE IF NOT EXISTS chats (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    is_group INTEGER NOT NULL,
    last_activity_unix_ns INTEGER NOT NULL,
    unread_count INTEGER NOT NULL,
    last_snippet TEXT NOT NULL,
    alias TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS chats_last_activity ON chats(last_activity_unix_ns DESC);
`

// migrateAliasColumn adds the alias column to databases created before it
// existed. SQLite has no IF NOT EXISTS for ALTER TABLE ADD COLUMN, so we
// detect the "duplicate column" error and treat it as success.
func migrateAliasColumn(db *sql.DB) error {
	_, err := db.Exec(`ALTER TABLE chats ADD COLUMN alias TEXT NOT NULL DEFAULT ''`)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "duplicate column") {
		return nil
	}
	return err
}

// Store wraps a SQLite database holding the chat list.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the chat store at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open chat store: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate chat store: %w", err)
	}
	if err := migrateAliasColumn(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate chat store (alias): %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// List returns all chats sorted by most recent activity first.
func (s *Store) List() ([]engine.Chat, error) {
	rows, err := s.db.Query(`SELECT id, title, is_group, last_activity_unix_ns, unread_count, last_snippet, alias
        FROM chats ORDER BY last_activity_unix_ns DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []engine.Chat
	for rows.Next() {
		var (
			id, title, snippet, alias string
			isGroup                   int
			ns                        int64
			unread                    int
		)
		if err := rows.Scan(&id, &title, &isGroup, &ns, &unread, &snippet, &alias); err != nil {
			return nil, err
		}
		out = append(out, engine.Chat{
			ID:           engine.ChatID(id),
			Title:        title,
			Alias:        alias,
			IsGroup:      isGroup != 0,
			LastActivity: nsToTime(ns),
			UnreadCount:  unread,
			LastSnippet:  snippet,
		})
	}
	return out, rows.Err()
}

// nsToTime restores a time.Time from a nanosecond column, preserving the
// zero-value sentinel (we store 0 for chats with no activity, so callers
// can use LastActivity.IsZero() to detect "no history").
func nsToTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Get returns a single chat by ID. Returns (Chat{}, false, nil) if missing.
func (s *Store) Get(id engine.ChatID) (engine.Chat, bool, error) {
	row := s.db.QueryRow(`SELECT id, title, is_group, last_activity_unix_ns, unread_count, last_snippet, alias
        FROM chats WHERE id = ?`, string(id))
	var (
		gotID, title, snippet, alias string
		isGroup                      int
		ns                           int64
		unread                       int
	)
	err := row.Scan(&gotID, &title, &isGroup, &ns, &unread, &snippet, &alias)
	if errors.Is(err, sql.ErrNoRows) {
		return engine.Chat{}, false, nil
	}
	if err != nil {
		return engine.Chat{}, false, err
	}
	return engine.Chat{
		ID:           engine.ChatID(gotID),
		Title:        title,
		Alias:        alias,
		IsGroup:      isGroup != 0,
		LastActivity: nsToTime(ns),
		UnreadCount:  unread,
		LastSnippet:  snippet,
	}, true, nil
}

// Upsert writes the chat row, replacing any existing entry with the same ID.
func (s *Store) Upsert(c engine.Chat) error {
	isGroup := 0
	if c.IsGroup {
		isGroup = 1
	}
	_, err := s.db.Exec(`INSERT INTO chats (id, title, is_group, last_activity_unix_ns, unread_count, last_snippet)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            title = excluded.title,
            is_group = excluded.is_group,
            last_activity_unix_ns = excluded.last_activity_unix_ns,
            unread_count = excluded.unread_count,
            last_snippet = excluded.last_snippet`,
		string(c.ID), c.Title, isGroup, timeToNs(c.LastActivity), c.UnreadCount, c.LastSnippet)
	return err
}

// timeToNs is the inverse of nsToTime: zero time stores as 0 so the
// IsZero() round-trip survives across persistence.
func timeToNs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// SetTitle updates only the title for an existing row. No-op if the chat
// isn't present yet.
func (s *Store) SetTitle(id engine.ChatID, title string) error {
	_, err := s.db.Exec(`UPDATE chats SET title = ? WHERE id = ?`, title, string(id))
	return err
}

// ClearUnread zeroes the unread counter for a chat.
func (s *Store) ClearUnread(id engine.ChatID) error {
	_, err := s.db.Exec(`UPDATE chats SET unread_count = 0 WHERE id = ?`, string(id))
	return err
}

// SetAlias writes a user-set local alias for a chat. Pass an empty string to
// clear it. No-op if the chat row doesn't exist yet — the engine inserts on
// first activity, and a missing row means we'd be aliasing a chat we don't
// know about.
func (s *Store) SetAlias(id engine.ChatID, alias string) error {
	_, err := s.db.Exec(`UPDATE chats SET alias = ? WHERE id = ?`, alias, string(id))
	return err
}
