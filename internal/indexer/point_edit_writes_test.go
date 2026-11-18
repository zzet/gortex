package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

type pointEditWriteFixture struct {
	root    string
	store   *store_sqlite.Store
	passive *sql.DB
	mi      *MultiIndexer
	sources map[string]string
}

// A deterministic hub has 200 definitions and 1,000 incoming calls from twenty
// small leaf files. Everything is generated in TempDir; no user corpus is read
// or changed. All edits use the production point executor including its tails.
func newPointEditWriteFixture(tb testing.TB, logger *zap.Logger) *pointEditWriteFixture {
	tb.Helper()
	f := &pointEditWriteFixture{root: tb.TempDir(), sources: make(map[string]string)}
	var hub strings.Builder
	hub.WriteString("package fixture\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&hub, "func Hub%03d() int { return 1 }\n", i)
	}
	f.sources["hub.go"] = hub.String()
	for i := 0; i < 20; i++ {
		var leaf strings.Builder
		fmt.Fprintf(&leaf, "package fixture\nfunc Leaf%02d() int { return ", i)
		for j := 0; j < 50; j++ {
			if j > 0 {
				leaf.WriteString(" + ")
			}
			fmt.Fprintf(&leaf, "Hub%03d()", (i*50+j)%200)
		}
		leaf.WriteString(" }\n")
		f.sources[fmt.Sprintf("leaf%02d.go", i)] = leaf.String()
	}
	for name, source := range f.sources {
		require.NoError(tb, os.WriteFile(filepath.Join(f.root, name), []byte(source), 0600))
	}
	path := filepath.Join(tb.TempDir(), "writes.sqlite")
	var err error
	f.store, err = store_sqlite.Open(path)
	require.NoError(tb, err)
	tb.Cleanup(func() { require.NoError(tb, f.store.Close()) })
	cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "fixture", Path: f.root}}}
	configPath := filepath.Join(tb.TempDir(), "config.yaml")
	cfg.SetConfigPath(configPath)
	require.NoError(tb, cfg.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(tb, err)
	f.mi = NewMultiIndexer(f.store, newTestRegistry(), search.NewNull(), manager, logger)
	tb.Cleanup(func() { require.NoError(tb, f.mi.Close(context.Background())) })
	results, err := f.mi.IndexAll()
	require.NoError(tb, err)
	require.Empty(tb, results["fixture"].FailedFiles)
	f.passive, err = sql.Open("sqlite", path)
	require.NoError(tb, err)
	f.passive.SetMaxOpenConns(1)
	tb.Cleanup(func() { require.NoError(tb, f.passive.Close()) })
	f.checkpoint(tb)
	return f
}
func (f *pointEditWriteFixture) checkpoint(tb testing.TB) {
	tb.Helper()
	var busy, frames, copied int
	require.NoError(tb, f.passive.QueryRow(`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &frames, &copied))
	require.Zero(tb, busy)
	require.Equal(tb, frames, copied, "all readers of the isolated fixture must have returned")
	require.NoError(tb, f.store.CheckpointWAL()) // writer connection TRUNCATE
}
func (f *pointEditWriteFixture) edit(tb testing.TB, name, text string) {
	tb.Helper()
	require.NoError(tb, os.WriteFile(filepath.Join(f.root, name), []byte(text), 0600))
	result, err := f.mi.incrementalPointRepoRaw("fixture", filepath.Join(f.root, name))
	require.NoError(tb, err)
	require.Empty(tb, result.FailedFiles)
}
func pointEditText(source, target, edit string) string {
	switch edit {
	case "noop":
		return source
	case "body":
		return strings.Replace(source, "return ", "return 2 + ", 1)
	case "add_func":
		return source + "\nfunc AddedPointEdit() int { return 3 }\n"
	case "rename":
		name := "Hub000"
		if target == "leaf" {
			name = "Leaf00"
		}
		return strings.Replace(source, "func "+name+"(", "func RenamedPointEdit(", 1)
	case "shift":
		return "// point edit shifts source locations\n" + source
	default:
		panic("unknown edit " + edit)
	}
}

func BenchmarkPointEditWrites(b *testing.B) {
	for _, target := range []string{"hub", "leaf"} {
		for _, edit := range []string{"noop", "body", "add_func", "rename", "shift"} {
			b.Run(target+"/"+edit, func(b *testing.B) {
				f := newPointEditWriteFixture(b, zap.NewNop())
				name := "hub.go"
				if target == "leaf" {
					name = "leaf00.go"
				}
				original := f.sources[name]
				modified := pointEditText(original, target, edit)
				// Measure the named forward edit only. Restore/checkpoint the
				// original outside both the timed interval and counter samples.
				b.ResetTimer()
				b.StopTimer()
				var totals store_sqlite.VFSIOSplit
				for i := 0; i < b.N; i++ {
					before := store_sqlite.WriteMark()
					b.StartTimer()
					f.edit(b, name, modified)
					f.checkpoint(b)
					d := store_sqlite.WriteMark().Since(before)
					b.StopTimer()
					totals = addVFSIOSplit(totals, d)
					f.edit(b, name, original)
					f.checkpoint(b)
				}
				d := totals
				b.ReportMetric(float64(d.WALBytes())/float64(b.N), "wal_bytes/op")
				b.ReportMetric(float64(d.WriterMainWriteBytes+d.OtherMainWriteBytes)/float64(b.N), "main_bytes/op")
				b.ReportMetric(float64(d.Rows.Total())/float64(b.N), "rows/op")
				b.ReportMetric(float64(d.Transactions)/float64(b.N), "txns/op")
			})
		}
	}
}

func TestPointEditRowBudget(t *testing.T) {
	f := newPointEditWriteFixture(t, zap.NewNop())
	hubNodes := f.store.GetFileNodes("fixture/hub.go")
	require.GreaterOrEqual(t, len(hubNodes), 200)
	ids := make([]string, 0, len(hubNodes))
	for _, n := range hubNodes {
		ids = append(ids, n.ID)
	}
	incoming := 0
	for _, edges := range f.store.GetInEdgesByNodeIDs(ids) {
		for _, e := range edges {
			if e.Kind == graph.EdgeCalls {
				incoming++
			}
		}
	}
	require.Equal(t, 1000, incoming)
	for _, name := range []string{"hub.go", "leaf00.go"} {
		t.Run(name, func(t *testing.T) {
			before := store_sqlite.WriteMark()
			f.edit(t, name, f.sources[name])
			require.Zero(t, store_sqlite.WriteMark().Since(before).Rows.Total(), "same bytes must not rewrite payload rows")
			before = store_sqlite.WriteMark()
			f.edit(t, name, pointEditText(f.sources[name], "", "add_func"))
			rows := store_sqlite.WriteMark().Since(before).Rows.Total()
			t.Logf("add_func direct rows: %d", rows)
			f.edit(t, name, f.sources[name])
			f.checkpoint(t)
			// First isolated baseline on Apple M5 Pro: hub 5607, leaf 292
			// direct rows. Rounded ceilings leave small schema bookkeeping
			// headroom while catching a return to broader row rewrites.
			ceiling := int64(6000)
			if name == "leaf00.go" {
				ceiling = 350
			}
			require.LessOrEqual(t, rows, ceiling)
		})
	}
}

func TestPointEditWriteTelemetryFieldsAndLineBudget(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	f := newPointEditWriteFixture(t, zap.New(core))
	logs.TakeAll()
	w, err := NewWatcher(f.mi.indexers["fixture"], config.WatchConfig{}, zap.New(core))
	require.NoError(t, err)
	w.pointReindexRaw = func(path string, _ *PublicationStamps) (*IndexResult, error) {
		return f.mi.incrementalPointRepoRaw("fixture", path)
	}
	source := pointEditText(f.sources["hub.go"], "hub", "add_func")
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "hub.go"), []byte(source), 0600))
	require.NoError(t, w.patchGraphAfterReceiptCheck(filepath.Join(f.root, "hub.go"), ChangeModified))
	seen := map[string]int{}
	for _, entry := range logs.All() {
		m := entry.ContextMap()
		prefix := ""
		switch entry.Message {
		case "indexer: reconcile phase complete", "resolver: incremental phase complete":
			prefix = "phase_"
		case "dataflow: params materialized":
			prefix = "dataflow_"
		case "graph patch complete":
			require.Contains(t, m, "incident_edges")
		case "indexer: graph apply steps":
			for key := range m {
				if strings.HasSuffix(key, "_ms") && !strings.Contains(key, "cpu") && !strings.Contains(key, "wait") {
					step := strings.TrimSuffix(key, "_ms")
					if _, ok := m[step+"_faults"]; ok {
						assertWriteFields(t, m, step+"_")
					}
				}
			}
			seen[entry.Message]++
			continue
		case "incremental derived passes complete":
			for _, family := range []string{"hierarchy", "tests", "capability", "framework", "external_calls", "cross_repo", "contracts"} {
				assertWriteFields(t, m, family+"_")
			}
			seen[entry.Message]++
			continue
		default:
			continue
		}
		assertWriteFields(t, m, prefix)
		seen[entry.Message]++
	}
	for _, message := range []string{"indexer: reconcile phase complete", "resolver: incremental phase complete", "indexer: graph apply steps", "dataflow: params materialized", "incremental derived passes complete", "graph patch complete"} {
		require.Positive(t, seen[message], message)
	}
	// No new info event was added: set-reindex detail is inline on these phases.
	require.Equal(t, 1, seen["graph patch complete"])
	require.Zero(t, logs.FilterMessage("store_sqlite: reindex writes").Len())
}
func assertWriteFields(t *testing.T, fields map[string]any, prefix string) {
	t.Helper()
	for _, suffix := range []string{"wal_bytes", "main_bytes", "rows", "txns"} {
		require.Contains(t, fields, prefix+suffix)
	}
}
