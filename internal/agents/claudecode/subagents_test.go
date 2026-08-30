package claudecode

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/profiles"
)

// TestSubAgentToolPropagation locks in the invariant that every Gortex
// sub-agent declares an explicit, graph-only MCP tool allowlist — the
// mechanism by which Gortex propagates its tools to sub-agents and prevents a
// sub-agent from escaping to Bash/Grep/Glob.
func TestSubAgentToolPropagation(t *testing.T) {
	require.Contains(t, SubAgents, "gortex-search.md")
	require.Contains(t, SubAgents, "gortex-impact.md")

	for name, def := range SubAgents {
		t.Run(name, func(t *testing.T) {
			tools := SubAgentTools(def)
			require.NotEmpty(t, tools,
				"%s must declare a tools allowlist so the sub-agent inherits Gortex tools", name)

			seen := map[string]bool{}
			for _, tool := range tools {
				require.Truef(t, strings.HasPrefix(tool, "mcp__gortex__"),
					"%s lists non-gortex tool %q — the allowlist must be graph-only (no Bash/Grep/Glob escape)", name, tool)
				require.Falsef(t, seen[tool], "%s lists duplicate tool %q", name, tool)
				seen[tool] = true
			}
			for _, required := range []string{"mcp__gortex__capabilities", "mcp__gortex__explore"} {
				require.Truef(t, seen[required], "%s should grant compact entry point %s", name, required)
			}
			for _, legacy := range []string{"mcp__gortex__smart_context", "mcp__gortex__search_symbols", "mcp__gortex__read_file"} {
				require.Falsef(t, seen[legacy], "%s should not grant legacy tool %s", name, legacy)
			}
		})
	}
}

func TestSubAgentBodiesUseOnlyCompactVocabulary(t *testing.T) {
	for name, def := range SubAgents {
		for _, legacy := range []string{"smart_context", "search_symbols", "get_symbol_source", "find_usages", "get_callers", "verify_change", "read_file"} {
			require.NotContainsf(t, def, legacy, "%s contains legacy MCP tool %q", name, legacy)
		}
		require.NotContains(t, def, "facade-v1")
		require.Equalf(t, 1, strings.Count(def, profiles.WorktreeBranchRoutingPolicy),
			"%s must embed the canonical worktree/branch policy exactly once", name)
	}
}

// TestSubAgentFrontmatterNameMatchesFile guards against a rename drifting the
// frontmatter `name:` away from the installed filename.
func TestSubAgentFrontmatterNameMatchesFile(t *testing.T) {
	for name, def := range SubAgents {
		base := strings.TrimSuffix(name, ".md")
		require.Containsf(t, def, "name: "+base,
			"%s frontmatter name must match its filename", name)
	}
}

func TestSubAgentToolsParsesEmpty(t *testing.T) {
	require.Nil(t, SubAgentTools("---\nname: x\ndescription: no tools line\n---\nbody"))
}
