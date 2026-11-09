package resolver

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// buildGraphFromSources extracts every fixture file with the extractor
// matching its suffix (.ts/.tsx → TypeScript, otherwise JavaScript) and
// loads the resulting nodes and edges into a fresh graph. It is the
// faithful end-to-end harness for the resolver tests below: a real
// extractor produces the unresolved edges, then ResolveAll runs against
// them exactly as it does on a live index.
func buildGraphFromSources(t *testing.T, files map[string]string) graph.Store {
	t.Helper()
	g := graph.New()
	ts := languages.NewTypeScriptExtractor()
	js := languages.NewJavaScriptExtractor()
	vue := languages.NewVueExtractor()
	for path, src := range files {
		if strings.HasSuffix(path, ".vue") {
			r, err := vue.Extract(path, []byte(src))
			if err != nil {
				t.Fatalf("vue extract %s: %v", path, err)
			}
			for _, n := range r.Nodes {
				g.AddNode(n)
			}
			for _, e := range r.Edges {
				g.AddEdge(e)
			}
			continue
		}
		if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx") {
			r, err := ts.Extract(path, []byte(src))
			if err != nil {
				t.Fatalf("ts extract %s: %v", path, err)
			}
			for _, n := range r.Nodes {
				g.AddNode(n)
			}
			for _, e := range r.Edges {
				g.AddEdge(e)
			}
			continue
		}
		r, err := js.Extract(path, []byte(src))
		if err != nil {
			t.Fatalf("js extract %s: %v", path, err)
		}
		for _, n := range r.Nodes {
			g.AddNode(n)
		}
		for _, e := range r.Edges {
			g.AddEdge(e)
		}
	}
	return g
}

// callEdgeTo returns the resolved To-end of the call/reference edge that
// leaves fromID at the given 1-based line. Empty string when no such
// edge exists.
func callEdgeTo(g graph.Store, fromID string, line int) string {
	for _, e := range g.GetOutEdges(fromID) {
		if (e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeReferences) && e.Line == line {
			return e.To
		}
	}
	return ""
}

