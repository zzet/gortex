package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// TEST-ONLY DDL on disposable actual Store fixtures. Neither schemaSQL nor the
// production query is changed. Keep the existing edges_by_to intact.
const incomingCompositeResearchIndex = "gortex_research_incoming_positive"
const incomingCompositeResearchDDL = `CREATE INDEX gortex_research_incoming_positive
ON edges(view_gen, to_id, kind, id) WHERE view_gen > 0`

type incomingCompositeCase struct {
	name                         string
	generations, selected, noise int
	distinct, absent             bool
	edgesPerGeneration           int
}

// incomingCompositeCases is the research corpus. It is what the benchmarks in
// this file measure, and it is deliberately large: the access-cost question
// they exist to answer is only visible across dozens of retained generations
// and tens of thousands of same-generation unrelated endpoints.
var incomingCompositeCases = []incomingCompositeCase{
	{"single_distinct", 1, 0, 0, true, false, 1024},
	{"retained_distinct_newest", 32, 31, 0, true, false, 1024},
	{"retained_noisy_distinct_newest", 32, 31, 16_384, true, false, 1024},
	{"retained_noisy_distinct_oldest", 32, 0, 16_384, true, false, 1024},
	{"retained_noisy_repeated", 32, 31, 16_384, false, false, 1024},
	{"retained_noisy_absent", 32, 31, 16_384, true, true, 1024},
}

// incomingCompositeSmallCases carries every shape of the research corpus —
// single versus retained history, newest versus oldest selection, distinct
// versus repeated sources, present versus absent target, quiet versus noisy
// generation — at the smallest fixture that still separates them: more than one
// retained generation on each side of the selected one, and an order of
// magnitude more unrelated same-generation endpoints than selected rows, laid
// down before the selected rows in rowid order.
//
// The corpus size is not what any of the assertions in this file read. They
// read row budgets, page parity against the public Store path, and the plan of
// queries that name their index with INDEXED BY, and all three are the same at
// either size. Building the research corpus instead costs ~11 minutes under the
// race detector, so the tests run this one and the benchmarks keep the other.
var incomingCompositeSmallCases = []incomingCompositeCase{
	{"single_distinct", 1, 0, 0, true, false, 64},
	{"retained_distinct_newest", 8, 7, 0, true, false, 64},
	{"retained_noisy_distinct_newest", 8, 7, 1024, true, false, 64},
	{"retained_noisy_distinct_oldest", 8, 0, 1024, true, false, 64},
	{"retained_noisy_repeated", 8, 7, 1024, false, false, 64},
	{"retained_noisy_absent", 8, 7, 1024, true, true, 64},
}

// incomingCompositeTestCases is what the tests iterate. Set
// GORTEX_STORE_RESEARCH_CORPUS to re-run exactly the same assertions over the
// full research corpus.
func incomingCompositeTestCases() []incomingCompositeCase {
	if os.Getenv("GORTEX_STORE_RESEARCH_CORPUS") != "" {
		return incomingCompositeCases
	}
	return incomingCompositeSmallCases
}

type incomingCompositeBytes struct{ DB, WAL, SHM int64 }

func incomingCompositeFileBytes(tb testing.TB, path string) incomingCompositeBytes {
	tb.Helper()
	var out incomingCompositeBytes
	for _, item := range []struct {
		suffix string
		value  *int64
	}{{"", &out.DB}, {"-wal", &out.WAL}, {"-shm", &out.SHM}} {
		info, err := os.Stat(path + item.suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			tb.Fatal(err)
		}
		*item.value = info.Size()
	}
	if out.DB+out.WAL+out.SHM > 128<<20 {
		tb.Fatalf("research fixture exceeded128MiB: %+v", out)
	}
	return out
}

// incomingCompositeSeeds holds one built database per distinct case, so the
// arms that ask for the same corpus (the installed and uninstalled arms of one
// case, and the same case across tests) build it once and copy it afterwards.
// Every caller still gets a private database it may index, ANALYZE, checkpoint
// and write to.
type incomingCompositeSeed struct {
	path        string
	generations []int64
}

