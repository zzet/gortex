package indexer

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/goanalysis"
)

// Semantic enrichment on a working-tree chain. A chained child carries a
// handful of nodes, so the admission floor judged against the child alone
// never admits a compiler pass: its edited file would be published without
// the type-resolved facts a direct build of the same state carries. The floor
// is judged against the whole working-tree state instead (the chain's census
// plus the child's own), and these tests hold the result against a clean
// semantic index of the same checkout.

// goTypesManager is a semantic manager with the in-process go/types provider
// — the compiler-grade Go pass the daemon runs.
func goTypesManager(t testing.TB) *semantic.Manager {
	t.Helper()
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	mgr.RegisterProvider(goanalysis.NewProvider(goanalysis.ModeTypeCheck, false, zap.NewNop()))
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

// builderIndexSemantic is builderIndex with the go/types pass enabled.
func builderIndexSemantic(mgr *semantic.Manager) func(testing.TB, *store_sqlite.Store, string) {
	return func(t testing.TB, store *store_sqlite.Store, dir string) {
		t.Helper()
		idx := New(store, builderRegistry(), config.Default().Index, zap.NewNop())
		defer idx.Close()
		idx.SetRepoPrefix(builderRepoPrefix)
		idx.SetWorkspaceID(builderRepoPrefix)
		idx.SetProjectID(builderRepoPrefix)
		idx.SetSemanticManager(mgr)
		if _, err := idx.Index(dir); err != nil {
			t.Fatalf("semantic index %s: %v", dir, err)
		}
	}
}

// semanticChainFixture is a checkout whose base corpus and every working-tree
// build run the go/types pass. The working-tree builds load the whole module
// (the reference load) unless GX_SUBSECOND_SEMANTIC_HANDLE_ROOTS=1 asks for the
// product's handle-rooted load.
func semanticChainFixture(t *testing.T, tree map[string]string) (*coordinatorFixture, *CheckoutCoordinator, *semantic.Manager) {
	t.Helper()
	return semanticChainFixtureWith(t, tree, os.Getenv("GX_SUBSECOND_SEMANTIC_HANDLE_ROOTS") == "1")
}

// semanticChainFixtureWith is semanticChainFixture with the load chosen
// explicitly: handleRoots is the product's load (handle-rooted, sibling
// bodies stripped, type-check state retained); false is the whole-module
// reference load (SparseGenerationBuilder.wholeModuleCompilerLoad).
func semanticChainFixtureWith(t *testing.T, tree map[string]string, handleRoots bool) (*coordinatorFixture, *CheckoutCoordinator, *semantic.Manager) {
	t.Helper()
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "16")
	mgr := goTypesManager(t)
	f := newCoordinatorFixtureIndexedBy(t, tree, builderIndexSemantic(mgr))
	builder := builderNewBuilder(f.store)
	builder.Semantic = mgr
	builder.wholeModuleCompilerLoad = !handleRoots
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	return f, c, mgr
}

// semanticBindingUnitSource is an edit unit that also binds a named type of
// its own (KindNNN), so the unit's file has semantic_binding_types rows: the
// fixture's own units bind only int, which the projection drops, and a
// binding oracle over them compares two empty sets. The type is the unit's
// own and uniquely named, so no unit's closure reaches another's.
func semanticBindingUnitSource(layout accumulatedDirtyLayout, i int, body, comment bool) string {
	return accumulatedDirtyUnitSource(layout, i, body, comment) + fmt.Sprintf(`
// Kind%03[1]d is this unit's named type.
type Kind%03[1]d int

// Holder%03[1]d binds the unit's named type.
func Holder%03[1]d() Kind%03[1]d {
	var k Kind%03[1]d = %[1]d
	return k
}
`, i)
}

// semanticBindingTree is retentionTree with every unit binding a named type.
func semanticBindingTree() map[string]string {
	tree := map[string]string{"go.mod": "module " + accumulatedDirtyModule + "\n\ngo 1.22\n"}
	for i := 0; i < retentionUnits; i++ {
		for _, layout := range []accumulatedDirtyLayout{accumulatedDirtyIndependent, accumulatedDirtySamePackage} {
			tree[accumulatedDirtyUnitPath(layout, i)] = semanticBindingUnitSource(layout, i, false, false)
		}
	}
	return tree
}

