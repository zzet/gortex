package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestCursorMetadataDecodedBeforeRowsNext(t *testing.T) {
	store := openRawBytesTestStore(t)
	payloadA := strings.Repeat("a", 4096)
	payloadB := strings.Repeat("b", 4096)
	store.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "a", QualName: "pkg.a", Meta: map[string]any{"payload": payloadA}},
		{ID: "b", Kind: graph.KindFunction, Name: "b", QualName: "pkg.b", Meta: map[string]any{"payload": payloadB}},
	}, []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1, Meta: map[string]any{"payload": payloadA}},
		{From: "b", To: "a", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2, Meta: map[string]any{"payload": payloadB}},
	})

	nodeRows, err := store.db.Query(`SELECT ` + lookupNodeCols + ` FROM nodes ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*graph.Node
	for nodeRows.Next() {
		node, scanErr := scanNodeCursor(nodeRows)
		if scanErr != nil {
			_ = nodeRows.Close()
			t.Fatal(scanErr)
		}
		nodes = append(nodes, node)
	}
	if err := nodeRows.Err(); err != nil {
		_ = nodeRows.Close()
		t.Fatal(err)
	}
	if err := nodeRows.Close(); err != nil {
		t.Fatal(err)
	}
	assertMetadataPayload(t, nodesByID(nodes)["a"].Meta, payloadA)
	assertMetadataPayload(t, nodesByID(nodes)["b"].Meta, payloadB)

	edgeRows, err := store.db.Query(`SELECT ` + lookupEdgeCols + ` FROM edges ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var edges []*graph.Edge
	for edgeRows.Next() {
		edge, scanErr := store.scanEdgeCursor(edgeRows)
		if scanErr != nil {
			_ = edgeRows.Close()
			t.Fatal(scanErr)
		}
		edges = append(edges, edge)
	}
	if err := edgeRows.Err(); err != nil {
		_ = edgeRows.Close()
		t.Fatal(err)
	}
	if err := edgeRows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(edges))
	}
	for _, edge := range edges {
		if edge.From == "a" {
			assertMetadataPayload(t, edge.Meta, payloadA)
		} else {
			assertMetadataPayload(t, edge.Meta, payloadB)
		}
	}
}

func TestPointNodeScansKeepCopiedMetadata(t *testing.T) {
	store := openRawBytesTestStore(t)
	payload := strings.Repeat("point", 1024)
	store.AddNode(&graph.Node{
		ID:       "point",
		Kind:     graph.KindFunction,
		Name:     "point",
		QualName: "pkg.point",
		Meta:     map[string]any{"payload": payload},
	})

	assertMetadataPayload(t, store.GetNode("point").Meta, payload)
	assertMetadataPayload(t, store.GetNodeByQualName("pkg.point").Meta, payload)
	node, err := store.GetNodeContext(context.Background(), "point")
	if err != nil {
		t.Fatal(err)
	}
	assertMetadataPayload(t, node.Meta, payload)
}

func openRawBytesTestStore(tb testing.TB) *Store {
	tb.Helper()
	store, err := Open(filepath.Join(tb.TempDir(), "rawbytes.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	return store
}

func nodesByID(nodes []*graph.Node) map[string]*graph.Node {
	out := make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		out[node.ID] = node
	}
	return out
}

func assertMetadataPayload(t *testing.T, meta map[string]any, want string) {
	t.Helper()
	got, _ := meta["payload"].(string)
	if got != want {
		t.Fatalf("metadata payload length = %d, want %d", len(got), len(want))
	}
}

var benchmarkMetadataSink any

func BenchmarkMetadataCursorScan(b *testing.B) {
	for _, metadataBytes := range []int{256, 4096} {
		store := metadataBenchmarkStore(b, metadataBytes)
		b.Run(fmt.Sprintf("node/%d/copied-bytes", metadataBytes), func(b *testing.B) {
			benchmarkNodeMetadataCursor(b, store, false)
		})
		b.Run(fmt.Sprintf("node/%d/raw-bytes", metadataBytes), func(b *testing.B) {
			benchmarkNodeMetadataCursor(b, store, true)
		})
		b.Run(fmt.Sprintf("edge/%d/copied-bytes", metadataBytes), func(b *testing.B) {
			benchmarkEdgeMetadataCursor(b, store, false)
		})
		b.Run(fmt.Sprintf("edge/%d/raw-bytes", metadataBytes), func(b *testing.B) {
			benchmarkEdgeMetadataCursor(b, store, true)
		})
	}
}

func metadataBenchmarkStore(b *testing.B, metadataBytes int) *Store {
	b.Helper()
	store := openRawBytesTestStore(b)
	payload := strings.Repeat("x", metadataBytes)
	const rowCount = 128
	nodes := make([]*graph.Node, 0, rowCount)
	edges := make([]*graph.Edge, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		id := fmt.Sprintf("n-%03d", i)
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: id,
			Meta: map[string]any{"payload": payload, "row": id},
		})
		to := fmt.Sprintf("n-%03d", (i+1)%rowCount)
		edges = append(edges, &graph.Edge{
			From: id, To: to, Kind: graph.EdgeCalls, FilePath: "bench.go", Line: i + 1,
			Meta: map[string]any{"payload": payload, "row": id},
		})
	}
	store.AddBatch(nodes, edges)
	return store
}

func benchmarkNodeMetadataCursor(b *testing.B, store *Store, raw bool) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := store.db.Query(`SELECT ` + lookupNodeCols + ` FROM nodes ORDER BY id`)
		if err != nil {
			b.Fatal(err)
		}
		for rows.Next() {
			var node *graph.Node
			if raw {
				node, err = scanNodeCursor(rows)
			} else {
				var metaBlob []byte
				node, err = scanNodeWithMeta(rows, &metaBlob)
			}
			if err != nil {
				_ = rows.Close()
				b.Fatal(err)
			}
			benchmarkMetadataSink = node.Meta["payload"]
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			b.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkEdgeMetadataCursor(b *testing.B, store *Store, raw bool) {
	b.Helper()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := store.db.Query(`SELECT ` + lookupEdgeCols + ` FROM edges ORDER BY id`)
		if err != nil {
			b.Fatal(err)
		}
		for rows.Next() {
			var edge *graph.Edge
			if raw {
				edge, err = store.scanEdgeCursor(rows)
			} else {
				var metaBlob []byte
				edge, err = scanEdgeWithMeta(store, rows, &metaBlob)
			}
			if err != nil {
				_ = rows.Close()
				b.Fatal(err)
			}
			benchmarkMetadataSink = edge.Meta["payload"]
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			b.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
