package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type replayProjectionReader struct {
	graph.Reader
	files, edges, widest int
	batchErr, fileErr    error
}

func (r *replayProjectionReader) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*graph.Node, error) {
	r.files++
	r.widest = max(r.widest, len(paths))
	if len(paths) > 1 && r.batchErr != nil {
		return nil, r.batchErr
	}
	if len(paths) == 1 && r.fileErr != nil {
		return nil, r.fileErr
	}
	return graph.ContractFileNodesContext(ctx, r.Reader, repo, paths)
}
func (r *replayProjectionReader) GetOutEdgesByNodeIDsWithMetadataContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
	r.edges++
	return graph.GetOutEdgesByNodeIDsWithMetadataContext(ctx, r.Reader, ids, limit)
}
func replayProjectionFixture(n int) (*replayProjectionReader, []ContractFollowupFile, *contractReplayCoreBatch) {
	core := graph.New()
	var files []ContractFollowupFile
	batch := &contractReplayCoreBatch{}
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("repo/f%d.go", i)
		file := ContractFollowupFile{Path: path, RepoPrefix: "repo"}
		files = append(files, file)
		batch.add(file)
		core.AddBatch([]*graph.Node{
			{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo"},
			{ID: path + "::fn", Kind: graph.KindFunction, Name: "fn", FilePath: path, RepoPrefix: "repo", StartLine: 7, EndLine: 9, Meta: map[string]any{"nested": map[string]any{"values": []string{"full", "node"}}}},
		}, []*graph.Edge{
			{From: path + "::fn", To: "repo/bean.go::Bean", Kind: graph.EdgeCalls, FilePath: path, Line: 8, Confidence: .91, Meta: map[string]any{"via": "spring.Bean", "nested": map[string]any{"all": "kept"}}},
			{From: path + "::fn", To: "repo/ordinary.go::Call", Kind: graph.EdgeCalls, FilePath: path, Origin: "ordinary-still-charged"},
		})
	}
	return &replayProjectionReader{Reader: core}, files, batch
}
func replayProjectionContext(ctx context.Context, batch *contractReplayCoreBatch) context.Context {
	return context.WithValue(context.WithValue(ctx, contractReplayCoreBatchKey{}, batch), contractExtractionCoreContextKey{}, true)
}

func TestContractReplayProjectionBatchKeepsCallbacksAndCompleteAnswers(t *testing.T) {
	reader, files, batch := replayProjectionFixture(12)
	defer batch.close()
	ctx := replayProjectionContext(t.Context(), batch)
	callbacks := 0
	installed := func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		callbacks++
		require.Equal(t, true, ctx.Value(contractExtractionCoreContextKey{}))
		return readContractFollowupCoreFile(ctx, reader, file)
	}
	var first ContractFollowupCoreFile
	for i, file := range files {
		want, err := readContractFollowupCoreFile(context.WithValue(t.Context(), contractExtractionCoreContextKey{}, true), reader.Reader, file)
		require.NoError(t, err)
		got, err := installed(ctx, file)
		require.NoError(t, err)
		require.Equal(t, want, got, "all rich fields and ordinary non-seed edges are still present")
		if i == 0 {
			first = got
		}
	}
	require.Equal(t, 12, callbacks, "each logical read must pass the installed callback")
	require.Equal(t, 1, reader.files, "one typed backend projection replaces twelve serial projections")
	require.Equal(t, 1, reader.edges)
	require.Equal(t, 12, reader.widest)
	first.Nodes[1].Meta["nested"].(map[string]any)["values"].([]string)[0] = "changed"
	first.Edges[0].Meta["nested"].(map[string]any)["all"] = "changed"
	again, err := installed(ctx, files[0])
	require.NoError(t, err)
	require.Equal(t, "full", again.Nodes[1].Meta["nested"].(map[string]any)["values"].([]string)[0])
	require.Equal(t, "kept", again.Edges[0].Meta["nested"].(map[string]any)["all"])
	// A wrapper refusal remains authoritative even when a raw batch is ready.
	sentinel := errors.New("installed wrapper refuses")
	wrapped := func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		_, err := installed(ctx, file)
		require.NoError(t, err)
		return ContractFollowupCoreFile{}, sentinel
	}
	_, err = wrapped(ctx, files[1])
	require.ErrorIs(t, err, sentinel)
}

