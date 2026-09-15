package indexer

import (
	"slices"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Conservative handling for missing metadata and dynamic-language cases.
//
// The closure's job is one-directional: it must be a SUPERSET of what the
// resolver binds. Where it cannot prove parity with a resolver arm — because
// the metadata that arm reads is not reachable through LayerBase, or because
// the arm belongs to a language family the closure's JS/TS-shaped rules do not
// describe — it has to admit the candidate rather than narrow.
//
// The cases below state that rule for the arms the import-placement parity
// harness declared as unproven residuals (builder_closure.go's own doc): the
// qualified-name arm, which IS mirrored and has to stay enumerable on every
// base the coordinator builds, and the relative-import pass, which is NOT
// mirrored because it binds nothing on the whole-index path — a claim this file
// pins against the real resolver rather than asserting in a comment.

// qualNameBlindBase is a LayerBase that offers only the graph.Reader surface:
// the single-valued GetNodeByQualName (reader.go:24) and no batched
// GetNodesByQualNames. It is the "missing metadata" shape — a base layer that
// cannot enumerate the qualified-name candidate set resolveImport's own arm
// reads (cachedFindNodesByQualName, resolver.go:2444).
//
// The embedded INTERFACE, rather than a struct pointer, is what makes the
// method set exact: a concrete *store_sqlite.Store would carry
// GetNodesByQualNames along with everything else and closureBatchedQualNames
// would find it.
type qualNameBlindBase struct {
	graph.Reader
	inner LayerBase
}

func (b qualNameBlindBase) GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node {
	return b.inner.GetFileNodesByPaths(filePaths)
}

// countingQualNameBase counts the batched qualified-name lookups a build
// issues, and how many names each one carried.
type countingQualNameBase struct {
	LayerBase
	calls *int
	names *int
}

func (b countingQualNameBase) GetNodesByQualNames(qualNames []string) map[string][]*graph.Node {
	*b.calls++
	*b.names += len(qualNames)
	return b.LayerBase.(closureQualNameLookup).GetNodesByQualNames(qualNames)
}

// TestClosureQualNameArmIsEnumerableThroughEveryProductionBaseShape is the
// conservative-handling rule for missing metadata, stated against the base
// shapes production actually constructs rather than against a synthetic one.
//
// There are two. A build whose base generation is zero reads the store directly
// (checkout_coordinator.go:1773). EVERY OTHER build — every incremental commit
// layer (:1788, taken whenever base.generationID > 0) and every dirty layer
// (:2128) — reads the commitLayerBase ancestryLayerBase mints (:2143-2145),
// which embeds the graph.Reader INTERFACE (:3240-3251) and therefore does NOT carry
// GetNodesByQualNames in its own method set even though the reader inside it
// always does. If the mirror asked only `base.(closureQualNameLookup)` it would
// degrade to the single-valued lookup on the incremental path — which is the
// hot path — and see one candidate where the resolver's arm reads every one.
func TestClosureQualNameArmIsEnumerableThroughEveryProductionBaseShape(t *testing.T) {
	store, walk := importParityCase(t, importParityFixture)

	// The composed reader graphview hands the coordinator is an *OverlaidView
	// (materialize.go:770-802), which merges every overlay and surviving base
	// qualified-name candidate (overlay.go:457-505).
	overlaid := graph.NewOverlaidView(store, graph.NewOverlayLayer())

	for _, tc := range []struct {
		name string
		base LayerBase
	}{
		{"store base (checkout_coordinator.go:1773)", store},
		{"commit layer base over the store (ancestryLayerBase, :2143)", commitLayerBase{Reader: store}},
		{"dirty layer base over a materialized view (:2128)", commitLayerBase{Reader: overlaid}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if closureBatchedQualNames(tc.base) == nil {
				t.Fatalf("%T cannot enumerate qualified-name candidates: the mirror degrades to the "+
					"single-valued graph.Reader lookup on this base, so the resolver's arm "+
					"(resolver.go:3961-3972) is read one candidate at a time", tc.base)
			}
			shaped := &closureWalk{b: walk.b, req: walk.req}
			shaped.req.Base = tc.base
			if !shaped.qualNamesEnumerable() {
				t.Fatalf("qualNamesEnumerable() = false on %T", tc.base)
			}
			// The union the arm and the cascade produce together: the
			// qualified-name candidate plus the two `logger` decoys
			// lastDirIndex offers. What matters here is that it does not
			// depend on the base's composition — a base whose candidate set
			// degrades to the single-valued lookup would still find this one
			// candidate, so the assertion is spelled on the whole placement.
			got := shaped.importedFiles("Service/logger", []string{"qual/app.ts"})
			want := []string{
				"repo/a/logger/index.ts", "repo/b/logger/index.ts", "repo/k8s/svc.yaml",
			}
			if !slices.Equal(got, want) {
				t.Errorf("importedFiles(%q) on %T = %v, want %v — the qualified-name arm's "+
					"placement must not depend on the base's composition",
					"Service/logger", tc.base, got, want)
			}
			if qual := shaped.qualNameImportedFiles("Service/logger"); !slices.Equal(qual, []string{"repo/k8s/svc.yaml"}) {
				t.Errorf("the qualified-name arm alone placed %v on %T, want [repo/k8s/svc.yaml]",
					qual, tc.base)
			}
		})
	}

	// The shape that makes the unwrap load-bearing: commitLayerBase does not
	// satisfy the batched lookup on its own. If this ever stops being true the
	// unwrap is dead code and should go, not stay as decoration.
	var commit LayerBase = commitLayerBase{Reader: store}
	if _, direct := commit.(closureQualNameLookup); direct {
		t.Errorf("commitLayerBase now satisfies closureQualNameLookup directly; " +
			"closureBatchedQualNames' unwrap branch is dead and should be removed")
	}
}

