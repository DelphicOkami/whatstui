// Package ui hosts the Bubble Tea application. It depends on
// internal/engine and must never import a backend directly.
package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mdp/qrterminal/v3"
	"rsc.io/qr"

	"github.com/delphicokami/whatstui/internal/engine"
	imagerender "github.com/delphicokami/whatstui/internal/ui/image"
)

type state int

const (
	stateInitializing state = iota
	statePairing
	stateConnected
	stateDisconnected
	stateError
)

type pane int

const (
	paneList pane = iota
	paneConversation
)

type mode int

const (
	modeNormal mode = iota
	modeInsert
)

// unreadClearer is an optional capability surfaced by engines that track
// unread counts. The whatsapp engine satisfies this; the stub does not.
type unreadClearer interface {
	ClearUnread(engine.ChatID) error
}

// localAliaser is an optional capability for engines that can persist a
// user-set local nickname for a chat. The alias is display-only and never
// sent to the server. Engines that don't implement this make 'R' a no-op.
type localAliaser interface {
	SetLocalAlias(engine.ChatID, string) error
}

// Model is the root Bubble Tea model.
type Model struct {
	eng     engine.MessagingEngine
	pairing <-chan engine.PairingEvent
	events  <-chan engine.Event

	state state
	qr    string
	err   error

	loggingOut bool

	width  int
	height int

	chats       []engine.Chat
	chatIndex   map[engine.ChatID]int
	selectedIdx int
	// listOffset is the index of the first chat row drawn in the list pane.
	// Adjusted on navigation so selectedIdx stays inside the visible window.
	listOffset int

	pane     pane
	mode     mode
	openChat engine.ChatID

	viewport viewport.Model
	input    textarea.Model

	// M5: new-chat prompt. Lives above the chat list when active. Owns
	// every key press while active (see handleKey) so typing stays inside
	// the field and doesn't fire normal-mode commands like 'j'/'q'.
	newChat       textinput.Model
	newChatActive bool
	newChatBusy   bool
	newChatErr    string

	// Local-alias prompt (R). Lives above the chat list while active and
	// owns every key press, identical to the new-chat prompt. aliasTarget
	// is the ChatID being renamed — captured at open time so a list refresh
	// or selection move mid-edit doesn't retarget the rename.
	aliasInput  textinput.Model
	aliasActive bool
	aliasBusy   bool
	aliasErr    string
	aliasTarget engine.ChatID

	// M5: group-member overlay state. members caches the participant
	// list per chat between toggles so we only fetch once. showingMembers
	// is per-session — we don't persist whether the panel was open.
	members         map[engine.ChatID][]engine.GroupParticipant
	membersLoading  bool
	membersErr      string
	showingMembers  bool

	// M6: shared search affordance. searchInput owns key input while
	// searchActive is true; searchQuery is the committed (or live) query
	// applied as a filter to the active pane. The pane that owned focus
	// when '/' was pressed is recorded in searchPane so esc returns the
	// user where they came from.
	searchInput   textinput.Model
	searchActive  bool
	searchQuery   string
	searchPane    pane

	// M6: cheat-sheet line above the footer. Toggled with '?'.
	help bool

	// imageIDs maps a message's MessageID to the kitty graphics placement
	// it was transmitted with. Presence here means "the bytes are already
	// in the terminal; just emit a placeholder grid of the recorded size".
	// Absence means "not transmitted yet (or terminal doesn't support
	// graphics)". The rows/cols are decided at transmit time and must
	// match the placeholder grid we render with — kitty crops otherwise.
	// nextImageID is the monotonic allocator; we reserve 0 as the "no id"
	// sentinel and wrap below 0xFFFFFF to stay inside truecolour fg-encoding.
	imageIDs    map[engine.MessageID]imagePlacement
	nextImageID uint32

	// messages buffers messages for the open conversation, seeded from the
	// engine's persistent store on first open and topped up by live events.
	messages map[engine.ChatID][]engine.Message
	// msgChat lets MessageStatusChanged find the chat owning a message ID.
	msgChat map[engine.MessageID]engine.ChatID

	// History pagination state. historyLoaded marks chats whose initial
	// backfill has completed; historyExhausted marks chats with no older
	// pages left in the store. historyLoading + loadingChat gate concurrent
	// requests and drive the "Loading older messages…" banner.
	historyLoaded    map[engine.ChatID]bool
	historyExhausted map[engine.ChatID]bool
	historyLoading   bool
	// loadingKind distinguishes a quick local-store read from a slower
	// server backfill so the banner copy can adapt and so a stale
	// HistoryBackfilled event for the wrong chat doesn't clear the banner.
	loadingKind loadKind
	loadingChat engine.ChatID
	// awaitingBackfill is true while we've sent a RequestHistory and are
	// waiting for the matching HistoryBackfilled event (or the timeout).
	awaitingBackfill bool

	// Initial-sync progress, surfaced as a bar above the main panes while
	// active. syncActive flips false once the engine reports Done; we don't
	// re-show the bar on subsequent reconnects.
	syncActive  bool
	syncStage   string
	syncPercent int
}

// imagePlacement records the kitty graphics ID and the cell grid size we
// transmitted it with. Render emits exactly rows × cols placeholder cells
// so the placeholder grid matches kitty's virtual placement (set via
// r=/c= in Transmit) — mismatched sizes produce cropping or repeated
// edges.
type imagePlacement struct {
	id   uint32
	rows int
	cols int
}

type loadKind int

const (
	loadNone loadKind = iota
	loadLocal
	loadServer
)

// historyPageSize is how many older messages a single local read pulls.
const historyPageSize = 20

// historyServerPageSize is how many older messages we ask the backend for
// when local history runs out. whatsmeow recommends 50 per on-demand pull.
const historyServerPageSize = 50

// historyBackfillTimeout caps how long we keep the loading banner up
// waiting for a HistoryBackfilled response before giving up and marking
// the chat exhausted.
const historyBackfillTimeout = 15 * time.Second

// New builds a root model wired to the given engine. The engine should be
// constructed but not yet connected — Init kicks off Connect.
func New(eng engine.MessagingEngine) Model {
	ta := textarea.New()
	ta.Placeholder = "Type a message…"
	ta.Prompt = "│ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 4096
	ta.SetHeight(3)
	// Vim feel: plain Enter sends (intercepted at Update level), Ctrl+J or
	// Alt+Enter inserts a newline.
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"))

	vp := viewport.New(0, 0)

	nc := textinput.New()
	nc.Placeholder = "phone (e.g. +447700900123) or JID"
	nc.Prompt = "new chat › "
	nc.CharLimit = 64

	ai := textinput.New()
	ai.Placeholder = "leave empty to clear"
	ai.Prompt = "alias › "
	ai.CharLimit = 64

	si := textinput.New()
	si.Placeholder = "search…"
	si.Prompt = "/ "
	si.CharLimit = 128

	return Model{
		eng:              eng,
		pairing:          eng.PairingFlow(context.Background()),
		events:           eng.Subscribe(),
		state:            stateInitializing,
		chatIndex:        make(map[engine.ChatID]int),
		messages:         make(map[engine.ChatID][]engine.Message),
		msgChat:          make(map[engine.MessageID]engine.ChatID),
		historyLoaded:    make(map[engine.ChatID]bool),
		historyExhausted: make(map[engine.ChatID]bool),
		imageIDs:         make(map[engine.MessageID]imagePlacement),
		viewport:         vp,
		input:            ta,
		newChat:          nc,
		aliasInput:       ai,
		searchInput:      si,
		members:          make(map[engine.ChatID][]engine.GroupParticipant),
	}
}

type connectMsg struct{ err error }

type pairingMsg struct {
	evt    engine.PairingEvent
	closed bool
}

type engineMsg struct {
	evt    engine.Event
	closed bool
}

type logoutMsg struct{ err error }

type chatsLoadedMsg struct {
	chats []engine.Chat
	err   error
}