var (
	incomingCompositeSeedMu sync.Mutex
	incomingCompositeSeeded = map[incomingCompositeCase]incomingCompositeSeed{}
)

func incomingCompositeSeedFor(tb testing.TB, tc incomingCompositeCase) incomingCompositeSeed {
	tb.Helper()
	incomingCompositeSeedMu.Lock()
	defer incomingCompositeSeedMu.Unlock()
	// Labels and absent-target queries do not change the constructed corpus.
	// Reuse its closed seed while each caller still receives a private copy.
	key := tc
	key.name = ""
	key.absent = false
	if seed, ok := incomingCompositeSeeded[key]; ok {
		return seed
	}
	dir, err := os.MkdirTemp(packageScratch(tb), "composite-seed")
	if err != nil {
		tb.Fatal(err)
	}
	seed := incomingCompositeSeed{path: filepath.Join(dir, "incoming-composite-research.sqlite")}
	seed.generations = buildIncomingCompositeFixture(tb, tc, seed.path)
	incomingCompositeSeeded[key] = seed
	return seed
}

// This is the frozen Fa3 fixture's real Store/catalog construction with bounded
// unrelated endpoints inserted BEFORE publication in the selected generation.
func prepareIncomingCompositeFixture(tb testing.TB, tc incomingCompositeCase) incomingGenerationFanoutFixture {
	tb.Helper()
	if tc.generations < 1 || tc.generations > 32 || tc.selected < 0 || tc.selected >= tc.generations || tc.noise < 0 || tc.noise > 16_384 || tc.edgesPerGeneration < 1 || tc.edgesPerGeneration > 1024 {
		tb.Fatal("invalid bounded composite fixture")
	}
	seed := incomingCompositeSeedFor(tb, tc)
	path := filepath.Join(tb.TempDir(), "incoming-composite-research.sqlite")
	if err := copyStoreFile(seed.path, path); err != nil {
		tb.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	f := incomingGenerationFanoutFixture{control: s, path: path, target: "fanout/target.go::Target", edgesPerGeneration: tc.edgesPerGeneration, distinct: tc.distinct, generations: seed.generations}
	f.assertDiskBudget(tb)
	return f
}

// buildIncomingCompositeFixture populates one database at path and closes it,
// returning the generation ids it published.
func buildIncomingCompositeFixture(tb testing.TB, tc incomingCompositeCase, path string) []int64 {
	tb.Helper()
	s, err := openPristine(tb, path)
	if err != nil {
		tb.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = s.Close()
		}
	}()
	f := incomingGenerationFanoutFixture{control: s, path: path, target: "fanout/target.go::Target", edgesPerGeneration: tc.edgesPerGeneration, distinct: tc.distinct}
	s.AddBatch([]*graph.Node{{ID: "generation0-poison", Name: "Poison", RepoPrefix: "fanout", FilePath: "fanout/poison.go", Kind: graph.KindType}}, []*graph.Edge{{From: "generation0-poison", To: f.target, Kind: graph.EdgeKind("calls"), FilePath: "fanout/poison.go", Line: 1}})
	for generation := 0; generation < tc.generations; generation++ {
		id, err := s.Catalog().CreateViewGeneration(tb.Context(), ViewGeneration{OwnerKind: "dedicated_graph", GraphID: "incoming-composite-research", GenerationKind: "dedicated", TreeOID: fmt.Sprintf("source-%02d", generation), ConfigHash: "policy", State: ViewGenerationBuilding, CreatedAt: 1})
		if err != nil {
			tb.Fatal(err)
		}
		f.generations = append(f.generations, id)
		handle := s.AtGeneration(id)
		nodes := []*graph.Node{{ID: f.target, Name: "Target", RepoPrefix: "fanout", FilePath: "fanout/target.go", Kind: graph.KindType}}
		edges := make([]*graph.Edge, 0, f.edgesPerGeneration+tc.noise)
		for site := 0; site < f.edgesPerGeneration; site++ {
			source := 0
			if tc.distinct {
				source = site
			}
			from := incomingGenerationFanoutSource(generation, source)
			file := fmt.Sprintf("fanout/g%02d.go", generation)
			if tc.distinct || site == 0 {
				nodes = append(nodes, &graph.Node{ID: from, Name: "Source", RepoPrefix: "fanout", FilePath: file, Kind: graph.KindType})
			}
			edges = append(edges, &graph.Edge{From: from, To: f.target, Kind: graph.EdgeKind("calls"), FilePath: file, Line: site + 1})
		}
		if generation == tc.selected {
			for i := 0; i < tc.noise; i++ {
				target := fmt.Sprintf("fanout/noise.go::Target%05d", i)
				nodes = append(nodes, &graph.Node{ID: target, Name: "Noise", RepoPrefix: "fanout", FilePath: "fanout/noise.go", Kind: graph.KindType})
				edges = append(edges, &graph.Edge{From: incomingGenerationFanoutSource(generation, 0), To: target, Kind: graph.EdgeKind("calls"), FilePath: "fanout/noise.go", Line: i + 1})
			}
			// Put unrelated endpoints BEFORE the matching rows in rowid order.
			// This exposes the same-generation scan cost of forcing only the
			// existing (view_gen,id) index, including small distinct limits.
			ordered := make([]*graph.Edge, 0, len(edges))
			ordered = append(ordered, edges[f.edgesPerGeneration:]...)
			ordered = append(ordered, edges[:f.edgesPerGeneration]...)
			edges = ordered
		}
		handle.AddBatch(nodes, edges)
		if err := s.Catalog().PublishViewGeneration(tb.Context(), id, 2); err != nil {
			tb.Fatal(err)
		}
		if handle.EdgeCount() != len(edges) {
			tb.Fatal("fixture edge population changed")
		}
	}
	f.assertDiskBudget(tb)
	closed = true
	if err := s.Close(); err != nil {
		tb.Fatal(err)
	}
	return f.generations
}

