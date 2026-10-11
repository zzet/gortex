package graph

import "testing"

// Aggregate counters must charge a replaced base row by its actual file,
// including canonical identities whose IDs do not encode that file.
func TestOverlayCountsResolveBaseNodeFile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          string
		coverOld    bool
		coverNew    bool
		remove      bool
		tombstone   bool
		replacement string
	}{
		{name: "opaque replacement in covered file", coverOld: true, replacement: "repo/helper.go"},
		{name: "opaque removal in covered file", coverOld: true, remove: true},
		{name: "opaque removal in tombstoned file", coverOld: true, remove: true, tombstone: true},
		{name: "opaque relocation from covered file", coverOld: true, coverNew: true, replacement: "other/next.go"},
		{name: "opaque relocation from uncovered file", coverNew: true, replacement: "other/next.go"},
		{name: "prefixed relocation from uncovered file", id: "other/next.go::Canonical", coverNew: true, replacement: "other/next.go"},
		{name: "opaque identity removal outside covered files", remove: true},
		{name: "prefixed identity removal outside covered files", id: "other/next.go::Canonical", coverNew: true, remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.id
			if id == "" {
				id = "env::GORTEX_DERIVED_OUTPUT_PROBE"
			}
			base := New()
			original := &Node{ID: id, Name: "Canonical", Kind: KindContract, FilePath: "repo/helper.go", RepoPrefix: "repo"}
			base.AddNode(original)
			base.AddNode(&Node{ID: "repo/keep.go::Keep", Kind: KindFunction, FilePath: "repo/keep.go", RepoPrefix: "repo"})
			base.AddNode(&Node{ID: "other/keep.go::Keep", Kind: KindFunction, FilePath: "other/keep.go", RepoPrefix: "other"})
			layer := NewOverlayLayer()
			if tc.coverOld {
				layer.MarkFile(original.FilePath, tc.tombstone)
			}
			if tc.coverNew {
				layer.MarkFile("other/next.go", false)
			}
			if tc.remove {
				layer.MarkRemoved(original.Name, original.ID)
			}
			wantTotal := 2
			wantRepo := map[string]int{"repo": 1, "other": 1}
			if tc.replacement != "" {
				replacement := *original
				replacement.FilePath = tc.replacement
				if tc.replacement == "other/next.go" {
					replacement.RepoPrefix = "other"
				}
				layer.AddNode(tc.replacement, &replacement)
				wantTotal++
				wantRepo[replacement.RepoPrefix]++
			}
			view := NewOverlaidView(base, layer)
			nodes := view.AllNodes()
			if len(nodes) != wantTotal {
				t.Fatalf("enumeration = %d nodes, want %d", len(nodes), wantTotal)
			}
			enumerated := make(map[string]int)
			seen := make(map[string]bool)
			for _, node := range nodes {
				if node == nil || seen[node.ID] {
					t.Fatalf("enumeration contains nil or duplicate node: %+v", node)
				}
				seen[node.ID] = true
				enumerated[node.RepoPrefix]++
			}
			// Count aggregation must stay on the overlay footprint; a full
			// node scan would conceal mistakes and scale with the corpus.
			boundedBase := &overlayCountFootprintReader{Reader: base}
			counts := NewOverlaidView(boundedBase, layer)
			if got := counts.NodeCount(); got != wantTotal {
				t.Errorf("NodeCount = %d, enumeration = %d", got, wantTotal)
			}
			if got := counts.Stats().TotalNodes; got != wantTotal {
				t.Errorf("Stats.TotalNodes = %d, enumeration = %d", got, wantTotal)
			}
			stats := counts.RepoStats()
			for repo, want := range wantRepo {
				if got := enumerated[repo]; got != want {
					t.Errorf("enumerated repo %q = %d, want %d", repo, got, want)
				}
				if got := stats[repo].TotalNodes; got != want {
					t.Errorf("RepoStats[%q].TotalNodes = %d, enumeration = %d", repo, got, want)
				}
			}
			if boundedBase.nodeScans != 0 {
				t.Errorf("aggregate counters scanned all base nodes %d times", boundedBase.nodeScans)
			}
			if base.GetNode(id) != original || original.FilePath != "repo/helper.go" || base.NodeCount() != 3 {
				t.Fatal("overlay aggregate reads changed the base")
			}
		})
	}
}

type overlayCountFootprintReader struct {
	Reader
	nodeScans int
}

func (r *overlayCountFootprintReader) AllNodes() []*Node {
	r.nodeScans++
	return r.Reader.AllNodes()
}
