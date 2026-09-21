package indexer

import (
	"path"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/resolver"
)

// Import-placement parity: closureWalk.importedFiles against the resolver's
// own import cascade.
//
// importedFiles claims, in a comment, to place an import exactly where the
// resolver's cascade could bind it. Nothing pinned that claim. The differential
// below states it as two checkable halves over a fixture the real indexer has
// indexed with the real resolver:
//
//   - NO DROP (the hard half). Every in-repo file the resolver actually bound
//     an import edge to is a file the closure places for that same importer. A
//     generation missing one re-derives its importer against a world the
//     import cannot bind in — a divergence from a whole index, which is the
//     one failure mode the closure exists to prevent.
//   - NO STRAY (the bounded half). Every file the closure places sits in a
//     directory the resolver's OWN import index offers for that specifier —
//     the directory the path names, a directory whose last component matches,
//     or the relative join — and, when the specifier is a bare JS/TS one, in a
//     directory that holds a module entry point, which is the single gate
//     `consider` applies to a same-repo candidate (resolver.go:4008-4010).
//     The relative join is the only one of the three that can SUPPRESS the
//     others, and only when it answers for every importer: resolveImport takes
//     the relative arm's result only `if to != ""` (resolver.go:3906), so a
//     miss reaches the cascade with the raw `./…` payload.
//
// NO STRAY is deliberately NOT "every file the resolver actually bound". The
// resolver's same-repo branch takes the FIRST candidate the scan reaches with
// no precision test at all (resolver.go:4062-4070; its own words, at
// jsts_imports.go:276-277: "the same-repo branch of `consider` accepts the
// first one with no further check"), in whatever order buildDirIndexes
// bucketed the corpus. Which of several same-named directories that lands on
// is not a function of anything the closure can see, so every candidate has to
// be placed. TestImportPlacementKeepsEverySameNamedCandidateDirectory states
// that as a rule of its own.
//
// The vocabulary the differential feeds in is the production one: each fixture
// file is run through closureWalk.extractInto into a real closureRefs, so the
// specifiers AND their importer attribution are exactly what collectIntroduced
// would hand importedFiles. That is also the only way the C/C++ half of the
// fixture is covered at all — a `#include` produces no KindImport node with a
// path meta, only an `unresolved::import::<name>` edge endpoint that
// closureRefs.addEndpoint reads.

