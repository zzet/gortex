package graphview_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

type nodeMaskFixture struct {
	control, lower, upper *store_sqlite.Store
	raw                   *sql.DB
	upperID               int64
	id, sibling           string
}

func newNodeMaskFixture(t testing.TB) nodeMaskFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "explicit-mask.sqlite")
	s, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	newGeneration := func(base int64) int64 {
		id, err := s.Catalog().CreateViewGeneration(context.Background(), store_sqlite.ViewGeneration{
			OwnerKind: "dedicated_graph", GraphID: "explicit-mask-fixture", GenerationKind: "dedicated", BaseGenerationID: base,
			TreeOID: "same-bytes", ConfigHash: "same-policy", State: store_sqlite.ViewGenerationBuilding, CreatedAt: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	lowerID := newGeneration(0)
	lower := s.AtGeneration(lowerID)
	id, sibling := "alpha/a.cs::Run", "alpha/a.cs::Sibling"
	lower.AddBatch([]*graph.Node{
		{ID: id, Name: "Run", QualName: "Child.Run", RepoPrefix: "alpha", FilePath: "alpha/a.cs", Kind: graph.NodeKind("function"), Language: "csharp", Meta: map[string]interface{}{"revision": "old"}},
		{ID: sibling, Name: "Sibling", QualName: "Sibling", RepoPrefix: "alpha", FilePath: "alpha/a.cs", Kind: graph.KindType, Language: "csharp"},
	}, []*graph.Edge{
		{From: id, To: sibling, Kind: graph.EdgeKind("calls"), FilePath: "alpha/a.cs", Line: 1},
		{From: sibling, To: id, Kind: graph.EdgeKind("calls"), FilePath: "alpha/a.cs", Line: 2},
	})
	if err := s.Catalog().PublishViewGeneration(context.Background(), lowerID, 2); err != nil {
		t.Fatal(err)
	}
	upperID := newGeneration(lowerID)
	s.AddNode(&graph.Node{ID: id, Name: "gen0 poison", RepoPrefix: "alpha", FilePath: "alpha/a.cs"})
	return nodeMaskFixture{control: s, lower: lower, upper: s.AtGeneration(upperID), raw: raw, upperID: upperID, id: id, sibling: sibling}
}

// Same literal test can be compiled in a reader-only RED probe on the old
// source: this fixture adds the new storage discriminator explicitly and
// inserts a format row through SQL, without depending on a new writer API.
// Old readers misread it as a legacy tombstone and discard lower adjacency.
func setExplicitMaskFixtureRow(t testing.TB, f nodeMaskFixture, id, kind string) {
	t.Helper()
	var count int
	if err := f.raw.QueryRow(`SELECT COUNT(*) FROM pragma_table_xinfo('generation_node_tombstones') WHERE name='claim_kind'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		if _, err := f.raw.Exec(`ALTER TABLE generation_node_tombstones ADD COLUMN claim_kind TEXT NOT NULL DEFAULT 'legacy_tombstone'`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.raw.Exec(`INSERT INTO generation_node_tombstones(view_gen,node_id,claim_kind) VALUES(?,?,?)`, f.upperID, id, kind); err != nil {
		t.Fatal(err)
	}
}

func explicitMaskView(t testing.TB, f nodeMaskFixture) (*graph.OverlaidView, *graphview.GenerationLayer) {
	t.Helper()
	layer, err := graphview.NewGenerationLayer(f.upper)
	if err != nil {
		t.Fatal(err)
	}
	return graph.NewOverlaidViewWithLayer(f.lower, layer), layer
}

func TestGenerationLayerExplicitIdentityMaskKeepsNodeAndAdjacencyReadersConsistent(t *testing.T) {
	f := newNodeMaskFixture(t)
	node := f.lower.GetNode(f.id)
	node.Meta["revision"] = "new"
	f.upper.AddBatch([]*graph.Node{node}, nil)
	setExplicitMaskFixtureRow(t, f, f.id, "identity_replace")
	view, layer := explicitMaskView(t, f)
	assert := func(surface string, nodes []*graph.Node) {
		t.Helper()
		n := 0
		for _, node := range nodes {
			if node != nil && node.ID == f.id {
				n++
				if node.Meta["revision"] != "new" {
					t.Errorf("%s stale row", surface)
				}
			}
		}
		if n != 1 {
			t.Errorf("%s has %d copies of replacement", surface, n)
		}
	}
	assert("point", []*graph.Node{view.GetNode(f.id)})
	assert("batch", []*graph.Node{view.GetNodesByIDs([]string{f.id})[f.id]})
	assert("bulk", view.AllNodes())
	assert("file", view.GetFileNodes("alpha/a.cs"))
	assert("repo", view.GetRepoNodes("alpha"))
	assert("name", view.FindNodesByName("Run"))
	assert("qual", []*graph.Node{view.GetNodeByQualName("Child.Run")})
	assert("qual batch", view.GetNodesByQualNames([]string{"Child.Run"})["Child.Run"])
	var kinds []*graph.Node
	for n := range view.NodesByKind(graph.NodeKind("function")) {
		kinds = append(kinds, n)
	}
	assert("kind", kinds)
	if !layer.OwnsNodeIdentity(f.id) || layer.OwnsOutEdges(f.id) || layer.IsRemovedID(f.id) || layer.HasFile("alpha/a.cs") {
		t.Error("identity-only gained deletion/file/edge ownership")
	}
	if view.GetNode(f.sibling) == nil || len(view.GetFileNodes("alpha/a.cs")) != 2 {
		t.Error("unchanged sibling hidden")
	}
	if len(view.GetOutEdges(f.id)) != 1 || len(view.GetInEdges(f.id)) != 1 || len(view.AllEdges()) != 2 {
		t.Error("identity-only suppressed lower adjacency")
	}
	if len(view.GetOutEdgesByNodeIDs([]string{f.id})[f.id]) != 1 || len(view.GetInEdgesByNodeIDs([]string{f.id})[f.id]) != 1 {
		t.Error("batch adjacency disagrees")
	}
	if view.NodeCount() != 2 || view.Stats().TotalNodes != 2 || view.RepoStats()["alpha"].TotalNodes != 2 || view.EdgeCount() != 2 {
		t.Error("node/edge counts disagree")
	}
	if f.upper.NodeCount() != 1 || f.upper.EdgeCount() != 0 {
		t.Error("whole-file/adjacency payload copied")
	}
	if f.lower.GetNode(f.id).Meta["revision"] != "old" || f.control.GetNode(f.id).Name != "gen0 poison" {
		t.Error("ready lower/gen0 mutated")
	}
}

func TestGenerationLayerLegacyCarriedMaskStillOwnsOutgoing(t *testing.T) {
	for _, id := range []string{"alpha/a.cs::Run", "alpha::builtin::String"} {
		t.Run(id, func(t *testing.T) {
			f := newNodeMaskFixture(t)
			node := &graph.Node{ID: id, Name: "Carried", RepoPrefix: "alpha", Kind: graph.KindType}
			if id == f.id {
				node = f.lower.GetNode(id)
			}
			f.upper.AddBatch([]*graph.Node{node}, nil)
			setExplicitMaskFixtureRow(t, f, id, "legacy_tombstone")
			view, layer := explicitMaskView(t, f)
			if !layer.IsRemovedID(id) || !layer.OwnsNodeIdentity(id) || !layer.OwnsOutEdges(id) || view.GetNode(id) == nil {
				t.Error("legacy row+tombstone weakened")
			}
			matches := 0
			for _, n := range view.GetRepoNodes("alpha") {
				if n.ID == id {
					matches++
				}
			}
			if matches != 1 {
				t.Errorf("carried legacy row omitted/duplicated in repo: %d", matches)
			}
			if id == f.id && len(view.GetOutEdges(id)) != 0 {
				t.Error("legacy adjacency revived")
			}
		})
	}
}

func TestGenerationLayerIdentityAndExplicitOutgoingClaimsCompose(t *testing.T) {
	f := newNodeMaskFixture(t)
	f.upper.AddBatch([]*graph.Node{f.lower.GetNode(f.id)}, nil)
	setExplicitMaskFixtureRow(t, f, f.id, "identity_replace")
	if err := f.upper.SetEdgeSourceMasks([]store_sqlite.EdgeSourceMask{{SourceID: f.id, Mode: store_sqlite.OwnershipReplace}}); err != nil {
		t.Fatal(err)
	}
	view, layer := explicitMaskView(t, f)
	if !layer.OwnsOutEdges(f.id) || len(view.GetOutEdges(f.id)) != 0 || len(view.GetInEdges(f.id)) != 1 {
		t.Error("independent explicit adjacency replacement failed")
	}
	if len(view.GetFileNodes("alpha/a.cs")) != 2 || view.NodeCount() != 2 {
		t.Error("source claim hid node/sibling")
	}
}

func TestGenerationLayerIdentityMissingSameGenerationRowIsNotLowerFallback(t *testing.T) {
	f := newNodeMaskFixture(t)
	setExplicitMaskFixtureRow(t, f, f.id, "identity_replace")
	if layer, err := graphview.NewGenerationLayer(f.upper); err == nil || layer != nil {
		t.Fatalf("invalid marker accepted: %v %v", layer, err)
	}
}

func TestGenerationLayerIdentityKindUpgradeNeverRevivesOutgoing(t *testing.T) {
	f := newNodeMaskFixture(t)
	f.upper.AddBatch([]*graph.Node{f.lower.GetNode(f.id)}, nil)
	setExplicitMaskFixtureRow(t, f, f.id, "identity_replace")
	if err := f.upper.SetNodeTombstones([]string{f.id}); err != nil {
		t.Fatal(err)
	}
	// Exact SQL simulates a later identity-only request with the same monotone
	// merge as the public writer, without adding a writer-API RED dependency.
	if _, err := f.raw.Exec(`INSERT INTO generation_node_tombstones(view_gen,node_id,claim_kind) VALUES(?,?,'identity_replace')
ON CONFLICT(view_gen,node_id) DO UPDATE SET claim_kind=CASE WHEN generation_node_tombstones.claim_kind='legacy_tombstone' THEN 'legacy_tombstone' ELSE 'identity_replace' END`, f.upperID, f.id); err != nil {
		t.Fatal(err)
	}
	view, layer := explicitMaskView(t, f)
	if !layer.OwnsOutEdges(f.id) || view.GetNode(f.id) == nil || len(view.GetOutEdges(f.id)) != 0 {
		t.Error("upgrade weakened old source ownership")
	}
	ids := []string{}
	for _, n := range view.GetFileNodes("alpha/a.cs") {
		ids = append(ids, n.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("upgrade dropped node from file: %v", ids)
	}
	if !reflect.DeepEqual(f.lower.GetNode(f.id).Meta, map[string]interface{}{"revision": "old"}) {
		t.Error("lower mutated")
	}
}

// Constructor costs must track explicit mask count, not upper payload count.
// The 4K and 32K ordinary upper cases both have exactly 32 file masks.
// Nothing is timed here until root runs the serial Go benchmark lane.
func BenchmarkGenerationLayerExplicitIdentityMaskPreparation(b *testing.B) {
	for _, tc := range []struct{ masked, identities int }{
		{0, 0}, {4096, 0}, {32768, 0}, {0, 1}, {4096, 1}, {32768, 1}, {0, 1024},
	} {
		b.Run(fmt.Sprintf("upper_%d_identity_%d_files_%d", tc.masked, tc.identities, map[bool]int{true: 32, false: 0}[tc.masked > 0]), func(b *testing.B) {
			f := newNodeMaskFixture(b)
			nodes := make([]*graph.Node, 0, tc.masked+tc.identities)
			var masks []store_sqlite.FileMask
			if tc.masked > 0 {
				for file := 0; file < 32; file++ {
					masks = append(masks, store_sqlite.FileMask{RepoPrefix: "alpha", FilePath: fmt.Sprintf("alpha/delta%d.cs", file), Mode: store_sqlite.OwnershipReplace})
				}
			}
			for n := 0; n < tc.masked; n++ {
				path := fmt.Sprintf("alpha/delta%d.cs", n%32)
				nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s::M%d", path, n), Name: fmt.Sprintf("M%d", n), FilePath: path, RepoPrefix: "alpha", Kind: graph.KindType})
			}
			for n := 0; n < tc.identities; n++ {
				node := f.lower.GetNode(f.id)
				if n > 0 {
					node.ID = fmt.Sprintf("alpha/derived.cs::D%d", n)
					node.FilePath = "alpha/derived.cs"
				}
				nodes = append(nodes, node)
				setExplicitMaskFixtureRow(b, f, node.ID, "identity_replace")
			}
			f.upper.AddBatch(nodes, nil)
			if err := f.upper.SetFileMasks(masks); err != nil {
				b.Fatal(err)
			}
			if err := f.control.PublishPayloadGeneration(context.Background(), f.upperID, 3); err != nil {
				b.Fatal(err)
			}
			_, layer := explicitMaskView(b, f)
			if layer.OwnsNodeIdentity(f.id) != (tc.identities > 0) || layer.OwnsOutEdges(f.id) {
				b.Fatal("benchmark identity oracle failed")
			}
			b.Run("constructor", func(b *testing.B) {
				b.ReportAllocs()
				for n := 0; n < b.N; n++ {
					l, err := graphview.NewGenerationLayer(f.upper)
					if err != nil || l.OwnsNodeIdentity(f.id) != (tc.identities > 0) {
						b.Fatalf("constructor oracle: %v", err)
					}
				}
			})
			b.Run("cold_point", func(b *testing.B) {
				b.ReportAllocs()
				for n := 0; n < b.N; n++ {
					v, _ := explicitMaskView(b, f)
					if v.GetNode(f.id) == nil {
						b.Fatal("cold point missing")
					}
				}
			})
			b.Run("repeated_view", func(b *testing.B) {
				b.ReportAllocs()
				for n := 0; n < b.N; n++ {
					v, _ := explicitMaskView(b, f)
					if v.GetNode(f.id) == nil || v.GetNode(f.sibling) == nil {
						b.Fatal("repeated view missing")
					}
				}
			})
			v, _ := explicitMaskView(b, f)
			_ = v.GetNode(f.id)
			b.Run("hot_point", func(b *testing.B) {
				b.ReportAllocs()
				for n := 0; n < b.N; n++ {
					if v.GetNode(f.id) == nil {
						b.Fatal("hot point missing")
					}
				}
			})
		})
	}
}
