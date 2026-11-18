package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractExtractionReplayKeepsFullAnswerWithoutScratchWrites(t *testing.T) {
	const path = "repo/dashboard.go"
	edges := []*graph.Edge{
		{From: path + "::render", To: "repo/target.go::Call", Kind: graph.EdgeCalls, FilePath: path, Meta: map[string]any{"ordinary": map[string]any{"kept_for_bound": true}}},
		{From: path + "::render", To: "repo/provider.go::Bean", Kind: graph.EdgeCalls, FilePath: path, Confidence: .91, Meta: map[string]any{"via": "spring.Bean", "nested": map[string]any{"kept": []string{"a", "b"}}}},
	}
	meta := map[string]any{"signature": "func render()", "nested": map[string]any{"all_fields": []string{"a", "b"}}}
	original := extractionFileEvidence(t, edges, meta)
	nodes, seeds, err := original.loadExtractionFile(path)
	require.NoError(t, err)
	want, ok := contractAnswerDigest(contractFileAnswer{Nodes: nodes, Edges: seeds})
	require.True(t, ok)
	next := extractionFileEvidence(t, edges, meta)
	readCore := next.readCore
	next.readCore = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		require.Equal(t, true, ctx.Value(contractExtractionCoreContextKey{}), "the installed checked extraction projection remains in use")
		return readCore(ctx, file)
	}
	answer, err := (&contractConvergence{}).ask(next, original.ledger.reads[0])
	require.NoError(t, err)
	got, ok := contractAnswerDigest(answer)
	require.True(t, ok)
	require.Equal(t, want, got, "full nodes, nested metadata and retained edge payloads must be identical")
	require.Equal(t, contractFileAnswer{Nodes: nodes, Edges: seeds}, answer, "complete fields, including internal confidence and metadata, stay identical")
	require.Zero(t, next.writtenNodes, "a replay question must not copy core nodes into scratch SQLite")
	require.Zero(t, next.writtenEdges, "a replay question must not copy core edges into scratch SQLite")
	// Returned values are detached, exactly as the extraction answer was.
	answer.(contractFileAnswer).Nodes[1].Meta["signature"] = "private"
	require.Equal(t, "func render()", meta["signature"])
}

func TestContractExtractionReplayRetainsByteRowAndContextFailures(t *testing.T) {
	const path = "repo/dashboard.go"
	read := contractRecordedRead{kind: contractReadExtractionFile, args: []string{path}}
	t.Run("nonseed_edge_still_charged", func(t *testing.T) {
		e := extractionFileEvidence(t, []*graph.Edge{{From: path + "::render", To: "repo/target.go::Call", Kind: graph.EdgeCalls, FilePath: path, Origin: strings.Repeat("x", contractFollowupCompactLimit)}}, nil)
		_, err := (&contractConvergence{}).ask(e, read)
		require.ErrorIs(t, err, graph.ErrContractProjectionLimit, "the exact previous import byte bound includes edges omitted from extraction seeds")
		require.Zero(t, e.writtenNodes)
	})
	t.Run("row_bound", func(t *testing.T) {
		e := extractionFileEvidence(t, nil, nil)
		e.readCore = func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error) {
			return ContractFollowupCoreFile{Nodes: make([]*graph.Node, graph.ContractProjectionRowLimit+1)}, nil
		}
		_, err := (&contractConvergence{}).ask(e, read)
		require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	})
	t.Run("canceled", func(t *testing.T) {
		e := extractionFileEvidence(t, nil, nil)
		ctx, cancel := context.WithCancel(e.ctx)
		cancel()
		e.ctx = ctx
		_, err := (&contractConvergence{}).ask(e, read)
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, e.writtenNodes)
	})
	t.Run("checked_read_error", func(t *testing.T) {
		e := extractionFileEvidence(t, nil, nil)
		e.readCore = func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error) {
			return ContractFollowupCoreFile{}, graph.ErrContractProjectionStale
		}
		_, err := (&contractConvergence{}).ask(e, read)
		require.ErrorIs(t, err, graph.ErrContractProjectionStale)
		require.Zero(t, e.writtenNodes)
	})
}
