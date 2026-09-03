package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Parity + benchmark coverage for the production JSONB ingest path
// (add_batch_json.go). The fixture deliberately stresses encoding edges:
// quotes, NUL bytes, non-ASCII, >2^53 integers, nested metadata, and NULL
// columns — the raw stored rows must be byte- and type-identical to the
// placeholder writer's.

func jsonIngestExperimentFixture(nodeCount, edgeCount int) ([]*graph.Node, []*graph.Edge) {
	nodes := make([]*graph.Node, nodeCount)
	for i := range nodes {
		meta := map[string]any{
			"signature":       fmt.Sprintf("func symbol_%d(arg string) (int, error)", i),
			"visibility":      "public",
			"doc":             fmt.Sprintf("fixture documentation %d — λ", i),
			"external":        i%7 == 0,
			"return_type":     "(int, error)",
			"is_async":        i%11 == 0,
			"is_static":       i%13 == 0,
			"is_abstract":     i%17 == 0,
			"is_exported":     i%3 == 0,
			"updated_at":      int64(9007199254740993) + int64(i),
			"data_class":      "code",
			"semantic_type":   "func(string) (int, error)",
			"semantic_source": "fixture",
			"clone_sig":       fmt.Sprintf("clone:%016x", i),
			"ordinal":         int64(i),
			"ratio":           float64(i%101) / 100,
			"tags":            []string{"ingest", fmt.Sprintf("bucket-%d", i%19)},
		}
		if i%29 == 0 {
			meta["nested"] = map[string]any{"enabled": true, "rank": i}
		}
		nodes[i] = &graph.Node{
			ID:          fmt.Sprintf("repo::node::%08d", i),
			Kind:        graph.NodeKind("function"),
			Name:        fmt.Sprintf("symbol_%d", i),
			QualName:    fmt.Sprintf("example/repo/pkg%d.symbol_%d", i%251, i),
			FilePath:    fmt.Sprintf("pkg/%03d/file_%05d.go", i%251, i%4096),
			StartLine:   i%700 + 1,
			EndLine:     i%700 + 7,
			StartColumn: i % 32,
			EndColumn:   i%32 + 8,
			Language:    "go",
			RepoPrefix:  "example/repo",
			WorkspaceID: "workspace",
			ProjectID:   fmt.Sprintf("project-%d", i%7),
			Meta:        meta,
		}
	}
	if len(nodes) > 0 {
		nodes[0].Name = "quote-' nul-\x00 unicode-λ"
		nodes[0].Meta["doc"] = "line one\nline two\x00λ"
	}

	edges := make([]*graph.Edge, edgeCount)
	for i := range edges {
		meta := map[string]any{
			"ref_name":                   fmt.Sprintf("callee_%d", i%997),
			"resolve_terminal":           i%31 == 0,
			"resolve_terminal_reason":    fmt.Sprintf("fixture-%d", i%5),
			"semantic_source":            "fixture",
			"return_usage":               "assigned",
			"candidate_count_before_sql": int64(i % 23),
		}
		if i%37 == 0 {
			meta["nested"] = map[string]any{"edge": i, "valid": true}
		}
		edges[i] = &graph.Edge{
			From:            fmt.Sprintf("repo::node::%08d", i%nodeCount),
			To:              fmt.Sprintf("unresolved::callee_%d", i%997),
			Kind:            graph.EdgeKind("calls"),
			FilePath:        fmt.Sprintf("pkg/%03d/file_%05d.go", i%251, i%4096),
			Line:            i + 1,
			Confidence:      0.25 + float64(i%75)/100,
			ConfidenceLabel: "ast",
			Origin:          "parser",
			Tier:            "ast",
			CrossRepo:       i%43 == 0,
			Meta:            meta,
		}
	}
	return nodes, edges
}

func ingestCurrentExperiment(tx *sql.Tx, nodes []*graph.Node, edges []*graph.Edge) (int, int, error) {
	limit := sqliteBatchVariableHardCap
	_, nodeStatements, _, err := insertNodeChunksTxLimited(tx, baseViewGeneration, nodes, false, &limit)
	if err != nil {
		return nodeStatements, 0, err
	}
	_, edgeStatements, _, err := insertEdgeChunksTxLimited(tx, baseViewGeneration, edges, false, &limit)
	return nodeStatements, edgeStatements, err
}

func ingestJSONExperiment(tx *sql.Tx, nodes []*graph.Node, edges []*graph.Edge) (int, int, error) {
	_, nodeStatements, _, err := insertNodeChunksJSONBTx(tx, baseViewGeneration, nodes, false)
	if err != nil {
		return nodeStatements, 0, err
	}
	_, edgeStatements, _, err := insertEdgeChunksJSONBTx(tx, baseViewGeneration, edges, false)
	return nodeStatements, edgeStatements, err
}