func TestContractReplayProjectionBatchFallsBackWithoutCertifyingPartialRows(t *testing.T) {
	for _, failure := range []error{graph.ErrContractProjectionLimit, graph.ErrContractProjectionUnsupported, errors.New("checked batch changed")} {
		t.Run(failure.Error(), func(t *testing.T) {
			reader, files, batch := replayProjectionFixture(3)
			defer batch.close()
			reader.batchErr = failure
			ctx := replayProjectionContext(t.Context(), batch)
			for _, file := range files {
				got, err := readContractFollowupCoreFile(ctx, reader, file)
				require.NoError(t, err)
				require.Len(t, got.Nodes, 2)
				require.Len(t, got.Edges, 2)
			}
			require.Equal(t, 4, reader.files, "failed batch plus all three original per-file reads")
			require.Equal(t, 3, reader.edges)
		})
	}
	reader, files, batch := replayProjectionFixture(2)
	defer batch.close()
	reader.batchErr = graph.ErrContractProjectionLimit
	reader.fileErr = errors.New("real file refused")
	_, err := readContractFollowupCoreFile(replayProjectionContext(t.Context(), batch), reader, files[0])
	require.ErrorIs(t, err, reader.fileErr)
}

func TestContractReplayProjectionBatchHasBoundedBracketLifetime(t *testing.T) {
	reader, files, batch := replayProjectionFixture(129)
	ctx := replayProjectionContext(t.Context(), batch)
	for _, file := range files {
		got, err := readContractFollowupCoreFile(ctx, reader, file)
		require.NoError(t, err)
		require.Len(t, got.Nodes, 2)
	}
	require.Equal(t, 2, reader.files)
	require.Equal(t, 128, reader.widest)
	require.Len(t, batch.rows, 1, "only one bounded chunk survives, never the whole census")
	batch.close()
	require.Empty(t, batch.rows)
	require.Empty(t, batch.files)
	_, err := readContractFollowupCoreFile(ctx, reader, files[0])
	require.NoError(t, err)
	require.Equal(t, 3, reader.files, "closed bracket cannot supply a retained answer")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = readContractFollowupCoreFile(canceled, reader, files[0])
	require.ErrorIs(t, err, context.Canceled)
}

func TestContractReplayProjectionBatchNeverNarrowsFullReadsOrUnlistedFiles(t *testing.T) {
	reader, files, batch := replayProjectionFixture(3)
	defer batch.close()
	batch.files = batch.files[:2]
	ctx := context.WithValue(t.Context(), contractReplayCoreBatchKey{}, batch)
	_, err := readContractFollowupCoreFile(ctx, reader, files[0])
	require.NoError(t, err)
	require.Empty(t, batch.rows, "full enrichment/declaration reads ignore extraction batching")
	ctx = context.WithValue(ctx, contractExtractionCoreContextKey{}, true)
	_, err = readContractFollowupCoreFile(ctx, reader, files[2])
	require.NoError(t, err)
	require.Empty(t, batch.rows, "a path outside the R0/R1 candidate union cannot be prefetched")
	require.Equal(t, 2, reader.files)
}

func TestContractReplayProjectionBatchIsRejectedWhenClosingStampMoves(t *testing.T) {
	f := newRouteConstantFixture(t)
	batchReads := 0
	f.adjust = func(snapshot *ContractFollowupSnapshot) {
		read := snapshot.ReadCoreFile
		snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
			if batch, _ := ctx.Value(contractReplayCoreBatchKey{}).(*contractReplayCoreBatch); batch != nil {
				batchReads++
			}
			return read(ctx, file)
		}
	}
	stage := 0
	hooks := &contractConvergenceHooks{
		afterExtract: func(path string) {
			if stage == 0 && path == "fixture/routes.go" {
				stage = 1
				f.saveKeepingKey("consts.go", jobConsts+"// first receipt move\n")
			}
		},
		beforeRevalidateR1: func() {
			if stage == 1 && batchReads > 0 {
				stage = 2
				f.saveKeepingKey("consts.go", jobConsts+"// closing receipt move\n")
			}
		},
	}
	report, err := f.job(hooks, nil)
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, 2, stage, "real capture replays checked rows, then observes the closing move")
	require.Positive(t, batchReads, "the production capture callback remains installed during batch replay")
	require.Equal(t, contractMeta(coldContracts(t, map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts + "// closing receipt move\n", "helper.go": jobHelper})), contractMeta(f.contracts()))
}
