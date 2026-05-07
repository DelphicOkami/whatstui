// Package ui hosts the Bubble Tea application. It depends on
// internal/engine and must never import a backend directly.
package ui

import (
	"bytes"
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mdp/qrterminal/v3"
	"rsc.io/qr"

	"github.com/delphicokami/charming-whatsmeow/internal/engine"
)

type state int

const (
	stateInitializing state = iota
	statePairing
	stateConnected
	stateDisconnected
	stateError
)

// Model is the root Bubble Tea model.
type Model struct {
	eng     engine.MessagingEngine
	pairing <-chan engine.PairingEvent
	events  <-chan engine.Event

	state state
	qr    string
	err   error

	loggingOut bool
}

// New builds a root model wired to the given engine. The engine should be
// constructed but not yet connected — Init kicks off Connect.
func New(eng engine.MessagingEngine) Model {
	return Model{
		eng:     eng,
		pairing: eng.PairingFlow(context.Background()),
		events:  eng.Subscribe(),
		state:   stateInitializing,
	}
}

// connectMsg carries the result of an async engine.Connect call.
type connectMsg struct{ err error }

// pairingMsg carries one item from the engine's pairing channel.
type pairingMsg struct {
	evt    engine.PairingEvent
	closed bool
}

// engineMsg carries one item from the engine's event channel.
type engineMsg struct {
	evt    engine.Event
	closed bool
}

// logoutMsg carries the result of an async Logout call.
type logoutMsg struct{ err error }

// Init kicks off Connect and starts listening on the pairing/event channels.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.connectCmd(), m.nextPairing(), m.nextEvent())
}

func (m Model) connectCmd() tea.Cmd {
	return func() tea.Msg {
		return connectMsg{err: m.eng.Connect(context.Background())}
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

// Update satisfies tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			_ = m.eng.Disconnect()
			return m, tea.Quit
		case "L":
			if !m.loggingOut {
				m.loggingOut = true
				m.state = stateInitializing
				m.qr = ""
				m.err = nil
				return m, m.logoutCmd()
			}
		}

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
		case msg.evt.QR != "":
			m.state = statePairing
			m.qr = msg.evt.QR
		}
		return m, m.nextPairing()

	case engineMsg:
		if msg.closed {
			return m, nil
		}
		if d, ok := msg.evt.(engine.Disconnected); ok {
			if d.Err != nil {
				m.state = stateError
				m.err = d.Err
			} else {
				m.state = stateDisconnected
			}
		}
		return m, m.nextEvent()

	case logoutMsg:
		m.loggingOut = false
		if msg.err != nil {
			m.state = stateError
			m.err = msg.err
			return m, nil
		}
		// Restart from a clean slate.
		return m, m.connectCmd()
	}
	return m, nil
}

// View satisfies tea.Model.
func (m Model) View() string {
	header := "charming-whatsmeow — M1\n"
	footer := "\nq: quit   L: log out\n"

	switch m.state {
	case stateInitializing:
		return header + "\nconnecting…\n" + footer
	case statePairing:
		return header + "\nScan this QR with WhatsApp → Linked devices:\n\n" +
			renderQR(m.qr) + "\n" + footer
	case stateConnected:
		return header + "\nConnected. (M1 ends here — chat list lands in M2.)\n" + footer
	case stateDisconnected:
		return header + "\nDisconnected. Reconnecting…\n" + footer
	case stateError:
		return header + "\nError: " + errString(m.err) + "\n" + footer
	}
	return header + footer
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
