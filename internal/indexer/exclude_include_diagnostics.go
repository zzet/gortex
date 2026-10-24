package indexer

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/excludes"
	"go.uber.org/zap"
)

// Include diagnostics use the user's line, never the matcher compiler's
// inserted root anchor. Bare names and wildcards with no fixed parent get an
// informational semantics notice, not a claim that an existing file is blocked.
func (idx *Indexer) warnIncludes(m *excludes.Matcher) {
	if idx.logger == nil {
		return
	}
	for _, raw := range idx.config.Include {
		p := strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(raw)), "!"), "./")
		body := strings.Trim(p, "/")
		if body == "" {
			continue
		}
		fields := []zap.Field{zap.String("repo", idx.repoPrefix), zap.String("pattern", raw)}
		if inGitDir(body) {
			if idx.firstReincludeWarning("", "include-hard", raw) {
				idx.logger.Warn("indexer: ignoring an include under .git; Git's own data is never indexed", fields...)
			}
			continue
		}
		if excludes.InAgentConfigDir(body) && !excludes.IsAgentConfigMCPDir(body) && !excludes.IsAgentConfigMCPFile(body) {
			if idx.firstReincludeWarning("", "include-hard", raw) {
				idx.logger.Warn("indexer: ignoring an include under an agent-config folder; non-MCP agent state is never indexed", fields...)
			}
			continue
		}
		wildcard := strings.ContainsAny(p, `*?[\`)
		if wildcard && strings.Contains(strings.TrimSuffix(p, "/"), "/") {
			// Includes with an inner slash are rooted, unlike exclude literals.
			if parent, rule := m.ExcludedNegationParent("!/" + strings.TrimPrefix(p, "/")); parent != "" && m.MatchRel(parent+"/") {
				if idx.firstReincludeWarning("", "include-parent", raw) {
					idx.logger.Warn("indexer: include is blocked by an excluded parent; use an anchored literal include to keep its ancestors open",
						append(fields, zap.String("parent", parent), zap.String("excluded_by", rule))...)
				}
				continue
			}
		}
		if !strings.HasPrefix(p, "/") && !strings.HasPrefix(body, "**/") && strings.Contains(strings.TrimSuffix(p, "/"), "/") {
			if idx.firstReincludeWarning("", "include-anchor", raw) {
				idx.logger.Info("indexer: include with an inner slash is now root-anchored; it no longer matches the same path below other folders", fields...)
			}
		} else if wildcard || (!strings.HasPrefix(p, "/") && !strings.Contains(body, "/")) {
			if idx.firstReincludeWarning("", "include-parent-limited", raw) {
				idx.logger.Info("indexer: bare-name and wildcard includes can reopen matching folders only when their parents are open; they cannot reach through excluded ancestors", fields...)
			}
		}
	}
}
