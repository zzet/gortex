package resolver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type builtinBatchCountingStore struct {
	graph.Store
	getNodesByIDs int
	addNode       int
	addBatch      int
}

func (s *builtinBatchCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesByIDs++
	return s.Store.GetNodesByIDs(ids)
}

func (s *builtinBatchCountingStore) AddNode(n *graph.Node) {
	s.addNode++
	s.Store.AddNode(n)
}

func (s *builtinBatchCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatch++
	s.Store.AddBatch(nodes, edges)
}

func TestAttributeGoBuiltins_FunctionCall(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::Run"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "Run", FilePath: "pkg/foo.go", Language: "go"})
	edge := &graph.Edge{From: owner, To: "unresolved::append", Kind: graph.EdgeArgOf, FilePath: "pkg/foo.go", Line: 5}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "builtin::go::append", edge.To,
		"call to `append` must retarget onto builtin::go::append")
	n := g.GetNode("builtin::go::append")
	require.NotNil(t, n, "KindBuiltin node must be materialised")
	assert.Equal(t, graph.KindBuiltin, n.Kind)
	assert.Equal(t, "append", n.Name)
	assert.Equal(t, "go", n.Language)
	assert.Equal(t, true, n.Meta["builtin"])
	assert.Equal(t, "func", n.Meta["builtin_kind"])
}

func TestAttributeGoBuiltins_Type(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::Handler"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "Handler", FilePath: "pkg/foo.go", Language: "go"})

	paramID := owner + "#param:s"
	g.AddNode(&graph.Node{ID: paramID, Kind: graph.KindParam, Name: "s", FilePath: "pkg/foo.go", Language: "go"})
	edge := &graph.Edge{From: paramID, To: "unresolved::string", Kind: graph.EdgeTypedAs, FilePath: "pkg/foo.go", Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "builtin::go::type::string", edge.To,
		"typed_as `string` must retarget onto builtin::go::type::string")
	n := g.GetNode("builtin::go::type::string")
	require.NotNil(t, n)
	assert.Equal(t, graph.KindBuiltin, n.Kind)
	assert.Equal(t, "type", n.Meta["builtin_kind"])
}

func TestAttributeGoBuiltinsUsesEnclosingOwnerRepo(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::Handler"
	g.AddNode(&graph.Node{
		ID: owner, Kind: graph.KindFunction, Name: "Handler", FilePath: "pkg/foo.go",
		Language: "go", RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project",
	})
	edge := &graph.Edge{From: owner + "#param:synthetic", To: "unresolved::len", Kind: graph.EdgeArgOf, Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "repo::builtin::go::len", edge.To)
	n := g.GetNode(edge.To)
	require.NotNil(t, n)
	assert.Equal(t, "repo", n.RepoPrefix, "per-repo builtin node must participate in purge/scoping")
	assert.Equal(t, "workspace", n.WorkspaceID, "builtin must inherit the source workspace boundary")
	assert.Equal(t, "project", n.ProjectID, "builtin must inherit the source project boundary")
}

func TestAttributeGoBuiltins_DedupedAcrossManyEdges(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::F"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "F", FilePath: "pkg/foo.go", Language: "go"})

	// Many calls to len from the same function.
	for i := 1; i <= 5; i++ {
		g.AddEdge(&graph.Edge{From: owner, To: "unresolved::len", Kind: graph.EdgeArgOf, FilePath: "pkg/foo.go", Line: i})
	}

	New(g).attributeGoBuiltins()

	// Exactly one KindBuiltin node should be created regardless of
	// how many edges referenced it.
	count := 0
	for n := range g.NodesByKind(graph.KindBuiltin) {
		if n.ID == "builtin::go::len" {
			count++
		}
	}
	assert.Equal(t, 1, count, "exactly one KindBuiltin per unique builtin")
}

func TestAttributeGoBuiltinsBatchesSourceReadsAndNodeWrites(t *testing.T) {
	base := graph.New()
	for i := 0; i < 100; i++ {
		owner := fmt.Sprintf("opaque::caller-%03d", i)
		base.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "caller", Language: "go", RepoPrefix: "repo"})
		base.AddEdge(&graph.Edge{From: owner, To: "unresolved::len", Kind: graph.EdgeCalls, Line: i + 1})
	}
	store := &builtinBatchCountingStore{Store: base}

	New(store).attributeGoBuiltins()

	assert.Equal(t, 2, store.getNodesByIDs, "builtin sources and the sentinels already served must each be loaded in one set query")
	assert.Equal(t, 0, store.addNode, "builtin materialisation must not write one node at a time")
	assert.Equal(t, 1, store.addBatch, "builtin nodes must be materialised in one batch")
	for i := 0; i < 100; i++ {
		owner := fmt.Sprintf("opaque::caller-%03d", i)
		assert.True(t, hasEdgeKind(base, owner, "repo::builtin::go::len", graph.EdgeCalls))
	}

	// A later pass whose sentinel the graph already serves writes no node.
	for i := 100; i < 110; i++ {
		owner := fmt.Sprintf("opaque::caller-%03d", i)
		base.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "caller", Language: "go", RepoPrefix: "repo"})
		base.AddEdge(&graph.Edge{From: owner, To: "unresolved::len", Kind: graph.EdgeCalls, Line: i + 1})
	}
	New(store).attributeGoBuiltins()
	assert.Equal(t, 1, store.addBatch, "a served builtin sentinel must not be written again")
	assert.True(t, hasEdgeKind(base, "opaque::caller-105", "repo::builtin::go::len", graph.EdgeCalls))
}

