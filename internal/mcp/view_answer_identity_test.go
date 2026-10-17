package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

// answerIdentityFields are the rider fields a worktree answer names the stack
// it was served from with.
var answerIdentityFields = []string{
	"route_epoch", "commit_generation_id", "dirty_generation_id",
	"checkout_incarnation", "view_fingerprint", "source_fingerprint",
}

// writeViewChainGeneration publishes one working-tree generation of the
// fixture worktree over base, carrying the working-copy fingerprint it was
// built from. Every chain member shares the layer and policy columns, which is
// what lets the materializer compose a chain parent under the routed top.
func writeViewChainGeneration(t *testing.T, store *store_sqlite.Store, graphID string, base int64, fingerprint string, nodes []*graph.Node, masks []store_sqlite.FileMask) int64 {
	t.Helper()
	generationID, handle, err := store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{
		OwnerKind:            "dedicated_graph",
		GraphID:              graphID,
		LayerID:              "layer-view-chain",
		CheckoutID:           viewTestWorktree,
		GenerationKind:       "dirty",
		BaseGenerationID:     base,
		TreeOID:              "tree-view-chain",
		LowerViewFingerprint: fingerprint,
		CreatedAt:            9000,
	})
	require.NoError(t, err)
	if len(nodes) > 0 {
		handle.AddBatch(nodes, nil)
	}
	if len(masks) > 0 {
		require.NoError(t, handle.SetFileMasks(masks))
	}
	require.NoError(t, store.PublishPayloadGeneration(context.Background(), generationID, 9500))
	return generationID
}

// routeViewChain routes the fixture worktree at [commit, D2] where D2 stands
// on D1 and D1 on the commit generation.
func routeViewChain(t *testing.T, stack *viewStack) (d1, d2 int64) {
	t.Helper()
	d1 = writeViewChainGeneration(t, stack.store, stack.graphID, stack.commit, "fingerprint-d1",
		[]*graph.Node{viewFileNode("repo/keep.go", 9), viewRepoNode("repo/keep.go::Keeper", "Keeper", graph.KindFunction, "repo/keep.go", 3)},
		[]store_sqlite.FileMask{{RepoPrefix: "repo", FilePath: "repo/keep.go", Mode: store_sqlite.OwnershipReplace}})
	d2 = writeViewChainGeneration(t, stack.store, stack.graphID, d1, "fingerprint-d2",
		[]*graph.Node{viewFileNode("repo/top.go", 4), viewRepoNode("repo/top.go::Top", "Top", graph.KindFunction, "repo/top.go", 2)},
		[]store_sqlite.FileMask{{RepoPrefix: "repo", FilePath: "repo/top.go", Mode: store_sqlite.OwnershipReplace}})
	routeViewCheckout(t, stack.store, stack.graphID, stack.commit, d2, store_sqlite.RouteActive)
	return d1, d2
}

func riderInt(t *testing.T, rider map[string]any, key string) int64 {
	t.Helper()
	value, ok := rider[key].(float64)
	require.True(t, ok, "rider field %s is %T (%v), want a number: %v", key, rider[key], rider[key], rider)
	return int64(value)
}

