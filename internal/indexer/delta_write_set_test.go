package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The committed-base delta's WRITE SET.
//
// A dedicated delta widens its file set to the affected closure because the
// pass can only resolve against what its own generation carries. What the
// generation KEEPS is a different question, and this file pins it on the
// production entrypoint the daemon reaches for a commit on a dedicated base:
// BuildClaimedDedicatedDelta.
//
// Before the three clauses below were fixed, the separation was inert on any
// corpus that records cross-file value flow at the destination file — which is
// every corpus with a const, a var or a cross-file call. A ten-file commit
// therefore published its whole two-hundred-file resolve closure at full node
// density.

// ---------------------------------------------------------------- fixtures --

// deltaWriteSetTree is the closure-heavy shape: one hub that calls every leaf,
// nine satellites beside it, and `leaves` leaf files the hub drags into the
// closure. `edited` applies a body-only edit to the ten non-leaf files — no
// signature, no identity, so every leaf re-derives to exactly what the layer
// below already holds.
func deltaWriteSetTree(leaves int, edited bool) map[string]string {
	tree := make(map[string]string, leaves+10)
	hub := "package dedicated\n\nfunc Hub() {\n"
	for i := range leaves {
		name := fmt.Sprintf("Leaf%03d", i)
		tree[fmt.Sprintf("leaf%03d.go", i)] = fmt.Sprintf("package dedicated\n\nfunc %s() {\n}\n", name)
		hub += "\t" + name + "()\n"
	}
	if edited {
		hub += "\tLeaf000()\n"
	}
	hub += "}\n"
	tree["hub.go"] = hub
	for i := range 9 {
		body := "\tLeaf000()\n"
		if edited {
			body += "\tLeaf000()\n"
		}
		tree[fmt.Sprintf("sat%d.go", i)] = fmt.Sprintf("package dedicated\n\nfunc Sat%d() {\n%s}\n", i, body)
	}
	return tree
}

// deltaWriteSetChangedPaths is the ten-file change set the edit above makes.
func deltaWriteSetChangedPaths(prefix string) []string {
	paths := []string{prefix + "/hub.go"}
	for i := range 9 {
		paths = append(paths, fmt.Sprintf("%s/sat%d.go", prefix, i))
	}
	slices.Sort(paths)
	return paths
}

// deltaWriteSetPkgTree mirrors the sustained-workload corpus (sustainedIOGenerateFixture
// in cmd/gortex): package-per-directory, an aliased cross-package import in
// every file, a const and a var block for bulk, four intra-package calls, one
// cross-package call and a revision probe. It is the shape the P4 diagnosis
// measured, and the shape on which the separation used to withdraw nothing.
// `edited` applies the same body edit to the first ten files.
func deltaWriteSetPkgTree(files, packages int, edited bool) map[string]string {
	tree := map[string]string{"go.mod": "module example.invalid/dedicated\n\ngo 1.24\n"}
	for index := range files {
		pkg := index % packages
		crossPkg, crossFile := pkg, pkg
		if next := pkg + 1; next < packages {
			crossPkg, crossFile = next, next
		}
		tree[fmt.Sprintf("p%03d/file%05d.go", pkg, index)] =
			deltaWriteSetPkgSource(pkg, index, pkg, crossPkg, crossFile, edited && index < 10)
	}
	return tree
}