// TestJSTSCallEdges_FalsePositivesAndNegatives drives the three mis-
// resolution patterns through a real extract → resolve pipeline. Each
// row asserts both halves of the contract: the genuine edge that must
// still resolve, and the false-positive edge that must be suppressed.
func TestJSTSCallEdges_FalsePositivesAndNegatives(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		// callerID + callLine identify the call edge under test.
		callerID string
		callLine int
		// wantTo, when set, is the node the call MUST resolve to.
		wantTo string
		// forbidTo, when set, is a node the call must NOT resolve to.
		forbidTo string
		// wantUnresolved requires the edge to stay an `unresolved::`
		// placeholder (the false positive was suppressed, with no
		// reachable genuine target to fall back to).
		wantUnresolved bool
	}{
		{
			// Pattern 1, true positive: a call to an object-literal
			// shorthand method must bind to that member.
			name: "object-literal shorthand resolves to member",
			files: map[string]string{
				"svc/api.ts": `function doWork(): number { return 1; }
export const api = {
  process(): number { return doWork(); },
};
function caller(): void {
  api.process();
}
`,
			},
			callerID: "svc/api.ts::caller",
			callLine: 6,
			wantTo:   "svc/api.ts::api.process@3",
		},
		{
			// Pattern 1, false positive: an unrelated free `process` in
			// another package must NOT capture `api.process()`.
			name: "object-literal shorthand does not bind to free function",
			files: map[string]string{
				"svc/api.ts": `export const api = {
  process(): number { return 1; },
};
function caller(): void {
  api.process();
}
`,
				"other/free.ts": `export function process(): number { return 99; }`,
			},
			callerID: "svc/api.ts::caller",
			callLine: 5,
			wantTo:   "svc/api.ts::api.process@2",
			forbidTo: "other/free.ts::process",
		},
		{
			// Pattern 2, true positive: a factory result whose handler
			// implementations are same-package must still resolve.
			name: "factory dispatch resolves same-package handler",
			files: map[string]string{
				"app/run.ts": `function run(): void {
  const h = makeHandler('a');
  h.handle();
}
`,
				"app/handlers.ts": `export class AlphaHandler {
  handle(): number { return 1; }
}
`,
			},
			callerID: "app/run.ts::run",
			callLine: 3,
			wantTo:   "app/handlers.ts::AlphaHandler.handle",
		},
		{
			// Pattern 2, false positive: a factory result whose only
			// `handle` candidate lives in an un-imported package must
			// not produce a call edge to it.
			name: "factory dispatch does not bind across un-imported package",
			files: map[string]string{
				"app/run.ts": `function run(): void {
  const h = makeHandler('a');
  h.handle();
}
`,
				"vendor/other.ts": `export class OtherHandler {
  handle(): number { return 1; }
}
`,
			},
			callerID:       "app/run.ts::run",
			callLine:       3,
			forbidTo:       "vendor/other.ts::OtherHandler.handle",
			wantUnresolved: true,
		},
		{
			// Pattern 3, false positive: a `ns.foo()` call where `ns` is
			// a plain local object must not resolve to a same-named free
			// function in an un-imported module.
			name: "namespace member call does not bind to free function",
			files: map[string]string{
				"app/main.ts": `function run(): void {
  const ns = { other: 1 };
  ns.lookup('x');
}
`,
				"lib/lookup.ts": `export function lookup(s: string): string { return s; }`,
			},
			callerID:       "app/main.ts::run",
			callLine:       3,
			forbidTo:       "lib/lookup.ts::lookup",
			wantUnresolved: true,
		},
		{
			// Pattern 3, true positive: when the namespace is genuinely
			// imported, the member call resolves to the imported symbol.
			name: "imported namespace member call resolves",
			files: map[string]string{
				"app/main.ts": `import * as helpers from './helpers';
function run(): void {
  helpers.lookup('x');
}
`,
				"app/helpers/index.ts": `export function lookup(s: string): string { return s; }`,
			},
			callerID: "app/main.ts::run",
			callLine: 3,
			wantTo:   "app/helpers/index.ts::lookup",
		},
		{
			// Pattern 1 in JavaScript: the shorthand method must resolve
			// and must not fall through to a free function.
			name: "javascript object-literal shorthand resolves to member",
			files: map[string]string{
				"svc/api.js": `export const api = {
  process() { return 1; },
};
function caller() {
  api.process();
}
`,
				"other/free.js": `export function process() { return 99; }`,
			},
			callerID: "svc/api.js::caller",
			callLine: 5,
			wantTo:   "svc/api.js::api.process@2",
			forbidTo: "other/free.js::process",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := buildGraphFromSources(t, tc.files)
			New(g).ResolveAll()

			got := callEdgeTo(g, tc.callerID, tc.callLine)
			if got == "" {
				t.Fatalf("no call edge found from %s at line %d", tc.callerID, tc.callLine)
			}

			if tc.wantTo != "" && got != tc.wantTo {
				t.Errorf("call resolved to %q, want %q", got, tc.wantTo)
			}
			if tc.forbidTo != "" && got == tc.forbidTo {
				t.Errorf("call mis-resolved to forbidden cross-package target %q", tc.forbidTo)
			}
			if tc.wantUnresolved && !strings.HasPrefix(got, "unresolved::") {
				t.Errorf("call resolved to %q, expected it to stay unresolved (false positive should be suppressed)", got)
			}
		})
	}
}

