package ui_test

import (
	"strings"
	"testing"

	"github.com/delphicokami/charming-whatsmeow/internal/engine/stub"
	"github.com/delphicokami/charming-whatsmeow/internal/ui"
)

func TestModelView(t *testing.T) {
	m := ui.New(stub.New())
	if got := m.View(); !strings.Contains(got, "charming-whatsmeow") {
		t.Fatalf("View missing app name: %q", got)
	}
}
