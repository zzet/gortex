package graphview

import (
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestGenerationLayerCompactKindInventoryKeepsFileMasks(t *testing.T) {
	builder := newGenerationLayerBuilder(t)
	builder.MarkFile("repo/a.go", false)
	builder.AddNode("repo/a.go", &graph.Node{ID: "repo/a.go", Kind: graph.KindFile, FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go", WorkspaceID: "w", ProjectID: "p", Meta: map[string]any{"full": "payload"}})
	builder.AddNode("repo/a.go", &graph.Node{ID: "repo/a.go::fn", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"})
	layer := builder.Freeze().(graph.OverlayLayerProjectionReader)
	full := layer.LayerNodesInScope([]string{"repo"}, nil, false, graph.KindFile)
	compact := layer.LayerNodesInScope([]string{"repo"}, nil, true, graph.KindFile)
	if len(full) != 1 || len(compact) != 1 {
		t.Fatalf("full=%d compact=%d", len(full), len(compact))
	}
	a, b := full[0], compact[0]
	if a.ID != b.ID || a.Kind != b.Kind || a.FilePath != b.FilePath || a.RepoPrefix != b.RepoPrefix || a.Language != b.Language || a.WorkspaceID != b.WorkspaceID || a.ProjectID != b.ProjectID || a.Meta["full"] != "payload" || b.Meta != nil {
		t.Fatalf("full=%+v compact=%+v", a, b)
	}
	if len(layer.LayerNodesInScope([]string{"foreign"}, nil, true, graph.KindFile)) != 0 {
		t.Fatal("cross-repository inventory")
	}
}
