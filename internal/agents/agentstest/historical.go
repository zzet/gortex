package agentstest

import (
	"strings"

	"github.com/zzet/gortex/internal/toolref"
)

// BeforeTransientGuidance reconstructs the three shared policy lines present
// before #1030. Historical migration tests must compare genuinely old bytes
// against the frozen catalogs, rather than reinterpret today's text as shipped
// content. This helper never changes production ownership fingerprints.
func BeforeTransientGuidance(body string) string {
	body = strings.Replace(body, toolref.TransportClause()+" "+toolref.DegradedModeLine(),
		"Native Gortex MCP is mandatory for this MCP-configured workflow. If its callable tools are missing, report a Gortex MCP integration failure and stop; do not start a daemon or switch to a CLI/shell fallback.", 1)
	body = strings.Replace(body, "- When Gortex can serve the step, do not substitute shell reads, file search, or Git plumbing.",
		"- Do not substitute shell reads, file search, or Git plumbing.", 1)
	return strings.Replace(body, "- When Gortex can serve the step, use `change` before a mutation and again after it. Write only through `edit` or `refactor`.",
		"- Use `change` before a mutation and again after it. Write only through `edit` or `refactor`.", 1)
}
