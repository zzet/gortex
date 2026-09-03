package indexer

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// The closure's reference-driven cases.
//
// Each one is a minimised reproducer of a way the closure can come up short of
// "what else must this generation carry": a reference the change INTRODUCES has
// no base edge to walk, an added file has no base nodes at all, a definition
// the change introduces has untouched files already parked on its placeholder,
// a module-local import names a package no base edge points at, a closure
// file's own references sit one hop further out than the seeds', and a clone
// pair is recorded in two files at once.
//
// They are stated the same way — build the layer, then require the composition
// to answer like a plain whole index of the tree it describes — with the
// closure's own path list asserted alongside, so a case that starts passing for
// an unrelated reason is still caught naming the file it was about.
//
// The two producer cases are the exception. They cover what the generation says
// about itself where no closure walk can close the gap: a corpus statistic
// computed over a file set, and a closure the cap cut short.

// closureCase is one built commit layer: the two trees, the report, and both
// halves of the differential.
type closureCase struct {
	store    *store_sqlite.Store
	report   BuildReport
	composed graph.Reader
	flat     graph.Reader
}

// buildClosureCase commits treeA, indexes it as the base corpus, commits treeB
// over it, and builds the commit layer spanning the two.
func buildClosureCase(t *testing.T, treeA, treeB map[string]string) closureCase {
	t.Helper()
	return buildClosureCaseWithEvidenceFailure(t, treeA, treeB, "")
}

// buildClosureCaseWithEvidenceFailure forces the target-side semantic probe for
// failEvidencePath to miss once. The generation pass reads the same source
// successfully afterwards, producing the legacy full-reverse closure for an
// apples-to-apples parity comparison with the optimized closure.
func buildClosureCaseWithEvidenceFailure(
	t *testing.T,
	treeA, treeB map[string]string,
	failEvidencePath string,
) closureCase {
	t.Helper()
	builderIsolateGit(t)
	dir := builderTempDir(t, "repo")
	builderGit(t, dir, "init", "--initial-branch=main")
	builderWriteTree(t, dir, treeA)
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "A")
	baseTree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dir)

	builderWriteTree(t, dir, treeB)
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "B")
	targetTree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")
	commitOID := builderGit(t, dir, "rev-parse", "HEAD")

	builder := builderNewBuilder(store)
	var (
		generationID int64
		report       BuildReport
		err          error
	)
	if failEvidencePath == "" {
		generationID, report, err = builder.BuildCommitLayer(context.Background(), CommitLayerRequest{
			Identity: GenerationIdentity{
				OwnerKind:           "dedicated_graph",
				GraphID:             "graph-closure",
				LayerID:             "layer-" + targetTree,
				CheckoutID:          "checkout-closure",
				ProvenanceCommitOID: commitOID,
			},
			Base:          store,
			RepoDir:       dir,
			BaseTreeOID:   baseTree,
			TargetTreeOID: targetTree,
			RootPath:      dir,
			RepoPrefix:    builderRepoPrefix,
			WorkspaceID:   builderRepoPrefix,
			ProjectID:     builderRepoPrefix,
		})
	} else {
		changes, diffErr := diffTreeChanges(context.Background(), dir, baseTree, targetTree)
		if diffErr != nil {
			t.Fatalf("diffTreeChanges: %v", diffErr)
		}
		target, sourceErr := source.NewGitTreeSource(context.Background(), dir, targetTree)
		if sourceErr != nil {
			t.Fatalf("NewGitTreeSource: %v", sourceErr)
		}
		defer target.Close() //nolint:errcheck // read-only test source
		generationID, report, err = builder.Build(context.Background(), BuildRequest{
			Identity: GenerationIdentity{
				OwnerKind:           "dedicated_graph",
				GraphID:             "graph-closure",
				LayerID:             "layer-" + targetTree,
				CheckoutID:          "checkout-closure",
				GenerationKind:      CommitLayerGenerationKind,
				TreeOID:             targetTree,
				ProvenanceCommitOID: commitOID,
			},
			Base:        store,
			Target:      &failFirstOpenSource{ContentSource: target, path: failEvidencePath},
			Changes:     changes,
			RootPath:    dir,
			RepoPrefix:  builderRepoPrefix,
			WorkspaceID: builderRepoPrefix,
			ProjectID:   builderRepoPrefix,
		})
	}
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	if report.ClosureTruncated {
		t.Fatalf("closure truncated at %d files — not what this case is about", report.ClosureCap)
	}

	flat := builderOpenStore(t, "flat")
	builderIndex(t, flat, dir)
	return closureCase{
		store:    store,
		report:   report,
		composed: builderComposed(t, store, generationID),
		flat:     flat,
	}
}

