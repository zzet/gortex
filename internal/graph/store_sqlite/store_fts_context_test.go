package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type symbolSearchContextAPI interface {
	SearchSymbolsContext(context.Context, string, int) ([]graph.SymbolHit, error)
	SearchSymbolsRepoScopedContext(context.Context, string, []string, int) ([]graph.SymbolHit, error)
}

func newSymbolSearchContextStore(t *testing.T) (*Store, symbolSearchContextAPI) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "symbols.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})

	nodes := []*graph.Node{
		{ID: "repo-a::AlphaWorker", Kind: graph.KindFunction, Name: "AlphaWorker", RepoPrefix: "repo-a"},
		{ID: "repo-b::BetaWorker", Kind: graph.KindFunction, Name: "BetaWorker", RepoPrefix: "repo-b"},
	}
	if err := s.AddBatchChecked(nodes, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.BulkUpsertSymbolFTS("repo-a", []graph.SymbolFTSItem{{NodeID: nodes[0].ID, Tokens: "alpha worker search"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.BulkUpsertSymbolFTS("repo-b", []graph.SymbolFTSItem{{NodeID: nodes[1].ID, Tokens: "beta worker search"}}); err != nil {
		t.Fatal(err)
	}

	api, ok := any(s).(symbolSearchContextAPI)
	if !ok {
		t.Fatal("Store does not expose context-aware symbol search")
	}
	return s, api
}

func TestSearchSymbolsContextPreservesRankingScopeAndCancellation(t *testing.T) {
	_, api := newSymbolSearchContextStore(t)

	legacyStore := any(api).(*Store)
	legacy, err := legacyStore.SearchSymbolsRepoScoped("worker search", []string{"repo-a"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.SearchSymbolsRepoScopedContext(context.Background(), "worker search", []string{"repo-a"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, legacy) {
		t.Fatalf("context results = %#v, legacy = %#v", got, legacy)
	}
	if len(got) != 1 || got[0].NodeID != "repo-a::AlphaWorker" {
		t.Fatalf("scoped hits = %#v", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := api.SearchSymbolsContext(ctx, "worker search", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled search error = %v, want context.Canceled", err)
	}
}

func TestSearchSymbolsContextCancelsBlockedExactAndFTSQueries(t *testing.T) {
	s, api := newSymbolSearchContextStore(t)
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)

	for _, tc := range []struct {
		name string
		run  func(context.Context) error
	}{
		{
			name: "exact_name",
			run: func(ctx context.Context) error {
				_, err := api.SearchSymbolsContext(ctx, "AlphaWorker", 10)
				return err
			},
		},
		{
			name: "fts",
			run: func(ctx context.Context) error {
				_, err := api.SearchSymbolsRepoScopedContext(ctx, "worker search", []string{"repo-a"}, 10)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertBlockedSymbolSearchCancels(t, s, tc.run)
		})
	}
}

func assertBlockedSymbolSearchCancels(t *testing.T, s *Store, run func(context.Context) error) {
	t.Helper()
	blocker, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	blockerOpen := true
	defer func() {
		if blockerOpen {
			_ = blocker.Close()
		}
	}()

	before := s.db.Stats().WaitCount
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- run(ctx)
	}()

	waitDeadline := time.Now().Add(2 * time.Second)
	for s.db.Stats().WaitCount == before && time.Now().Before(waitDeadline) {
		time.Sleep(time.Millisecond)
	}
	if s.db.Stats().WaitCount == before {
		cancel()
		_ = blocker.Close()
		blockerOpen = false
		select {
		case <-result:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("symbol search never waited for the held reader connection")
	}

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked search error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		_ = blocker.Close()
		blockerOpen = false
		select {
		case <-result:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("blocked symbol search did not return after cancellation")
	}
	if err := blocker.Close(); err != nil {
		t.Fatal(err)
	}
	blockerOpen = false

	reuseCtx, reuseCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer reuseCancel()
	if err := run(reuseCtx); err != nil {
		t.Fatalf("reader pool was not reusable after cancellation: %v", err)
	}
}