func semanticWriteUnit(t *testing.T, repoDir string, layout accumulatedDirtyLayout, i int, body bool) {
	t.Helper()
	full := filepath.Join(repoDir, filepath.FromSlash(accumulatedDirtyUnitPath(layout, i)))
	if err := os.WriteFile(full, []byte(semanticBindingUnitSource(layout, i, body, false)), 0o644); err != nil {
		t.Fatalf("write unit %d: %v", i, err)
	}
}

// semanticGenerationBindingRows counts the binding rows one generation wrote.
func semanticGenerationBindingRows(t *testing.T, f *coordinatorFixture, generationID int64) int {
	t.Helper()
	var n int
	if err := parityOpenRaw(t, f.store).QueryRow(
		`SELECT COUNT(*) FROM semantic_binding_types WHERE repo_prefix = ? AND view_gen = ?`,
		builderRepoPrefix, generationID).Scan(&n); err != nil {
		t.Fatalf("count binding rows of generation %d: %v", generationID, err)
	}
	return n
}

// semanticGenerationPackages is packages(H): the directories of the Go files
// a published generation carries, the roots a handle-rooted load of it takes.
func semanticGenerationPackages(t *testing.T, f *coordinatorFixture, generationID int64) []string {
	t.Helper()
	dirs := map[string]struct{}{}
	for _, p := range parityColumn(t, parityOpenRaw(t, f.store),
		`SELECT DISTINCT file_path FROM nodes WHERE view_gen = ? AND language = 'go'`, generationID) {
		if strings.HasSuffix(p, ".go") {
			dirs[path.Dir(p)] = struct{}{}
		}
	}
	out := make([]string, 0, len(dirs))
	for d := range dirs {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// goTypesRan reports whether a build's enrichment stage ran the Go pass.
func goTypesRan(w *GenerationWorkCounters) bool {
	return w != nil && slices.Contains(w.CompilerContext.Ran, "go")
}

// semanticRenderEdge renders an edge with one documented normalization: a
// references edge keeps no line. A function that reads a local twice (the
// body edit's `v += 1` and `return v`) has ONE references edge for the
// (from, to, kind) identity, and its line names one of the reads: which one
// differs between a whole-repository pass and a working-tree build of the
// same file even when the build is direct (the chain root below shows it), so
// the line is not evidence about the chain. Both candidate lines are reads of
// the same local.
func semanticRenderEdge(e *graph.Edge) string {
	if e.Kind == graph.EdgeReferences {
		copied := *e
		copied.Line = 0
		return builderRenderEdge(&copied)
	}
	return builderRenderEdge(e)
}

func semanticRenderEdges(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			out = append(out, semanticRenderEdge(e))
		}
	}
	slices.Sort(out)
	return out
}

// semanticEdges renders the edges a type-resolving pass stamped.
func semanticEdges(r graph.Reader) []string {
	var stamped []*graph.Edge
	for _, e := range r.AllEdges() {
		if e == nil {
			continue
		}
		source, _ := e.Meta["semantic_source"].(string)
		if source == "" && !strings.HasPrefix(e.Origin, "lsp") {
			continue
		}
		stamped = append(stamped, e)
	}
	return semanticRenderEdges(stamped)
}

// assertSemanticParity compares the served checkout view with a clean
// semantic index of the worktree: every node and edge (origin and metadata
// included), the type-resolved edges on their own, and the composed
// semantic_binding_types rows.
func assertSemanticParity(t *testing.T, f *coordinatorFixture, mgr *semantic.Manager, label string) {
	t.Helper()
	for _, diff := range semanticParityDiffs(t, f, mgr, label) {
		t.Error(diff)
	}
}

