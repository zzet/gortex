package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func foldedNameCandidateBenchmarkFixture(b testing.TB) (*Store, int) {
	s := openNodeSearchKeyTestStore(b)
	stopStoreBackgroundReaders(s)
	const count = 20000
	nodes := make([]*graph.Node, 0, count+128)
	for i := range count {
		name := fmt.Sprintf("OrdinarySymbol%05d", i)
		if i%400 == 0 {
			name = fmt.Sprintf("NarrowTarget%05d", i)
		}
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("ascii-%05d", i), Name: name,
			Kind: graph.KindFunction, FilePath: "pkg/file.go", RepoPrefix: "repo",
			WorkspaceID: "workspace", ProjectID: "project"})
	}
	for i := range 128 {
		name := fmt.Sprintf("ΩAuxiliary%03d", i)
		if i%2 == 0 {
			name = fmt.Sprintf("Before\x00Auxiliary%03d", i)
		}
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("unsafe-%03d", i), Name: name,
			Kind: graph.KindFunction, FilePath: "pkg/file.go", RepoPrefix: "repo",
			WorkspaceID: "workspace", ProjectID: "project"})
	}
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		b.Fatal(err)
	}
	return s, len(nodes)
}

// Real SQLite compact selection only, not hydration/MCP. Fixture construction
// and readTx setup are untimed. Empty RepoAllow makes the identical test-only
// Accept counter correspond to one invocation per successfully decoded row.
func BenchmarkFoldedNameCandidatePrefilter(b *testing.B) {
	s, fixtureRows := foldedNameCandidateBenchmarkFixture(b)
	for _, needle := range []string{"target", "absent-query-xyz"} {
		plan := foldedPrefilterPlan(b, s, needle)
		b.Logf("actual unchanged-fixture candidate plan: %s", plan)
		if !strings.Contains(plan, "USING COVERING INDEX nodes_name_candidates") {
			b.Fatal("existing covering index not selected in actual benchmark fixture")
		}
	}
	type selector func(context.Context, *sql.Tx, int64, string, int, graph.NameSearchFilter) ([]graph.NodeSearchKey, bool, error)
	for _, needle := range []string{"target", "absent-query-xyz"} {
		for _, arm := range []struct {
			name string
			run  selector
		}{{"Before", selectFoldedNameCandidatesBeforePrefilter}, {"After", selectFoldedNameCandidates}} {
			b.Run(needle+"/"+arm.name, func(b *testing.B) {
				b.StopTimer()
				tx, err := s.db.BeginTx(b.Context(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					b.Fatal(err)
				}
				want, wantMore, err := selectFoldedNameCandidatesBeforePrefilter(b.Context(), tx, s.viewGen, needle, 10, graph.NameSearchFilter{})
				if err != nil {
					_ = tx.Rollback()
					b.Fatal(err)
				}
				decoded := 0
				filter := graph.NameSearchFilter{Accept: func(*graph.Node) bool { decoded++; return true }}
				var got []graph.NodeSearchKey
				var more bool
				b.ReportAllocs()
				b.ResetTimer()
				b.StartTimer()
				for range b.N {
					got, more, err = arm.run(b.Context(), tx, s.viewGen, needle, 10, filter)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				if closeErr := tx.Rollback(); closeErr != nil {
					b.Fatal(closeErr)
				}
				if !reflect.DeepEqual(got, want) || more != wantMore {
					b.Fatal("benchmark semantic parity failed")
				}
				b.ReportMetric(float64(decoded)/float64(b.N), "decoded-rows/op")
				b.ReportMetric(float64(fixtureRows), "fixture-rows")
			})
		}
	}
}