type failFirstOpenSource struct {
	source.ContentSource
	path   string
	failed bool
}

func (s *failFirstOpenSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	if path == s.path && !s.failed {
		s.failed = true
		return nil, source.FileMeta{}, errors.New("synthetic target-evidence read failure")
	}
	return s.ContentSource.Open(path)
}

// assertClosureCarries requires every named path to be in the closure — the
// files the walk had to discover, spelled repo-relative.
func assertClosureCarries(t *testing.T, report BuildReport, paths ...string) {
	t.Helper()
	for _, want := range paths {
		if !slices.Contains(report.ClosurePaths, want) {
			t.Errorf("the closure does not carry %q; it carries %v", want, report.ClosurePaths)
		}
	}
}

// closureFixtureTypes is the type file every case below is written against. A
// repository-local type keeps the resolver from minting a pathless stub, which
// is a composition gap of its own and not what these cases are about.
const closureFixtureTypes = `package fixture

type Options struct{}

type Result struct{}
`

// TestClosureCarriesIntroducedCallTarget pins the reference a change
// INTRODUCES. core.go gains a call to Helper, which it did not make before, so
// no base edge out of core.go names helper.go: a closure that walks only base
// edges re-derives core.go against a world its new call cannot bind in.
func TestClosureCarriesIntroducedCallTarget(t *testing.T) {
	treeA := map[string]string{
		"types.go": closureFixtureTypes,
		"core.go": `package fixture

func Compute(o Options) Result {
	return Result{}
}
`,
		"helper.go": `package fixture

func Helper() Result {
	return Result{}
}
`,
		"island.go": `package fixture

func Island() Result {
	return Result{}
}
`,
	}
	treeB := map[string]string{}
	for path, body := range treeA {
		treeB[path] = body
	}
	treeB["core.go"] = `package fixture

func Compute(o Options) Result {
	return Helper()
}
`

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report, "helper.go")
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestClosureResolvesAddedFileAgainstBase pins the added file. added.go has no
// base nodes at all, so a closure seeded from the base layer's edges is empty
// for it and the pass re-derives it against an empty world — its own parameter
// type and its call both park on stubs a whole index of the same tree binds.
func TestClosureResolvesAddedFileAgainstBase(t *testing.T) {
	treeA := map[string]string{
		"types.go": closureFixtureTypes,
		"helper.go": `package fixture

func Helper() Result {
	return Result{}
}
`,
		"island.go": `package fixture

func Island() Result {
	return Result{}
}
`,
	}
	treeB := map[string]string{}
	for path, body := range treeA {
		treeB[path] = body
	}
	treeB["added.go"] = `package fixture

func Added(o Options) Result {
	return Helper()
}
`

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report, "helper.go", "types.go")
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestClosureCarriesPlaceholderReferrers pins the reverse direction of an
// INTRODUCED definition. caller.go calls Helper in the base tree, where nothing
// defines it, so the base parks the call on an `unresolved::Helper`
// placeholder. The change adds helper.go — a file with no base nodes, so no
// base edge and no reference fact links it to caller.go, and a reverse frontier
// walked from the seeds' base nodes finds nothing. A whole index of the target
// tree binds the call; a generation that never re-derives caller.go leaves the
// stale placeholder edge showing through from below.
func TestClosureCarriesPlaceholderReferrers(t *testing.T) {
	treeA := map[string]string{
		"types.go": closureFixtureTypes,
		"caller.go": `package fixture

func Call(o Options) Result {
	return Helper()
}
`,
		"island.go": `package fixture

func Island() Result {
	return Result{}
}
`,
	}
	treeB := map[string]string{}
	for path, body := range treeA {
		treeB[path] = body
	}
	treeB["helper.go"] = `package fixture

func Helper() Result {
	return Result{}
}
`

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report, "caller.go")
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestClosureResolvesModuleLocalImport pins the module-local import. core.go
// gains an import of a package inside its own module; without the package's
// files and the module manifest in the generation, the pass cannot confirm the
// package exists, classifies the path as stdlib, and mints PERMANENT PATHLESS
// stubs for it — identities no file mask can reach.
//
// The assertion is therefore stated twice: the composition must agree with a
// whole index, AND the generation must carry no pathless identity at all. A
// tombstoned stub would satisfy the first on its own reads while still being
// the wrong payload.
func TestClosureResolvesModuleLocalImport(t *testing.T) {
	treeA := map[string]string{
		"go.mod":   "module fixture\n\ngo 1.22\n",
		"types.go": closureFixtureTypes,
		"sub/sub.go": `package sub

type Payload struct{}

func Assist() Payload {
	return Payload{}
}
`,
		"core.go": `package fixture

func Compute(o Options) Result {
	return Result{}
}
`,
		"island.go": `package fixture

func Island() Result {
	return Result{}
}
`,
	}
	treeB := map[string]string{}
	for path, body := range treeA {
		treeB[path] = body
	}
	treeB["core.go"] = `package fixture

import "fixture/sub"

func Compute(o Options) Result {
	sub.Assist()
	return Result{}
}
`

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report, "sub/sub.go", "go.mod")
	if c.report.UnmaskedPayloadNodes != 0 {
		t.Errorf("the generation carries %d pathless identities, want none; tombstones=%d",
			c.report.UnmaskedPayloadNodes, c.report.NodeTombstones)
	}
	for _, node := range c.store.AtGeneration(c.report.GenerationID).AllNodes() {
		if node != nil && node.FilePath == "" {
			t.Errorf("the generation carries a pathless identity %q (kind %s)", node.ID, node.Kind)
		}
	}
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestClosureIteratesToFixedPoint pins the hop bound. Only core.go changes;
// mid.go is one hop out and deep.go is two, so a one-hop closure re-derives
// mid.go against a world its own call into deep.go cannot bind in.
func TestClosureIteratesToFixedPoint(t *testing.T) {
	treeA := map[string]string{
		"types.go": closureFixtureTypes,
		"core.go": `package fixture

func Compute(o Options) Result {
	return Mid()
}
`,
		"mid.go": `package fixture

func Mid() Result {
	return Deep()
}
`,
		"deep.go": `package fixture

func Deep() Result {
	return Result{}
}
`,
		"island.go": `package fixture

func Island() Result {
	return Result{}
}
`,
	}
	treeB := map[string]string{}
	for path, body := range treeA {
		treeB[path] = body
	}
	treeB["core.go"] = `package fixture

func Compute(o Options, again Options) Result {
	return Mid()
}
`

	c := buildClosureCase(t, treeA, treeB)
	assertClosureCarries(t, c.report, "mid.go", "deep.go")
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// closureCloneBody is a function body long enough to carry a clone signature —
// near-duplicate detection ignores anything under a couple of dozen normalised
// tokens — written so two copies of it normalise to the same token stream.
const closureCloneBody = `	a := o
	b := a
	c := b
	d := c
	e := d
	f := e
	_ = f
	return Result{}
`

// TestClosureCarriesCloneCounterpart pins the similarity relation. mid.go is
// pulled into the closure because core.go calls it, and twin.go is its
// near-duplicate — a relation the base layer records as a symmetric PAIR of
// edges, one in each file. Claiming mid.go without twin.go replaces mid.go's
// half of the pair with nothing while twin.go's half keeps showing through
// from below, which is a composed graph where a symmetric relation points only
// one way.
func TestClosureCarriesCloneCounterpart(t *testing.T) {
	treeA := map[string]string{
		"types.go": closureFixtureTypes,
		"core.go": `package fixture

func Compute(o Options) Result {
	return Mid(o)
}
`,
		"mid.go":  "package fixture\n\nfunc Mid(o Options) Result {\n" + closureCloneBody + "}\n",
		"twin.go": "package fixture\n\nfunc Twin(o Options) Result {\n" + closureCloneBody + "}\n",
	}
	treeB := map[string]string{}
	for path, body := range treeA {
		treeB[path] = body
	}
	treeB["core.go"] = `package fixture

func Compute(o Options, again Options) Result {
	return Mid(o)
}
`

	c := buildClosureCase(t, treeA, treeB)
	// The pair has to exist in the base at all, or the case is testing nothing.
	if len(c.flat.GetOutEdges(builderRepoPrefix+"/mid.go::Mid")) == 0 {
		t.Fatal("the fixture's two bodies are not clones; the case cannot show anything")
	}
	assertClosureCarries(t, c.report, "mid.go", "twin.go")
	builderAssertReadersAgree(t, c.composed, c.flat)
	// The closure reaches a pair the base already records. It cannot reach a
	// pair the change CREATES between a claimed file and an untouched one, and
	// it cannot re-derive a corpus statistic from part of a corpus, so the build
	// says so rather than letting the reader assume the relation is whole.
	assertProducerState(t, c.report, string(graphview.CapSimilarity),
		store_sqlite.ProducerStateIncomplete, "corpus")
}

// TestSimilarityProducerFollowsTheClonePass pins the other state: with
// near-duplicate detection switched off, the generation writes no similarity at
// all, which is a different fact from writing a partial one and is reported as
// one.
func TestSimilarityProducerFollowsTheClonePass(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "repo")
	builderGit(t, dir, "init", "--initial-branch=main")
	tree := map[string]string{
		"types.go": closureFixtureTypes,
		"core.go": `package fixture

func Compute(o Options) Result {
	return Result{}
}
`,
	}
	builderWriteTree(t, dir, tree)
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "A")
	baseTree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dir)

	tree["core.go"] = `package fixture

func Compute(o Options, again Options) Result {
	return Result{}
}
`
	builderWriteTree(t, dir, tree)
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "B")
	targetTree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")

	builder := builderNewBuilder(store)
	off := false
	builder.Config.Coverage.Clones.Enabled = &off
	_, report, err := builder.BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:  "dedicated_graph",
			GraphID:    "graph-closure",
			LayerID:    "layer-" + targetTree,
			CheckoutID: "checkout-closure",
		},
		Base:          store,
		RepoDir:       dir,
		BaseTreeOID:   baseTree,
		TargetTreeOID: targetTree,
		RootPath:      dir,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	assertProducerState(t, report, string(graphview.CapSimilarity),
		store_sqlite.ProducerStateDisabledByConfig, "switched off")
}

