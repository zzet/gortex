package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

// Each call has its own gate so abandoned and replacement reads can finish
// in either order, without relying on scheduler timing or database latency.
type reposLifetimeCall struct {
	ctx        context.Context
	release    chan struct{}
	once       sync.Once
	stats      map[string]graph.GraphStats
	err        error
	panicValue any
}

func (c *reposLifetimeCall) finish() { c.once.Do(func() { close(c.release) }) }

type reposLifetimeStore struct {
	graph.Store
	calls chan *reposLifetimeCall
}

func (s *reposLifetimeStore) RepoStatsContext(ctx context.Context) (map[string]graph.GraphStats, error) {
	c := &reposLifetimeCall{ctx: ctx, release: make(chan struct{})}
	s.calls <- c
	<-c.release
	if c.panicValue != nil {
		panic(c.panicValue)
	}
	return c.stats, c.err
}

// Embedding graph.Store rather than reposLifetimeStore deliberately hides the
// optional context method and exercises the non-cancelable fallback.
type reposLifetimeFallback struct {
	graph.Store
	reader *reposLifetimeStore
}

func (s *reposLifetimeFallback) RepoStats() map[string]graph.GraphStats {
	stats, _ := s.reader.RepoStatsContext(context.Background())
	return stats
}

func awaitReposLifetime[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for repo lifetime boundary")
	}
	var zero T
	return zero
}

func reposLifetimeFlight(t *testing.T, h *Handler) *repoSnapshotFlight {
	t.Helper()
	h.repoSnapshot.mu.Lock()
	defer h.repoSnapshot.mu.Unlock()
	if h.repoSnapshot.flight == nil {
		t.Fatal("missing active flight")
	}
	return h.repoSnapshot.flight
}

func startReposLifetimeRequest(h *Handler, ctx context.Context) <-chan int {
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/repos", nil).WithContext(ctx))
		done <- w.Code
	}()
	return done
}

func TestReposWorkerLifetimeCancellation(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "context"
		if fallback {
			name = "fallback"
		}
		t.Run(name, func(t *testing.T) {
			leases := graphview.NewLeaseManager()
			s := &reposLifetimeStore{Store: seededGraph(t), calls: make(chan *reposLifetimeCall, 1)}
			var store graph.Store = s
			if fallback {
				store = &reposLifetimeFallback{Store: s.Store, reader: s}
			}
			h := viewBindingHandler(t, store, leases)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := startReposLifetimeRequest(h, ctx)
			call := awaitReposLifetime(t, s.calls)
			defer call.finish()
			flight := reposLifetimeFlight(t, h)
			cancel()
			if code := awaitReposLifetime(t, done); code != 503 {
				t.Fatalf("status=%d", code)
			}
			if !fallback {
				awaitReposLifetime(t, call.ctx.Done())
			}
			if !leases.InUse(graphview.BaseCorpusGeneration) {
				t.Fatal("canceled handler released the worker's generation")
			}
			call.finish()
			awaitReposLifetime(t, flight.done)
			if leases.InUse(graphview.BaseCorpusGeneration) {
				t.Fatal("worker leaked generation after completion")
			}
			h.repoSnapshot.mu.Lock()
			cached := !h.repoSnapshot.collected.IsZero()
			h.repoSnapshot.mu.Unlock()
			if cached {
				t.Fatal("abandoned worker published a snapshot")
			}
		})
	}
}

func TestReposWorkerLifetimeReplacement(t *testing.T) {
	for _, oldFirst := range []bool{false, true} {
		name := "replacement-finishes-first"
		if oldFirst {
			name = "abandoned-finishes-first"
		}
		t.Run(name, func(t *testing.T) {
			leases := graphview.NewLeaseManager()
			s := &reposLifetimeStore{Store: seededGraph(t), calls: make(chan *reposLifetimeCall, 2)}
			h := viewBindingHandler(t, s, leases)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			oldDone := startReposLifetimeRequest(h, ctx)
			old := awaitReposLifetime(t, s.calls)
			defer old.finish()
			oldFlight := reposLifetimeFlight(t, h)
			cancel()
			awaitReposLifetime(t, oldDone)
			freshDone := startReposLifetimeRequest(h, context.Background())
			fresh := awaitReposLifetime(t, s.calls)
			defer fresh.finish()
			freshFlight := reposLifetimeFlight(t, h)
			if freshFlight == oldFlight {
				t.Fatal("replacement joined abandoned flight")
			}
			old.stats = map[string]graph.GraphStats{"old": {TotalNodes: 1}}
			fresh.stats = map[string]graph.GraphStats{"fresh": {TotalNodes: 2}}
			if oldFirst {
				old.finish()
				awaitReposLifetime(t, oldFlight.done)
				if !leases.InUse(graphview.BaseCorpusGeneration) {
					t.Fatal("abandoned worker released replacement's pin")
				}
			}
			fresh.finish()
			if code := awaitReposLifetime(t, freshDone); code != 200 {
				t.Fatalf("replacement status=%d", code)
			}
			if !oldFirst {
				if !leases.InUse(graphview.BaseCorpusGeneration) {
					t.Fatal("replacement released abandoned worker's pin")
				}
				old.finish()
				awaitReposLifetime(t, oldFlight.done)
			}
			if leases.InUse(graphview.BaseCorpusGeneration) {
				t.Fatal("finished flights leaked generation")
			}
			h.repoSnapshot.mu.Lock()
			rows, collected := h.repoSnapshot.rows, h.repoSnapshot.collected
			h.repoSnapshot.mu.Unlock()
			if collected.IsZero() || len(rows) != 1 || rows[0].ID != "fresh" {
				t.Fatalf("replacement cache overwritten: rows=%v collected=%v", rows, collected)
			}
		})
	}
}

