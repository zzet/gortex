package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A daemon stopped after the claimed base's payload landed and before it was
// published keeps its reservation, and the next start resumes it: the claim
// still names the generation, the generation is still building, the copy is
// not taken again, and the published base is the one an uninterrupted build
// publishes.
func TestClaimedDedicatedBaseResumesAfterAStopBeforePublication(t *testing.T) {
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	probe := &copyProbe{store: builder.Store}

	// First start: stopped once the payload is in (just before publication).
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	_, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: probe,
		PrePublish: func(context.Context, int64) error {
			stop()
			return context.Canceled
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the stopped build returned %v, want the cancellation", err)
	}
	if len(probe.calls) != 1 {
		t.Fatalf("fixture precondition: the first start copied %d times", len(probe.calls))
	}
	catalog := builder.Store.Catalog()
	row, found, err := catalog.GetViewGeneration(context.Background(), claim.GenerationID)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationBuilding {
		t.Fatalf("after the stop the reservation is %+v (found=%v err=%v); want it still building", row.State, found, err)
	}
	publication, _, err := catalog.DedicatedBasePublication(context.Background(), claim.Desire.Authority.GraphID)
	if err != nil || publication.AttemptState != "building" || publication.Claim.GenerationID != claim.GenerationID {
		t.Fatalf("after the stop the claim is %q on generation %d (err=%v); want it still building on %d",
			publication.AttemptState, publication.Claim.GenerationID, err, claim.GenerationID)
	}

	// Next start: the claim coalesces onto the live reservation.
	again, err := catalog.ClaimDedicatedBaseBuild(context.Background(), store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: claim.Desire, AttemptToken: "private-attempt-two", ExpectedActiveGenerationID: claim.ExpectedActiveGenerationID,
		ProvenanceCommitOID: request.Identity.ProvenanceCommitOID, CreatedAt: 2,
	})
	if err != nil || again.GenerationID != claim.GenerationID {
		t.Fatalf("the next start's claim = %+v (err=%v); want the reservation %d", again, err, claim.GenerationID)
	}
	logs.TakeAll()
	resumeProbe := &copyProbe{store: builder.Store}
	id := buildClaimedBase(t, builder, ClaimedDedicatedBaseRequest{
		Claim: again, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: resumeProbe,
	})
	if route, reason := claimedRoute(t, logs); route != claimedBaseResumeRoute {
		t.Fatalf("route = %q (%s), want the resume of the interrupted payload", route, reason)
	}
	if len(resumeProbe.calls) != 0 {
		t.Fatalf("the resumed build copied generation zero again: %+v", resumeProbe.calls)
	}

	// The oracle: an uninterrupted copy build of an independent fixture.
	reference, referenceRequest, referenceClaim, _ := claimedCopyFixture(t)
	indexGenerationZero(t, reference, referenceRequest)
	referenceID := buildClaimedBase(t, reference, ClaimedDedicatedBaseRequest{
		Claim: referenceClaim, RootPath: referenceRequest.RootPath, WorkspaceID: referenceRequest.WorkspaceID,
		ProjectID: referenceRequest.ProjectID, CopySource: &copyProbe{store: reference.Store},
	})
	gotNodes, gotEdges := claimedBasePayload(materializeClaimedBase(t, builder, request.Identity.GraphID, id).Reader)
	wantNodes, wantEdges := claimedBasePayload(materializeClaimedBase(t, reference, referenceRequest.Identity.GraphID, referenceID).Reader)
	if len(gotNodes) == 0 || len(gotEdges) == 0 {
		t.Fatalf("the resumed base composed empty: %d nodes, %d edges", len(gotNodes), len(gotEdges))
	}
	if !equalStrings(gotNodes, wantNodes) || !equalStrings(gotEdges, wantEdges) {
		t.Fatalf("the resumed base differs from an uninterrupted build: nodes %d vs %d, edges %d vs %d",
			len(gotNodes), len(wantNodes), len(gotEdges), len(wantEdges))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A claimed build that failed (not a stop) leaves a failed generation holding
// its payload. Once the next claim has bound a new generation the failed one
// is retired at once — its catalog row and its rows are gone — instead of
// waiting for the retirement sweep. A claim that is still live is never
// retired.
func TestReplacedFailedDedicatedClaimIsRetiredPromptly(t *testing.T) {
	builder, request, claim, _ := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	ctx := context.Background()
	_, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, CopySource: &copyProbe{store: builder.Store},
		PrePublish: func(context.Context, int64) error { return errors.New("injected publication failure") },
	})
	if err == nil {
		t.Fatal("the injected failure did not fail the build")
	}
	catalog := builder.Store.Catalog()
	failed, _, err := catalog.DedicatedBasePublication(ctx, claim.Desire.Authority.GraphID)
	if err != nil || failed.AttemptState != "failed" {
		t.Fatalf("fixture precondition: the claim is %q (err=%v), want failed", failed.AttemptState, err)
	}
	if len(builder.Store.AtGeneration(claim.GenerationID).GetRepoNodesLight(request.RepoPrefix)) == 0 {
		t.Fatal("fixture precondition: the failed generation holds no rows")
	}

	// A live claim is never retired, whatever the new claim names.
	if retired, err := retireReplacedDedicatedClaim(ctx, builder.Store, failed.Claim, "building",
		store_sqlite.DedicatedBaseBuildClaim{GenerationID: claim.GenerationID + 100}, nil); retired || err != nil {
		t.Fatalf("a live claim was retired (retired=%v err=%v)", retired, err)
	}

	next, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: claim.Desire, AttemptToken: "private-attempt-two", ExpectedActiveGenerationID: claim.ExpectedActiveGenerationID,
		ProvenanceCommitOID: request.Identity.ProvenanceCommitOID, CreatedAt: 2,
	})
	if err != nil || next.GenerationID == claim.GenerationID {
		t.Fatalf("the next claim = %+v (err=%v); want a new generation", next, err)
	}
	retired, err := retireReplacedDedicatedClaim(ctx, builder.Store, failed.Claim, failed.AttemptState, next, nil)
	if err != nil || !retired {
		t.Fatalf("the replaced failed claim was not retired (retired=%v err=%v)", retired, err)
	}
	if _, found, _ := catalog.GetViewGeneration(ctx, claim.GenerationID); found {
		t.Fatal("the replaced failed generation is still in the catalog")
	}
	if rows := builder.Store.AtGeneration(claim.GenerationID).GetRepoNodesLight(request.RepoPrefix); len(rows) != 0 {
		t.Fatalf("the replaced failed generation still holds %d rows", len(rows))
	}
}
