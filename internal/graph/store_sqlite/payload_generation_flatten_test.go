package store_sqlite

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The copy-based chain fold, rule by rule, over synthetic generations whose
// masks are written directly: each rule of the layer contract is exercised by
// a member above hiding a row of a member below it.

func flattenMember(t *testing.T, store *Store, layer string, nodes []*graph.Node, edges []*graph.Edge,
	files []FileMask, identities []string, sources []string,
) int64 {
	t.Helper()
	id := reservedGeneration(t, store, layer)
	handle := store.AtGeneration(id)
	if err := handle.AddBatchChecked(nodes, edges); err != nil {
		t.Fatalf("%s: write payload: %v", layer, err)
	}
	if len(files) > 0 {
		if err := handle.SetFileMasks(files); err != nil {
			t.Fatalf("%s: file masks: %v", layer, err)
		}
	}
	if len(identities) > 0 {
		if err := handle.SetNodeTombstones(identities); err != nil {
			t.Fatalf("%s: identity masks: %v", layer, err)
		}
	}
	if len(sources) > 0 {
		masks := make([]EdgeSourceMask, 0, len(sources))
		for _, s := range sources {
			masks = append(masks, EdgeSourceMask{SourceID: s, Mode: OwnershipReplace})
		}
		if err := handle.SetEdgeSourceMasks(masks); err != nil {
			t.Fatalf("%s: edge-source masks: %v", layer, err)
		}
	}
	return id
}

func flatNode(id, file string) *graph.Node {
	return &graph.Node{ID: id, Kind: graph.KindFunction, Name: id, FilePath: file, RepoPrefix: "repo", StartLine: 1, EndLine: 2}
}

func flatEdge(from, to, file string) *graph.Edge {
	return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: 1}
}

func renderGenerationEdges(t *testing.T, store *Store, id int64) []string {
	t.Helper()
	var out []string
	for _, e := range store.AtGeneration(id).AllEdges() {
		out = append(out, e.From+"->"+e.To+"@"+e.FilePath)
	}
	sort.Strings(out)
	return out
}

func renderGenerationNodes(t *testing.T, store *Store, id int64) []string {
	t.Helper()
	var out []string
	for _, n := range store.AtGeneration(id).AllNodes() {
		out = append(out, n.ID+"@"+n.FilePath)
	}
	sort.Strings(out)
	return out
}

func TestFlattenGenerationChainAppliesEachLayerRule(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()

	// Bottom member: owns a.go and b.go; b.go's B calls T1 (recorded at b.go),
	// and a.go's A calls T1 (recorded at a.go).
	bottom := flattenMember(t, store, "bottom",
		[]*graph.Node{flatNode("repo/a.go::A", "repo/a.go"), flatNode("repo/b.go::B", "repo/b.go"), flatNode("repo/gone.go::G", "repo/gone.go")},
		[]*graph.Edge{flatEdge("repo/a.go::A", "repo/t.go::T1", "repo/a.go"), flatEdge("repo/b.go::B", "repo/t.go::T1", "repo/b.go")},
		[]FileMask{
			{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: OwnershipReplace},
			{RepoPrefix: "repo", FilePath: "repo/b.go", Mode: OwnershipReplace},
			{RepoPrefix: "repo", FilePath: "repo/gone.go", Mode: OwnershipReplace},
		}, nil, nil)
	// Top member:
	//   - claims a.go with a new body (path rule: bottom's a.go rows hidden);
	//   - replaces B's outgoing set without claiming b.go (source rule: the
	//     edge bottom recorded at b.go is hidden, the node B still shows);
	//   - removes G's identity without claiming gone.go (identity rule).
	top := flattenMember(t, store, "top",
		[]*graph.Node{flatNode("repo/a.go::A", "repo/a.go")},
		[]*graph.Edge{flatEdge("repo/a.go::A", "repo/t.go::T2", "repo/a.go"), flatEdge("repo/b.go::B", "repo/t.go::T2", "repo/b.go")},
		[]FileMask{{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: OwnershipReplace}},
		[]string{"repo/gone.go::G"}, []string{"repo/b.go::B"})
	folded := reservedGeneration(t, store, "folded")

	if _, err := store.FlattenGenerationChain(ctx, []int64{bottom, top}, folded); err != nil {
		t.Fatalf("FlattenGenerationChain: %v", err)
	}
	if got, want := renderGenerationNodes(t, store, folded), []string{
		"repo/a.go::A@repo/a.go", "repo/b.go::B@repo/b.go",
	}; !slices.Equal(got, want) {
		t.Errorf("folded nodes = %v, want %v", got, want)
	}
	if got, want := renderGenerationEdges(t, store, folded), []string{
		"repo/a.go::A->repo/t.go::T2@repo/a.go",
		"repo/b.go::B->repo/t.go::T2@repo/b.go",
	}; !slices.Equal(got, want) {
		t.Errorf("folded edges = %v, want %v (the bottom member's T1 calls are hidden by the path and source rules)", got, want)
	}
	masks, err := store.AtGeneration(folded).FileMasksContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(masks) != 3 {
		t.Errorf("folded file claims = %+v, want a.go, b.go and gone.go", masks)
	}
	identities, err := store.AtGeneration(folded).NodeIdentityMasksContext(ctx)
	if err != nil || len(identities) != 1 || identities[0].NodeID != "repo/gone.go::G" {
		t.Errorf("folded identity masks = %+v err=%v, want G's", identities, err)
	}
	marks, err := store.AtGeneration(folded).EdgeSourceMasksContext(ctx)
	if err != nil || len(marks) != 1 || marks[0].SourceID != "repo/b.go::B" {
		t.Errorf("folded edge-source marks = %+v err=%v, want B's", marks, err)
	}

	// The whole-generation copy carries every row and mask verbatim.
	copied := reservedGeneration(t, store, "copied")
	if _, err := store.CopyGenerationPayloadWhole(ctx, top, copied); err != nil {
		t.Fatalf("CopyGenerationPayloadWhole: %v", err)
	}
	if got, want := renderGenerationEdges(t, store, copied), renderGenerationEdges(t, store, top); !slices.Equal(got, want) {
		t.Errorf("copied edges = %v, want %v", got, want)
	}
	copiedMarks, _ := store.AtGeneration(copied).EdgeSourceMasksContext(ctx)
	copiedIdentities, _ := store.AtGeneration(copied).NodeIdentityMasksContext(ctx)
	if len(copiedMarks) != 1 || len(copiedIdentities) != 1 {
		t.Errorf("the copy lost masks: marks %+v identities %+v", copiedMarks, copiedIdentities)
	}
}