// assertProducerState requires the build to have declared producer in state,
// for a reason naming want.
func assertProducerState(
	t *testing.T,
	report BuildReport,
	producer string,
	state store_sqlite.ProducerState,
	reason string,
) {
	t.Helper()
	for _, row := range report.Producers {
		if row.Producer != producer {
			continue
		}
		if row.State != state {
			t.Errorf("%s is %q, want %q", producer, row.State, state)
		}
		if !strings.Contains(row.Reason, reason) {
			t.Errorf("%s reason %q does not say %q", producer, row.Reason, reason)
		}
		return
	}
	t.Errorf("the build declared nothing for %q: %v", producer, report.Producers)
}

// TestClosureTruncationNarrowsProducers pins what a cut closure says about
// itself. The cap is set below the number of files the walk would take, so the
// generation is knowingly missing files it should have re-derived: the build
// publishes anyway and narrows the two producers whose completeness the cut
// actually costs — local resolution, and the incoming-edge index that a
// dependent past the cap would have contributed to.
type closurePlanningBase struct {
	LayerBase
	fileNodeCalls  int
	inEdgeCalls    int
	outEdgeCalls   int
	nodeCalls      int
	afterFileNodes func()
	afterInEdges   func()
	afterOutEdges  func()
	afterNodes     func()
}

