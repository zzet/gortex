package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestVisitNodesByNamesInRepoContextExactParityAndGeneration(t *testing.T) {
	s, _ := openTempStore(t)
	for _, gen := range []int64{0, 41} {
		var nodes []*graph.Node
		for _, repo := range []string{"", "a", "b"} {
			for i, name := range []string{"Match", "Match", "match", "", "bad\ufffd", "raw placeholder"} {
				id := fmt.Sprintf("%s/file.go::%d", repo, i)
				nodes = append(nodes, &graph.Node{ID: id, Name: name, Kind: graph.KindFunction, RepoPrefix: repo, FilePath: repo + "/file.go", Meta: map[string]any{"doc": "complete fields", "generation": gen}})
			}
		}
		require.NoError(t, s.AtGeneration(gen).AddBatchChecked(nodes, nil))
		// Bypass JSON's malformed-UTF replacement when constructing the exact
		// raw TEXT fixture; the replacement neighbor remains a distinct row.
		for _, repo := range []string{"", "a", "b"} {
			_, err := s.writerDB.Exec(`UPDATE nodes SET name=? WHERE view_gen=? AND id=?`, "bad\xff", gen, fmt.Sprintf("%s/file.go::5", repo))
			require.NoError(t, err)
		}
	}
	h := s.AtGeneration(41)
	for _, repo := range []string{"", "a", "missing"} {
		for _, names := range [][]string{{"Match"}, {"Match", "match", "", "missing", "Match"}, {"bad\xff", "Match", ""}} {
			var want, got []*graph.Node
			require.NoError(t, h.VisitNodesByNamesContext(t.Context(), names, func(n *graph.Node) bool {
				if n.RepoPrefix == repo {
					want = append(want, n)
				}
				return true
			}))
			require.NoError(t, h.VisitNodesByNamesInRepoContext(t.Context(), names, repo, func(n *graph.Node) bool { got = append(got, n); return true }))
			require.Equal(t, want, got, "exact row fields and per-name order, including unowned/empty/raw names")
			if names[0] == "bad\xff" && repo != "missing" {
				var raw []*graph.Node
				for _, node := range got {
					if node.Name == "bad\xff" {
						raw = append(raw, node)
					}
				}
				require.Len(t, raw, 1, "malformed UTF-8 must match the actual raw row, not only miss its replacement neighbor")
				require.Equal(t, fmt.Sprintf("%s/file.go::5", repo), raw[0].ID)
			}
		}
	}
	seen := 0
	require.NoError(t, h.VisitNodesByNamesInRepoContext(t.Context(), []string{"Match", "match"}, "a", func(*graph.Node) bool { seen++; return false }))
	require.Equal(t, 1, seen)
	ctx, cancel := context.WithCancel(t.Context())
	require.ErrorIs(t, h.VisitNodesByNamesInRepoContext(ctx, []string{"Match"}, "a", func(*graph.Node) bool { cancel(); return true }), context.Canceled)
}

func TestVisitNodesByNamesInRepoContextPreservesReadErrorsAndWaitCancellation(t *testing.T) {
	s, _ := openTempStore(t)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "a/broken", Name: "Match", RepoPrefix: "a", Kind: graph.KindFunction}}, nil))
	_, err := s.writerDB.Exec(`UPDATE nodes SET start_line='broken' WHERE id='a/broken'`)
	require.NoError(t, err)
	require.Error(t, s.VisitNodesByNamesInRepoContext(t.Context(), []string{"Match"}, "a", func(*graph.Node) bool { t.Fatal("invalid selected row decoded"); return true }))
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	before := s.db.Stats().WaitCount
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			_ = conn.Close()
			select {
			case <-done:
				joined = true
			case <-time.After(time.Second):
				t.Error("repo visitor waiter did not join during cleanup")
			}
		}
	})
	go func() {
		done <- s.VisitNodesByNamesInRepoContext(ctx, []string{"missing"}, "a", func(*graph.Node) bool { return true })
	}()
	deadline := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == before && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if s.db.Stats().WaitCount == before {
		t.Fatal("repo visitor did not wait on its reader")
	}
	cancel()
	select {
	case err := <-done:
		joined = true
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled reader wait did not return")
	}
	require.NoError(t, conn.Close())
	require.NoError(t, s.db.Close())
	require.Error(t, s.VisitNodesByNamesInRepoContext(t.Context(), []string{"Match"}, "a", func(*graph.Node) bool { return true }))
}

func TestVisitNodesByNamesInRepoContextKeepsSeekPlans(t *testing.T) {
	s := openPayloadStore(t)
	small := smallStoreStats(t, s)
	_, _, _ = keyListGraph(t, s, 200)
	for _, state := range keyListStatsStates(small) {
		state.apply(t, s)
		for _, plan := range []string{planOf(t, s, repoNamesSeekSQL, `["Match",""]`, s.viewGen, "repo"), planOf(t, s, repoExactRawNameSeekSQL, "bad\xff", s.viewGen, "repo")} {
			t.Logf("%s: %s", state.name, plan)
			require.Contains(t, plan, "nodes_by_name (name=? AND view_gen=?)")
			require.NotContains(t, plan, "SCAN nodes")
			require.NotContains(t, strings.ToUpper(plan), "TEMP B-TREE")
		}
	}
}

func BenchmarkRepoExactNameVisitorDecodedRows(b *testing.B) {
	s, err := openPristine(b, filepath.Join(b.TempDir(), "names.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	var nodes []*graph.Node
	for repo := range 8 {
		prefix := fmt.Sprintf("repo%d", repo)
		for i := range 64 {
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s/f.go::%d", prefix, i), Name: "Shared", RepoPrefix: prefix, Kind: graph.KindFunction, FilePath: prefix + "/f.go", Meta: map[string]any{"doc": strings.Repeat("payload ", 32)}})
		}
	}
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		b.Fatal(err)
	}
	for _, scoped := range []bool{false, true} {
		label := "legacy_global_decode"
		if scoped {
			label = "SQL_repo_filter"
		}
		b.Run(label, func(b *testing.B) {
			decoded := 0
			b.ReportAllocs()
			for range b.N {
				matched := 0
				for repo := range 8 {
					prefix := fmt.Sprintf("repo%d", repo)
					visit := func(n *graph.Node) bool {
						decoded++
						if n.RepoPrefix == prefix {
							matched++
						}
						return true
					}
					var err error
					if scoped {
						err = s.VisitNodesByNamesInRepoContext(context.Background(), []string{"Shared"}, prefix, visit)
					} else {
						err = s.VisitNodesByNamesContext(context.Background(), []string{"Shared"}, visit)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				if matched != len(nodes) {
					b.Fatalf("owned matches=%d want%d", matched, len(nodes))
				}
			}
			b.ReportMetric(float64(decoded)/float64(b.N), "decoded_rows/op")
		})
	}
}