// A worktree answer names the exact stack it was read from, and the names are
// the ones the materialized view itself leased: the routed top is the dirty
// generation, and the commit generation is the first non-dirty one below it
// even when chain parents sit between them.
func TestWorktreeRiderNamesTheGenerationsThatAnswered(t *testing.T) {
	stack := newViewStack(t)
	d1, d2 := routeViewChain(t, stack)

	var generations []int64
	var epoch int64
	var fingerprint string
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", nil, func(ctx context.Context) (*mcplib.CallToolResult, error) {
		view := requestViewFromContext(ctx)
		require.NotNil(t, view)
		require.NotNil(t, view.materialized, "the worktree request was not served by a materialized view")
		generations = view.materialized.Generations()
		epoch = view.materialized.CheckoutRouteEpoch
		fingerprint = view.materialized.ID.Fingerprint()
		return mcplib.NewToolResultText(`{"ok":true}`), nil
	})
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Equal(t, []int64{stack.commit, d1, d2}, generations, "the view did not compose the dirty chain")

	rider := resultFreshness(t, res)
	t.Logf("worktree rider: %v", rider)
	for _, field := range answerIdentityFields {
		require.Contains(t, rider, field, "worktree rider lacks %s: %v", field, rider)
	}
	require.Equal(t, generations[len(generations)-1], riderInt(t, rider, "dirty_generation_id"),
		"the rider names a dirty generation the view did not serve")
	require.Equal(t, stack.commit, riderInt(t, rider, "commit_generation_id"),
		"the rider names a commit generation that is not the first non-dirty generation the view leased")
	require.Equal(t, epoch, riderInt(t, rider, "route_epoch"))
	require.Equal(t, fingerprint, rider["view_fingerprint"])
	require.Equal(t, "fingerprint-d2", rider["source_fingerprint"], "the source fingerprint is not the routed top's")
	require.Equal(t, "inc-worktree", rider["checkout_incarnation"])

	route, found, err := stack.store.Catalog().GetCheckoutRoute(context.Background(), viewTestWorktree)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, route.RouteEpoch, epoch, "the answer and the route disagree on the epoch the view pinned")
}

// A view whose route moved after it was materialized still names its own
// stack: the commit generation is recovered by walking the dirty chain, not
// read from the route that has since moved.
func TestWorktreeAnswerIdentityWalksTheChainWhenTheRouteMoved(t *testing.T) {
	stack := newViewStack(t)
	d1, d2 := routeViewChain(t, stack)

	view, err := stack.srv.materializer.MaterializeCheckout(context.Background(), viewTestWorktree)
	require.NoError(t, err)
	defer view.Close()

	// The route moves on: the old dirty generation is replaced.
	route, _, err := stack.store.Catalog().GetCheckoutRoute(context.Background(), viewTestWorktree)
	require.NoError(t, err)
	require.NoError(t, stack.store.Catalog().FlipCheckoutRoute(context.Background(), store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID: viewTestWorktree, GraphID: stack.graphID,
		CommitGenerationID: stack.commit, DirtyGenerationID: d1,
		State: store_sqlite.RouteActive, ExpectedRouteEpoch: route.RouteEpoch,
	}))

	checkout, _, err := stack.store.Catalog().GetCheckout(context.Background(), viewTestWorktree)
	require.NoError(t, err)
	identity := stack.srv.worktreeAnswerIdentity(context.Background(), checkout, view)
	require.NotNil(t, identity)
	require.Equal(t, d2, identity.dirtyGenerationID)
	require.Equal(t, stack.commit, identity.commitGenerationID, "the chain walk did not reach the commit generation")
	require.Equal(t, route.RouteEpoch, identity.routeEpoch, "the identity names the moved route's epoch, not the view's")
	require.Equal(t, "fingerprint-d2", identity.sourceFingerprint)
}