func (b *closurePlanningBase) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	b.fileNodeCalls++
	nodes := b.LayerBase.GetFileNodesByPaths(paths)
	if b.afterFileNodes != nil {
		b.afterFileNodes()
	}
	return nodes
}

func (b *closurePlanningBase) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	b.inEdgeCalls++
	edges := b.LayerBase.GetInEdgesByNodeIDs(ids)
	if b.afterInEdges != nil {
		b.afterInEdges()
	}
	return edges
}

func (b *closurePlanningBase) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	b.outEdgeCalls++
	edges := b.LayerBase.GetOutEdgesByNodeIDs(ids)
	if b.afterOutEdges != nil {
		b.afterOutEdges()
	}
	return edges
}

func (b *closurePlanningBase) GetNodesByIDs(ids []string) map[string]*graph.Node {
	b.nodeCalls++
	nodes := b.LayerBase.GetNodesByIDs(ids)
	if b.afterNodes != nil {
		b.afterNodes()
	}
	return nodes
}

func (b *closurePlanningBase) calls() int {
	return b.fileNodeCalls + b.inEdgeCalls + b.outEdgeCalls + b.nodeCalls
}

type closurePlanningSource map[string]source.FileMeta

func (s closurePlanningSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	meta, ok := s[path]
	if !ok {
		return nil, source.FileMeta{}, source.ErrNotInSource
	}
	return io.NopCloser(strings.NewReader("package fixture\n")), meta, nil
}