func TestAttributeGoBuiltins_NonGoLeftAlone(t *testing.T) {
	g := graph.New()
	// A Python source emitting a reference to `len` (Python builtin)
	// — must NOT get attributed to Go's `builtin::go::len`.
	owner := "pkg/app.py::process"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "process", FilePath: "pkg/app.py", Language: "python"})
	edge := &graph.Edge{From: owner, To: "unresolved::len", Kind: graph.EdgeArgOf, FilePath: "pkg/app.py", Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "unresolved::len", edge.To,
		"Python source must NOT cross-bind to Go's len builtin")
}

func TestAttributeGoBuiltins_UnknownNameLeftAlone(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::F"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "F", FilePath: "pkg/foo.go", Language: "go"})
	edge := &graph.Edge{From: owner, To: "unresolved::myCustomFunc", Kind: graph.EdgeArgOf, FilePath: "pkg/foo.go", Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "unresolved::myCustomFunc", edge.To,
		"non-builtin names must stay unresolved")
}

func TestAttributeGoBuiltins_QualifiedShapeLeftAlone(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::F"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "F", FilePath: "pkg/foo.go", Language: "go"})

	// `*.len` is qualified — leave to other passes.
	edge := &graph.Edge{From: owner, To: "unresolved::*.len", Kind: graph.EdgeArgOf, FilePath: "pkg/foo.go", Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "unresolved::*.len", edge.To, "qualified `*.len` shape must be left alone")
}

type recordingGoBuiltinStore struct {
	*graph.Graph
	compact []graph.UnresolvedEdgeTargetReindex
	full    []graph.EdgeReindex
}

func (s *recordingGoBuiltinStore) ReindexUnresolvedEdgeTargets(batch []graph.UnresolvedEdgeTargetReindex) {
	s.compact = append(s.compact, batch...)
}

func (s *recordingGoBuiltinStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.full = append(s.full, batch...)
}

func TestAttributeGoBuiltinCandidatesUsesCompactTargetReindex(t *testing.T) {
	base := graph.New()
	store := &recordingGoBuiltinStore{Graph: base}
	source := &graph.Node{
		ID:          "repo/pkg/file.go::callBuiltin",
		Kind:        graph.KindFunction,
		Language:    "go",
		RepoPrefix:  "repo",
		WorkspaceID: "workspace",
		ProjectID:   "project",
	}
	base.AddBatch([]*graph.Node{source}, nil)

	edge := &graph.Edge{
		From:     source.ID,
		To:       "unresolved::len",
		Kind:     graph.EdgeCalls,
		FilePath: "pkg/file.go",
		Line:     17,
	}
	resolver := &Resolver{graph: store}
	resolver.attributeGoBuiltinCandidates([]*graph.Edge{edge})

	target, _, _ := goBuiltinTarget("repo", "len")
	if edge.To != target {
		t.Fatalf("edge target = %q, want %q", edge.To, target)
	}
	if len(store.full) != 0 {
		t.Fatalf("full edge reindexes = %d, want 0", len(store.full))
	}
	if len(store.compact) != 1 {
		t.Fatalf("compact target reindexes = %d, want 1", len(store.compact))
	}
	wantOld := graph.EdgeIdentity{
		From: source.ID, To: "unresolved::len", Kind: graph.EdgeCalls,
		FilePath: edge.FilePath, Line: edge.Line,
	}
	if store.compact[0].Old != wantOld || store.compact[0].NewTo != target {
		t.Fatalf("compact mutation = %+v, want old=%+v new_to=%q", store.compact[0], wantOld, target)
	}
	if builtin := base.GetNode(target); builtin == nil {
		t.Fatalf("builtin node %q was not materialised before retargeting", target)
	} else if builtin.WorkspaceID != source.WorkspaceID || builtin.ProjectID != source.ProjectID {
		t.Fatalf("builtin boundary = (%q, %q), want (%q, %q)",
			builtin.WorkspaceID, builtin.ProjectID, source.WorkspaceID, source.ProjectID)
	}
}

