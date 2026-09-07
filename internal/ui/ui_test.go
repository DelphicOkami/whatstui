package ui_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/delphicokami/whatstui/internal/engine"
	"github.com/delphicokami/whatstui/internal/engine/stub"
	"github.com/delphicokami/whatstui/internal/ui"
)

func TestModelView(t *testing.T) {
	m := ui.New(stub.New())
	if got := m.View(); !strings.Contains(got, "whatstui") {
		t.Fatalf("View missing app name: %q", got)
	}
}

func TestModelHandlesWindowSize(t *testing.T) {
	var m tea.Model = ui.New(stub.New())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if got := m.View(); !strings.Contains(got, "whatstui") {
		t.Fatalf("View missing app name after WindowSize: %q", got)
	}
}

// TestFooterContainsModalHints sanity-checks the M3 footer wording so the
// nvim-style key map stays discoverable.
func TestFooterContainsModalHints(t *testing.T) {
	m := ui.New(stub.New())
	view := m.View()
	if !strings.Contains(view, "q: quit") {
		t.Fatalf("footer missing quit hint: %q", view)
	}
}

// fakeEngine is a minimal engine.MessagingEngine that lets tests pre-seed
// chats and history responses to drive the M4 backfill flow.
type fakeEngine struct {
	mu       sync.Mutex
	chats    []engine.Chat
	pages    map[engine.ChatID][][]engine.Message
	pairing  chan engine.PairingEvent
	events   chan engine.Event
	historyN int
	requestN int
	caps     engine.Capabilities

	// M5 hooks.
	startChatResult engine.Chat
	startChatErr    error
	startChatN      int
	lastStartChatID string
	participants    map[engine.ChatID][]engine.GroupParticipant
	participantsN   int
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		pages:   map[engine.ChatID][][]engine.Message{},
		pairing: make(chan engine.PairingEvent, 1),
		events:  make(chan engine.Event, 16),
		caps:    engine.Capabilities{HistorySync: true},
	}
}

func (f *fakeEngine) Connect(context.Context) error    { return nil }
func (f *fakeEngine) Disconnect() error                { return nil }
func (f *fakeEngine) Logout(context.Context) error     { return nil }
func (f *fakeEngine) PairingFlow(context.Context) <-chan engine.PairingEvent { return f.pairing }
func (f *fakeEngine) Chats(context.Context) ([]engine.Chat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]engine.Chat(nil), f.chats...), nil
}
func (f *fakeEngine) Contacts(context.Context) ([]engine.Contact, error) { return nil, nil }
func (f *fakeEngine) History(_ context.Context, id engine.ChatID, _ time.Time, _ int) ([]engine.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyN++
	pages := f.pages[id]
	if len(pages) == 0 {
		return nil, nil
	}
	page := pages[0]
	f.pages[id] = pages[1:]
	return page, nil
}
func (f *fakeEngine) RequestHistory(_ context.Context, _ engine.ChatID, _ int) error {
	f.mu.Lock()
	f.requestN++
	f.mu.Unlock()
	return nil
}
func (f *fakeEngine) SendText(context.Context, engine.ChatID, string) (engine.MessageID, error) {
	return "", nil
}
func (f *fakeEngine) StartChat(_ context.Context, id string) (engine.Chat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startChatN++
	f.lastStartChatID = id
	if f.startChatErr != nil {
		return engine.Chat{}, f.startChatErr
	}
	if f.startChatResult.ID != "" {
		f.chats = append(f.chats, f.startChatResult)
		return f.startChatResult, nil
	}
	return engine.Chat{}, nil
}
func (f *fakeEngine) GroupParticipants(_ context.Context, id engine.ChatID) ([]engine.GroupParticipant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.participantsN++
	return append([]engine.GroupParticipant(nil), f.participants[id]...), nil
}
func (f *fakeEngine) CreateGroup(context.Context, string, []engine.ContactID) (engine.Chat, error) {
	return engine.Chat{}, nil
}
func (f *fakeEngine) Subscribe() <-chan engine.Event { return f.events }
func (f *fakeEngine) Capabilities() engine.Capabilities {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caps
}

