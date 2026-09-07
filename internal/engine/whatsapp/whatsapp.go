// Package whatsapp implements engine.MessagingEngine on top of whatsmeow.
//
// M1 brought up pairing and connection. M2 adds:
//   - inbound message handling: extract text, persist the chat, emit
//     MessageReceived and ChatUpdated.
//   - chat persistence via internal/store/chatstore so the TUI's left pane
//     survives restarts. The engine owns the store; the UI calls Chats().
package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waCompanionReg "go.mau.fi/whatsmeow/proto/waCompanionReg"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	waWeb "go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"

	"github.com/delphicokami/whatstui/internal/engine"
	"github.com/delphicokami/whatstui/internal/store/chatstore"
	"github.com/delphicokami/whatstui/internal/store/messagestore"
)

// Engine is the whatsmeow-backed MessagingEngine.
type Engine struct {
	dbPath         string
	storeDBPath    string
	messagesDBPath string

	mu         sync.Mutex
	container  *sqlstore.Container
	device     *store.Device
	client     *whatsmeow.Client
	bgCancel   context.CancelFunc
	chats      *chatstore.Store
	messages   *messagestore.Store
	groupNames map[string]string
	// oldestInfo caches the oldest known MessageInfo per chat so on-demand
	// history requests can anchor on a real (chat,msgID,fromMe,timestamp)
	// tuple. Updated on every ingest path (live, send, HistorySync).
	oldestInfo map[engine.ChatID]types.MessageInfo

	// initialSyncDone gates SyncProgress reporting so on-demand backfills
	// after the first post-pairing sync don't drive the UI's progress bar.
	initialSyncDone bool

	// pairedThisSession is true only between observing events.PairSuccess
	// and the completion of the post-pairing initial sync. WhatsApp also
	// sends small RECENT HistorySyncs on subsequent reconnects (delta of
	// queued messages while offline); without this gate they'd re-trigger
	// the progress bar every run.
	pairedThisSession bool

	events  chan engine.Event
	pairing chan engine.PairingEvent

	logFile *os.File
	logger  *log.Logger
}

func (e *Engine) logf(format string, args ...any) {
	if e.logger != nil {
		e.logger.Printf(format, args...)
	}
}

// waLogger adapts a *log.Logger to whatsmeow's waLog.Logger interface so the
// library's internal handshake / sync trace flows into the same log file.
type waLogger struct {
	mod string
	out *log.Logger
}

func (w *waLogger) printf(level, msg string, args ...any) {
	if w.out == nil {
		return
	}
	if w.mod != "" {
		w.out.Printf("["+level+"] ["+w.mod+"] "+msg, args...)
		return
	}
	w.out.Printf("["+level+"] "+msg, args...)
}
func (w *waLogger) Errorf(msg string, args ...any) { w.printf("ERROR", msg, args...) }
func (w *waLogger) Warnf(msg string, args ...any)  { w.printf("WARN", msg, args...) }
func (w *waLogger) Infof(msg string, args ...any)  { w.printf("INFO", msg, args...) }
func (w *waLogger) Debugf(msg string, args ...any) { w.printf("DEBUG", msg, args...) }
func (w *waLogger) Sub(mod string) waLog.Logger {
	sub := mod
	if w.mod != "" {
		sub = w.mod + "/" + mod
	}
	return &waLogger{mod: sub, out: w.out}
}

// DefaultDBPath returns ~/.local/share/whatstui/session.db.
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "whatstui", "session.db"), nil
}

