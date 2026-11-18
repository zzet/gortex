package store_sqlite

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestExternalCallCandidatePaging(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for line := 1; line <= 520; line++ {
		s.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/library::Hello", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: line})
	}
	s.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "b/lib.go::Hello", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: 2000})
	for line := 521; line <= 1040; line++ {
		s.AddEdge(&graph.Edge{From: "a/main.go::Caller", To: "dep::example.org/library::Hello", Kind: graph.EdgeReferences, FilePath: "a/main.go", Line: line})
	}
	var highWater int64
	require.NoError(t, s.db.QueryRow("SELECT MAX(id) FROM edges").Scan(&highWater))
	page, err := s.externalCallCandidatePage(graph.EdgeCalls, 0, highWater)
	require.NoError(t, err)
	require.Len(t, page.Edges, 512)
	seen := make(map[int]bool)
	for edge := range s.ExternalCallCandidateEdgesSeq() {
		require.False(t, seen[edge.Line], "reindexed edges must not re-enter the scan")
		seen[edge.Line] = true
		oldTo := edge.To
		edge.To = fmt.Sprintf("dep::example.org/new::Hello%d", edge.Line)
		s.ReindexEdge(edge, oldTo) // cursor must close before callbacks mutate
		if edge.Line == 1 {
			s.AddEdge(&graph.Edge{From: edge.From, To: "dep::example.org/new::Fn", Kind: graph.EdgeReferences, FilePath: edge.FilePath, Line: 99999})
		}
	}
	assert.Len(t, seen, 1040)
	assert.False(t, seen[99999], "new references cannot enter the second kind stream")
	assert.False(t, seen[2000], "resolved calls must never cross the SQL projection")
	visits := 0
	for range s.ExternalCallCandidateEdgesSeq() {
		visits++
		break
	}
	assert.Equal(t, 1, visits)
}

func TestGoModuleReindexApplied(t *testing.T) {
	store, batch := openBusyReindexFixture(t, filepath.Join(t.TempDir(), "graph.sqlite"))
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.busyRetryTimeout = time.Millisecond
	store.writeMu.Lock()
	applied := store.ReindexEdgesApplied(batch)
	store.writeMu.Unlock()
	require.False(t, applied)
	require.True(t, store.ReindexEdgesApplied(batch))
	requireReindexedTarget(t, store, "repo/target.go::Target")
}
