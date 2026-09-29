package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A working-tree generation built over the previously published one: the
// child plans only the paths whose admitted input changed since its parent,
// writes payload for those and their named frontier, and composes with the
// parent's chain to exactly what an independent whole index of the checkout
// serves. These run through the real builder and store over the
// accumulated-edit module (accumulated_dirty_fixture_test.go), with private
// roots.

type chainChildRun struct {
	t       *testing.T
	store   *store_sqlite.Store
	repoDir string
	layout  accumulatedDirtyLayout
	chains  *dirtyChainBuilder
	root    int64
	dirty   map[string]struct{}
}

// newChainChildRun commits the module, makes units 0..dirty-1 dirty and
// publishes them as one direct generation — the chain root the tests build a
// child over.
func newChainChildRun(t *testing.T, name string, layout accumulatedDirtyLayout, dirty int) *chainChildRun {
	t.Helper()
	store := builderOpenStore(t, "chain-child-"+name)
	repoDir := accumulatedDirtyRepo(t, layout, store)
	builder := builderNewBuilder(store)
	r := &chainChildRun{
		t: t, store: store, repoDir: repoDir, layout: layout,
		chains: newDirtyChainBuilder(t, builder, store, repoDir, true),
		dirty:  map[string]struct{}{},
	}
	r.chains.compact = false
	for i := 0; i < dirty; i++ {
		accumulatedDirtyWriteUnit(t, repoDir, layout, i, true, false)
		r.dirty[accumulatedDirtyUnitPath(layout, i)] = struct{}{}
	}
	root, report, _ := r.chains.build()
	if report.ParentGenerationID != 0 || report.ChainDepth != 1 {
		t.Fatalf("the chain root was built over %d at depth %d, want direct", report.ParentGenerationID, report.ChainDepth)
	}
	r.root = root
	return r
}

func (r *chainChildRun) write(rel, body string) {
	r.t.Helper()
	full := filepath.Join(r.repoDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", rel, err)
	}
}

func (r *chainChildRun) remove(rel string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.repoDir, filepath.FromSlash(rel))); err != nil {
		r.t.Fatalf("remove %s: %v", rel, err)
	}
}

// child builds the next generation and requires it to be a delta over parent.
func (r *chainChildRun) child(parent int64) (int64, BuildReport, []int64) {
	r.t.Helper()
	id, report, chain := r.chains.build()
	if report.ParentGenerationID != parent {
		r.t.Fatalf("generation %d was built over %d (fallback %q), want a delta over %d",
			id, report.ParentGenerationID, report.ChainFallbackReason, parent)
	}
	if report.ChainDepth != len(chain) {
		r.t.Fatalf("generation %d reports depth %d for chain %v", id, report.ChainDepth, chain)
	}
	return id, report, chain
}

// nodeFiles is every distinct file the generation itself stores node rows
// at ("" for pathless nodes).
func (r *chainChildRun) nodeFiles(id int64) []string {
	r.t.Helper()
	return parityColumn(r.t, parityOpenRaw(r.t, r.store),
		`SELECT DISTINCT file_path FROM nodes WHERE view_gen = ? ORDER BY file_path`, id)
}

