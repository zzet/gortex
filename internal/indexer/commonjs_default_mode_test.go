package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Under "type": "module" a .js importer is native ESM, and NodeNext applies the
// same rule to .ts. Neither may publish the compiled-interop .default unless
// native ESM and compiled interop justify the exact same callable.
func TestModulePackageDoesNotPublishCompiledDefault(t *testing.T) {
	const page = "import def from './lib.cjs'\nexport function run() { return def() }\n"
	for _, tc := range []struct {
		name     string
		lib      string
		mjsWant  string
		ctsWant  string
		unproven string // empty means must stay unresolved
	}{
		{
			name:    "distinct callables",
			lib:     "function main() {}\nfunction alt() {}\nmodule.exports = main\nmodule.exports.__esModule = true\nmodule.exports.default = alt\n",
			mjsWant: "lib.cjs::main",
			ctsWant: "lib.cjs::alt",
		},
		{
			name:    "interop property only",
			lib:     "function target() {}\nexports.__esModule = true\nexports.default = target\n",
			ctsWant: "lib.cjs::target",
		},
		{
			name:    "object container with interop default",
			lib:     "function target() {}\nvar api = {}\nmodule.exports = api\nmodule.exports.__esModule = true\nmodule.exports.default = target\n",
			ctsWant: "lib.cjs::target",
		},
		{
			name:     "same callable in both modes",
			lib:      "function target() {}\nmodule.exports = target\nmodule.exports.__esModule = true\nmodule.exports.default = target\n",
			mjsWant:  "lib.cjs::target",
			ctsWant:  "lib.cjs::target",
			unproven: "lib.cjs::target",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, content := range map[string]string{
				"package.json":  `{"type":"module"}`,
				"tsconfig.json": `{"compilerOptions":{"module":"NodeNext","moduleResolution":"NodeNext"}}`,
				"lib.cjs":       tc.lib,
				"native.js":     page,
				"nodenext.ts":   page,
				"explicit.mjs":  page,
				"compiled.cts":  page,
			} {
				writeFileMk(t, filepath.Join(root, rel), content)
			}
			idx, _ := newSQLiteIndexer(t)
			_, err := idx.Index(root)
			require.NoError(t, err)

			if tc.mjsWant != "" {
				require.Equal(t, tc.mjsWant, callTargetForName(t, idx.Graph(), "explicit.mjs", "run"))
			} else {
				target := callTargetForName(t, idx.Graph(), "explicit.mjs", "run")
				require.True(t, graph.IsUnresolvedTarget(target), "explicit.mjs resolved to %s", target)
			}
			if tc.ctsWant != "" {
				require.Equal(t, tc.ctsWant, callTargetForName(t, idx.Graph(), "compiled.cts", "run"))
			} else {
				target := callTargetForName(t, idx.Graph(), "compiled.cts", "run")
				require.True(t, graph.IsUnresolvedTarget(target), "compiled.cts resolved to %s", target)
			}
			for _, file := range []string{"native.js", "nodenext.ts"} {
				target := callTargetForName(t, idx.Graph(), file, "run")
				if tc.unproven != "" {
					require.Equal(t, tc.unproven, target, file)
				} else {
					require.True(t, graph.IsUnresolvedTarget(target), "%s resolved to %s", file, target)
				}
			}
		})
	}
}
