package stub_test

import (
	"context"
	"testing"

	"github.com/delphicokami/whatstui/internal/engine"
	"github.com/delphicokami/whatstui/internal/engine/stub"
)

// Compile-time check that *Engine satisfies the interface.
var _ engine.MessagingEngine = (*stub.Engine)(nil)

func TestStubLifecycle(t *testing.T) {
	e := stub.New()
	if err := e.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := e.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if e.Subscribe() == nil {
		t.Fatal("Subscribe returned nil channel")
	}
	if e.PairingFlow(context.Background()) == nil {
		t.Fatal("PairingFlow returned nil channel")
	}
}
