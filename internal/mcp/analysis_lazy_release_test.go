package mcp

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/zzet/gortex/internal/analysis"
)

func TestReleaseTransientAnalysisDropsPublishedCompatibilityViews(t *testing.T) {
	s := &Server{
		analysisGenerationReady: true,
		communities:             &analysis.CommunityResult{},
		processes:               &analysis.ProcessResult{},
		hotspots:                []analysis.HotspotEntry{{ID: "hot"}},
		hotspotsReady:           true,
	}

	if !s.releaseTransientAnalysisIfIdle() {
		t.Fatal("expected published transient analysis to be released")
	}
	if s.communities != nil || s.processes != nil || len(s.hotspots) != 1 || !s.hotspotsReady {
		t.Fatalf("transient analysis retained: communities=%v processes=%v hotspots=%v ready=%v",
			s.communities != nil, s.processes != nil, s.hotspots != nil, s.hotspotsReady)
	}
}

func TestReleaseTransientAnalysisDropsOversizeHotspots(t *testing.T) {
	for _, entries := range [][]analysis.HotspotEntry{
		{{ID: strings.Repeat("x", hotspotResultRetentionBytes)}},
		make([]analysis.HotspotEntry, hotspotResultRetentionBytes/int(unsafe.Sizeof(analysis.HotspotEntry{}))+1),
	} {
		s := &Server{analysisGenerationReady: true, hotspots: entries, hotspotsReady: true}
		if hotspotResultRetainable(entries) {
			t.Fatal("oversize result accepted")
		}
		s.releaseTransientAnalysisIfIdle()
		if s.hotspots != nil || s.hotspotsReady {
			t.Fatal("oversize result retained")
		}
	}
}

func TestHotspotResultRetentionIncludesStringBytes(t *testing.T) {
	entryBytes := int(unsafe.Sizeof(analysis.HotspotEntry{}))
	sliceBytes := int(unsafe.Sizeof([]analysis.HotspotEntry(nil)))
	entries := []analysis.HotspotEntry{{ID: strings.Repeat("x", hotspotResultRetentionBytes-entryBytes-sliceBytes)}}
	if !hotspotResultRetainable(entries) {
		t.Fatal("exact budget rejected")
	}
	entries[0].FilePath = "x"
	if hotspotResultRetainable(entries) {
		t.Fatal("string bytes outside the budget retained")
	}
}

func TestHotspotResultRetentionOwnsSubstringsAndSubsliceStorage(t *testing.T) {
	large := strings.Repeat("x", 2*hotspotResultRetentionBytes)
	backing := []analysis.HotspotEntry{
		{Name: large},
		{ID: large[:1], Name: large[1:3], Kind: large[3:6], FilePath: large[6:10], ComplexityScore: 3.5},
		{Name: large},
	}
	input := backing[1:2]
	s := &Server{analysisGenerationReady: true, hotspots: input, hotspotsReady: true}
	s.releaseTransientAnalysisIfIdle()
	if !s.hotspotsOwned || !s.hotspotsReady || len(s.hotspots) != 1 || cap(s.hotspots) != 1 {
		t.Fatal("eligible complete result was not compacted")
	}
	if &s.hotspots[0] == &input[0] || s.hotspots[0] != input[0] {
		t.Fatal("retained slice aliases its source or changed the result")
	}
	owned := s.hotspots[0]
	for i, value := range []string{owned.ID, owned.Name, owned.Kind, owned.FilePath} {
		source := []string{input[0].ID, input[0].Name, input[0].Kind, input[0].FilePath}[i]
		if unsafe.StringData(value) == unsafe.StringData(source) {
			t.Fatal("retained string still aliases a large source allocation")
		}
	}
	first := &s.hotspots[0]
	s.releaseTransientAnalysisIfIdle()
	if &s.hotspots[0] != first {
		t.Fatal("already owned result was unnecessarily copied again")
	}
}

func TestReleaseTransientAnalysisKeepsUnpublishedCompatibilityViews(t *testing.T) {
	communities := &analysis.CommunityResult{}
	s := &Server{communities: communities}

	if s.releaseTransientAnalysisIfIdle() {
		t.Fatal("unpublished transient analysis must remain available")
	}
	if s.communities != communities {
		t.Fatal("unpublished compatibility view was dropped")
	}
}
