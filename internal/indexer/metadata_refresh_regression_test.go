package indexer

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func metadataRegressionNodes() []*graph.Node {
	return []*graph.Node{
		{ID: "sample.go", Kind: graph.KindFile, Name: "sample.go", FilePath: "sample.go"},
		{ID: "sample.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "sample.go"},
		{ID: "sample.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "sample.go"},
		{ID: "sample.go::Z", Kind: graph.KindFunction, Name: "Z", FilePath: "sample.go"},
	}
}

func metadataRegressionRefresh(mode string, g graph.Store, nodes []*graph.Node, edges []*graph.Edge) ([]graph.EdgeReindex, bool) {
	fresh := make([]*graph.Node, len(nodes))
	ids := make([]string, 0, len(nodes))
	existing := make(map[string]struct{}, len(nodes))
	for i, node := range nodes {
		copyNode := *node
		fresh[i] = &copyNode
		ids = append(ids, node.ID)
		existing[node.ID] = struct{}{}
	}
	if mode == "point" {
		return metadataEdgeRefreshes(g, "sample.go", nodes, fresh, edges)
	}
	stage := &incrementalBatchStage{
		graphPath: "sample.go", priorNodes: nodes,
		result: &parser.ExtractionResult{Nodes: fresh, Edges: edges},
	}
	ok := prepareMetadataRefreshFromView(stage, g.GetOutEdgesByNodeIDs(ids), existing)
	return stage.edgeRefreshes, ok
}

// This is a candidate-level regression, not an assertion that splitting a Go
// function body is classified metadata-only. The real classifier is tested below.
func TestMetadataRefreshMatchesTargetBeforeLocation(t *testing.T) {
	for _, mode := range []string{"point", "batch"} {
		t.Run(mode, func(t *testing.T) {
			g := graph.New()
			nodes := metadataRegressionNodes()
			g.AddBatch(nodes, []*graph.Edge{
				{From: "sample.go::Caller", To: "sample.go::A", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 10, Origin: "resolver-a", Confidence: 0.8},
				{From: "sample.go::Caller", To: "sample.go::Z", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 10, Origin: "resolver-z", Confidence: 0.6},
			})
			updates, ok := metadataRegressionRefresh(mode, g, nodes, []*graph.Edge{
				{From: "sample.go::Caller", To: "sample.go::Z", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 10},
				{From: "sample.go::Caller", To: "sample.go::A", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 11},
			})
			require.True(t, ok)
			require.Len(t, updates, 2)
			for _, update := range updates {
				switch update.Edge.To {
				case "sample.go::A":
					require.Equal(t, 11, update.Edge.Line)
					require.EqualValues(t, "resolver-a", update.Edge.Origin)
					require.InDelta(t, 0.8, update.Edge.Confidence, 0.0001)
				case "sample.go::Z":
					require.Equal(t, 10, update.Edge.Line)
					require.EqualValues(t, "resolver-z", update.Edge.Origin)
					require.InDelta(t, 0.6, update.Edge.Confidence, 0.0001)
				default:
					t.Fatalf("unexpected target %q", update.Edge.To)
				}
			}
		})
	}
}

func TestMetadataRefreshRefusesAmbiguousOrChangedTargets(t *testing.T) {
	for _, mode := range []string{"point", "batch"} {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/changed=%v", mode, changed), func(t *testing.T) {
				g := graph.New()
				nodes := metadataRegressionNodes()
				second := "sample.go::A"
				if changed {
					second = "sample.go::Z"
				}
				g.AddBatch(nodes, []*graph.Edge{
					{From: "sample.go::Caller", To: "sample.go::A", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 10},
					{From: "sample.go::Caller", To: second, Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 20},
				})
				fresh := []*graph.Edge{{From: "sample.go::Caller", To: "sample.go::A", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 11}}
				if !changed {
					fresh = append(fresh, &graph.Edge{From: "sample.go::Caller", To: "sample.go::A", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 21})
				}
				updates, ok := metadataRegressionRefresh(mode, g, nodes, fresh)
				require.False(t, ok, "ambiguous or changed target groups must use structural reindex")
				require.Empty(t, updates)
			})
		}
	}
}

