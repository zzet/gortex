package hooks

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/mcp"
)

// collectHookGuidance runs every hook / adapter guidance emitter with inputs
// that force it to render, and returns the resulting text keyed by a label so a
// failure names the offending template. The seam vars (fileIndexedFn,
// daemonReachableFn) are stubbed so the deny paths fire without a daemon.
func collectHookGuidance(t *testing.T) map[string]string {
	t.Helper()

	prevIndexed := fileIndexedFn
	prevReach := daemonReachableFn
	prevScope := scopeTrackedFn
	t.Cleanup(func() {
		fileIndexedFn = prevIndexed
		daemonReachableFn = prevReach
		scopeTrackedFn = prevScope
	})
	daemonReachableFn = func() bool { return true }
	scopeTrackedFn = func(string, string) bool { return true }

	out := map[string]string{}

	// Pure producers — no seam needed.
	out["defaultGrepGuidance"] = defaultGrepGuidance()
	out["defaultGlobGuidance"] = defaultGlobGuidance()
	out["formatGrepDeny"] = formatGrepDeny("SomeSymbol", []grepSymbolHit{
		{Name: "SomeSymbol", Kind: "function", FilePath: "pkg/a.go", Line: 12},
	})
	out["nudgeReason_empty"] = nudgeReason("")
	out["nudgeReason_withGuidance"] = nudgeReason(defaultGrepGuidance())
	out["gortexReadAdvisory"] = gortexReadAdvisory("mcp__gortex__read_file", "pkg/a.go")
	out["gortexToolGuidance"] = gortexToolGuidance
	out["kimiSubagentFallbackBriefing"] = kimiSubagentFallbackBriefing()
	out["rulePreamble"] = rulePreamble()
	out["consultUnlockReason"] = consultUnlockReason("some deny reason")

	// Indexed-source deny paths: force fileIndexedFn to report "indexed".
	fileIndexedFn = func(_, _ string) (bool, int) { return true, 7 }
	out["enrichRead_deny"] = enrichRead(map[string]any{"file_path": "pkg/a.go"}, "/repo").reason
	out["enrichBash_readSource_deny"] = enrichBash(map[string]any{"command": "cat pkg/a.go"}, "/repo").reason
	t.Setenv(editBlockingEnvVar, "1")
	out["enrichEdit_deny"] = enrichEdit(map[string]any{"file_path": "pkg/a.go"}, "/repo").reason
	out["enrichWrite_deny"] = enrichWrite(map[string]any{"file_path": "pkg/a.go"}, "/repo").reason

	// Greedy-glob deny path.
	out["enrichGlob_deny"] = enrichGlob(map[string]any{"pattern": "**/*.go"}).reason

	// Not-indexed soft-guidance paths.
	fileIndexedFn = func(_, _ string) (bool, int) { return false, 0 }
	out["enrichRead_soft"] = enrichRead(map[string]any{"file_path": "pkg/a.go"}, "/repo").context
	out["enrichBash_readSource_soft"] = enrichBash(map[string]any{"command": "cat pkg/a.go"}, "/repo").context

	// Drop any that came back empty so the assertions below only see rendered
	// templates.
	for k, v := range out {
		if strings.TrimSpace(v) == "" {
			delete(out, k)
		}
	}
	return out
}

// TestGuidanceNeverEmitsBareToolVerb is the #259 regression gate: it iterates
// the REAL MCP tool registry (daemon-free) and asserts that no hook / adapter
// guidance template ever renders a bare `gortex <tool>` shell shape — the
// invalid form (`gortex read_file <path>`) agents invented from guidance that
// named a tool without an invocation shape. Hook guidance for an MCP-configured
// profile must not emit any shell transport; CLI-only renderers are tested in
// their own packages.
func TestGuidanceNeverEmitsBareToolVerb(t *testing.T) {
	names := mcp.RegisteredToolNames()
	if len(names) < 50 {
		t.Fatalf("expected the MCP registry to enumerate the full tool surface, got %d names", len(names))
	}

	guidance := collectHookGuidance(t)
	if len(guidance) < 12 {
		t.Fatalf("collected only %d rendered guidance templates — the sweep is incomplete", len(guidance))
	}

	// Match a lowercase `gortex` (the CLI binary) followed by only spaces/tabs
	// (same line) and then the tool name at a word boundary.
	for _, name := range names {
		re := regexp.MustCompile(`\bgortex[ \t]+` + regexp.QuoteMeta(name) + `\b`)
		for label, text := range guidance {
			if loc := re.FindString(text); loc != "" {
				t.Errorf("guidance %q renders the invalid bare shape %q — use `gortex call %s --arg …` instead:\n%s",
					label, loc, name, text)
			}
		}
	}
}

// TestGuidanceRequiresNativeMCP is the positive counterpart: every rendered
// redirect must identify a missing callable handle as a host integration
// failure, never reinterpret it as permission to start infrastructure or use
// the Bash mirror.
func TestGuidanceRequiresNativeMCP(t *testing.T) {
	guidance := collectHookGuidance(t)
	const want = "Native Gortex MCP is mandatory"
	mustRequire := []string{
		"defaultGrepGuidance", "defaultGlobGuidance", "formatGrepDeny",
		"nudgeReason_empty", "gortexReadAdvisory", "kimiSubagentFallbackBriefing",
		"rulePreamble", "enrichRead_deny", "enrichBash_readSource_deny",
		"enrichEdit_deny", "enrichWrite_deny", "enrichGlob_deny",
		"enrichRead_soft", "enrichBash_readSource_soft",
	}
	for _, label := range mustRequire {
		text, ok := guidance[label]
		if !ok {
			t.Errorf("guidance %q did not render — the collector or emitter changed", label)
			continue
		}
		if !strings.Contains(text, want) {
			t.Errorf("guidance %q does not require native MCP:\n%s", label, text)
		}
		if strings.Contains(text, "gortex call ") {
			t.Errorf("guidance %q advertises a CLI fallback in an MCP-configured profile:\n%s", label, text)
		}
	}
}