// New constructs a whatsapp engine that will store its session at dbPath.
// Pass an empty string to use DefaultDBPath. The chat-list store lives next
// to the session DB as store.db.
func New(dbPath string) (*Engine, error) {
	if dbPath == "" {
		p, err := DefaultDBPath()
		if err != nil {
			return nil, err
		}
		dbPath = p
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}

	logPath := filepath.Join(filepath.Dir(dbPath), "whatstui.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	logger := log.New(logFile, "", log.LstdFlags|log.Lmicroseconds)

	host, _ := os.Hostname()
	deviceName := "whatstui"
	if host != "" {
		deviceName = "whatstui-" + host
	}
	store.DeviceProps.Os = &deviceName

	// Cap the post-pair archive at 14 days. Anything older is reachable
	// via ON_DEMAND backfill when the user scrolls to the top of a chat.
	store.DeviceProps.RequireFullSync = proto.Bool(true)
	if store.DeviceProps.HistorySyncConfig == nil {
		store.DeviceProps.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{}
	}
	store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(14)
	store.DeviceProps.HistorySyncConfig.FullSyncSizeMbLimit = proto.Uint32(256)
	store.DeviceProps.HistorySyncConfig.StorageQuotaMb = proto.Uint32(256)

	return &Engine{
		dbPath:         dbPath,
		storeDBPath:    filepath.Join(filepath.Dir(dbPath), "store.db"),
		messagesDBPath: filepath.Join(filepath.Dir(dbPath), "messages.db"),
		events:         make(chan engine.Event, 64),
		pairing:        make(chan engine.PairingEvent, 8),
		groupNames:     make(map[string]string),
		oldestInfo:     make(map[engine.ChatID]types.MessageInfo),
		logFile:        logFile,
		logger:         logger,
	}, nil
}

// Connect opens the session store and connects to WhatsApp. If no device is
// paired yet, QR codes flow out via PairingFlow until a phone scans them.
func (e *Engine) Connect(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.client != nil && e.client.IsConnected() {
		return nil
	}

	wl := &waLogger{out: e.logger}
	e.logf("Connect: opening session store at %s", e.dbPath)
	// WAL + a generous busy_timeout matters during initial pairing: whatsmeow
	// runs prekey decryption (HistorySync protocol messages) concurrently with
	// app-state sync and contact ingestion, and the rollback journal default
	// surfaces those collisions as SQLITE_BUSY — which silently drops the
	// HistorySync blob, leaving the user with chats but no message history.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)",
		e.dbPath,
	)
	container, err := sqlstore.New(ctx, "sqlite", dsn, wl.Sub("sqlstore"))
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return fmt.Errorf("get device: %w", err)
	}

	chats, err := chatstore.Open(e.storeDBPath)
	if err != nil {
		_ = container.Close()
		return fmt.Errorf("open chat store: %w", err)
	}

	messages, err := messagestore.Open(e.messagesDBPath)
	if err != nil {
		_ = chats.Close()
		_ = container.Close()
		return fmt.Errorf("open message store: %w", err)
	}

	if device.ID == nil {
		e.logf("Connect: no paired device — will request QR")
	} else {
		e.logf("Connect: using paired device %s", device.ID.String())
	}
	client := whatsmeow.NewClient(device, wl.Sub("client"))
	client.EnableAutoReconnect = true
	client.AddEventHandler(e.handleEvent)

	bgCtx, cancel := context.WithCancel(context.Background())

	cleanup := func() {
		cancel()
		_ = messages.Close()
		_ = chats.Close()
		_ = container.Close()
	}

	if device.ID == nil {
		qrChan, err := client.GetQRChannel(bgCtx)
		if err != nil {
			cleanup()
			return fmt.Errorf("get QR channel: %w", err)
		}
		if err := client.Connect(); err != nil {
			cleanup()
			return fmt.Errorf("connect: %w", err)
		}
		go e.forwardQR(qrChan)
		go e.seedChatList(bgCtx, client, chats)
	} else {
		if err := client.Connect(); err != nil {
			cleanup()
			return fmt.Errorf("connect: %w", err)
		}
		// Already paired — let any pairing-flow listener know immediately.
		select {
		case e.pairing <- engine.PairingEvent{Paired: true}:
		default:
		}
		go e.seedChatList(bgCtx, client, chats)
	}

	e.container = container
	e.device = device
	e.client = client
	e.bgCancel = cancel
	e.chats = chats
	e.messages = messages
	return nil
}

func (e *Engine) forwardQR(in <-chan whatsmeow.QRChannelItem) {
	for item := range in {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			e.pairing <- engine.PairingEvent{QR: item.Code}
		case "success":
			e.pairing <- engine.PairingEvent{Paired: true}
		case whatsmeow.QRChannelEventError:
			e.pairing <- engine.PairingEvent{Err: item.Error}
		default:
			e.pairing <- engine.PairingEvent{Err: fmt.Errorf("pairing failed: %s", item.Event)}
		}
	}
}

func (e *Engine) handleEvent(evt any) {
	switch v := evt.(type) {
	case *events.Connected:
		e.logf("event: Connected")
	case *events.PairSuccess:
		e.logf("event: PairSuccess id=%s", v.ID.String())
		e.mu.Lock()
		e.pairedThisSession = true
		e.initialSyncDone = false
		e.mu.Unlock()
		e.emit(engine.SyncProgress{Stage: "paired", Percent: 0})
	case *events.OfflineSyncPreview:
		e.logf("event: OfflineSyncPreview total=%d msg=%d notif=%d", v.Total, v.Messages, v.Notifications)
	case *events.OfflineSyncCompleted:
		e.logf("event: OfflineSyncCompleted count=%d", v.Count)
	case *events.AppStateSyncComplete:
		e.logf("event: AppStateSyncComplete name=%s", v.Name)
		e.mu.Lock()
		client := e.client
		chats := e.chats
		bgCancel := e.bgCancel
		e.mu.Unlock()
		if client != nil && chats != nil && bgCancel != nil {
			go e.seedChatList(context.Background(), client, chats)
		}
	case *events.Message:
		e.handleMessage(v)
	case *events.Receipt:
		e.handleReceipt(v)
	case *events.HistorySync:
		e.handleHistorySync(v)
	case *events.Disconnected:
		e.logf("event: Disconnected")
		e.emit(engine.Disconnected{})
	case *events.LoggedOut:
		e.logf("event: LoggedOut reason=%v", v.Reason)
		e.emit(engine.Disconnected{Err: fmt.Errorf("logged out: %v", v.Reason)})
	case *events.StreamReplaced:
		e.emit(engine.Disconnected{Err: errors.New("stream replaced")})
	case *events.ClientOutdated:
		e.emit(engine.Disconnected{Err: errors.New("client outdated")})
	default:
		e.logf("event: %T", evt)
	}
}

// rememberOldest updates the per-chat oldest-known MessageInfo cache so a
// future RequestHistory call can pivot on it. Called on every ingest path.
func (e *Engine) rememberOldest(info types.MessageInfo) {
	if info.ID == "" {
		return
	}
	id := engine.ChatID(e.canonicalChatJID(info.Chat).String())
	e.mu.Lock()
	cur, ok := e.oldestInfo[id]
	if !ok || info.Timestamp.Before(cur.Timestamp) {
		e.oldestInfo[id] = info
	}
	e.mu.Unlock()
}