// importParityFixture is a repository whose import shapes are the ones the
// cascade resolves differently: a package-shaped specifier with two same-named
// directories, a Python one whose candidates straddle a top-level directory, a
// relative one, a bare JS/TS one with no entry point, a bare JS/TS one with two
// entry points, a monorepo barrel, and a C++ translation unit.
var importParityFixture = map[string]string{
	"go.mod": "module example.com/fixture\n\ngo 1.24\n",

	// A. A package-shaped Go specifier. `example.com/fixture/util` spells out
	// util/, and internal/util/ merely shares its last path component — but
	// the resolver has no rule that prefers one over the other, so both are
	// candidates and both have to be placed.
	"core.go": `package fixture

import "example.com/fixture/util"

func Compute() int { return util.Helper() }
`,
	"util/helper.go": `package util

func Helper() int { return 1 }
`,
	"internal/util/helper.go": `package util

func Other() int { return 2 }
`,

	// B. The same shape in Python, with one candidate directory at the
	// repository root. The root one is what a prefixed exact-directory probe
	// short-circuits on, hiding deep/pyutil/ — which is the directory the
	// resolver was measured to bind.
	"app/main.py": `import pyutil


def go():
    return pyutil.run()
`,
	"pyutil/__init__.py": `def run():
    return 1
`,
	"deep/pyutil/__init__.py": `def run():
    return 2
`,

	// C. JS/TS. A relative specifier with a binding, a side-effect-only
	// relative specifier, a bare specifier whose name matches an in-repo
	// directory holding NO entry point, and a bare specifier with two
	// candidate directories that both hold one.
	"web/app.ts": `import { log } from 'shared/b/logger';
import { auth } from './auth';
import './cfg';
import { parse } from 'graphql';

export function run() { return log() + auth() + parse(); }
`,
	"web/auth.ts":                   "export function auth() { return 1; }\n",
	"web/cfg.ts":                    "export const closureOnlyCfg = 1;\n",
	"web/feature/graphql/schema.ts": "export function parse() { return 2; }\n",
	"a/logger/index.ts":             "export function log() { return 1; }\n",
	"b/logger/index.ts":             "export function log() { return 2; }\n",

	// C2. The relative MISS. deep/ holds no `auth` module at all, so the
	// resolver's relative arm returns "" (resolveJSTSImportTarget,
	// jsts_imports.go:99-117) and resolveImport carries on to the cascade
	// with the raw `./auth` — where lastPathComponent is `auth`, bareJSTS is
	// false, and `consider` binds the first misc/auth/ candidate it reaches
	// (resolver.go:3906, :3766, :3815-3822). A closure that treats the join
	// as terminal places nothing here and DROPS whatever the resolver bound.
	// misc/auth/ carries a sibling beside its barrel so the placement has to
	// be the whole candidate directory, not the entry point alone.
	"deep/app.ts": `import { auth } from './auth';

export function boot() { return auth(); }
`,
	"misc/auth/index.ts": "export { auth } from './side';\n",
	"misc/auth/side.ts":  "export function auth() { return 7; }\n",

	// C3. The relative HIT onto a directory barrel, with a same-named
	// decoy directory elsewhere. The join answers, so the cascade is never
	// reached and `other/local/` must NOT be placed — this is the narrowing
	// the relative arm buys, and it has to survive the miss fall-through.
	"local/app.ts": `import { here } from './local';

export function boot() { return here(); }
`,
	"local/local/index.ts": "export { here } from './impl';\n",
	"local/local/impl.ts":  "export function here() { return 8; }\n",
	"other/local/index.ts": "export function here() { return 9; }\n",

	// C4. The relative miss the JOIN nevertheless reaches — a JS/TS importer
	// whose sibling carries a NON-JS/TS extension. probeJSTSFile
	// (jsts_imports.go:156-200) probes only jsTSImportExts, so `mixed/svc.py`
	// is invisible to it: resolveJSTSImportTarget returns "" and resolveImport
	// carries on to the cascade, where lastDirIndex["svc"] offers svcdir/svc/.
	// A closure that answers on the wider closureModuleProbes list (which
	// carries `.py` so that what resolveRelativeImports binds in its own later
	// pass is still PLACED) suppresses that cascade and drops the binding.
	"mixed/app.ts": `import './svc';

export function boot() { return 1; }
`,
	"mixed/svc.py":        "def svc():\n    return 1\n",
	"svcdir/svc/index.ts": "export { svc } from './side';\n",
	"svcdir/svc/side.ts":  "export function svc() { return 2; }\n",

	// C5. The same miss for the other half of the resolver's precondition:
	// a NON-JS/TS importer. resolveJSTSImportTarget rejects the caller
	// outright (jsts_imports.go:99-101), so the relative arm can never
	// terminate resolveImport for it and the cascade is always reached —
	// no matter how well the join itself probes. The Ruby sibling is exactly
	// the join a language-blind arm would answer on.
	"rb/app.rb": "require_relative './svc2'\n\ndef run\n  1\nend\n",
	// …and its JS/TS neighbour, which fails the OTHER half: the caller is
	// JS/TS but the probe set is not, so the join still cannot answer.
	"rb/app.ts": `import './svc2';

export function boot() { return 1; }
`,
	"rb/svc2.rb":            "def svc2\n  1\nend\n",
	"svcdir2/svc2/index.ts": "export { svc2 } from './side';\n",
	"svcdir2/svc2/side.ts":  "export function svc2() { return 3; }\n",

	// C6. The importer-language precondition ALONE, with the probe-set half
	// held constant: a non-JS/TS importer whose relative join lands squarely
	// on a JS/TS file. probeJSTSFile would answer for that stem — but
	// resolveJSTSImportTarget never runs it, because it rejects the caller
	// first (jsts_imports.go:99-101). So resolveImport still reaches the
	// cascade and binds a svcdir3/svc3/ candidate. Without C6 the language
	// precondition is unobservable: every other non-JS/TS importer in this
	// fixture joins onto a non-JS/TS sibling, which the probe set rejects on
	// its own.
	"rbjs/app.rb":           "require_relative './svc3'\n\ndef run\n  1\nend\n",
	"rbjs/svc3.ts":          "export function svc3() { return 4; }\n",
	"svcdir3/svc3/index.ts": "export { svc3 } from './side';\n",
	"svcdir3/svc3/side.ts":  "export function svc3() { return 5; }\n",

	// C7. probeJSTSFile's single-file-component EARLY RETURN, isolated. A
	// `.vue`/`.svelte`/`.astro` stem is probed verbatim and the case returns
	// right there (jsts_imports.go:168-174) — the stem is never extended with
	// a module extension and never probed as a directory barrel. `sfc/W.vue`
	// does not exist, so resolveJSTSImportTarget returns "" and resolveImport
	// reaches the cascade with the raw `./W.vue`, whose last component names
	// wdir/W.vue/.
	//
	// `sfc/W.vue.ts` is the trap: an arm that fell THROUGH the SFC case into
	// the `stem + ext` loop would answer on it — a file probeJSTSFile never
	// looks at — and suppress that cascade. Placing it is still right, the
	// way every closureModuleProbes hit is.
	"sfc/app.ts": `import './W.vue';

export function boot() { return 1; }
`,
	"sfc/W.vue.ts":        "export function widget() { return 6; }\n",
	"wdir/W.vue/index.ts": "export { widget } from './side';\n",
	"wdir/W.vue/side.ts":  "export function widget() { return 7; }\n",

	// D. The monorepo shape the entry-point rule must not break: a bare
	// specifier naming a directory that does hold an entry point. The whole
	// directory joins, barrel and sibling alike.
	"mono/app.ts": `import { emit } from 'barrel';

export function boot() { return emit(); }
`,
	"packages/app/barrel/index.ts": "export { emit } from './write';\n",
	"packages/app/barrel/write.ts": "export function emit() { return 3; }\n",

	// E. A C++ translation unit: an angle include whose name matches two
	// in-repo directories, and a quoted include of a real sibling header the
	// generic cascade leaves external.
	"native/main.cpp": `#include <vecs>
#include "helper.h"

int main() { return 0; }
`,
	"native/helper.h":    "#pragma once\nint helper();\n",
	"native/vecs/impl.h": "#pragma once\nint impl();\n",
	"extra/vecs/other.h": "#pragma once\nint other();\n",

	// F. The QUALIFIED-NAME arm (resolver.go:3961-3972), which runs ahead of
	// the whole cascade and binds a node by its QualName with no directory
	// involved — across language families. The Kubernetes extractor stamps
	// `<kind>/<name>` as the resource node's qualified name
	// (parser/languages/kubernetes.go:89), so a bare TypeScript specifier
	// spelled `Service/logger` binds the YAML resource node in k8s/svc.yaml.
	// Measured: the indexed graph carries
	// `imports repo/qual/app.ts -> repo/k8s::Service::_default::logger`.
	//
	// Neither cascade key names k8s/: dirIndex has no `Service/logger`
	// directory, and lastDirIndex["logger"] offers a/logger and b/logger — the
	// decoys that make the case non-vacuous. A cascade-only closure therefore
	// DROPS the file the resolver actually bound, which is why the arm is
	// mirrored rather than declared a superset-only residual.
	"qual/app.ts": `import { ping } from 'Service/logger';

export function boot() { return ping(); }
`,
	"k8s/svc.yaml": `apiVersion: v1
kind: Service
metadata:
  name: logger
spec:
  ports:
    - port: 80
`,

	// G. The relative-import PASS (relative_imports.go), whose specifiers carry
	// no `./` prefix and therefore reach neither the relative join nor the
	// cascade. Three shapes at once — a C-family quoted include of a
	// same-directory header (resolveCInclude, :96-149), a Python relative import
	// the extractor has already joined onto its package (resolvePython, :60-68),
	// and a PHP literal `require __DIR__ . '/lib.php'` (resolvePhpInclude,
	// :320-…).
	//
	// The closure mirrors NONE of them, and these files are what measures why:
	// every one of these imports is still `external::…` in the indexed fixture,
	// because resolveImport stamped that target on the edge (resolver.go:4123)
	// before the pass could test for the `unresolved::import::` prefix its
	// C-family and PHP arms require. NO DROP is checked against whatever the
	// real pass did, so the day it binds one of these the differential says so.
	"csame/main.cpp": `#include "sibling.h"

int csame() { return 0; }
`,
	"csame/sibling.h": "#pragma once\nint sibling();\n",
	"pypkg/__init__.py": `def pkg():
    return 0
`,
	"pypkg/app.py": `from . import leaf


def go():
    return leaf.run()
`,
	"pypkg/leaf.py": `def run():
    return 1
`,
	"phpdir/app.php": `<?php
require_once __DIR__ . '/lib.php';
`,
	"phpdir/lib.php": `<?php
function libfn() { return 1; }
`,

	// H. The npm-manifest gate (declaresExternalNpmDep, resolver.go:4000),
	// applied at :3907 to skip the cascade outright for a bare specifier the
	// importer's package.json declares a registry dependency. The closure reads
	// no manifest, so it keeps placing the in-repo directory — a SUPERSET, the
	// direction the closure is allowed to err in, and the differential is what
	// states that it stays a superset rather than becoming a drop.
	"npmapp/package.json": `{
  "name": "npmapp",
  "dependencies": { "lodashish": "^4.0.0" }
}
`,
	"npmapp/app.ts": `import { chunk } from 'lodashish';

export function boot() { return chunk(); }
`,
	"vendored/lodashish/index.ts": "export function chunk() { return 1; }\n",
}