func (s closurePlanningSource) Stat(path string) (source.FileMeta, error) {
	meta, ok := s[path]
	if !ok {
		return source.FileMeta{}, source.ErrNotInSource
	}
	return meta, nil
}

func (s closurePlanningSource) Walk(ctx context.Context, yield func(source.FileMeta) error) error {
	for _, meta := range s {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(meta); err != nil {
			return err
		}
	}
	return nil
}

func (closurePlanningSource) Identity() string { return "closure-planning" }
func (closurePlanningSource) Close() error     { return nil }

func benchmarkClosurePlanningFixture(fileCount int) (*SparseGenerationBuilder, *closurePlanningBase, BuildRequest) {
	const repo = "bench"
	baseGraph := graph.New()
	seedPath := repo + "/seed.go"
	seedID := seedPath + "::Seed"
	baseGraph.AddNode(&graph.Node{
		ID: seedID, Name: "Seed", Kind: graph.KindFunction,
		FilePath: seedPath, RepoPrefix: repo,
	})
	target := make(closurePlanningSource, fileCount)
	for i := 0; i < fileCount; i++ {
		rel := "dep_" + strconv.Itoa(i) + ".go"
		graphPath := repo + "/" + rel
		id := graphPath + "::Dependency"
		baseGraph.AddNode(&graph.Node{
			ID: id, Name: "Dependency", Kind: graph.KindFunction,
			FilePath: graphPath, RepoPrefix: repo,
		})
		baseGraph.AddEdge(&graph.Edge{
			From: seedID, To: id, Kind: graph.EdgeCalls, FilePath: seedPath,
		})
		target[rel] = source.FileMeta{Path: rel, Size: 16}
	}
	base := &closurePlanningBase{LayerBase: baseGraph}
	builder := &SparseGenerationBuilder{Logger: zap.NewNop()}
	builder.Config.AffectedByReresolveMax = fileCount + 1
	return builder, base, BuildRequest{
		Base: base, Target: target, RepoPrefix: repo,
	}
}

func TestAffectedClosureStopsAfterCanceledBaseRead(t *testing.T) {
	builder, base, req := benchmarkClosurePlanningFixture(100)
	deleted := map[string]struct{}{"seed.go": {}}
	ctx, cancel := context.WithCancel(context.Background())
	base.afterFileNodes = cancel

	var report BuildReport
	closure, err := builder.affectedClosureContext(ctx, req, nil, deleted, &report)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("affectedClosureContext error = %v, want context.Canceled", err)
	}
	if closure != nil {
		t.Fatalf("closure = %v, want nil", closure)
	}
	if base.fileNodeCalls != 1 {
		t.Fatalf("file-node calls = %d, want 1", base.fileNodeCalls)
	}
	if base.inEdgeCalls != 0 || base.outEdgeCalls != 0 || base.nodeCalls != 0 {
		t.Fatalf("graph calls after cancellation: in=%d out=%d nodes=%d",
			base.inEdgeCalls, base.outEdgeCalls, base.nodeCalls)
	}
}

