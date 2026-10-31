package reach

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const (
	seedA = "a/a.go::A"
	seedB = "b/b.go::B"
)

// repoTopologyStore holds repositories a and b, one caller-callee pair in a and
// one function in b, plus the extra edges given.
func repoTopologyStore(t *testing.T, extra ...*graph.Edge) *store_sqlite.Store {
	t.Helper()
	store := openTopologyScopeStore(t)
	store.AddBatch([]*graph.Node{
		{ID: seedA, Kind: graph.KindFunction, Name: "A", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "a/a.go::CallsA", Kind: graph.KindFunction, Name: "CallsA", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: seedB, Kind: graph.KindFunction, Name: "B", FilePath: "b/b.go", RepoPrefix: "b"},
		{ID: "dep::x::X", Kind: graph.KindFunction, Name: "X"},
	}, append([]*graph.Edge{
		{From: "a/a.go::CallsA", To: seedA, Kind: graph.EdgeCalls, Confidence: 1},
	}, extra...))
	return store
}

// awaitCoupling waits until done holds for the coupling of g.
func awaitCoupling(t *testing.T, g graph.Store, done func(*repoCoupling) bool) {
	t.Helper()
	deadline := time.Now().Add(topologyScopeWait)
	for {
		topology.mu.Lock()
		ok := topology.coupling.store == g && done(&topology.coupling)
		topology.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the repository coupling read never landed")
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitRepoCoupling reads the coupling of g and waits until it is known.
func awaitRepoCoupling(t *testing.T, g graph.Store) {
	t.Helper()
	ensureRepoCoupling(g)
	awaitCoupling(t, g, func(c *repoCoupling) bool { return c.ready })
}

// admitted reports whether begin returns within topologyScopeWait.
func admitted[F any](begin func() F) (F, bool) {
	done := make(chan F, 1)
	go func() { done <- begin() }()
	select {
	case finish := <-done:
		return finish, true
	case <-time.After(topologyScopeWait):
		var none F
		return none, false
	}
}

func lookupWithin(t *testing.T, g graph.Store, seed string, within time.Duration) bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() {
		_, _, _, hit, truncated := LookupContext(context.Background(), g, seed)
		done <- hit && !truncated
	}()
	select {
	case exact := <-done:
		if !exact {
			t.Fatalf("lookup of %s was not an exact hit", seed)
		}
		return true
	case <-time.After(within):
		return false
	}
}

func TestRepoTopologyMutationsHoldOnlyTheirRepository(t *testing.T) {
	store := repoTopologyStore(t)
	awaitRepoCoupling(t, store)
	finishA := BeginRepoTopologyMutation(store, "a")

	finishB, ok := admitted(func() func(bool, []string) { return BeginRepoTopologyMutation(store, "b") })
	if !ok {
		finishA(false, nil)
		t.Fatal("a mutation of b queued behind a mutation of a")
	}
	finishB(false, nil)
	if !lookupWithin(t, store, seedB, topologyScopeWait) {
		finishA(false, nil)
		t.Fatal("a lookup in b waited for a mutation of a")
	}

	blocked := make(chan bool, 1)
	go func() {
		_, _, _, hit, truncated := LookupContext(context.Background(), store, seedA)
		blocked <- hit && !truncated
	}()
	whole := make(chan func(bool), 1)
	go func() { whole <- BeginTopologyMutation(store) }()
	select {
	case <-blocked:
		t.Fatal("a lookup in a escaped a mutation of a")
	case <-whole:
		t.Fatal("a whole-graph mutation entered beside a mutation of a")
	case <-time.After(20 * time.Millisecond):
	}
	finishA(true, []string{"a/a.go"})
	finishWhole := <-whole
	finishWhole(false)
	select {
	case exact := <-blocked:
		if !exact {
			t.Fatal("the lookup released by the mutation was not an exact hit")
		}
	case <-time.After(topologyScopeWait):
		t.Fatal("a lookup in a stayed blocked after the mutation finished")
	}
}

func TestRepoTopologyMutationRetiresCoupledRecords(t *testing.T) {
	bFiles := []string{"b/b.go"}
	cases := []struct {
		name     string
		extra    []*graph.Edge
		bind     []*graph.Edge // bound inside the mutation of b
		prefix   string
		frontier []string
		retireA  bool
	}{
		{name: "independent", prefix: "b", frontier: bFiles},
		{name: "joined by an edge", prefix: "b", frontier: bFiles, retireA: true, extra: []*graph.Edge{
			{From: seedB, To: seedA, Kind: graph.EdgeCalls, Confidence: 1},
		}},
		{name: "joined through an unowned node", prefix: "b", frontier: bFiles, retireA: true, extra: []*graph.Edge{
			{From: seedB, To: "dep::x::X", Kind: graph.EdgeCalls, Confidence: 1},
			{From: "dep::x::X", To: seedA, Kind: graph.EdgeValueFlow, Confidence: 1},
		}},
		{name: "joined by a reference the mutation bound", prefix: "b", frontier: bFiles, retireA: true, bind: []*graph.Edge{
			{From: seedB, To: seedA, Kind: graph.EdgeReferences, Confidence: 1},
		}},
		{name: "joined through an unowned node the mutation reached", prefix: "b", frontier: bFiles, retireA: true,
			extra: []*graph.Edge{{From: "dep::x::X", To: seedA, Kind: graph.EdgeValueFlow, Confidence: 1}},
			bind:  []*graph.Edge{{From: seedB, To: "dep::x::X", Kind: graph.EdgeCalls, Confidence: 1}},
		},
		{name: "edges anywhere", prefix: "b", retireA: true},
		{name: "unknown repository", prefix: "", frontier: bFiles, retireA: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := repoTopologyStore(t, tc.extra...)
			awaitRepoCoupling(t, store)
			BuildIndex(store)
			finish := BeginRepoTopologyMutation(store, tc.prefix)
			if tc.bind != nil {
				store.AddBatch(nil, tc.bind)
			}
			finish(true, tc.frontier)

			if _, _, _, hit, _ := LookupCached(store, seedB); hit {
				t.Fatal("the mutated repository kept its records")
			}
			if _, _, _, hit, _ := LookupCached(store, seedA); hit == tc.retireA {
				t.Fatalf("record of a hit=%v after a mutation of %q, want %v", hit, tc.prefix, !tc.retireA)
			}
		})
	}
}