type sendResultMsg struct {
	id  engine.MessageID
	err error
}

type historyLoadedMsg struct {
	chatID  engine.ChatID
	msgs    []engine.Message
	prepend bool
	err     error
}

// backfillRequestedMsg is emitted after RequestHistory returns. The UI then
// flips into the "loadServer" loading state and waits for either a matching
// HistoryBackfilled engine event or backfillTimeoutMsg.
type backfillRequestedMsg struct {
	chatID engine.ChatID
	err    error
}

type backfillTimeoutMsg struct {
	chatID engine.ChatID
}

// startChatMsg lands when the engine has finished resolving a "new chat"
// request — either with a real Chat or with an error to surface to the UI.
type startChatMsg struct {
	chat engine.Chat
	err  error
}

// participantsLoadedMsg lands when GroupParticipants returns. The chatID
// is preserved so a stale response (user toggled to a different chat in
// the meantime) can be ignored.
type participantsLoadedMsg struct {
	chatID engine.ChatID
	people []engine.GroupParticipant
	err    error
}

// Init kicks off Connect and starts listening on the pairing/event channels.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.connectCmd(), m.nextPairing(), m.nextEvent(), textarea.Blink)
}

func (m Model) connectCmd() tea.Cmd {
	return func() tea.Msg {
		return connectMsg{err: m.eng.Connect(context.Background())}
	}
}

func (m Model) loadChatsCmd() tea.Cmd {
	return func() tea.Msg {
		c, err := m.eng.Chats(context.Background())
		return chatsLoadedMsg{chats: c, err: err}
	}
}

func (m Model) nextPairing() tea.Cmd {
	ch := m.pairing
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return pairingMsg{closed: true}
		}
		return pairingMsg{evt: evt}
	}
}

func (m Model) nextEvent() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return engineMsg{closed: true}
		}
		return engineMsg{evt: evt}
	}
}

func (m Model) logoutCmd() tea.Cmd {
	return func() tea.Msg {
		return logoutMsg{err: m.eng.Logout(context.Background())}
	}
}

func (m Model) historyCmd(chatID engine.ChatID, before time.Time, limit int, prepend bool) tea.Cmd {
	eng := m.eng
	return func() tea.Msg {
		msgs, err := eng.History(context.Background(), chatID, before, limit)
		return historyLoadedMsg{chatID: chatID, msgs: msgs, prepend: prepend, err: err}
	}
}

func (m Model) requestBackfillCmd(chatID engine.ChatID) tea.Cmd {
	eng := m.eng
	return func() tea.Msg {
		err := eng.RequestHistory(context.Background(), chatID, historyServerPageSize)
		return backfillRequestedMsg{chatID: chatID, err: err}
	}
}

func (m Model) backfillTimeoutCmd(chatID engine.ChatID) tea.Cmd {
	return tea.Tick(historyBackfillTimeout, func(time.Time) tea.Msg {
		return backfillTimeoutMsg{chatID: chatID}
	})
}

// setAliasMsg lands when the engine has finished persisting (or rejecting) a
// local alias edit. The chat refresh itself arrives separately as a
// ChatUpdated event from the engine.
type setAliasMsg struct {
	chatID engine.ChatID
	err    error
}

func (m Model) setAliasCmd(id engine.ChatID, alias string) tea.Cmd {
	eng := m.eng
	return func() tea.Msg {
		aliaser, ok := eng.(localAliaser)
		if !ok {
			return setAliasMsg{chatID: id, err: errors.New("engine does not support local aliases")}
		}
		return setAliasMsg{chatID: id, err: aliaser.SetLocalAlias(id, alias)}
	}
}

func (m Model) startChatCmd(identifier string) tea.Cmd {
	eng := m.eng
	return func() tea.Msg {
		c, err := eng.StartChat(context.Background(), identifier)
		return startChatMsg{chat: c, err: err}
	}
}

func (m Model) participantsCmd(chatID engine.ChatID) tea.Cmd {
	eng := m.eng
	return func() tea.Msg {
		people, err := eng.GroupParticipants(context.Background(), chatID)
		return participantsLoadedMsg{chatID: chatID, people: people, err: err}
	}
}

// bellCmd writes the ASCII BEL byte to stderr. Wired to inbound (not-from-me)
// MessageReceived events. We don't try to gate on terminal focus — most
// terminals don't reliably report focus to a CLI app, and the user explicitly
// wanted always-on. Terminals that have audible-bell disabled will swallow it.
func bellCmd() tea.Cmd {
	return func() tea.Msg {
		_, _ = os.Stderr.Write([]byte{'\a'})
		return nil
	}
}

func (m Model) sendCmd(chatID engine.ChatID, body string) tea.Cmd {
	eng := m.eng
	return func() tea.Msg {
		id, err := eng.SendText(context.Background(), chatID, body)
		return sendResultMsg{id: id, err: err}
	}
}

