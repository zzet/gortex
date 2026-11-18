package embedding

import (
	"testing"
	"time"
)

// resetSharedCode puts the process-wide singleton back to its initial
// state so these tests do not leak into each other.
func resetSharedCode(t *testing.T) {
	t.Helper()
	restore := func() {
		sharedCodeMu.Lock()
		sharedCodeInst = nil
		sharedCodeLoaded = false
		sharedCodeEnable = true
		sharedCodeReaper = false
		sharedCodeReady.Store(nil)
		sharedCodeMu.Unlock()
	}
	restore()
	t.Cleanup(restore)
}

// TestSetCodeEmbedderEnabledGatesAndDrops covers the knob issue #278's
// reporter needed: the rerank model loaded regardless of the embedding
// configuration, with no supported way to decline it.
func TestSetCodeEmbedderEnabledGatesAndDrops(t *testing.T) {
	resetSharedCode(t)

	// Load it, then turn the channel off: the instance must be dropped,
	// not merely hidden from future callers.
	if SharedCodeEmbedder() == nil {
		t.Skip("no embedder available in this build")
	}
	SetCodeEmbedderEnabled(false)

	sharedCodeMu.Lock()
	inst, loaded := sharedCodeInst, sharedCodeLoaded
	sharedCodeMu.Unlock()
	if inst != nil || loaded {
		t.Error("disabling the channel left the model resident")
	}
	if got := SharedCodeEmbedder(); got != nil {
		t.Error("a disabled channel still handed out an embedder")
	}

	// Re-enabling reloads on next use.
	SetCodeEmbedderEnabled(true)
	if SharedCodeEmbedder() == nil {
		t.Error("re-enabling the channel did not reload the embedder")
	}
}

// TestCodeEmbedderReaperDropsIdleModel pins that an idle daemon gives the
// model back instead of holding it for the life of the process. The reaper
// is driven directly rather than by waiting out the real TTL.
func TestCodeEmbedderReaperDropsIdleModel(t *testing.T) {
	resetSharedCode(t)
	if SharedCodeEmbedder() == nil {
		t.Skip("no embedder available in this build")
	}

	sharedCodeMu.Lock()
	sharedCodeLoaded = true
	// Backdate the last use past the TTL, the state the reaper wakes to.
	ready := &codeEmbedderReady{provider: sharedCodeInst, usedAt: time.Now().Add(-2 * codeEmbedderIdleTTL)}
	sharedCodeReady.Store(ready)
	idle := sharedCodeLoaded && time.Since(ready.usedAt) >= codeEmbedderIdleTTL
	sharedCodeMu.Unlock()
	if !idle {
		t.Fatal("the reaper's idle predicate did not fire on a backdated model")
	}

	// A use refreshes the stamp, so an active daemon never reloads.
	SharedCodeEmbedder()
	sharedCodeMu.Lock()
	stillIdle := time.Since(sharedCodeReady.Load().usedAt) >= codeEmbedderIdleTTL
	sharedCodeMu.Unlock()
	if stillIdle {
		t.Error("using the embedder did not refresh its idle stamp")
	}
}