// TestFlattenGenerationChainHidesEdgesNamingAnIdentityAMemberAboveDropped is
// the endpoint rule: a member above that claims a file and no longer carries
// one of its symbols hides every edge below naming that symbol, wherever the
// edge was recorded — the composed chain serves neither the value the gone
// symbol flowed into another file nor another file's call into it.
func TestFlattenGenerationChainHidesEdgesNamingAnIdentityAMemberAboveDropped(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()

	// Bottom member: claims a.go (Gone, Kept) and b.go (B); b.go records an
	// edge out of Gone and one into it, plus one out of Kept.
	bottom := flattenMember(t, store, "bottom",
		[]*graph.Node{
			flatNode("repo/a.go::Gone", "repo/a.go"), flatNode("repo/a.go::Kept", "repo/a.go"),
			flatNode("repo/b.go::B", "repo/b.go"),
		},
		[]*graph.Edge{
			flatEdge("repo/a.go::Gone", "repo/b.go::B", "repo/b.go"),
			flatEdge("repo/b.go::B", "repo/a.go::Gone", "repo/b.go"),
			flatEdge("repo/a.go::Kept", "repo/b.go::B", "repo/b.go"),
		},
		[]FileMask{
			{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: OwnershipReplace},
			{RepoPrefix: "repo", FilePath: "repo/b.go", Mode: OwnershipReplace},
		}, nil, nil)
	// Top member: re-derives a.go without Gone.
	top := flattenMember(t, store, "top",
		[]*graph.Node{flatNode("repo/a.go::Kept", "repo/a.go")}, nil,
		[]FileMask{{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: OwnershipReplace}}, nil, nil)
	folded := reservedGeneration(t, store, "folded")
	if _, err := store.FlattenGenerationChain(ctx, []int64{bottom, top}, folded); err != nil {
		t.Fatalf("FlattenGenerationChain: %v", err)
	}
	if got, want := renderGenerationEdges(t, store, folded), []string{
		"repo/a.go::Kept->repo/b.go::B@repo/b.go",
	}; !slices.Equal(got, want) {
		t.Errorf("folded edges = %v, want %v (the edges naming Gone recorded at b.go are hidden)", got, want)
	}
	if got, want := renderGenerationNodes(t, store, folded), []string{
		"repo/a.go::Kept@repo/a.go", "repo/b.go::B@repo/b.go",
	}; !slices.Equal(got, want) {
		t.Errorf("folded nodes = %v, want %v", got, want)
	}
}