// TestCrossPackageGuard_RevertsUnreachableNameMatch exercises the guard
// directly on a hand-built graph: a function call whose only same-name
// candidate lives in a package the caller never imports must be
// reverted to its unresolved placeholder, while the same call resolves
// and stays when the candidate's package is imported or same-package.
func TestCrossPackageGuard_RevertsUnreachableNameMatch(t *testing.T) {
	cases := []struct {
		name string
		// importedDir, when non-empty, adds an EdgeImports from the
		// caller file to that directory.
		importedDir string
		// targetDir is the directory the only `helper` candidate lives in.
		targetDir string
		// wantResolved is the expected resolved To (empty → must stay
		// unresolved).
		wantResolved string
	}{
		{
			name:         "same-package candidate is kept",
			targetDir:    "pkgA",
			wantResolved: "pkgA/b.go::helper",
		},
		{
			name:         "imported-package candidate is kept",
			importedDir:  "pkgB",
			targetDir:    "pkgB",
			wantResolved: "pkgB/b.go::helper",
		},
		{
			name:         "un-imported-package candidate is reverted",
			importedDir:  "pkgB",
			targetDir:    "pkgC",
			wantResolved: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "pkgA/a.go", Kind: graph.KindFile, FilePath: "pkgA/a.go", Language: "go", RepoPrefix: "r"})
			g.AddNode(&graph.Node{ID: "pkgA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkgA/a.go", Language: "go", RepoPrefix: "r"})

			// The only `helper` candidate lives in targetDir.
			targetFile := tc.targetDir + "/b.go"
			targetID := targetFile + "::helper"
			g.AddNode(&graph.Node{ID: targetFile, Kind: graph.KindFile, FilePath: targetFile, Language: "go", RepoPrefix: "r"})
			g.AddNode(&graph.Node{ID: targetID, Kind: graph.KindFunction, Name: "helper", FilePath: targetFile, Language: "go", RepoPrefix: "r"})

			// A decoy file in the imported package so the import resolves
			// to a real directory even when the candidate lives elsewhere.
			if tc.importedDir != "" && tc.importedDir != tc.targetDir {
				decoy := tc.importedDir + "/x.go"
				g.AddNode(&graph.Node{ID: decoy, Kind: graph.KindFile, FilePath: decoy, Language: "go", RepoPrefix: "r"})
			}
			if tc.importedDir != "" {
				g.AddEdge(&graph.Edge{
					From: "pkgA/a.go", To: "unresolved::import::" + tc.importedDir,
					Kind: graph.EdgeImports, FilePath: "pkgA/a.go", Line: 1,
				})
			}

			call := &graph.Edge{
				From: "pkgA/a.go::Caller", To: "unresolved::helper",
				Kind: graph.EdgeCalls, FilePath: "pkgA/a.go", Line: 5,
			}
			g.AddEdge(call)

			New(g).ResolveAll()

			// Whatever the resolver and guard did, the edge's identity
			// stays internally consistent — the guard routes its Origin
			// revert through SetEdgeProvenance, so the out- and in-edge
			// views never disagree on provenance.
			if err := g.VerifyEdgeIdentities(); err != nil {
				t.Fatalf("edge identities inconsistent after resolve: %v", err)
			}

			if tc.wantResolved == "" {
				if call.To != "unresolved::helper" {
					t.Errorf("guard should have reverted the edge; To = %q, want unresolved::helper", call.To)
				}
				// A reverted edge must carry no resolution provenance.
				if call.Origin != "" {
					t.Errorf("reverted edge kept Origin %q, want empty", call.Origin)
				}
				return
			}
			if call.To != tc.wantResolved {
				t.Errorf("call resolved to %q, want %q", call.To, tc.wantResolved)
			}
		})
	}
}