// canonicalChatJID resolves LID DM JIDs to their phone-number form when a
// mapping is known, so the same conversation is always keyed identically.
// WhatsApp is migrating DMs from @s.whatsapp.net to @lid; without this, an
// inbound LID message creates a duplicate chat row whose history lives
// under the PN form. Group, broadcast, and LIDs without a known PN
// mapping are returned unchanged.
func (e *Engine) canonicalChatJID(jid types.JID) types.JID {
	if jid.IsEmpty() {
		return jid
	}
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	e.mu.Lock()
	client := e.client
	e.mu.Unlock()
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return jid
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pn, err := client.Store.LIDs.GetPNForLID(ctx, jid)
	if err != nil || pn.IsEmpty() {
		return jid
	}
	return pn
}

// canonicalChatID is the engine.ChatID-typed convenience over canonicalChatJID
// for code paths that receive a string-typed chat id from the UI.
func (e *Engine) canonicalChatID(id engine.ChatID) engine.ChatID {
	jid, err := types.ParseJID(string(id))
	if err != nil {
		return id
	}
	return engine.ChatID(e.canonicalChatJID(jid).String())
}

// handleHistorySync ingests a HistorySync blob (BOOTSTRAP / RECENT / FULL /
// ON_DEMAND), persisting every text message to the message store and
// emitting one HistoryBackfilled event per chat with a non-zero new-row
// count. ChatUpdated is intentionally not emitted here — live message
// events handle that path; we just want backfill to land in the store
// without disturbing the chat list ordering.
func (e *Engine) handleHistorySync(evt *events.HistorySync) {
	if evt == nil || evt.Data == nil {
		e.logf("HistorySync: nil event/data")
		return
	}
	syncType := evt.Data.GetSyncType()
	convs := evt.Data.GetConversations()
	progress := int(evt.Data.GetProgress())
	e.logf("HistorySync: type=%s progress=%d conversations=%d",
		syncType.String(), progress, len(convs))

	// Surface progress to the UI for the post-pairing initial sync only —
	// ON_DEMAND backfills (user scrolled to top) shouldn't drive the
	// connect-time progress bar.
	if syncType != waHistorySync.HistorySync_ON_DEMAND {
		e.mu.Lock()
		paired := e.pairedThisSession
		alreadyDone := e.initialSyncDone
		e.mu.Unlock()
		if paired && !alreadyDone {
			done := progress >= 100
			stage := strings.ToLower(syncType.String())
			e.emit(engine.SyncProgress{Stage: stage, Percent: progress, Done: done})
			if done {
				e.mu.Lock()
				e.initialSyncDone = true
				e.pairedThisSession = false
				e.mu.Unlock()
			}
		}
	}

	e.mu.Lock()
	messages := e.messages
	chats := e.chats
	client := e.client
	e.mu.Unlock()
	if messages == nil {
		return
	}

	for _, conv := range convs {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		chatJID = e.canonicalChatJID(chatJID)
		chatID := engine.ChatID(chatJID.String())

		var newest engine.Message
		added := 0
		for _, hsm := range conv.GetMessages() {
			info, msg := historySyncMessage(chatJID, hsm)
			if msg.ID == "" || msg.Body == "" {
				continue
			}
			inserted, err := messages.InsertIfNew(msg)
			if err != nil {
				continue
			}
			if inserted {
				added++
				// Mirror the live path: kick off async media download for
				// image-bearing rows we just stored. Only on first insert —
				// re-replays of the same HistorySync would otherwise spam
				// duplicate downloads on every reconnect.
				if client != nil && msg.Attachment != nil &&
					msg.Attachment.Kind == engine.AttachmentImage {
					if img := hsm.GetMessage().GetMessage().GetImageMessage(); img != nil {
						go e.downloadImage(client, msg, img)
					}
				}
			}
			if msg.Timestamp.After(newest.Timestamp) {
				newest = msg
			}
			e.rememberOldest(info)
		}
		if added > 0 {
			e.emit(engine.HistoryBackfilled{ChatID: chatID, AddedCount: added})
		}

		// Seed/refresh the chat row so the left pane reflects this archive
		// even before app-state sync delivers contacts/groups.
		if chats == nil {
			continue
		}
		title := conv.GetName()
		isGroup := chatJID.Server == types.GroupServer
		if title == "" {
			if isGroup {
				e.mu.Lock()
				title = e.groupNames[chatJID.String()]
				e.mu.Unlock()
			}
			if title == "" {
				title = chatJID.User
			}
		} else if isGroup {
			e.mu.Lock()
			e.groupNames[chatJID.String()] = title
			e.mu.Unlock()
		}

		prev, exists, _ := chats.Get(chatID)
		seeded := engine.Chat{
			ID:           chatID,
			Title:        title,
			IsGroup:      isGroup,
			LastActivity: newest.Timestamp,
			LastSnippet:  snippet(newest.Body),
		}
		if exists {
			if prev.Title != "" && title == chatJID.User {
				seeded.Title = prev.Title
			}
			if !prev.LastActivity.Before(seeded.LastActivity) {
				seeded.LastActivity = prev.LastActivity
				seeded.LastSnippet = prev.LastSnippet
			}
			seeded.UnreadCount = prev.UnreadCount
		}
		if err := chats.Upsert(seeded); err == nil {
			e.emit(engine.ChatUpdated{Chat: seeded})
		}
	}
}