// TestImportPlacementParityWithResolverCascade is the differential harness.
func TestImportPlacementParityWithResolverCascade(t *testing.T) {
	store, walk := importParityCase(t, importParityFixture)
	refs := importParityVocabulary(t, walk, importParityFixture)

	for _, importer := range importParityImporters(refs) {
		t.Run(importer, func(t *testing.T) {
			oracleFiles, oracleDirs := importParityOracle(t, store, builderGraphPath(builderRepoPrefix, importer))
			placements := importParityPlacements(walk, refs, importer)

			placed := map[string]struct{}{}
			var flat []string
			for _, p := range placements {
				t.Logf("spec %q -> %v", p.spec, p.placed)
				for _, f := range p.placed {
					if _, dup := placed[f]; dup {
						continue
					}
					placed[f] = struct{}{}
					flat = append(flat, f)
				}
			}
			sort.Strings(flat)
			t.Logf("resolver binds=%v (dirs %v)", oracleFiles, sortedKeys(oracleDirs))
			t.Logf("closure places=%v", flat)

			// NO DROP.
			for _, bound := range oracleFiles {
				if _, ok := placed[bound]; !ok {
					t.Errorf("closure DROPS %q, which the resolver binds an import out of %s to; it places %v",
						bound, importer, flat)
				}
			}

			// NO STRAY.
			for _, p := range placements {
				specCallers := refs.importCallers(p.spec)
				// The one arm that never reaches `consider`: the qualified-name
				// arm. It offers a file in a directory the cascade's own two
				// keys do not name, and it is not subject to the entry-point
				// gate. Read off the store's qualified-name index — the
				// resolver's own primitive — so the exemption cannot be a
				// restatement of the placement it exempts.
				// The exemption is per FILE, not per directory: the arm binds a
				// NODE and the closure places that node's file, so exempting
				// its whole directory would authorise placements the arm never
				// offers.
				offers := importParityQualNameOffers(walk, p.spec)
				for _, f := range p.placed {
					if _, offered := offers[f]; offered {
						continue
					}
					if !importParityCandidateDir(walk, p.spec, specCallers, path.Dir(f)) {
						t.Errorf("closure places %q for specifier %q in %s, but %q is not a directory the "+
							"resolver's import index offers for that specifier",
							f, p.spec, importer, path.Dir(f))
					}
				}
				if !importParityBareJSTS(p.spec, refs.importCallers(p.spec)) {
					continue
				}
				for _, f := range p.placed {
					if _, offered := offers[f]; offered {
						continue
					}
					if !importParityDirHasEntryPoint(walk, path.Dir(f)) {
						t.Errorf("closure places %q for the bare JS/TS specifier %q in %s, but %q holds no "+
							"module entry point, so `consider` (resolver.go:4008-4010) rejects every "+
							"candidate in it",
							f, p.spec, importer, path.Dir(f))
					}
				}
			}
		})
	}
}

// TestImportPlacementExercisesTheLastComponentArm keeps the differential
// honest about WHICH arm it is pinning.
//
// resolveImport answers a package-shaped specifier from the qualified-name
// index (resolver.go:3961-3972) BEFORE the dirIndex/lastDirIndex cascade is
// reached, and from a relative join before that. An agreement between the
// closure's last-component arm and a resolver answer produced on one of those
// earlier arms would be a fixture coincidence, not parity — so this asserts
// that for each specifier the cascade cases rest on, no earlier arm could have
// fired: nothing in the corpus carries that qualified name, and the specifier
// is neither relative nor a directory the path names outright.
func TestImportPlacementExercisesTheLastComponentArm(t *testing.T) {
	store, walk := importParityCase(t, importParityFixture)

	for _, spec := range []string{
		"example.com/fixture/util", "pyutil", "shared/b/logger", "vecs", "graphql", "barrel",
	} {
		t.Run(spec, func(t *testing.T) {
			if node := store.GetNodeByQualName(spec); node != nil {
				t.Fatalf("the qualified-name arm (resolver.go:3961-3972) can fire for %q — it names %q "+
					"(kind %s) — so this case does not exercise the last-component arm",
					spec, node.ID, node.Kind)
			}
			if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
				t.Fatalf("%q is relative; the cascade is never reached", spec)
			}
			walk.buildDirIndexes()
			if files := walk.dirIndex[spec]; len(files) > 0 {
				t.Fatalf("%q names a directory outright (%v); the first cascade arm answers it, "+
					"not the last-component one", spec, files)
			}
			if files := walk.lastDirIndex[path.Base(spec)]; len(files) == 0 {
				t.Fatalf("%q reaches no last-component candidate at all; the case is vacuous", spec)
			}
		})
	}
}

