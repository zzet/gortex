package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type reposCountingStore struct {
	*graph.Graph
	reads atomic.Int64
}

func (s *reposCountingStore) RepoStats() map[string]graph.GraphStats {
	s.reads.Add(1)
	return s.Graph.RepoStats()
}

func TestReposWarmRequestsShareAggregate(t *testing.T) {
	g := &reposCountingStore{Graph: graph.New()}
	g.AddNode(&graph.Node{ID: "fixture::main", Kind: graph.KindFunction, RepoPrefix: "fixture", Language: "go"})
	h := &Handler{graph: g}
	for range 2 {
		w := httptest.NewRecorder()
		h.handleRepos(w, httptest.NewRequest("GET", "/v1/repos", nil))
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["snapshot_age_ms"]; !ok {
			t.Fatal("snapshot age missing")
		}
	}
	if n := g.reads.Load(); n != 1 {
		t.Fatalf("aggregate reads=%d, want 1", n)
	}
}

type reposBlockingStore struct {
	*graph.Graph
	reads    atomic.Int64
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (s *reposBlockingStore) RepoStatsContext(ctx context.Context) (map[string]graph.GraphStats, error) {
	s.reads.Add(1)
	s.started <- struct{}{}
	select {
	case <-s.release:
		return s.RepoStats(), nil
	case <-ctx.Done():
		s.canceled <- struct{}{}
		return nil, ctx.Err()
	}
}

func TestReposConcurrentRefreshAndAbandonedCancellation(t *testing.T) {
	s := &reposBlockingStore{Graph: graph.New(), started: make(chan struct{}, 16), canceled: make(chan struct{}, 16), release: make(chan struct{})}
	var cache repoSnapshotCache
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, _, err := cache.get(context.Background(), s, nil); err != nil {
				t.Error(err)
			}
		})
	}
	<-s.started
	close(s.release)
	wg.Wait()
	if s.reads.Load() != 1 {
		t.Fatalf("concurrent refreshes=%d", s.reads.Load())
	}
	cache.mu.Lock()
	cache.collected = time.Now().Add(-2 * repoSnapshotTTL)
	cache.mu.Unlock()
	s.release = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := cache.get(ctx, s, nil); done <- err }()
	<-s.started
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("error=%v", err)
	}
	select {
	case <-s.canceled:
	case <-time.After(time.Second):
		t.Fatal("abandoned aggregate query was not canceled")
	}
}

func BenchmarkReposLargeSyntheticStore(b *testing.B) {
	s, err := store_sqlite.Open(filepath.Join(b.TempDir(), "repos.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	const count = 100000
	nodes := make([]*graph.Node, 0, count)
	edges := make([]*graph.Edge, 0, count*5)
	for i := range count {
		repo := fmt.Sprintf("fixture/repo%03d", i%100)
		id := fmt.Sprintf("%s::function%d", repo, i)
		nodes = append(nodes, &graph.Node{ID: id, RepoPrefix: repo, FilePath: repo + "/main.go", Kind: graph.KindFunction, Language: "go"})
		for line := range 5 {
			edges = append(edges, &graph.Edge{From: id, To: "unresolved::callee", Kind: graph.EdgeCalls, Line: line + 1})
		}
	}
	if err := s.AddBatchChecked(nodes, edges); err != nil {
		b.Fatal(err)
	}
	h := &Handler{graph: s}
	for _, warm := range []bool{false, true} {
		b.Run(fmt.Sprintf("warm=%t", warm), func(b *testing.B) {
			if warm {
				h.handleRepos(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/repos", nil))
			}
			b.ResetTimer()
			for range b.N {
				if !warm {
					h.repoSnapshot = repoSnapshotCache{}
				}
				w := httptest.NewRecorder()
				h.handleRepos(w, httptest.NewRequest("GET", "/v1/repos", nil))
				if w.Code != 200 {
					b.Fatal(w.Body.String())
				}
			}
		})
	}
}
