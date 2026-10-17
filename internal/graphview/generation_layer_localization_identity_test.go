package graphview_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func assertLocalizationIdentityPage(t testing.TB, page graph.BoundedNodeProjection, want map[string]string, total int, truncated bool) {
	t.Helper()
	if page.Total != total || page.Truncated != truncated || len(page.Nodes) != len(want) {
		t.Fatalf("page rows=%d total=%d truncated=%t want=%d/%d/%t", len(page.Nodes), page.Total, page.Truncated, len(want), total, truncated)
	}
	seen := map[string]bool{}
	prior := ""
	for _, node := range page.Nodes {
		if node == nil {
			t.Fatal("nil projection row")
		}
		name, found := want[node.ID]
		if !found || seen[node.ID] || node.Name != name {
			t.Fatalf("unexpected projection row %+v", node)
		}
		if prior != "" && node.ID <= prior {
			t.Fatal("projection order/identity duplicated")
		}
		seen[node.ID], prior = true, node.ID
	}
}

func TestGenerationLayerLocalizationIdentityFileAndRenamedName(t *testing.T) {
	f := newNodeMaskFixture(t)
	node := f.lower.GetNode(f.id)
	node.Name, node.QualName, node.StartLine = "Renamed", "Child.Renamed", 91
	node.Meta["revision"] = "new"
	f.upper.AddNode(node)
	if err := f.upper.SetNodeIdentityReplacements([]string{f.id}); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	view, layer := explicitMaskView(t, f)
	if layer.HasFile("alpha/a.cs") {
		t.Fatal("identity acquired whole-file ownership")
	}
	page, err := view.FindFileNodesBounded(t.Context(), "alpha/a.cs", graph.LocalizationNodeScope{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, page, map[string]string{f.id: "Renamed", f.sibling: "Sibling"}, 2, false)
	for _, row := range page.Nodes {
		if row.ID == f.id && row.StartLine != 91 {
			t.Fatal("bounded file retained old source location")
		}
		if row.Meta != nil {
			t.Fatal("bounded file transferred retrieval metadata")
		}
	}
	page, err = view.FindFileNodesBounded(t.Context(), "alpha/a.cs", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, page, map[string]string{f.id: "Renamed"}, 2, true)
	old, err := view.FindNodesByNameBounded(t.Context(), "Run", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, old, nil, 0, false)
	current, err := view.FindNodesByNameBounded(t.Context(), "Renamed", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, current, map[string]string{f.id: "Renamed"}, 1, false)
	if f.upper.NodeCount() != 1 || f.upper.EdgeCount() != 0 || f.lower.GetNode(f.id).Name != "Run" {
		t.Fatal("projection fixture copied siblings/edges or changed lower")
	}
}

func TestGenerationLayerLocalizationIdentityScopeAndTestGate(t *testing.T) {
	f := newNodeMaskFixture(t)
	node := f.lower.GetNode(f.id)
	node.WorkspaceID, node.ProjectID, node.Kind = "new-workspace", "new-project", graph.KindType
	node.Meta["is_test"] = true
	f.upper.AddNode(node)
	if err := f.upper.SetNodeIdentityReplacements([]string{f.id}); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	view, _ := explicitMaskView(t, f)
	for _, tc := range []struct {
		name  string
		scope graph.LocalizationNodeScope
		want  map[string]string
	}{
		{"new_scope", graph.LocalizationNodeScope{WorkspaceID: "new-workspace", ProjectID: "new-project"}, map[string]string{f.id: "Run"}},
		{"old_scope", graph.LocalizationNodeScope{WorkspaceID: "alpha"}, map[string]string{f.sibling: "Sibling"}},
		{"old_kind", graph.LocalizationNodeScope{Kinds: map[graph.NodeKind]bool{graph.NodeKind("function"): true}}, nil},
		{"exclude_tests", graph.LocalizationNodeScope{ExcludeTests: true}, map[string]string{f.sibling: "Sibling"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := view.FindFileNodesBounded(t.Context(), "alpha/a.cs", tc.scope, 1)
			if err != nil {
				t.Fatal(err)
			}
			assertLocalizationIdentityPage(t, page, tc.want, len(tc.want), false)
		})
	}
	oldNameScope, err := view.FindNodesByNameBounded(t.Context(), "Run", graph.LocalizationNodeScope{WorkspaceID: "alpha"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, oldNameScope, nil, 0, false)
}

func TestGenerationLayerLocalizationLegacyDeletionAndDetachedAddition(t *testing.T) {
	f := newNodeMaskFixture(t)
	if err := f.upper.SetNodeTombstones([]string{f.id}); err != nil {
		t.Fatal(err)
	}
	newID := "alpha/uncovered.cs::Added"
	f.upper.AddNode(&graph.Node{ID: newID, Name: "Added", RepoPrefix: "alpha", FilePath: "alpha/uncovered.cs", Kind: graph.KindType})
	if err := f.upper.SetNodeIdentityReplacements([]string{newID}); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	view, _ := explicitMaskView(t, f)
	page, err := view.FindFileNodesBounded(t.Context(), "alpha/a.cs", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, page, map[string]string{f.sibling: "Sibling"}, 1, false)
	added, err := view.FindFileNodesBounded(t.Context(), "alpha/uncovered.cs", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, added, map[string]string{newID: "Added"}, 1, false)
}

func TestGenerationLayerLocalizationIdentityLargeCohortKeepsUnrelatedQueriesAvailable(t *testing.T) {
	f := newNodeMaskFixture(t)
	ids := make([]string, 1024)
	nodes := make([]*graph.Node, 1024)
	for i := range ids {
		ids[i] = fmt.Sprintf("alpha/metadata.cs::M%04d", i)
		nodes[i] = &graph.Node{ID: ids[i], Name: "Metadata", RepoPrefix: "alpha", FilePath: "alpha/metadata.cs", Kind: graph.KindType, Meta: map[string]interface{}{"is_test": i < 1022}}
	}
	f.upper.AddBatch(nodes, nil)
	if err := f.upper.SetNodeIdentityReplacements(ids); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	view, _ := explicitMaskView(t, f)
	file, err := view.FindFileNodesBounded(t.Context(), "alpha/a.cs", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, file, map[string]string{f.id: "Run"}, 2, true)
	name, err := view.FindNodesByNameBounded(t.Context(), "Run", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, name, map[string]string{f.id: "Run"}, 1, false)
	carried, err := view.FindFileNodesBounded(t.Context(), "alpha/metadata.cs", graph.LocalizationNodeScope{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, carried, map[string]string{ids[0]: "Metadata"}, 2, true)
	// The production rows are after three full 256-row metadata batches.
	// This verifies batching, not a global/file 256-marker availability gate.
	production, err := view.FindFileNodesBounded(t.Context(), "alpha/metadata.cs", graph.LocalizationNodeScope{ExcludeTests: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalizationIdentityPage(t, production, map[string]string{ids[1022]: "Metadata"}, 2, true)
	for _, query := range []func(context.Context) (graph.BoundedNodeProjection, error){
		func(ctx context.Context) (graph.BoundedNodeProjection, error) {
			return view.FindFileNodesBounded(ctx, "alpha/a.cs", graph.LocalizationNodeScope{}, 1)
		},
		func(ctx context.Context) (graph.BoundedNodeProjection, error) {
			return view.FindNodesByNameBounded(ctx, "Run", graph.LocalizationNodeScope{}, 1)
		},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		page, err := query(ctx)
		if !errors.Is(err, context.Canceled) || len(page.Nodes) != 0 || page.Total != 0 {
			t.Fatalf("canceled partial page=%+v err=%v", page, err)
		}
	}
}

// Only work matching the actual query is budgeted. The same layer must keep
// unrelated name/file queries available, for both real SQLite and Graph bases.
func TestGenerationLayerLocalizationIdentityMatchingWorkBudget(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("memory_%t_legacy_%t", memory, legacy), func(t *testing.T) {
				f := newNodeMaskFixture(t)
				allocate := func(base int64) int64 {
					t.Helper()
					id, err := f.control.Catalog().CreateViewGeneration(t.Context(), store_sqlite.ViewGeneration{
						OwnerKind: "dedicated_graph", GraphID: "localization-budget-fixture", GenerationKind: "dedicated", BaseGenerationID: base,
						TreeOID: "budget-source", ConfigHash: "budget-policy", State: store_sqlite.ViewGenerationBuilding, CreatedAt: 1,
					})
					if err != nil {
						t.Fatal(err)
					}
					return id
				}
				lowerID := allocate(0)
				f.lower = f.control.AtGeneration(lowerID)
				ids := make([]string, 4097)
				lowerRows := make([]*graph.Node, 0, len(ids)+1)
				upperRows := make([]*graph.Node, 0, len(ids))
				for i := range ids {
					ids[i] = fmt.Sprintf("alpha/matching.cs::N%04d", i)
					lowerRows = append(lowerRows, &graph.Node{ID: ids[i], Name: "Budget", RepoPrefix: "alpha", FilePath: "alpha/matching.cs", Kind: graph.KindType})
					upperRows = append(upperRows, &graph.Node{ID: ids[i], Name: "UpdatedBudget", RepoPrefix: "alpha", FilePath: "alpha/matching.cs", Kind: graph.KindType})
				}
				keep := "alpha/unrelated.cs::Keep"
				lowerRows = append(lowerRows, &graph.Node{ID: keep, Name: "Keep", RepoPrefix: "alpha", FilePath: "alpha/unrelated.cs", Kind: graph.KindType})
				f.lower.AddBatch(lowerRows, nil)
				if err := f.control.Catalog().PublishViewGeneration(t.Context(), lowerID, 2); err != nil {
					t.Fatal(err)
				}
				f.upperID = allocate(lowerID)
				f.upper = f.control.AtGeneration(f.upperID)
				if legacy {
					if err := f.upper.SetNodeTombstones(ids); err != nil {
						t.Fatal(err)
					}
				} else {
					f.upper.AddBatch(upperRows, nil)
					if err := f.upper.SetNodeIdentityReplacements(ids); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
					t.Fatal(err)
				}
				view, layer := explicitMaskView(t, f)
				if memory {
					g := graph.New()
					g.AddBatch(lowerRows, nil)
					view = graph.NewOverlaidViewWithLayer(g, layer)
				}
				name, err := view.FindNodesByNameBounded(t.Context(), "Keep", graph.LocalizationNodeScope{}, 1)
				if err != nil {
					t.Fatal(err)
				}
				assertLocalizationIdentityPage(t, name, map[string]string{keep: "Keep"}, 1, false)
				file, err := view.FindFileNodesBounded(t.Context(), "alpha/unrelated.cs", graph.LocalizationNodeScope{}, 1)
				if err != nil {
					t.Fatal(err)
				}
				assertLocalizationIdentityPage(t, file, map[string]string{keep: "Keep"}, 1, false)
				page, err := view.FindNodesByNameBounded(t.Context(), "Budget", graph.LocalizationNodeScope{}, 1)
				var limitErr *graph.BoundedLocalizationLimitError
				if !errors.As(err, &limitErr) || limitErr.Resource != "identity-filtered node inspections" || limitErr.Limit != 4096 || len(page.Nodes) != 0 || page.Total != 0 || page.Truncated {
					t.Fatalf("matching-name budget partial=%+v err=%v", page, err)
				}
				if legacy {
					page, err = view.FindFileNodesBounded(t.Context(), "alpha/matching.cs", graph.LocalizationNodeScope{}, 1)
					if !errors.As(err, &limitErr) || limitErr.Resource != "identity-filtered node inspections" || len(page.Nodes) != 0 || page.Total != 0 {
						t.Fatalf("matching-file budget partial=%+v err=%v", page, err)
					}
				}
			})
		}
	}
}

func TestGenerationLayerLocalizationIdentityReadFailureDoesNotFallBack(t *testing.T) {
	f := newNodeMaskFixture(t)
	f.upper.AddNode(f.lower.GetNode(f.id))
	if err := f.upper.SetNodeIdentityReplacements([]string{f.id}); err != nil {
		t.Fatal(err)
	}
	if err := f.control.PublishPayloadGeneration(t.Context(), f.upperID, 3); err != nil {
		t.Fatal(err)
	}
	view, _ := explicitMaskView(t, f)
	if err := f.control.Close(); err != nil {
		t.Fatal(err)
	}
	page, err := view.FindFileNodesBounded(t.Context(), "alpha/a.cs", graph.LocalizationNodeScope{ExcludeTests: true}, 1)
	if err == nil || len(page.Nodes) != 0 || page.Total != 0 || page.Truncated {
		t.Fatalf("read failure became partial/lower result=%+v err=%v", page, err)
	}
}