func deltaWriteSetPkgSource(pkg, index, callee, crossPkg, crossFile int, changed bool) string {
	out := fmt.Sprintf("package p%03d\n\n", pkg)
	if crossPkg != pkg {
		out += fmt.Sprintf("import cross %q\n\n", fmt.Sprintf("example.invalid/dedicated/p%03d", crossPkg))
	}
	out += fmt.Sprintf("const gxSalt%05d = %d\n\n", index, index*7919%100003)
	out += "var (\n"
	for i := range 12 {
		out += fmt.Sprintf("\tgxData%05dN%02d = %d\n", index, i, (index+i*7919)%100003)
	}
	out += ")\n\n"
	if changed {
		out += fmt.Sprintf("func Fn%05dS0() int { return gxSalt%05d + 1 }\n\n", index, index)
	} else {
		out += fmt.Sprintf("func Fn%05dS0() int { return gxSalt%05d }\n\n", index, index)
	}
	for i := 1; i <= 4; i++ {
		if callee == index {
			out += fmt.Sprintf("func Fn%05dS%d() int { return Fn%05dS0() + gxData%05dN%02d }\n\n", index, i, index, index, i)
			continue
		}
		out += fmt.Sprintf("func Fn%05dS%d() int { return Fn%05dS0() + Fn%05dS0() + gxData%05dN%02d }\n\n", index, i, callee, index, index, i)
	}
	if crossPkg != pkg {
		out += fmt.Sprintf("func Fn%05dS5() int { return cross.Fn%05dS0() + cross.Fn%05dS1() }\n\n", index, crossFile, crossFile)
	} else {
		out += fmt.Sprintf("func Fn%05dS5() int { return Fn%05dS0() + Fn%05dS1() }\n\n", index, crossFile, crossFile)
	}
	probe := 0
	if changed {
		probe = 1
	}
	out += fmt.Sprintf("func GxProbe%05dRev%d() int { return Fn%05dS0() }\n", index, probe, index)
	return out
}

// deltaWriteSetPkgChangedPaths is the ten-file change set of the package tree.
func deltaWriteSetPkgChangedPaths(prefix string, packages int) []string {
	paths := make([]string, 0, 10)
	for index := range 10 {
		paths = append(paths, fmt.Sprintf("%s/p%03d/file%05d.go", prefix, index%packages, index))
	}
	return paths
}

