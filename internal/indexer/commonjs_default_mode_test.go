package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Under "type": "module" a .js importer is native ESM, and NodeNext applies the
// same rule to .ts. Neither may publish the compiled-interop .default.
func TestModulePackageDoesNotPublishCompiledDefault(t *testing.T) {
	root := t.TempDir()
	const page = "import def from './lib.cjs'\nexport function run() { return def() }\n"
	for rel, content := range map[string]string{
		"package.json":  `{"type":"module"}`,
		"tsconfig.json": `{"compilerOptions":{"module":"NodeNext","moduleResolution":"NodeNext"}}`,
		"lib.cjs":       "function main() {}\nfunction alt() {}\nmodule.exports = main\nmodule.exports.__esModule = true\nmodule.exports.default = alt\n",
		"native.js":     page,
		"nodenext.ts":   page,
		"explicit.mjs":  page,
	} {
		writeFileMk(t, filepath.Join(root, rel), content)
	}
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	require.Equal(t, "lib.cjs::main", callTargetForName(t, idx.Graph(), "explicit.mjs", "run"))
	for _, file := range []string{"native.js", "nodenext.ts"} {
		target := callTargetForName(t, idx.Graph(), file, "run")
		require.True(t, graph.IsUnresolvedTarget(target), "%s resolved to %s", file, target)
	}
}