// Update satisfies tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizePanes()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case connectMsg:
		if msg.err != nil {
			m.state = stateError
			m.err = msg.err
		}
		return m, nil

	case pairingMsg:
		if msg.closed {
			return m, nil
		}
		switch {
		case msg.evt.Err != nil:
			m.state = stateError
			m.err = msg.evt.Err
		case msg.evt.Paired:
			m.state = stateConnected
			m.qr = ""
			return m, tea.Batch(m.nextPairing(), m.loadChatsCmd())
		case msg.evt.QR != "":
			m.state = statePairing
			m.qr = msg.evt.QR
		}
		return m, m.nextPairing()

	case chatsLoadedMsg:
		if msg.err == nil {
			m.chats = msg.chats
			m.rebuildIndex()
		}
		return m, nil

	case engineMsg:
		if msg.closed {
			return m, nil
		}
		switch e := msg.evt.(type) {
		case engine.Disconnected:
			if e.Err != nil {
				m.state = stateError
				m.err = e.Err
			} else {
				m.state = stateDisconnected
			}
		case engine.ChatUpdated:
			m.upsertChat(e.Chat)
		case engine.MessageReceived:
			m.ensureImageTransmitted(e.Message)
			m.appendMessage(e.Message)
			if !e.Message.FromMe {
				return m, tea.Batch(m.nextEvent(), bellCmd())
			}
		case engine.MessageStatusChanged:
			m.updateStatus(e.ID, e.Status)
		case engine.SyncProgress:
			m.syncStage = e.Stage
			m.syncPercent = e.Percent
			m.syncActive = !e.Done
		case engine.HistoryBackfilled:
			if m.awaitingBackfill && m.loadingChat == e.ChatID && e.ChatID == m.openChat {
				// Server delivered. Re-read the local store to pick up the
				// new rows; applyHistoryLoaded will splice + clear loading.
				m.awaitingBackfill = false
				msgs := m.messages[e.ChatID]
				var before time.Time
				if len(msgs) > 0 {
					before = msgs[0].Timestamp
				}
				return m, tea.Batch(m.nextEvent(), m.historyCmd(e.ChatID, before, historyPageSize, true))
			}
		}
		return m, m.nextEvent()

	case historyLoadedMsg:
		cmd := m.applyHistoryLoaded(msg)
		return m, cmd

	case backfillRequestedMsg:
		// RequestHistory returned. If it errored or the engine had nothing
		// to anchor on (no oldestInfo), bail out and mark exhausted so
		// further scroll-ups don't keep firing.
		if msg.err != nil || !m.awaitingBackfill || m.loadingChat != msg.chatID {
			m.awaitingBackfill = false
			m.historyLoading = false
			m.loadingKind = loadNone
			m.loadingChat = ""
			if msg.err != nil {
				m.err = msg.err
				m.historyExhausted[msg.chatID] = true
			}
			return m, nil
		}
		return m, m.backfillTimeoutCmd(msg.chatID)

	case backfillTimeoutMsg:
		if m.awaitingBackfill && m.loadingChat == msg.chatID {
			m.awaitingBackfill = false
			m.historyLoading = false
			m.loadingKind = loadNone
			m.loadingChat = ""
			m.historyExhausted[msg.chatID] = true
		}
		return m, nil

	case setAliasMsg:
		m.aliasBusy = false
		if msg.err != nil {
			m.aliasErr = msg.err.Error()
			return m, nil
		}
		m.aliasActive = false
		m.aliasErr = ""
		m.aliasTarget = ""
		m.aliasInput.Reset()
		return m, nil

	case startChatMsg:
		m.newChatBusy = false
		if msg.err != nil {
			m.newChatErr = msg.err.Error()
			return m, nil
		}
		m.newChatActive = false
		m.newChatErr = ""
		m.newChat.Reset()
		m.upsertChat(msg.chat)
		// Select the newly created chat so the user can immediately open it.
		if idx, ok := m.chatIndex[msg.chat.ID]; ok {
			m.selectedIdx = idx
		}
		return m, m.loadChatsCmd()

	case participantsLoadedMsg:
		m.membersLoading = false
		if msg.err != nil {
			m.membersErr = msg.err.Error()
			return m, nil
		}
		m.members[msg.chatID] = msg.people
		m.membersErr = ""
		// Newly arrived names mean per-message sender labels in the open
		// group conversation can now resolve — re-render so the placeholders
		// (or last-resort JID-user fallbacks) get replaced with real names.
		if msg.chatID == m.openChat {
			m.refreshConversation()
		}
		return m, nil

	case sendResultMsg:
		// The optimistic emit + receipts drive UI state; we only surface
		// hard send failures here so the user sees something went wrong.
		if msg.err != nil {
			m.err = msg.err
		}
		return m, nil

	case logoutMsg:
		m.loggingOut = false
		if msg.err != nil {
			m.state = stateError
			m.err = msg.err
			return m, nil
		}
		return m, m.connectCmd()
	}

	// Drive child components when a non-key, non-engine message arrives
	// (e.g. cursor blink ticks for the textarea / new-chat input).
	if m.state == stateConnected && m.searchActive {
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}
	if m.state == stateConnected && m.aliasActive {
		var cmd tea.Cmd
		m.aliasInput, cmd = m.aliasInput.Update(msg)
		return m, cmd
	}
	if m.state == stateConnected && m.newChatActive {
		var cmd tea.Cmd
		m.newChat, cmd = m.newChat.Update(msg)
		return m, cmd
	}
	if m.state == stateConnected && m.pane == paneConversation {
		var cmds []tea.Cmd
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		cmds = append(cmds, cmd)
		if m.mode == modeInsert {
			m.input, cmd = m.input.Update(msg)
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Search prompt: textinput owns every key. Live-filters as you type
	// (the rendered query is just searchInput.Value()). Esc clears the
	// committed filter and closes; enter commits and closes (filter
	// stays applied; pressing '/' again reopens with empty input).
	if m.state == stateConnected && m.searchActive {
		switch msg.String() {
		case "esc":
			m.searchActive = false
			m.searchQuery = ""
			m.searchInput.Reset()
			return m, nil
		case "enter":
			m.searchQuery = strings.TrimSpace(m.searchInput.Value())
			m.searchActive = false
			m.searchInput.Blur()
			if m.searchPane == paneConversation {
				m.refreshConversation()
				m.viewport.GotoBottom()
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		// Live re-render so the conversation viewport reflects the filter
		// while the user is still typing.
		if m.searchPane == paneConversation {
			m.refreshConversation()
		}
		return m, cmd
	}

	// New-chat prompt: textinput owns every key while active. Esc cancels,
	// enter submits. Without this, alphabetic chars typed into the prompt
	// would also fire normal-mode commands like 'q' (quit) or 'j' (nav).
	if m.state == stateConnected && m.aliasActive {
		switch msg.String() {
		case "esc":
			m.aliasActive = false
			m.aliasErr = ""
			m.aliasTarget = ""
			m.aliasInput.Reset()
			return m, nil
		case "enter":
			if m.aliasBusy || m.aliasTarget == "" {
				return m, nil
			}
			alias := strings.TrimSpace(m.aliasInput.Value())
			m.aliasBusy = true
			m.aliasErr = ""
			return m, m.setAliasCmd(m.aliasTarget, alias)
		}
		var cmd tea.Cmd
		m.aliasInput, cmd = m.aliasInput.Update(msg)
		return m, cmd
	}

	if m.state == stateConnected && m.newChatActive {
		switch msg.String() {
		case "esc":
			m.newChatActive = false
			m.newChatErr = ""
			m.newChat.Reset()
			return m, nil
		case "enter":
			id := strings.TrimSpace(m.newChat.Value())
			if id == "" || m.newChatBusy {
				return m, nil
			}
			m.newChatBusy = true
			m.newChatErr = ""
			return m, m.startChatCmd(id)
		}
		var cmd tea.Cmd
		m.newChat, cmd = m.newChat.Update(msg)
		return m, cmd
	}

	// Insert mode in the conversation pane: textarea owns most keys.
	if m.state == stateConnected && m.pane == paneConversation && m.mode == modeInsert {
		switch msg.String() {
		case "esc":
			m.mode = modeNormal
			m.input.Blur()
			return m, nil
		case "enter":
			body := strings.TrimSpace(m.input.Value())
			if body == "" {
				return m, nil
			}
			m.input.Reset()
			return m, m.sendCmd(m.openChat, body)
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	// Normal mode (pairing/list/conversation).
	switch msg.String() {
	case "ctrl+c":
		_ = m.eng.Disconnect()
		return m, tea.Quit
	case "q":
		_ = m.eng.Disconnect()
		return m, tea.Quit
	case "L":
		if !m.loggingOut {
			m.loggingOut = true
			m.state = stateInitializing
			m.qr = ""
			m.err = nil
			m.chats = nil
			m.chatIndex = make(map[engine.ChatID]int)
			m.messages = make(map[engine.ChatID][]engine.Message)
			m.msgChat = make(map[engine.MessageID]engine.ChatID)
			m.historyLoaded = make(map[engine.ChatID]bool)
			m.historyExhausted = make(map[engine.ChatID]bool)
			m.historyLoading = false
			m.loadingKind = loadNone
			m.loadingChat = ""
			m.awaitingBackfill = false
			m.selectedIdx = 0
			m.pane = paneList
			m.mode = modeNormal
			return m, m.logoutCmd()
		}
		return m, nil
	}

	if m.state != stateConnected {
		return m, nil
	}

	if m.pane == paneList {
		switch msg.String() {
		case "?":
			m.help = !m.help
			return m, nil
		case "/":
			m.searchActive = true
			m.searchPane = paneList
			m.searchInput.Reset()
			m.searchInput.Focus()
			return m, textinput.Blink
		case "n":
			m.newChatActive = true
			m.newChatErr = ""
			m.newChat.Focus()
			return m, textinput.Blink
		case "R":
			vc := m.visibleChats()
			if len(vc) == 0 || m.selectedIdx >= len(vc) {
				return m, nil
			}
			c := vc[m.selectedIdx]
			m.aliasActive = true
			m.aliasErr = ""
			m.aliasTarget = c.ID
			m.aliasInput.Reset()
			m.aliasInput.SetValue(c.Alias)
			m.aliasInput.CursorEnd()
			m.aliasInput.Focus()
			return m, textinput.Blink
		case "down", "j":
			vc := m.visibleChats()
			if len(vc) > 0 && m.selectedIdx < len(vc)-1 {
				m.selectedIdx++
			}
			m.listOffset = m.clampedListOffset()
		case "up", "k":
			if m.selectedIdx > 0 {
				m.selectedIdx--
			}
			m.listOffset = m.clampedListOffset()
		case "g":
			m.selectedIdx = 0
			m.listOffset = m.clampedListOffset()
		case "G":
			vc := m.visibleChats()
			if len(vc) > 0 {
				m.selectedIdx = len(vc) - 1
			}
			m.listOffset = m.clampedListOffset()
		case "enter", "l", "right":
			if len(m.visibleChats()) > 0 {
				return m, m.openSelectedChat()
			}
		case "tab":
			// Hop into the conversation pane without changing which chat is
			// open — only works when one is already open.
			if m.openChat != "" {
				m.pane = paneConversation
			}
			return m, nil
		}
		return m, nil
	}

	// Conversation pane, normal mode.
	switch msg.String() {
	case "?":
		m.help = !m.help
		return m, nil
	case "/":
		m.searchActive = true
		m.searchPane = paneConversation
		m.searchInput.Reset()
		m.searchInput.Focus()
		return m, textinput.Blink
	case "esc", "h", "left", "tab":
		// Esc also clears any conversation-side search filter so the
		// user lands back in the list with no leftover state. Tab is a
		// non-clearing pane swap.
		if msg.String() != "tab" && m.searchQuery != "" {
			m.searchQuery = ""
			m.refreshConversation()
		}
		m.pane = paneList
		m.showingMembers = false
		return m, nil
	case "m":
		return m, m.toggleMembers()
	case "R":
		if m.openChat == "" {
			return m, nil
		}
		var current string
		if idx, ok := m.chatIndex[m.openChat]; ok {
			current = m.chats[idx].Alias
		}
		m.aliasActive = true
		m.aliasErr = ""
		m.aliasTarget = m.openChat
		m.aliasInput.Reset()
		m.aliasInput.SetValue(current)
		m.aliasInput.CursorEnd()
		m.aliasInput.Focus()
		m.pane = paneList
		return m, textinput.Blink
	case "i", "a":
		m.mode = modeInsert
		m.input.Focus()
		return m, textarea.Blink
	case "j", "down":
		m.viewport.ScrollDown(1)
		return m, nil
	case "k", "up":
		m.viewport.ScrollUp(1)
		return m, m.maybeFetchOlder()
	case "ctrl+d":
		m.viewport.HalfPageDown()
		return m, nil
	case "ctrl+u":
		m.viewport.HalfPageUp()
		return m, m.maybeFetchOlder()
	case "ctrl+f", "pgdown":
		m.viewport.PageDown()
		return m, nil
	case "ctrl+b", "pgup":
		m.viewport.PageUp()
		return m, m.maybeFetchOlder()
	case "g":
		m.viewport.GotoTop()
		return m, m.maybeFetchOlder()
	case "G":
		m.viewport.GotoBottom()
	}
	return m, nil
}

func (m *Model) applyHistoryLoaded(msg historyLoadedMsg) tea.Cmd {
	if msg.err != nil {
		m.historyLoading = false
		m.loadingKind = loadNone
		m.loadingChat = ""
		m.err = msg.err
		return nil
	}

	existing := m.messages[msg.chatID]
	seen := make(map[engine.MessageID]bool, len(existing)+len(msg.msgs))

	var added int
	if msg.prepend {
		for _, x := range existing {
			seen[x.ID] = true
		}
		var older []engine.Message
		for _, x := range msg.msgs {
			if seen[x.ID] {
				continue
			}
			older = append(older, x)
			m.msgChat[x.ID] = msg.chatID
			m.ensureImageTransmitted(x)
		}
		added = len(older)
		if added > 0 {
			merged := make([]engine.Message, 0, added+len(existing))
			merged = append(merged, older...)
			merged = append(merged, existing...)
			m.messages[msg.chatID] = merged
		}
	} else {
		for _, x := range msg.msgs {
			seen[x.ID] = true
			m.msgChat[x.ID] = msg.chatID
			m.ensureImageTransmitted(x)
		}
		var live []engine.Message
		for _, x := range existing {
			if !seen[x.ID] {
				live = append(live, x)
			}
		}
		merged := make([]engine.Message, 0, len(msg.msgs)+len(live))
		merged = append(merged, msg.msgs...)
		merged = append(merged, live...)
		m.messages[msg.chatID] = merged
		m.historyLoaded[msg.chatID] = true
	}

	if msg.chatID == m.openChat {
		oldHeight := m.viewport.TotalLineCount()
		oldOffset := m.viewport.YOffset
		m.refreshConversation()
		if msg.prepend {
			delta := m.viewport.TotalLineCount() - oldHeight
			m.viewport.SetYOffset(oldOffset + delta)
		} else {
			m.viewport.GotoBottom()
		}
	}

	// Empty result — escalate to a server backfill if the engine supports
	// it and we aren't already awaiting one. Covers both the prepend path
	// (scrolled to top, no older rows locally) and the initial open of a
	// chat whose local store has no rows yet.
	emptyPrepend := msg.prepend && added == 0
	emptyInitial := !msg.prepend && len(msg.msgs) == 0
	if emptyPrepend || emptyInitial {
		caps := m.eng.Capabilities()
		if caps.HistorySync && !m.awaitingBackfill && !m.historyExhausted[msg.chatID] {
			m.historyLoading = true
			m.loadingKind = loadServer
			m.loadingChat = msg.chatID
			m.awaitingBackfill = true
			return m.requestBackfillCmd(msg.chatID)
		}
		// No capability or already exhausted — done.
		m.historyLoading = false
		m.loadingKind = loadNone
		m.loadingChat = ""
		m.historyExhausted[msg.chatID] = true
		return nil
	}

	// Normal path: local read produced rows (or initial load completed).
	m.historyLoading = false
	m.loadingKind = loadNone
	m.loadingChat = ""
	return nil
}

func (m *Model) loadHistoryIfNeeded() tea.Cmd {
	if m.openChat == "" {
		return nil
	}
	if m.historyLoaded[m.openChat] {
		return nil
	}
	if m.historyLoading {
		return nil
	}
	m.historyLoading = true
	m.loadingKind = loadLocal
	m.loadingChat = m.openChat
	return m.historyCmd(m.openChat, time.Time{}, historyPageSize, false)
}

func (m *Model) maybeFetchOlder() tea.Cmd {
	if m.openChat == "" {
		return nil
	}
	if !m.historyLoaded[m.openChat] {
		return nil
	}
	if m.historyExhausted[m.openChat] {
		return nil
	}
	if m.historyLoading {
		return nil
	}
	if !m.viewport.AtTop() {
		return nil
	}
	msgs := m.messages[m.openChat]
	var before time.Time
	if len(msgs) > 0 {
		before = msgs[0].Timestamp
	}
	m.historyLoading = true
	m.loadingKind = loadLocal
	m.loadingChat = m.openChat
	return m.historyCmd(m.openChat, before, historyPageSize, true)
}

// toggleMembers flips the group-member overlay for the open chat. No-op
// for non-group chats and for engines whose Capabilities().Groups is
// false. The participant list is fetched lazily on first open per chat.
func (m *Model) toggleMembers() tea.Cmd {
	if m.openChat == "" {
		return nil
	}
	if !m.eng.Capabilities().Groups {
		return nil
	}
	idx, ok := m.chatIndex[m.openChat]
	if !ok || !m.chats[idx].IsGroup {
		return nil
	}
	m.showingMembers = !m.showingMembers
	if !m.showingMembers {
		return nil
	}
	if _, cached := m.members[m.openChat]; cached {
		return nil
	}
	m.membersLoading = true
	m.membersErr = ""
	return m.participantsCmd(m.openChat)
}

// listCapacity is how many chat rows fit in the list pane right now. Each
// chat row consumes 3 vertical lines (title + snippet + separator); overlays
// (new-chat input, search input, filter banner) eat extra rows above the list.
func (m Model) listCapacity() int {
	bodyHeight := max(m.height-3, 5)
	extras := 0
	if m.newChatActive {
		extras += 3
	}
	if m.searchActive && m.searchPane == paneList {
		extras += 2
	} else if !m.searchActive && m.searchPane == paneList && m.searchQuery != "" {
		extras += 2
	}
	rows := bodyHeight - extras
	if rows < 3 {
		rows = 3
	}
	return max(rows/3, 1)
}

// clampedListOffset returns a listOffset that keeps selectedIdx visible
// within the current window and never scrolls past the end of the list.
func (m Model) clampedListOffset() int {
	n := len(m.visibleChats())
	if n == 0 {
		return 0
	}
	
	capacity := m.listCapacity()
	off := m.listOffset
	m.selectedIdx = min(m.selectedIdx, off)
	if m.selectedIdx >= off+capacity {
		off = m.selectedIdx - capacity + 1
	}
	if maxOff := n - capacity; off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	return off
}

// visibleChats returns the chat list filtered for display. With no search
// query active, chats we have no history for (zero LastActivity — typically
// contact-store seeds that haven't sent or received anything in our sync
// window) are hidden so the list reflects active conversations. While
// searching, every chat is searchable by title regardless of activity.
func (m Model) visibleChats() []engine.Chat {
	q := m.activeListQuery()
	if q == "" {
		out := make([]engine.Chat, 0, len(m.chats))
		for _, c := range m.chats {
			if c.LastActivity.IsZero() {
				continue
			}
			out = append(out, c)
		}
		return out
	}
	q = strings.ToLower(q)
	out := make([]engine.Chat, 0, len(m.chats))
	for _, c := range m.chats {
		if strings.Contains(strings.ToLower(c.Title), q) ||
			(c.Alias != "" && strings.Contains(strings.ToLower(c.Alias), q)) {
			out = append(out, c)
		}
	}
	return out
}

// activeListQuery is the search query that should filter the chat list
// right now: either the live input value while searching the list pane,
// or the committed query if the list pane was the search target.
func (m Model) activeListQuery() string {
	if m.searchActive && m.searchPane == paneList {
		return m.searchInput.Value()
	}
	if !m.searchActive && m.searchPane == paneList {
		return m.searchQuery
	}
	return ""
}

// activeConvQuery mirrors activeListQuery for the conversation pane.
func (m Model) activeConvQuery() string {
	if m.searchActive && m.searchPane == paneConversation {
		return m.searchInput.Value()
	}
	if !m.searchActive && m.searchPane == paneConversation {
		return m.searchQuery
	}
	return ""
}

func (m *Model) openSelectedChat() tea.Cmd {
	vc := m.visibleChats()
	if m.selectedIdx >= len(vc) {
		return nil
	}
	c := vc[m.selectedIdx]
	m.openChat = c.ID
	m.pane = paneConversation
	m.mode = modeNormal
	m.showingMembers = false
	m.refreshConversation()
	m.viewport.GotoBottom()

	// Local zero so the badge disappears immediately; engine clears its row
	// async so a restart sees the same. Index via chatIndex so the update
	// lands in the unfiltered slice even when a filter is active.
	if c.UnreadCount > 0 {
		c.UnreadCount = 0
		if idx, ok := m.chatIndex[c.ID]; ok {
			m.chats[idx] = c
		}
	}
	var cmds []tea.Cmd
	if cmd := m.loadHistoryIfNeeded(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	// Group chats: fetch the participant list eagerly so the conversation
	// pane can label each peer message with the sender's name. The members
	// overlay (toggled by 'm') reuses the same cache.
	if c.IsGroup && m.eng.Capabilities().Groups {
		if _, cached := m.members[c.ID]; !cached && !m.membersLoading {
			m.membersLoading = true
			m.membersErr = ""
			cmds = append(cmds, m.participantsCmd(c.ID))
		}
	}
	if u, ok := m.eng.(unreadClearer); ok {
		id := m.openChat
		cmds = append(cmds, func() tea.Msg {
			_ = u.ClearUnread(id)
			return nil
		})
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

func (m *Model) resizePanes() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	leftWidth := max(m.width/3, 28)
	leftWidth = min(leftWidth, 50)
	rightWidth := max(m.width-leftWidth-1, 10)
	bodyHeight := max(m.height-3, 5)

	// Right pane internal layout: header (1) + blank (1) + viewport + input
	// (input height + 1 blank).
	inputH := 3
	innerWidth := max(rightWidth-4, 10) // padding 1,2 on each side
	vpHeight := max(bodyHeight-2-inputH-2, 3)
	m.viewport.Width = innerWidth
	m.viewport.Height = vpHeight
	m.input.SetWidth(innerWidth)
	m.input.SetHeight(inputH)
	m.refreshConversation()
}

// debugf writes a line to $CHARMING_WHATSMEOW_DEBUG_LOG when set. No-op
// otherwise. Used for transient diagnostics that would otherwise corrupt
// the alt-screen if written to stderr while the TUI is running.
func debugf(format string, args ...any) {
	path := os.Getenv("CHARMING_WHATSMEOW_DEBUG_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, time.Now().Format("15:04:05.000")+" "+format+"\n", args...)
}

// ensureImageTransmitted is the single point where image bytes get pushed
// to the terminal. It's a no-op when the terminal doesn't speak the kitty
// graphics protocol, when the message has no usable attachment yet, or when
// we've already transmitted this message's image. The transmission is
// written directly to stderr — bypassing Bubble Tea's frame buffer — which
// is safe because q=2,U=1 is silent (no cursor movement, no glyph at the
// current position).
func (m *Model) ensureImageTransmitted(msg engine.Message) {
	if !imagerender.Supported() {
		debugf("ensureImage: unsupported terminal, msg=%s", msg.ID)
		return
	}
	if msg.Attachment == nil ||
		msg.Attachment.Kind != engine.AttachmentImage ||
		msg.Attachment.Path == "" {
		debugf("ensureImage: skip msg=%s att=%+v", msg.ID, msg.Attachment)
		return
	}
	if _, ok := m.imageIDs[msg.ID]; ok {
		return
	}
	// Pick the placement grid up front. We use stable bounds rather than
	// the live viewport so that subsequent resizes don't desync the
	// placeholder grid from the transmitted r=/c= (which would crop).
	// imageMaxRows × imageMaxCols is the ceiling; FitCells preserves the
	// image's aspect ratio inside it.
	rows, cols := imagerender.FitCells(
		msg.Attachment.Width, msg.Attachment.Height,
		imageMaxRows, imageMaxCols)
	if rows <= 0 || cols <= 0 {
		debugf("ensureImage: zero placement msg=%s w=%d h=%d", msg.ID, msg.Attachment.Width, msg.Attachment.Height)
		return
	}

	// Allocate next ID. Reserve 0 as "unset" and wrap below 0xFFFFFF so the
	// 24-bit truecolour fg-encoding in the placeholder doesn't overflow.
	m.nextImageID++
	if m.nextImageID == 0 || m.nextImageID > 0xFFFFFF {
		m.nextImageID = 1
	}
	if err := imagerender.Transmit(os.Stderr, m.nextImageID, rows, cols, msg.Attachment.Path); err != nil {
		debugf("ensureImage: transmit failed msg=%s path=%s err=%v", msg.ID, msg.Attachment.Path, err)
		return
	}
	debugf("ensureImage: ok msg=%s id=%d rows=%d cols=%d path=%s", msg.ID, m.nextImageID, rows, cols, msg.Attachment.Path)
	m.imageIDs[msg.ID] = imagePlacement{id: m.nextImageID, rows: rows, cols: cols}
}

// imageMaxRows / imageMaxCols cap the placeholder grid for any single
// inline image. Chosen to fit comfortably in a typical bubble width
// without hogging the viewport. Resizing the terminal smaller than these
// will clip the right/bottom edges of images — acceptable trade-off
// versus retransmitting on every resize.
const (
	imageMaxRows = 15
	imageMaxCols = 40
)

func (m *Model) appendMessage(msg engine.Message) {
	prev := m.messages[msg.ChatID]
	// Replace if same ID already present (status carry-over).
	for i := range prev {
		if prev[i].ID == msg.ID {
			prev[i] = msg
			m.messages[msg.ChatID] = prev
			m.msgChat[msg.ID] = msg.ChatID
			if msg.ChatID == m.openChat {
				m.refreshConversation()
			}
			return
		}
	}
	m.messages[msg.ChatID] = append(prev, msg)
	m.msgChat[msg.ID] = msg.ChatID
	if msg.ChatID == m.openChat {
		atBottom := m.viewport.AtBottom()
		m.refreshConversation()
		if atBottom {
			m.viewport.GotoBottom()
		}
	}
}

func (m *Model) updateStatus(id engine.MessageID, status engine.MessageStatus) {
	chatID, ok := m.msgChat[id]
	if !ok {
		return
	}
	msgs := m.messages[chatID]
	for i := range msgs {
		if msgs[i].ID == id {
			// Don't downgrade.
			if status > msgs[i].Status || msgs[i].Status == engine.StatusUnknown {
				msgs[i].Status = status
				m.messages[chatID] = msgs
				if chatID == m.openChat {
					m.refreshConversation()
				}
			}
			return
		}
	}
}

func (m *Model) refreshConversation() {
	if m.openChat == "" {
		return
	}
	msgs := m.messages[m.openChat]
	width := m.viewport.Width
	if width <= 0 {
		width = 40
	}
	isGroup := false
	if idx, ok := m.chatIndex[m.openChat]; ok {
		isGroup = m.chats[idx].IsGroup
	}
	names := senderNameLookup(m.members[m.openChat])
	m.viewport.SetContent(renderMessages(msgs, width, m.activeConvQuery(), m.imageIDs, isGroup, names))
}

// senderNameLookup builds an ID→display-name function from a participant
// list. Returns a closure (not a map) so renderMessages can also fall back
// to a JID-user heuristic when the participant list hasn't loaded yet or
// the sender has since left the group.
func senderNameLookup(people []engine.GroupParticipant) func(engine.ContactID) string {
	byID := make(map[engine.ContactID]string, len(people))
	for _, p := range people {
		if p.Name != "" {
			byID[p.ID] = p.Name
		}
	}
	return func(id engine.ContactID) string {
		if name, ok := byID[id]; ok {
			return name
		}
		// Last-resort: extract the user portion of a JID-shaped ID
		// ("447700900123@s.whatsapp.net" → "447700900123") so the label is
		// at least recognisable until the participant list arrives.
		s := string(id)
		if at := strings.IndexByte(s, '@'); at > 0 {
			return s[:at]
		}
		return s
	}
}

func (m *Model) rebuildIndex() {
	m.chatIndex = make(map[engine.ChatID]int, len(m.chats))
	for i, c := range m.chats {
		m.chatIndex[c.ID] = i
	}
	if m.selectedIdx >= len(m.chats) {
		m.selectedIdx = 0
	}
	m.listOffset = m.clampedListOffset()
}

func (m *Model) upsertChat(c engine.Chat) {
	if idx, ok := m.chatIndex[c.ID]; ok {
		m.chats[idx] = c
	} else {
		m.chats = append(m.chats, c)
	}
	sort.SliceStable(m.chats, func(i, j int) bool {
		return m.chats[i].LastActivity.After(m.chats[j].LastActivity)
	})
	m.rebuildIndex()
}

// View satisfies tea.Model.
func (m Model) View() string {
	switch m.state {
	case stateInitializing:
		return appHeader() + "\nconnecting…\n" + m.footer()
	case statePairing:
		return appHeader() + "\nScan this QR with WhatsApp → Linked devices:\n\n" +
			renderQR(m.qr) + "\n" + m.footer()
	case stateConnected:
		return m.renderConnected() + m.cheatSheet() + m.footer()
	case stateDisconnected:
		return appHeader() + "\nDisconnected. Reconnecting…\n" + m.footer()
	case stateError:
		return appHeader() + "\nError: " + errString(m.err) + "\n" + m.footer()
	}
	return appHeader() + m.footer()
}

// cheatSheet renders the optional context-sensitive help line above the
// footer when m.help is true. Toggled with '?'. Stays empty otherwise so
// the layout doesn't shift for users who never touch it.
func (m Model) cheatSheet() string {
	if !m.help {
		return ""
	}
	var line string
	switch {
	case m.searchActive:
		line = "search: type to filter   esc: clear   enter: commit"
	case m.newChatActive:
		line = "new chat: esc cancel   enter start"
	case m.aliasActive:
		line = "alias: esc cancel   enter save (empty clears)"
	case m.pane == paneList:
		line = "j/k nav   enter open   n new chat   R rename   / search   ? hide help   q quit"
	case m.mode == modeInsert:
		line = "esc normal   enter send   ctrl+j newline   ? hide help"
	default:
		line = "h/esc back   j/k scroll   i insert   m members   / search   ? hide help"
	}
	return "\n" + cheatSheetStyle.Render(line)
}

func (m Model) footer() string {
	if m.state != stateConnected {
		return "\nq: quit   L: log out"
	}
	if m.newChatActive {
		return "\nesc: cancel   enter: start chat"
	}
	if m.aliasActive {
		return "\nesc: cancel   enter: save (empty clears)"
	}
	if m.searchActive {
		return "\nesc: clear   enter: commit   ?: help"
	}
	if m.pane == paneList {
		return "\nj/k: nav   enter/l: open   tab: focus chat   n: new   R: rename   /: search   ?: help   q: quit"
	}
	if m.mode == modeInsert {
		return "\nesc: normal   enter: send   ctrl+j: newline"
	}
	return "\nh/esc: back   tab: list   j/k: scroll   i: insert   m: members   R: rename   /: search   ?: help"
}

// Adaptive colour palette. Light/Dark pairs let lipgloss pick based on the
// terminal background. The Dark side preserves the pre-M6 hardcoded values;
// Light is a hand-tuned mirror so contrast survives on a white background.
var (
	colourFg        = lipgloss.AdaptiveColor{Light: "0", Dark: "15"}
	colourFgInverse = lipgloss.AdaptiveColor{Light: "15", Dark: "15"}
	colourAccent    = lipgloss.AdaptiveColor{Light: "29", Dark: "22"}
	colourBorder    = lipgloss.AdaptiveColor{Light: "250", Dark: "240"}
	colourSelected  = lipgloss.AdaptiveColor{Light: "254", Dark: "236"}
	colourMuted     = lipgloss.AdaptiveColor{Light: "242", Dark: "245"}
	colourMeta      = lipgloss.AdaptiveColor{Light: "243", Dark: "244"}
	colourBadge     = lipgloss.AdaptiveColor{Light: "28", Dark: "34"}
	colourInfo      = lipgloss.AdaptiveColor{Light: "27", Dark: "33"}
	colourFail      = lipgloss.AdaptiveColor{Light: "1", Dark: "9"}
	colourWarn      = lipgloss.AdaptiveColor{Light: "166", Dark: "172"}
	colourMatch     = lipgloss.AdaptiveColor{Light: "226", Dark: "226"}
	colourMatchFg   = lipgloss.AdaptiveColor{Light: "0", Dark: "0"}
)

var (
	headerStyle = lipgloss.NewStyle().
			Foreground(colourFgInverse).
			Background(colourAccent).
			Bold(true).
			Padding(0, 1)

	listPaneStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, true, false, false).
			BorderForeground(colourBorder)

	rightPaneStyle = lipgloss.NewStyle().Padding(1, 2)

	chatRowStyle = lipgloss.NewStyle().Padding(0, 1)

	chatRowSelectedStyle = lipgloss.NewStyle().
				Padding(0, 1).
				Background(colourAccent).
				Foreground(colourFgInverse).
				Bold(true)

	chatRowOpenStyle = lipgloss.NewStyle().
				Padding(0, 1).
				Background(colourSelected).
				Foreground(colourFg)

	chatTitleStyle = lipgloss.NewStyle().Bold(true)

	chatSnippetStyle = lipgloss.NewStyle().
				Foreground(colourMuted)

	timeStyle = lipgloss.NewStyle().
			Foreground(colourMeta)

	unreadBadgeStyle = lipgloss.NewStyle().
				Background(colourBadge).
				Foreground(colourFgInverse).
				Padding(0, 1).
				Bold(true)

	emptyHintStyle = lipgloss.NewStyle().
			Foreground(colourMeta).
			Italic(true)

	bubbleOwnStyle = lipgloss.NewStyle().
			Background(colourAccent).
			Foreground(colourFgInverse).
			Padding(0, 1)

	bubblePeerStyle = lipgloss.NewStyle().
			Background(colourSelected).
			Foreground(colourFg).
			Padding(0, 1)

	bubbleMetaStyle = lipgloss.NewStyle().
			Foreground(colourMeta)

	senderLabelStyle = lipgloss.NewStyle().
				Foreground(colourMuted).
				Bold(true).
				PaddingLeft(1)

	statusReadStyle = lipgloss.NewStyle().Foreground(colourInfo)
	statusFailStyle = lipgloss.NewStyle().Foreground(colourFail)

	matchHighlightStyle = lipgloss.NewStyle().
				Background(colourMatch).
				Foreground(colourMatchFg).
				Bold(true)

	cheatSheetStyle = lipgloss.NewStyle().
			Foreground(colourMeta).
			Italic(true)

	syncBarStyle = lipgloss.NewStyle().
			Foreground(colourFgInverse).
			Background(colourInfo).
			Bold(true)

	modeNormalBadge = lipgloss.NewStyle().
			Background(colourInfo).
			Foreground(colourFgInverse).
			Padding(0, 1).Bold(true).Render("NORMAL")
	modeInsertBadge = lipgloss.NewStyle().
			Background(colourWarn).
			Foreground(colourFgInverse).
			Padding(0, 1).Bold(true).Render("INSERT")
)

func appHeader() string {
	return headerStyle.Render("whatstui") + "\n"
}

// renderSyncBar draws a single-row progress affordance shown above the main
// body during the post-pairing initial sync. Width follows the terminal so
// it spans both panes. Empty string when inactive.
func (m Model) renderSyncBar() string {
	if !m.syncActive {
		return ""
	}
	width := m.width
	if width <= 0 {
		width = 60
	}
	label := m.syncStage
	if label == "" {
		label = "syncing"
	}
	label = fmt.Sprintf(" Initial sync (%s) %d%% ", label, m.syncPercent)
	barWidth := max(width-lipgloss.Width(label)-2, 8)
	filled := barWidth * m.syncPercent / 100
	filled = min(max(filled, 0), barWidth)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
	return syncBarStyle.Render(label+bar) + "\n"
}

func (m Model) renderConnected() string {
	width := m.width
	if width <= 0 {
		width = 100
	}
	height := m.height
	if height <= 0 {
		height = 30
	}

	leftWidth := max(width/3, 28)
	leftWidth = min(leftWidth, 50)
	rightWidth := max(width-leftWidth-1, 10)

	bodyHeight := max(height-3, 5)
	if m.syncActive {
		bodyHeight = max(bodyHeight-1, 5)
	}

	left := listPaneStyle.
		Width(leftWidth).
		Height(bodyHeight).
		Render(m.renderChatList(leftWidth - 1))

	right := rightPaneStyle.
		Width(rightWidth).
		Height(bodyHeight).
		Render(m.renderRightPane())

	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	return appHeader() + m.renderSyncBar() + body
}

func (m Model) renderChatList(innerWidth int) string {
	var b strings.Builder
	if m.newChatActive {
		b.WriteString(m.newChat.View())
		b.WriteString("\n")
		if m.newChatBusy {
			b.WriteString(emptyHintStyle.Render("verifying…"))
			b.WriteString("\n")
		} else if m.newChatErr != "" {
			b.WriteString(statusFailStyle.Render(m.newChatErr))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if m.aliasActive {
		b.WriteString(m.aliasInput.View())
		b.WriteString("\n")
		if m.aliasBusy {
			b.WriteString(emptyHintStyle.Render("saving…"))
			b.WriteString("\n")
		} else if m.aliasErr != "" {
			b.WriteString(statusFailStyle.Render(m.aliasErr))
			b.WriteString("\n")
		} else {
			b.WriteString(emptyHintStyle.Render("local only — never sent"))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if m.searchActive && m.searchPane == paneList {
		b.WriteString(m.searchInput.View())
		b.WriteString("\n\n")
	} else if q := m.activeListQuery(); q != "" {
		b.WriteString(emptyHintStyle.Render("filter: " + q))
		b.WriteString("\n\n")
	}
	if len(m.chats) == 0 {
		b.WriteString(emptyHintStyle.Render("No chats yet.\n\nIncoming messages\nwill appear here.\n\nPress 'n' for a new chat."))
		return b.String()
	}
	vc := m.visibleChats()
	if len(vc) == 0 {
		b.WriteString(emptyHintStyle.Render("No chats match."))
		return b.String()
	}
	off := m.clampedListOffset()
	capacity := m.listCapacity()
	end := off + capacity
	end = min(end, len(vc))
	if off > 0 {
		b.WriteString(emptyHintStyle.Render("↑ more"))
		b.WriteString("\n")
	}
	for i := off; i < end; i++ {
		c := vc[i]
		isSelected := i == m.selectedIdx && !m.newChatActive && !m.aliasActive
		isOpen := c.ID == m.openChat
		switch {
		case isSelected && m.pane == paneList:
			row := renderChatRow(c, innerWidth-4)
			b.WriteString(chatRowSelectedStyle.Width(innerWidth).Render("❯ " + row))
		case isSelected:
			// Selection still tracked while focus is in conversation pane —
			// show a dimmer marker so the user can see where j/k will resume.
			row := renderChatRow(c, innerWidth-4)
			b.WriteString(chatRowOpenStyle.Width(innerWidth).Render("❯ " + row))
		case isOpen:
			row := renderChatRow(c, innerWidth-4)
			b.WriteString(chatRowOpenStyle.Width(innerWidth).Render("● " + row))
		default:
			row := renderChatRow(c, innerWidth-4)
			b.WriteString(chatRowStyle.Width(innerWidth).Render("  " + row))
		}
		b.WriteString("\n")
	}
	if end < len(vc) {
		b.WriteString(emptyHintStyle.Render("↓ more"))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// displayTitle returns the alias if the user has set one, otherwise the
// engine-supplied Title. Centralised so list rows and the conversation
// header stay in lock-step.
func displayTitle(c engine.Chat) string {
	if c.Alias != "" {
		return c.Alias
	}
	return c.Title
}

func renderChatRow(c engine.Chat, width int) string {
	width = max(width, 12)
	ts := formatTimestamp(c.LastActivity)
	tsBlock := timeStyle.Render(ts)

	titleWidth := max(width-lipgloss.Width(tsBlock)-1, 4)
	title := truncate(displayTitle(c), titleWidth)
	if c.IsGroup {
		title = "# " + title
	}
	titleLine := lipgloss.JoinHorizontal(
		lipgloss.Top,
		chatTitleStyle.Width(titleWidth).Render(title),
		" ",
		tsBlock,
	)

	snippetWidth := width
	snippet := c.LastSnippet
	if c.UnreadCount > 0 {
		badge := unreadBadgeStyle.Render(fmt.Sprintf("%d", c.UnreadCount))
		snippetWidth = max(width-lipgloss.Width(badge)-1, 4)
		snippet = truncate(snippet, snippetWidth)
		return titleLine + "\n" + lipgloss.JoinHorizontal(
			lipgloss.Top,
			chatSnippetStyle.Width(snippetWidth).Render(snippet),
			" ",
			badge,
		)
	}
	snippet = truncate(snippet, snippetWidth)
	return titleLine + "\n" + chatSnippetStyle.Width(snippetWidth).Render(snippet)
}

func (m Model) renderRightPane() string {
	if m.pane == paneList || m.openChat == "" {
		if len(m.chats) == 0 {
			return emptyHintStyle.Render("Select a chat to view messages.")
		}
		return emptyHintStyle.Render("Press enter / l on a chat to open it.")
	}

	var c engine.Chat
	if idx, ok := m.chatIndex[m.openChat]; ok {
		c = m.chats[idx]
	}
	title := displayTitle(c)
	if title == "" {
		title = string(m.openChat)
	}
	if c.IsGroup {
		title = "# " + title
	}

	var modeBadge string
	if m.mode == modeInsert {
		modeBadge = modeInsertBadge
	} else {
		modeBadge = modeNormalBadge
	}
	header := lipgloss.JoinHorizontal(
		lipgloss.Top,
		chatTitleStyle.Render(title),
		"  ",
		modeBadge,
	)

	body := m.viewport.View()
	if m.showingMembers && c.IsGroup {
		body = m.renderMembers(c.ID)
	}
	input := m.input.View()
	var searchBar string
	if m.searchActive && m.searchPane == paneConversation {
		searchBar = m.searchInput.View() + "\n"
	} else if q := m.activeConvQuery(); q != "" {
		searchBar = emptyHintStyle.Render("filter: "+q) + "\n"
	}
	if m.historyLoading && m.loadingChat == m.openChat {
		text := "Loading older messages…"
		if m.loadingKind == loadServer {
			text = "Fetching older messages from server…"
		}
		banner := emptyHintStyle.Render(text)
		return header + "\n" + searchBar + banner + "\n" + body + "\n" + input
	}
	return header + "\n" + searchBar + body + "\n" + input
}

// renderMembers returns a plain text rendering of the participant list
// for chatID. Admin rows are flagged with a leading "*". Falls through to
// status copy while the request is in flight or after an error.
func (m Model) renderMembers(chatID engine.ChatID) string {
	if m.membersLoading {
		return emptyHintStyle.Render("Loading members…")
	}
	if m.membersErr != "" {
		return statusFailStyle.Render("Members: " + m.membersErr)
	}
	people := m.members[chatID]
	if len(people) == 0 {
		return emptyHintStyle.Render("No members.")
	}
	var b strings.Builder
	b.WriteString(chatTitleStyle.Render(fmt.Sprintf("Members (%d)", len(people))))
	b.WriteString("\n\n")
	for _, p := range people {
		marker := "  "
		if p.IsAdmin {
			marker = "* "
		}
		b.WriteString(marker)
		b.WriteString(p.Name)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderMessages(msgs []engine.Message, width int, query string, imageIDs map[engine.MessageID]imagePlacement, isGroup bool, senderName func(engine.ContactID) string) string {
	if query != "" {
		filtered := make([]engine.Message, 0, len(msgs))
		ql := strings.ToLower(query)
		for _, msg := range msgs {
			if strings.Contains(strings.ToLower(msg.Body), ql) {
				filtered = append(filtered, msg)
			}
		}
		msgs = filtered
	}
	if len(msgs) == 0 {
		if query != "" {
			return emptyHintStyle.Render("No messages match.")
		}
		return emptyHintStyle.Render("No messages in view.\nNew messages will appear here.")
	}
	bubbleMax := max(width*3/4, 12)
	var b strings.Builder
	for i, msg := range msgs {
		// Image attachments with a known kitty graphics ID render as a
		// placeholder grid in place of the wrapped body. Everything else
		// (failed transmission, no kitty support, or download still in
		// flight) falls through to the textual body — usually "[image]".
		isImage := false
		var body string
		if p, ok := imageIDs[msg.ID]; ok && msg.Attachment != nil &&
			msg.Attachment.Kind == engine.AttachmentImage {
			// Render at the exact rows/cols recorded at transmit time.
			// Recomputing here against the live viewport would desync from
			// the transmitted r=/c= and crop the image.
			body = imagerender.Placeholder(p.id, p.rows, p.cols)
			isImage = true
		} else {
			body = wrapForBubble(msg.Body, bubbleMax-2)
			if query != "" {
				body = highlightMatches(body, query)
			}
		}
		meta := msg.Timestamp.Format("15:04")
		if msg.FromMe {
			meta = meta + " " + statusGlyph(msg.Status)
		}
		bubble := body + "\n" + bubbleMetaStyle.Render(meta)
		var rendered string
		switch {
		case isImage:
			// Skip the lipgloss bubble background/foreground for image
			// messages: bubbleOwnStyle/bubblePeerStyle would emit an SGR
			// foreground that overrides the placeholder's fg-encoded image
			// ID and the terminal would lose track of which image to draw.
			rendered = bubble
		case msg.FromMe:
			rendered = bubbleOwnStyle.Render(bubble)
		default:
			rendered = bubblePeerStyle.Render(bubble)
		}
		if msg.FromMe {
			b.WriteString(lipgloss.PlaceHorizontal(width, lipgloss.Right, rendered))
		} else {
			if isGroup && senderName != nil {
				name := senderName(msg.SenderID)
				if name != "" {
					label := senderLabelStyle.Render(name)
					b.WriteString(lipgloss.PlaceHorizontal(width, lipgloss.Left, label))
					b.WriteString("\n")
				}
			}
			b.WriteString(lipgloss.PlaceHorizontal(width, lipgloss.Left, rendered))
		}
		if i < len(msgs)-1 {
			b.WriteString("\n\n")
		}
	}
	return b.String()
}

// highlightMatches wraps each case-insensitive occurrence of query in body
// with matchHighlightStyle. Operates per line so wrapping doesn't span the
// styled run across newlines (lipgloss handles each segment cleanly).
func highlightMatches(body, query string) string {
	if query == "" {
		return body
	}
	ql := strings.ToLower(query)
	var out strings.Builder
	for li, line := range strings.Split(body, "\n") {
		if li > 0 {
			out.WriteByte('\n')
		}
		ll := strings.ToLower(line)
		i := 0
		for i < len(line) {
			j := strings.Index(ll[i:], ql)
			if j < 0 {
				out.WriteString(line[i:])
				break
			}
			out.WriteString(line[i : i+j])
			out.WriteString(matchHighlightStyle.Render(line[i+j : i+j+len(ql)]))
			i += j + len(ql)
		}
	}
	return out.String()
}

func wrapForBubble(body string, w int) string {
	if w <= 0 {
		return body
	}
	var out strings.Builder
	for li, line := range strings.Split(body, "\n") {
		if li > 0 {
			out.WriteString("\n")
		}
		runes := []rune(line)
		for len(runes) > w {
			out.WriteString(string(runes[:w]))
			out.WriteString("\n")
			runes = runes[w:]
		}
		out.WriteString(string(runes))
	}
	return out.String()
}

func statusGlyph(s engine.MessageStatus) string {
	switch s {
	case engine.StatusPending:
		return bubbleMetaStyle.Render("·")
	case engine.StatusSent:
		return bubbleMetaStyle.Render("✓")
	case engine.StatusDelivered:
		return bubbleMetaStyle.Render("✓✓")
	case engine.StatusRead:
		return statusReadStyle.Render("✓✓")
	case engine.StatusFailed:
		return statusFailStyle.Render("!")
	}
	return ""
}

func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	if now.Sub(t) < 7*24*time.Hour {
		return t.Format("Mon")
	}
	return t.Format("02/01")
}

func truncate(s string, m int) string {
	if m <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= m {
		return s
	}
	if m == 1 {
		return "…"
	}
	runes := []rune(s)
	for len(runes) > 0 {
		candidate := string(runes) + "…"
		if lipgloss.Width(candidate) <= m {
			return candidate
		}
		runes = runes[:len(runes)-1]
	}
	return "…"
}

func errString(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

func renderQR(code string) string {
	if code == "" {
		return "(waiting for QR…)"
	}
	var buf bytes.Buffer
	qrterminal.GenerateWithConfig(code, qrterminal.Config{
		Level:          qr.L,
		Writer:         &buf,
		HalfBlocks:     true,
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		QuietZone:      1,
	})
	return strings.TrimRight(buf.String(), "\n")
}