// TestImportPlacementKeepsEverySameNamedCandidateDirectory is the rule the
// closure may not narrow past.
//
// The resolver's same-repo branch binds the FIRST candidate its scan reaches
// (resolver.go:4062-4070, short-circuited by stop() at :3805/:3812) and applies
// NO precision test to it. dirMatchesImport (resolver.go:5984-5996) looks like
// that test but is not: its own contract restricts it to authorising
// CROSS-repo candidates (":5749-5752"), and jsts_imports.go:290-293 rejects
// applying it to the same-repo branch by name. Every candidate this walk sees
// is same-repo (buildDirIndexes keeps only owned paths), so narrowing by
// import-path suffix here drops files the resolver binds — measured: an
// `import "example.com/fixture/util"` out of a Go file binds to
// `deep/util/__init__.py` when that row sorts first.
//
// Each case below therefore expects EVERY candidate directory's files.
func TestImportPlacementKeepsEverySameNamedCandidateDirectory(t *testing.T) {
	_, walk := importParityCase(t, importParityFixture)

	for _, tc := range []struct {
		name    string
		spec    string
		callers []string
		want    []string
	}{{
		// A suffix test would keep util/ and drop internal/util/.
		name:    "package-shaped specifier keeps the directory it does not spell out",
		spec:    "example.com/fixture/util",
		callers: []string{"core.go"},
		want:    []string{"repo/internal/util/helper.go", "repo/util/helper.go"},
	}, {
		// A prefixed exact-directory probe would return repo/pyutil/ and stop.
		name:    "root candidate directory does not short-circuit the last-component arm",
		spec:    "pyutil",
		callers: []string{"app/main.py"},
		want:    []string{"repo/deep/pyutil/__init__.py", "repo/pyutil/__init__.py"},
	}, {
		// A suffix test would keep b/logger (a suffix of shared/b/logger) and
		// drop a/logger — which is the directory the resolver was measured to
		// bind.
		name:    "bare js specifier keeps every entry-point directory",
		spec:    "shared/b/logger",
		callers: []string{"web/app.ts"},
		want:    []string{"repo/a/logger/index.ts", "repo/b/logger/index.ts"},
	}, {
		name:    "non-js specifier keeps every same-named directory",
		spec:    "vecs",
		callers: []string{"native/main.cpp"},
		want:    []string{"repo/extra/vecs/other.h", "repo/native/vecs/impl.h"},
	}, {
		// The relative arm is a FIRST TRY, not a terminal one. `mono/` holds
		// no `auth` module, so resolveJSTSImportTarget returns "" and
		// resolveImport carries on with the RAW `./auth`
		// (resolver.go:3906-3914): lastPathComponent is `auth`
		// (resolver.go:5960-5966), bareJSTS is false for a `./` prefix
		// (jsts_imports.go:253-264) so `consider` applies NO gate, and the
		// first misc/auth/ candidate binds (resolver.go:4043-4049).
		// Refusing to fall through here is a DROP, not a narrowing.
		name:    "relative specifier that misses falls into the last-component arm",
		spec:    "./auth",
		callers: []string{"mono/app.ts"},
		want:    []string{"repo/misc/auth/index.ts", "repo/misc/auth/side.ts"},
	}, {
		// A caller set that mixes a hit with a miss is the union of both
		// arms: web/ joins onto its own auth.ts, mono/ reaches the cascade.
		// Placing only one of the two drops an import the resolver binds.
		name:    "a mixed caller set carries both the join and the cascade",
		spec:    "./auth",
		callers: []string{"mono/app.ts", "web/app.ts"},
		want: []string{
			"repo/misc/auth/index.ts", "repo/misc/auth/side.ts", "repo/web/auth.ts",
		},
	}, {
		// A join that lands on a NON-JS/TS sibling is not an answer.
		// probeJSTSFile (jsts_imports.go:156-200) probes jsTSImportExts only,
		// so `mixed/svc.py` never terminates resolveImport's relative arm and
		// the cascade runs with the raw `./svc`. The `.py` file is still
		// PLACED — resolveRelativeImports binds it in a later pass
		// (relative_imports.go:24-33) and over-placing is free — but it may
		// not SUPPRESS svcdir/svc/.
		name:    "a non-js sibling places but does not answer",
		spec:    "./svc",
		callers: []string{"mixed/app.ts"},
		want: []string{
			"repo/mixed/svc.py", "repo/svcdir/svc/index.ts", "repo/svcdir/svc/side.ts",
		},
	}, {
		// The same shape out of a non-JS/TS importer. Both halves of the
		// resolver's precondition fail here at once, which is why the case
		// below exists to isolate the language half on its own.
		name:    "a non-js importer with a non-js sibling reaches the cascade",
		spec:    "./svc2",
		callers: []string{"rb/app.rb"},
		want: []string{
			"repo/rb/svc2.rb", "repo/svcdir2/svc2/index.ts", "repo/svcdir2/svc2/side.ts",
		},
	}, {
		// The importer-LANGUAGE half, isolated. The join lands on a JS/TS
		// file, so probeJSTSFile itself would answer for this stem — but
		// resolveJSTSImportTarget rejects the caller before it ever probes
		// (jsts_imports.go:99-101, `if !isJSTSPath(callerFile) { return "" }`),
		// so resolveImport reaches the cascade anyway and svcdir3/svc3/ must
		// be placed. Dropping the language check while keeping the exact
		// probe set is invisible to every other case in this file.
		name:    "a non-js importer does not answer even on a js/ts join",
		spec:    "./svc3",
		callers: []string{"rbjs/app.rb"},
		want: []string{
			"repo/rbjs/svc3.ts", "repo/svcdir3/svc3/index.ts", "repo/svcdir3/svc3/side.ts",
		},
	}, {
		// And the two halves are independent: a JS/TS importer joining onto a
		// Ruby sibling still reaches the cascade, because the probe set is
		// what failed, not the caller language.
		name:    "a js importer joining a non-js sibling still reaches the cascade",
		spec:    "./svc2",
		callers: []string{"rb/app.ts"},
		want: []string{
			"repo/rb/svc2.rb", "repo/svcdir2/svc2/index.ts", "repo/svcdir2/svc2/side.ts",
		},
	}, {
		// probeJSTSFile's single-file-component case is a VERBATIM probe with
		// an EARLY RETURN (jsts_imports.go:168-174): `sfc/W.vue` is absent, so
		// the case returns "" without ever trying `sfc/W.vue.ts` or
		// `sfc/W.vue/index.ts`, resolveJSTSImportTarget returns "", and
		// resolveImport reaches the cascade with the raw `./W.vue` —
		// lastPathComponent `W.vue`, which names wdir/W.vue/. An arm that
		// fell through the SFC case into the extension loops would answer on
		// `sfc/W.vue.ts` and suppress that cascade. The `.ts` neighbour is
		// still PLACED, the way every closureModuleProbes hit is.
		name:    "a single-file-component stem is probed verbatim and never extended",
		spec:    "./W.vue",
		callers: []string{"sfc/app.ts"},
		want: []string{
			"repo/sfc/W.vue.ts", "repo/wdir/W.vue/index.ts", "repo/wdir/W.vue/side.ts",
		},
	}, {
		// The QUALIFIED-NAME arm, which the cascade cannot describe at all.
		// resolveImport binds `Service/logger` to the Kubernetes resource node
		// in k8s/svc.yaml (QualName `<kind>/<name>`,
		// parser/languages/kubernetes.go:89) at resolver.go:3961-3972 and
		// RETURNS — measured in the differential's own oracle, which reports
		// `resolver binds=[repo/k8s/svc.yaml]` for this importer.
		//
		// The two `logger` decoys are what make the case non-vacuous: a
		// cascade-only closure places exactly them and nothing else, so the
		// k8s file is a hard DROP rather than a narrowing. They stay placed
		// because the arm may also FALL THROUGH — pickResolverQualNameCandidate
		// returning nil on an ambiguous candidate set carries on to the cascade
		// (resolver.go:3966-3972) — so the union is the only safe answer.
		name:    "a qualified-name candidate is placed beside the cascade",
		spec:    "Service/logger",
		callers: []string{"qual/app.ts"},
		want: []string{
			"repo/a/logger/index.ts", "repo/b/logger/index.ts", "repo/k8s/svc.yaml",
		},
	}, {
		// The npm-manifest gate is superset-only. declaresExternalNpmDep
		// (resolver.go:4000, applied at :4033) makes the resolver skip the
		// cascade for `lodashish` because npmapp/package.json declares it a
		// registry dependency; the closure reads no manifest and keeps the
		// in-repo directory. Over-admission, never a drop.
		name:    "a manifest-declared npm dependency is still placed",
		spec:    "lodashish",
		callers: []string{"npmapp/app.ts"},
		want:    []string{"repo/vendored/lodashish/index.ts"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			assertImportPlacement(t, walk, tc.spec, tc.callers, tc.want)
		})
	}
}

// TestImportPlacementRefusesUnbindableSameNamedDirectory states the two
// narrowings the resolver's own rules authorise, so a regression names the
// rule rather than a fixture file.
func TestImportPlacementRefusesUnbindableSameNamedDirectory(t *testing.T) {
	_, walk := importParityCase(t, importParityFixture)

	for _, tc := range []struct {
		name    string
		spec    string
		callers []string
		want    []string
	}{{
		// A bare JS/TS specifier binds a directory entry point or nothing:
		// web/feature/graphql/ holds none, so `consider` rejects every
		// candidate in it and the import stays external.
		name:    "bare js specifier with no directory entry point",
		spec:    "graphql",
		callers: []string{"web/app.ts"},
		want:    nil,
	}, {
		// The entry-point rule is the JS/TS module loader's, so it applies
		// only to a JS/TS importer (isJSTSBareSpecifier, jsts_imports.go:253).
		// The same specifier from a C++ translation unit keeps the wider set.
		name:    "the entry-point rule does not apply to a non-js importer",
		spec:    "graphql",
		callers: []string{"native/main.cpp"},
		want:    []string{"repo/web/feature/graphql/schema.ts"},
	}, {
		// …and an entry point carries its whole directory with it: the
		// resolver binds the edge to index.ts, but what the specifier makes
		// reachable is the module behind it (importedDirForSpec,
		// resolver.go:5528-5546).
		name:    "bare js specifier with a directory entry point",
		spec:    "barrel",
		callers: []string{"mono/app.ts"},
		want:    []string{"repo/packages/app/barrel/index.ts", "repo/packages/app/barrel/write.ts"},
	}, {
		// A quoted include of a sibling header names no directory at all, so
		// neither cascade key offers a candidate — and the relative-import
		// pass, which is the arm that WOULD bind it, never reaches the edge:
		// resolveImport has already stamped `external::helper.h` on it
		// (resolver.go:4123) by the time resolveRelativeImports tests for the
		// `unresolved::import::` prefix its C-family arm requires
		// (relative_imports.go:204-241). Measured — the indexed fixture carries
		// `imports repo/native/main.cpp -> external::helper.h`, pinned by
		// TestClosureRelativePassShapesStayExternalInAWholeIndex — so placing
		// the header would be cost with nothing behind it.
		name:    "quoted include that names no directory places nothing",
		spec:    "helper.h",
		callers: []string{"native/main.cpp"},
		want:    nil,
	}, {
		// A relative specifier whose join finds a module file is placed on
		// that file, not by last component.
		name:    "relative specifier joins the importer's directory",
		spec:    "./auth",
		callers: []string{"web/app.ts"},
		want:    []string{"repo/web/auth.ts"},
	}, {
		// A relative specifier whose join ANSWERS never reaches the cascade
		// (resolveImport returns at :3679), so a same-named decoy directory
		// elsewhere in the corpus is not placed. This is the narrowing the
		// relative arm buys, and it has to survive the miss fall-through
		// that TestImportPlacementKeepsEverySameNamedCandidateDirectory
		// pins — unioning the two arms unconditionally would break it.
		name:    "a relative join that answers suppresses the cascade",
		spec:    "./local",
		callers: []string{"local/app.ts"},
		want:    []string{"repo/local/local/impl.ts", "repo/local/local/index.ts"},
	}, {
		// A relative miss with no same-named directory anywhere reaches the
		// cascade and still finds nothing: the import stays external and
		// there is no base file to carry.
		name:    "relative specifier that misses everything places nothing",
		spec:    "./nosuchmodule",
		callers: []string{"mono/app.ts"},
		want:    nil,
	}, {
		// The per-binding payload is what the cascade keys on verbatim:
		// lastPathComponent("./auth::auth") is "auth::auth"
		// (resolver.go:5960-5966), which no directory carries, so the
		// per-binding edge of a relative MISS binds nothing. The module-half
		// specifier addImportSpecifier records alongside it is what carries
		// the placement.
		name:    "a per-binding relative miss keys on the raw payload",
		spec:    "./auth::auth",
		callers: []string{"mono/app.ts"},
		want:    nil,
	}, {
		// A per-binding payload carries the export after the specifier; the
		// module half still governs the placement.
		name:    "relative specifier with a per-binding export",
		spec:    "./auth::auth",
		callers: []string{"web/app.ts"},
		want:    []string{"repo/web/auth.ts"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			assertImportPlacement(t, walk, tc.spec, tc.callers, tc.want)
		})
	}
}

