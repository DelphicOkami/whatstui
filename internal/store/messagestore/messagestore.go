// Package messagestore persists engine-neutral messages so conversation
// history survives restarts and the TUI can lazy-load older pages on scroll.
// Schema is a single messages table keyed by MessageID.
package messagestore

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/delphicokami/whatstui/internal/engine"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id TEXT PRIMARY KEY,
    chat_id TEXT NOT NULL,
    sender_id TEXT NOT NULL,
    body TEXT NOT NULL,
    timestamp_unix_ns INTEGER NOT NULL,
    from_me INTEGER NOT NULL,
    status INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_chat_time ON messages(chat_id, timestamp_unix_ns);
`

// attachmentMigration adds attachment columns idempotently. Older databases
// created before M6's image support only have the base columns; running the
// ALTERs once on open is cheaper than a versioned migration system. Errors
// from "duplicate column" are swallowed inside applyAttachmentMigration.
var attachmentMigration = []string{
	`ALTER TABLE messages ADD COLUMN attachment_kind INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE messages ADD COLUMN attachment_mime TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE messages ADD COLUMN attachment_path TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE messages ADD COLUMN attachment_width INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE messages ADD COLUMN attachment_height INTEGER NOT NULL DEFAULT 0`,
}

// Store wraps a SQLite database holding persisted messages.
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the message store at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open message store: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate message store: %w", err)
	}
	for _, stmt := range attachmentMigration {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			_ = db.Close()
			return nil, fmt.Errorf("migrate attachments: %w", err)
		}
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

// Upsert writes a message row, replacing any existing entry with the same ID.
func (s *Store) Upsert(m engine.Message) error {
	fromMe := 0
	if m.FromMe {
		fromMe = 1
	}
	aKind, aMime, aPath, aW, aH := attachmentCols(m.Attachment)
	_, err := s.db.Exec(`INSERT INTO messages
        (id, chat_id, sender_id, body, timestamp_unix_ns, from_me, status,
         attachment_kind, attachment_mime, attachment_path, attachment_width, attachment_height)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            chat_id = excluded.chat_id,
            sender_id = excluded.sender_id,
            body = excluded.body,
            timestamp_unix_ns = excluded.timestamp_unix_ns,
            from_me = excluded.from_me,
            status = excluded.status,
            attachment_kind = excluded.attachment_kind,
            attachment_mime = excluded.attachment_mime,
            attachment_path = excluded.attachment_path,
            attachment_width = excluded.attachment_width,
            attachment_height = excluded.attachment_height`,
		string(m.ID), string(m.ChatID), string(m.SenderID), m.Body,
		m.Timestamp.UnixNano(), fromMe, int(m.Status),
		aKind, aMime, aPath, aW, aH)
	return err
}

// InsertIfNew writes a message row only if no row with the same ID exists.
// Returns true when a new row was inserted, false if it was a duplicate.
// Used by history-sync ingestion so we never overwrite a more-current live
// row with stale backfill data.
func (s *Store) InsertIfNew(m engine.Message) (bool, error) {
	fromMe := 0
	if m.FromMe {
		fromMe = 1
	}
	aKind, aMime, aPath, aW, aH := attachmentCols(m.Attachment)
	res, err := s.db.Exec(`INSERT OR IGNORE INTO messages
        (id, chat_id, sender_id, body, timestamp_unix_ns, from_me, status,
         attachment_kind, attachment_mime, attachment_path, attachment_width, attachment_height)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(m.ID), string(m.ChatID), string(m.SenderID), m.Body,
		m.Timestamp.UnixNano(), fromMe, int(m.Status),
		aKind, aMime, aPath, aW, aH)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetAttachmentPath updates only the on-disk path for an existing message's
// attachment row. Used by the engine after async media download completes,
// so a restart sees the cached file without re-downloading.
func (s *Store) SetAttachmentPath(id engine.MessageID, path string) error {
	_, err := s.db.Exec(
		`UPDATE messages SET attachment_path = ? WHERE id = ?`,
		path, string(id))
	return err
}

func attachmentCols(a *engine.Attachment) (kind int, mime, path string, w, h int) {
	if a == nil {
		return 0, "", "", 0, 0
	}
	return int(a.Kind), a.MIME, a.Path, a.Width, a.Height
}

// SetStatus updates the delivery status for a message ID. To match the UI's
// non-downgrade rule, this only writes when the new status is strictly higher
// than the stored one (StatusUnknown is always overwritten).
func (s *Store) SetStatus(id engine.MessageID, status engine.MessageStatus) error {
	_, err := s.db.Exec(`UPDATE messages SET status = ?
        WHERE id = ? AND (status < ? OR status = 0)`,
		int(status), string(id), int(status))
	return err
}

// History returns up to limit messages for chatID strictly older than before,
// in ascending timestamp order (oldest first) ready for display. A zero
// before value means "from the most recent backwards".
func (s *Store) History(chatID engine.ChatID, before time.Time, limit int) ([]engine.Message, error) {
	if limit <= 0 {
		return nil, nil
	}
	useBound := !before.IsZero()
	bound := before.UnixNano()

	rows, err := s.db.Query(`SELECT id, chat_id, sender_id, body, timestamp_unix_ns, from_me, status,
            attachment_kind, attachment_mime, attachment_path, attachment_width, attachment_height
        FROM messages
        WHERE chat_id = ? AND (? = 0 OR timestamp_unix_ns < ?)
        ORDER BY timestamp_unix_ns DESC
        LIMIT ?`,
		string(chatID), boolToInt(useBound), bound, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var desc []engine.Message
	for rows.Next() {
		var (
			id, cid, sid, body, aMime, aPath string
			ns                                int64
			fromMe, status, aKind, aW, aH    int
		)
		if err := rows.Scan(&id, &cid, &sid, &body, &ns, &fromMe, &status,
			&aKind, &aMime, &aPath, &aW, &aH); err != nil {
			return nil, err
		}
		m := engine.Message{
			ID:        engine.MessageID(id),
			ChatID:    engine.ChatID(cid),
			SenderID:  engine.ContactID(sid),
			Body:      body,
			Timestamp: time.Unix(0, ns),
			FromMe:    fromMe != 0,
			Status:    engine.MessageStatus(status),
		}
		if aKind != int(engine.AttachmentNone) {
			m.Attachment = &engine.Attachment{
				Kind:   engine.AttachmentKind(aKind),
				MIME:   aMime,
				Path:   aPath,
				Width:  aW,
				Height: aH,
			}
		}
		desc = append(desc, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Reverse to ascending.
	for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
		desc[i], desc[j] = desc[j], desc[i]
	}
	return desc, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
