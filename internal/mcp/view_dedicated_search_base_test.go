package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

func TestDedicatedSearchDoesNotReadOrReplayPrimaryCorpus(t *testing.T) {
	f, _ := newAdjacencyMemoFixtureWithFiles(t, map[string]string{"owned.txt": "DedicatedContentWitness\n\nDedicatedContentWitness belongs to the committed root.\n"})
	primaryID := "repo/primary-only.go::PrimaryOnlySearchWitness"
	f.store.AddNode(&graph.Node{ID: primaryID, Name: "PrimaryOnlySearchWitness", Kind: graph.KindFunction, FilePath: "repo/primary-only.go", RepoPrefix: "repo"})
	require.NoError(t, f.store.AppendContent("repo", []graph.ContentFTSItem{{NodeID: primaryID, FilePath: "repo/primary-only.go", Body: "PrimaryOnlySearchWitness"}}))
	primaryHits, err := f.store.SearchContent("PrimaryOnlySearchWitness", "repo", 20)
	require.NoError(t, err)
	require.NotEmpty(t, primaryHits, "control must have primary-only content")
	authority := indexer.NewOutputGenerationAuthority(f.srv.materializer.Leases)
	advance := func() error {
		receipt, err := authority.Begin(context.Background(), indexer.OutputEntryWatcherDirScan, indexer.OutputMutationTarget{Kind: indexer.OutputGenerationLegacy, OwnerKey: "root:" + f.primary, RepoPrefix: "repo", RootPath: f.primary})
		if err != nil {
			return err
		}
		if !receipt.Witnessed() {
			return errors.New("primary mutation lacks witness")
		}
		return receipt.Complete()
	}
	require.NoError(t, advance())
	calls := 0
	req := makeReq("search_symbols", map[string]any{"query": "New", requireExactArgName: true, requireFreshArgName: true})
	ctx := WithSessionCWD(WithSessionID(context.Background(), "dedicated-search-base"), f.worktree)
	handler := f.srv.wrapToolHandler(func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls++
		view := requestViewFromContext(ctx)
		if view == nil || view.materialized == nil {
			return nil, errors.New("missing materialized dedicated view")
		}
		if view.materialized.ComposesBaseCorpus() {
			return nil, errors.New("fixture still composes generation zero")
		}
		if view.readsBaseCorpus {
			return nil, errors.New("dedicated view readsBaseCorpus is true")
		}
		if view.basePin != nil {
			return nil, errors.New("dedicated view acquired raw primary base pin")
		}
		if !view.excludesBaseCorpus() {
			return nil, errors.New("dedicated search includes generation zero candidates")
		}
		if view.reader.GetNode(primaryID) != nil || view.reader.GetNode("repo/edit.go::New") == nil {
			return nil, errors.New("selected graph isolation failed")
		}
		hits, err := view.content.SearchContent("PrimaryOnlySearchWitness", "repo", 20)
		if err != nil {
			return nil, err
		}
		if len(hits) != 0 {
			return nil, errors.New("dedicated content includes primary-only rows")
		}
		selected, err := view.content.SearchContent("DedicatedContentWitness", "repo", 20)
		if err != nil {
			return nil, err
		}
		if len(selected) == 0 {
			return nil, errors.New("committed content witness missing")
		}
		if err := advance(); err != nil {
			return nil, err
		}
		answer, err := f.srv.handleSearchSymbols(ctx, req)
		if err != nil {
			return nil, err
		}
		return answer, nil
	})
	result, err := handler(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, viewResultText(t, result))
	require.Contains(t, viewResultText(t, result), "repo/edit.go::New")
	require.NotContains(t, viewResultText(t, result), primaryID)
	require.Equal(t, 1, calls, "a primary-only mutation must not replay a dedicated search")
	require.Equal(t, true, resultFreshness(t, result)["exact"])
	require.Nil(t, resultFreshness(t, result)["fallback_reason"])
}

func TestDedicatedSearchRetainsGenerationAndRepositoryLeases(t *testing.T) {
	f, _ := newAdjacencyMemoFixture(t)
	leases := f.srv.materializer.Leases
	var drain *graphview.RepositoryDrain
	req := makeReq("get_symbol", map[string]any{"id": "repo/edit.go::New"})
	ctx := WithSessionCWD(WithSessionID(context.Background(), "dedicated-search-lifetime"), f.worktree)
	handler := f.srv.wrapToolHandler(func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		view := requestViewFromContext(ctx)
		if view == nil || view.materialized == nil {
			return nil, errors.New("missing dedicated view")
		}
		if view.materialized.ComposesBaseCorpus() || view.basePin != nil {
			return nil, errors.New("dedicated fixture acquired raw base pin")
		}
		for _, id := range view.materialized.Generations() {
			if !leases.InUse(id) {
				return nil, fmt.Errorf("generation %d is not leased", id)
			}
		}
		scope := requestRepositoryScopeFromContext(ctx)
		if scope.Holders() != 1 {
			return nil, errors.New("request repository scope has no independent live holder")
		}
		owners := scope.Owners()
		var owner graphview.RepositoryOwner
		for _, candidate := range owners {
			if candidate.RepoPrefix == "repo" {
				owner = candidate
			}
		}
		if owner.GraphID == "" {
			return nil, errors.New("serving scope has no repo owner")
		}
		var err error
		drain, err = leases.CloseRepositoryAdmission(owner)
		if err != nil {
			return nil, err
		}
		select {
		case <-drain.Done():
			return nil, errors.New("dedicated owner drained during a serving request")
		default:
		}
		if view.reader.GetNode("repo/edit.go::New") == nil {
			return nil, errors.New("dedicated reader lost selected node")
		}
		return mcplib.NewToolResultText(`{"ok":true}`), nil
	})
	_, err := handler(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, drain)
	// The real fixture also owns coordinator/publication readers. Join those
	// explicitly before asserting global owner drain; request return alone
	// is not an assertion that every independent reader has finished.
	require.NoError(t, f.srv.lifecycle.Close())
	select {
	case <-drain.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("dedicated owner did not drain after request")
	}
}