func incomingCompositeDiagnostic(tb testing.TB, f incomingGenerationFanoutFixture) *sql.DB {
	tb.Helper()
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		tb.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	tb.Cleanup(func() { _ = db.Close() })
	return db
}

func incomingCompositeInstall(tb testing.TB, db *sql.DB, f incomingGenerationFanoutFixture) time.Duration {
	tb.Helper()
	before := incomingCompositeFileBytes(tb, f.path)
	started := time.Now()
	if _, err := db.ExecContext(tb.Context(), incomingCompositeResearchDDL); err != nil {
		tb.Fatal(err)
	}
	elapsed := time.Since(started)
	after := incomingCompositeFileBytes(tb, f.path)
	tb.Logf("untimed research index creation duration=%s before=%+v after=%+v; file lengths, NOT OS writes", elapsed, before, after)
	return elapsed
}

func incomingCompositeCheckpoint(tb testing.TB, db *sql.DB) {
	tb.Helper()
	var busy, logFrames, checkpointed int
	if err := db.QueryRowContext(tb.Context(), "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		tb.Fatal(err)
	}
	if busy != 0 {
		tb.Fatalf("fixture checkpoint busy=%d log=%d checkpointed=%d", busy, logFrames, checkpointed)
	}
}

// ANALYZE is performed on the private writer-capable diagnostic connection.
// Reopen the idle Store read connection OUTSIDE timing so its planner loads the
// resulting statistics rather than assuming another connection's state changed.
// All compared arms use the same single-reader-pool configuration.
func incomingCompositeRefreshReader(f incomingGenerationFanoutFixture) {
	f.control.db.SetMaxOpenConns(1)
	f.control.db.SetMaxIdleConns(0)
	f.control.db.SetMaxIdleConns(1)
}

type incomingCompositeQuery struct{ name, sql string }