func TestSemanticShapeAdjacencyStopsAfterEachCanceledBatch(t *testing.T) {
	tests := []struct {
		name string
		hook func(*closurePlanningBase, context.CancelFunc)
		want [4]int
	}{
		{
			name: "in edges",
			hook: func(base *closurePlanningBase, cancel context.CancelFunc) {
				base.afterInEdges = cancel
			},
			want: [4]int{1, 1, 0, 0},
		},
		{
			name: "out edges",
			hook: func(base *closurePlanningBase, cancel context.CancelFunc) {
				base.afterOutEdges = cancel
			},
			want: [4]int{1, 1, 1, 0},
		},
		{
			name: "missing nodes",
			hook: func(base *closurePlanningBase, cancel context.CancelFunc) {
				base.afterNodes = cancel
			},
			want: [4]int{1, 1, 1, 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, base, req := benchmarkSemanticReverseFanoutFixture(1)
			baseGraph := base.LayerBase.(*graph.Graph)
			seedNodes := baseGraph.FindNodesByName("Seed")
			if len(seedNodes) != 1 {
				t.Fatalf("Seed nodes = %d, want 1", len(seedNodes))
			}
			const pathlessParamID = "builtin::int"
			baseGraph.AddNode(&graph.Node{
				ID: pathlessParamID, Name: "int", Kind: graph.KindParam,
				Meta: map[string]any{"position": 0, "type": "int"},
			})
			baseGraph.AddEdge(&graph.Edge{
				From: pathlessParamID, To: seedNodes[0].ID, Kind: graph.EdgeParamOf,
			})

			ctx, cancel := context.WithCancel(context.Background())
			tt.hook(base, cancel)
			_, err := builderSemanticSeedNodeIDs(
				ctx,
				req,
				[]string{builderGraphPath(req.RepoPrefix, "seed.go")},
				nil,
				newBuilderSemanticTarget(map[string]struct{}{"seed.go": {}}),
			)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("builderSemanticSeedNodeIDs error = %v, want context.Canceled", err)
			}
			got := [4]int{base.fileNodeCalls, base.inEdgeCalls, base.outEdgeCalls, base.nodeCalls}
			if got != tt.want {
				t.Fatalf("base reads = %v, want %v; a read continued after cancellation", got, tt.want)
			}
		})
	}
}

func BenchmarkSparsePlanningCanceledClosure(b *testing.B) {
	const fileCount = 1000
	builder, base, req := benchmarkClosurePlanningFixture(fileCount)
	deleted := map[string]struct{}{"seed.go": {}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var report BuildReport
		closure, err := builder.affectedClosureContext(ctx, req, nil, deleted, &report)
		if !errors.Is(err, context.Canceled) {
			b.Fatalf("affectedClosureContext error = %v, want context.Canceled", err)
		}
		if closure != nil {
			b.Fatalf("closure = %v, want nil", closure)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(base.calls())/float64(b.N), "base_calls/op")
}

type semanticPlanningSource struct {
	files  map[string]source.FileMeta
	bodies map[string]string
}

func (s semanticPlanningSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	meta, ok := s.files[path]
	if !ok {
		return nil, source.FileMeta{}, source.ErrNotInSource
	}
	return io.NopCloser(strings.NewReader(s.bodies[path])), meta, nil
}

func (s semanticPlanningSource) Stat(path string) (source.FileMeta, error) {
	meta, ok := s.files[path]
	if !ok {
		return source.FileMeta{}, source.ErrNotInSource
	}
	return meta, nil
}

func (s semanticPlanningSource) Walk(ctx context.Context, yield func(source.FileMeta) error) error {
	for _, meta := range s.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(meta); err != nil {
			return err
		}
	}
	return nil
}

func (semanticPlanningSource) Identity() string { return "semantic-planning" }
func (semanticPlanningSource) Close() error     { return nil }

func benchmarkSemanticReverseFanoutFixture(fileCount int) (*SparseGenerationBuilder, *closurePlanningBase, BuildRequest) {
	const (
		repo     = "bench"
		seedRel  = "seed.go"
		seedPath = repo + "/" + seedRel
		body     = "package fixture\n\nfunc Seed() {}\n"
	)
	extractor := languages.NewGoExtractor()
	registry := parser.NewRegistry()
	registry.Register(extractor)
	result, err := safeExtractWithOptions(extractor, seedPath, []byte(body), parser.ExtractionOptions{})
	if err != nil {
		panic(err)
	}
	baseGraph := graph.New()
	seedID := ""
	for _, node := range result.Nodes {
		baseGraph.AddNode(node)
		if node != nil && node.Name == "Seed" && node.Kind == graph.KindFunction {
			seedID = node.ID
		}
	}
	for _, edge := range result.Edges {
		baseGraph.AddEdge(edge)
	}
	if result.Tree != nil {
		result.Tree.Close()
	}
	if seedID == "" {
		panic("Go extraction did not produce Seed")
	}
	files := map[string]source.FileMeta{
		seedRel: {Path: seedRel, Size: int64(len(body))},
	}
	for i := 0; i < fileCount; i++ {
		rel := "dependent_" + strconv.Itoa(i) + ".go"
		graphPath := repo + "/" + rel
		id := graphPath + "::Dependent"
		files[rel] = source.FileMeta{Path: rel, Size: 16}
		baseGraph.AddNode(&graph.Node{
			ID: id, Name: "Dependent", Kind: graph.KindFunction,
			FilePath: graphPath, RepoPrefix: repo,
		})
		baseGraph.AddEdge(&graph.Edge{
			From: id, To: seedID, Kind: graph.EdgeCalls, FilePath: graphPath,
		})
	}
	base := &closurePlanningBase{LayerBase: baseGraph}
	builder := &SparseGenerationBuilder{Registry: registry, Logger: zap.NewNop()}
	builder.Config.AffectedByReresolveMax = fileCount + 1
	return builder, base, BuildRequest{
		Base: base,
		Target: semanticPlanningSource{
			files:  files,
			bodies: map[string]string{seedRel: body},
		},
		RepoPrefix: repo,
	}
}