func ingestJSONReturningExperiment(tx *sql.Tx, nodes []*graph.Node, edges []*graph.Edge) (int, int, error) {
	_, nodeStatements, _, err := insertNodeChunksJSONBTx(tx, baseViewGeneration, nodes, true)
	if err != nil {
		return nodeStatements, 0, err
	}
	_, edgeStatements, _, err := insertEdgeChunksJSONBTx(tx, baseViewGeneration, edges, true)
	return nodeStatements, edgeStatements, err
}

func writeIngestExperiment(path string, nodes []*graph.Node, edges []*graph.Edge, jsonPath bool) error {
	store, err := Open(path)
	if err != nil {
		return err
	}
	tx, err := store.writerDB.Begin()
	if err != nil {
		_ = store.Close()
		return err
	}
	if jsonPath {
		_, _, err = ingestJSONExperiment(tx, nodes, edges)
	} else {
		_, _, err = ingestCurrentExperiment(tx, nodes, edges)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	return errorsJoin(err, store.Close())
}

// errorsJoin is intentionally tiny so this experiment remains compatible with
// the package's current Go language level without changing production imports.
func errorsJoin(first, second error) error {
	if first != nil {
		return first
	}
	return second
}

func compareAttachedIngestTable(ctx context.Context, conn *sql.Conn, table string, columns []string, identity string) error {
	conditions := make([]string, 0, len(columns)*2)
	for _, column := range columns {
		conditions = append(conditions,
			fmt.Sprintf("a.%s IS b.%s", column, column),
			fmt.Sprintf("typeof(a.%s) = typeof(b.%s)", column, column),
		)
	}
	matches := strings.Join(conditions, " AND ")
	for _, pair := range [][2]string{{"main", "reference"}, {"reference", "main"}} {
		query := fmt.Sprintf(`SELECT COUNT(*)
FROM %s.%s AS a
LEFT JOIN %s.%s AS b ON %s
WHERE b.rowid IS NULL OR NOT (%s)`, pair[0], table, pair[1], table, identity, matches)
		if table == "nodes" {
			query = strings.Replace(query, "b.rowid IS NULL", "b.id IS NULL", 1)
		}
		var differences int
		if err := conn.QueryRowContext(ctx, query).Scan(&differences); err != nil {
			return err
		}
		if differences != 0 {
			return fmt.Errorf("%s differs in %d rows (%s -> %s)", table, differences, pair[0], pair[1])
		}
	}
	return nil
}

func TestJSONBIngestExperimentPreservesRawRowsAfterReopen(t *testing.T) {
	nodes, edges := jsonIngestExperimentFixture(257, 1025)
	currentPath := filepath.Join(t.TempDir(), "current.sqlite")
	jsonPath := filepath.Join(t.TempDir(), "json.sqlite")
	if err := writeIngestExperiment(currentPath, nodes, edges, false); err != nil {
		t.Fatal(err)
	}
	if err := writeIngestExperiment(jsonPath, nodes, edges, true); err != nil {
		t.Fatal(err)
	}

	current, err := Open(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	updates := make([]*graph.Node, 32)
	for i := range updates {
		copyNode := *nodes[i]
		copyNode.Name += "-updated"
		copyNode.Meta = make(map[string]any, len(nodes[i].Meta))
		for key, value := range nodes[i].Meta {
			copyNode.Meta[key] = value
		}
		delete(copyNode.Meta, "clone_sig")
		copyNode.Meta["doc"] = fmt.Sprintf("updated %d", i)
		updates[i] = &copyNode
	}
	duplicateEdges := make([]*graph.Edge, 32)
	for i := range duplicateEdges {
		copyEdge := *edges[i]
		copyEdge.Confidence = 0.001
		copyEdge.Meta = map[string]any{"replacement": true}
		duplicateEdges[i] = &copyEdge
	}
	tx, err := current.writerDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ingestCurrentExperiment(tx, updates, duplicateEdges); err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}

	candidate, err := Open(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = candidate.writerDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ingestJSONExperiment(tx, updates, duplicateEdges); err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}

	candidate, err = Open(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	ctx := context.Background()
	conn, err := candidate.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS reference`, currentPath); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `DETACH DATABASE reference`) }()

	nodeColumns := strings.Split(nodeInsertColumns, ", ")
	if err := compareAttachedIngestTable(ctx, conn, "nodes", nodeColumns, "a.id = b.id"); err != nil {
		t.Fatal(err)
	}
	edgeColumns := strings.Split(edgeInsertColumns, ", ")
	edgeIdentity := "a.from_id = b.from_id AND a.to_id = b.to_id AND a.kind = b.kind AND a.file_path = b.file_path AND a.line = b.line"
	if err := compareAttachedIngestTable(ctx, conn, "edges", edgeColumns, edgeIdentity); err != nil {
		t.Fatal(err)
	}
	for _, database := range []string{"main", "reference"} {
		var integrity string
		if err := conn.QueryRowContext(ctx, `PRAGMA `+database+`.integrity_check`).Scan(&integrity); err != nil {
			t.Fatal(err)
		}
		if integrity != "ok" {
			t.Fatalf("%s integrity_check: %s", database, integrity)
		}
	}
}

// TestAddBatchJSONBPathMatchesPlaceholderPath drives the real AddBatch entry
// point with the JSONB path active (default) and disabled (kill switch), and
// asserts identical stored rows plus identical re-add no-op semantics. This
// is the store-level guarantee that the cold-load fast path cannot change
// what a cold index persists.
func TestAddBatchJSONBPathMatchesPlaceholderPath(t *testing.T) {
	nodes, edges := jsonIngestExperimentFixture(300, 1200)

	jsonStorePath := filepath.Join(t.TempDir(), "jsonb.sqlite")
	jsonStore, err := Open(jsonStorePath)
	if err != nil {
		t.Fatal(err)
	}
	jsonStore.AddBatch(nodes, edges)
	// Re-adding the identical batch must be a no-op on the JSONB path too —
	// the upsert change-detection WHERE clause is shared with the
	// placeholder writer.
	stats, err := jsonStore.addBatchSetOriented(nodes, edges)
	if err != nil {
		t.Fatal(err)
	}
	if stats.nodeRowsChanged != 0 || stats.edgeRowsInserted != 0 {
		t.Fatalf("re-adding identical batch changed rows: %+v", stats)
	}
	if err := jsonStore.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GORTEX_SQLITE_JSONB_INGEST", "0")
	placeholderPath := filepath.Join(t.TempDir(), "placeholder.sqlite")
	placeholderStore, err := Open(placeholderPath)
	if err != nil {
		t.Fatal(err)
	}
	placeholderStore.AddBatch(nodes, edges)
	if err := placeholderStore.Close(); err != nil {
		t.Fatal(err)
	}

	verify, err := Open(jsonStorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer verify.Close()
	ctx := context.Background()
	conn, err := verify.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS reference`, placeholderPath); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `DETACH DATABASE reference`) }()

	nodeColumns := strings.Split(nodeInsertColumns, ", ")
	if err := compareAttachedIngestTable(ctx, conn, "nodes", nodeColumns, "a.id = b.id"); err != nil {
		t.Fatal(err)
	}
	edgeColumns := strings.Split(edgeInsertColumns, ", ")
	edgeIdentity := "a.from_id = b.from_id AND a.to_id = b.to_id AND a.kind = b.kind AND a.file_path = b.file_path AND a.line = b.line"
	if err := compareAttachedIngestTable(ctx, conn, "edges", edgeColumns, edgeIdentity); err != nil {
		t.Fatal(err)
	}
}

