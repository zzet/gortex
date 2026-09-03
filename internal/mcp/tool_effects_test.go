package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// TestLegacyEffectRegistryNamesAreRegistered closes the other half of the
// effect audit: daemon tests assert every known writer has the right effect,
// while this protocol-side test asserts those effect entries still name real
// legacy MCP tools. Facade names are validated by the facade surface tests.
func TestLegacyEffectRegistryNamesAreRegistered(t *testing.T) {
	srv := newFullTestServer(t)
	registered := make(map[string]bool)
	for _, descriptor := range srv.ToolDescriptors() {
		registered[descriptor.Name] = true
	}

	facades := map[string]bool{
		"edit": true, "refactor": true, "remember": true,
		"workspace_admin": true, "publish_review": true,
		"overlay": true, "session": true,
	}
	// These legacy tools are registered only when their optional subsystem is
	// enabled (multi-repo, overlays, proxy routing, or lazy discovery), which
	// the minimal full test server intentionally does not initialize.
	conditionalLegacy := map[string]bool{
		"overlay_delete": true, "overlay_drop": true, "overlay_drop_branch": true,
		"overlay_fork": true, "overlay_keepalive": true, "overlay_merge": true,
		"overlay_push": true, "overlay_register": true, "overlay_switch": true,
		"proxy_disable": true, "proxy_enable": true,
		"set_active_project": true, "tools_search": true,
		"track_repository": true, "untrack_repository": true,
		"forget_checkout": true, "set_primary_checkout": true,
		"reconcile_checkouts": true,
	}
	for name := range daemon.ToolEffects {
		if facades[name] || conditionalLegacy[name] {
			continue
		}
		if !registered[name] {
			t.Errorf("daemon.ToolEffects classifies unknown legacy MCP tool %q", name)
		}
	}
}