func BenchmarkSparsePlanningUnchangedReverseFanout(b *testing.B) {
	const fileCount = 900
	builder, base, req := benchmarkSemanticReverseFanoutFixture(fileCount)
	present := map[string]struct{}{"seed.go": {}}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var report BuildReport
		closure, err := builder.affectedClosureContext(context.Background(), req, present, nil, &report)
		if err != nil {
			b.Fatal(err)
		}
		if len(closure) != 0 {
			b.Fatalf("closure files = %d, want 0", len(closure))
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(base.calls())/float64(b.N), "base_calls/op")
}

func TestSemanticReverseFanoutSkipsBodyOnlyChange(t *testing.T) {
	builder, _, req := benchmarkSemanticReverseFanoutFixture(20)
	var report BuildReport
	closure, err := builder.affectedClosureContext(
		context.Background(), req, map[string]struct{}{"seed.go": {}}, nil, &report)
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) != 0 {
		t.Fatalf("body-only closure = %v, want no reverse dependents", closure)
	}
}

func TestSemanticReverseFanoutKeepsShapeChange(t *testing.T) {
	const dependentCount = 20
	builder, _, req := benchmarkSemanticReverseFanoutFixture(dependentCount)
	target := req.Target.(semanticPlanningSource)
	const changed = "package fixture\n\nfunc Seed(value int) {}\n"
	target.bodies["seed.go"] = changed
	target.files["seed.go"] = source.FileMeta{Path: "seed.go", Size: int64(len(changed))}
	req.Target = target

	var report BuildReport
	closure, err := builder.affectedClosureContext(
		context.Background(), req, map[string]struct{}{"seed.go": {}}, nil, &report)
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) != dependentCount {
		t.Fatalf("shape-change closure files = %d, want %d: %v", len(closure), dependentCount, closure)
	}
}

func TestSemanticReverseFanoutFallsBackForDeletedFile(t *testing.T) {
	const dependentCount = 20
	builder, _, req := benchmarkSemanticReverseFanoutFixture(dependentCount)

	var report BuildReport
	closure, err := builder.affectedClosureContext(
		context.Background(), req, nil, map[string]struct{}{"seed.go": {}}, &report)
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) != dependentCount {
		t.Fatalf("deleted-file fallback closure files = %d, want %d: %v", len(closure), dependentCount, closure)
	}
}

func TestSemanticReverseFanoutFallsBackForIncompleteTargetExtraction(t *testing.T) {
	const dependentCount = 20
	builder, _, req := benchmarkSemanticReverseFanoutFixture(dependentCount)
	builder.Registry = parser.NewRegistry()

	var report BuildReport
	closure, err := builder.affectedClosureContext(
		context.Background(), req, map[string]struct{}{"seed.go": {}}, nil, &report)
	if err != nil {
		t.Fatal(err)
	}
	if len(closure) != dependentCount {
		t.Fatalf("incomplete-target fallback closure files = %d, want %d: %v", len(closure), dependentCount, closure)
	}
}