func TestReposWorkerLifetimeFailureCleanup(t *testing.T) {
	for _, panics := range []bool{false, true} {
		name := "error"
		if panics {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			leases := graphview.NewLeaseManager()
			s := &reposLifetimeStore{Store: seededGraph(t), calls: make(chan *reposLifetimeCall, 1)}
			h := viewBindingHandler(t, s, leases)
			done := startReposLifetimeRequest(h, context.Background())
			call := awaitReposLifetime(t, s.calls)
			defer call.finish()
			want := 503
			if panics {
				call.panicValue = "repo read panic"
				want = 500
			} else {
				call.err = errors.New("repo read failed")
			}
			call.finish()
			if code := awaitReposLifetime(t, done); code != want {
				t.Fatalf("status=%d want=%d", code, want)
			}
			if leases.InUse(graphview.BaseCorpusGeneration) {
				t.Fatal("failed worker leaked generation")
			}
			// Failure must not poison either the flight slot or the TTL cache.
			retryDone := startReposLifetimeRequest(h, context.Background())
			retry := awaitReposLifetime(t, s.calls)
			defer retry.finish()
			retry.finish()
			if code := awaitReposLifetime(t, retryDone); code != 200 {
				t.Fatalf("retry status=%d", code)
			}
			if leases.InUse(graphview.BaseCorpusGeneration) {
				t.Fatal("retry leaked generation")
			}
		})
	}
}

func TestReposWorkerLifetimeRefusesReleasedPin(t *testing.T) {
	leases := graphview.NewLeaseManager()
	pin := leases.AcquireBaseCorpus("")
	pin.Release()
	s := &reposCountingStore{Graph: graph.New()}
	var cache repoSnapshotCache
	if _, _, err := cache.get(context.Background(), s, pin); err == nil {
		t.Fatal("accepted released pin")
	}
	if s.reads.Load() != 0 {
		t.Fatal("read after refused handoff")
	}
}

// get evaluates Done only after joining the flight; this makes waiter
// admission observable without a sleep or polling the flight's internals.
type reposJoinedContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *reposJoinedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func TestReposWorkerLifetimeSharedWaiter(t *testing.T) {
	leases := graphview.NewLeaseManager()
	s := &reposLifetimeStore{Store: seededGraph(t), calls: make(chan *reposLifetimeCall, 2)}
	h := viewBindingHandler(t, s, leases)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := startReposLifetimeRequest(h, ctx)
	call := awaitReposLifetime(t, s.calls)
	defer call.finish()
	joined := &reposJoinedContext{Context: context.Background(), joined: make(chan struct{})}
	secondDone := make(chan error, 1)
	go func() {
		pin := leases.AcquireBaseCorpus("")
		_, _, err := h.repoSnapshot.get(joined, s, pin)
		pin.Release()
		secondDone <- err
	}()
	awaitReposLifetime(t, joined.joined)
	cancel()
	awaitReposLifetime(t, firstDone)
	if err := call.ctx.Err(); err != nil {
		t.Fatalf("remaining waiter lost shared query: %v", err)
	}
	select {
	case extra := <-s.calls:
		extra.finish()
		t.Fatal("waiter started a second aggregate")
	default:
	}
	call.finish()
	if err := awaitReposLifetime(t, secondDone); err != nil {
		t.Fatal(err)
	}
	if leases.InUse(graphview.BaseCorpusGeneration) {
		t.Fatal("shared flight leaked generation")
	}
}
