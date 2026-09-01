package codeowners

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// CODEOWNERS globs were compiled by go-gitignore directly, which splices
// pattern text into a regexp with almost nothing escaped. A rule path
// carrying a regexp metacharacter therefore claimed files it never named
// — the same defect that emptied a whole index in #624, applied to
// ownership instead of indexing.
func TestMatchFile_RegexMetacharactersInPatternAreLiteral(t *testing.T) {
	rules := Parse([]byte("*$ @backup-team\n"))

	assert.Nil(t, MatchFile("src/app.go", rules),
		`"*$" must not own every file in the repository`)
	assert.Nil(t, MatchFile("README.md", rules))
	assert.Equal(t, []string{"@backup-team"}, MatchFile("notes.txt$", rules),
		`"*$" still owns the files it actually names`)
}

func TestMatchFile_AlternationIsLiteral(t *testing.T) {
	rules := Parse([]byte("api|web @platform\n"))

	assert.Nil(t, MatchFile("api", rules))
	assert.Nil(t, MatchFile("web", rules))
	assert.Equal(t, []string{"@platform"}, MatchFile("api|web", rules))
}

// A hand-constructed Rule takes the uncached compile path; it must get
// the same treatment.
func TestMatchFile_UncachedRuleUsesSameCompiler(t *testing.T) {
	rules := []Rule{{Pattern: "*$", Owners: []string{"@backup-team"}}}
	assert.Nil(t, MatchFile("src/app.go", rules))
}