// Only a worktree view carries the answer identity: a request the base corpus
// serves keeps the rider it had.
func TestBaseRiderCarriesNoAnswerIdentity(t *testing.T) {
	stack := newViewStack(t)
	res, err := stack.callWithView(t, stack.repoRoot, "get_symbol", nil, captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	rider := resultFreshness(t, res)
	for _, field := range answerIdentityFields {
		require.NotContains(t, rider, field, "a base answer carries worktree identity %s: %v", field, rider)
	}
}

// provingFreshnessWaiter is the lifecycle's two halves: the proof and the
// ticket.
type provingFreshnessWaiter struct {
	*fakeFreshnessWaiter
	proofs atomic.Int32
	prove  func(checkoutID, root string) (indexer.CheckoutFreshProof, error)
}

func (p *provingFreshnessWaiter) ProveCheckoutFresh(_ context.Context, checkoutID, root string) (indexer.CheckoutFreshProof, error) {
	p.proofs.Add(1)
	return p.prove(checkoutID, root)
}

// A route the proof shows current answers fresh without a ticket, and the
// rider says so.
func TestRequireFreshAnsweredByTheProofTakesNoTicket(t *testing.T) {
	stack := newViewStack(t)
	waiter := &provingFreshnessWaiter{
		fakeFreshnessWaiter: &fakeFreshnessWaiter{},
		prove: func(checkoutID, root string) (indexer.CheckoutFreshProof, error) {
			return indexer.CheckoutFreshProof{
				Fresh: true, CommitGenerationID: stack.commit, DirtyGenerationID: stack.dirty,
				SampledAt: time.Now(), Sample: 7 * time.Millisecond,
			}, nil
		},
	}
	stack.srv.freshnessWaiter = waiter

	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.EqualValues(t, 1, waiter.proofs.Load())
	require.Empty(t, waiter.observed(), "a proven route still admitted a refresh ticket")

	rider := resultFreshness(t, res)
	require.Equal(t, true, rider["fresh"], "rider = %v", rider)
	require.Equal(t, freshViaProof, rider["fresh_via"], "rider = %v", rider)
	require.InDelta(t, 7.0, rider["fresh_sample_ms"], 0.001, "rider = %v", rider)
	require.NotContains(t, rider, "fresh_proof_reason")
	require.NotContains(t, rider, "publication_record", "a proof opens no publication record: %v", rider)
	require.Equal(t, stack.dirty, riderInt(t, rider, "dirty_generation_id"))
}

// A proof that sees the tree changed hands the wait to the ticket path, and
// the publication record opened for that ticket carries the instant the change
// was observed and the ticket's completion.
func TestRequireFreshFallsBackToTheTicketWhenTheProofRefuses(t *testing.T) {
	stack := newViewStack(t)
	sampledAt := time.Now()
	waiter := &provingFreshnessWaiter{
		fakeFreshnessWaiter: &fakeFreshnessWaiter{
			answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
				return settledTicket(checkoutID, root, uint64(stack.dirty)), nil
			},
		},
		prove: func(checkoutID, root string) (indexer.CheckoutFreshProof, error) {
			return indexer.CheckoutFreshProof{
				Reason: indexer.FreshProofSnapshotDiffers, CommitGenerationID: stack.commit, DirtyGenerationID: stack.dirty,
				SampledAt: sampledAt, Sample: 3 * time.Millisecond,
			}, nil
		},
	}
	stack.srv.freshnessWaiter = waiter

	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.False(t, res.IsError, viewResultText(t, res))
	require.Len(t, waiter.observed(), 1, "the refused proof did not fall back to exactly one ticket")

	rider := resultFreshness(t, res)
	require.Equal(t, true, rider["fresh"], "rider = %v", rider)
	t.Logf("ticket-settled rider: %v", rider)
	require.Equal(t, freshViaTicket, rider["fresh_via"], "rider = %v", rider)
	require.Equal(t, indexer.FreshProofSnapshotDiffers, rider["fresh_proof_reason"], "rider = %v", rider)
	key, _ := rider["publication_record"].(string)
	require.NotEmpty(t, key, "a ticket-settled wait names no publication record: %v", rider)

	record, ok := indexer.DefaultPublicationPhases().Lookup(key)
	require.True(t, ok, "the named publication record %q does not exist", key)
	snapshot := record.Snapshot()
	require.Equal(t, viewTestWorktree, snapshot.CheckoutID)
	require.Equal(t, indexer.PublicationSourceFreshRequest, snapshot.Source)
	require.True(t, snapshot.Terminal, "the completed wait left its record open: %+v", snapshot)
	require.Equal(t, stack.dirty, snapshot.DirtyGenerationID, "the record does not name the published generation")
	phases := map[indexer.PublicationPhase]int64{}
	for _, phase := range snapshot.Phases {
		phases[phase.Phase] = phase.OffsetNS
	}
	for _, want := range []indexer.PublicationPhase{
		indexer.PublicationReceived, indexer.PublicationChangeObserved,
		indexer.PublicationTicketEnqueued, indexer.PublicationTicketCompleted,
	} {
		require.Contains(t, phases, want, "record lacks %s: %+v", want, snapshot)
	}
	require.LessOrEqual(t, phases[indexer.PublicationTicketEnqueued], phases[indexer.PublicationTicketCompleted])
}