// runCmd drains a tea.Cmd and feeds every produced message back through
// Update. Cmds that block longer than 100ms (the engine event/pairing
// readers, which are intentionally infinite in production) are dropped so
// tests can make progress.
func runCmd(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(100 * time.Millisecond):
		return m
	}
	switch v := msg.(type) {
	case nil:
		return m
	case tea.BatchMsg:
		for _, c := range v {
			m = runCmd(t, m, c)
		}
		return m
	default:
		var next tea.Cmd
		m, next = m.Update(msg)
		return runCmd(t, m, next)
	}
}

// connectFake takes a fresh model through the connect+pairing dance so the
// chats list is populated and the model is in stateConnected.
func connectFake(t *testing.T, f *fakeEngine) tea.Model {
	t.Helper()
	var m tea.Model = ui.New(f)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	f.pairing <- engine.PairingEvent{Paired: true}
	m = runCmd(t, m, m.(ui.Model).Init())
	return m
}

func TestOpenChatLoadsHistoryFromEngine(t *testing.T) {
	f := newFakeEngine()
	chatID := engine.ChatID("alice")
	f.chats = []engine.Chat{{ID: chatID, Title: "Alice", LastActivity: time.Unix(1000, 0)}}
	f.pages[chatID] = [][]engine.Message{{
		{ID: "m1", ChatID: chatID, Body: "older one", Timestamp: time.Unix(900, 0)},
		{ID: "m2", ChatID: chatID, Body: "older two", Timestamp: time.Unix(950, 0)},
	}}

	m := connectFake(t, f)
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	if f.historyN != 1 {
		t.Fatalf("History calls = %d, want 1", f.historyN)
	}
	view := m.View()
	if !strings.Contains(view, "older one") || !strings.Contains(view, "older two") {
		t.Fatalf("view missing history-loaded messages: %q", view)
	}
}

// Without server-side HistorySync capability, scroll-up on a chat whose
// local store is drained should mark the chat exhausted after one empty
// prepend rather than retrying.
func TestExhaustedWithoutHistorySyncCapability(t *testing.T) {
	f := newFakeEngine()
	f.caps = engine.Capabilities{}
	chatID := engine.ChatID("alice")
	f.chats = []engine.Chat{{ID: chatID, Title: "Alice", LastActivity: time.Unix(1000, 0)}}
	f.pages[chatID] = [][]engine.Message{
		{{ID: "m1", ChatID: chatID, Body: "only", Timestamp: time.Unix(900, 0)}},
		nil, // prepend returns nothing
	}

	m := connectFake(t, f)
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)
	if f.historyN != 1 {
		t.Fatalf("initial History calls = %d, want 1", f.historyN)
	}

	// First scroll-up: empty local prepend.
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = runCmd(t, m, cmd)
	if f.requestN != 0 {
		t.Fatalf("RequestHistory calls = %d, want 0 (no HistorySync capability)", f.requestN)
	}

	// Second scroll-up: exhausted, should not refire.
	prevN := f.historyN
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	_ = runCmd(t, m, cmd)
	if f.historyN != prevN {
		t.Fatalf("History calls after exhausted = %d, want %d", f.historyN, prevN)
	}
}

// With HistorySync, an empty local prepend should escalate to RequestHistory
// and a subsequent HistoryBackfilled event should trigger a re-read of the
// local store, picking up the freshly persisted older rows.
func TestScrollUpEscalatesToServerBackfill(t *testing.T) {
	f := newFakeEngine()
	f.caps = engine.Capabilities{HistorySync: true}
	chatID := engine.ChatID("alice")
	f.chats = []engine.Chat{{ID: chatID, Title: "Alice", LastActivity: time.Unix(1000, 0)}}
	f.pages[chatID] = [][]engine.Message{
		{{ID: "m1", ChatID: chatID, Body: "local one", Timestamp: time.Unix(900, 0)}},
		nil, // first prepend: empty -> escalate to server
		{{ID: "m0", ChatID: chatID, Body: "from server", Timestamp: time.Unix(500, 0)}}, // re-read after backfill
	}

	m := connectFake(t, f)
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = runCmd(t, m, cmd)
	if f.requestN != 1 {
		t.Fatalf("RequestHistory calls = %d, want 1", f.requestN)
	}

	// Simulate the engine emitting HistoryBackfilled after persisting.
	f.events <- engine.HistoryBackfilled{ChatID: chatID, AddedCount: 1}
	// nextEvent is parked in a goroutine from Init; pump it manually by
	// posting an engineMsg through Update via a fresh nextEvent cycle.
	// runCmd's 100ms timeout will let the parked reader return.
	m = runCmd(t, m, m.(ui.Model).Init())
	// Drain in case the backfill re-fetch landed.
	for i := 0; i < 4 && !strings.Contains(m.View(), "from server"); i++ {
		m = runCmd(t, m, nil)
	}

	if !strings.Contains(m.View(), "from server") {
		t.Fatalf("view missing backfilled message: %q", m.View())
	}
}

