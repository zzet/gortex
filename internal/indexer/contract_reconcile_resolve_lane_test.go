package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
)

// resolveLaneProbeStore records whether the store's resolver lane was already
// held at the moment the contract reconciler touched graph edges. A resolve
// pass rewrites To / Kind / Origin on the store's live *Edge values under that
// same mutex, so a scan that finds the lane free is a scan that can read an
// edge mid-rewrite.
type resolveLaneProbeStore struct {
	graph.Store

	evictCalls    int
	evictLaneHeld bool

	outEdgeCalls    int
	outEdgeLaneHeld bool
}

func (s *resolveLaneProbeStore) laneHeld() bool {
	mu := s.ResolveMutex()
	if mu.TryLock() {
		mu.Unlock()
		return false
	}
	return true
}

func (s *resolveLaneProbeStore) EvictEdgesByKinds(kinds []graph.EdgeKind) int {
	s.evictCalls++
	s.evictLaneHeld = s.laneHeld()
	return graph.EvictEdgesByKinds(s.Store, kinds)
}

func (s *resolveLaneProbeStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.outEdgeCalls++
	s.outEdgeLaneHeld = s.laneHeld()
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *resolveLaneProbeStore) ReplaceDerivedContracts(
	replacement graph.DerivedContractReplacement,
) (graph.DerivedContractReplaceResult, error) {
	return graph.ReplaceDerivedContracts(s.Store, replacement)
}

func newResolveLaneProbeIndexer(t *testing.T) (*MultiIndexer, *resolveLaneProbeStore) {
	t.Helper()
	probe := &resolveLaneProbeStore{Store: graph.New()}
	mi := NewMultiIndexer(probe, parser.NewRegistry(), search.NewNull(), nil, zap.NewNop())
	return mi, probe
}

// TestReconcileContractEdgesHoldsResolveLane pins the full reconcile pass to
// the resolver lane. Without it the whole-graph EdgeMatches / topic eviction
// scan races an indexing ResolveAll that is rewriting those very edges.
func TestReconcileContractEdgesHoldsResolveLane(t *testing.T) {
	mi, probe := newResolveLaneProbeIndexer(t)

	mi.ReconcileContractEdges()

	require.Equal(t, 1, probe.evictCalls, "reconcile did not evict the derived edge generation")
	require.True(t, probe.evictLaneHeld,
		"ReconcileContractEdges scanned graph edges without holding Store.ResolveMutex()")
}

// TestReconcileContractEdgesForFrontierHoldsResolveLane pins the incremental
// sibling to the same lane: its incident-edge scan dereferences the same live
// store edges.
func TestReconcileContractEdgesForFrontierHoldsResolveLane(t *testing.T) {
	mi, probe := newResolveLaneProbeIndexer(t)

	mi.ReconcileContractEdgesForFrontier(DerivedInvalidationPlan{
		ContractSymbolIDs: []string{"svc/api.go::Handler"},
	})

	require.Equal(t, 1, probe.outEdgeCalls, "frontier reconcile did not scan incident contract edges")
	require.True(t, probe.outEdgeLaneHeld,
		"ReconcileContractEdgesForFrontier scanned graph edges without holding Store.ResolveMutex()")
}