func TestSemanticReverseFanoutNamedPathlessBuiltinMatchesLegacy(t *testing.T) {
	treeA := map[string]string{
		"seed.go":   "package fixture\n\nfunc Seed(value int) int { return value }\n",
		"caller.go": "package fixture\n\nfunc Caller() int { return Seed(1) }\n",
	}
	treeB := make(map[string]string, len(treeA))
	for path, body := range treeA {
		treeB[path] = body
	}
	treeB["seed.go"] = "package fixture\n\nfunc Seed(value int) int {\n\tnext := value\n\treturn next\n}\n"

	optimized := buildClosureCase(t, treeA, treeB)
	legacy := buildClosureCaseWithEvidenceFailure(t, treeA, treeB, "seed.go")
	if slices.Contains(optimized.report.ClosurePaths, "caller.go") {
		t.Fatalf("optimized body-only closure unexpectedly carries caller.go: %v", optimized.report.ClosurePaths)
	}
	assertClosureCarries(t, legacy.report, "caller.go")

	pathlessInt := false
	for _, node := range optimized.store.FindNodesByName("int") {
		if node != nil && node.Name == "int" && node.FilePath == "" {
			pathlessInt = true
			break
		}
	}
	if !pathlessInt {
		t.Fatal("fixture has no named pathless int node; it does not cover resolution-insensitive shape parity")
	}
	// Compare the two generation strategies directly. The generic flat helper
	// intentionally leaves workspace/project identity off pathless builtins,
	// while sparse generations stamp the request identity; that fixture-only
	// metadata difference is unrelated to reverse-fanout parity.
	builderAssertReadersAgree(t, optimized.composed, legacy.composed)
}

func TestSemanticReverseFanoutBodyOnlyParity(t *testing.T) {
	treeA := map[string]string{
		"seed.go":   "package fixture\n\nfunc Seed() {}\n",
		"caller.go": "package fixture\n\nfunc Caller() { Seed() }\n",
		"other.go":  "package fixture\n\nfunc Other() { Seed() }\n",
	}
	treeB := make(map[string]string, len(treeA))
	for path, body := range treeA {
		treeB[path] = body
	}
	treeB["seed.go"] = "package fixture\n\nfunc Seed() {\n\t// body-only edit\n}\n"

	c := buildClosureCase(t, treeA, treeB)
	for _, path := range []string{"caller.go", "other.go"} {
		if slices.Contains(c.report.ClosurePaths, path) {
			t.Errorf("body-only closure unexpectedly carries %q: %v", path, c.report.ClosurePaths)
		}
	}
	builderAssertReadersAgree(t, c.composed, c.flat)
}

func TestClosureTruncationNarrowsProducers(t *testing.T) {
	builderIsolateGit(t)
	dir := builderTempDir(t, "repo")
	builderGit(t, dir, "init", "--initial-branch=main")
	treeA := map[string]string{
		"types.go": closureFixtureTypes,
		"core.go": `package fixture

func Compute(o Options) Result {
	return Mid()
}
`,
		"mid.go": `package fixture

func Mid() Result {
	return Deep()
}
`,
		"deep.go": `package fixture

func Deep() Result {
	return Result{}
}
`,
	}
	builderWriteTree(t, dir, treeA)
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "A")
	baseTree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")

	store := builderOpenStore(t, "base")
	builderIndex(t, store, dir)

	treeA["core.go"] = `package fixture

func Compute(o Options, again Options) Result {
	return Mid()
}
`
	builderWriteTree(t, dir, treeA)
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-m", "B")
	targetTree := builderGit(t, dir, "rev-parse", "HEAD^{tree}")

	builder := builderNewBuilder(store)
	builder.Config.AffectedByReresolveMax = 1
	_, report, err := builder.BuildCommitLayer(context.Background(), CommitLayerRequest{
		Identity: GenerationIdentity{
			OwnerKind:  "dedicated_graph",
			GraphID:    "graph-closure",
			LayerID:    "layer-" + targetTree,
			CheckoutID: "checkout-closure",
		},
		Base:          store,
		RepoDir:       dir,
		BaseTreeOID:   baseTree,
		TargetTreeOID: targetTree,
		RootPath:      dir,
		RepoPrefix:    builderRepoPrefix,
		WorkspaceID:   builderRepoPrefix,
		ProjectID:     builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("BuildCommitLayer: %v", err)
	}
	if !report.ClosureTruncated {
		t.Fatalf("the closure was not truncated at a cap of 1: closure=%v", report.ClosurePaths)
	}
	if report.ClosureCap != 1 {
		t.Errorf("ClosureCap = %d, want 1", report.ClosureCap)
	}
	if len(report.ClosurePaths) != 1 {
		t.Errorf("the closure carries %v, want one path", report.ClosurePaths)
	}
	for _, producer := range []string{
		string(graphview.CapResolutionLocal),
		string(graphview.CapIncomingEdges),
	} {
		// The reason has to name the cap the walk hit, or a reader is told the
		// generation is thin without being told how thin.
		assertProducerState(t, report, producer, store_sqlite.ProducerStateIncomplete, "1")
	}
}
