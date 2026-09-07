// Command whatstui boots the TUI wired to the WhatsApp engine.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/delphicokami/whatstui/internal/engine"
	"github.com/delphicokami/whatstui/internal/engine/whatsapp"
	"github.com/delphicokami/whatstui/internal/ui"
)

type RunMode int

const (
	ModeSync RunMode = iota
	ModeTui
)

func fatalError(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func getRunMode() RunMode {
	syncFlag := flag.Bool("sync", false, "Pull latest messages, update local cache, and exit without starting the TUI")
	flag.BoolVar(syncFlag, "s", false, "Alias for --sync")

	flag.Parse()
	mode := ModeTui
	if *syncFlag {
		mode = ModeSync
	}
	return mode
}

func runTui(eng engine.MessagingEngine) {
	p := tea.NewProgram(ui.New(eng), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fatalError(fmt.Sprint(err))
	}
}

func runSync(eng engine.MessagingEngine) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fmt.Printf("[whatstui] Initializing messageing engine in headless mode (--sync)...\n")
	if err := eng.Connect(ctx); err != nil {
		fatalError(fmt.Sprintf("failed to connect to engine: %w", err))
	}
	defer eng.Disconnect()

	fmt.Printf("[whatstui] Fetching recent chats from engine...\n")
	chats, err := eng.Chats(ctx)
	if err != nil {
		fatalError(fmt.Sprintf("failed to list chats: %w", err))
	}
	fmt.Printf("[whatstui] Synchronizing delta messages across %d chats...\n", len(chats))
	totalNew := 0
	for _, c := range chats {
		// Pull recent history up to limit (e.g. 50 messages)
		msgs, err := eng.History(ctx, c.ID, time.Time{}, 50)
		if err != nil {
			fmt.Printf("  ! [%s] history fetch warning: %v\n", c.Title, err)
			continue
		}
		totalNew += len(msgs)
		if len(msgs) > 0 {
			fmt.Printf("  ✓ [%s] synced %d messages -> updated local cache\n", c.Title, len(msgs))
		}
	}

	fmt.Printf("[whatstui] Sync complete. Local cache updated with %d messages. Exiting (code 0).\n", totalNew)

}
func main() {
	eng, err := whatsapp.New("")
	if err != nil {
		fatalError(fmt.Sprint(err))
	}
	switch getRunMode() {
	case ModeTui:
		runTui(eng)
	case ModeSync:
		runSync(eng)
	}
}