// TestImportPlacementRefusesToAnswerOnADeletedJoinTarget states the corpus the
// relative arm's VERDICT is read from, at the level of the arm itself.
//
// w.fileIndex is the BASE corpus (buildDirIndexes reads req.Base), but
// resolveImport runs against the POST-change tree. `./local` out of
// local/app.ts answers on local/local/index.ts while that file is there —
// TestImportPlacementRefusesUnbindableSameNamedDirectory pins exactly that,
// and the decoy other/local/ stays out. Delete the barrel in the same change
// and the resolver's probe misses, so the cascade runs and the decoy is back
// in: continuing to answer would be a DROP.
//
// Only the VERDICT is corrected. Placement still offers everything the join
// reaches, deleted or not — a file the change removed is a seed the caller
// already carries (affectedClosureContext seeds `deleted`), so over-placing it
// costs nothing.
func TestImportPlacementRefusesToAnswerOnADeletedJoinTarget(t *testing.T) {
	_, walk := importParityCase(t, importParityFixture)
	walk.buildDirIndexes()
	if _, ok := walk.fileIndex["repo/local/local/index.ts"]; !ok {
		t.Fatalf("the base corpus does not carry the join target; the case is vacuous")
	}
	// The same change deletes the directory the join answers on.
	walk.deleted = map[string]struct{}{
		"local/local/index.ts": {},
		"local/local/impl.ts":  {},
	}

	assertImportPlacement(t, walk, "./local", []string{"local/app.ts"}, []string{
		// The join's own placement, unchanged.
		"repo/local/local/impl.ts", "repo/local/local/index.ts",
		// …plus the cascade the miss now reaches: every directory whose last
		// component is `local` (resolver.go:4043-4049).
		"repo/local/app.ts", "repo/other/local/index.ts",
	})
}

