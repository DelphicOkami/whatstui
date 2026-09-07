package whatsapp_test

import (
	"github.com/delphicokami/whatstui/internal/engine"
	"github.com/delphicokami/whatstui/internal/engine/whatsapp"
)

// Compile-time check that *Engine satisfies the interface.
var _ engine.MessagingEngine = (*whatsapp.Engine)(nil)
