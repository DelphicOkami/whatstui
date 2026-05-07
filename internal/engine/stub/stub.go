// Package stub provides a no-op MessagingEngine used by M0 to wire the TUI
// before any real backend exists. Every method returns zero-value/nil and the
// event channels are never closed unless the engine is disconnected.
package stub

import (
	"context"
	"errors"
	"time"

	"github.com/delphicokami/charming-whatsmeow/internal/engine"
)

// Engine is a no-op MessagingEngine.
type Engine struct {
	events  chan engine.Event
	pairing chan engine.PairingEvent
}

// New returns a stub engine with empty event channels.
func New() *Engine {
	return &Engine{
		events:  make(chan engine.Event),
		pairing: make(chan engine.PairingEvent),
	}
}

func (e *Engine) Connect(context.Context) error                          { return nil }
func (e *Engine) Disconnect() error                                      { return nil }
func (e *Engine) Logout(context.Context) error                           { return nil }
func (e *Engine) PairingFlow(context.Context) <-chan engine.PairingEvent { return e.pairing }

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

func (e *Engine) Subscribe() <-chan engine.Event    { return e.events }
func (e *Engine) Capabilities() engine.Capabilities { return engine.Capabilities{} }
