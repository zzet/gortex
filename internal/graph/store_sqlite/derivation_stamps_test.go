package store_sqlite

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const (
	stampFile   = "repo/pkg/store.go"
	stampSrcA   = "repo/pkg/store.go::Store.Bump"
	stampSrcB   = "repo/pkg/store.go::Store.Reset"
	stampField1 = "repo/pkg/store.go::Store.mu"
	stampField2 = "repo/pkg/store.go::Store.count"
	stampFileID = "repo/pkg/store.go"
)

// publishedStampGeneration builds and publishes a generation holding one file
// with two methods, a call between them and two capability edges, stamped
// capability version 1. It returns the store and the generation.
func publishedStampGeneration(t *testing.T) (*Store, int64) {
	t.Helper()
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	handle.AddBatch([]*graph.Node{
		{ID: stampFileID, Kind: graph.KindFile, Name: "store.go", FilePath: stampFile, RepoPrefix: "repo",
			Meta: map[string]any{"source_derived_decl_fingerprint": "old"}},
		{ID: stampSrcA, Kind: graph.KindMethod, Name: "Bump", FilePath: stampFile, RepoPrefix: "repo"},
		{ID: stampSrcB, Kind: graph.KindMethod, Name: "Reset", FilePath: stampFile, RepoPrefix: "repo"},
		{ID: stampField1, Kind: graph.KindField, Name: "mu", FilePath: stampFile, RepoPrefix: "repo"},
		{ID: stampField2, Kind: graph.KindField, Name: "count", FilePath: stampFile, RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: stampSrcA, To: stampSrcB, Kind: graph.EdgeCalls, FilePath: stampFile, Line: 3},
		{From: stampSrcA, To: stampField1, Kind: graph.EdgeAccessesField, FilePath: stampFile, Line: 4},
		{From: stampSrcB, To: stampField2, Kind: graph.EdgeAccessesField, FilePath: stampFile, Line: 9},
	})
	if err := handle.WriteDerivationStamps(ctx, map[string]int{"capability": 1, "fingerprints": 2}); err != nil {
		t.Fatalf("WriteDerivationStamps while building: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 5000); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return store, generationID
}

func stampEdgesAt(t *testing.T, store *Store, generationID int64) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT from_id, to_id, kind FROM edges WHERE view_gen = ?`, generationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var from, to, kind string
		if err := rows.Scan(&from, &to, &kind); err != nil {
			t.Fatal(err)
		}
		out = append(out, kind+" "+from+" -> "+to)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestDerivationStampsSealWithTheGeneration: stamps are written while the
// generation builds, read back after publish, refused by an ordinary write once
// published, listed as stale for a newer pass version, and swept on retirement.
func TestDerivationStampsSealWithTheGeneration(t *testing.T) {
	ctx := context.Background()
	store, generationID := publishedStampGeneration(t)
	got, err := store.AtGeneration(generationID).DerivationStamps(ctx)
	if err != nil || got["capability"] != 1 || got["fingerprints"] != 2 || len(got) != 2 {
		t.Fatalf("stamps after publish = %v, %v", got, err)
	}
	if err := store.AtGeneration(generationID).WriteDerivationStamps(ctx, map[string]int{"capability": 3}); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("stamp write to a published generation = %v, want ErrPayloadGenerationSealed", err)
	}
	if err := store.AtGeneration(generationID).WriteDerivationStamps(ctx, map[string]int{"": 1}); !errors.Is(err, ErrDerivationStampInvalid) {
		t.Fatalf("empty pass = %v, want ErrDerivationStampInvalid", err)
	}
	stale, err := store.GenerationsWithStaleDerivation(ctx, "capability", 2)
	if err != nil || len(stale) != 1 || stale[0].GenerationID != generationID || stale[0].Version != 1 {
		t.Fatalf("stale for capability v2 = %+v, %v", stale, err)
	}
	if stale, err := store.GenerationsWithStaleDerivation(ctx, "capability", 1); err != nil || len(stale) != 0 {
		t.Fatalf("stale for capability v1 = %+v, %v; want none", stale, err)
	}
	if stale, err := store.GenerationsWithStaleDerivation(ctx, "hierarchy", 1); err != nil || len(stale) != 1 || stale[0].Version != 0 {
		t.Fatalf("an unstamped pass reads as version 0: %+v, %v", stale, err)
	}
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if n := countAtGeneration(t, store, "generation_derivation_stamps", generationID); n != 0 {
		t.Fatalf("retirement left %d stamp rows", n)
	}
}

// TestDerivedCorrectionRewritesOnlyThePassRowsOfASealedGeneration corrects a
// published generation's capability rows for one source and a file node's
// derived fingerprint, and pins that nothing else moved: the call edge, the
// other source's capability edge, the seal against ordinary writes.
func TestDerivedCorrectionRewritesOnlyThePassRowsOfASealedGeneration(t *testing.T) {
	ctx := context.Background()
	store, generationID := publishedStampGeneration(t)
	before := stampEdgesAt(t, store, generationID)

	c, err := store.BeginDerivedCorrection(ctx, DerivedCorrectionRequest{
		GenerationID: generationID, Pass: "capability", FromVersion: 1, ToVersion: 2,
		EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField},
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	newEdge := &graph.Edge{From: stampSrcA, To: stampField2, Kind: graph.EdgeAccessesField, FilePath: stampFile, Line: 5}
	fileNode := &graph.Node{ID: stampFileID, Kind: graph.KindFile, Name: "store.go", FilePath: stampFile, RepoPrefix: "repo",
		Meta: map[string]any{"source_derived_decl_fingerprint": "new"}}
	if err := c.ReplaceSourceEdges(ctx, []string{stampSrcA}, []*graph.Edge{newEdge}, []*graph.Node{fileNode}); err != nil {
		t.Fatalf("chunk: %v", err)
	}
	// Re-running a chunk is idempotent (a correction interrupted before its
	// Finish runs again from the start).
	if err := c.ReplaceSourceEdges(ctx, []string{stampSrcA}, []*graph.Edge{newEdge}, nil); err != nil {
		t.Fatalf("repeated chunk: %v", err)
	}
	if epoch := store.GenerationCorrectionEpoch(generationID); epoch != 0 {
		t.Fatalf("epoch before Finish = %d", epoch)
	}
	stats, err := c.Finish(ctx)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if stats.Chunks != 2 || stats.ChangedChunks != 1 || stats.EdgesInserted != 1 || stats.EdgesDeleted != 1 || stats.NodesUpdated != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	after := stampEdgesAt(t, store, generationID)
	want := []string{
		"accesses_field " + stampSrcA + " -> " + stampField2,
		"accesses_field " + stampSrcB + " -> " + stampField2,
		"calls " + stampSrcA + " -> " + stampSrcB,
	}
	if len(after) != len(want) {
		t.Fatalf("edges after = %v (before %v), want %v", after, before, want)
	}
	for i := range want {
		if after[i] != want[i] {
			t.Fatalf("edges after = %v (before %v), want %v", after, before, want)
		}
	}
	node := store.AtGeneration(generationID).GetNode(stampFileID)
	if node == nil || node.Meta["source_derived_decl_fingerprint"] != "new" {
		t.Fatalf("file node after correction = %+v", node)
	}
	stamps, err := store.AtGeneration(generationID).DerivationStamps(ctx)
	if err != nil || stamps["capability"] != 2 || stamps["fingerprints"] != 2 {
		t.Fatalf("stamps after = %v, %v", stamps, err)
	}
	if epoch := store.GenerationCorrectionEpoch(generationID); epoch != 1 {
		t.Fatalf("epoch after Finish = %d, want 1", epoch)
	}
	// The seal against ordinary payload writes is untouched.
	if err := store.AtGeneration(generationID).AddBatchChecked(nil, []*graph.Edge{
		{From: stampSrcB, To: stampSrcA, Kind: graph.EdgeCalls, FilePath: stampFile, Line: 11},
	}); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("ordinary write after a correction = %v, want ErrPayloadGenerationSealed", err)
	}
	if _, err := c.Finish(ctx); !errors.Is(err, ErrDerivedCorrectionRefused) {
		t.Fatalf("second Finish = %v, want ErrDerivedCorrectionRefused", err)
	}
}

// TestDerivedCorrectionRefusals pins every refusal: a row outside the pass,
// a new or moved identity, an oversized chunk, a stamp that moved, and a
// generation that is retiring.
func TestDerivedCorrectionRefusals(t *testing.T) {
	ctx := context.Background()
	store, generationID := publishedStampGeneration(t)
	req := DerivedCorrectionRequest{GenerationID: generationID, Pass: "capability", FromVersion: 1, ToVersion: 2,
		EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}}

	if _, err := store.BeginDerivedCorrection(ctx, DerivedCorrectionRequest{GenerationID: generationID, Pass: "capability", FromVersion: 0, ToVersion: 2,
		EdgeKinds: req.EdgeKinds}); !errors.Is(err, ErrDerivationStampMoved) {
		t.Fatalf("begin from the wrong version = %v, want ErrDerivationStampMoved", err)
	}
	if _, err := store.BeginDerivedCorrection(ctx, DerivedCorrectionRequest{GenerationID: generationID, Pass: "capability", FromVersion: 1, ToVersion: 1,
		EdgeKinds: req.EdgeKinds}); !errors.Is(err, ErrDerivationStampInvalid) {
		t.Fatalf("a non-increasing version = %v, want ErrDerivationStampInvalid", err)
	}
	if _, err := store.BeginDerivedCorrection(ctx, DerivedCorrectionRequest{GenerationID: 0, Pass: "capability", FromVersion: 1, ToVersion: 2,
		EdgeKinds: req.EdgeKinds}); !errors.Is(err, ErrDerivedCorrectionRefused) {
		t.Fatalf("the base generation = %v, want ErrDerivedCorrectionRefused", err)
	}

	c, err := store.BeginDerivedCorrection(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	refused := func(name string, sources []string, edges []*graph.Edge, nodes []*graph.Node) {
		t.Helper()
		if err := c.ReplaceSourceEdges(ctx, sources, edges, nodes); !errors.Is(err, ErrDerivedCorrectionRefused) {
			t.Fatalf("%s = %v, want ErrDerivedCorrectionRefused", name, err)
		}
	}
	refused("an edge of another kind", []string{stampSrcA},
		[]*graph.Edge{{From: stampSrcA, To: stampSrcB, Kind: graph.EdgeCalls, FilePath: stampFile, Line: 7}}, nil)
	refused("an edge whose source is outside the chunk", []string{stampSrcA},
		[]*graph.Edge{{From: stampSrcB, To: stampField1, Kind: graph.EdgeAccessesField, FilePath: stampFile, Line: 7}}, nil)
	refused("a new identity", nil, nil,
		[]*graph.Node{{ID: "repo/pkg/store.go::Store.fresh", Kind: graph.KindField, Name: "fresh", FilePath: stampFile, RepoPrefix: "repo"}})
	refused("a renamed identity", nil, nil,
		[]*graph.Node{{ID: stampField1, Kind: graph.KindField, Name: "mutex", FilePath: stampFile, RepoPrefix: "repo"}})
	refused("a re-kinded identity", nil, nil,
		[]*graph.Node{{ID: stampField1, Kind: graph.KindFunction, Name: "mu", FilePath: stampFile, RepoPrefix: "repo"}})
	big := make([]*graph.Edge, DerivedCorrectionMaxChunkRows+1)
	for i := range big {
		big[i] = &graph.Edge{From: stampSrcA, To: stampField1, Kind: graph.EdgeAccessesField, FilePath: stampFile, Line: i}
	}
	refused("an oversized chunk", []string{stampSrcA}, big, nil)
	if got := stampEdgesAt(t, store, generationID); len(got) != 3 {
		t.Fatalf("a refused chunk wrote rows: %v", got)
	}

	// Another correction finishes first: this one's chunks and Finish are
	// refused rather than overwriting the newer stamp.
	other, err := store.BeginDerivedCorrection(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Finish(ctx); err != nil {
		t.Fatalf("other finish: %v", err)
	}
	if err := c.ReplaceSourceEdges(ctx, []string{stampSrcA}, nil, nil); !errors.Is(err, ErrDerivationStampMoved) {
		t.Fatalf("chunk after the stamp moved = %v, want ErrDerivationStampMoved", err)
	}
	if _, err := c.Finish(ctx); !errors.Is(err, ErrDerivationStampMoved) {
		t.Fatalf("finish after the stamp moved = %v, want ErrDerivationStampMoved", err)
	}

	// A generation that turned retiring is never written: its sweep may
	// already have run past the rows a chunk would add.
	next, err := store.BeginDerivedCorrection(ctx, DerivedCorrectionRequest{GenerationID: generationID, Pass: "capability", FromVersion: 2, ToVersion: 3,
		EdgeKinds: req.EdgeKinds})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.writerDB.Exec(`UPDATE view_generations SET state = ? WHERE generation_id = ?`, string(ViewGenerationRetiring), generationID); err != nil {
		t.Fatal(err)
	}
	if err := next.ReplaceSourceEdges(ctx, []string{stampSrcA},
		[]*graph.Edge{{From: stampSrcA, To: stampField1, Kind: graph.EdgeAccessesField, FilePath: stampFile, Line: 8}}, nil); !errors.Is(err, ErrDerivedCorrectionRefused) {
		t.Fatalf("chunk into a retiring generation = %v, want ErrDerivedCorrectionRefused", err)
	}
	if _, err := next.Finish(ctx); !errors.Is(err, ErrDerivedCorrectionRefused) {
		t.Fatalf("finish on a retiring generation = %v, want ErrDerivedCorrectionRefused", err)
	}
	if got := stampEdgesAt(t, store, generationID); len(got) != 3 {
		t.Fatalf("a retiring generation was written: %v", got)
	}
}

// A correction whose chunks restate the rows a generation already holds
// changes nothing: no chunk commits, the correction epoch stays (so caches of
// the generation's rows stay valid), the stamp still moves. A chunk that
// changes one row moves the epoch.
func TestDerivedCorrectionOfIdenticalRowsKeepsTheEpoch(t *testing.T) {
	ctx := context.Background()
	store, generationID := publishedStampGeneration(t)
	same := []*graph.Edge{{From: stampSrcA, To: stampField1, Kind: graph.EdgeAccessesField, FilePath: stampFile, Line: 4}}
	c, err := store.BeginDerivedCorrection(ctx, DerivedCorrectionRequest{GenerationID: generationID, Pass: "capability", FromVersion: 1, ToVersion: 2,
		EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	if err != nil {
		t.Fatal(err)
	}
	walBefore := walFileSize(store.dbPath + "-wal")
	if err := c.ReplaceSourceEdges(ctx, []string{stampSrcA}, same, nil); err != nil {
		t.Fatal(err)
	}
	stats, err := c.Finish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Chunks != 1 || stats.ChangedChunks != 0 {
		t.Fatalf("stats = %+v, want one unchanged chunk", stats)
	}
	if epoch := store.GenerationCorrectionEpoch(generationID); epoch != 0 {
		t.Fatalf("an identical rewrite moved the correction epoch to %d", epoch)
	}
	if stamps, err := store.AtGeneration(generationID).DerivationStamps(ctx); err != nil || stamps["capability"] != 2 {
		t.Fatalf("the stamp must still move: %v, %v", stamps, err)
	}
	_ = walBefore

	changed := []*graph.Edge{{From: stampSrcA, To: stampField2, Kind: graph.EdgeAccessesField, FilePath: stampFile, Line: 4}}
	c2, err := store.BeginDerivedCorrection(ctx, DerivedCorrectionRequest{GenerationID: generationID, Pass: "capability", FromVersion: 2, ToVersion: 3,
		EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.ReplaceSourceEdges(ctx, []string{stampSrcA}, changed, nil); err != nil {
		t.Fatal(err)
	}
	stats, err = c2.Finish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.ChangedChunks != 1 {
		t.Fatalf("stats = %+v, want one changed chunk", stats)
	}
	if epoch := store.GenerationCorrectionEpoch(generationID); epoch != 1 {
		t.Fatalf("a changing correction left the epoch at %d, want 1", epoch)
	}
}