// TestClosureKeepsTheEntryPointGateOnEveryBaseShape is the other half of the
// same rule, and the cost half.
//
// The entry-point gate (closureJSTSEntryPoints, mirroring `consider`'s
// resolver.go:4008-4010) is the issue-#450 cost control: without it an
// `import … from 'graphql'` drags every file under any `*/graphql/` directory
// into the generation. It filters the candidates the CASCADE reaches, and the
// qualified-name arm's candidates are placed beside them rather than through
// it — so no property of that arm can justify switching it off. In particular a
// base that cannot enumerate qualified names must NOT relax it: widening the
// cascade would not place the candidate the mirror failed to see (it is a node
// in a file neither cascade key names) and would disable the control for every
// bare specifier of every build on that base.
func TestClosureKeepsTheEntryPointGateOnEveryBaseShape(t *testing.T) {
	store, walk := importParityCase(t, importParityFixture)

	// Non-vacuity: the gate has something to refuse. web/feature/graphql/ holds
	// a schema module and no `index.<ext>`.
	walk.buildDirIndexes()
	if got := walk.lastDirIndex["graphql"]; !slices.Contains(got, "repo/web/feature/graphql/schema.ts") {
		t.Fatalf("the cascade does not offer the ungated candidate at all (lastDirIndex[graphql] = %v); "+
			"this case would prove nothing", got)
	}

	blind := qualNameBlindBase{Reader: store, inner: store}
	if closureBatchedQualNames(blind) != nil {
		t.Fatalf("qualNameBlindBase still offers the batched lookup; the fixture is wrong")
	}

	for _, tc := range []struct {
		name string
		base LayerBase
	}{
		{"store base", store},
		{"commit layer base", commitLayerBase{Reader: store}},
		{"a base that cannot enumerate qualified names", blind},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shaped := &closureWalk{b: walk.b, req: walk.req}
			shaped.req.Base = tc.base
			if got := shaped.importedFiles("graphql", []string{"web/app.ts"}); len(got) != 0 {
				t.Errorf("importedFiles(%q) on %T = %v, want nothing: web/feature/graphql/ holds no "+
					"module entry point, so `consider` rejects every candidate in it and the "+
					"closure must not carry them either",
					"graphql", tc.base, got)
			}
		})
	}
}

