package daemon

import "testing"

// TestAdditiveToolsAreClassifiedDurableWriters keeps AdditiveTools a strict
// refinement of ToolEffects: an additive tool must still be a durable writer
// (never read-only), and IsDestructive must hold for every other durable
// writer while session-only controls and reads stay non-destructive.
func TestAdditiveToolsAreClassifiedDurableWriters(t *testing.T) {
	for name := range AdditiveTools {
		if !IsMutating(name) {
			t.Errorf("AdditiveTools entry %q must be a durable writer in ToolEffects", name)
		}
		if IsDestructive(name) {
			t.Errorf("AdditiveTools entry %q must not be destructive", name)
		}
	}
	for _, name := range []string{"edit_file", "write_file", "post_review", "index_repository", "save_scope", "edit_memory"} {
		if !IsDestructive(name) {
			t.Errorf("%q should be destructive", name)
		}
	}
	for _, name := range []string{"overlay_push", "set_planning_mode", "tools_search", "search_symbols", "query_memories"} {
		if IsDestructive(name) {
			t.Errorf("%q should not be destructive", name)
		}
	}
}