// TestNewChatPromptOpensAndStartsChat exercises the M5 'n' affordance:
// pressing 'n' from the chat list activates the prompt, typing fills it,
// and pressing enter calls StartChat with the resulting identifier. On
// success the new chat appears in the list and is selected.
func TestNewChatPromptOpensAndStartsChat(t *testing.T) {
	f := newFakeEngine()
	f.startChatResult = engine.Chat{
		ID:           engine.ChatID("447700900123@s.whatsapp.net"),
		Title:        "New Friend",
		LastActivity: time.Unix(2000, 0),
	}

	m := connectFake(t, f)

	// 'n' from chat-list normal mode activates the prompt.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if !strings.Contains(m.View(), "new chat") {
		t.Fatalf("prompt didn't open: %q", m.View())
	}

	// Type a few characters into the textinput.
	for _, r := range "+447700900123" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	// Enter triggers StartChat. runCmd drains the resulting cmd.
	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	if f.startChatN != 1 {
		t.Fatalf("StartChat calls = %d, want 1", f.startChatN)
	}
	if f.lastStartChatID != "+447700900123" {
		t.Fatalf("StartChat identifier = %q, want %q", f.lastStartChatID, "+447700900123")
	}
	if !strings.Contains(m.View(), "New Friend") {
		t.Fatalf("new chat not shown in list: %q", m.View())
	}
}

// TestNewChatPromptEscCancels confirms esc closes the prompt without
// firing StartChat and that 'n' / 'q' typed into the field don't trigger
// normal-mode commands.
func TestNewChatPromptEscCancels(t *testing.T) {
	f := newFakeEngine()
	m := connectFake(t, f)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	// 'q' inside the prompt must NOT quit; it should be inserted as text.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if f.startChatN != 0 {
		t.Fatalf("StartChat unexpectedly called %d times", f.startChatN)
	}
	if strings.Contains(m.View(), "new chat ›") {
		t.Fatalf("prompt still visible after esc: %q", m.View())
	}
}

// TestMemberOverlayShowsParticipants verifies that 'm' in a group chat
// loads and renders the participant list, behind the Capabilities.Groups
// flag.
func TestMemberOverlayShowsParticipants(t *testing.T) {
	f := newFakeEngine()
	f.caps = engine.Capabilities{Groups: true}
	chatID := engine.ChatID("group@g.us")
	f.chats = []engine.Chat{{ID: chatID, Title: "Crew", IsGroup: true, LastActivity: time.Unix(1000, 0)}}
	f.participants = map[engine.ChatID][]engine.GroupParticipant{
		chatID: {
			{ID: "alice", Name: "Alice", IsAdmin: true},
			{ID: "bob", Name: "Bob"},
		},
	}

	m := connectFake(t, f)
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	// 'm' toggles the overlay and triggers GroupParticipants.
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	m = runCmd(t, m, cmd)

	if f.participantsN != 1 {
		t.Fatalf("GroupParticipants calls = %d, want 1", f.participantsN)
	}
	view := m.View()
	if !strings.Contains(view, "Alice") || !strings.Contains(view, "Bob") {
		t.Fatalf("members overlay missing names: %q", view)
	}
	if !strings.Contains(view, "Members (2)") {
		t.Fatalf("members header missing count: %q", view)
	}

	// Toggling again hides the overlay (and shouldn't refetch).
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	if strings.Contains(m.View(), "Members (2)") {
		t.Fatalf("overlay didn't close: %q", m.View())
	}
	// Reopen — should hit cache, no second fetch.
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	_ = runCmd(t, m, cmd)
	if f.participantsN != 1 {
		t.Fatalf("GroupParticipants refetched = %d, want 1 (cached)", f.participantsN)
	}
}

