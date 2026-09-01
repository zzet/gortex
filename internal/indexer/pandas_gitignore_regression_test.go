package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// pandasIgnoreExcerpt is the head of pandas-dev/pandas' .gitignore,
// verbatim. Line 7 is "*$" — an Emacs autosave pattern that used to
// compile into a regexp matching every path in the repository, so
// `gortex track` on pandas reported success while indexing none of its
// 1,519 Python files (#624).
const pandasIgnoreExcerpt = `#########################################
# Editor temporary/working/backup files #
.#*
*\#*\#
[#]*#
*~
*$
*.bak
*.log

# Compiled source #
###################
*.pxi
*.o
*.py[ocd]
*.so
MANIFEST

# Python files #
################
build
dist
*.egg-info
__pycache__

# OS generated files #
######################
.DS_Store
Icon?
`

// The whole pipeline the report exercised: a .gitignore on disk, through
// the config manager's exclude layering, into a real index pass.
func TestMultiIndexer_PandasGitignoreDoesNotEmptyTheRepo(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".gitignore":     pandasIgnoreExcerpt,
		"main.go":        "package main\n\nfunc main() {}\n",
		"core/frame.go":  "package core\n\nfunc DataFrame() {}\n",
		"core/series.go": "package core\n\nfunc Series() {}\n",
		// Still ignored, by patterns that were never the problem.
		"build/gen.go":      "package build\n",
		"core/.frame.go~":   "package core\n",
		"dist/bundled.go":   "package dist\n",
		"pkg.egg-info/x.go": "package egg\n",
	}
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
	}

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: root, Name: "pandas"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	g := graph.New()
	mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())

	results, err := mi.IndexAll()
	require.NoError(t, err)
	require.Contains(t, results, "pandas")
	assert.Equal(t, 3, results["pandas"].FileCount,
		"the three source files must be indexed; the ignore file's other patterns still apply")

	require.NotEmpty(t, g.FindNodesByName("DataFrame"),
		"a symbol from a source file the ignore rules never named must reach the graph")
	assert.Empty(t, g.FindNodesByName("Series0"))
}
