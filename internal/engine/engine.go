// Package engine defines the engine-agnostic contract the TUI talks to.
//
// The TUI must never import a specific backend (whatsmeow, etc.). All chat
// interaction goes through MessagingEngine so a future Signal/Matrix/XMPP
// engine can be dropped in without touching UI code. Backend-specific concepts
// are normalised into the domain types in this package at the engine boundary.
package engine

import (
	"context"
	"time"
)

// ChatID is an engine-neutral identifier for a chat.
type ChatID string

// MessageID is an engine-neutral identifier for a message.
type MessageID string

// ContactID is an engine-neutral identifier for a contact.
type ContactID string

// Chat is a normalised conversation.
//
// Alias is a user-set local nickname that overrides Title for display. It's
// persisted by the engine but never sent to the server — engines without a
// SetLocalAlias capability simply leave it empty. Title remains the
// authoritative server-side name so the UI can show the original alongside,
// or restore it if the alias is cleared.
type Chat struct {
	ID           ChatID
	Title        string
	Alias        string
	IsGroup      bool
	LastActivity time.Time
	UnreadCount  int
	LastSnippet  string
}

// Contact is a normalised user/peer.
type Contact struct {
	ID   ContactID
	Name string
}

// GroupParticipant is a member of a group chat. Engines that don't model
// admin status should leave IsAdmin false; the UI must not rely on it for
// correctness, only for display. Name is whatever the engine considers the
// best human label (full name, push name, JID user as last resort).
type GroupParticipant struct {
	ID      ContactID
	Name    string
	IsAdmin bool
}

// MessageStatus reflects delivery state of an outgoing message.
type MessageStatus int

const (
	StatusUnknown MessageStatus = iota
	StatusPending
	StatusSent
	StatusDelivered
	StatusRead
	StatusFailed
)

// Message is a normalised message.
type Message struct {
	ID        MessageID
	ChatID    ChatID
	SenderID  ContactID
	Body      string
	Timestamp time.Time
	FromMe    bool
	Status    MessageStatus
	// Attachment is non-nil when the message carries media. Body still holds
	// a human-readable fallback (e.g. "[image]") so engines/UIs that don't
	// know how to render media degrade gracefully.
	Attachment *Attachment
}

// AttachmentKind enumerates the engine-neutral media types. M6+ only models
// images; other kinds are placeholders for future work.
type AttachmentKind int

const (
	AttachmentNone AttachmentKind = iota
	AttachmentImage
)

// Attachment describes a downloadable media payload attached to a message.
// Path is the absolute path to a locally cached file, or "" if the engine
// hasn't finished downloading yet — callers should treat an empty Path as
// "not ready, try again on the next event for this message ID".
type Attachment struct {
	Kind   AttachmentKind
	MIME   string
	Path   string
	Width  int
	Height int
}

// Capabilities advertises which optional features an engine supports.
type Capabilities struct {
	Presence     bool
	Groups       bool
	HistorySync  bool
	LinkPreviews bool
	ReadReceipts bool
}

// Event is the sum type emitted on the engine's event channel.
type Event interface{ event() }

// MessageReceived fires when a new inbound message arrives.
type MessageReceived struct{ Message Message }

// MessageStatusChanged fires when an outgoing message's status changes.
type MessageStatusChanged struct {
	ID     MessageID
	Status MessageStatus
}

// PresenceChanged fires when a contact's presence updates.
type PresenceChanged struct {
	ContactID ContactID
	Online    bool
	LastSeen  time.Time
}

// ChatUpdated fires when a chat's metadata changes (title, last activity, unread).
type ChatUpdated struct{ Chat Chat }

// Disconnected fires when the engine loses its connection.
type Disconnected struct{ Err error }

// HistoryBackfilled fires when the engine has finished persisting an
// asynchronously requested batch of historical messages for a chat.
// AddedCount is the number of new rows written to the engine's local
// message store (already-known messages aren't double-counted). The UI
// should re-query History after seeing this.
type HistoryBackfilled struct {
	ChatID     ChatID
	AddedCount int
}

// SyncProgress reports progress through the post-pairing initial sync.
// Stage is a short human-readable label ("history", "app state", "done").
// Percent is 0–100. Done=true indicates initial sync is complete and the UI
// can hide any progress affordance.
type SyncProgress struct {
	Stage   string
	Percent int
	Done    bool
}

func (MessageReceived) event()      {}
func (MessageStatusChanged) event() {}
func (PresenceChanged) event()      {}
func (ChatUpdated) event()          {}
func (Disconnected) event()         {}
func (HistoryBackfilled) event()    {}
func (SyncProgress) event()         {}

// PairingEvent is emitted during the pairing flow (QR strings, paired, errors).
type PairingEvent struct {
	QR     string
	Paired bool
	Err    error
}

// MessagingEngine is the contract the TUI depends on. Concrete backends live
// under internal/engine/<name>. M0 declares the surface; later milestones
// fill in the WhatsApp implementation.
type MessagingEngine interface {
	// Lifecycle.
	Connect(ctx context.Context) error
	Disconnect() error
	Logout(ctx context.Context) error
	PairingFlow(ctx context.Context) <-chan PairingEvent

	// Reads.
	Chats(ctx context.Context) ([]Chat, error)
	History(ctx context.Context, chatID ChatID, before time.Time, limit int) ([]Message, error)
	Contacts(ctx context.Context) ([]Contact, error)

	// RequestHistory asks the backend to deliver up to count older messages
	// for chatID, beyond what's already in the engine's local store. The
	// call returns immediately; the response arrives asynchronously and is
	// signalled via a HistoryBackfilled event after the engine has
	// persisted what came back. Engines that don't support server-side
	// history pulls (Capabilities.HistorySync == false) may return nil
	// without doing anything.
	RequestHistory(ctx context.Context, chatID ChatID, count int) error

	// GroupParticipants returns the membership of a group chat. Engines
	// without group support (Capabilities.Groups == false) may return nil
	// without error; callers should gate on the capability before calling.
	GroupParticipants(ctx context.Context, chatID ChatID) ([]GroupParticipant, error)

	// Writes.
	SendText(ctx context.Context, chatID ChatID, body string) (MessageID, error)
	StartChat(ctx context.Context, identifier string) (Chat, error)
	CreateGroup(ctx context.Context, name string, members []ContactID) (Chat, error)

	// Events & capabilities.
	Subscribe() <-chan Event
	Capabilities() Capabilities
}