// TestSearchListFiltersByTitle: '/' in the chat list opens a textinput,
// typing filters chats by title (case-insensitive), and 'q' typed into
// the search field is treated as text rather than firing the quit command.
func TestSearchListFiltersByTitle(t *testing.T) {
	f := newFakeEngine()
	f.chats = []engine.Chat{
		{ID: "a", Title: "Alice", LastActivity: time.Unix(3, 0)},
		{ID: "b", Title: "Bob", LastActivity: time.Unix(2, 0)},
		{ID: "c", Title: "Carol", LastActivity: time.Unix(1, 0)},
	}

	m := connectFake(t, f)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	for _, r := range "bo" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	view := m.View()
	if !strings.Contains(view, "Bob") {
		t.Fatalf("filtered list should keep Bob: %q", view)
	}
	if strings.Contains(view, "Alice") || strings.Contains(view, "Carol") {
		t.Fatalf("filtered list should drop Alice/Carol: %q", view)
	}

	// Esc clears the filter and restores all chats.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	view = m.View()
	if !(strings.Contains(view, "Alice") && strings.Contains(view, "Bob") && strings.Contains(view, "Carol")) {
		t.Fatalf("esc should restore full chat list: %q", view)
	}
}

// TestSearchConversationFilters confirms that '/' inside an open chat
// filters the rendered messages to those whose body contains the query.
func TestSearchConversationFilters(t *testing.T) {
	f := newFakeEngine()
	chatID := engine.ChatID("alice")
	f.chats = []engine.Chat{{ID: chatID, Title: "Alice", LastActivity: time.Unix(1000, 0)}}
	f.pages[chatID] = [][]engine.Message{{
		{ID: "m1", ChatID: chatID, Body: "hello there", Timestamp: time.Unix(900, 0)},
		{ID: "m2", ChatID: chatID, Body: "general kenobi", Timestamp: time.Unix(950, 0)},
	}}

	m := connectFake(t, f)
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	for _, r := range "kenobi" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	view := m.View()
	if !strings.Contains(view, "kenobi") {
		t.Fatalf("matching message missing: %q", view)
	}
	if strings.Contains(view, "hello there") {
		t.Fatalf("non-matching message should be filtered out: %q", view)
	}
}

// TestHelpToggleShowsCheatSheet confirms '?' surfaces the cheat-sheet line
// and a second press hides it again.
func TestHelpToggleShowsCheatSheet(t *testing.T) {
	f := newFakeEngine()
	f.chats = []engine.Chat{{ID: "a", Title: "Alice", LastActivity: time.Unix(1, 0)}}
	m := connectFake(t, f)

	if strings.Contains(m.View(), "hide help") {
		t.Fatalf("cheat-sheet visible before '?': %q", m.View())
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if !strings.Contains(m.View(), "hide help") {
		t.Fatalf("cheat-sheet missing after '?': %q", m.View())
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	if strings.Contains(m.View(), "hide help") {
		t.Fatalf("cheat-sheet not hidden after second '?': %q", m.View())
	}
}

// TestMemberOverlayGatedByCapability ensures 'm' is a no-op when the
// engine doesn't advertise group support, even on a chat marked as a group.
func TestMemberOverlayGatedByCapability(t *testing.T) {
	f := newFakeEngine()
	f.caps = engine.Capabilities{} // no Groups
	chatID := engine.ChatID("group@g.us")
	f.chats = []engine.Chat{{ID: chatID, Title: "Crew", IsGroup: true, LastActivity: time.Unix(1000, 0)}}

	m := connectFake(t, f)
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	_ = runCmd(t, m, cmd)

	if f.participantsN != 0 {
		t.Fatalf("GroupParticipants called %d times despite missing capability", f.participantsN)
	}
}