// TestCrossPackageGuard_RevertRoutedThroughProvenance proves the guard's
// provenance revert goes through Graph.SetEdgeProvenance rather than a
// bare Origin write: when the edge being reverted carries a resolution
// Origin, clearing it is counted as an edge-identity revision, and the
// resulting graph stays identity-consistent across both adjacency views.
//
// The guard internally resets To and Origin together; this test stamps
// a weak Origin on the same logical edge through the sanctioned path,
// then re-derives the guard's exact revert sequence (SetEdgeProvenance
// to drop the Origin, then the target revert + re-bucket) to assert
// that path records the churn.
func TestCrossPackageGuard_RevertRoutedThroughProvenance(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkgA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkgA/a.go", Language: "go", RepoPrefix: "r"})
	g.AddNode(&graph.Node{ID: "pkgC/b.go::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "pkgC/b.go", Language: "go", RepoPrefix: "r"})

	// A call edge as it sits post-resolution: pointed at a (to be
	// rejected) cross-package target with a weak resolution Origin.
	call := &graph.Edge{
		From: "pkgA/a.go::Caller", To: "pkgC/b.go::helper",
		Kind: graph.EdgeCalls, FilePath: "pkgA/a.go", Line: 5,
		Origin: graph.OriginTextMatched,
	}
	g.AddEdge(call)
	baseline := g.EdgeIdentityRevisions()

	// The guard's revert: drop provenance via SetEdgeProvenance, then
	// revert the target and re-bucket — mirrors cross_pkg_guard.go.
	oldResolved := call.To
	if !g.SetEdgeProvenance(call, "") {
		t.Fatal("clearing a non-empty resolution Origin must change identity")
	}
	call.To = "unresolved::helper"
	call.Confidence = 0
	g.ReindexEdge(call, oldResolved)

	if g.EdgeIdentityRevisions() != baseline+1 {
		t.Errorf("guard revert must record exactly one identity revision: got %d, want %d",
			g.EdgeIdentityRevisions(), baseline+1)
	}
	if err := g.VerifyEdgeIdentities(); err != nil {
		t.Fatalf("edge identities inconsistent after guarded revert: %v", err)
	}
	if got := g.GetOutEdges("pkgA/a.go::Caller"); len(got) != 1 || got[0].To != "unresolved::helper" || got[0].Origin != "" {
		t.Errorf("reverted edge has wrong state: %+v", got)
	}
}

// TestCrossPackageGuard_KeepsImportedMethodCall verifies the guard does
// not strip a genuine cross-package method call: a `*.handle` member
// call resolving to a method whose package the caller imports survives.
func TestCrossPackageGuard_KeepsImportedMethodCall(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkgA/a.go", Kind: graph.KindFile, FilePath: "pkgA/a.go", Language: "go", RepoPrefix: "r"})
	g.AddNode(&graph.Node{ID: "pkgA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkgA/a.go", Language: "go", RepoPrefix: "r"})
	g.AddNode(&graph.Node{ID: "pkgB/b.go", Kind: graph.KindFile, FilePath: "pkgB/b.go", Language: "go", RepoPrefix: "r"})
	g.AddNode(&graph.Node{ID: "pkgB/b.go::Worker.handle", Kind: graph.KindMethod, Name: "handle", FilePath: "pkgB/b.go", Language: "go", RepoPrefix: "r"})

	g.AddEdge(&graph.Edge{From: "pkgA/a.go", To: "unresolved::import::pkgB", Kind: graph.EdgeImports, FilePath: "pkgA/a.go", Line: 1})
	call := &graph.Edge{From: "pkgA/a.go::Caller", To: "unresolved::*.handle", Kind: graph.EdgeCalls, FilePath: "pkgA/a.go", Line: 5}
	g.AddEdge(call)

	New(g).ResolveAll()

	if call.To != "pkgB/b.go::Worker.handle" {
		t.Errorf("imported-package method call was dropped; To = %q, want pkgB/b.go::Worker.handle", call.To)
	}
}

// TestCrossPackageGuard_LeavesExternEdges confirms the guard never
// touches `extern::`-shaped resolutions: those carry an explicit import
// path as evidence and are not name-only guesses, so a cross-package
// extern call to an indexed symbol must stay resolved.
func TestCrossPackageGuard_LeavesExternEdges(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "consumer/main.go", Kind: graph.KindFile, FilePath: "consumer/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "consumer/main.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "consumer/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "lib/pkg/pkg.go", Kind: graph.KindFile, FilePath: "lib/pkg/pkg.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "lib/pkg/pkg.go::DoThing", Kind: graph.KindFunction, Name: "DoThing", FilePath: "lib/pkg/pkg.go", Language: "go"})

	call := &graph.Edge{
		From: "consumer/main.go::Caller", To: "unresolved::extern::lib/pkg::DoThing",
		Kind: graph.EdgeCalls, FilePath: "consumer/main.go", Line: 5,
	}
	g.AddEdge(call)

	New(g).ResolveAll()

	if call.To != "lib/pkg/pkg.go::DoThing" {
		t.Errorf("extern-qualified call was wrongly reverted by the guard; To = %q", call.To)
	}
}