// historySyncMessage extracts a normalised engine.Message and a
// types.MessageInfo (used for oldest-pointer caching) from a HistorySyncMsg
// envelope. Returns zeros if the payload doesn't carry text.
func historySyncMessage(chat types.JID, hsm *waHistorySync.HistorySyncMsg) (types.MessageInfo, engine.Message) {
	if hsm == nil {
		return types.MessageInfo{}, engine.Message{}
	}
	wmi := hsm.GetMessage()
	if wmi == nil || wmi.GetKey() == nil {
		return types.MessageInfo{}, engine.Message{}
	}
	body := webMessageBody(wmi)
	if body == "" {
		return types.MessageInfo{}, engine.Message{}
	}
	key := wmi.GetKey()
	id := key.GetID()
	fromMe := key.GetFromMe()
	ts := time.Unix(int64(wmi.GetMessageTimestamp()), 0)

	sender := chat
	if fromMe {
		// Sender is self — leave the JID as-is from chat for 1:1; for groups
		// we don't have device info here, so stringify as the chat user.
	} else if chat.Server == types.GroupServer {
		if p := wmi.GetParticipant(); p != "" {
			if pj, err := types.ParseJID(p); err == nil {
				sender = pj
			}
		}
	}

	info := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			Sender:   sender,
			IsFromMe: fromMe,
			IsGroup:  chat.Server == types.GroupServer,
		},
		ID:        id,
		Timestamp: ts,
	}
	msg := engine.Message{
		ID:         engine.MessageID(id),
		ChatID:     engine.ChatID(chat.String()),
		SenderID:   engine.ContactID(sender.String()),
		Body:       body,
		Timestamp:  ts,
		FromMe:     fromMe,
		Status:     engine.StatusUnknown,
		Attachment: imageAttachment(wmi.GetMessage()),
	}
	return info, msg
}

func webMessageBody(wmi *waWeb.WebMessageInfo) string {
	m := wmi.GetMessage()
	if m == nil {
		return ""
	}
	if c := m.GetConversation(); c != "" {
		return c
	}
	if ext := m.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	switch {
	case m.GetImageMessage() != nil:
		return "[image]"
	case m.GetVideoMessage() != nil:
		return "[video]"
	case m.GetAudioMessage() != nil:
		return "[audio]"
	case m.GetDocumentMessage() != nil:
		return "[document]"
	case m.GetStickerMessage() != nil:
		return "[sticker]"
	}
	return ""
}

// handleMessage normalises an incoming whatsmeow message, persists the chat
// row, and emits MessageReceived + ChatUpdated.
func (e *Engine) handleMessage(evt *events.Message) {
	body := messageBody(evt)
	if body == "" {
		return
	}

	chatJID := e.canonicalChatJID(evt.Info.Chat)
	chatID := engine.ChatID(chatJID.String())

	e.mu.Lock()
	chats := e.chats
	messages := e.messages
	client := e.client
	cachedGroupName := e.groupNames[chatJID.String()]
	e.mu.Unlock()
	if chats == nil {
		return
	}

	prev, _, _ := chats.Get(chatID)

	title := e.deriveTitle(evt, prev.Title, cachedGroupName)
	unread := prev.UnreadCount
	if !evt.Info.IsFromMe {
		unread++
	}

	chat := engine.Chat{
		ID:           chatID,
		Title:        title,
		IsGroup:      evt.Info.IsGroup,
		LastActivity: evt.Info.Timestamp,
		UnreadCount:  unread,
		LastSnippet:  snippet(body),
	}
	_ = chats.Upsert(chat)

	msg := engine.Message{
		ID:        engine.MessageID(evt.Info.ID),
		ChatID:    chatID,
		SenderID:  engine.ContactID(evt.Info.Sender.String()),
		Body:      body,
		Timestamp: evt.Info.Timestamp,
		FromMe:    evt.Info.IsFromMe,
		Status:    engine.StatusUnknown,
		Attachment: imageAttachment(evt.Message),
	}
	if messages != nil {
		_ = messages.Upsert(msg)
	}
	e.rememberOldest(evt.Info)

	e.emit(engine.MessageReceived{Message: msg})
	e.emit(engine.ChatUpdated{Chat: chat})

	// Group title resolution (lazy, async — re-emit ChatUpdated on success).
	if evt.Info.IsGroup && cachedGroupName == "" && client != nil {
		go e.resolveGroupName(client, chatJID)
	}

	// Image download (async). Re-emits MessageReceived once the bytes are on
	// disk so the UI can swap [image] for an inline render. The engine event
	// handler must not block, so the download runs on a fresh goroutine.
	if msg.Attachment != nil && msg.Attachment.Kind == engine.AttachmentImage &&
		client != nil && evt.Message.GetImageMessage() != nil {
		go e.downloadImage(client, msg, evt.Message.GetImageMessage())
	}
}

// imageAttachment returns an engine.Attachment for an inbound message that
// carries an ImageMessage payload. Width/Height come from WhatsApp's
// metadata; Path is left empty so the UI knows the file isn't on disk yet.
func imageAttachment(m *waE2E.Message) *engine.Attachment {
	if m == nil {
		return nil
	}
	img := m.GetImageMessage()
	if img == nil {
		return nil
	}
	return &engine.Attachment{
		Kind:   engine.AttachmentImage,
		MIME:   img.GetMimetype(),
		Width:  int(img.GetWidth()),
		Height: int(img.GetHeight()),
	}
}

