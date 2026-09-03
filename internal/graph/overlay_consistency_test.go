package graph_test

import (
	"testing"

	"github.com/zzet/gortex/internal/graph/overlaytest"
)

// TestOverlayLayerConformance runs the composition matrix against the
// in-process layer the MCP overlay middleware builds. The matrix itself
// lives in overlaytest so every other implementation of the layer
// contract is held to the same rules; this file is the in-memory
// implementation's entry into it.
func TestOverlayLayerConformance(t *testing.T) {
	overlaytest.Run(t, overlaytest.NewInMemoryLayerBuilder)
}
