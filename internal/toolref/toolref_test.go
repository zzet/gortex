package toolref

import (
	"strings"
	"testing"
)

func TestCLIFallbackUsesCompactPublicSurface(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"read_file":         "gortex call read --arg target=",
		"get_symbol_source": "gortex call read --arg target=",
		"search_symbols":    "gortex call search --arg operation=symbols",
		"find_usages":       "gortex call relations --arg operation=usages",
		"smart_context":     "gortex call explore --arg operation=context",
		"edit_file":         "gortex call edit --arg target=",
		"index_repository":  "gortex call workspace_admin --arg operation=index",
	}
	for internal, want := range tests {
		internal, want := internal, want
		t.Run(internal, func(t *testing.T) {
			t.Parallel()
			got := CLIFallback(internal)
			if !strings.Contains(got, want) {
				t.Fatalf("CLIFallback(%q) = %q, want compact call containing %q", internal, got, want)
			}
			if strings.Contains(got, "gortex call "+internal+" ") {
				t.Fatalf("CLIFallback(%q) leaked the internal tool name: %q", internal, got)
			}
		})
	}
}

func TestMCPRequiredLineRejectsTransportFallback(t *testing.T) {
	t.Parallel()
	want := "  - Native Gortex MCP is the transport for indexed code. " + TransportClause() + " " + DegradedModeLine() + "\n"
	if got := MCPRequiredLine(); got != want {
		t.Fatalf("MCPRequiredLine() = %q, want %q", got, want)
	}
}

func TestDegradedModeAndTransportBounds(t *testing.T) {
	t.Parallel()
	line := DegradedModeLine()
	if len(line) > 240 {
		t.Fatalf("degraded guidance grew to %d bytes", len(line))
	}
	for _, text := range []string{"Never start a daemon", "empty tool search is not absence", "verify a write before resending", "do other work natively", "report only the step"} {
		if !strings.Contains(line, text) {
			t.Errorf("degraded guidance lost %q", text)
		}
	}
	for _, text := range []string{"and stop", "gortex call "} {
		if strings.Contains(line, text) {
			t.Errorf("degraded guidance contains %q", text)
		}
	}
	clause := TransportClause()
	if len(clause) > 80 || !strings.Contains(clause, "`gortex call`") || !strings.Contains(clause, "git, build, and test") {
		t.Fatalf("transport clause lost its bounded contract: %q (%d bytes)", clause, len(clause))
	}
}

func TestCLIFallbackPlaceholdersAreShellQuoted(t *testing.T) {
	t.Parallel()
	for name, example := range cliExample {
		if strings.Contains(example, "=<") {
			t.Errorf("cliExample[%q] contains an unquoted shell-redirection placeholder: %s", name, example)
		}
	}
}
