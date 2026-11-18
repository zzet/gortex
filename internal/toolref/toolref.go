// Package toolref renders references to Gortex tools for agent guidance.
// MCP-capable profiles keep indexed operations on MCP while temporary
// failures leave agents free to continue other work. Bash-only profiles and
// CLI diagnostics can separately render the real `gortex call <tool> --arg
// k=v` shape, never the invalid bare `gortex <tool>` form.
package toolref

// cliExample maps internal/compatibility tool names used by hooks to the
// compact public call an agent should execute from Bash. Hooks may still use
// the internal handler name for their daemon probe; emitted guidance must not
// make an agent learn that implementation vocabulary.
//
// Symbol-ID placeholders render both forms — `<file>::<Name|Recv.Name>` — so
// an agent targeting a method knows the ID carries the receiver
// (`pkg/s.go::Server.Handle`), not the bare method name; the bare form only
// resolves functions and types.
var cliExample = map[string]string{
	"read_file":           `gortex call read --arg target='{"file":"<file>"}'`,
	"get_symbol_source":   `gortex call read --arg target='{"symbol":"<file>::<Name|Recv.Name>"}'`,
	"get_editing_context": `gortex call read --arg operation=editing_context --arg target='{"file":"<file>"}'`,
	"get_file_summary":    `gortex call read --arg operation=summary --arg target='{"file":"<file>"}'`,
	"get_symbol":          `gortex call read --arg target='{"symbol":"<file>::<Name|Recv.Name>"}'`,
	"search_symbols":      `gortex call search --arg operation=symbols --arg query='<name>'`,
	"search_text":         `gortex call search --arg operation=text --arg query='<text>'`,
	"find_usages":         `gortex call relations --arg operation=usages --arg target='{"symbol":"<file>::<Name|Recv.Name>"}'`,
	"get_callers":         `gortex call relations --arg operation=callers --arg target='{"symbol":"<file>::<Name|Recv.Name>"}'`,
	"smart_context":       `gortex call explore --arg operation=context --arg task='<task>'`,
	"explore":             `gortex call explore --arg task='<task>'`,
	"get_repo_outline":    `gortex call explore --arg operation=outline --arg options='{"path_prefix":"<dir>/"}'`,
	"edit_file":           `gortex call edit --arg target='{"file":"<file>"}' --arg match='<old>' --arg replacement='<new>'`,
	"edit_symbol":         `gortex call edit --arg target='{"symbol":"<id>"}' --arg match='<old>' --arg replacement='<new>'`,
	"index_repository":    `gortex call workspace_admin --arg operation=index --arg arguments='{"path":"<repo-root>"}'`,
	"reindex_repository":  `gortex call workspace_admin --arg operation=reindex --arg arguments='{"path":"<repo-root>"}'`,
}

// CLIFallback renders the compact shell invocation for one operation, for
// example `gortex call read --arg target='{"file":"..."}'`. This is the single
// place an internal tool reference becomes agent-facing Bash — nothing else
// should hand-assemble a `gortex …` shape, so the bare-verb mistake can never
// be re-minted piecemeal.
func CLIFallback(tool string) string {
	if example := cliExample[tool]; example != "" {
		return example
	}
	return "gortex call " + tool + " --arg key=value"
}

// ConcreteCLIFallback reports whether tool has a reviewed, shell-safe compact
// invocation rather than the generic compatibility fallback.
func ConcreteCLIFallback(tool string) (string, bool) {
	example, ok := cliExample[tool]
	return example, ok
}

// MCPMissingToolsException is retained for older per-agent policy strings.
// Shared producers use the unconditional step-scoped degraded guidance below;
// per-agent content migrations remain a separate compatibility change.
const MCPMissingToolsException = "Outside a Gortex-tracked repo, or when the user says to proceed without Gortex, say so once and continue with native tools."

// Constant forms let shared instruction blocks remain compile-time constants;
// the rendering helpers below return these exact sentences on every surface.
const DegradedModeText = "If a Gortex call fails or tools seem absent (empty tool search is not absence), retry later, verify a write before resending it, do other work natively, report only the step needing the graph. Never start a daemon, track, or reconfigure."

// ToolSurfacePolicyText keeps installed examples subordinate to the actual
// session policy. Explicit compatibility presets remain valid MCP surfaces;
// this rule neither promotes tools nor bypasses an operator restriction.
const ToolSurfacePolicyText = "Follow advertised tools and server/session instructions. Compact examples require those tools; otherwise follow legacy session guidance. Missing `change` alone is not missing editing when legacy editors are advertised. Read-only policies and explicit denials still apply."

const TransportText = "Never use `gortex call` in place of MCP; git, build, and test commands are fine."

// DegradedModeLine is the bounded standing policy for transient and unavailable
// graph operations, including ambiguous write acknowledgements.
func DegradedModeLine() string { return DegradedModeText }

// TransportClause keeps the MCP transport boundary separate from ordinary
// developer commands and native work that does not need the graph.
func TransportClause() string { return TransportText }

// MCPRequiredLine is shared by MCP-capable lifecycle guidance.
func MCPRequiredLine() string {
	return "  - Native Gortex MCP is the transport for indexed code. " + TransportClause() + " " + DegradedModeLine() + "\n"
}
