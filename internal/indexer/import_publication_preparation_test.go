package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestImportPublicationPreparesMetadataBeforeLaneAdmission(t *testing.T) {
	f := newCoordinatorAncestryFixture(t)
	identity := f.request.Identity
	id, _, err := f.c.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: identity.OwnerKind, GraphID: identity.GraphID,
		CheckoutID: identity.CheckoutID, GenerationKind: DirtyLayerGenerationKind,
		TreeOID: identity.TreeOID, ConfigHash: identity.ConfigHash,
		ExtractorVersions: identity.ExtractorVersions, ResolverVersion: identity.ResolverVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := NewViewBuildGate()
	gate.Open()
	ctx, cancel := context.WithCancel(t.Context())
	entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var result error
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("publication did not join")
		}
	})
	go func() {
		defer close(finished)
		var release func()
		defer func() {
			if release != nil {
				release()
			}
		}()
		admission := &importFoldPublication{beforePublish: func(ctx context.Context) error {
			var err error
			release, err = gate.Acquire(ctx, ViewBuildBackground)
			return err
		}}
		// Fault-inject slow metadata preparation at the actual per-call
		// dispatch boundary, without a global hook or an alternate publisher.
		prepare := func(ctx context.Context, generation int64) (*store_sqlite.PreparedPayloadGenerationPublication, error) {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return f.c.store.PreparePayloadGenerationPublication(ctx, generation)
		}
		result = publishCopiedGeneration(ctx, id, admission, prepare)
		if result == nil && !gate.Stats().Active {
			t.Error("successful publication released its caller's physical lane")
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata preparation was not reached")
	}
	foregroundCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	foregroundRelease, err := gate.Acquire(foregroundCtx, ViewBuildInteractive)
	if err != nil {
		t.Fatalf("metadata preparation held the physical lane: %v", err)
	}
	writeErr := f.c.store.AddBatchChecked([]*graph.Node{{ID: "metadata-foreground", Kind: graph.KindFunction, Name: "Foreground"}}, nil)
	foregroundRelease()
	if writeErr != nil {
		t.Fatalf("checked foreground SQL during preparation: %v", writeErr)
	}
	close(resume)
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not finish")
	}
	if result != nil {
		t.Fatal(result)
	}
	row, found, err := f.c.catalog.GetViewGeneration(t.Context(), id)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("successful publication: %+v/%v", row, err)
	}
}

func TestImportPublicationCancellationDoesNotAdmitOrPublish(t *testing.T) {
	f := newCoordinatorAncestryFixture(t)
	identity := f.request.Identity
	id, _, err := f.c.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: identity.OwnerKind, GraphID: identity.GraphID,
		CheckoutID: identity.CheckoutID, GenerationKind: DirtyLayerGenerationKind,
		TreeOID: identity.TreeOID, ConfigHash: identity.ConfigHash,
		ExtractorVersions: identity.ExtractorVersions, ResolverVersion: identity.ResolverVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	admitted := false
	admission := &importFoldPublication{beforePublish: func(context.Context) error {
		admitted = true
		return nil
	}}
	err = publishCopiedGeneration(ctx, id, admission, f.c.store.PreparePayloadGenerationPublication)
	if !errors.Is(err, context.Canceled) || admitted {
		t.Fatalf("cancelled preparation admitted=%v error=%v", admitted, err)
	}
	row, found, err := f.c.catalog.GetViewGeneration(t.Context(), id)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationBuilding || row.PublishedAt != 0 {
		t.Fatalf("cancelled preparation published: %+v/%v", row, err)
	}
}