// semanticParityDiffs is assertSemanticParity returning the differences
// instead of failing, for tests that document a known gap.
func semanticParityDiffs(t *testing.T, f *coordinatorFixture, mgr *semantic.Manager, label string) []string {
	t.Helper()
	view := chainMaterialize(t, f)
	defer view.Close()
	clean := builderOpenStore(t, "semantic-clean-"+label)
	builderIndexSemantic(mgr)(t, clean, f.worktree)

	var diffs []string
	add := func(diff string) {
		if diff != "" {
			diffs = append(diffs, diff)
		}
	}
	add(semanticSetDiff(label+": type-resolved edges", semanticEdges(view.Reader), semanticEdges(clean)))
	add(semanticSetDiff(label+": all edges", semanticRenderEdges(view.Reader.AllEdges()), semanticRenderEdges(clean.AllEdges())))
	add(semanticSetDiff(label+": all nodes", builderRenderNodes(view.Reader.AllNodes()), builderRenderNodes(clean.AllNodes())))

	// Bindings are composed file-keyed: a generation's rows answer for the
	// files it claims (Replace/Delete masks), the layers beneath for every
	// other file. The ordinary index keeps its compiler projection in the
	// provider's memory (the pass corpus is in memory), so the clean index
	// persists no rows at all; the reference for a claimed file is therefore
	// a direct compiler pass over the worktree, and for an unclaimed file the
	// base corpus's own rows. A generation that writes rows for files it does
	// not claim, or stale rows for a file it does, fails here.
	const bindings = `SELECT file_path, line || '|' || name || '|' || type_name FROM semantic_binding_types WHERE repo_prefix = ? AND view_gen = ?`
	db := parityOpenRaw(t, f.store)
	base := parityRows(t, db, bindings, 0)
	composed := slices.Clone(base)
	claimed := map[string]struct{}{}
	for _, generationID := range view.Generations() {
		masks := parityMasks(t, db, generationID)
		kept := composed[:0:0]
		for _, row := range composed {
			file, _, _ := strings.Cut(row, "\t")
			if _, masked := masks.files[file]; !masked {
				kept = append(kept, row)
			}
		}
		for file := range masks.files {
			claimed[file] = struct{}{}
		}
		composed = append(kept, parityRows(t, db, bindings, generationID)...)
	}
	slices.Sort(composed)
	reference := semanticReferenceBindingRows(t, f, label)
	var want []string
	claimedWithRows := 0
	for _, row := range reference {
		file, _, _ := strings.Cut(row, "\t")
		if _, ok := claimed[file]; ok {
			want = append(want, row)
			claimedWithRows++
		}
	}
	for _, row := range base {
		file, _, _ := strings.Cut(row, "\t")
		if _, ok := claimed[file]; !ok {
			want = append(want, row)
		}
	}
	slices.Sort(want)
	if claimedWithRows == 0 {
		t.Logf("%s: no file the view's generations claim has semantic_binding_types rows; the binding comparison is vacuous", label)
	}
	add(semanticSetDiff(label+": semantic_binding_types", composed, want))
	return diffs
}

// semanticReferenceBindingRows is a direct whole-module compiler pass's
// binding rows for the worktree as it is now, rendered like the oracle's.
func semanticReferenceBindingRows(t *testing.T, f *coordinatorFixture, label string) []string {
	t.Helper()
	store := builderOpenStore(t, "semantic-bindings-"+label)
	provider := goanalysis.NewProvider(goanalysis.ModeTypeCheck, false, zap.NewNop())
	defer provider.Close()
	if _, err := provider.EnrichRepo(store, builderRepoPrefix, f.worktree); err != nil {
		t.Fatalf("%s: reference compiler pass: %v", label, err)
	}
	return parityRows(t, parityOpenRaw(t, store),
		`SELECT file_path, line || '|' || name || '|' || type_name FROM semantic_binding_types WHERE repo_prefix = ? AND view_gen = ?`, 0)
}

