package indexer

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func dedicatedExistingClaimRequest(claim store_sqlite.DedicatedBaseBuildClaim) store_sqlite.ClaimDedicatedBaseBuildRequest {
	return store_sqlite.ClaimDedicatedBaseBuildRequest{
		ExistingGenerationID: claim.GenerationID, Desire: claim.Desire,
		ExpectedActiveGenerationID: claim.ExpectedActiveGenerationID, AttemptToken: claim.AttemptToken,
		BaseGenerationID: claim.BaseGenerationID, LayerID: claim.LayerID, LowerViewFingerprint: claim.LowerViewFingerprint,
	}
}

func TestDedicatedExistingClaimFailedReplacementCannotAllocate(t *testing.T) {
	builder, fixture, first := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	catalog := builder.Store.Catalog()
	if err := catalog.FailDedicatedBaseBuild(ctx, store_sqlite.FailDedicatedBaseBuildRequest{Claim: first, Error: "first attempt failed"}); err != nil {
		t.Fatal(err)
	}
	replacement, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: first.Desire, ExpectedActiveGenerationID: first.ExpectedActiveGenerationID, AttemptToken: "replacement-B",
	})
	if err != nil || replacement.GenerationID == first.GenerationID || replacement.Status != "allocated" {
		t.Fatalf("replacement=%+v %v", replacement, err)
	}
	if err := catalog.FailDedicatedBaseBuild(ctx, store_sqlite.FailDedicatedBaseBuildRequest{Claim: replacement, Error: "replacement failed"}); err != nil {
		t.Fatal(err)
	}
	before, _, err := catalog.DedicatedBasePublication(ctx, first.Desire.Authority.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, fixture.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = catalog.ClaimDedicatedBaseBuild(ctx, dedicatedExistingClaimRequest(first))
	if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("old A validation after failed B=%v", err)
	}
	after, _, err := catalog.DedicatedBasePublication(ctx, first.Desire.Authority.GraphID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("validation replaced failed current claim: before=%+v after=%+v err=%v", before, after, err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	// Zero mode deliberately preserves ordinary retry/allocation behavior.
	retry, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: first.Desire, ExpectedActiveGenerationID: first.ExpectedActiveGenerationID, AttemptToken: "ordinary-retry-C",
	})
	if err != nil || retry.Status != "allocated" || retry.GenerationID == first.GenerationID || retry.GenerationID == replacement.GenerationID {
		t.Fatalf("ordinary retry compatibility: %+v %v", retry, err)
	}
}

func TestDedicatedExistingClaimLiveStatesAreReadOnly(t *testing.T) {
	for _, state := range []string{"building", "ready", "adopted"} {
		t.Run(state, func(t *testing.T) {
			builder, fixture, claim := privateClaimedDedicatedFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if state != "building" {
				// This runs the actual initial builder's opted-in validation call.
				id, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: fixture.RootPath})
				if err != nil || id != claim.GenerationID {
					t.Fatalf("initial builder=%d %v", id, err)
				}
			}
			if state == "adopted" {
				if _, err := builder.Store.Catalog().AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
					t.Fatal(err)
				}
			}
			check, err := installDedicatedWriteAudit(ctx, fixture.StorePath)
			if err != nil {
				t.Fatal(err)
			}
			for range 4 {
				validated, err := builder.Store.Catalog().ClaimDedicatedBaseBuild(ctx, dedicatedExistingClaimRequest(claim))
				wantStatus := "ready"
				if state == "building" {
					wantStatus = "building"
				}
				if err != nil || validated.GenerationID != claim.GenerationID || validated.AttemptToken != claim.AttemptToken ||
					validated.Status != wantStatus || validated.AlreadyAdopted != (state == "adopted") {
					t.Fatalf("%s validation=%+v %v", state, validated, err)
				}
			}
			if state != "building" {
				id, report, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: "/nonexistent/initial-ready-replay"})
				if err != nil || id != claim.GenerationID || !report.Coalesced {
					t.Fatalf("initial ready reuse=%d %+v %v", id, report, err)
				}
			}
			if err := check(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDedicatedExistingClaimRejectsWrongIDTokenAndNegative(t *testing.T) {
	builder, fixture, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	check, err := installDedicatedWriteAudit(ctx, fixture.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "token", "negative", "desire"} {
		t.Run(field, func(t *testing.T) {
			request := dedicatedExistingClaimRequest(claim)
			switch field {
			case "id":
				request.ExistingGenerationID++
			case "token":
				request.AttemptToken = "unrelated-attempt"
			case "negative":
				request.ExistingGenerationID = -1
			case "desire":
				request.Desire.Epoch++
			}
			_, err := builder.Store.Catalog().ClaimDedicatedBaseBuild(ctx, request)
			if err == nil || (field != "negative" && !errors.Is(err, store_sqlite.ErrCatalogStaleGuard)) {
				t.Fatalf("%s guard=%v", field, err)
			}
		})
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func TestDedicatedExistingClaimDirectFailureIsReadOnly(t *testing.T) {
	builder, fixture, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	if err := builder.Store.Catalog().FailDedicatedBaseBuild(ctx, store_sqlite.FailDedicatedBaseBuildRequest{Claim: claim, Error: "failed"}); err != nil {
		t.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, fixture.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Store.Catalog().ClaimDedicatedBaseBuild(ctx, dedicatedExistingClaimRequest(claim)); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("failed validation=%v", err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkDedicatedExistingClaimValidation(b *testing.B) {
	for _, mode := range []string{"ordinary", "validate_only"} {
		b.Run(mode, func(b *testing.B) {
			builder, fixture, claim := privateClaimedDedicatedFixture(b)
			ctx := context.Background()
			request := dedicatedExistingClaimRequest(claim)
			if mode == "ordinary" {
				request.ExistingGenerationID = 0
			}
			check, err := installDedicatedWriteAudit(ctx, fixture.StorePath)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				validated, err := builder.Store.Catalog().ClaimDedicatedBaseBuild(ctx, request)
				if err != nil || validated.GenerationID != claim.GenerationID || validated.AttemptToken != claim.AttemptToken {
					b.Fatalf("claim validation=%+v %v", validated, err)
				}
			}
			b.StopTimer()
			if err := check(); err != nil {
				b.Fatal(err)
			}
		})
	}
}