// An unowned node that gains its first edge into a repository joins it with
// every repository that has an edge into the node.
func TestRepoCouplingLearnsAnUnownedNodeTurnedHub(t *testing.T) {
	store := repoTopologyStore(t, &graph.Edge{From: seedA, To: "dep::x::X", Kind: graph.EdgeCalls, Confidence: 1})
	awaitRepoCoupling(t, store)
	finish := BeginRepoTopologyMutation(store, "b")
	store.AddBatch(nil, []*graph.Edge{{From: "dep::x::X", To: seedB, Kind: graph.EdgeValueFlow, Confidence: 1}})
	finish(true, []string{"b/b.go"})

	BuildIndex(store)
	finish = BeginRepoTopologyMutation(store, "a")
	store.AddBatch(
		[]*graph.Node{{ID: "a/c.go::C", Kind: graph.KindFunction, Name: "C", FilePath: "a/c.go", RepoPrefix: "a"}},
		[]*graph.Edge{{From: "a/c.go::C", To: seedA, Kind: graph.EdgeCalls, Confidence: 1}},
	)
	finish(true, []string{"a/c.go"})
	if _, _, _, hit, _ := LookupCached(store, seedB); hit {
		t.Fatal("a new caller in a reaches b through an unowned node, but the records of b were kept")
	}
}

// A whole-graph mutation can join repositories anywhere, so the coupling is
// read again before a mutation of one repository narrows.
func TestWholeGraphMutationForgetsRepoCoupling(t *testing.T) {
	store := repoTopologyStore(t)
	awaitRepoCoupling(t, store)

	finish := BeginTopologyMutation(store)
	store.AddBatch(nil, []*graph.Edge{{From: seedB, To: seedA, Kind: graph.EdgeCalls, Confidence: 1}})
	finish(true)

	awaitRepoCoupling(t, store)
	BuildIndex(store)
	BeginRepoTopologyMutation(store, "b")(true, []string{})
	if _, _, _, hit, _ := LookupCached(store, seedA); hit {
		t.Fatal("a mutation of b kept the records of a after an edge joined them")
	}
}

// Without a coupling source every repository mutation holds the whole graph.
func TestRepoTopologyMutationWithoutCouplingHoldsTheWholeGraph(t *testing.T) {
	g, ids := newCallChain(t, 3)
	BuildIndex(g)
	BeginRepoTopologyMutation(g, "b")(true, []string{})
	if _, _, _, hit, _ := LookupCached(g, ids[2]); hit {
		t.Fatal("a repository mutation of a store without coupling kept unrelated records")
	}
}

// slowCouplingStore holds its coupling read until release and then reports no
// pairs, or err.
type slowCouplingStore struct {
	*store_sqlite.Store
	release chan struct{}
	err     error
	reads   atomic.Int32
}

func (s *slowCouplingStore) RepoCouplingPairs(ctx context.Context) ([][2]string, error) {
	s.reads.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil, s.err
}

// A mutation never waits for the coupling read; it holds the whole graph
// until the read lands, and what it bound meanwhile is added to the read's
// result, whose snapshot may predate it.
func TestRepoTopologyMutationDoesNotWaitForTheCouplingRead(t *testing.T) {
	store := &slowCouplingStore{Store: repoTopologyStore(t), release: make(chan struct{})}
	started := time.Now()
	finish := BeginRepoTopologyMutation(store, "b")
	store.AddBatch(nil, []*graph.Edge{{From: seedB, To: seedA, Kind: graph.EdgeReferences, Confidence: 1}})
	finish(true, []string{"b/b.go"})
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("a mutation waited %s for the coupling read", waited)
	}

	close(store.release)
	awaitCoupling(t, store, func(c *repoCoupling) bool { return c.ready })
	BuildIndex(store)
	BeginRepoTopologyMutation(store, "b")(true, []string{})
	if _, _, _, hit, _ := LookupCached(store, seedA); hit {
		t.Fatal("the coupling lost an edge bound while it was being read")
	}
}

// A store whose coupling read fails, as one that runs out of budget does,
// keeps whole-graph mutations and is not read again.
func TestFailedCouplingReadIsNotRetried(t *testing.T) {
	store := &slowCouplingStore{Store: repoTopologyStore(t), release: make(chan struct{}), err: context.DeadlineExceeded}
	BeginRepoTopologyMutation(store, "b")(false, nil)
	close(store.release)
	awaitCoupling(t, store, func(c *repoCoupling) bool { return c.missed })

	BuildIndex(store)
	BeginRepoTopologyMutation(store, "b")(true, []string{"b/b.go"})
	if reads := store.reads.Load(); reads != 1 {
		t.Fatalf("the coupling was read %d times, want once", reads)
	}
	if _, _, _, hit, _ := LookupCached(store, seedA); hit {
		t.Fatal("a mutation of a store with no coupling kept the records of another repository")
	}
}