// semanticSetDiff renders the rows one side has and the other lacks; empty
// when the multisets agree.
func semanticSetDiff(what string, got, want []string) string {
	count := func(rows []string) map[string]int {
		m := map[string]int{}
		for _, r := range rows {
			m[r]++
		}
		return m
	}
	g, w := count(got), count(want)
	var extra, missing []string
	for r, n := range g {
		if n > w[r] {
			extra = append(extra, fmt.Sprintf("%dx %s", n-w[r], r))
		}
	}
	for r, n := range w {
		if n > g[r] {
			missing = append(missing, fmt.Sprintf("%dx %s", n-g[r], r))
		}
	}
	if len(extra) == 0 && len(missing) == 0 {
		return ""
	}
	slices.Sort(extra)
	slices.Sort(missing)
	return fmt.Sprintf("%s: %d rows only in the served view, %d only in the clean index\n  served only:\n    %s\n  clean only:\n    %s",
		what, len(extra), len(missing), strings.Join(extra, "\n    "), strings.Join(missing, "\n    "))
}

func TestDirtyChainChildRunsSemanticEnrichment(t *testing.T) {
	// Every unit binds a named type, so the binding half of the oracle
	// compares real rows: a generation that writes the whole repository's
	// rows instead of its own files' duplicates every unmasked file's rows in
	// the file-keyed composition.
	f, c, mgr := semanticChainFixtureWith(t, semanticBindingTree(), false)
	coordinatorReconcile(t, c)

	// The whole-module load of this checkout: 20 one-file packages and one
	// 20-file package.
	const modulePackages, moduleFiles = retentionUnits + 1, 2 * retentionUnits
	assertCounter := func(label string, w *GenerationWorkCounters) {
		t.Helper()
		cc := w.CompilerContext
		if !cc.Measured || cc.Scope != semantic.CompilerScopeFull || cc.ScopeReason != semantic.CompilerScopeReasonDisabled ||
			cc.Loads != 1 || cc.Packages != modulePackages || cc.Files != moduleFiles {
			t.Errorf("%s: compiler context %+v, want measured, scope %s (%s), 1 load of %d packages / %d files",
				label, cc, semantic.CompilerScopeFull, semantic.CompilerScopeReasonDisabled, modulePackages, moduleFiles)
		}
	}

	// The chain root dirties enough units for the working-tree state to
	// clear the admission floor on its own: its direct build runs the pass.
	for i := 0; i < 8; i++ {
		semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, i, true)
	}
	root := coordinatorReconcile(t, c)
	if root.DirtyParentGenerationID != 0 || !goTypesRan(root.DirtyWork) {
		t.Fatalf("the chain root = %+v (compiler context %+v), want a direct build that ran go/types",
			root, root.DirtyWork.CompilerContext)
	}
	assertCounter("root", root.DirtyWork)
	assertSemanticParity(t, f, mgr, "root")

	// Chained single-file edits: each child is a handful of nodes, and still
	// runs the pass, because the working-tree state it describes clears the
	// floor.
	for k, edit := range []struct {
		layout accumulatedDirtyLayout
		unit   int
	}{
		{accumulatedDirtyIndependent, 9}, {accumulatedDirtySamePackage, 3}, {accumulatedDirtyIndependent, 12},
	} {
		semanticWriteUnit(t, f.worktree, edit.layout, edit.unit, true)
		out := coordinatorReconcile(t, c)
		if out.DirtyParentGenerationID == 0 {
			t.Fatalf("edit %d = %+v, want a chained build", k, out)
		}
		w := out.DirtyWork
		if !goTypesRan(w) {
			t.Fatalf("chained edit %d ran no go/types pass: compiler context %+v", k, w.CompilerContext)
		}
		t.Logf("chained edit %d: parent=%d depth=%d parser_inputs=%d context=%d withheld=%d retained=%d go/types=%v packages=%d files=%d scope=%s",
			k, out.DirtyParentGenerationID, out.DirtyChainDepth, w.ParserInputs, w.PlanContext, w.ContextWithheldFiles,
			w.ContextRetainedFiles, w.CompilerContext.Ran, w.CompilerContext.Packages, w.CompilerContext.Files, w.CompilerContext.Scope)
		assertCounter(fmt.Sprintf("chained-%d", k), w)
		if n := semanticGenerationBindingRows(t, f, out.DirtyGenerationID); n == 0 {
			t.Errorf("chained edit %d: the generation wrote no binding rows; the oracle's binding half is vacuous", k)
		}
		assertSemanticParity(t, f, mgr, fmt.Sprintf("chained-%d", k))
	}
}