func deltaWriteSetWriteTree(t testing.TB, root string, tree map[string]string) {
	t.Helper()
	for name, body := range tree {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// deltaWriteSetStoreBytes is the on-disk size of the store and its write-ahead
// log, which is what the P4 diagnosis measured as "+8.22 MB of store" for one
// ten-file commit.
func deltaWriteSetStoreBytes(t *testing.T, storePath string) int64 {
	t.Helper()
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		info, err := os.Stat(storePath + suffix)
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}

// deltaWriteSetPayloadPaths is the payload census for one generation: every
// path it physically carries a node, an edge or a files-inventory row at.
func deltaWriteSetPayloadPaths(t *testing.T, store *store_sqlite.Store, repoPrefix string, generationID int64) []string {
	t.Helper()
	handle := store.AtGeneration(generationID)
	seen := map[string]struct{}{}
	for _, node := range handle.AllNodes() {
		if node != nil && node.FilePath != "" {
			seen[node.FilePath] = struct{}{}
		}
	}
	for _, edge := range handle.AllEdges() {
		if edge != nil && edge.FilePath != "" {
			seen[edge.FilePath] = struct{}{}
		}
	}
	rows, err := handle.FileMetasForRepo(repoPrefix)
	if err != nil {
		t.Fatalf("FileMetasForRepo: %v", err)
	}
	for _, row := range rows {
		seen[row.FilePath] = struct{}{}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// deltaWriteSetFixture is a dedicated base over one tree, ready for exactly
// one committed-base delta over it.
type deltaWriteSetFixture struct {
	builder   *SparseGenerationBuilder
	request   dedicatedBuilderFixtureRequest
	baseClaim store_sqlite.DedicatedBaseBuildClaim
	baseID    int64
	base      LayerBase
	git       func(...string) string
}

func newDeltaWriteSetFixture(t testing.TB, tree map[string]string) deltaWriteSetFixture {
	t.Helper()
	ctx := context.Background()
	builder, request, git := privateDedicatedBuilderFixture(t)
	deltaWriteSetWriteTree(t, request.RootPath, tree)
	git("add", "-A")
	git("-c", "user.name=Delta Test", "-c", "user.email=delta@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-qm", "write-set fixture")
	request.Identity.TreeOID = git("rev-parse", "HEAD^{tree}")
	request.Identity.ProvenanceCommitOID = git("rev-parse", "HEAD")

	catalog := builder.Store.Catalog()
	authority, err := catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: request.Identity.GraphID,
		Owner:   store_sqlite.DedicatedBaseOwner{CheckoutID: request.Identity.CheckoutID, Incarnation: "private-incarnation"},
		Token:   "delta-write-set-authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: authority,
		Identity: store_sqlite.DedicatedBaseIdentity{TreeOID: request.Identity.TreeOID, ConfigHash: request.Identity.ConfigHash,
			ExtractorVersions: request.Identity.ExtractorVersions, ResolverVersion: request.Identity.ResolverVersion},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "delta-write-set-full", CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		t.Fatal(err)
	}
	materializer := &graphview.Materializer{Store: builder.Store, Catalog: catalog,
		Leases: graphview.NewLeaseManager(), Logger: builder.Logger}
	view, err := materializer.MaterializeRefView(ctx, claim.Desire.Authority.GraphID, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(view.Close)
	return deltaWriteSetFixture{
		builder: builder, request: request, baseClaim: claim, baseID: id,
		// Exactly the base the daemon hands BuildClaimedDedicatedDelta —
		// dedicated_base_advance.go:230.
		base: commitLayerBase{Reader: view.Reader, facts: newAncestryRefFacts(builder.Store, view)},
		git:  git,
	}
}

// advance writes the edited tree, commits it, reserves a delta over the base
// and builds it through the production entrypoint.
func (f deltaWriteSetFixture) advance(t *testing.T, tree map[string]string) (int64, BuildReport) {
	t.Helper()
	ctx := context.Background()
	deltaWriteSetWriteTree(t, f.request.RootPath, tree)
	f.git("add", "-A")
	f.git("-c", "user.name=Delta Test", "-c", "user.email=delta@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-qm", "ten-file edit")
	target := f.git("rev-parse", "HEAD^{tree}")

	catalog := f.builder.Store.Catalog()
	p, _, err := catalog.DedicatedBasePublication(ctx, f.baseClaim.Desire.Authority.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	identity := f.baseClaim.Desire.Identity
	identity.TreeOID = target
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: f.baseClaim.Desire.Authority, ExpectedDesiredEpoch: p.Desire.Epoch, Identity: identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, ExpectedActiveGenerationID: f.baseID,
		AttemptToken:     fmt.Sprintf("delta-write-set-attempt-%d", desire.Epoch),
		BaseGenerationID: f.baseID, LowerViewFingerprint: fmt.Sprintf("test-full-lower-%d", f.baseID), CreatedAt: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, report, err := f.builder.BuildClaimedDedicatedDelta(ctx, ClaimedDedicatedDeltaRequest{
		Claim: claim, Base: f.base, BaseTreeOID: f.request.Identity.TreeOID,
		RepoDir: f.request.RootPath, RootPath: f.request.RootPath,
		WorkspaceID: f.request.WorkspaceID, ProjectID: f.request.ProjectID,
	})
	if err != nil {
		t.Fatalf("BuildClaimedDedicatedDelta: %v", err)
	}
	return id, report
}

// -------------------------------------------------------- the entrypoint ---

// TestDedicatedDeltaWriteSetIsTheChangeNotTheClosure is the acceptance test on
// the production entrypoint: a ten-file body-only commit over a dedicated base
// leaves the published delta carrying payload rows for the ten changed files,
// not for the two hundred closure files the pass had to read to resolve them.
func TestDedicatedDeltaWriteSetIsTheChangeNotTheClosure(t *testing.T) {
	const leaves = 200
	fixture := newDeltaWriteSetFixture(t, deltaWriteSetTree(leaves, false))
	generationID, report := fixture.advance(t, deltaWriteSetTree(leaves, true))

	handle := fixture.builder.Store.AtGeneration(generationID)
	carried := deltaWriteSetPayloadPaths(t, fixture.builder.Store, fixture.request.RepoPrefix, generationID)
	t.Logf("write set: closure=%d truncated=%v cap=%d | pass produced %d nodes / %d edges | "+
		"generation carries %d paths, %d nodes, %d edges | context masks=%d retained=%d in_memory=%v",
		len(report.ClosurePaths), report.ClosureTruncated, report.ClosureCap,
		report.NodeCount, report.EdgeCount,
		len(carried), len(handle.AllNodes()), len(handle.AllEdges()),
		report.ContextMasks, len(report.ContextRetainedPaths), report.ContextHeldInMemory)

	if len(report.ClosurePaths) < leaves/2 {
		t.Fatalf("closure spans %d files; the measurement needs the leaf population", len(report.ClosurePaths))
	}
	want := deltaWriteSetChangedPaths(fixture.request.RepoPrefix)
	if !slices.Equal(carried, want) {
		t.Fatalf("the delta carries payload at %d paths, want the %d changed files\ngot %v",
			len(carried), len(want), carried)
	}
	if len(report.ContextRetainedPaths) != 0 {
		t.Fatalf("a body-only edit retained %d closure files: %v",
			len(report.ContextRetainedPaths), report.ContextRetainedPaths)
	}
}

// TestDedicatedDeltaWriteSetOnACrossPackageCorpus runs the same ten-file
// commit over the sustained-workload corpus shape — the one the P4 diagnosis
// measured, where every file carries a cross-package import, a const, a var
// block and incoming value flow from another file.
//
// This is the shape the withdrawal used to be INERT on: the comparison read
// only the outbound half of a path's recorded adjacency, so every file whose
// constants and cross-file calls arrive as edges INTO its functions compared a
// full carried set against an empty base set and kept its claim. The assertion
// is a ratio rather than an exact set because which files of a multi-file
// package the closure walk reaches is not fixed; what is fixed is that the
// write set tracks the change and not the closure.
func TestDedicatedDeltaWriteSetOnACrossPackageCorpus(t *testing.T) {
	const files, packages = 240, 12
	fixture := newDeltaWriteSetFixture(t, deltaWriteSetPkgTree(files, packages, false))
	before := deltaWriteSetStoreBytes(t, fixture.request.StorePath)
	generationID, report := fixture.advance(t, deltaWriteSetPkgTree(files, packages, true))
	after := deltaWriteSetStoreBytes(t, fixture.request.StorePath)
	t.Logf("store bytes across the one delta: %d -> %d (%+d)", before, after, after-before)

	handle := fixture.builder.Store.AtGeneration(generationID)
	carried := deltaWriteSetPayloadPaths(t, fixture.builder.Store, fixture.request.RepoPrefix, generationID)
	nodes := len(handle.AllNodes())
	t.Logf("write set: closure=%d truncated=%v cap=%d | pass produced %d nodes / %d edges | "+
		"generation carries %d paths, %d nodes, %d edges | context masks=%d retained=%d in_memory=%v",
		len(report.ClosurePaths), report.ClosureTruncated, report.ClosureCap,
		report.NodeCount, report.EdgeCount,
		len(carried), nodes, len(handle.AllEdges()),
		report.ContextMasks, len(report.ContextRetainedPaths), report.ContextHeldInMemory)

	if len(report.ClosurePaths) < 150 {
		t.Fatalf("closure spans %d files; the measurement needs a wide closure", len(report.ClosurePaths))
	}
	for _, want := range deltaWriteSetPkgChangedPaths(fixture.request.RepoPrefix, packages) {
		if !slices.Contains(carried, want) {
			t.Fatalf("the delta does not carry its own changed file %q: %v", want, carried)
		}
	}
	// The write set used to be the whole closure at full node density: 203
	// paths and 2,029 nodes for this fixture, ~10 nodes a file, the same
	// density as the root generation — 105% of the closure and 100% of the
	// pass's own output. Half is a deliberately loose ceiling: which files of
	// a multi-file package the closure walk reaches, and therefore how many
	// import edges the pass hangs off a representative the change set touches,
	// moves between runs (12, 31 and 50 paths observed on this fixture). The
	// per-clause pins are the unit tests below; this one pins that the write
	// set no longer tracks the closure at all.
	if ceiling := len(report.ClosurePaths) / 2; len(carried) > ceiling {
		t.Fatalf("the delta carries payload at %d paths over a %d-file closure (ceiling %d); retained=%d: %v",
			len(carried), len(report.ClosurePaths), ceiling,
			len(report.ContextRetainedPaths), report.ContextRetainedPaths)
	}
	if ceiling := report.NodeCount / 2; nodes > ceiling {
		t.Fatalf("the delta keeps %d of the %d nodes the pass produced (ceiling %d)",
			nodes, report.NodeCount, ceiling)
	}
}

// ------------------------------------------------ the comparison clauses ---

// deltaWriteSetSeparate runs the fallback separation directly over one base
// corpus and one generation payload, which is the cheapest way to pin a single
// clause of the comparison.
func deltaWriteSetSeparate(
	t *testing.T, store, handle *store_sqlite.Store, plan buildPlan,
) contextSeparation {
	t.Helper()
	separation, err := builderNewBuilder(store).separateContextPayload(
		context.Background(),
		BuildRequest{RepoPrefix: builderRepoPrefix, Base: store},
		plan, handle)
	if err != nil {
		t.Fatalf("separateContextPayload: %v", err)
	}
	return separation
}

func deltaWriteSetFn(graphPath, name string) *graph.Node {
	return &graph.Node{
		ID: graphPath + "::" + name, Kind: graph.KindFunction, Name: name,
		FilePath: graphPath, RepoPrefix: builderRepoPrefix,
	}
}

// TestContextPathWithInboundCrossFileFlowIsWithdrawn pins the clause that made
// the whole separation inert in production.
//
// helper.go's only recorded adjacency is an edge INTO one of its symbols from
// a symbol that lives somewhere else — the shape an extractor produces for a
// constant, a variable or a call flowing into a function declared here. The
// layer below records the same edge at the same path. Reading only the
// outbound half of the path's adjacency made the base set empty, the counts
// disagree, and the path keep a claim it did not need.
func TestContextPathWithInboundCrossFileFlowIsWithdrawn(t *testing.T) {
	store := builderOpenStore(t, "inbound-flow")
	sourceRel, contextRel := "caller.go", "helper.go"
	sourcePath := builderGraphPath(builderRepoPrefix, sourceRel)
	contextPath := builderGraphPath(builderRepoPrefix, contextRel)
	payload := func() ([]*graph.Node, []*graph.Edge) {
		return []*graph.Node{deltaWriteSetFn(sourcePath, "Run"), deltaWriteSetFn(contextPath, "Helper")},
			[]*graph.Edge{{
				From: sourcePath + "::Run", To: contextPath + "::Helper",
				Kind: graph.EdgeValueFlow, FilePath: contextPath, Line: 3,
			}}
	}
	nodes, edges := payload()
	store.AddBatch(nodes, edges)
	handle := builderContextDerivedHandle(t, store)
	nodes, edges = payload()
	handle.AddBatch(nodes, edges)

	plan := buildPlan{indexed: []string{sourceRel, contextRel}, context: []string{contextRel}}
	separation := deltaWriteSetSeparate(t, store, handle, plan)
	if _, gone := separation.withheld[contextPath]; !gone {
		t.Fatalf("a context path whose only adjacency enters it was retained: %v", separation.retainedPaths)
	}
	if handle.GetNode(contextPath+"::Helper") != nil {
		t.Fatalf("the withdrawn identity kept its node row")
	}
}

// TestWithdrawnPathDoesNotKeepTheEdgesRecordedAtIt pins the restore rule.
//
// The eviction reaches every edge touching a withdrawn identity, and the ones
// the generation must keep are the ones RECORDED AT A FILE IT STILL CLAIMS —
// the changed file's own calls into the withdrawn symbol. An edge recorded at
// the withdrawn path itself is that path's own entry and must stay gone, or
// the withdrawal leaves an edge row (and therefore a payload path, and
// therefore a replace claim) standing over a file it just emptied.
func TestWithdrawnPathDoesNotKeepTheEdgesRecordedAtIt(t *testing.T) {
	store := builderOpenStore(t, "restore-by-file")
	changedRel, contextRel := "caller.go", "helper.go"
	changedPath := builderGraphPath(builderRepoPrefix, changedRel)
	contextPath := builderGraphPath(builderRepoPrefix, contextRel)
	stub := builderRepoPrefix + "/unresolved::Constant"
	payload := func() ([]*graph.Node, []*graph.Edge) {
		return []*graph.Node{deltaWriteSetFn(changedPath, "Run"), deltaWriteSetFn(contextPath, "Helper")},
			[]*graph.Edge{
				// Recorded at the withdrawn path: the layer below serves it.
				{From: stub, To: contextPath + "::Helper", Kind: graph.EdgeValueFlow, FilePath: contextPath, Line: 3},
				// Recorded at the changed file: the generation claims it.
				{From: changedPath + "::Run", To: contextPath + "::Helper", Kind: graph.EdgeCalls, FilePath: changedPath, Line: 4},
			}
	}
	nodes, edges := payload()
	store.AddBatch(nodes, edges)
	handle := builderContextDerivedHandle(t, store)
	nodes, edges = payload()
	handle.AddBatch(nodes, edges)

	plan := buildPlan{indexed: []string{changedRel, contextRel}, context: []string{contextRel}}
	separation := deltaWriteSetSeparate(t, store, handle, plan)
	if _, gone := separation.withheld[contextPath]; !gone {
		t.Fatalf("the context path was retained: %v", separation.retainedPaths)
	}
	var atContext, atChanged int
	for _, edge := range handle.AllEdges() {
		switch edge.FilePath {
		case contextPath:
			atContext++
		case changedPath:
			atChanged++
		}
	}
	if atContext != 0 {
		t.Fatalf("the withdrawn path kept %d edge rows recorded at it", atContext)
	}
	if atChanged != 1 {
		t.Fatalf("the changed file kept %d of its own edges into the withdrawn symbol, want 1", atChanged)
	}
}

// TestImportRepresentativeDifferenceDoesNotBlockWithdrawal pins the third
// clause: one import statement, two spellings of the same relation.
//
// The bounded pass and the whole index pick different files of the same
// imported package to hang the import edge on. The relation is the same, the
// file's bytes are unchanged, and the pass has no better claim to its
// representative than the whole index had to its own — so the path is still
// served from below. A representative the change set touches, or one in
// another package, is a real difference and keeps the claim.
func TestImportRepresentativeDifferenceDoesNotBlockWithdrawal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		baseTarget string
		changed    []string
		withdrawn  bool
	}{
		{name: "another file of the same package", baseTarget: "pkg/two.go", withdrawn: true},
		{name: "a file the change set touches", baseTarget: "pkg/two.go",
			changed: []string{"pkg/two.go"}, withdrawn: false},
		{name: "a file in another package", baseTarget: "other/two.go", withdrawn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := builderOpenStore(t, "import-representative")
			contextRel := "importer.go"
			contextPath := builderGraphPath(builderRepoPrefix, contextRel)
			carriedTarget := builderGraphPath(builderRepoPrefix, "pkg/one.go")
			baseTarget := builderGraphPath(builderRepoPrefix, tc.baseTarget)
			importEdge := func(to string) *graph.Edge {
				return &graph.Edge{From: contextPath, To: to, Kind: graph.EdgeImports, FilePath: contextPath, Line: 3}
			}
			// The import edge hangs off the FILE node, so the fixture has to
			// carry one: without it neither endpoint lives at the candidate
			// path and foreignSource refuses the path before the comparison.
			fileNode := func() *graph.Node {
				return &graph.Node{ID: contextPath, Kind: graph.KindFile, Name: "importer.go",
					FilePath: contextPath, RepoPrefix: builderRepoPrefix}
			}
			store.AddBatch([]*graph.Node{fileNode(), deltaWriteSetFn(contextPath, "Importer")},
				[]*graph.Edge{importEdge(baseTarget)})
			handle := builderContextDerivedHandle(t, store)
			handle.AddBatch([]*graph.Node{fileNode(), deltaWriteSetFn(contextPath, "Importer")},
				[]*graph.Edge{importEdge(carriedTarget)})

			indexed := append([]string{contextRel}, tc.changed...)
			plan := buildPlan{indexed: indexed, context: []string{contextRel}}
			separation := deltaWriteSetSeparate(t, store, handle, plan)
			_, gone := separation.withheld[contextPath]
			if gone != tc.withdrawn {
				t.Fatalf("withdrawn=%v, want %v (retained %v)", gone, tc.withdrawn, separation.retainedPaths)
			}
		})
	}
}

// ----------------------------------------------------------- closure cap ---

// TestCommittedBaseClosureCapIsSizedOffTheChangeSet pins the cap split and the
// knob rule together.
//
// The committed-base delta sizes its resolution corpus off its change set
// rather than inheriting index.affected_by_reresolve_max — that knob tunes the
// incremental pipeline's re-resolve frontier over the working copy, and
// inheriting it meant a repository that raised one silently widened the
// resolution corpus of every commit on a dedicated base.
//
// It is not, however, DROPPED: the knob is a ceiling on resolve fan-out, so a
// committed base takes the minimum of the two. A configured value at or below
// the change-sized cap still bounds the walk, a configured value above it never
// raises the walk past what the change needs, and the report says which of the
// two fired. The literal magnitudes are asserted as literals, not as
// expressions over the constants, so re-tuning a constant cannot leave this
// green by construction.
func TestCommittedBaseClosureCapIsSizedOffTheChangeSet(t *testing.T) {
	store := builderOpenStore(t, "closure-cap")

	changes := func(n int) []LayerPathChange {
		out := make([]LayerPathChange, 0, n)
		for i := range n {
			out = append(out, LayerPathChange{Path: fmt.Sprintf("f%d.go", i), Kind: LayerPathModified})
		}
		return out
	}
	committed := func(n int) BuildRequest {
		return BuildRequest{
			Identity: GenerationIdentity{GenerationKind: DedicatedBaseGenerationKind, BaseGenerationID: 7},
			Changes:  changes(n),
		}
	}
	for _, tc := range []struct {
		name       string
		knob       int
		req        BuildRequest
		want       int
		wantSource string
	}{
		// Every other sparse build reads the knob directly, as before.
		{"a root dedicated build keeps the configured cap", 17,
			BuildRequest{Identity: GenerationIdentity{GenerationKind: DedicatedBaseGenerationKind}, Changes: changes(50)},
			17, ClosureCapFromOperator},
		{"a commit layer keeps the configured cap", 17,
			BuildRequest{Identity: GenerationIdentity{GenerationKind: "commit", BaseGenerationID: 7}, Changes: changes(50)},
			17, ClosureCapFromOperator},
		{"an unset knob leaves a commit layer on the built-in default", 0,
			BuildRequest{Identity: GenerationIdentity{GenerationKind: "commit", BaseGenerationID: 7}, Changes: changes(50)},
			200, ClosureCapFromDefault},

		// A committed base with no knob set: the computed cap, clamped.
		{"a one-file commit gets the floor", 0, committed(1), 200, ClosureCapFromChangeSized},
		{"a fifty-file commit is sized off the change", 0, committed(50), 1600, ClosureCapFromChangeSized},
		{"a huge commit stops at the ceiling", 0, committed(100000), 4096, ClosureCapFromChangeSized},

		// A committed base with the knob set: the minimum of the two, and the
		// knob is never exceeded.
		{"a lower operator knob bounds the committed base", 50, committed(50), 50, ClosureCapFromOperator},
		{"an operator knob below the floor still bounds a one-file commit", 20, committed(1), 20, ClosureCapFromOperator},
		{"a knob equal to the change-sized cap is named as the operator bound", 1600, committed(50),
			1600, ClosureCapFromOperator},
		{"a higher operator knob never raises the committed-base cap", 100000, committed(50),
			1600, ClosureCapFromChangeSized},
		{"a knob above the ceiling never raises a huge commit past it", 100000, committed(100000),
			4096, ClosureCapFromChangeSized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := builderNewBuilder(store)
			builder.Config.AffectedByReresolveMax = tc.knob
			got, source := builder.builderClosureCap(tc.req)
			if got != tc.want || source != tc.wantSource {
				t.Fatalf("builderClosureCap = (%d, %q), want (%d, %q)", got, source, tc.want, tc.wantSource)
			}
			if tc.knob > 0 && got > tc.knob {
				t.Fatalf("cap %d exceeds the operator knob %d", got, tc.knob)
			}
		})
	}
}

// TestCommittedBaseClosureCapReachesTheBuild proves neither half of the rule is
// a dead helper: a real BuildClaimedDedicatedDelta runs at the change-sized cap
// when no knob is configured, and at the operator's value when one is — and the
// published completeness fact names which bound cut the closure.
func TestCommittedBaseClosureCapReachesTheBuild(t *testing.T) {
	const leaves = 40

	t.Run("an unset knob lets the change-sized cap reach the build", func(t *testing.T) {
		fixture := newDeltaWriteSetFixture(t, deltaWriteSetTree(leaves, false))
		fixture.builder.Config.AffectedByReresolveMax = 0
		_, report := fixture.advance(t, deltaWriteSetTree(leaves, true))
		if want := builderCommittedBaseClosureCap(10); report.ClosureCap != want {
			t.Fatalf("the delta build ran with cap %d, want the change-sized %d", report.ClosureCap, want)
		}
		if report.ClosureCapSource != ClosureCapFromChangeSized {
			t.Fatalf("cap source %q, want %q", report.ClosureCapSource, ClosureCapFromChangeSized)
		}
		if report.ClosureTruncated {
			t.Fatalf("a 50-file fixture truncated at cap %d", report.ClosureCap)
		}
	})

	t.Run("a configured knob bounds the build and rides on the completeness fact", func(t *testing.T) {
		const knob = 5
		fixture := newDeltaWriteSetFixture(t, deltaWriteSetTree(leaves, false))
		fixture.builder.Config.AffectedByReresolveMax = knob
		generationID, report := fixture.advance(t, deltaWriteSetTree(leaves, true))
		if report.ClosureCap != knob {
			t.Fatalf("the delta build ran with cap %d, want the operator's %d", report.ClosureCap, knob)
		}
		if report.ClosureCapSource != ClosureCapFromOperator {
			t.Fatalf("cap source %q, want %q", report.ClosureCapSource, ClosureCapFromOperator)
		}
		if !report.ClosureTruncated {
			t.Fatalf("a %d-leaf closure was not truncated at cap %d", leaves, knob)
		}
		if report.ClosureFiles > knob {
			t.Fatalf("the closure kept %d files past a cap of %d", report.ClosureFiles, knob)
		}
		// The knowingly incomplete generation says WHY it is incomplete, and
		// the reason names the bound an operator can move.
		states, err := fixture.builder.Store.AtGeneration(generationID).ProducerStates()
		if err != nil {
			t.Fatalf("ProducerStates: %v", err)
		}
		var reason string
		for _, row := range states {
			if row.Producer == string(graphview.CapResolutionLocal) {
				if row.State != store_sqlite.ProducerStateIncomplete {
					t.Fatalf("local resolution is %q after a truncated closure, want incomplete", row.State)
				}
				reason = row.Reason
			}
		}
		if !strings.Contains(reason, "index.affected_by_reresolve_max") {
			t.Fatalf("the truncation reason %q does not name the operator bound that cut it", reason)
		}
	})
}
