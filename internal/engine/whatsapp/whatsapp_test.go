package whatsapp_test

import (
	"github.com/delphicokami/charming-whatsmeow/internal/engine"
	"github.com/delphicokami/charming-whatsmeow/internal/engine/whatsapp"
)

// Compile-time check that *Engine satisfies the interface.
var _ engine.MessagingEngine = (*whatsapp.Engine)(nil)
