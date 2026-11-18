package tstypes

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/semantic"
)

// Confirming a target is not enough: one caller can call that target at
// several source sites. Neither insertion order nor SQLite's query plan may
// decide which site receives the fact's confidence and provider attribution.
func TestCallConfirmationKeepsRequestedCallsiteProvenance(t *testing.T) {
	for _, sqlite := range []bool{false, true} {
		for _, full := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				for _, strategy := range []resolutionStrategy{strategyDirect, strategyInferred} {
					for _, action := range []string{"confirm", "adopt", "create"} {
						strategyName := string(strategy)
						if strategy == strategyDirect {
							strategyName = "direct"
						}
						name := fmt.Sprintf("sqlite=%v/full=%v/reverse=%v/%s/%s", sqlite, full, reverse, strategyName, action)
						t.Run(name, func(t *testing.T) {
							const file = "App.java"
							caller := &graph.Node{ID: file + "::App.handle", Name: "handle", Kind: graph.KindMethod, FilePath: file}
							target := &graph.Node{ID: file + "::Svc.run", Name: "run", Kind: graph.KindMethod, FilePath: file}
							edge := func(to string, line int) *graph.Edge {
								return &graph.Edge{From: caller.ID, To: to, Kind: graph.EdgeCalls, FilePath: file,
									Line: line, Origin: graph.OriginASTInferred, Confidence: 0.4}
							}
							edges := []*graph.Edge{edge(target.ID, 3)}
							switch action {
							case "confirm":
								edges = append(edges, edge(target.ID, 30))
							case "adopt":
								edges = append(edges, edge("unresolved::run", 30))
							}
							if reverse && len(edges) == 2 {
								edges[0], edges[1] = edges[1], edges[0]
							}

							var s graph.Store = graph.New()
							dbPath := filepath.Join(t.TempDir(), "calls.sqlite")
							if sqlite {
								disk, err := store_sqlite.Open(dbPath)
								if err != nil {
									t.Fatal(err)
								}
								s = disk
							}
							s.AddBatch([]*graph.Node{caller, target}, edges)
							reopen := func(analyze bool) {
								t.Helper()
								if !sqlite {
									return
								}
								if err := s.(*store_sqlite.Store).Close(); err != nil {
									t.Fatal(err)
								}
								if analyze {
									db, err := sql.Open("sqlite", dbPath)
									if err != nil {
										t.Fatal(err)
									}
									_, err = db.Exec("ANALYZE")
									closeErr := db.Close()
									if err != nil {
										t.Fatal(err)
									}
									if closeErr != nil {
										t.Fatal(closeErr)
									}
								}
								disk, err := store_sqlite.Open(dbPath)
								if err != nil {
									t.Fatal(err)
								}
								s = disk
							}
							reopen(true)
							t.Cleanup(func() {
								if sqlite {
									if err := s.(*store_sqlite.Store).Close(); err != nil {
										t.Error(err)
									}
								}
							})

							ap := newApplier(s, JavaSpec(), "java-types")
							if full {
								ap.setAdjacencyKinds(nil, nil)
							}
							// Exercise the production batch shape after ordinary planner
							// statistics and reopen, not a forced SQLite index.
							ids := []string{caller.ID, target.ID}
							for len(ids) < 5000 {
								ids = append(ids, fmt.Sprintf("missing::%04d", len(ids)))
							}
							ap.loadAdjacency(ids)
							ap.loadCalls(ids)
							confidence := astConfidence
							if strategy == strategyInferred {
								confidence = inferredConfidence
							}
							res := &semantic.EnrichResult{}
							ap.upgradeOrCreateCall(caller, target, callFact{line: 30, method: "run"}, file, res, strategy, confidence)
							// Repeating the same fact in this applier must see the
							// confirmed/adopted/minted edge in its live calls list.
							repeated := &semantic.EnrichResult{}
							ap.upgradeOrCreateCall(caller, target, callFact{line: 30, method: "run"}, file, repeated, strategy, confidence)
							if repeated.EdgesAdded != 0 || repeated.EdgesConfirmed != 0 {
								t.Errorf("same-site reapplication added=%d confirmed=%d, want no changes",
									repeated.EdgesAdded, repeated.EdgesConfirmed)
							}
							reopen(false)

							calls := s.GetOutEdges(caller.ID)
							if len(calls) != 2 {
								t.Fatalf("got %d calls, want two distinct source sites", len(calls))
							}
							seen := map[int]bool{}
							for _, e := range calls {
								seen[e.Line] = true
								if e.To != target.ID {
									t.Errorf("line %d: target = %q, want %q", e.Line, e.To, target.ID)
								}
								if e.Line == 30 {
									if e.Origin != graph.OriginASTResolved || e.Confidence != confidence || metaVal(e) != "java-types" {
										t.Errorf("requested site did not persist resolved provenance: %+v", e)
									}
									if strategy == strategyInferred && e.Meta["resolution_strategy"] != string(strategyInferred) {
										t.Errorf("requested site lost inferred strategy: %+v", e)
									}
								} else if e.Line != 3 || e.Origin != graph.OriginASTInferred || e.Confidence != 0.4 || metaVal(e) != nil {
									t.Errorf("unobserved site changed: %+v", e)
								}
							}
							if !seen[3] || !seen[30] {
								t.Errorf("source sites = %v, want lines 3 and 30", seen)
							}
							if action == "create" {
								if res.EdgesAdded != 1 || res.EdgesConfirmed != 0 {
									t.Errorf("create result: %+v", res)
								}
							} else if res.EdgesConfirmed != 1 || res.EdgesAdded != 0 {
								t.Errorf("confirmation result: %+v", res)
							}
						})
					}
				}
			}
		}
	}
}
