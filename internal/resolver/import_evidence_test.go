package resolver

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestImportEvidence_DisambiguatesBareJSTSCalls drives the import-closure
// disambiguation through the real extract → resolve pipeline (the same
// harness as the cross-package guard tests). Each row pins one leg of the
// precedence contract documented in import_evidence.go.
func TestImportEvidence_DisambiguatesBareJSTSCalls(t *testing.T) {
	// The library-side alias-cast export (`createStoreImpl as
	// CreateStore`) lands as a variable/constant node — the value-callee
	// shape (zustand's `persist`-style export) whose repo-wide ambiguity
	// refuses every call edge without import evidence.
	vanillaCastExport := `type CreateStore = (init?: unknown) => unknown;
const createStoreImpl = (init: unknown): unknown => init;
export const createStore = createStoreImpl as CreateStore;
export const other = 1;
`
	cases := []struct {
		name     string
		files    map[string]string
		callerID string
		callLine int
		// wantTo, when set, is the node the call MUST resolve to.
		wantTo string
		// forbidTo, when set, is a node the call must NOT resolve to.
		forbidTo string
		// wantUnresolved requires the edge to stay an `unresolved::`
		// placeholder (ambiguity without import evidence still refuses).
		wantUnresolved bool
	}{
		{
			// Cross-dir ambiguity that used to refuse: the caller's
			// explicit import of one candidate's file resolves it.
			name: "import closure wins over ambiguity refusal",
			files: map[string]string{
				"src/vanilla.ts": vanillaCastExport,
				"tests/shadow/helper.test.ts": `export const createStore = () => ({ local: true });
`,
				"tests/basic.test.ts": `import { createStore } from '../src/vanilla';
function makeCounter(): unknown {
  return createStore(() => ({ count: 0 }));
}
`,
			},
			callerID: "tests/basic.test.ts::makeCounter",
			callLine: 3,
			wantTo:   "src/vanilla.ts::createStore",
			forbidTo: "tests/shadow/helper.test.ts::createStore",
		},
		{
			// A same-directory neighbour defining the name is NOT ambient
			// scope in the ES module system — the explicit import of
			// another candidate must beat the same-dir shadow the
			// locality loop would otherwise bind.
			name: "same-dir shadow loses to explicit import",
			files: map[string]string{
				"src/vanilla.ts": `export function createStore(init: unknown): unknown { return init; }
`,
				"tests/util.ts": `export function createStore(): unknown { return { helper: true }; }
`,
				"tests/a.test.ts": `import { createStore } from '../src/vanilla';
function setup(): unknown {
  return createStore({});
}
`,
			},
			callerID: "tests/a.test.ts::setup",
			callLine: 3,
			wantTo:   "src/vanilla.ts::createStore",
			forbidTo: "tests/util.ts::createStore",
		},
		{
			// No import evidence: cross-dir value-callee ambiguity keeps
			// today's refusal — no arbitrary winner.
			name: "no-import ambiguity still refuses",
			files: map[string]string{
				"src/vanilla.ts": vanillaCastExport,
				"other/helper.ts": `export const createStore = () => ({ local: true });
`,
				"app/main.ts": `function run(): unknown {
  return createStore();
}
`,
			},
			callerID:       "app/main.ts::run",
			callLine:       2,
			wantUnresolved: true,
		},
		{
			// Both candidates' files are imported (one for an unrelated
			// binding): the import statement alone cannot arbitrate, so
			// the pick refuses exactly like the no-import case.
			// Alias-cast exports (KindVariable) keep today's behaviour
			// deterministic: the value-callee fallback refuses on
			// repo-wide ambiguity.
			name: "multiple imported candidates still refuse",
			files: map[string]string{
				"a/one.ts": `type MakeA = () => unknown;
const implA = () => ({ a: true });
export const createStore = implA as MakeA;
`,
				"b/two.ts": `type MakeB = () => unknown;
const implB = () => ({ b: true });
export const createStore = implB as MakeB;
export const helper = () => 1;
`,
				"app/main.ts": `import { createStore } from '../a/one';
import { helper } from '../b/two';
function run(): unknown {
  helper();
  return createStore();
}
`,
			},
			callerID:       "app/main.ts::run",
			callLine:       5,
			wantUnresolved: true,
		},
		{
			// A module-local definition blocks the import pick: the call
			// binds to the caller file's own symbol even though another
			// candidate's file is imported (for an unrelated binding).
			name: "module-local definition beats import evidence",
			files: map[string]string{
				"src/vanilla.ts": vanillaCastExport,
				"tests/local.test.ts": `import { other } from '../src/vanilla';
export function createStore(): unknown { return { local: other }; }
function setup(): unknown {
  return createStore();
}
`,
			},
			callerID: "tests/local.test.ts::setup",
			callLine: 4,
			wantTo:   "tests/local.test.ts::createStore",
			forbidTo: "src/vanilla.ts::createStore",
		},
		{
			// File-local tier: with NO import, the caller's own helper
			// must win over a same-named helper in a neighbouring file
			// of the same directory (candidate iteration order used to
			// decide this — zustand's persistSync tests bound to
			// persistAsync's helper).
			name: "own-file helper beats same-dir neighbour shadow",
			files: map[string]string{
				"tests/sync.test.ts": `export function createStore(): unknown { return { sync: true }; }
function setup(): unknown {
  return createStore();
}
`,
				"tests/async.test.ts": `export function createStore(): unknown { return { async: true }; }
`,
			},
			callerID: "tests/sync.test.ts::setup",
			callLine: 3,
			wantTo:   "tests/sync.test.ts::createStore",
			forbidTo: "tests/async.test.ts::createStore",
		},
		{
			// Barrel hop: importing a re-exporting module makes the
			// re-exported module's symbols import-evidence too.
			name: "re-export barrel hop carries the evidence",
			files: map[string]string{
				"src/vanilla.ts": vanillaCastExport,
				"src/index.ts": `export { createStore } from './vanilla.ts';
`,
				"tests/shadow/helper.test.ts": `export const createStore = () => ({ local: true });
`,
				"tests/barrel.test.ts": `import { createStore } from '../src/index';
function setup(): unknown {
  return createStore(() => ({}));
}
`,
			},
			callerID: "tests/barrel.test.ts::setup",
			callLine: 3,
			wantTo:   "src/vanilla.ts::createStore",
			forbidTo: "tests/shadow/helper.test.ts::createStore",
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
				t.Errorf("call mis-resolved to forbidden candidate %q", tc.forbidTo)
			}
			if tc.wantUnresolved && !strings.HasPrefix(got, "unresolved::") {
				t.Errorf("call resolved to %q, expected it to stay unresolved", got)
			}
		})
	}
}

