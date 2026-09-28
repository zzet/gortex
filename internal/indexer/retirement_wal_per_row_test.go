package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// retirementWALFixtureRows builds one generation's worth of rows shaped like
// a working-tree layer of a Go repository: per file, a file node, functions
// and methods with receivers, and calls, reads and references between them,
// with the metadata an extractor stamps.
func retirementWALFixtureRows(files, perFile int) ([]*graph.Node, []*graph.Edge) {
	var nodes []*graph.Node
	var edges []*graph.Edge
	for f := 0; f < files; f++ {
		path := fmt.Sprintf("repo/internal/pkg%02d/file_%04d.go", f%37, f)
		nodes = append(nodes, &graph.Node{ID: path, Kind: graph.KindFile, Name: filepath.Base(path), FilePath: path, RepoPrefix: "repo", Language: "go",
			Meta: map[string]any{"source_derived_decl_fingerprint": fmt.Sprintf("%064x", f), "source_derived_runtime_fingerprint": fmt.Sprintf("%064x", f+1)}})
		for i := 0; i < perFile; i++ {
			id := path + "::Receiver" + strconv.Itoa(f) + ".Method" + strconv.Itoa(i)
			nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindMethod, Name: "Method" + strconv.Itoa(i), QualName: "pkg.Receiver.Method" + strconv.Itoa(i),
				FilePath: path, StartLine: 10 + 12*i, EndLine: 20 + 12*i, RepoPrefix: "repo", Language: "go",
				Meta: map[string]any{"receiver": "Receiver" + strconv.Itoa(f), "return_type": "error", "signature": "func (r *Receiver) Method(ctx context.Context) error"}})
			edges = append(edges,
				&graph.Edge{From: path, To: id, Kind: graph.EdgeDefines, FilePath: path, Line: 10 + 12*i},
				&graph.Edge{From: id, To: path + "::Receiver" + strconv.Itoa(f) + ".Method" + strconv.Itoa((i+1)%perFile), Kind: graph.EdgeCalls, FilePath: path, Line: 12 + 12*i,
					Meta: map[string]any{"recv_self": true}},
				&graph.Edge{From: id, To: "unresolved::*.Close", Kind: graph.EdgeCalls, FilePath: path, Line: 13 + 12*i, Meta: map[string]any{"receiver_expr": "r.store"}},
				&graph.Edge{From: id, To: path + "::Receiver" + strconv.Itoa(f) + ".field" + strconv.Itoa(i%5), Kind: graph.EdgeReads, FilePath: path, Line: 14 + 12*i})
		}
	}
	return nodes, edges
}

// The WAL a retirement writes per deleted row, with the store's real index
// set, for a working-tree layer whose rows sit beside the base corpus's copy
// of the same identities (the identity-first indexes interleave them). It
// logs the measurement; GX_RETIRE_WAL_ROWS scales the fixture.
func TestRetirementWALPerDeletedRow(t *testing.T) {
	files, perFile := 40, 25
	if raw := os.Getenv("GX_RETIRE_WAL_ROWS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			files = n / perFile
		}
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	baseNodes, baseEdges := retirementWALFixtureRows(files, perFile)
	store.AddBatch(baseNodes, baseEdges)
	generationID, handle, _, err := store.BeginPayloadGenerationWithStatus(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: "graph-wal", LayerID: "layer-wal", CheckoutID: "checkout-wal",
		GenerationKind: DirtyLayerGenerationKind, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes, edges := retirementWALFixtureRows(files, perFile)
	insertMark := store.WALWriteMark()
	handle.AddBatch(nodes, edges)
	inserted := store_sqlite.WALWrittenBetween(insertMark, store.WALWriteMark())
	if err := store.Catalog().SetViewGenerationState(ctx, generationID, store_sqlite.ViewGenerationFailed, store_sqlite.ViewGenerationBuilding); err != nil {
		t.Fatal(err)
	}
	census, err := store.GenerationPayloadRowCensus(ctx, generationID)
	if err != nil {
		t.Fatal(err)
	}
	var rows int64
	for _, n := range census.Tables {
		rows += n
	}
	mark := store.WALWriteMark()
	started := time.Now()
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	retired := store_sqlite.WALWrittenBetween(mark, store.WALWriteMark())
	if !retired.Valid || retired.Reset || rows == 0 {
		t.Fatalf("measurement unusable: %+v rows=%d", retired, rows)
	}
	t.Logf("retire: payload rows %d (nodes %d, edges %d, by table %v) wal %d bytes (%d frames) = %.0f bytes per deleted row; insert wal %d bytes = %.0f per row; retire %s",
		rows, len(nodes), len(edges), census.Tables, retired.Bytes, retired.Frames, float64(retired.Bytes)/float64(rows),
		inserted.Bytes, float64(inserted.Bytes)/float64(rows), elapsed)
	if retired.Bytes <= 0 {
		t.Fatal("a retirement wrote no WAL")
	}
}