func incomingCompositeQueries(tb testing.TB, installed bool) []incomingCompositeQuery {
	tb.Helper()
	if strings.Count(scopedIncomingSourcePageSQL, "FROM edges INDEXED BY edges_by_to") != 1 || strings.Count(scopedIncomingSourcePageSQL, "AND view_gen = ?") != 1 {
		tb.Fatal("known raw query changed; re-review research inputs")
	}
	positive := strings.Replace(scopedIncomingSourcePageSQL, "AND view_gen = ?", "AND view_gen = ? AND view_gen > 0", 1)
	queries := []incomingCompositeQuery{
		{"current_forced", scopedIncomingSourcePageSQL},
		{"positive_unforced", strings.Replace(positive, " INDEXED BY edges_by_to", "", 1)},
		{"positive_existing_generation", strings.Replace(positive, "edges_by_to", "edges_by_generation", 1)},
	}
	if installed {
		queries = append(queries, incomingCompositeQuery{"positive_new_composite", strings.Replace(positive, "edges_by_to", incomingCompositeResearchIndex, 1)})
	}
	return queries
}

// Same query-parameterized SQL-page probe for EVERY comparison arm. This uses
// the actual Store read pool and transaction, but is NOT a production dispatch
// or overlay benchmark. It reproduces the empty-scope raw page/sentinel/budget
// contract only. No candidate source or production replacement overlay is used.
func incomingCompositeReadProbe(ctx context.Context, s *Store, generation int64, target, query string) (graph.BoundedIncomingSourceProjection, int, error) {
	out := graph.BoundedIncomingSourceProjection{Sources: map[string][]string{}, Truncated: map[string]bool{}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.BoundedIncomingSourceProjection{}, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	seen := map[string]bool{}
	budget := &graph.IncomingSourceBudget{}
	after := int64(0)
	pageSize := 2
	var page []incomingSourcePageRow
	for {
		if err := ctx.Err(); err != nil {
			return graph.BoundedIncomingSourceProjection{}, 0, err
		}
		readLimit := min(pageSize, budget.Remaining()+1)
		rows, err := tx.QueryContext(ctx, query, target, "calls", after, generation, readLimit)
		if err != nil {
			return graph.BoundedIncomingSourceProjection{}, 0, err
		}
		page = page[:0]
		for rows.Next() {
			page = append(page, incomingSourcePageRow{})
			row := &page[len(page)-1]
			if err := rows.Scan(&row.id, &row.candidate.From, &row.candidate.FilePath); err != nil {
				_ = rows.Close()
				return graph.BoundedIncomingSourceProjection{}, 0, err
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return graph.BoundedIncomingSourceProjection{}, 0, err
		}
		if err := rows.Close(); err != nil {
			return graph.BoundedIncomingSourceProjection{}, 0, err
		}
		if err := budget.Charge(len(page)); err != nil {
			return graph.BoundedIncomingSourceProjection{}, 0, err
		}
		for _, row := range page {
			seen[row.candidate.From] = true
			if len(seen) > 1 {
				out.Truncated[target] = true
				break
			}
		}
		if out.Truncated[target] || len(page) < readLimit {
			break
		}
		after = page[len(page)-1].id
		pageSize = min(pageSize*2, maxIncomingSourcePageRows)
	}
	if !out.Truncated[target] {
		for source := range seen {
			out.Sources[target] = append(out.Sources[target], source)
		}
	}
	if err := tx.Commit(); err != nil {
		return graph.BoundedIncomingSourceProjection{}, 0, err
	}
	return out, graph.MaxIncomingSourceCandidateRows - budget.Remaining(), nil
}

func checkIncomingCompositeRead(tb testing.TB, f incomingGenerationFanoutFixture, tc incomingCompositeCase, target string, page graph.BoundedIncomingSourceProjection, charged int, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatal(err)
	}
	if tc.absent {
		if len(page.Sources) != 0 || len(page.Truncated) != 0 || charged != 0 {
			tb.Fatalf("absent target read other adjacency: %+v rows=%d", page, charged)
		}
		return
	}
	checkIncomingGenerationFanout(tb, f, tc.selected, page, nil)
	want := f.edgesPerGeneration
	if tc.distinct {
		want = 2
	}
	if charged != want {
		tb.Fatalf("selected-row count=%d want%d target=%s", charged, want, target)
	}
}

func TestIncomingCompositeIndexQueryParityIsolationAndPlans(t *testing.T) {
	for _, tc := range incomingCompositeTestCases() {
		for _, installed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/new_index_%t", tc.name, installed), func(t *testing.T) {
				f := prepareIncomingCompositeFixture(t, tc)
				diagnostic := incomingCompositeDiagnostic(t, f)
				if installed {
					incomingCompositeInstall(t, diagnostic, f)
				}
				target := f.target
				if tc.absent {
					target = "fanout/absent.go::NeverIndexed"
				}
				for _, analyzed := range []bool{false, true} {
					if analyzed {
						if _, err := diagnostic.ExecContext(t.Context(), "ANALYZE edges"); err != nil {
							t.Fatal(err)
						}
					}
					incomingCompositeRefreshReader(f)
					budget := &graph.IncomingSourceBudget{}
					public, err := f.control.AtGeneration(f.generations[tc.selected]).FindIncomingSourcesScoped(t.Context(), []string{target}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, budget)
					checkIncomingCompositeRead(t, f, tc, target, public, graph.MaxIncomingSourceCandidateRows-budget.Remaining(), err)
					for _, query := range incomingCompositeQueries(t, installed) {
						page, charged, err := incomingCompositeReadProbe(t.Context(), f.control, f.generations[tc.selected], target, query.sql)
						checkIncomingCompositeRead(t, f, tc, target, page, charged, err)
						if !reflect.DeepEqual(page, public) {
							t.Fatalf("query%s differs from actual public Store result: %+v versus%+v", query.name, page, public)
						}
						plan := incomingGenerationFanoutPlans(t, f.control.db, query.sql, target, f.generations[tc.selected])
						if query.name == "positive_new_composite" && (!strings.Contains(plan, incomingCompositeResearchIndex) || !strings.Contains(plan, "view_gen=?") || !strings.Contains(plan, "to_id=?") || !strings.Contains(plan, "kind=?") || !strings.Contains(plan, "id>?") || strings.Contains(plan, "TEMP B-TREE")) {
							t.Fatalf("composite point/range probe is not ordered and generation/endpoint-qualified: %s", plan)
						}
						t.Logf("installed=%t analyzed=%t query=%s plan=%s", installed, analyzed, query.name, plan)
					}
				}
				// The original index and actual gen0 public path survive new DDL.
				zero, err := f.control.FindIncomingSourcesBounded(t.Context(), []string{f.target}, graph.EdgeKind("calls"), 1)
				if err != nil || zero.Truncated[f.target] || !reflect.DeepEqual(zero.Sources[f.target], []string{"generation0-poison"}) {
					t.Fatalf("gen0 behavior changed: %+v %v", zero, err)
				}
				if installed {
					partial := incomingCompositeQueries(t, true)[3]
					zeroProbe, charged, err := incomingCompositeReadProbe(t.Context(), f.control, 0, f.target, partial.sql)
					if err != nil || charged != 0 || len(zeroProbe.Sources) != 0 {
						t.Fatalf("positive query admitted gen0: %+v %d %v", zeroProbe, charged, err)
					}
				}
				incomingCompositeCheckpoint(t, diagnostic)
				t.Logf("postcheckpoint fixture bytes=%+v", incomingCompositeFileBytes(t, f.path))
			})
		}
	}
}