// mediaDir is the on-disk cache for downloaded attachments. Lives next to
// the session DB so it shares lifecycle with the paired device.
func (e *Engine) mediaDir() string {
	return filepath.Join(filepath.Dir(e.dbPath), "media")
}

// downloadImage fetches the image bytes via whatsmeow, writes them to the
// media cache, persists the path on the message row, and re-emits
// MessageReceived so the UI can re-render with the now-ready attachment.
// All errors are swallowed: a failed download just leaves the message
// showing its [image] fallback, which is the correct degraded state.
func (e *Engine) downloadImage(client *whatsmeow.Client, msg engine.Message, img *waE2E.ImageMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	data, err := client.Download(ctx, img)
	if err != nil || len(data) == 0 {
		return
	}

	if err := os.MkdirAll(e.mediaDir(), 0o755); err != nil {
		return
	}
	ext := extForMime(img.GetMimetype())
	path := filepath.Join(e.mediaDir(), string(msg.ID)+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return
	}

	e.mu.Lock()
	messages := e.messages
	e.mu.Unlock()
	if messages != nil {
		_ = messages.SetAttachmentPath(msg.ID, path)
	}

	if msg.Attachment == nil {
		msg.Attachment = &engine.Attachment{Kind: engine.AttachmentImage}
	}
	updated := msg
	att := *msg.Attachment
	att.Path = path
	updated.Attachment = &att
	e.emit(engine.MessageReceived{Message: updated})
}

func extForMime(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".jpg"
}

// seedChatList populates the chat store from WhatsApp's authoritative data
// once a connection is up: every joined group and every known contact. This
// is the only path that recovers a chat list when WhatsApp doesn't redeliver
// HistorySync (e.g. an already-paired reconnect, or a pairing where the
// initial sync didn't land). Existing rows keep their LastActivity / unread /
// snippet — we only refresh titles. New rows are inserted with zero
// LastActivity so they sort below any chat that's actually seen traffic.
func (e *Engine) seedChatList(ctx context.Context, client *whatsmeow.Client, chats *chatstore.Store) {
	if client == nil || chats == nil {
		return
	}
	// Wait briefly for the connection to settle before issuing IQs.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}
	if !client.IsLoggedIn() {
		e.logf("seedChatList: not logged in yet, skipping")
		return
	}

	groupCount := 0
	contactCount := 0
	defer func() {
		e.logf("seedChatList: done groups=%d contacts=%d", groupCount, contactCount)
	}()

	if groups, err := client.GetJoinedGroups(ctx); err == nil {
		groupCount = len(groups)
		for _, g := range groups {
			if g == nil {
				continue
			}
			id := engine.ChatID(g.JID.String())
			name := g.Name
			if name == "" {
				name = g.JID.User
			}
			e.mu.Lock()
			if g.Name != "" {
				e.groupNames[g.JID.String()] = g.Name
			}
			e.mu.Unlock()
			e.seedUpsert(chats, engine.Chat{
				ID:      id,
				Title:   name,
				IsGroup: true,
			})
		}
	}

	contacts, err := client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		e.logf("seedChatList: GetAllContacts err=%v", err)
		return
	}
	for jid, info := range contacts {
		if jid.Server != types.DefaultUserServer {
			continue
		}
		title := contactTitle(info, jid)
		if title == "" {
			continue
		}
		contactCount++
		e.seedUpsert(chats, engine.Chat{
			ID:      engine.ChatID(jid.String()),
			Title:   title,
			IsGroup: false,
		})
	}
}

// seedUpsert preserves an existing row's activity/unread/snippet and only
// refreshes the title. For a brand-new row it inserts the seeded chat as-is.
func (e *Engine) seedUpsert(chats *chatstore.Store, seeded engine.Chat) {
	cur, ok, _ := chats.Get(seeded.ID)
	if ok {
		if cur.Title != seeded.Title && seeded.Title != "" {
			if err := chats.SetTitle(seeded.ID, seeded.Title); err == nil {
				cur.Title = seeded.Title
				e.emit(engine.ChatUpdated{Chat: cur})
			}
		}
		return
	}
	if err := chats.Upsert(seeded); err != nil {
		return
	}
	e.emit(engine.ChatUpdated{Chat: seeded})
}

func contactTitle(info types.ContactInfo, jid types.JID) string {
	switch {
	case info.FullName != "":
		return info.FullName
	case info.BusinessName != "":
		return info.BusinessName
	case info.PushName != "":
		return info.PushName
	}
	return jid.User
}

func (e *Engine) resolveGroupName(client *whatsmeow.Client, jid types.JID) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := client.GetGroupInfo(ctx, jid)
	if err != nil || info == nil || info.Name == "" {
		return
	}
	e.mu.Lock()
	e.groupNames[jid.String()] = info.Name
	chats := e.chats
	e.mu.Unlock()
	if chats == nil {
		return
	}
	id := engine.ChatID(jid.String())
	_ = chats.SetTitle(id, info.Name)
	if cur, ok, _ := chats.Get(id); ok {
		e.emit(engine.ChatUpdated{Chat: cur})
	}
}