// TestClosureAttributesImportsToTheirImporter drives the production entrypoint.
//
// The placement rules above are a function of WHO named the specifier, so the
// walk records each import against the file being extracted
// (closureRefs.addImport, fed by extractInto). Nothing outside that wiring can
// supply it: with the attribution gone, every relative specifier reaches
// relativeImportedFiles with no importer to join against, so the join can
// answer for nobody and the placement falls back to the cascade — which for
// `./cfg` reaches no directory named `cfg` and therefore carries nothing. A
// hard drop, not a narrowing. This case asks the real BuildCommitLayer path
// for the closure and requires the joined file in it.
//
// The second half asks the same build for the conservative superset: a bare
// specifier with two candidate entry-point directories has to carry both,
// because the resolver's first-hit scan may land on either.
func TestClosureAttributesImportsToTheirImporter(t *testing.T) {
	treeA := map[string]string{
		"web/app.ts":          "export function run() { return 1; }\n",
		"web/cfg.ts":          "export const closureOnlyCfg = 1;\n",
		"x/ambig/index.ts":    "export function ambig() { return 1; }\n",
		"y/ambig/index.ts":    "export function ambig() { return 2; }\n",
		"unrelated/lonely.ts": "export function lonely() { return 9; }\n",
	}
	treeB := map[string]string{}
	for p, body := range treeA {
		treeB[p] = body
	}
	// Side-effect imports only: neither specifier's module exports a name
	// app.ts mentions, so the name arm of collectIntroduced cannot be what
	// places these files. Only the import arm can.
	treeB["web/app.ts"] = `import './cfg';
import 'ambig';

export function run() { return 1; }
`

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report, "web/cfg.ts", "x/ambig/index.ts", "y/ambig/index.ts")
	if slices.Contains(c.report.ClosurePaths, "unrelated/lonely.ts") {
		t.Errorf("the closure carries %q, which no import of the change names; it carries %v",
			"unrelated/lonely.ts", c.report.ClosurePaths)
	}
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestClosureCarriesTheCascadeBehindARelativeMiss drives the fall-through
// through the production build path rather than through importedFiles alone.
//
// treeB introduces `import { auth } from './auth'` into deep/app.ts, where no
// `deep/auth.*` exists. The resolver's relative arm returns "" for that edge
// and resolveImport carries on with the raw specifier, binding a misc/auth/
// candidate out of lastDirIndex (resolver.go:3906, :3815-3822). If the closure
// treats the join as terminal, BuildCommitLayer carries neither file and the
// composed reader diverges from a whole index on the very import the change
// introduced — so builderAssertReadersAgree is the other half of the claim.
//
// The negative half is the narrowing this must not undo: local/app.ts's
// `./local` join ANSWERS onto local/local/, so the same-named decoy
// other/local/ must stay out of the closure.
func TestClosureCarriesTheCascadeBehindARelativeMiss(t *testing.T) {
	treeA := map[string]string{
		"deep/app.ts":          "export function boot() { return 0; }\n",
		"local/app.ts":         "export function start() { return 0; }\n",
		"misc/auth/index.ts":   "export { closureOnlyAuth } from './side';\n",
		"misc/auth/side.ts":    "export function closureOnlyAuth() { return 7; }\n",
		"local/local/index.ts": "export { closureOnlyHere } from './impl';\n",
		"local/local/impl.ts":  "export function closureOnlyHere() { return 8; }\n",
		"other/local/index.ts": "export function closureOnlyDecoy() { return 9; }\n",
	}
	treeB := map[string]string{}
	for p, body := range treeA {
		treeB[p] = body
	}
	// Side-effect imports only: neither specifier's module exports a name
	// the change mentions, so the NAME arm of collectIntroduced cannot be
	// what places these files. Only the import arm can — which is what makes
	// the assertions below revert-red on the placement rule itself.
	treeB["deep/app.ts"] = `import './auth';

export function boot() { return 0; }
`
	treeB["local/app.ts"] = `import './local';

export function start() { return 0; }
`

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report,
		"misc/auth/index.ts", "misc/auth/side.ts",
		"local/local/index.ts", "local/local/impl.ts")
	if slices.Contains(c.report.ClosurePaths, "other/local/index.ts") {
		t.Errorf("the closure carries %q, but `./local` out of local/app.ts joins onto "+
			"local/local/ and the resolver never reaches the cascade for it "+
			"(resolver.go:3906); it carries %v",
			"other/local/index.ts", c.report.ClosurePaths)
	}
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestClosureCarriesTheCascadeBehindANonJSSiblingJoin drives the second half
// of the fall-through through the production build path.
//
// The join here REACHES a file — `mixed/svc.py` — but not one probeJSTSFile
// can see (jsts_imports.go:156-200 probes jsTSImportExts only), so
// resolveJSTSImportTarget returns "" and resolveImport carries on to the
// cascade, which binds a svcdir/svc/ candidate out of lastDirIndex
// (resolver.go:3906, :3815-3822). A closure whose relative arm answers on the
// wider closureModuleProbes list suppresses that cascade, BuildCommitLayer
// carries neither svcdir/svc/ file, and the composed reader leaves the import
// `external::` where a whole index binds it — which is what
// builderAssertReadersAgree is here to catch.
//
// The `.py` sibling itself must still be carried: resolveRelativeImports binds
// that shape in its own later pass (relative_imports.go:24-33), so placing it
// is required and only ANSWERING on it is wrong.
func TestClosureCarriesTheCascadeBehindANonJSSiblingJoin(t *testing.T) {
	treeA := map[string]string{
		"mixed/app.ts":        "export function boot() { return 0; }\n",
		"mixed/svc.py":        "def svc():\n    return 1\n",
		"svcdir/svc/index.ts": "export { closureOnlySvc } from './side';\n",
		"svcdir/svc/side.ts":  "export function closureOnlySvc() { return 7; }\n",
	}
	treeB := map[string]string{}
	for p, body := range treeA {
		treeB[p] = body
	}
	// Side-effect import only: the module exports no name the change
	// mentions, so the NAME arm of collectIntroduced cannot be what places
	// these files. Only the import arm can.
	treeB["mixed/app.ts"] = `import './svc';

export function boot() { return 0; }
`

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report,
		"svcdir/svc/index.ts", "svcdir/svc/side.ts", "mixed/svc.py")
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestClosureCarriesTheCascadeBehindADeletedJoinTarget pins the one corpus the
// relative arm's VERDICT is allowed to be computed against.
//
// The verdict answers "did resolveImport's relative arm terminate", and
// resolveImport runs against the POST-change tree. relativeArmProbeHits reads
// w.fileIndex, which buildDirIndexes fills from req.Base — the BASE corpus —
// so the same change can delete the file the join would have hit and leave it
// in the index the verdict is read from. Answering there suppresses a cascade
// the resolver demonstrably runs: mixed/svc.ts is gone by the time the import
// resolves, `./svc` misses probeJSTSFile, and lastDirIndex offers svcdir/svc/.
//
// The failure is a DROP at the composed reader, not merely a thinner closure —
// BuildCommitLayer carries neither svcdir/svc/ file, so the generation leaves
// the import on `external::./svc` where a whole index binds
// svcdir/svc/index.ts. builderAssertReadersAgree is what states that.
//
// The mirror-image case needs no correction and is therefore not asserted: a
// file the change ADDS is absent from the base fileIndex, so the verdict is
// already "not answered" and the caller unions the cascade — over-admission,
// which is the direction the closure is allowed to err in.
func TestClosureCarriesTheCascadeBehindADeletedJoinTarget(t *testing.T) {
	treeA := map[string]string{
		"mixed/app.ts":        "export function boot() { return 0; }\n",
		"mixed/svc.ts":        "export function closureOnlyGone() { return 7; }\n",
		"svcdir/svc/index.ts": "export { closureOnlyCascade } from './side';\n",
		"svcdir/svc/side.ts":  "export function closureOnlyCascade() { return 9; }\n",
	}
	treeB := map[string]string{}
	for p, body := range treeA {
		treeB[p] = body
	}
	// Side-effect import only, so the NAME arm of collectIntroduced cannot be
	// what places these files — only the import arm can.
	treeB["mixed/app.ts"] = `import './svc';

export function boot() { return 0; }
`
	// …and the join target the base corpus still carries is deleted by the
	// very same change.
	delete(treeB, "mixed/svc.ts")

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report, "svcdir/svc/index.ts", "svcdir/svc/side.ts")
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestImportPlacementCensusIsBounded is the cost half of the differential,
// stated as a number rather than as a rule.
//
// NO STRAY above bounds each placement by a rule — a directory the resolver's
// import index offers, or a file its qualified-name arm offers — and a rule can
// only ever authorise what it describes. This states the OTHER thing a reviewer
// needs: exactly what the walk places for the whole fixture, file by file. Any
// arm added, widened, or gated differently moves a row here and has to be
// justified against the resolver, whatever rule it claims to follow.
//
// It is a golden table on purpose. The fixture is fixed, the extraction is the
// production one, and the placements are deterministic, so a diff in this table
// is a real change in what a generation carries — 49 files over 36 specifiers
// as it stands.
func TestImportPlacementCensusIsBounded(t *testing.T) {
	_, walk := importParityCase(t, importParityFixture)
	refs := importParityVocabulary(t, walk, importParityFixture)

	want := map[string][]string{
		"./W.vue":       {"repo/sfc/W.vue.ts", "repo/wdir/W.vue/index.ts", "repo/wdir/W.vue/side.ts"},
		"./auth":        {"repo/misc/auth/index.ts", "repo/misc/auth/side.ts", "repo/web/auth.ts"},
		"./auth::auth":  {"repo/web/auth.ts"},
		"./cfg":         {"repo/web/cfg.ts"},
		"./impl":        {"repo/local/local/impl.ts"},
		"./impl::here":  {"repo/local/local/impl.ts"},
		"./local":       {"repo/local/local/impl.ts", "repo/local/local/index.ts"},
		"./local::here": {"repo/local/local/impl.ts", "repo/local/local/index.ts"},
		"./side": {
			"repo/misc/auth/side.ts", "repo/svcdir/svc/side.ts", "repo/svcdir2/svc2/side.ts",
			"repo/svcdir3/svc3/side.ts", "repo/wdir/W.vue/side.ts",
		},
		"./side::auth":             {"repo/misc/auth/side.ts"},
		"./side::svc":              {"repo/svcdir/svc/side.ts"},
		"./side::svc2":             {"repo/svcdir2/svc2/side.ts"},
		"./side::svc3":             {"repo/svcdir3/svc3/side.ts"},
		"./side::widget":           {"repo/wdir/W.vue/side.ts"},
		"./svc":                    {"repo/mixed/svc.py", "repo/svcdir/svc/index.ts", "repo/svcdir/svc/side.ts"},
		"./svc2":                   {"repo/rb/svc2.rb", "repo/svcdir2/svc2/index.ts", "repo/svcdir2/svc2/side.ts"},
		"./svc3":                   {"repo/rbjs/svc3.ts", "repo/svcdir3/svc3/index.ts", "repo/svcdir3/svc3/side.ts"},
		"./write":                  {"repo/packages/app/barrel/write.ts"},
		"./write::emit":            {"repo/packages/app/barrel/write.ts"},
		"/lib.php":                 nil,
		"Service/logger":           {"repo/a/logger/index.ts", "repo/b/logger/index.ts", "repo/k8s/svc.yaml"},
		"Service/logger::ping":     nil,
		"barrel":                   {"repo/packages/app/barrel/index.ts", "repo/packages/app/barrel/write.ts"},
		"barrel::emit":             nil,
		"example.com/fixture/util": {"repo/internal/util/helper.go", "repo/util/helper.go"},
		"graphql":                  nil,
		"graphql::parse":           nil,
		"helper.h":                 nil,
		"leaf":                     nil,
		"lodashish":                {"repo/vendored/lodashish/index.ts"},
		"lodashish::chunk":         nil,
		"pyutil":                   {"repo/deep/pyutil/__init__.py", "repo/pyutil/__init__.py"},
		"shared/b/logger":          {"repo/a/logger/index.ts", "repo/b/logger/index.ts"},
		"shared/b/logger::log":     nil,
		"sibling.h":                nil,
		"vecs":                     {"repo/extra/vecs/other.h", "repo/native/vecs/impl.h"},
	}

	specs := make([]string, 0, len(refs.imports))
	for spec := range refs.imports {
		specs = append(specs, spec)
	}
	sort.Strings(specs)
	if len(specs) != len(want) {
		t.Errorf("the fixture now yields %d specifiers, the census pins %d: %v",
			len(specs), len(want), specs)
	}
	placed := 0
	for _, spec := range specs {
		got := walk.importedFiles(spec, refs.importCallers(spec))
		placed += len(got)
		expected, pinned := want[spec]
		if !pinned {
			t.Errorf("specifier %q is not in the census; it places %v", spec, got)
			continue
		}
		if !slices.Equal(got, expected) {
			t.Errorf("census drift for %q: places %v, pinned %v", spec, got, expected)
		}
	}
	if placed != 49 {
		t.Errorf("the fixture's whole import placement is %d files, pinned at 49", placed)
	}
}