func TestMetadataRefreshPreservesForeignFileRows(t *testing.T) {
	for _, mode := range []string{"point", "batch"} {
		t.Run(mode, func(t *testing.T) {
			g, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, g.Close()) })
			nodes := metadataRegressionNodes()
			foreign := &graph.Edge{From: "sample.go::Caller", To: "sample.go::A", Kind: graph.EdgeCalls, FilePath: "foreign.go", Line: 77, Origin: "foreign-resolver", Confidence: 0.7}
			g.AddBatch(nodes, []*graph.Edge{
				{From: "sample.go::Caller", To: "sample.go::A", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 10},
				foreign,
			})
			updates, ok := metadataRegressionRefresh(mode, g, nodes, []*graph.Edge{
				{From: "sample.go::Caller", To: "sample.go::A", Kind: graph.EdgeCalls, FilePath: "sample.go", Line: 11},
			})
			require.True(t, ok)
			require.Len(t, updates, 1)
			g.ReindexEdges(updates)
			var seen bool
			for _, edge := range g.GetOutEdgesByNodeIDs([]string{"sample.go::Caller"})["sample.go::Caller"] {
				if edge.FilePath == "foreign.go" {
					require.Equal(t, foreign, edge)
					seen = true
				}
			}
			require.True(t, seen, "refresh must retain rows owned by another file")
		})
	}
}

// Embed the concrete store so optional SQLite capabilities are not lost merely
// to install this test observer. These counters measure attempted API rows,
// not SQL mutations; a separate untimed SQL trigger probe measures the latter.
type metadataRecordingStore struct {
	*store_sqlite.Store
	addNodes     atomic.Int64
	addEdges     atomic.Int64
	refreshEdges atomic.Int64
}

func (s *metadataRecordingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addNodes.Add(int64(len(nodes)))
	s.addEdges.Add(int64(len(edges)))
	s.Store.AddBatch(nodes, edges)
}
func (s *metadataRecordingStore) ReindexEdges(edges []graph.EdgeReindex) {
	s.refreshEdges.Add(int64(len(edges)))
	s.Store.ReindexEdges(edges)
}
func (s *metadataRecordingStore) resetAttempts() {
	s.addNodes.Store(0)
	s.addEdges.Store(0)
	s.refreshEdges.Store(0)
}

type metadataProductionFixture struct {
	root, path, dbPath string
	idx                *Indexer
	store              *metadataRecordingStore
}

func newMetadataProductionFixture(tb testing.TB, source string, prime bool) metadataProductionFixture {
	tb.Helper()
	root := tb.TempDir()
	path := filepath.Join(root, "sample.go")
	require.NoError(tb, os.WriteFile(path, []byte(source), 0600))
	require.NoError(tb, os.WriteFile(filepath.Join(root, "other.go"), []byte("package sample\nfunc Foreign() { A() }\n"), 0600))
	dbPath := filepath.Join(tb.TempDir(), "graph.db")
	raw, err := store_sqlite.Open(dbPath)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, raw.Close()) })
	store := &metadataRecordingStore{Store: raw}
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(store, reg, cfg.Index, zap.NewNop())
	_, err = idx.Index(root)
	require.NoError(tb, err)
	if prime {
		require.NoError(tb, os.WriteFile(path, []byte("// primed\n"+source), 0600))
		require.NoError(tb, idx.IndexFile(path))
	}
	return metadataProductionFixture{root: root, path: path, dbPath: dbPath, idx: idx, store: store}
}
func (f metadataProductionFixture) reindex(tb testing.TB, source string) *IndexResult {
	tb.Helper()
	require.NoError(tb, os.WriteFile(f.path, []byte(source), 0600))
	result, err := f.idx.IncrementalReindexPaths(f.root, []string{f.path})
	require.NoError(tb, err)
	require.NotNil(tb, result)
	return result
}
func metadataFileCallCoordinates(g graph.Store, file string) []string {
	var ids []string
	for _, node := range g.GetFileNodes(file) {
		ids = append(ids, node.ID)
	}
	var rows []string
	for _, edges := range g.GetOutEdgesByNodeIDs(ids) {
		for _, edge := range edges {
			if edge.Kind == graph.EdgeCalls && edge.FilePath == file {
				rows = append(rows, fmt.Sprintf("%s|%s|%s|%d|%s", edge.From, edge.To, edge.FilePath, edge.Line, edge.Alias))
			}
		}
	}
	sort.Strings(rows)
	return rows
}

const metadataUniqueCalls = "package sample\n\nfunc A() {}\nfunc Z() {}\nfunc Caller() { Z(); A() }\n"
const metadataRepeatedCalls = "package sample\n\nfunc A() {}\nfunc Z() {}\nfunc Caller() {\n A()\n A()\n}\n"