// A proof that errors says nothing about the route; the ticket decides, and
// no proof reason is invented for the rider.
func TestRequireFreshProofErrorLeavesTheTicketToDecide(t *testing.T) {
	stack := newViewStack(t)
	waiter := &provingFreshnessWaiter{
		fakeFreshnessWaiter: &fakeFreshnessWaiter{
			answer: func(call int, checkoutID, root string) (*indexer.CheckoutRefreshTicket, error) {
				return settledTicket(checkoutID, root, uint64(stack.dirty)), nil
			},
		},
		prove: func(string, string) (indexer.CheckoutFreshProof, error) {
			return indexer.CheckoutFreshProof{}, errors.New("sampler failed")
		},
	}
	stack.srv.freshnessWaiter = waiter
	res, err := stack.callWithView(t, stack.worktreeRoot, "get_symbol", freshArgs(nil, time.Minute), captureReader(stack.srv, new(graph.Reader)))
	require.NoError(t, err)
	require.Len(t, waiter.observed(), 1)
	rider := resultFreshness(t, res)
	require.Equal(t, true, rider["fresh"], "rider = %v", rider)
	require.Equal(t, freshViaTicket, rider["fresh_via"])
	require.NotContains(t, rider, "fresh_proof_reason")
}

// An MCP edit's publication record opens at the tool call's arrival, carries
// the disk commit and the ticket's admission and completion, and is what the
// mutation receipt and the graph-refresh receipt render.
func TestMutationReceiptRendersPublicationPhases(t *testing.T) {
	mutation, done, path := newReceiptCheckoutMutation(t)
	s := &Server{mutationReindexWait: time.Nanosecond}
	arrived := time.Now()
	ctx := withToolReceivedAt(context.Background(), arrived)
	ctx, _ = withMutationCommitNote(ctx)
	ctx = withCheckoutMutation(ctx, mutation, filepath.Dir(path))
	data := []byte("package committed\n")
	record, err := s.commitFileMutation(ctx, "write_file", "", "", "edit.go", path, data, 0o600)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	mutation.ticket.ContentHash = hex.EncodeToString(sum[:])

	outcome := s.mutationReindexState(ctx, path)
	require.True(t, outcome.Pending, "%+v", outcome)
	record.recordGraph(outcome)

	status := s.mutationStatusPayload(record)
	pending, ok := status["publication_phases"].(indexer.PublicationPhaseSnapshot)
	require.True(t, ok, "the mutation receipt renders no publication phases: %#v", status)
	require.Equal(t, outcome.Receipt, pending.Key)
	require.Equal(t, indexer.PublicationSourceMCPEdit, pending.Source)
	require.EqualValues(t, 7, pending.Ticket)
	require.False(t, pending.Terminal)
	offsets := map[indexer.PublicationPhase]int64{}
	for _, phase := range pending.Phases {
		offsets[phase.Phase] = phase.OffsetNS
	}
	require.Contains(t, offsets, indexer.PublicationReceived)
	require.Contains(t, offsets, indexer.PublicationReceiptCommitted)
	require.Contains(t, offsets, indexer.PublicationTicketEnqueued)
	require.LessOrEqual(t, offsets[indexer.PublicationReceiptCommitted], offsets[indexer.PublicationTicketEnqueued],
		"the disk commit is recorded after the ticket that follows it")

	done <- indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 29, Reindexed: true}
	waitCheckoutReceipt(t, s, outcome.Receipt)
	var final indexer.PublicationPhaseSnapshot
	require.Eventually(t, func() bool {
		final, _ = s.mutationStatusPayload(record)["publication_phases"].(indexer.PublicationPhaseSnapshot)
		return final.Terminal
	}, 5*time.Second, time.Millisecond, "the ticket's completion never closed the record")
	require.EqualValues(t, 29, final.DirtyGenerationID)
	require.Equal(t, indexer.PublicationTicketCompleted, final.Phases[len(final.Phases)-1].Phase)

	// The graph-refresh receipt is scoped to the checkout that admitted it.
	payload, ok := s.graphRefreshReceiptPayload(ctx, outcome.Receipt)
	require.True(t, ok, "the graph-refresh receipt is not visible to its own checkout")
	require.Contains(t, payload, "publication_phases", "the graph-refresh receipt renders no publication phases")
}
