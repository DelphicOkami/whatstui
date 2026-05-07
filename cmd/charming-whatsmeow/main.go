// Command charming-whatsmeow boots the TUI wired to a stub engine. M0 only.
// Real engines land in later milestones; this binary exists to prove the
// engine ↔ UI seam compiles and runs end to end.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/delphicokami/charming-whatsmeow/internal/engine/stub"
	"github.com/delphicokami/charming-whatsmeow/internal/ui"
)

func main() {
	eng := stub.New()
	p := tea.NewProgram(ui.New(eng))
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