// deriveTitle picks a human-friendly title given current event and any
// previously stored title. Existing non-empty titles win unless we have a
// strictly better source (group name resolved, real contact name).
func (e *Engine) deriveTitle(evt *events.Message, prev, cachedGroup string) string {
	if evt.Info.IsGroup {
		if cachedGroup != "" {
			return cachedGroup
		}
		if prev != "" {
			return prev
		}
		return evt.Info.Chat.User
	}

	if prev != "" {
		return prev
	}

	// 1:1 chat. Prefer contact-store fields, fall back to push name, then JID.
	if e.client != nil {
		if info, err := e.client.Store.Contacts.GetContact(context.Background(), evt.Info.Chat); err == nil && info.Found {
			if info.FullName != "" {
				return info.FullName
			}
			if info.BusinessName != "" {
				return info.BusinessName
			}
			if info.PushName != "" {
				return info.PushName
			}
		}
	}
	if !evt.Info.IsFromMe && evt.Info.PushName != "" {
		return evt.Info.PushName
	}
	return evt.Info.Chat.User
}

func messageBody(evt *events.Message) string {
	if evt.Message == nil {
		return ""
	}
	if c := evt.Message.GetConversation(); c != "" {
		return c
	}
	if ext := evt.Message.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	switch {
	case evt.Message.GetImageMessage() != nil:
		return "[image]"
	case evt.Message.GetVideoMessage() != nil:
		return "[video]"
	case evt.Message.GetAudioMessage() != nil:
		return "[audio]"
	case evt.Message.GetDocumentMessage() != nil:
		return "[document]"
	case evt.Message.GetStickerMessage() != nil:
		return "[sticker]"
	}
	return ""
}

func snippet(body string) string {
	body = strings.ReplaceAll(body, "\n", " ")
	const max = 80
	if len(body) <= max {
		return body
	}
	return body[:max] + "…"
}

func (e *Engine) emit(ev engine.Event) {
	select {
	case e.events <- ev:
	default:
	}
}

// Disconnect tears the session down without forgetting the paired device.
func (e *Engine) Disconnect() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.bgCancel != nil {
		e.bgCancel()
		e.bgCancel = nil
	}
	if e.client != nil {
		e.client.Disconnect()
	}
	if e.messages != nil {
		_ = e.messages.Close()
	}
	if e.chats != nil {
		_ = e.chats.Close()
	}
	if e.container != nil {
		_ = e.container.Close()
	}
	e.client = nil
	e.container = nil
	e.device = nil
	e.chats = nil
	e.messages = nil
	return nil
}