// masks is the generation's own file claims, path -> ownership mode.
func (r *chainChildRun) masks(id int64) map[string]string {
	r.t.Helper()
	rows, err := parityOpenRaw(r.t, r.store).Query(
		`SELECT file_path, ownership_mode FROM generation_file_masks WHERE view_gen = ?`, id)
	if err != nil {
		r.t.Fatalf("masks of %d: %v", id, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var p, mode string
		if err := rows.Scan(&p, &mode); err != nil {
			r.t.Fatalf("scan mask: %v", err)
		}
		out[strings.TrimPrefix(p, builderRepoPrefix+"/")] = mode
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("masks of %d: %v", id, err)
	}
	return out
}

func (r *chainChildRun) parity(chain []int64, label string) {
	r.t.Helper()
	if result := assertCleanIndexParityChain(r.t, r.store, chain, r.repoDir, label, true); !result.ok() {
		r.t.Fatalf("%s: composed chain %v differs from the clean index: %v", label, chain, result.Diffs)
	}
}

func TestDirtyChildUndoOfAddedFileDeletes(t *testing.T) {
	const added = "extra/extra.go"
	store := "package extra\n\n// Extra exists only in the working tree.\nfunc Extra() int {\n\treturn 7\n}\n"
	r := newChainChildRunWith(t, "undo-add", 3, func(r *chainChildRun) { r.write(added, store) })
	r.remove(added)

	id, report, chain := r.child(r.root)
	if report.DeletedFiles != 1 {
		t.Fatalf("the undo of an added file planned %d deletions, want 1", report.DeletedFiles)
	}
	if mode := r.masks(id)[added]; mode != string(store_sqlite.OwnershipDelete) {
		t.Errorf("the removed file carries mask %q, want delete", mode)
	}
	composed := dirtyChainComposed(t, r.store, chain)
	if got := composed.GetFileNodes(builderRepoPrefix + "/" + added); len(got) > 0 {
		t.Errorf("the removed file still serves %d nodes", len(got))
	}
	if got := composed.FindNodesByName("Extra"); len(got) > 0 {
		t.Errorf("FindNodesByName(Extra) still answers %d nodes", len(got))
	}
	r.parity(chain, "undo-add")
}

// newChainChildRunWith is newChainChildRun with an extra working-tree change
// applied before the root is published.
func newChainChildRunWith(t *testing.T, name string, dirty int, extra func(*chainChildRun)) *chainChildRun {
	t.Helper()
	store := builderOpenStore(t, "chain-child-"+name)
	repoDir := accumulatedDirtyRepo(t, accumulatedDirtyIndependent, store)
	builder := builderNewBuilder(store)
	r := &chainChildRun{
		t: t, store: store, repoDir: repoDir, layout: accumulatedDirtyIndependent,
		chains: newDirtyChainBuilder(t, builder, store, repoDir, true),
		dirty:  map[string]struct{}{},
	}
	r.chains.compact = false
	for i := 0; i < dirty; i++ {
		accumulatedDirtyWriteUnit(t, repoDir, accumulatedDirtyIndependent, i, true, false)
		r.dirty[accumulatedDirtyUnitPath(accumulatedDirtyIndependent, i)] = struct{}{}
	}
	extra(r)
	root, report, _ := r.chains.build()
	if report.ParentGenerationID != 0 {
		t.Fatalf("the chain root was built over %d, want direct", report.ParentGenerationID)
	}
	r.root = root
	return r
}

func TestDirtyChildRenameDeletesOldPath(t *testing.T) {
	r := newChainChildRun(t, "rename", accumulatedDirtyIndependent, 3)
	old := accumulatedDirtyUnitPath(accumulatedDirtyIndependent, 7)
	moved := filepath.ToSlash(filepath.Join(filepath.Dir(old), "moved.go"))
	body, err := os.ReadFile(filepath.Join(r.repoDir, filepath.FromSlash(old)))
	if err != nil {
		t.Fatal(err)
	}
	r.remove(old)
	r.write(moved, string(body))

	id, report, chain := r.child(r.root)
	if report.DeletedFiles != 1 || report.AddedFiles != 1 {
		t.Fatalf("the rename planned added=%d deleted=%d, want one of each", report.AddedFiles, report.DeletedFiles)
	}
	masks := r.masks(id)
	if masks[old] != string(store_sqlite.OwnershipDelete) {
		t.Errorf("the old path carries mask %q, want delete", masks[old])
	}
	if masks[moved] != string(store_sqlite.OwnershipReplace) {
		t.Errorf("the new path carries mask %q, want replace", masks[moved])
	}
	composed := dirtyChainComposed(t, r.store, chain)
	if got := composed.GetFileNodes(builderRepoPrefix + "/" + old); len(got) > 0 {
		t.Errorf("the old path still serves %d nodes", len(got))
	}
	if got := composed.GetFileNodes(builderRepoPrefix + "/" + moved); len(got) == 0 {
		t.Error("the new path serves nothing")
	}
	r.parity(chain, "rename")
}

func TestDirtyChildManifestChangeFallsBackDirect(t *testing.T) {
	r := newChainChildRun(t, "manifest", accumulatedDirtyIndependent, 3)
	r.write("go.mod", "module "+accumulatedDirtyModule+"\n\ngo 1.23\n")

	// The builder refuses the delta outright…
	manifest, reason := loadDirtyChainManifest(context.Background(), r.store, []int64{r.root})
	if reason != "" {
		t.Fatalf("the root's manifest does not resolve: %s", reason)
	}
	req := r.chains.request()
	req.Base = commitLayerBase{Reader: dirtyChainComposed(t, r.store, []int64{r.root}), corpus: r.store}
	req.Identity.BaseGenerationID = r.root
	req.parent, req.parentManifest, req.parentDepth = r.root, manifest, 1
	_, _, err := r.chains.builder.BuildDirtyLayer(context.Background(), req)
	var fallback *DirtyChainFallbackError
	if !errors.As(err, &fallback) || fallback.Reason != dirtyChainFallbackDependencyManifestChanged {
		t.Fatalf("a go.mod change over a dirty parent returned %v, want the %s fallback",
			err, dirtyChainFallbackDependencyManifestChanged)
	}

	// …and the state is built direct, with the reason on its report.
	_, report, chain := r.chains.build()
	if report.ParentGenerationID != 0 || report.ChainDepth != 1 {
		t.Fatalf("the fallback build stood on %d at depth %d, want direct", report.ParentGenerationID, report.ChainDepth)
	}
	if report.ChainFallbackReason != dirtyChainFallbackDependencyManifestChanged ||
		report.Work.ChainFallbackReasons[dirtyChainFallbackDependencyManifestChanged] != 1 {
		t.Errorf("the direct build reports fallback %q / %v, want %s",
			report.ChainFallbackReason, report.Work.ChainFallbackReasons, dirtyChainFallbackDependencyManifestChanged)
	}
	meta, _, found, err := r.store.AtGeneration(chain[0]).InputManifest(context.Background())
	if err != nil || !found || !meta.IsFull {
		t.Errorf("the direct fallback's manifest is %+v found=%v err=%v, want a full manifest", meta, found, err)
	}
	r.parity(chain, "manifest")
}

func TestDirtyChildImportChangeRemovesOldFacts(t *testing.T) {
	r := newChainChildRun(t, "import", accumulatedDirtyIndependent, 3)
	r.write("consumer/consumer.go", `package consumer

import "`+accumulatedDirtyModule+`/chain/b"

// Consume is the chain's one importing consumer.
func Consume() int {
	return b.B()
}
`)
	id, report, chain := r.child(r.root)
	r.assertOnlyTouched(id, report)
	composed := dirtyChainComposed(t, r.store, chain)
	for _, e := range composed.GetOutEdges(builderRepoPrefix + "/consumer/consumer.go::Consume") {
		if e != nil && strings.Contains(e.To, "/chain/a/") {
			t.Errorf("the consumer still reaches %s after dropping the import", e.To)
		}
	}
	r.parity(chain, "import")
}

func TestDirtyChildPartialUndoReemitsCommittedFile(t *testing.T) {
	r := newChainChildRun(t, "partial-undo", accumulatedDirtyIndependent, 5)
	undone := accumulatedDirtyUnitPath(accumulatedDirtyIndependent, 2)
	accumulatedDirtyWriteUnit(t, r.repoDir, accumulatedDirtyIndependent, 2, false, false)

	id, report, chain := r.child(r.root)
	if report.ChangedFiles != 1 || !slices.Contains(report.IndexedPaths, undone) {
		t.Fatalf("the undo planned changed=%d indexed=%v, want %s re-emitted", report.ChangedFiles, report.IndexedPaths, undone)
	}
	if mode := r.masks(id)[undone]; mode != string(store_sqlite.OwnershipReplace) {
		t.Errorf("the undone file carries mask %q, want replace (the committed bytes re-emitted)", mode)
	}
	base := builderRenderNodes(r.store.AtGeneration(0).GetFileNodes(builderRepoPrefix + "/" + undone))
	composed := dirtyChainComposed(t, r.store, chain)
	if got := builderRenderNodes(composed.GetFileNodes(builderRepoPrefix + "/" + undone)); !slices.Equal(got, base) {
		t.Errorf("after the partial undo the view serves\n  %v\nthe committed base serves\n  %v", got, base)
	}
	r.assertOnlyTouched(id, report, undone)
	r.parity(chain, "partial-undo")
}

func TestDirtyChildSamePackageIsolatesOutput(t *testing.T) {
	r := newChainChildRun(t, "same-package", accumulatedDirtySamePackage, accumulatedDirtyUnits)
	edited := accumulatedDirtyUnitPath(accumulatedDirtySamePackage, accumulatedDirtyBodyTarget)
	accumulatedDirtyWriteUnit(t, r.repoDir, accumulatedDirtySamePackage, accumulatedDirtyBodyTarget, true, false)

	id, report, chain := r.child(r.root)
	allowed := map[string]struct{}{"": {}, builderRepoPrefix + "/" + edited: {}}
	for _, p := range report.ContextRetainedPaths {
		allowed[builderRepoPrefix+"/"+p] = struct{}{}
	}
	for _, f := range r.nodeFiles(id) {
		if _, ok := allowed[f]; !ok {
			t.Errorf("the child stores node rows at %s, outside the edited file and its retained closure %v",
				f, report.ContextRetainedPaths)
		}
	}
	if report.Work.ParserInputs > 1+len(report.ContextPaths)+len(report.ContextRetainedPaths) {
		t.Errorf("the child parsed %d files for one edit (context %v, retained %v)",
			report.Work.ParserInputs, report.ContextPaths, report.ContextRetainedPaths)
	}
	r.assertOnlyTouched(id, report, edited)
	r.parity(chain, "same-package")
}

func TestDirtyChildSignatureChangeKeepsDependentsBound(t *testing.T) {
	r := newChainChildRun(t, "signature", accumulatedDirtyIndependent, 3)
	r.write("chain/b/b.go", `package b

import "`+accumulatedDirtyModule+`/chain/c"

// B calls down the chain.
func B(delta int) int {
	return c.C() + delta
}
`)
	// a, the changed signature's caller, is not re-derived: the delta re-binds
	// its call through the incoming leg.
	id, report, chain := r.child(r.root)
	if slices.Contains(report.IndexedPaths, "chain/a/a.go") {
		t.Errorf("the child re-derived the caller a: %v", report.IndexedPaths)
	}
	r.assertOnlyTouched(id, report)
	composed := dirtyChainComposed(t, r.store, chain)
	var callsB bool
	for _, e := range composed.GetOutEdges(builderRepoPrefix + "/chain/a/a.go::A") {
		if e != nil && e.To == builderRepoPrefix+"/chain/b/b.go::B" {
			callsB = true
		}
	}
	if !callsB {
		t.Error("a's call no longer reaches B after its signature changed")
	}
	r.parity(chain, "signature")
}

// assertOnlyTouched fails when a parser input or a node row of the child
// lands on an accumulated dirty path the edit did not touch.
func (r *chainChildRun) assertOnlyTouched(id int64, report BuildReport, touched ...string) {
	r.t.Helper()
	for _, p := range report.Work.ParserInputPaths {
		if _, acc := r.dirty[p]; acc && !slices.Contains(touched, p) {
			r.t.Errorf("the child parsed the untouched dirty file %s", p)
		}
	}
	for _, f := range r.nodeFiles(id) {
		rel := strings.TrimPrefix(f, builderRepoPrefix+"/")
		if _, acc := r.dirty[rel]; acc && !slices.Contains(touched, rel) {
			r.t.Errorf("the child stores node rows at the untouched dirty file %s", rel)
		}
	}
}

func TestDirtyChildReusesParentPayload(t *testing.T) {
	r := newChainChildRun(t, "reuse", accumulatedDirtyIndependent, accumulatedDirtyUnits)
	edited := accumulatedDirtyUnitPath(accumulatedDirtyIndependent, accumulatedDirtyBodyTarget)
	accumulatedDirtyWriteUnit(t, r.repoDir, accumulatedDirtyIndependent, accumulatedDirtyBodyTarget, true, false)

	recordLastEditDelta(nil)
	id, report, chain := r.child(r.root)
	if got, want := report.IndexedPaths, []string{edited}; !slices.Equal(got, want) {
		t.Errorf("the child indexed %v, want the edited file alone %v", got, want)
	}
	if delta := LastEditDeltaReport(); delta == nil || !slices.Equal(delta.Paths, []string{edited}) || len(delta.SharedRowEmitters) > 0 {
		t.Errorf("the child's delta re-derived %+v, want the edited file alone", delta)
	}
	if got := report.Work.ReusedPriorPayloadFiles; got != accumulatedDirtyUnits {
		t.Errorf("the child reused %d parent files, want %d", got, accumulatedDirtyUnits)
	}
	if report.ManifestEntriesWritten != 1 {
		t.Errorf("the child wrote %d manifest rows, want 1 (the edited path)", report.ManifestEntriesWritten)
	}
	r.assertOnlyTouched(id, report, edited)
	for _, f := range r.nodeFiles(id) {
		if f != "" && f != builderRepoPrefix+"/"+edited {
			t.Errorf("the child stores node rows at %s", f)
		}
	}
	census, err := r.store.GenerationPayloadRowCensus(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if census.NodeFiles > 2 {
		t.Errorf("the child stores nodes at %d files, want the edited file and the pathless stubs", census.NodeFiles)
	}
	meta, entries, found, err := r.store.AtGeneration(id).InputManifest(context.Background())
	if err != nil || !found || meta.IsFull || len(entries) != 1 || entries[0].FilePath != edited {
		t.Errorf("the child's manifest is meta=%+v entries=%+v found=%v err=%v, want one delta row for %s",
			meta, entries, found, err, edited)
	}
	r.parity(chain, "reuse")
}