// TestClosureQualNameArmPlacesOnlyOwnedCandidateFiles states the two filters the
// mirror applies, both of which are the resolver's own.
//
//   - A candidate with no FilePath, or one belonging to another repository,
//     names nothing this generation could carry. The synthetic external nodes
//     the resolver itself mints (external_call::<eco>::<path>) carry exactly
//     that shape and a QualName equal to the import path, so without the
//     ownership filter every external import would place a phantom path.
//   - The candidate's FILE is placed, not its directory: the arm binds a NODE,
//     and there is no barrel/entry-point indirection to cover.
func TestClosureQualNameArmPlacesOnlyOwnedCandidateFiles(t *testing.T) {
	_, walk := importParityCase(t, importParityFixture)
	walk.buildDirIndexes()

	if !walk.qualNamesEnumerable() {
		t.Fatalf("the fixture base does not offer the batched qualified-name lookup")
	}
	if want := []string{"repo/k8s/svc.yaml"}; !slices.Equal(walk.qualNameImportedFiles("Service/logger"), want) {
		t.Errorf("qualNameImportedFiles(%q) = %v, want %v (the candidate's own file, not its directory)",
			"Service/logger", walk.qualNameImportedFiles("Service/logger"), want)
	}
	if got := walk.qualNameImportedFiles("no.such.qualified.name"); len(got) != 0 {
		t.Errorf("qualNameImportedFiles on an absent qualified name = %v, want nothing", got)
	}

	// The ownership filter. The same candidate, read by a walk building a
	// DIFFERENT repository's generation, names a path that generation could
	// never claim: builderRelPath refuses it, and offering it to admit() would
	// be a path outside the build's own tree.
	foreign := &closureWalk{b: walk.b, req: walk.req}
	foreign.req.RepoPrefix = "otherrepo"
	if got := foreign.qualNameImportedFiles("Service/logger"); len(got) != 0 {
		t.Errorf("qualNameImportedFiles placed %v for a build of %q, but the candidate's file "+
			"belongs to another repository", got, foreign.req.RepoPrefix)
	}
}

// TestClosureQualNameLookupIsBatchedOncePerBuild bounds what the mirror costs.
//
// GetNodesByQualNames is one SQL statement per CALL, whatever the payload
// (store_lookups.go:164-177), and closureRefs.addImportSpecifier records both a
// re-export's module half and its `<path>::<export>` half — so a lookup issued
// per specifier is roughly two round trips per distinct import the walk
// reaches, most of them for a qualified name nothing carries. The production
// entry point (collectIntroduced) prepares the whole set in ONE call before any
// placement runs, and issues none at all for the relative specifiers whose
// placement returns on the relative arm ahead of the qualified-name one.
func TestClosureQualNameLookupIsBatchedOncePerBuild(t *testing.T) {
	_, walk := importParityCase(t, importParityFixture)

	calls, names := 0, 0
	counted := &closureWalk{b: walk.b, req: walk.req}
	counted.req.Base = countingQualNameBase{LayerBase: walk.req.Base, calls: &calls, names: &names}
	counted.seeds = map[string]struct{}{}
	counted.chosen = map[string]struct{}{}
	counted.deleted = map[string]struct{}{}

	present := make(map[string]struct{}, len(importParityFixture))
	for rel := range importParityFixture {
		present[rel] = struct{}{}
	}
	out := map[string]struct{}{}
	counted.collectIntroduced(present, out)

	if calls != 1 {
		t.Errorf("the build issued %d batched qualified-name lookups, want exactly 1: "+
			"collectIntroduced prepares the whole specifier set before the placement loop",
			calls)
	}

	// One call carrying every distinct specifier — not one call per specifier,
	// and not one per specifier that happens to be looked at twice. The fixture
	// has to be big enough for the difference to be visible.
	refs := importParityVocabulary(t, walk, importParityFixture)
	total := len(refs.imports)
	if total < 10 {
		t.Fatalf("the fixture carries only %d specifiers; one-call and per-call are hard to tell apart",
			total)
	}
	if names != total {
		t.Errorf("the batch carried %d names, want all %d specifiers the walk reached", names, total)
	}
}