func TestAttributeGoBuiltinCandidatesFallsBackWholeMixedBatchInOrder(t *testing.T) {
	base := graph.New()
	store := &recordingGoBuiltinStore{Graph: base}
	source := &graph.Node{
		ID: "repo/pkg/file.go::callBuiltin", Kind: graph.KindFunction,
		Language: "go", RepoPrefix: "repo",
	}
	base.AddBatch([]*graph.Node{source}, nil)

	unique := &graph.Edge{
		From: source.ID, To: "unresolved::cap", Kind: graph.EdgeCalls,
		FilePath: "pkg/file.go", Line: 2,
	}
	firstCollision := &graph.Edge{
		From: source.ID, To: "unresolved::len", Kind: graph.EdgeCalls,
		FilePath: "pkg/file.go", Line: 1,
	}
	secondCollision := &graph.Edge{
		From: source.ID, To: "unresolved::len", Kind: graph.EdgeCalls,
		FilePath: "pkg/file.go", Line: 1,
	}

	resolver := &Resolver{graph: store}
	resolver.attributeGoBuiltinCandidates([]*graph.Edge{unique, firstCollision, secondCollision})

	if len(store.compact) != 0 {
		t.Fatalf("compact target reindexes = %d, want 0 for mixed unsafe batch", len(store.compact))
	}
	if len(store.full) != 3 {
		t.Fatalf("full edge reindexes = %d, want 3", len(store.full))
	}
	if store.full[0].Edge != unique || store.full[1].Edge != firstCollision ||
		store.full[2].Edge != secondCollision {
		t.Fatalf("full fallback order changed: %+v", store.full)
	}
}

func TestCompactGoBuiltinTargetReindexesRejectsCollisionWithoutReordering(t *testing.T) {
	const target = "repo::builtin::go::len"
	first := &graph.Edge{From: "a.go::f", To: target, Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
	second := &graph.Edge{From: "a.go::f", To: target, Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
	batch := []graph.EdgeReindex{
		{Edge: first, OldTo: "unresolved::len"},
		{Edge: second, OldTo: "unresolved::cap"},
	}

	direct, ok := compactGoBuiltinTargetReindexes(batch)
	if ok || direct != nil {
		t.Fatalf("collision accepted: ok=%v direct=%v", ok, direct)
	}
	if batch[0].Edge != first || batch[0].OldTo != "unresolved::len" ||
		batch[1].Edge != second || batch[1].OldTo != "unresolved::cap" {
		t.Fatalf("fallback batch order changed: %+v", batch)
	}
}

func TestCompactGoBuiltinTargetReindexesPreservesDirectOrder(t *testing.T) {
	batch := []graph.EdgeReindex{
		{
			Edge:  &graph.Edge{From: "z.go::f", To: "repo::builtin::go::len", Kind: graph.EdgeCalls, FilePath: "z.go", Line: 2},
			OldTo: "unresolved::len",
		},
		{
			Edge:  &graph.Edge{From: "a.go::f", To: "repo::builtin::go::cap", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1},
			OldTo: "unresolved::cap",
		},
	}
	wantFirst := graph.EdgeIdentityFor(batch[0].Edge)
	wantFirst.To = batch[0].OldTo
	wantSecond := graph.EdgeIdentityFor(batch[1].Edge)
	wantSecond.To = batch[1].OldTo

	direct, ok := compactGoBuiltinTargetReindexes(batch)
	if !ok {
		t.Fatal("unique target-only batch unexpectedly rejected")
	}
	if len(direct) != 2 || direct[0].Old != wantFirst || direct[1].Old != wantSecond {
		t.Fatalf("compact mutation order = %+v, want old identities [%+v, %+v]", direct, wantFirst, wantSecond)
	}
}

func TestCompactGoBuiltinTargetReindexesRejectsNonTargetIdentityChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*graph.EdgeReindex)
	}{
		{name: "empty destination", mutate: func(m *graph.EdgeReindex) { m.Edge.To = "" }},
		{name: "old source", mutate: func(m *graph.EdgeReindex) { m.OldFrom = "old.go::f" }},
		{name: "old kind", mutate: func(m *graph.EdgeReindex) { m.OldKind = graph.EdgeReads }},
		{name: "old file", mutate: func(m *graph.EdgeReindex) { m.OldFilePath = "old.go" }},
		{name: "old line", mutate: func(m *graph.EdgeReindex) { m.OldLine = 7 }},
		{name: "refresh identity", mutate: func(m *graph.EdgeReindex) { m.RefreshIdentity = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutation := graph.EdgeReindex{
				Edge: &graph.Edge{
					From: "a.go::f", To: "repo::builtin::go::len", Kind: graph.EdgeCalls,
					FilePath: "a.go", Line: 1,
				},
				OldTo: "unresolved::len",
			}
			test.mutate(&mutation)
			if direct, ok := compactGoBuiltinTargetReindexes([]graph.EdgeReindex{mutation}); ok || direct != nil {
				t.Fatalf("unsafe identity change accepted: ok=%v direct=%v", ok, direct)
			}
		})
	}
}
