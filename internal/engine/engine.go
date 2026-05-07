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
type Chat struct {
	ID           ChatID
	Title        string
	IsGroup      bool
	LastActivity time.Time
	UnreadCount  int
}

// Contact is a normalised user/peer.
type Contact struct {
	ID   ContactID
	Name string
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

func (MessageReceived) event()      {}
func (MessageStatusChanged) event() {}
func (PresenceChanged) event()      {}
func (ChatUpdated) event()          {}
func (Disconnected) event()         {}

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
	PairingFlow(ctx context.Context) <-chan PairingEvent

	// Reads.
	Chats(ctx context.Context) ([]Chat, error)
	History(ctx context.Context, chatID ChatID, before time.Time, limit int) ([]Message, error)
	Contacts(ctx context.Context) ([]Contact, error)

	// Writes.
	SendText(ctx context.Context, chatID ChatID, body string) (MessageID, error)
	StartChat(ctx context.Context, identifier string) (Chat, error)
	CreateGroup(ctx context.Context, name string, members []ContactID) (Chat, error)

	// Events & capabilities.
	Subscribe() <-chan Event
	Capabilities() Capabilities
}
