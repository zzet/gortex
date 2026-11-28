package reach

import (
	"context"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const seedC = "c/c.go::C"

// learningCouplingStore parks the frontier read of gateFile until release.
// With readHold set, its store read answers from a snapshot taken when the
// read starts, closing snapshotted, but returns only once readHold is closed.
type learningCouplingStore struct {
	*store_sqlite.Store
	gateFile         string
	entered, release chan struct{}
	readHold         chan struct{}
	snapshotted      chan struct{}
}

func (s *learningCouplingStore) RepoCouplingPairs(ctx context.Context) ([][2]string, error) {
	pairs, err := s.Store.RepoCouplingPairs(ctx)
	if s.readHold == nil {
		return pairs, err
	}
	close(s.snapshotted)
	select {
	case <-s.readHold:
		return pairs, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *learningCouplingStore) RepoCouplingPairsForFiles(ctx context.Context, files []string) ([][2]string, error) {
	if len(files) == 1 && files[0] == s.gateFile {
		close(s.entered)
		<-s.release
	}
	return s.Store.RepoCouplingPairsForFiles(ctx, files)
}

func joinCToB(store *learningCouplingStore) {
	store.AddBatch(nil, []*graph.Edge{{From: seedC, To: seedB, Kind: graph.EdgeCalls, Confidence: 1}})
}

// What a repository mutation learns from its frontier joins the coupling even
// when the coupling grew while the mutation read its frontier: beside another
// mutation that learned a coupling of its own, or because the store read,
// whose snapshot predates the mutation's edge, landed meanwhile. A coupling
// that dropped it would let a later mutation of a keep the records of b,
// although an edge joins them.
func TestRepoCouplingKeepsWhatAMutationLearnedWhileItGrew(t *testing.T) {
	for _, tc := range []struct {
		name string
		// prepare sets the coupling up before the mutation of a starts; grow
		// grows it while that mutation's frontier read is parked.
		prepare, grow func(t *testing.T, store *learningCouplingStore)
	}{
		{
			name: "beside another mutation's learning",
			prepare: func(t *testing.T, store *learningCouplingStore) {
				awaitRepoCoupling(t, store)
			},
			grow: func(_ *testing.T, store *learningCouplingStore) {
				finishC := BeginRepoTopologyMutation(store, "c")
				joinCToB(store)
				finishC(true, []string{"c/c.go"})
			},
		},
		{
			name: "once the store read landed",
			prepare: func(t *testing.T, store *learningCouplingStore) {
				joinCToB(store)
				store.readHold, store.snapshotted = make(chan struct{}), make(chan struct{})
				ensureRepoCoupling(store)
				select {
				case <-store.snapshotted:
				case <-time.After(topologyScopeWait):
					t.Fatal("the store read never started")
				}
			},
			grow: func(t *testing.T, store *learningCouplingStore) {
				close(store.readHold)
				awaitCoupling(t, store, func(c *repoCoupling) bool { return c.ready })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &learningCouplingStore{
				Store:    repoTopologyStore(t),
				gateFile: "a/a.go",
				entered:  make(chan struct{}),
				release:  make(chan struct{}),
			}
			store.AddBatch([]*graph.Node{{ID: seedC, Kind: graph.KindFunction, Name: "C", FilePath: "c/c.go", RepoPrefix: "c"}}, nil)
			tc.prepare(t, store)

			finishA := BeginRepoTopologyMutation(store, "a")
			store.AddBatch(nil, []*graph.Edge{{From: seedA, To: seedB, Kind: graph.EdgeCalls, Confidence: 1}})
			doneA := make(chan struct{})
			go func() {
				finishA(true, []string{"a/a.go"})
				close(doneA)
			}()
			select {
			case <-store.entered:
			case <-doneA:
				t.Fatal("the mutation of a finished without reading its frontier")
			case <-time.After(topologyScopeWait):
				t.Fatal("the mutation of a never read its frontier")
			}
			tc.grow(t, store)
			close(store.release)
			select {
			case <-doneA:
			case <-time.After(topologyScopeWait):
				t.Fatal("the mutation of a never finished")
			}

			BuildIndex(store)
			BeginRepoTopologyMutation(store, "a")(true, []string{})
			if _, _, _, hit, _ := LookupCached(store, seedB); hit {
				t.Fatal("a mutation of a kept the records of b, although an edge joins them: the coupling dropped what a learned")
			}
		})
	}
}
