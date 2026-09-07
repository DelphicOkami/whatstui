# Roadmap

A personal WhatsApp TUI client built in Go with [Bubble Tea](https://github.com/charmbracelet/bubbletea) on top of [whatsmeow](https://github.com/tulir/whatsmeow).

## Guiding constraint: engine-agnostic core

The TUI must never import `whatsmeow` directly. All chat interaction goes through a `MessagingEngine` interface so a future Signal/Matrix/XMPP engine can be dropped in without touching UI code. WhatsApp-specific concepts (JIDs, presence semantics, link previews) get normalised at the engine boundary.

```
internal/
  engine/           # interface + shared domain types (Chat, Message, Contact, Event)
  engine/whatsapp/  # whatsmeow-backed implementation
  ui/               # Bubble Tea app — depends on engine, never on whatsmeow
  store/            # message/chat persistence (engine-neutral schema)
```

The interface is the contract; everything else is replaceable.

### `MessagingEngine` (target shape)

- **Lifecycle:** `Connect(ctx)`, `Disconnect()`, `PairingFlow(ctx) <-chan PairingEvent` (QR codes, paired, error)
- **Reads:** `Chats(ctx) ([]Chat, error)`, `History(ctx, chatID, before, limit) ([]Message, error)`, `Contacts(ctx) ([]Contact, error)`
- **Writes:** `SendText(ctx, chatID, body) (MessageID, error)`, `StartChat(ctx, identifier) (Chat, error)`, `CreateGroup(ctx, name, members) (Chat, error)`
- **Events:** `Subscribe() <-chan Event` — `MessageReceived`, `MessageStatusChanged`, `PresenceChanged`, `ChatUpdated`, `Disconnected`
- **Capabilities:** `Capabilities() Capabilities` — feature flags so the UI can hide what an engine doesn't support (e.g. Signal has no "presence" in the same shape)

Domain types live in `internal/engine` and are pure data — no `whatsmeow.JID`, no `proto.Message` leakage.

## Milestones

### M0 — Skeleton
- `go mod init`, project layout, baseline `MessagingEngine` interface with no methods filled in
- `cmd/whatstui/main.go` boots an empty Bubble Tea app wired to a stub engine
- CI: `go vet`, `go test ./...`, `golangci-lint`

### M1 — Pairing & connection
- WhatsApp engine: whatsmeow client, SQLite session store via `modernc.org/sqlite` (pure Go, no cgo), QR-code pairing surfaced via `PairingEvent`
- TUI: pairing screen renders QR (qrterminal or similar), transitions to a placeholder "connected" view
- Reconnect on disconnect with backoff

### M2 — Chat list & receive
- Engine emits `MessageReceived` and `ChatUpdated` events
- TUI: chat list view (Bubble Tea list component), unread counts, sort by last activity
- Local store persists chats + last message snippet so the list survives restarts

### M3 — Conversation view & send
- Open chat → message scrollback (viewport component), input field, `SendText` round-trip
- Outgoing message status (sent / delivered / read) reflected via `MessageStatusChanged`

### M4 — History sync
- Backfill on first connect; paginated lazy-load on scroll-up
- Persistent message store keyed by engine-neutral `MessageID`

### M5 — Groups & new chats
- `CreateGroup`, group member display, group-only metadata behind capability flag
- "Start new chat" modal: phone number / contact picker → `StartChat`

### M6 — Polish
- Search across chats/messages, keybinding help, theming, notifications (desktop bell), basic media awareness (show "[image]" placeholder even if not rendered)

## Out of scope (for now)

- Voice/video calls
- Inline media rendering (terminal image protocols can come later — keep media as opaque attachments in the engine model)
- Multi-account
- Encryption-at-rest beyond what whatsmeow's session store provides

## Open questions

- Storage: single SQLite DB shared by engine session + app message store, or separate? Leaning separate so engines stay swappable.
- Event bus: per-engine channel vs. a central pub/sub? Probably channel on the engine, fan-out in the UI layer.
- Threading model: one goroutine per engine event consumer, or funnel everything through `tea.Cmd`? Need to prototype in M2.

## Decisions

- **SQLite driver:** `modernc.org/sqlite` (pure Go, easy cross-compile) for both whatsmeow's session store and the app's message store.
- **Capability model:** `Capabilities()` flags on the engine. The UI hides features an engine can't support; we accept that not every WhatsApp feature will land in v1, and that's the price of keeping the core swappable.