var incomingCompositePageSink graph.BoundedIncomingSourceProjection
var incomingCompositeRowsSink int
var incomingCompositeErrorSink error

func BenchmarkIncomingCompositeCurrentPublicControl(b *testing.B) {
	tc := incomingCompositeCase{"public_current", 32, 31, 16_384, true, false, 1024}
	for _, installed := range []bool{false, true} {
		b.Run(fmt.Sprintf("new_index_%t", installed), func(b *testing.B) {
			f := prepareIncomingCompositeFixture(b, tc)
			diagnostic := incomingCompositeDiagnostic(b, f)
			if installed {
				incomingCompositeInstall(b, diagnostic, f)
			}
			for _, analyzed := range []bool{false, true} {
				if analyzed {
					if _, err := diagnostic.ExecContext(b.Context(), "ANALYZE edges"); err != nil {
						b.Fatal(err)
					}
				}
				incomingCompositeRefreshReader(f)
				b.Run(fmt.Sprintf("analyzed_%t", analyzed), func(b *testing.B) {
					handle := f.control.AtGeneration(f.generations[tc.selected])
					ids := []string{f.target}
					page, err := handle.FindIncomingSourcesScoped(b.Context(), ids, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, nil)
					checkIncomingGenerationFanout(b, f, tc.selected, page, err)
					failures := 0
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						page, err = handle.FindIncomingSourcesScoped(b.Context(), ids, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, nil)
						if err != nil {
							failures++
						}
					}
					b.StopTimer()
					incomingCompositePageSink, incomingCompositeErrorSink = page, err
					checkIncomingGenerationFanout(b, f, tc.selected, page, err)
					if failures != 0 {
						b.Fatalf("%d actual public Store controls failed", failures)
					}
				})
			}
		})
	}
}

