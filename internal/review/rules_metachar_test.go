package review

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Review rule globs were compiled by go-gitignore directly, which
// splices pattern text into a regexp with almost nothing escaped. A rule
// path holding a regexp metacharacter therefore governed files it never
// named — the ownership-side twin of the indexing failure in #624.
func TestRuleFor_RegexMetacharactersInRulePathAreLiteral(t *testing.T) {
	dir := t.TempDir()
	rulesPath := filepath.Join(dir, "review.yaml")
	require.NoError(t, os.WriteFile(rulesPath, []byte(`rules:
  - name: backups
    path: "*$"
    severity: critical
  - name: services
    path: "api|web"
    severity: critical
`), 0o644))

	r, err := NewRuleResolver(rulesPath, "")
	require.NoError(t, err)

	for _, f := range []string{"internal/app.go", "README.md", "api", "web"} {
		rule, ok := r.RuleFor(f)
		require.True(t, ok, "the embedded catch-all always matches")
		assert.NotEqual(t, "critical", rule.Severity,
			"%s must not be governed by a rule whose path never named it", f)
	}

	rule, ok := r.RuleFor("dump.sql$")
	require.True(t, ok)
	assert.Equal(t, "critical", rule.Severity,
		`"*$" still governs the files it actually names`)
}