// --- fixture plumbing ---------------------------------------------------

// importParityCase commits the tree, indexes it whole, and returns the store
// alongside a closure walk reading that store as its base layer and the
// committed working tree as its target content.
func importParityCase(t *testing.T, tree map[string]string) (*store_sqlite.Store, *closureWalk) {
	t.Helper()
	builderIsolateGit(t)
	dir := builderTempDir(t, "repo")
	builderGit(t, dir, "init", "--initial-branch=main")
	builderWriteTree(t, dir, tree)
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "A")

	store := builderOpenStore(t, "oracle")
	builderIndex(t, store, dir)

	target, err := source.NewFilesystemSource(dir)
	if err != nil {
		t.Fatalf("NewFilesystemSource: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	walk := &closureWalk{
		b:   builderNewBuilder(store),
		req: BuildRequest{Base: store, Target: target, RootPath: dir, RepoPrefix: builderRepoPrefix},
	}
	return store, walk
}

// importParityVocabulary runs every fixture file through the production
// extraction, so the specifiers and their importer attribution are exactly
// what collectIntroduced hands importedFiles.
func importParityVocabulary(t *testing.T, walk *closureWalk, tree map[string]string) *closureRefs {
	t.Helper()
	present := make(map[string]struct{}, len(tree))
	rels := make([]string, 0, len(tree))
	for rel := range tree {
		present[rel] = struct{}{}
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	semantic := newBuilderSemanticTarget(present)
	refs := &closureRefs{
		names:    map[string]struct{}{},
		imports:  map[string]map[string]struct{}{},
		defines:  map[string]struct{}{},
		semantic: &semantic,
	}
	for _, rel := range rels {
		walk.extractInto(rel, refs)
	}
	if len(refs.imports) == 0 {
		t.Fatalf("the fixture produced no import specifiers at all — the differential would be vacuous")
	}
	return refs
}

// importParityImporters returns the sorted files that named at least one
// specifier.
func importParityImporters(refs *closureRefs) []string {
	set := map[string]struct{}{}
	for _, callers := range refs.imports {
		for caller := range callers {
			set[caller] = struct{}{}
		}
	}
	return sortedKeys(set)
}

type importParityPlacement struct {
	spec   string
	placed []string
}

// importParityPlacements places every specifier the importer named, attributed
// to the importer set collectIntroduced would pass.
func importParityPlacements(walk *closureWalk, refs *closureRefs, importer string) []importParityPlacement {
	var specs []string
	for spec, callers := range refs.imports {
		if _, named := callers[importer]; named {
			specs = append(specs, spec)
		}
	}
	sort.Strings(specs)
	out := make([]importParityPlacement, 0, len(specs))
	for _, spec := range specs {
		out = append(out, importParityPlacement{
			spec:   spec,
			placed: walk.importedFiles(spec, refs.importCallers(spec)),
		})
	}
	return out
}

// importParityOracle reads what the resolver actually bound the importer's
// import edges to: the in-repo files, and the directories those files sit in.
func importParityOracle(t *testing.T, store *store_sqlite.Store, graphPath string) ([]string, map[string]struct{}) {
	t.Helper()
	files := map[string]struct{}{}
	for _, e := range store.GetOutEdges(graphPath) {
		if e == nil || e.Kind != graph.EdgeImports {
			continue
		}
		target := store.GetNode(e.To)
		if target == nil || target.FilePath == "" {
			// external:: / dep:: / unresolved — the resolver reached no
			// in-repo file for this import.
			continue
		}
		if _, owned := builderRelPath(builderRepoPrefix, target.FilePath); !owned {
			continue
		}
		files[target.FilePath] = struct{}{}
	}
	dirs := map[string]struct{}{}
	for f := range files {
		dirs[path.Dir(f)] = struct{}{}
	}
	return sortedKeys(files), dirs
}

// importParityCandidateDir reports whether dir is a directory the RESOLVER's
// own import index offers for this specifier out of this importer.
//
// It is spelled from the resolver's side, not the closure's:
// resolver.buildDirIndexes keys every file on its directory and on that
// directory's last component (resolver.go:2098-2109), resolveImport reads
// exactly those two keys (:3809, :3816), and the relative arm ahead of the
// cascade joins the specifier onto the importer's own directory
// (jsts_imports.go:132-148, relative_imports.go:95-110).
//
// A placement is SPECIFIER-scoped, not importer-scoped: importedFiles is
// handed the whole importer set (builder_closure.go:374 passes
// refs.importCallers) and returns one set for all of them, so a directory any
// one importer's join reaches is a directory the resolver offers for that
// specifier. `callers` is therefore that whole set.
//
// The relative arm is a first try, not a terminal one: resolveImport takes its
// answer only `if to != ""` (resolver.go:3906). When the join probe misses for
// ANY importer of the specifier, that importer's edge reaches the cascade with
// the raw `./…` payload, so the cascade's own two keys are offered for the
// specifier as well.
func importParityCandidateDir(walk *closureWalk, spec string, callers []string, dir string) bool {
	module := spec
	if i := strings.LastIndex(module, "::"); i >= 0 {
		module = module[:i]
	}
	cascade := dir == module || dir == spec || path.Base(dir) == path.Base(module)
	if !strings.HasPrefix(module, "./") && !strings.HasPrefix(module, "../") {
		return cascade
	}
	for _, caller := range callers {
		join := path.Clean(path.Join(path.Dir(builderGraphPath(builderRepoPrefix, caller)), module))
		if dir == join || dir == path.Dir(join) {
			return true
		}
	}
	if importParityRelativeArmAnswers(walk, module, callers) {
		return false
	}
	return cascade
}

// importParityRelativeArmAnswers spells resolveImport's relative-arm
// TERMINATION condition from the resolver's side, over an importer set.
//
// Both halves of the resolver's own precondition, and nothing else:
//
//   - resolveJSTSImportTarget opens with `if !isJSTSPath(callerFile) { return
//     "" }` (jsts_imports.go:99-101), so for a NON-JS/TS importer the arm can
//     never terminate resolveImport and the cascade is always reached. The
//     relative joins the other languages get (relative_imports.go — C
//     includes, PHP includes, Python/Dart relative imports) belong to
//     `resolveRelativeImports`, a separate pass that runs AFTER the resolve
//     loop, so they do not suppress this cascade.
//   - probeJSTSFile (jsts_imports.go:156-200) probes ONLY jsTSImportExts plus
//     the emitted-JS source counterparts and `index.<ext>` barrels — never
//     `.py`/`.php`/`.rb`/`.dart`/`.h…`/`__init__.py`. Its single-file-component
//     case (`.vue`/`.svelte`/`.astro`) is a verbatim probe with an early
//     return and no extension probing on top.
//
// Spelled here against the base corpus rather than shared with the production
// helpers so a regression in either one shows up as a disagreement. An earlier
// round of this harness replicated the production defect it was meant to
// catch — both the missing language precondition and the wider extension list
// — which is why this mirror is written from probeJSTSFile's own control flow
// rather than from an extension set.
func importParityRelativeArmAnswers(walk *closureWalk, module string, callers []string) bool {
	if len(callers) == 0 {
		return false
	}
	walk.buildDirIndexes()
	// jsTSImportExts (jsts_imports.go:36-38) verbatim.
	exts := []string{".ts", ".tsx", ".d.ts", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs"}
	// jsTSEmittedSourceExts (jsts_imports.go:59-68), read through the
	// resolver's own exported accessor.
	isFile := func(id string) bool { _, ok := walk.fileIndex[id]; return ok }
	probe := func(stem string) bool {
		if stem == "" {
			return false
		}
		switch ext := strings.ToLower(path.Ext(stem)); ext {
		case ".vue", ".svelte", ".astro":
			return isFile(stem)
		case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
			if isFile(stem) {
				return true
			}
			for _, srcExt := range resolver.EmittedJSSourceExts(ext) {
				if isFile(strings.TrimSuffix(stem, ext) + srcExt) {
					return true
				}
			}
		}
		for _, ext := range exts {
			if isFile(stem + ext) {
				return true
			}
		}
		for _, ext := range exts {
			if isFile(stem + "/index" + ext) {
				return true
			}
		}
		return false
	}
	for _, caller := range callers {
		// isJSTSPath (jsts_imports.go:236-243), spelled out.
		switch strings.ToLower(path.Ext(caller)) {
		case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs",
			".vue", ".svelte", ".astro":
		default:
			return false
		}
		if !probe(path.Clean(path.Join(path.Dir(builderGraphPath(builderRepoPrefix, caller)), module))) {
			return false
		}
	}
	return true
}

// importParityQualNameOffers is every in-repo file the resolver's one
// NON-cascade import arm offers for this specifier.
//
// There is exactly one, and it is not spelled from the closure's side: the
// QUALIFIED-NAME arm binds a node whose QualName IS the specifier
// (resolver.go:3961-3972), ahead of the cascade and with no directory involved,
// so the oracle is the resolver's OWN primitive — the store's qualified-name
// index, which is what cachedFindNodesByQualName reads
// (resolver.go:2444-2454) — and not a restatement of any control flow in
// builder_closure.go. Measured in this fixture: a TypeScript
// `import … from 'Service/logger'` binds the Kubernetes resource node in
// k8s/svc.yaml, a file no cascade key names.
//
// resolveRelativeImports' arms are deliberately absent. Its C-family and PHP
// arms test for an `unresolved::import::` prefix resolveImport has already
// overwritten with `external::` (resolver.go:4123) by the time the pass runs
// (resolver.go:1808), and its python arm probes an unprefixed stem against
// repository-prefixed node IDs — so in this harness they bind nothing, which
// TestClosureRelativePassShapesStayExternalInAWholeIndex pins directly and the
// NO DROP half below re-checks for every importer. An oracle that described
// those arms anyway would be authorising placements no measurement backs.
func importParityQualNameOffers(walk *closureWalk, spec string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, node := range importParityQualNameNodes(walk, spec) {
		if node == nil || node.FilePath == "" {
			continue
		}
		if _, owned := builderRelPath(builderRepoPrefix, node.FilePath); owned {
			out[node.FilePath] = struct{}{}
		}
	}
	return out
}

// importParityQualNameNodes is the resolver's own multi-valued qualified-name
// lookup (cachedFindNodesByQualName, resolver.go:2444-2454), read off the base
// store the fixture indexed.
func importParityQualNameNodes(walk *closureWalk, qualName string) []*graph.Node {
	if qualName == "" {
		return nil
	}
	type batchLookup interface {
		GetNodesByQualNames(qualNames []string) map[string][]*graph.Node
	}
	if batch, ok := walk.req.Base.(batchLookup); ok {
		return batch.GetNodesByQualNames([]string{qualName})[qualName]
	}
	if node := walk.req.Base.GetNodeByQualName(qualName); node != nil {
		return []*graph.Node{node}
	}
	return nil
}

// importParityBareJSTS mirrors the resolver's own precondition for the
// entry-point gate (isJSTSBareSpecifier, jsts_imports.go:253-264): a
// non-relative, non-absolute specifier whose importer is a JS/TS file. Spelled
// here rather than shared with the production helper so a regression in either
// one shows up as a disagreement.
func importParityBareJSTS(spec string, callers []string) bool {
	if len(callers) == 0 {
		return false
	}
	module := spec
	if i := strings.LastIndex(module, "::"); i >= 0 {
		module = module[:i]
	}
	switch {
	case module == "", module == ".", module == "..":
		return false
	case strings.HasPrefix(module, "./"), strings.HasPrefix(module, "../"), strings.HasPrefix(module, "/"):
		return false
	}
	for _, caller := range callers {
		switch strings.ToLower(path.Ext(caller)) {
		case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs", ".vue", ".svelte", ".astro":
		default:
			return false
		}
	}
	return true
}

// importParityDirHasEntryPoint reports whether the base corpus holds an
// `index.<ext>` in dir — the evidence isJSTSDirEntryPoint
// (jsts_imports.go:294-308) requires of a directory-shaped JS/TS module.
func importParityDirHasEntryPoint(walk *closureWalk, dir string) bool {
	walk.buildDirIndexes()
	for _, file := range walk.dirIndex[dir] {
		base := strings.ToLower(path.Base(file))
		for _, ext := range []string{".ts", ".tsx", ".d.ts", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs"} {
			if base == "index"+ext {
				return true
			}
		}
	}
	return false
}

func assertImportPlacement(t *testing.T, walk *closureWalk, spec string, callers, want []string) {
	t.Helper()
	got := append([]string(nil), walk.importedFiles(spec, callers)...)
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(got, "\n") != strings.Join(sorted, "\n") {
		t.Errorf("importedFiles(%q, %v) = %v, want %v", spec, callers, got, sorted)
	}
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
