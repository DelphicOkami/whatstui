// Package whatsapp implements engine.MessagingEngine on top of whatsmeow.
//
// M1 scope: open a session store, drive QR-code pairing, connect, and surface
// disconnects. Reads/writes (chats, history, send) are stubbed for later
// milestones — the contract on internal/engine is what the TUI sees.
package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	_ "modernc.org/sqlite"

	"github.com/delphicokami/charming-whatsmeow/internal/engine"
)

// Engine is the whatsmeow-backed MessagingEngine.
type Engine struct {
	dbPath string

	mu        sync.Mutex
	container *sqlstore.Container
	device    *store.Device
	client    *whatsmeow.Client
	bgCancel  context.CancelFunc

	events  chan engine.Event
	pairing chan engine.PairingEvent
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
// Pass an empty string to use DefaultDBPath.
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

	host, _ := os.Hostname()
	deviceName := "whatstui"
	if host != "" {
		deviceName = "whatstui-" + host
	}
	store.DeviceProps.Os = &deviceName

	return &Engine{
		dbPath:  dbPath,
		events:  make(chan engine.Event, 64),
		pairing: make(chan engine.PairingEvent, 8),
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

	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", e.dbPath)
	container, err := sqlstore.New(ctx, "sqlite", dsn, waLog.Noop)
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return fmt.Errorf("get device: %w", err)
	}

	client := whatsmeow.NewClient(device, waLog.Noop)
	client.EnableAutoReconnect = true
	client.AddEventHandler(e.handleEvent)

	bgCtx, cancel := context.WithCancel(context.Background())

	if device.ID == nil {
		qrChan, err := client.GetQRChannel(bgCtx)
		if err != nil {
			cancel()
			_ = container.Close()
			return fmt.Errorf("get QR channel: %w", err)
		}
		if err := client.Connect(); err != nil {
			cancel()
			_ = container.Close()
			return fmt.Errorf("connect: %w", err)
		}
		go e.forwardQR(qrChan)
	} else {
		if err := client.Connect(); err != nil {
			cancel()
			_ = container.Close()
			return fmt.Errorf("connect: %w", err)
		}
		// Already paired — let any pairing-flow listener know immediately.
		select {
		case e.pairing <- engine.PairingEvent{Paired: true}:
		default:
		}
	}

	e.container = container
	e.device = device
	e.client = client
	e.bgCancel = cancel
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
	case *events.Disconnected:
		e.emit(engine.Disconnected{})
	case *events.LoggedOut:
		e.emit(engine.Disconnected{Err: fmt.Errorf("logged out: %v", v.Reason)})
	case *events.StreamReplaced:
		e.emit(engine.Disconnected{Err: errors.New("stream replaced")})
	case *events.ClientOutdated:
		e.emit(engine.Disconnected{Err: errors.New("client outdated")})
	}
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
	if e.container != nil {
		_ = e.container.Close()
	}
	e.client = nil
	e.container = nil
	e.device = nil
	return nil
}

// Logout unpairs the device on WhatsApp's side (if possible) and removes the
// local session DB so the next Connect starts a fresh pairing flow.
func (e *Engine) Logout(ctx context.Context) error {
	e.mu.Lock()
	c := e.client
	cancel := e.bgCancel
	container := e.container
	e.client = nil
	e.container = nil
	e.device = nil
	e.bgCancel = nil
	dbPath := e.dbPath
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
	if container != nil {
		_ = container.Close()
	}
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
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

// Reads/writes land in M2+. Stub them so the interface is satisfied.

func (e *Engine) Chats(context.Context) ([]engine.Chat, error)       { return nil, nil }
func (e *Engine) Contacts(context.Context) ([]engine.Contact, error) { return nil, nil }
func (e *Engine) History(context.Context, engine.ChatID, time.Time, int) ([]engine.Message, error) {
	return nil, nil
}
func (e *Engine) SendText(context.Context, engine.ChatID, string) (engine.MessageID, error) {
	return "", errors.ErrUnsupported
}
func (e *Engine) StartChat(context.Context, string) (engine.Chat, error) {
	return engine.Chat{}, errors.ErrUnsupported
}
func (e *Engine) CreateGroup(context.Context, string, []engine.ContactID) (engine.Chat, error) {
	return engine.Chat{}, errors.ErrUnsupported
}
