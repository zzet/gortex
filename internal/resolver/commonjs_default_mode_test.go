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
		"plain module.exports": "function run(){}; module.exports = run",
		"same callable":        "function run(){}; module.exports = run; module.exports.__esModule = true; module.exports.default = run",
	} {
		t.Run(name, func(t *testing.T) {
			const page = "import def from './lib.cjs'; export function run(){def()}"
			g := buildGraphFromSources(t, map[string]string{
				"lib.cjs":      module,
				"native.mjs":   page,
				"compiled.cts": page,
				"page.js":      page,
				"page.ts":      page,
			})
			New(g).ResolveAll()
			for _, caller := range []string{"native.mjs::run", "compiled.cts::run", "page.js::run", "page.ts::run"} {
				require.Equal(t, "lib.cjs::run", callEdgeTo(g, caller, 1), caller)
			}
		})
	}
}

// An absent callable in either native ESM or compiled interop is disagreement
// when the consumer's module mode is unproven.
func TestCommonJSDefaultSingleModeNeedsProvenConsumerMode(t *testing.T) {
	const page = "import def from './lib.cjs'; export function run(){def()}"
	for _, tc := range []struct {
		name     string
		module   string
		native   string
		compiled string
	}{
		{
			name:     "compiled only",
			module:   "function run(){}; exports.__esModule = true; exports.default = run",
			native:   "unresolved::def",
			compiled: "lib.cjs::run",
		},
		{
			name:     "object container",
			module:   "function run(){}; var api = {}; module.exports = api; module.exports.__esModule = true; module.exports.default = run",
			native:   "unresolved::def",
			compiled: "lib.cjs::run",
		},
		{
			name:     "native only with esModule marker",
			module:   "function run(){}; module.exports = run; module.exports.__esModule = true",
			native:   "lib.cjs::run",
			compiled: "unresolved::def",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := buildGraphFromSources(t, map[string]string{
				"lib.cjs":      tc.module,
				"native.mjs":   page,
				"native.mts":   page,
				"compiled.cjs": page,
				"compiled.cts": page,
				"unproven.js":  page,
				"unproven.ts":  page,
			})
			New(g).ResolveAll()
			for caller, want := range map[string]string{
				"native.mjs::run":   tc.native,
				"native.mts::run":   tc.native,
				"compiled.cjs::run": tc.compiled,
				"compiled.cts::run": tc.compiled,
				"unproven.js::run":  "unresolved::def",
				"unproven.ts::run":  "unresolved::def",
			} {
				require.Equal(t, want, callEdgeTo(g, caller, 1), caller)
			}
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
