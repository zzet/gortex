package indexer

import (
	"context"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"slices"
	"testing"
)

func TestContractFileNodesBatchPreservesPerFileEvidenceWithOneRead(t *testing.T) {
	var nodes []*graph.Node
	var paths []string
	for i := 0; i < 4; i++ {
		path := fmt.Sprintf("repo/file%d.go", i)
		paths = append(paths, path)
		nodes = append(nodes, &graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo"}, &graph.Node{ID: path + "::F", Name: "F", Kind: graph.KindFunction, FilePath: path, RepoPrefix: "repo", Meta: map[string]any{"signature": fmt.Sprint(i)}})
	}
	e, core, full := fileNodesEvidence(t, nodes, nil)
	for _, path := range paths {
		e.files[path] = ContractFollowupFile{Path: path, RepoPrefix: "repo"}
	}
	got := e.GetFileNodesByPaths(paths)
	require.NoError(t, e.err)
	require.Len(t, got, 4)
	require.Len(t, e.ledger.reads, 4, "each file remains a separately replayable complete input")
	for i, path := range paths {
		require.Len(t, got[path], 2)
		require.Equal(t, contractReadFileNodes, e.ledger.reads[i].kind)
		require.Equal(t, []string{path}, e.ledger.reads[i].args)
		require.Contains(t, e.ledger.reads[i].owners, path)
		require.True(t, e.loadedFileNodes[path])
	}
	require.Zero(t, *full, "node-only batch cannot import outgoing edges")
	require.Equal(t, 1, core.calls, "one checked selected-core batch, not one composed query per file")
}

type contractBatchNodeControl struct {
	*contractFileNodesCore
	requests             [][]string
	limitAll, limitBatch bool
	failure              error
}

func (r *contractBatchNodeControl) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*graph.Node, error) {
	r.requests = append(r.requests, slices.Clone(paths))
	require.True(r.t, r.guard.recorded())
	if r.failure != nil {
		return map[string][]*graph.Node{paths[0]: {{ID: "partial"}}}, r.failure
	}
	if r.limitAll || (r.limitBatch && len(paths) > 1) {
		return nil, graph.ErrContractProjectionLimit
	}
	return r.contractFileNodesCore.LayerContractFileNodesContext(ctx, repo, paths)
}
func contractBatchFixture(t *testing.T, n int) (*contractFollowupEvidence, *contractBatchNodeControl, []string) {
	var nodes []*graph.Node
	var paths []string
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("repo/batch%04d.go", i)
		paths = append(paths, path)
		nodes = append(nodes, &graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, RepoPrefix: "repo", Meta: map[string]any{"input": "selected"}})
	}
	e, core, _ := fileNodesEvidence(t, nodes, nil)
	for _, path := range paths {
		e.files[path] = ContractFollowupFile{Path: path, RepoPrefix: "repo"}
	}
	counted := &contractBatchNodeControl{contractFileNodesCore: core}
	e.core = counted
	return e, counted, paths
}
func TestContractFileNodesBatchKeepsLimitsAndAuthoritativeErrors(t *testing.T) {
	for _, mode := range []string{"aggregate_limit", "per_file_limit", "partial_failure", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			e, core, paths := contractBatchFixture(t, 2)
			failure := errors.New("checked reader failed")
			switch mode {
			case "aggregate_limit":
				core.limitBatch = true
			case "per_file_limit":
				core.limitAll = true
			case "partial_failure":
				core.failure = failure
			case "canceled":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				e.ctx = ctx
			}
			got := e.GetFileNodesByPaths(paths)
			if mode == "aggregate_limit" {
				require.NoError(t, e.err)
				require.Len(t, got, 2)
				require.Len(t, core.requests, 3, "batch refusal then original per-file limits")
				require.Len(t, e.ledger.reads, 2)
				return
			}
			require.Nil(t, got)
			require.Empty(t, e.loadedFileNodes)
			require.Empty(t, e.ledger.reads, "partial rows never become certified input")
			for _, path := range paths {
				require.Contains(t, e.ledger.owners, path, "a failed batch retains all attempted owners for torn-read classification")
			}
			switch mode {
			case "per_file_limit":
				require.ErrorIs(t, e.err, graph.ErrContractProjectionLimit)
				require.Len(t, core.requests, 2)
			case "partial_failure":
				require.ErrorIs(t, e.err, failure)
				require.Len(t, core.requests, 1, "arbitrary errors cannot fall back")
			case "canceled":
				require.ErrorIs(t, e.err, context.Canceled)
			}
		})
	}
}
func TestContractFileNodesBatchBoundsAndPreservesScratchMetadata(t *testing.T) {
	e, core, paths := contractBatchFixture(t, contractFrontierReadBatchSize+2)
	got := e.GetFileNodesByPaths(paths)
	require.NoError(t, e.err)
	require.Len(t, got, len(paths))
	require.Len(t, core.requests, 2)
	for _, requested := range core.requests {
		require.LessOrEqual(t, len(requested), contractFrontierReadBatchSize)
	}
	require.Len(t, e.ledger.reads, len(paths))
	changed := got[paths[0]][0]
	changed.Meta["input"] = "local-shape"
	e.AddNode(changed)
	again := e.GetFileNodesByPaths(paths)
	require.NoError(t, e.err)
	require.Len(t, core.requests, 2, "loaded files use scratch, never pristine selected rows")
	require.Equal(t, "local-shape", again[paths[0]][0].Meta["input"])
	require.Equal(t, "selected", core.Reader.GetNode(paths[0]).Meta["input"], "selected rows remain detached")
}
func TestContractFileNodesBatchKeepsRepositoryCensusAndExcludedAuthority(t *testing.T) {
	e, core, paths := contractBatchFixture(t, 2)
	other := "other/b.go"
	e.files[other] = ContractFollowupFile{Path: other, RepoPrefix: "other"}
	e.allowedRepos["other"] = true
	core.Reader.(*graph.Graph).AddNode(&graph.Node{ID: other, Kind: graph.KindFile, FilePath: other, RepoPrefix: "other"})
	excluded := "repo/excluded.go"
	e.excluded = map[string]bool{excluded: true}
	requested := append(slices.Clone(paths), other, excluded)
	got := e.GetFileNodesByPaths(requested)
	require.NoError(t, e.err)
	require.Len(t, got[other], 1)
	require.Empty(t, got[excluded])
	require.Len(t, core.requests, 2, "repositories are never merged into another selected scope")
	for _, r := range core.requests {
		for _, p := range r {
			require.Equal(t, e.files[r[0]].RepoPrefix, e.files[p].RepoPrefix)
		}
	}
	require.Len(t, e.ledger.reads, 4)
	require.Contains(t, e.ledger.owners, excluded)
	require.Nil(t, e.GetFileNodesByPaths([]string{"outside/not-admitted.go"}))
	require.ErrorContains(t, e.err, "outside accepted census")
}
