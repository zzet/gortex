package claudecode

import (
	"strings"

	"github.com/zzet/gortex/internal/agents"
)

// SubAgents maps the filename under .claude/agents/ to a graph-only
// sub-agent definition. Each allowlist names tools present on the compact MCP
// surface received by every named client.
var SubAgents = agents.SubAgents

// SubAgentTools parses the tools allowlist from YAML frontmatter.
func SubAgentTools(def string) []string {
	for _, line := range strings.Split(def, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "tools:") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(t, "tools:"))
		var out []string
		for _, name := range strings.Split(rest, ",") {
			if n := strings.TrimSpace(name); n != "" {
				out = append(out, n)
			}
		}
		return out
	}
	return nil
}
