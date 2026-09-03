package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestPersistSemanticNodeStampsIsSetOrientedAndPreservesNodeData(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const nodeCount = semanticNodeStampChunkSize + 1
	nodes := make([]*graph.Node, 0, nodeCount+1)
	stamps := make([]graph.SemanticNodeStamp, 0, nodeCount+2)
	for i := 0; i < nodeCount; i++ {
		id := fmt.Sprintf("repo/file.go::Node%03d", i)
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("Node%03d", i),
			FilePath: "repo/file.go", RepoPrefix: "repo", StartLine: i + 1,
			Meta: map[string]any{
				"signature":       "func preserved()",
				"visibility":      "public",
				"doc":             "preserved doc",
				"semantic_type":   "old-semantic",
				"return_type":     "old-return",
				"semantic_source": "old-source",
				"opaque":          "keep-me",
			},
		})
		stamp := graph.SemanticNodeStamp{NodeID: id, SemanticSource: "go-types"}
		if i%2 == 0 {
			stamp.SemanticType = fmt.Sprintf("Semantic%03d", i)
		} else {
			stamp.ReturnType = fmt.Sprintf("Return%03d", i)
		}
		stamps = append(stamps, stamp)
	}

	preserveID := "repo/file.go::Preserve"
	nodes = append(nodes, &graph.Node{
		ID: preserveID, Kind: graph.KindFunction, Name: "Preserve",
		FilePath: "repo/file.go", RepoPrefix: "repo",
		Meta: map[string]any{
			"semantic_type":   "preserved-semantic",
			"return_type":     "preserved-return",
			"semantic_source": "preserved-source",
			"opaque":          "keep-me-too",
		},
	})
	stamps = append(stamps, graph.SemanticNodeStamp{NodeID: preserveID, SemanticSource: "ignored"})
	stamps = append(stamps, graph.SemanticNodeStamp{
		NodeID: "repo/missing.go::Missing", SemanticType: "Missing", SemanticSource: "go-types",
	})
	store.AddBatch(nodes, nil)

	countQuery, updateQuery, args := semanticNodeStampStatements(store.viewGen, stamps[:semanticNodeStampChunkSize])
	// One argument past the per-row values: the trailing generation binding.
	assert.Len(t, args, semanticNodeStampChunkSize*semanticNodeStampParamsPerRow+1)
	assert.Equal(t, semanticNodeStampChunkSize, strings.Count(countQuery, "(?,?,?,?)"))
	assert.Equal(t, semanticNodeStampChunkSize, strings.Count(updateQuery, "(?,?,?,?)"))

	beforeRevision := store.AnalysisMutationRevision()
	stats, err := store.persistSemanticNodeStamps(stamps)
	require.NoError(t, err)
	assert.Equal(t, (nodeCount+1)/2, stats.enriched, "only existing semantic-type stamps contribute to coverage")
	assert.Equal(t, nodeCount, stats.changedRows, "each existing non-empty stamp changes exactly one row")
	assert.Equal(t, 4, stats.statements, "two VALUES chunks use one count and one UPDATE each")
	assert.Equal(t, beforeRevision+1, store.AnalysisMutationRevision(), "one transaction advances the mutation clock once")

	got := store.GetNodesByIDs([]string{
		"repo/file.go::Node000",
		"repo/file.go::Node001",
		fmt.Sprintf("repo/file.go::Node%03d", nodeCount-1),
		preserveID,
	})
	even := got["repo/file.go::Node000"]
	require.NotNil(t, even)
	assert.Equal(t, "Semantic000", even.Meta["semantic_type"])
	assert.Equal(t, "old-return", even.Meta["return_type"], "empty return stamp must preserve the prior value")
	assert.Equal(t, "go-types", even.Meta["semantic_source"])
	assert.Equal(t, "keep-me", even.Meta["opaque"])
	assert.Equal(t, "func preserved()", even.Meta["signature"])
	assert.Equal(t, "public", even.Meta["visibility"])
	assert.Equal(t, "preserved doc", even.Meta["doc"])
	assert.Equal(t, 1, even.StartLine)

	odd := got["repo/file.go::Node001"]
	require.NotNil(t, odd)
	assert.Equal(t, "old-semantic", odd.Meta["semantic_type"], "empty semantic stamp must preserve the prior value")
	assert.Equal(t, "Return001", odd.Meta["return_type"])
	assert.Equal(t, "go-types", odd.Meta["semantic_source"])
	assert.Equal(t, "keep-me", odd.Meta["opaque"])

	preserved := got[preserveID]
	require.NotNil(t, preserved)
	assert.Equal(t, "preserved-semantic", preserved.Meta["semantic_type"])
	assert.Equal(t, "preserved-return", preserved.Meta["return_type"])
	assert.Equal(t, "preserved-source", preserved.Meta["semantic_source"])
	assert.Equal(t, "keep-me-too", preserved.Meta["opaque"])

	buildMinimalAnalysisGeneration(t, store, "node-stamps-noop", 0, true)
	beforeRevision = store.AnalysisMutationRevision()
	assert.Equal(t, (nodeCount+1)/2, store.PersistSemanticNodeStamps(stamps), "public coverage count is stable on an idempotent pass")
	assert.Equal(t, beforeRevision, store.AnalysisMutationRevision(), "idempotent stamps must not invalidate warm analysis state")
	_, found, err := store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.True(t, found, "idempotent stamps must preserve active warm analysis")

	assert.Equal(t, 1, store.PersistSemanticNodeStamps([]graph.SemanticNodeStamp{{
		NodeID: "repo/file.go::Node000", SemanticType: "SemanticChanged", SemanticSource: "go-types",
	}}))
	_, found, err = store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.False(t, found, "a changed stamp must invalidate active analysis in the same transaction")
}