// Logout unpairs the device on WhatsApp's side (if possible) and removes the
// local session DB so the next Connect starts a fresh pairing flow. The chat
// store is also wiped — the new session is a different account.
func (e *Engine) Logout(ctx context.Context) error {
	e.mu.Lock()
	c := e.client
	cancel := e.bgCancel
	container := e.container
	chats := e.chats
	messages := e.messages
	e.client = nil
	e.container = nil
	e.device = nil
	e.bgCancel = nil
	e.chats = nil
	e.messages = nil
	e.groupNames = make(map[string]string)
	e.oldestInfo = make(map[engine.ChatID]types.MessageInfo)
	e.initialSyncDone = false
	e.pairedThisSession = false
	dbPath := e.dbPath
	storeDBPath := e.storeDBPath
	messagesDBPath := e.messagesDBPath
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if c != nil {
		if c.IsLoggedIn() {
			_ = c.Logout(ctx)
		} else {
			c.Disconnect()
		}
	}
	if messages != nil {
		_ = messages.Close()
	}
	if chats != nil {
		_ = chats.Close()
	}
	if container != nil {
		_ = container.Close()
	}
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(storeDBPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(messagesDBPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (e *Engine) PairingFlow(context.Context) <-chan engine.PairingEvent {
	return e.pairing
}

func (e *Engine) Subscribe() <-chan engine.Event { return e.events }

func (e *Engine) Capabilities() engine.Capabilities {
	return engine.Capabilities{
		Presence:     true,
		Groups:       true,
		HistorySync:  true,
		LinkPreviews: true,
		ReadReceipts: true,
	}
}

// Chats returns the persisted chat list, sorted by most recent activity.
func (e *Engine) Chats(context.Context) ([]engine.Chat, error) {
	e.mu.Lock()
	chats := e.chats
	e.mu.Unlock()
	if chats == nil {
		return nil, nil
	}
	return chats.List()
}

// ClearUnread zeroes the unread counter for a chat (used when the UI opens it).
func (e *Engine) ClearUnread(id engine.ChatID) error {
	e.mu.Lock()
	chats := e.chats
	e.mu.Unlock()
	if chats == nil {
		return nil
	}
	return chats.ClearUnread(e.canonicalChatID(id))
}

// SetLocalAlias writes a user-set local alias for a chat. The alias is purely
// a UI display override — it never leaves the device. Passing an empty alias
// clears it. Emits ChatUpdated so the UI refreshes the list.
func (e *Engine) SetLocalAlias(id engine.ChatID, alias string) error {
	e.mu.Lock()
	chats := e.chats
	e.mu.Unlock()
	if chats == nil {
		return errors.New("not connected")
	}
	canonical := e.canonicalChatID(id)
	if err := chats.SetAlias(canonical, alias); err != nil {
		return err
	}
	c, ok, err := chats.Get(canonical)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	e.emit(engine.ChatUpdated{Chat: c})
	return nil
}

func (e *Engine) Contacts(context.Context) ([]engine.Contact, error) { return nil, nil }

// History returns up to limit persisted messages for chatID strictly older
// than before, in ascending timestamp order. A zero before value means
// "from the most recent backwards" — the natural shape for an initial open.
func (e *Engine) History(_ context.Context, chatID engine.ChatID, before time.Time, limit int) ([]engine.Message, error) {
	e.mu.Lock()
	messages := e.messages
	e.mu.Unlock()
	if messages == nil {
		return nil, nil
	}
	return messages.History(e.canonicalChatID(chatID), before, limit)
}

// RequestHistory asks WhatsApp's primary device for up to count older
// messages preceding our cached oldest known message in chatID. Returns
// nil immediately on success — the response arrives later via
// *events.HistorySync (type ON_DEMAND), at which point handleHistorySync
// persists rows and emits HistoryBackfilled.
func (e *Engine) RequestHistory(ctx context.Context, chatID engine.ChatID, count int) error {
	canonical := e.canonicalChatID(chatID)
	e.mu.Lock()
	client := e.client
	info, hasInfo := e.oldestInfo[canonical]
	e.mu.Unlock()
	if client == nil || !client.IsLoggedIn() {
		return errors.New("not connected")
	}
	if !hasInfo {
		// Nothing local to anchor on — without a MessageInfo we can't build
		// a HistorySync peer-message. Surface this so the UI stops waiting
		// for a backfill that will never arrive.
		return errors.New("no anchor message for chat — open the chat after at least one message has been received")
	}
	if count <= 0 {
		count = 50
	}
	msg := client.BuildHistorySyncRequest(&info, count)
	if _, err := client.SendPeerMessage(ctx, msg); err != nil {
		return fmt.Errorf("request history: %w", err)
	}
	return nil
}

// SendText sends a plain-text message to chatID and emits an optimistic
// MessageReceived (FromMe=true, Status=Pending) immediately so the UI can
// display the outgoing bubble. On the server's ack it emits
// MessageStatusChanged{Sent}; later receipts upgrade to Delivered/Read.
func (e *Engine) SendText(ctx context.Context, chatID engine.ChatID, body string) (engine.MessageID, error) {
	e.mu.Lock()
	client := e.client
	chats := e.chats
	messages := e.messages
	device := e.device
	e.mu.Unlock()
	if client == nil || !client.IsLoggedIn() {
		return "", errors.New("not connected")
	}
	jid, err := types.ParseJID(string(chatID))
	if err != nil {
		return "", fmt.Errorf("parse chat id: %w", err)
	}
	jid = e.canonicalChatJID(jid)
	chatID = engine.ChatID(jid.String())
	msgID := client.GenerateMessageID()
	id := engine.MessageID(msgID)

	var senderID engine.ContactID
	if device != nil && device.ID != nil {
		senderID = engine.ContactID(device.ID.String())
	}
	now := time.Now()
	pending := engine.Message{
		ID:        id,
		ChatID:    chatID,
		SenderID:  senderID,
		Body:      body,
		Timestamp: now,
		FromMe:    true,
		Status:    engine.StatusPending,
	}
	if messages != nil {
		_ = messages.Upsert(pending)
	}
	if device != nil && device.ID != nil {
		e.rememberOldest(types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:     jid,
				Sender:   *device.ID,
				IsFromMe: true,
				IsGroup:  jid.Server == types.GroupServer,
			},
			ID:        msgID,
			Timestamp: now,
		})
	}
	e.emit(engine.MessageReceived{Message: pending})

	resp, err := client.SendMessage(ctx, jid, &waE2E.Message{
		Conversation: proto.String(body),
	}, whatsmeow.SendRequestExtra{ID: msgID})
	if err != nil {
		if messages != nil {
			_ = messages.SetStatus(id, engine.StatusFailed)
		}
		e.emit(engine.MessageStatusChanged{ID: id, Status: engine.StatusFailed})
		return id, err
	}

	if chats != nil {
		prev, _, _ := chats.Get(chatID)
		title := prev.Title
		if title == "" {
			title = jid.User
		}
		chat := engine.Chat{
			ID:           chatID,
			Title:        title,
			IsGroup:      prev.IsGroup || jid.Server == types.GroupServer,
			LastActivity: resp.Timestamp,
			UnreadCount:  prev.UnreadCount,
			LastSnippet:  snippet(body),
		}
		_ = chats.Upsert(chat)
		e.emit(engine.ChatUpdated{Chat: chat})
	}

	if messages != nil {
		_ = messages.SetStatus(id, engine.StatusSent)
	}
	e.emit(engine.MessageStatusChanged{ID: id, Status: engine.StatusSent})
	return id, nil
}

// handleReceipt maps whatsmeow receipts to MessageStatusChanged events for
// each acknowledged message ID.
func (e *Engine) handleReceipt(r *events.Receipt) {
	var status engine.MessageStatus
	switch r.Type {
	case types.ReceiptTypeDelivered:
		status = engine.StatusDelivered
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		status = engine.StatusRead
	case types.ReceiptTypeServerError:
		status = engine.StatusFailed
	default:
		return
	}
	e.mu.Lock()
	messages := e.messages
	e.mu.Unlock()
	for _, id := range r.MessageIDs {
		if messages != nil {
			_ = messages.SetStatus(engine.MessageID(id), status)
		}
		e.emit(engine.MessageStatusChanged{ID: engine.MessageID(id), Status: status})
	}
}
// StartChat resolves identifier (raw phone number, +-prefixed phone, or a
// full s.whatsapp.net JID) to a real WhatsApp user, creates a chat row in
// the local store if one doesn't exist yet, and returns it. Verification
// goes through client.IsOnWhatsApp so we don't conjure rows for numbers
// the server can't actually reach. Group JIDs are rejected — group
// creation is a separate flow (CreateGroup) and isn't wired through here.
func (e *Engine) StartChat(ctx context.Context, identifier string) (engine.Chat, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return engine.Chat{}, errors.New("empty identifier")
	}

	e.mu.Lock()
	client := e.client
	chats := e.chats
	e.mu.Unlock()
	if client == nil || !client.IsLoggedIn() {
		return engine.Chat{}, errors.New("not connected")
	}

	var jid types.JID
	if strings.Contains(identifier, "@") {
		j, err := types.ParseJID(identifier)
		if err != nil {
			return engine.Chat{}, fmt.Errorf("parse JID: %w", err)
		}
		if j.Server == types.GroupServer {
			return engine.Chat{}, errors.New("group JIDs not supported here; use CreateGroup")
		}
		jid = j
	} else {
		// Phone path: keep digits only, then ask whatsmeow whether the number
		// resolves to a real WhatsApp user. The server returns the canonical
		// JID, which is what we persist.
		phone := normalisePhone(identifier)
		if phone == "" {
			return engine.Chat{}, errors.New("invalid phone number")
		}
		resps, err := client.IsOnWhatsApp(ctx, []string{"+" + phone})
		if err != nil {
			return engine.Chat{}, fmt.Errorf("verify number: %w", err)
		}
		if len(resps) == 0 || !resps[0].IsIn {
			return engine.Chat{}, fmt.Errorf("%s is not on WhatsApp", identifier)
		}
		jid = resps[0].JID
	}

	jid = e.canonicalChatJID(jid)
	chatID := engine.ChatID(jid.String())

	// Existing chat? Just return it.
	if chats != nil {
		if cur, ok, _ := chats.Get(chatID); ok {
			return cur, nil
		}
	}

	title := jid.User
	if info, err := client.Store.Contacts.GetContact(ctx, jid); err == nil && info.Found {
		switch {
		case info.FullName != "":
			title = info.FullName
		case info.BusinessName != "":
			title = info.BusinessName
		case info.PushName != "":
			title = info.PushName
		}
	}

	chat := engine.Chat{
		ID:           chatID,
		Title:        title,
		IsGroup:      false,
		LastActivity: time.Now(),
	}
	if chats != nil {
		_ = chats.Upsert(chat)
	}
	e.emit(engine.ChatUpdated{Chat: chat})
	return chat, nil
}

