package mcp

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

func markHotspotAnalysisCurrent(s *Server) {
	s.analysisEpoch = 1
	token := s.currentCommunityToken()
	s.communitiesToken = token
	s.adjacencyToken = token
}

func TestGetHotspotsCoalescesConcurrentLazyBuilds(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	var builds atomic.Int32
	s := &Server{
		graph: graph.New(),
		hotspotsFn: func(graph.Reader, *analysis.CommunityResult, float64) []analysis.HotspotEntry {
			builds.Add(1)
			return []analysis.HotspotEntry{{ID: "hot"}}
		},
	}
	markHotspotAnalysisCurrent(s)

	const callers = 32
	results := make(chan []analysis.HotspotEntry, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			results <- s.getHotspots()
		}()
	}
	wg.Wait()
	close(results)

	for got := range results {
		require.Len(t, got, 1)
		assert.Equal(t, "hot", got[0].ID)
	}
	assert.EqualValues(t, 1, builds.Load())
}

func TestGetHotspotsRebuildsAfterAnalysisEpochInvalidation(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	var builds atomic.Int32
	s := &Server{
		graph: graph.New(),
		hotspotsFn: func(graph.Reader, *analysis.CommunityResult, float64) []analysis.HotspotEntry {
			return []analysis.HotspotEntry{{ID: string(rune('0' + builds.Add(1)))}}
		},
	}
	markHotspotAnalysisCurrent(s)

	first := s.getHotspots()
	require.Len(t, first, 1)

	s.analysisMu.Lock()
	s.analysisEpoch++
	s.hotspots = nil
	s.hotspotsReady = false
	s.analysisMu.Unlock()

	second := s.getHotspots()
	require.Len(t, second, 1)
	assert.NotEqual(t, first[0].ID, second[0].ID)
	assert.EqualValues(t, 2, builds.Load())
}

func TestIncrementalCommunitiesInvalidatesInFlightHotspotBuild(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "pkg/a.go", Language: "go"})

	oldCommunities := &analysis.CommunityResult{
		NodeToComm: map[string]string{"pkg/a.go::A": "old"},
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var builds atomic.Int32
	var firstSawOld atomic.Bool
	s := &Server{
		graph:       g,
		communities: oldCommunities,
		hotspotsFn: func(_ graph.Reader, communities *analysis.CommunityResult, _ float64) []analysis.HotspotEntry {
			if builds.Add(1) == 1 {
				firstSawOld.Store(communities == oldCommunities)
				close(started)
				<-release
				return []analysis.HotspotEntry{{ID: "stale"}}
			}
			if communities == oldCommunities {
				return []analysis.HotspotEntry{{ID: "stale"}}
			}
			return []analysis.HotspotEntry{{ID: "fresh"}}
		},
	}
	markHotspotAnalysisCurrent(s)

	resultCh := make(chan []analysis.HotspotEntry, 1)
	go func() { resultCh <- s.getHotspots() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("hotspot build did not start")
	}

	g.AddNode(&graph.Node{ID: "pkg/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "pkg/b.go", Language: "go"})
	beforeEpoch := s.analysisEpoch
	communities, _ := s.incrementalCommunities()
	require.NotSame(t, oldCommunities, communities)

	s.analysisMu.RLock()
	assert.Equal(t, beforeEpoch+1, s.analysisEpoch)
	assert.False(t, s.hotspotsReady)
	assert.Nil(t, s.hotspots)
	s.analysisMu.RUnlock()

	close(release)
	select {
	case got := <-resultCh:
		require.Len(t, got, 1)
		assert.Equal(t, "fresh", got[0].ID)
	case <-time.After(5 * time.Second):
		t.Fatal("hotspot build did not finish")
	}
	assert.True(t, firstSawOld.Load())
	assert.EqualValues(t, 2, builds.Load())
}

func TestGetHotspotsRejectsCachedResultAfterGraphMutation(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "pkg/a.go"})
	s := &Server{
		graph:         g,
		hotspots:      []analysis.HotspotEntry{{ID: "stale"}},
		hotspotsReady: true,
		communities:   &analysis.CommunityResult{NodeToComm: map[string]string{"pkg/a.go::A": "old"}},
	}
	markHotspotAnalysisCurrent(s)

	g.AddNode(&graph.Node{ID: "pkg/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "pkg/b.go"})

	assert.Nil(t, s.getHotspots())
}
