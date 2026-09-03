package reach

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// topologyScopeWait bounds the positive assertion below. The happy path never
// spends it — a writer that is not queued behind anything arrives at once.
const topologyScopeWait = 5 * time.Second

func openTopologyScopeStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "scope.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestWritesBaseTopologyFollowsThePinnedGeneration pins the property the gate
// now keys on. A handle that reports no generation is the base corpus, because
// taking the gate for a mutation that did not need it costs concurrency while
// skipping it for one that did costs correctness.
func TestWritesBaseTopologyFollowsThePinnedGeneration(t *testing.T) {
	store := openTopologyScopeStore(t)
	cases := []struct {
		name  string
		store graph.Store
		want  bool
	}{
		{"absent", nil, false},
		{"base corpus", store, true},
		{"base handle", store.AtGeneration(0), true},
		{"derived generation", store.AtGeneration(7), false},
		{"reports no generation", graph.New(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WritesBaseTopology(tc.store); got != tc.want {
				t.Fatalf("WritesBaseTopology = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGenerationMutationLeavesTheWriterFree is the gate half: a mutation over a
// derived generation must not make a base writer queue, and must not retire the
// records a base reader is holding.
func TestGenerationMutationLeavesTheWriterFree(t *testing.T) {
	store := openTopologyScopeStore(t)
	before := BuildCounter()

	releaseGeneration := BeginTopologyMutation(store.AtGeneration(7))
	admitted := make(chan func(bool), 1)
	go func() { admitted <- BeginTopologyMutation(store) }()
	select {
	case releaseBase := <-admitted:
		releaseBase(false)
	case <-time.After(topologyScopeWait):
		releaseGeneration(true)
		t.Fatal("a base topology writer queued behind a generation mutation")
	}
	releaseGeneration(true)

	if after := BuildCounter(); after != before {
		t.Fatalf("a generation mutation moved the reach build counter from %d to %d", before, after)
	}
}
