package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/toolref"
)

// stubFileOutline replaces the get_file_summary seam the Read deny consults
// for its outline. Restored automatically via t.Cleanup.
func stubFileOutline(t *testing.T, summary *hookFileSummary, ok bool) {
	t.Helper()
	prev := fileSummaryFn
	fileSummaryFn = func(string, string) (*hookFileSummary, bool) { return summary, ok }
	t.Cleanup(func() { fileSummaryFn = prev })
}

func TestGrepDenyCarriesProbeHits(t *testing.T) {
	stubProbe(t, []grepSymbolHit{
		{Name: "handlePlace", Kind: "function", FilePath: "internal/place/handler.go", Line: 42},
		{Name: "placeEdges", Kind: "method", FilePath: "internal/place/edges.go", Line: 7},
	}, nil)

	result := probeSymbolPattern("Grep", "handlePlace", "", defaultGrepGuidance())
	if !result.deny {
		t.Fatalf("probed Grep hit was not denied: %#v", result)
	}
	for _, want := range []string{
		"handlePlace — internal/place/handler.go:42 (function)",
		"placeEdges — internal/place/edges.go:7 (method)",
	} {
		if !strings.Contains(result.reason, want) {
			t.Fatalf("Grep deny %q does not carry hit %q", result.reason, want)
		}
	}
}

func TestGrepDenyEvidenceStaysWithinByteBound(t *testing.T) {
	hits := make([]grepSymbolHit, 0, 40)
	for i := range 40 {
		hits = append(hits, grepSymbolHit{
			Name:     fmt.Sprintf("veryLongSymbolNameNumber%02d", i),
			Kind:     "function",
			FilePath: fmt.Sprintf("internal/some/deeply/nested/package/path/file%02d.go", i),
			Line:     i + 1,
		})
	}

	block := grepHitEvidence(hits)
	if block == "" {
		t.Fatal("bounded evidence block is empty for 40 hits")
	}
	if len(block) > denyEvidenceMaxBytes {
		t.Fatalf("evidence block is %d bytes, want <= %d", len(block), denyEvidenceMaxBytes)
	}
	if rows := strings.Count(block, " — "); rows > denyEvidenceMaxLines {
		t.Fatalf("evidence block carries %d rows, want <= %d", rows, denyEvidenceMaxLines)
	}
	if !strings.Contains(block, "... and ") {
		t.Fatalf("truncated evidence block does not report the dropped hits: %q", block)
	}

	stubProbe(t, hits, nil)
	result := probeSymbolPattern("Grep", "veryLongSymbolNameNumber00", "", defaultGrepGuidance())
	if !result.deny || !strings.Contains(result.reason, block) {
		t.Fatalf("Grep deny does not embed the bounded evidence block: %#v", result)
	}
}

func TestDenyEvidenceBlockCountsItsHeader(t *testing.T) {
	rows := make([]denyEvidenceRow, 0, 20)
	for i := range 20 {
		rows = append(rows, denyEvidenceRow{
			Label: fmt.Sprintf("Symbol%02d", i),
			File:  strings.Repeat("nested/", 12) + "file.go",
			Line:  i + 1,
			Note:  "function",
		})
	}

	block := denyEvidenceBlock(strings.Repeat("h", 200)+"\n", rows)
	if len(block) > denyEvidenceMaxBytes {
		t.Fatalf("headered evidence block is %d bytes, want <= %d", len(block), denyEvidenceMaxBytes)
	}
	if !strings.Contains(block, "... and ") {
		t.Fatalf("headered block dropped rows without saying so: %q", block)
	}
}

func TestGrepProbeDaemonUnreachableKeepsBaselineGuidance(t *testing.T) {
	stubProbe(t, nil, errDaemonUnreachable)

	result := probeSymbolPattern("Grep", "handlePlace", "", defaultGrepGuidance())
	if result.deny {
		t.Fatalf("unreachable daemon must not deny: %#v", result)
	}
	if result.context != defaultGrepGuidance() {
		t.Fatalf("unreachable-daemon guidance = %q, want the baseline message", result.context)
	}
}

func TestReadDenyCarriesSymbolOutline(t *testing.T) {
	stubIndexedFile(t, true, 3)
	stubFileOutline(t, &hookFileSummary{Symbols: []summaryNode{
		{Name: "Parse", Kind: "function", StartLine: 12, EndLine: 40},
		{Name: "Emit", Kind: "method", StartLine: 44, EndLine: 60},
	}}, true)

	result := enrichRead(map[string]any{"file_path": "internal/hooks/pretooluse.go"}, "/repo")
	if !result.deny {
		t.Fatalf("indexed Read was not denied: %#v", result)
	}
	for _, want := range []string{
		readOutlineHeader,
		"Parse — internal/hooks/pretooluse.go:12 (function)",
		"Emit — internal/hooks/pretooluse.go:44 (method)",
	} {
		if !strings.Contains(result.reason, want) {
			t.Fatalf("Read deny %q does not carry outline entry %q", result.reason, want)
		}
	}
}

