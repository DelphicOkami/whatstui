// Command charming-whatsmeow boots the TUI wired to the WhatsApp engine.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/delphicokami/charming-whatsmeow/internal/engine/whatsapp"
	"github.com/delphicokami/charming-whatsmeow/internal/ui"
)

func main() {
	eng, err := whatsapp.New("")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	p := tea.NewProgram(ui.New(eng), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