// TestClosureCarriesAQualifiedNameBindingAcrossLanguages drives the qualified-
// name mirror through the production build path.
//
// treeB introduces `import 'Service/logger'` into app.ts. A whole index of treeB
// binds that import to the Kubernetes resource node in k8s/svc.yaml by its
// qualified name (resolver.go:3961-3972) — a file no cascade key names, since
// dirIndex has no `Service/logger` and lastDirIndex["logger"] offers only the
// two decoy directories. A cascade-only closure carries the decoys and leaves
// the import parked, which is what the edge comparison below catches.
func TestClosureCarriesAQualifiedNameBindingAcrossLanguages(t *testing.T) {
	treeA := map[string]string{
		"app.ts": "export function boot() { return 0; }\n",
		// No `spec.ports`: the port node the Kubernetes extractor would emit
		// beside the resource has no bearing on the import binding and only
		// widens what this case has to account for.
		"k8s/svc.yaml":      "apiVersion: v1\nkind: Service\nmetadata:\n  name: logger\n",
		"a/logger/index.ts": "export function closureOnlyDecoyA() { return 1; }\n",
		"b/logger/index.ts": "export function closureOnlyDecoyB() { return 2; }\n",
	}
	treeB := map[string]string{}
	for p, body := range treeA {
		treeB[p] = body
	}
	// A side-effect import: the module exports no name the change mentions, so
	// the NAME arm of collectIntroduced cannot be what places the file. Only
	// the import arm can.
	treeB["app.ts"] = "import 'Service/logger';\n\nexport function boot() { return 0; }\n"

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report, "k8s/svc.yaml")

	// The divergence this case is really about: the import edge itself. A
	// cascade-only closure carries the two decoys, never re-derives
	// k8s/svc.yaml, and the composed reader leaves app.ts's import parked where
	// a whole index binds the resource node.
	builderSameStrings(t, "GetOutEdges(repo/app.ts)",
		builderRenderEdges(c.composed.GetOutEdges("repo/app.ts")),
		builderRenderEdges(c.flat.GetOutEdges("repo/app.ts")))

	// builderAssertReadersAgree is deliberately NOT used here, and the reason is
	// not this closure: the composed view's NodeCount counter disagrees with its
	// own AllNodes whenever a re-derived file emits a node whose ID is not
	// derived from its path — which every QualName-producing extractor in this
	// repository does (`<repo>/k8s::Service::_default::logger`,
	// parser/languages/kubernetes.go:89; the same shape in mybatis.go:152,:211
	// and dbt.go:167). Measured on a control with an EMPTY closure, the YAML
	// file being the change itself: composed NodeCount=3, composed AllNodes=4,
	// flat NodeCount=4 — no closure arm involved. The node SETS agree in both
	// shapes; only the counter does not. Recorded as a pre-existing composition
	// defect rather than absorbed here.
	builderSameStrings(t, "AllNodes",
		builderRenderNodes(c.composed.AllNodes()), builderRenderNodes(c.flat.AllNodes()))
	builderSameStrings(t, "AllEdges",
		builderRenderEdges(c.composed.AllEdges()), builderRenderEdges(c.flat.AllEdges()))
}

