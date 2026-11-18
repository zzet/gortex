package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// installCommunitiesForTest publishes a synthetic community result with the
// same graph-token discipline as production analysis. Tests must use this
// helper instead of assigning Server.communities directly: production readers
// deliberately reject results that are not tied to the current graph revision.
func installCommunitiesForTest(s *Server, communities *analysis.CommunityResult) {
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()

	s.communities = communities
	s.communitiesToken = s.currentCommunityToken()
	s.analysisEpoch++
	s.hotspots = nil
	s.hotspotsReady = false
	s.hotspotsOwned = false
}

func TestInstallCommunitiesForTestRespectsGraphFreshness(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "pkg/a.go"})
	s := &Server{graph: g}
	fixture := &analysis.CommunityResult{NodeToComm: map[string]string{"pkg/a.go::A": "c1"}}
	installCommunitiesForTest(s, fixture)

	if got := s.getCommunities(); got != fixture {
		t.Fatalf("current fixture = %p, want %p", got, fixture)
	}

	g.AddNode(&graph.Node{ID: "pkg/b.go::B", Name: "B", Kind: graph.KindFunction, FilePath: "pkg/b.go"})
	if got := s.getCommunities(); got != nil {
		t.Fatalf("stale fixture survived graph mutation: %#v", got)
	}
}
