package indexer

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type initialYieldCopyProbe struct {
	calls  int
	before func()
	err    error
}

func (p *initialYieldCopyProbe) CopyPayloadGeneration(context.Context, int64, int64, string) (GenerationCopyCounts, error) {
	p.calls++
	if p.before != nil {
		p.before()
	}
	return GenerationCopyCounts{}, p.err
}

func TestClaimedInitialCopyYieldFallsBackOnceAndCompletesSameReservation(t *testing.T) {
	b, req, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, b, req)
	assertGenerationZeroMatchesTheReservedExtractors(t, b, req)
	probe := &initialYieldCopyProbe{err: store_sqlite.ErrGenerationCopyYielded}
	probe.before = func() {
		row, ok, err := b.Store.Catalog().GetViewGeneration(t.Context(), claim.GenerationID)
		if err != nil || !ok || row.State != store_sqlite.ViewGenerationBuilding {
			t.Errorf("copy was not building: %+v %v", row, err)
		}
	}
	id, report := buildClaimedBaseWithReport(t, b, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: req.RootPath, WorkspaceID: req.WorkspaceID, ProjectID: req.ProjectID, CopySource: probe})
	if id != claim.GenerationID || report.Coalesced || probe.calls != 1 {
		t.Fatalf("not exactly one copy and same reservation: id=%d report=%+v calls=%d", id, report, probe.calls)
	}
	plans := logs.FilterMessage("claimed dedicated base source plan").All()
	if len(plans) != 2 || plans[0].ContextMap()["route"] != "copy_generation_zero" || plans[1].ContextMap()["route"] != "reparse_git_tree" {
		t.Fatalf("wrong route sequence: %+v", plans)
	}
	reference, rreq, rclaim, _ := claimedCopyFixture(t)
	rid, _ := buildClaimedBaseWithReport(t, reference, ClaimedDedicatedBaseRequest{Claim: rclaim, RootPath: rreq.RootPath, WorkspaceID: rreq.WorkspaceID, ProjectID: rreq.ProjectID})
	gotN, gotE := claimedBasePayload(materializeClaimedBase(t, b, req.Identity.GraphID, id).Reader)
	wantN, wantE := claimedBasePayload(materializeClaimedBase(t, reference, rreq.Identity.GraphID, rid).Reader)
	if !reflect.DeepEqual(gotN, wantN) || !reflect.DeepEqual(gotE, wantE) {
		t.Fatal("fallback differs from independent committed-tree reparse")
	}
}

func TestClaimedInitialCopyParentCancellationDoesNotStartFallback(t *testing.T) {
	b, req, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, b, req)
	assertGenerationZeroMatchesTheReservedExtractors(t, b, req)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	probe := &initialYieldCopyProbe{err: store_sqlite.ErrGenerationCopyYielded, before: cancel}
	_, _, err := b.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: req.RootPath, WorkspaceID: req.WorkspaceID, ProjectID: req.ProjectID, CopySource: probe})
	if !errors.Is(err, context.Canceled) || errors.Is(err, store_sqlite.ErrGenerationCopyYielded) || probe.calls != 1 || logs.FilterMessage("claimed dedicated base source plan").Len() != 1 {
		t.Fatalf("cancellation retried: %v calls=%d", err, probe.calls)
	}
	row, ok, stateErr := b.Store.Catalog().GetViewGeneration(t.Context(), claim.GenerationID)
	if stateErr != nil || !ok || row.State != store_sqlite.ViewGenerationBuilding {
		t.Fatalf("canceled reservation lost: %+v %v", row, stateErr)
	}
}

type initialCopyCloseFailure struct {
	bulkLoadProbe
	failure error
}

func (p *initialCopyCloseFailure) EndGenerationBulkLoad() error {
	_ = p.bulkLoadProbe.EndGenerationBulkLoad()
	return p.failure
}

func TestClaimedInitialCopyCloseFailureDoesNotStartFallback(t *testing.T) {
	b, req, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, b, req)
	assertGenerationZeroMatchesTheReservedExtractors(t, b, req)
	broken := errors.New("bulk close refused")
	loader := &initialCopyCloseFailure{failure: broken}
	probe := &initialYieldCopyProbe{err: store_sqlite.ErrGenerationCopyYielded}
	_, _, err := b.BuildClaimedDedicatedBase(t.Context(), ClaimedDedicatedBaseRequest{Claim: claim, RootPath: req.RootPath, WorkspaceID: req.WorkspaceID, ProjectID: req.ProjectID, CopySource: probe, BulkLoad: loader})
	if !errors.Is(err, broken) || errors.Is(err, store_sqlite.ErrGenerationCopyYielded) || probe.calls != 1 || logs.FilterMessage("claimed dedicated base source plan").Len() != 1 {
		t.Fatalf("close failure hidden by fallback: %v calls=%d", err, probe.calls)
	}
}