// TestClosureLeavesTheRelativeImportPassShapesUnplaced is the measurement that
// stands in for a mirror of resolveRelativeImports, and the alarm that fires if
// the measurement ever stops holding.
//
// The pass (relative_imports.go) is often described as binding four families
// whose specifiers carry no `./` prefix: Python stems, Dart URIs, C-family
// quoted includes and PHP literal includes. On the whole-index path only the
// first two are reachable. The pass keys on what `e.To` still is when it runs
// (:171-253) and it runs at resolver.go:1808, AFTER the resolve loop has put
// every `unresolved::import::…` edge through resolveImport — which ends by
// stamping `e.To = "external::" + importPath` (resolver.go:4123). The C-family
// arm (:204-241) and the PHP arm (:242-249) both test for the
// `unresolved::import::` prefix that rewrite destroyed, so neither ever fires;
// the python arm here misses for a second reason, resolvePython probing its
// stem against repository-prefixed node IDs.
//
// So the closure places nothing for those shapes, and this case is what keeps
// that honest: it builds the real commit layer for all three, requires the
// closure NOT to carry the targets (the cost bound), and requires the composed
// reader to equal a whole index anyway (the correctness bound). If the resolver
// is ever fixed to reach those edges, the reader comparison goes red and names
// the mirror that then has to be written.
func TestClosureLeavesTheRelativeImportPassShapesUnplaced(t *testing.T) {
	treeA := map[string]string{
		"csame/main.cpp":    "int csame() { return 0; }\n",
		"csame/sibling.h":   "#pragma once\nint closureOnlySibling();\n",
		"pypkg/__init__.py": "def pkg():\n    return 0\n",
		"pypkg/app.py":      "def go():\n    return 0\n",
		"pypkg/leaf.py":     "def closure_only_leaf():\n    return 1\n",
		"phpdir/app.php":    "<?php\nfunction appfn() { return 0; }\n",
		"phpdir/lib.php":    "<?php\nfunction closureOnlyLib() { return 1; }\n",
	}
	treeB := map[string]string{}
	for p, body := range treeA {
		treeB[p] = body
	}
	treeB["csame/main.cpp"] = "#include \"sibling.h\"\n\nint csame() { return 0; }\n"
	treeB["pypkg/app.py"] = "from . import leaf\n\n\ndef go():\n    return 0\n"
	treeB["phpdir/app.php"] = "<?php\nrequire_once __DIR__ . '/lib.php';\n"

	c := buildClosureCase(t, treeA, treeB)
	for _, unwanted := range []string{"csame/sibling.h", "phpdir/lib.php"} {
		if slices.Contains(c.report.ClosurePaths, unwanted) {
			t.Errorf("the closure carries %q, but the relative-import pass binds nothing for that "+
				"shape (the include edge stays external:: — resolver.go:4123 rewrote it before "+
				"relative_imports.go:204-249 could test its prefix); it carries %v",
				unwanted, c.report.ClosurePaths)
		}
	}
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestClosureRelativePassShapesStayExternalInAWholeIndex is the measurement the
// case above rests on, isolated so a change in the resolver shows up as a
// failure HERE — where the evidence is — rather than as an unexplained
// divergence in a composed reader.
func TestClosureRelativePassShapesStayExternalInAWholeIndex(t *testing.T) {
	store, _ := importParityCase(t, importParityFixture)

	for _, tc := range []struct {
		importer string
		want     string
	}{
		{"csame/main.cpp", "external::sibling.h"},
		{"native/main.cpp", "external::helper.h"},
		{"phpdir/app.php", "external::/lib.php"},
		{"pypkg/app.py", "external::pypkg/leaf"},
	} {
		t.Run(tc.importer, func(t *testing.T) {
			var targets []string
			for _, e := range store.GetOutEdges(builderGraphPath(builderRepoPrefix, tc.importer)) {
				if e == nil || e.Kind != graph.EdgeImports {
					continue
				}
				targets = append(targets, e.To)
			}
			sort.Strings(targets)
			if !slices.Contains(targets, tc.want) {
				t.Errorf("the resolver now binds %s's import somewhere other than %q (targets %v). "+
					"resolveRelativeImports has started reaching this family, so the closure needs "+
					"the mirror builder_closure.go's importedFiles doc says it does not",
					tc.importer, tc.want, targets)
			}
		})
	}
}