func BenchmarkIncomingCompositeIndexSQLAccess(b *testing.B) {
	for _, tc := range incomingCompositeCases {
		b.Run(tc.name, func(b *testing.B) {
			for _, installed := range []bool{false, true} {
				b.Run(fmt.Sprintf("new_index_%t", installed), func(b *testing.B) {
					f := prepareIncomingCompositeFixture(b, tc)
					diagnostic := incomingCompositeDiagnostic(b, f)
					if installed {
						incomingCompositeInstall(b, diagnostic, f)
					}
					target := f.target
					if tc.absent {
						target = "fanout/absent.go::NeverIndexed"
					}
					for _, analyzed := range []bool{false, true} {
						if analyzed {
							if _, err := diagnostic.ExecContext(b.Context(), "ANALYZE edges"); err != nil {
								b.Fatal(err)
							}
						}
						incomingCompositeRefreshReader(f)
						for _, query := range incomingCompositeQueries(b, installed) {
							b.Run(fmt.Sprintf("analyzed_%t/%s", analyzed, query.name), func(b *testing.B) {
								page, charged, err := incomingCompositeReadProbe(b.Context(), f.control, f.generations[tc.selected], target, query.sql)
								checkIncomingCompositeRead(b, f, tc, target, page, charged, err)
								failures := 0
								b.ReportAllocs()
								b.ResetTimer()
								for i := 0; i < b.N; i++ {
									page, charged, err = incomingCompositeReadProbe(b.Context(), f.control, f.generations[tc.selected], target, query.sql)
									if err != nil {
										failures++
									}
								}
								b.StopTimer()
								incomingCompositePageSink, incomingCompositeRowsSink, incomingCompositeErrorSink = page, charged, err
								checkIncomingCompositeRead(b, f, tc, target, page, charged, err)
								if failures != 0 {
									b.Fatalf("%d timed access probes failed", failures)
								}
								b.ReportMetric(float64(charged), "selected-rows/op")
								b.ReportMetric(float64(tc.noise), "same-gen-unrelated-edges")
								b.ReportMetric(float64(tc.generations), "retained-generations")
							})
						}
					}
				})
			}
		})
	}
}