func TestReadDenyOutlineHeadIsBounded(t *testing.T) {
	symbols := make([]summaryNode, 0, 30)
	for i := range 30 {
		symbols = append(symbols, summaryNode{
			Name:      fmt.Sprintf("SymbolNumber%02d", i),
			Kind:      "function",
			StartLine: i + 1,
		})
	}
	stubIndexedFile(t, true, len(symbols))
	stubFileOutline(t, &hookFileSummary{Symbols: symbols}, true)

	block := readOutlineEvidence("/repo", "internal/hooks/pretooluse.go")
	if len(block) > denyEvidenceMaxBytes {
		t.Fatalf("outline block is %d bytes, want <= %d", len(block), denyEvidenceMaxBytes)
	}
	if rows := strings.Count(block, " — "); rows > denyEvidenceMaxLines {
		t.Fatalf("outline block carries %d rows, want <= %d", rows, denyEvidenceMaxLines)
	}
	if !strings.Contains(block, "... and 25 more") {
		t.Fatalf("outline block does not report the remaining symbols: %q", block)
	}
}

// baselineReadDeny is the redirect-only Read deny as it reads with no graph
// results attached. Any degraded path must reproduce it byte for byte.
func baselineReadDeny(filePath string, symbolCount int) string {
	return fmt.Sprintf("[Gortex] BLOCKED: Read of %s (%d symbols indexed). Call `explore` first, then use `read` instead:\n", filePath, symbolCount) +
		"  - `read(target:{symbol:\"<id>\"})` — one symbol\n" +
		"  - `read(target:{symbols:[\"<id>\"]})` — several symbols\n" +
		"  - `read(operation:\"editing_context\", target:{file:\"<path>\"})` — full editing context\n" +
		gcxTip + toolref.MCPRequiredLine()
}

func TestReadDenyWithoutDaemonMatchesBaselineMessage(t *testing.T) {
	stubIndexedFile(t, true, 7)
	stubFileOutline(t, nil, false)

	result := enrichRead(map[string]any{"file_path": "internal/hooks/pretooluse.go"}, "/repo")
	if !result.deny {
		t.Fatalf("indexed Read was not denied: %#v", result)
	}
	if want := baselineReadDeny("internal/hooks/pretooluse.go", 7); result.reason != want {
		t.Fatalf("degraded Read deny = %q, want exactly %q", result.reason, want)
	}
}

func TestReadDenyOutlineGivesUpOnSlowDaemon(t *testing.T) {
	stubIndexedFile(t, true, 7)
	prev := fileSummaryFn
	fileSummaryFn = func(string, string) (*hookFileSummary, bool) {
		time.Sleep(3 * time.Second)
		return &hookFileSummary{Symbols: []summaryNode{{Name: "Parse", Kind: "function", StartLine: 12}}}, true
	}
	t.Cleanup(func() { fileSummaryFn = prev })

	start := time.Now()
	result := enrichRead(map[string]any{"file_path": "internal/hooks/pretooluse.go"}, "/repo")
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Fatalf("Read deny waited %s on the outline lookup, want the %s budget", elapsed, readOutlineTimeout)
	}
	if want := baselineReadDeny("internal/hooks/pretooluse.go", 7); result.reason != want {
		t.Fatalf("timed-out Read deny = %q, want exactly %q", result.reason, want)
	}
}

func TestLocalizationTerminalDenyCarriesNoGraphEvidence(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	stubIndexedFile(t, true, 9)
	stubFileOutline(t, &hookFileSummary{Symbols: []summaryNode{
		{Name: "Parse", Kind: "function", StartLine: 12},
	}}, true)
	stubProbe(t, []grepSymbolHit{
		{Name: "Parse", Kind: "function", FilePath: "repo/source.go", Line: 12},
	}, nil)

	cwd := t.TempDir()
	identity := beginTestLocalizationTurn(t, "terminal-evidence", "prompt-1", cwd)
	snapshotTestLocalizationTool(t, identity, gortexMCPToolPrefix+"read", "tool-1")
	post := localizationPostToolPayload(t, gortexMCPToolPrefix+"read", "tool-1", identity,
		terminalToolResponse(t, terminalContractMap(), true, false))
	captureHookStdout(t, func() { runPostToolUse(post) })

	for _, tt := range []struct {
		name  string
		tool  string
		input map[string]any
	}{
		{name: "host read", tool: "Read", input: map[string]any{"file_path": "repo/source.go"}},
		{name: "host grep", tool: "Grep", input: map[string]any{"pattern": "Parse", "path": "repo"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pre := preToolPayload(t, tt.tool, "", identity, tt.input)
			raw := captureHookStdout(t, func() { runPreToolUse(pre, 0, ModeDeny) })
			var output HookOutput
			if err := json.Unmarshal([]byte(raw), &output); err != nil {
				t.Fatalf("decode PreToolUse output %q: %v", raw, err)
			}
			if output.HookSpecificOutput == nil || output.HookSpecificOutput.PermissionDecision != "deny" {
				t.Fatalf("expected terminal deny, got %#v", output)
			}
			reason := output.HookSpecificOutput.PermissionDecisionReason
			if !strings.HasPrefix(reason, localizationTerminalDenyReason) {
				t.Fatalf("terminal deny reason = %q, want prefix %q", reason, localizationTerminalDenyReason)
			}
			if strings.Contains(reason, readOutlineHeader) || strings.Contains(reason, "Parse — ") {
				t.Fatalf("terminal deny leaked access-policy graph evidence: %q", reason)
			}
		})
	}
}
