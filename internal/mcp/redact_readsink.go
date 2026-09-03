package mcp

import "github.com/zzet/gortex/internal/review"

// maybeRedactConfigLeaf withholds secret-shaped values from the body of a
// config / data-leaf file before it is served by a content read sink. It
// returns the (possibly redacted) body and whether anything was withheld.
// Redaction is skipped when the caller passed allow_secrets, when the file is
// not a config leaf, when the owning repo disabled it, or when nothing
// secret-shaped is present — so benign config is returned untouched.
func (s *Server) maybeRedactConfigLeaf(language, relPath string, allowSecrets bool, content string) (string, bool) {
	if allowSecrets {
		return content, false
	}
	if !review.IsConfigLeafLanguage(language) && !review.IsConfigLeafPath(relPath) {
		return content, false
	}
	if !s.redactConfigSecretsEnabled(relPath) {
		return content, false
	}
	red, hits := review.RedactConfigLeaf(content)
	if hits == 0 {
		return content, false
	}
	return red, true
}

// redactConfigSecretsEnabled reports whether config-leaf secret redaction is
// active for the repo that owns relPath. It defaults to on (the safe default)
// when no config manager is wired — the single-repo and test paths.
func (s *Server) redactConfigSecretsEnabled(relPath string) bool {
	if s.configManager == nil {
		return true
	}
	// Base read: the repo prefix keys a config lookup, and config is a
	// property of the indexed repo, not of a session's edits.
	return s.configManager.GetRepoConfig(repoPrefixForPath(s.graph, relPath)).MCP.RedactConfigSecretsEnabled()
}
