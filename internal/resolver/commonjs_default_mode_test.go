package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const distinctCommonJSDefaults = "function main(){}; function alt(){}; module.exports = main; module.exports.__esModule = true; module.exports.default = alt"

func TestCommonJSDefaultNeedsProvenConsumerMode(t *testing.T) {
	const page = "import def from './lib.cjs'; export function run(){def()}"
	g := buildGraphFromSources(t, map[string]string{
		"lib.cjs":      distinctCommonJSDefaults,
		"native.mjs":   page,
		"native.mts":   page,
		"compiled.cts": page,
		"unproven.js":  page,
		"unproven.ts":  page,
	})
	New(g).ResolveAll()
	for caller, want := range map[string]string{
		"native.mjs::run":   "lib.cjs::main",
		"native.mts::run":   "lib.cjs::main",
		"compiled.cts::run": "lib.cjs::alt",
		"unproven.js::run":  "unresolved::def",
		"unproven.ts::run":  "unresolved::def",
	} {
		require.Equal(t, want, callEdgeTo(g, caller, 1), caller)
	}
}

func TestCommonJSDefaultWithoutConflictStillBinds(t *testing.T) {
	for name, module := range map[string]string{
		"same callable":    "function run(){}; module.exports = run; module.exports.__esModule = true; module.exports.default = run",
		"compiled only":    "function run(){}; exports.__esModule = true; exports.default = run",
		"object container": "function run(){}; var api = {}; module.exports = api; module.exports.__esModule = true; module.exports.default = run",
	} {
		t.Run(name, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"lib.cjs": module,
				"page.js": "import def from './lib.cjs'; export function run(){def()}",
			})
			New(g).ResolveAll()
			require.Equal(t, "lib.cjs::run", callEdgeTo(g, "page.js::run", 1))
		})
	}
}

// A container reassigned to a callable competes with .default again.
func TestCommonJSReassignedContainerIsNotAnObject(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"lib.cjs": "function main(){}; function alt(){}; var api = {}; api = main; module.exports = api; module.exports.__esModule = true; module.exports.default = alt",
		"page.js": "import def from './lib.cjs'; export function run(){def()}",
	})
	New(g).ResolveAll()
	require.Equal(t, "unresolved::def", callEdgeTo(g, "page.js::run", 1))
}
