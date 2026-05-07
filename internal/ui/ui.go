// Package ui hosts the Bubble Tea application. It depends on
// internal/engine and must never import a backend directly.
package ui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/delphicokami/charming-whatsmeow/internal/engine"
)

// Model is the root Bubble Tea model. M0 keeps it intentionally empty —
// later milestones add pairing, chat list, conversation views.
type Model struct {
	eng engine.MessagingEngine
}

// New builds a root model wired to the given engine.
func New(eng engine.MessagingEngine) Model {
	return Model{eng: eng}
}

// Init satisfies tea.Model.
func (m Model) Init() tea.Cmd { return nil }

// Update satisfies tea.Model. Only quit keys are wired in M0.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

// View satisfies tea.Model.
func (m Model) View() string {
	return "charming-whatsmeow — M0 skeleton\n\npress q to quit\n"
}