// TestImportEvidence_ResolvedEdgeStampsProvenance asserts the winning edge
// carries the structural-evidence tier: ast_resolved origin and the
// import_closure resolution marker, so the cross-package guard (which only
// polices text_matched / ast_inferred) never reverts it.
func TestImportEvidence_ResolvedEdgeStampsProvenance(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"src/vanilla.ts": `export function createStore(init: unknown): unknown { return init; }
`,
		"tests/util.ts": `export function createStore(): unknown { return { helper: true }; }
`,
		"tests/a.test.ts": `import { createStore } from '../src/vanilla';
function setup(): unknown {
  return createStore({});
}
`,
	})
	New(g).ResolveAll()

	for _, e := range g.GetOutEdges("tests/a.test.ts::setup") {
		if e.Kind != graph.EdgeCalls || e.Line != 3 {
			continue
		}
		if e.To != "src/vanilla.ts::createStore" {
			t.Fatalf("call resolved to %q, want src/vanilla.ts::createStore", e.To)
		}
		if e.Origin != graph.OriginASTResolved {
			t.Errorf("origin = %q, want %q", e.Origin, graph.OriginASTResolved)
		}
		if e.Meta == nil || e.Meta["resolution"] != "import_closure" {
			t.Errorf("meta resolution = %v, want import_closure", e.Meta)
		}
		return
	}
	t.Fatalf("no call edge found from tests/a.test.ts::setup at line 3")
}

// TestPickImportEvidenceCallee_LanguageGate pins the correctness gate: the
// pick never runs for non-JS/TS callers, so Go (directory-scoped packages,
// where a bare call can never name another package's symbol) and Python
// (`import x` does not bring bare names into scope) resolution is
// bit-for-bit unchanged.
func TestPickImportEvidenceCallee_LanguageGate(t *testing.T) {
	g := graph.New()
	r := New(g)
	candidates := []*graph.Node{
		{ID: "pkgA/b.go::helper", Kind: graph.KindFunction, FilePath: "pkgA/b.go", Name: "helper"},
		{ID: "pkgB/c.go::helper", Kind: graph.KindFunction, FilePath: "pkgB/c.go", Name: "helper"},
	}
	if pick := r.pickImportEvidenceCallee("pkgA/a.go", "helper", candidates); pick != nil {
		t.Errorf("go caller: pick = %v, want nil (language gate)", pick.ID)
	}
	if pick := r.pickImportEvidenceCallee("app/mod.py", "helper", candidates); pick != nil {
		t.Errorf("python caller: pick = %v, want nil (language gate)", pick.ID)
	}
}

// TestImportEvidence_SingleCandidateCrossDir pins that import evidence is
// consulted even when the name has exactly ONE candidate in the repo. A
// cross-directory call whose caller explicitly imports the candidate's file
// is structural evidence, not a name-only guess: without this the call fell
// into the "first same-repo match" fallback, got tagged text_matched, and
// find_usages hid it behind the import line. Same rule for a .ts caller
// and for an SFC <script>.
func TestImportEvidence_SingleCandidateCrossDir(t *testing.T) {
	g := buildGraphFromSources(t, map[string]string{
		"composable/useX.ts": `export function useX(): number { return 1; }
`,
		"components/a.ts": `import { useX } from '../composable/useX';
export const x = useX();
`,
		"components/B.vue": `<script setup lang="ts">
import { useX } from '../composable/useX';
const x = useX();
</script>
`,
	})
	New(g).ResolveAll()

	for _, tc := range []struct {
		from string
		line int
	}{
		{"components/a.ts", 2},
		{"components/B.vue", 3},
	} {
		var found bool
		for _, e := range g.GetOutEdges(tc.from) {
			if e.Kind != graph.EdgeCalls || e.Line != tc.line {
				continue
			}
			found = true
			if e.To != "composable/useX.ts::useX" {
				t.Errorf("%s: call resolved to %q, want composable/useX.ts::useX", tc.from, e.To)
			}
			if e.Origin != graph.OriginASTResolved {
				t.Errorf("%s: origin = %q, want %q", tc.from, e.Origin, graph.OriginASTResolved)
			}
		}
		if !found {
			t.Errorf("%s: no call edge at line %d", tc.from, tc.line)
		}
	}
}