func TestMetadataRefreshProductionPresentationOnly(t *testing.T) {
	f := newMetadataProductionFixture(t, metadataUniqueCalls, true)
	foreign := metadataFileCallCoordinates(f.store, "other.go")
	require.NotEmpty(t, foreign)
	updated := "// layout-only\n\n// primed\n" + metadataUniqueCalls
	result := f.reindex(t, updated)
	require.Greater(t, result.DerivedInvalidation.MetadataOnlyFiles, 0, "outside-body edit must exercise the actual metadata fast path")
	// The cold oracle compares source-owned call coordinates, not every derived
	// graph field. Resolver provenance is asserted separately by the helper test.
	oracle := newMetadataProductionFixture(t, updated, false)
	require.Equal(t, metadataFileCallCoordinates(oracle.store, "sample.go"), metadataFileCallCoordinates(f.store, "sample.go"))
	require.Equal(t, foreign, metadataFileCallCoordinates(f.store, "other.go"))
}

func TestMetadataRefreshProductionAmbiguousCallsFallBack(t *testing.T) {
	f := newMetadataProductionFixture(t, metadataRepeatedCalls, true)
	updated := "// layout-only\n\n// primed\n" + metadataRepeatedCalls
	result := f.reindex(t, updated)
	require.Zero(t, result.DerivedInvalidation.MetadataOnlyFiles, "repeated target identity has no stable callsite pairing")
	oracle := newMetadataProductionFixture(t, updated, false)
	require.Equal(t, metadataFileCallCoordinates(oracle.store, "sample.go"), metadataFileCallCoordinates(f.store, "sample.go"))
}

// Count trigger-visible nodes/edges INSERT/UPDATE/DELETE events in one
// untimed refresh. Counter writes, implicit REPLACE deletes, and internal
// index/FTS work are not counted. This is not total SQLite or disk-write volume.
func metadataPayloadRowProbe(tb testing.TB, f metadataProductionFixture, source string) (int64, int64) {
	tb.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	require.NoError(tb, err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE metadata_refresh_write_probe (name TEXT PRIMARY KEY, n INTEGER NOT NULL); INSERT INTO metadata_refresh_write_probe VALUES ('nodes',0),('edges',0)")
	require.NoError(tb, err)
	var names []string
	for _, table := range []string{"nodes", "edges"} {
		for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
			name := "metadata_probe_" + table + "_" + strings.ToLower(operation)
			names = append(names, name)
			_, err = db.Exec(fmt.Sprintf("CREATE TRIGGER %s AFTER %s ON %s BEGIN UPDATE metadata_refresh_write_probe SET n=n+1 WHERE name='%s'; END", name, operation, table, table))
			require.NoError(tb, err)
		}
	}
	f.reindex(tb, source)
	var nodes, edges int64
	require.NoError(tb, db.QueryRow("SELECT n FROM metadata_refresh_write_probe WHERE name='nodes'").Scan(&nodes))
	require.NoError(tb, db.QueryRow("SELECT n FROM metadata_refresh_write_probe WHERE name='edges'").Scan(&edges))
	for _, name := range names {
		_, err = db.Exec("DROP TRIGGER " + name)
		require.NoError(tb, err)
	}
	_, err = db.Exec("DROP TABLE metadata_refresh_write_probe")
	require.NoError(tb, err)
	return nodes, edges
}

func BenchmarkMetadataRefreshProduction(b *testing.B) {
	for _, kind := range []string{"presentation", "structural", "ambiguous"} {
		b.Run(kind, func(b *testing.B) {
			initial := metadataUniqueCalls
			if kind == "ambiguous" {
				initial = metadataRepeatedCalls
			}
			f := newMetadataProductionFixture(b, initial, true)
			variants := []string{"// primed\n" + initial, "// layout-only\n\n// primed\n" + initial}
			if kind == "structural" {
				variants[1] = strings.ReplaceAll(variants[0], "Z(); A()", "A(); A()")
			}
			nodes, edges := metadataPayloadRowProbe(b, f, variants[1])
			// Reprepare statements after removing the probe before timing begins.
			f.reindex(b, variants[0])
			f.store.resetAttempts()
			metadataFiles := 0
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result := f.reindex(b, variants[1-i%2])
				metadataFiles += result.DerivedInvalidation.MetadataOnlyFiles
				if kind == "presentation" && result.DerivedInvalidation.MetadataOnlyFiles == 0 {
					b.Fatal("presentation benchmark did not exercise metadata-only refresh")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(nodes), "nodes-row-events/probe")
			b.ReportMetric(float64(edges), "edges-row-events/probe")
			b.ReportMetric(float64(f.store.addNodes.Load())/float64(b.N), "AddBatch-node-attempts/op")
			b.ReportMetric(float64(f.store.addEdges.Load())/float64(b.N), "AddBatch-edge-attempts/op")
			b.ReportMetric(float64(f.store.refreshEdges.Load())/float64(b.N), "ReindexEdges-attempts/op")
			b.ReportMetric(float64(metadataFiles)/float64(b.N), "metadata-files/op")
		})
	}
}
