package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/excludes"
)

// pandasIgnoreExcerpt is the head of pandas-dev/pandas' .gitignore,
// verbatim. Line 7, "*$", is an Emacs autosave pattern; go-gitignore used
// to compile it into a regexp whose "$" was an end-of-text anchor, so it
// matched every path in the repository. `gortex track` then reported
// success on a repo holding 1,519 .py files and produced an index with
// zero source files in it, with no error or warning anywhere (#624).
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
doc/_build
dist
*.egg-info
__pycache__

# OS generated files #
######################
.DS_Store
Icon?
`

// A repo's own .gitignore must never be able to exclude the whole repo by
// accident. This walks the same layering `gortex track` uses: the file on
// disk, through the ConfigManager's exclude chain, into the matcher the
// index walk consults.
func TestEffectiveExclude_PandasGitignoreKeepsSourceFiles(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pandas", "core"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	write := func(rel, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644))
	}
	write(".gitignore", pandasIgnoreExcerpt)
	write("pyproject.toml", "[project]\nname = \"pandas\"\n")
	write("setup.py", "print(1)\n")
	write(filepath.Join("pandas", "__init__.py"), "x = 1\n")
	write(filepath.Join("pandas", "core", "frame.py"), "class DataFrame: pass\n")

	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	cm.LoadWorkspaceConfig("pandas", root)
	m := excludes.New(cm.EffectiveExclude("pandas"))

	for _, rel := range []string{
		"pyproject.toml",
		"setup.py",
		"pandas/__init__.py",
		"pandas/core/frame.py",
		"pandas/core/",
		"pandas/",
	} {
		excluded, pattern := m.Explain(rel)
		require.Falsef(t, excluded, "%s must be indexable; excluded by pattern %q", rel, pattern)
	}

	// The ignore file still does its actual job.
	for _, rel := range []string{
		"pandas/core/frame.pyc",
		"pandas/_libs/algos.so",
		"build/lib/x.py",
		"pandas/core/.frame.py~",
		"#frame.py#",
		"pandas.egg-info/PKG-INFO",
		"__pycache__/frame.cpython-312.pyc",
	} {
		require.Truef(t, m.MatchRel(rel), "%s should still be excluded", rel)
	}
}