func TestIncomingCompositeIndexFailedQueriesAndGenZeroRefusal(t *testing.T) {
	f := prepareIncomingCompositeFixture(t, incomingCompositeTestCases()[0])
	diagnostic := incomingCompositeDiagnostic(t, f)
	incomingCompositeInstall(t, diagnostic, f)
	query := incomingCompositeQueries(t, true)[3]
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	page, charged, err := incomingCompositeReadProbe(ctx, f.control, f.generations[0], f.target, query.sql)
	if !errors.Is(err, context.Canceled) || charged != 0 || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("canceled probe returned partial certification: %+v %d %v", page, charged, err)
	}
	if _, err := diagnostic.ExecContext(t.Context(), "DROP INDEX "+incomingCompositeResearchIndex); err != nil {
		t.Fatal(err)
	}
	page, charged, err = incomingCompositeReadProbe(t.Context(), f.control, f.generations[0], f.target, query.sql)
	if err == nil || charged != 0 || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("missing required research index silently fell back: %+v %d %v", page, charged, err)
	}
	page, err = f.control.FindIncomingSourcesBounded(t.Context(), []string{f.target}, graph.EdgeKind("calls"), 1)
	if err != nil || !reflect.DeepEqual(page.Sources[f.target], []string{"generation0-poison"}) {
		t.Fatalf("dropping ONLY research index damaged old gen0 access: %+v %v", page, err)
	}
}

func incomingCompositeWriter(tb testing.TB, f incomingGenerationFanoutFixture, zero bool) (*Store, int64) {
	tb.Helper()
	if zero {
		return f.control, 0
	}
	id, err := f.control.Catalog().CreateViewGeneration(tb.Context(), ViewGeneration{OwnerKind: "dedicated_graph", GraphID: "incoming-composite-writer", GenerationKind: "dedicated", TreeOID: "writer-source", ConfigHash: "policy", State: ViewGenerationBuilding, CreatedAt: 1})
	if err != nil {
		tb.Fatal(err)
	}
	return f.control.AtGeneration(id), id
}

func incomingCompositeEdgeBatch(offset, count int) []*graph.Edge {
	edges := make([]*graph.Edge, count)
	for i := range edges {
		edges[i] = &graph.Edge{From: "writer/source.go::Source", To: "writer/target.go::Target", Kind: graph.EdgeKind("calls"), FilePath: "writer/source.go", Line: offset + i + 1}
	}
	return edges
}

func incomingCompositeWriterCount(tb testing.TB, f incomingGenerationFanoutFixture, generation int64) int {
	tb.Helper()
	var count int
	if err := f.control.db.QueryRowContext(tb.Context(), `SELECT COUNT(*) FROM edges WHERE view_gen=? AND from_id=?`, generation, "writer/source.go::Source").Scan(&count); err != nil {
		tb.Fatal(err)
	}
	return count
}

func TestIncomingCompositeIndexActualAddBatchReplayAndNodeUpdate(t *testing.T) {
	for _, installed := range []bool{false, true} {
		for _, zero := range []bool{false, true} {
			t.Run(fmt.Sprintf("new_index_%t/gen0_%t", installed, zero), func(t *testing.T) {
				f := prepareIncomingCompositeFixture(t, incomingCompositeTestCases()[0])
				diagnostic := incomingCompositeDiagnostic(t, f)
				if installed {
					incomingCompositeInstall(t, diagnostic, f)
				}
				writer, generation := incomingCompositeWriter(t, f, zero)
				edges := incomingCompositeEdgeBatch(0, 256)
				node := &graph.Node{ID: "writer/source.go::Source", Name: "Before", RepoPrefix: "writer", FilePath: "writer/source.go", Kind: graph.KindType}
				writer.AddBatch([]*graph.Node{node}, edges)
				if got := incomingCompositeWriterCount(t, f, generation); got != len(edges) {
					t.Fatalf("insert rows=%d", got)
				}
				for range 3 {
					writer.AddBatch([]*graph.Node{node}, edges)
				}
				if got := incomingCompositeWriterCount(t, f, generation); got != len(edges) {
					t.Fatalf("idempotent replay multiplied edges=%d", got)
				}
				updated := *node
				updated.Name = "After"
				writer.AddBatch([]*graph.Node{&updated}, nil)
				got := writer.GetNode(node.ID)
				if got == nil || got.Name != "After" || incomingCompositeWriterCount(t, f, generation) != len(edges) {
					t.Fatalf("node update/control failed: %+v", got)
				}
				before := incomingCompositeFileBytes(t, f.path)
				incomingCompositeCheckpoint(t, diagnostic)
				after := incomingCompositeFileBytes(t, f.path)
				t.Logf("before checkpoint=%+v after=%+v; replay equality is semantic, NOT zero physical writes", before, after)
			})
		}
	}
}