func resetIngestExperiment(store *Store) error {
	tx, err := store.writerDB.Begin()
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM edges`); err == nil {
		_, err = tx.Exec(`DELETE FROM nodes`)
	}
	if err == nil {
		_, err = tx.Exec(`DELETE FROM sqlite_sequence WHERE name = 'edges'`)
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func BenchmarkSQLiteIngestBindingStrategies(b *testing.B) {
	nodes, edges := jsonIngestExperimentFixture(12000, 60000)
	strategies := []struct {
		name string
		run  func(*sql.Tx, []*graph.Node, []*graph.Edge) (int, int, error)
	}{
		{name: "current_placeholders", run: ingestCurrentExperiment},
		{name: "jsonb_bounded", run: ingestJSONExperiment},
		{name: "jsonb_bounded_returning", run: ingestJSONReturningExperiment},
	}
	for _, strategy := range strategies {
		strategy := strategy
		b.Run(strategy.name, func(b *testing.B) {
			store, err := Open(filepath.Join(b.TempDir(), "graph.sqlite"))
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ReportMetric(float64(len(nodes)), "nodes/op")
			b.ReportMetric(float64(len(edges)), "edges/op")
			var nodeStatements, edgeStatements int
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if err := resetIngestExperiment(store); err != nil {
					b.Fatal(err)
				}
				tx, err := store.writerDB.Begin()
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				nodeStatements, edgeStatements, err = strategy.run(tx, nodes, edges)
				if err == nil {
					err = tx.Commit()
				} else {
					_ = tx.Rollback()
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(nodeStatements+edgeStatements), "sql_statements/op")
			if err := store.Close(); err != nil {
				b.Fatal(err)
			}
		})
	}
}