// normalisePhone strips formatting and a leading + so what remains is a
// digits-only E.164-style number. Returns "" if no digits are present.
func normalisePhone(in string) string {
	var b strings.Builder
	for _, r := range in {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (e *Engine) CreateGroup(context.Context, string, []engine.ContactID) (engine.Chat, error) {
	return engine.Chat{}, errors.ErrUnsupported
}

// GroupParticipants resolves the membership of a group chat through
// whatsmeow's GetGroupInfo. Names are taken from the local contact store
// where available; otherwise we fall back to the JID user portion so the
// row is never empty. Non-group chatIDs return an error.
func (e *Engine) GroupParticipants(ctx context.Context, chatID engine.ChatID) ([]engine.GroupParticipant, error) {
	jid, err := types.ParseJID(string(chatID))
	if err != nil {
		return nil, fmt.Errorf("parse chat id: %w", err)
	}
	if jid.Server != types.GroupServer {
		return nil, errors.New("not a group chat")
	}
	e.mu.Lock()
	client := e.client
	e.mu.Unlock()
	if client == nil || !client.IsLoggedIn() {
		return nil, errors.New("not connected")
	}
	info, err := client.GetGroupInfo(ctx, jid)
	if err != nil {
		return nil, fmt.Errorf("get group info: %w", err)
	}
	out := make([]engine.GroupParticipant, 0, len(info.Participants))
	for _, p := range info.Participants {
		out = append(out, engine.GroupParticipant{
			ID:      engine.ContactID(p.JID.String()),
			Name:    e.participantName(ctx, p),
			IsAdmin: p.IsAdmin || p.IsSuperAdmin,
		})
	}
	return out, nil
}

func (e *Engine) participantName(ctx context.Context, p types.GroupParticipant) string {
	if p.DisplayName != "" {
		return p.DisplayName
	}
	if e.client != nil {
		lookup := p.JID
		if !p.PhoneNumber.IsEmpty() {
			lookup = p.PhoneNumber
		}
		if c, err := e.client.Store.Contacts.GetContact(ctx, lookup); err == nil && c.Found {
			switch {
			case c.FullName != "":
				return c.FullName
			case c.BusinessName != "":
				return c.BusinessName
			case c.PushName != "":
				return c.PushName
			}
		}
	}
	if !p.PhoneNumber.IsEmpty() {
		return p.PhoneNumber.User
	}
	return p.JID.User
}