// Use fixed -benchtime=100x (or another N<=256). Fresh inserts are bounded to
// 65,536 rows per leaf. Every arm prepares identical batches OUTSIDE timing.
// Replay and node-update controls measure actual AddBatch, not direct SQL.
func BenchmarkIncomingCompositeIndexActualAddBatch(b *testing.B) {
	for _, installed := range []bool{false, true} {
		for _, zero := range []bool{false, true} {
			for _, operation := range []string{"insert_edges", "replay_edges", "update_node_control"} {
				b.Run(fmt.Sprintf("new_index_%t/gen0_%t/%s", installed, zero, operation), func(b *testing.B) {
					if b.N > 256 {
						b.Fatal("bounded write benchmark requires fixed benchtime<=256x")
					}
					f := prepareIncomingCompositeFixture(b, incomingCompositeCases[0])
					diagnostic := incomingCompositeDiagnostic(b, f)
					if installed {
						incomingCompositeInstall(b, diagnostic, f)
					}
					writer, generation := incomingCompositeWriter(b, f, zero)
					nodes := []*graph.Node{
						{ID: "writer/source.go::Source", Name: "Even", RepoPrefix: "writer", FilePath: "writer/source.go", Kind: graph.KindType},
						{ID: "writer/source.go::Source", Name: "Odd", RepoPrefix: "writer", FilePath: "writer/source.go", Kind: graph.KindType},
					}
					writer.AddBatch([]*graph.Node{nodes[1]}, nil)
					batches := make([][]*graph.Edge, b.N)
					for i := range batches {
						switch operation {
						case "insert_edges":
							batches[i] = incomingCompositeEdgeBatch(i*256, 256)
						case "replay_edges":
							if i == 0 {
								batches[0] = incomingCompositeEdgeBatch(0, 256)
							}
							batches[i] = batches[0]
						}
					}
					if operation == "replay_edges" {
						writer.AddBatch(nil, batches[0])
					}
					incomingCompositeCheckpoint(b, diagnostic)
					before := incomingCompositeFileBytes(b, f.path)
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if operation == "update_node_control" {
							writer.AddBatch([]*graph.Node{nodes[i%2]}, nil)
						} else {
							writer.AddBatch(nil, batches[i])
						}
					}
					b.StopTimer()
					mid := incomingCompositeFileBytes(b, f.path)
					want := 0
					switch operation {
					case "insert_edges":
						want = b.N * 256
					case "replay_edges":
						want = 256
					}
					if got := incomingCompositeWriterCount(b, f, generation); got != want {
						b.Fatalf("AddBatch rows=%d want%d", got, want)
					}
					if operation == "update_node_control" {
						got := writer.GetNode(nodes[0].ID)
						if got == nil || got.Name != nodes[(b.N-1)%2].Name {
							b.Fatalf("node update lost: %+v", got)
						}
					}
					incomingCompositeCheckpoint(b, diagnostic)
					after := incomingCompositeFileBytes(b, f.path)
					b.ReportMetric(float64(mid.DB-before.DB), "precheckpoint-db-growth-B")
					b.ReportMetric(float64(mid.WAL), "precheckpoint-wal-B")
					b.ReportMetric(float64(mid.SHM), "precheckpoint-shm-B")
					b.ReportMetric(float64(after.DB-before.DB), "postcheckpoint-db-growth-B")
					b.ReportMetric(float64(after.WAL), "postcheckpoint-wal-B")
					b.ReportMetric(float64(after.SHM), "postcheckpoint-shm-B")
					b.ReportMetric(float64(after.DB), "final-db-B")
					b.ReportMetric(float64(want), "final-writer-edges")
				})
			}
		}
	}
}
