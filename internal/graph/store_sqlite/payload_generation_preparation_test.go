package store_sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestPayloadPublicationPreparationDoesNotHoldTheWriter(t *testing.T) {
	s := openPayloadStore(t)
	s.stopCheckpointLoop()
	seedPayloadControlPlane(t, s)
	id, _, err := s.BeginPayloadGeneration(t.Context(), payloadRequest())
	if err != nil {
		t.Fatal(err)
	}
	// Delay the actual initial catalog read at the read pool, independently
	// of payload publication. Foreground mutation uses the live writer.
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan struct{})
	var prepared *PreparedPayloadGenerationPublication
	var prepareErr error
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("metadata preparation did not join")
		}
	})
	before := s.db.Stats().WaitCount
	go func() {
		defer close(finished)
		prepared, prepareErr = s.PreparePayloadGenerationPublication(ctx, id)
	}()
	deadline := time.Now().Add(time.Second)
	for s.db.Stats().WaitCount == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.db.Stats().WaitCount == before {
		t.Fatal("initial metadata read never waited for its connection")
	}
	if s.publishDrains.Load() != 0 || s.payloadSealFor(id).state.Load() == payloadSealSealed {
		t.Fatal("metadata preparation entered the publication window")
	}
	if err := s.AddBatchChecked([]*graph.Node{{ID: "foreground-during-metadata", Kind: graph.KindFunction, Name: "Foreground"}}, nil); err != nil {
		t.Fatalf("foreground write while metadata read waits: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata preparation did not finish")
	}
	if prepareErr != nil || prepared == nil {
		t.Fatalf("preparation: %v", prepareErr)
	}
	if err := prepared.Publish(t.Context(), 1001); err != nil {
		t.Fatal(err)
	}
	if node := s.GetNode("foreground-during-metadata"); node == nil || node.Name != "Foreground" {
		t.Fatal("checked foreground write was lost")
	}
}

func TestPreparedPayloadPublicationRetainsLiveCatalogGuards(t *testing.T) {
	for _, scenario := range []string{"state_changed", "route_changed"} {
		t.Run(scenario, func(t *testing.T) {
			s := openPayloadStore(t)
			seedPayloadControlPlane(t, s)
			id, _, err := s.BeginPayloadGeneration(t.Context(), payloadRequest())
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := s.PreparePayloadGenerationPublication(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "state_changed" {
				if err := s.Catalog().SetViewGenerationState(t.Context(), id, ViewGenerationFailed, ViewGenerationBuilding); err != nil {
					t.Fatal(err)
				}
				if err := prepared.Publish(t.Context(), 1001); !errors.Is(err, ErrCatalogStaleGuard) {
					t.Fatalf("stale prepared publication: %v", err)
				}
				row, found, err := s.Catalog().GetViewGeneration(t.Context(), id)
				if err != nil || !found || row.State != ViewGenerationFailed {
					t.Fatalf("failed generation changed: %+v/%v", row, err)
				}
				return
			}
			otherRequest := payloadRequest()
			otherRequest.TreeOID = "other-ready-generation"
			other, _, err := s.BeginPayloadGeneration(t.Context(), otherRequest)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.PublishPayloadGeneration(t.Context(), other, 1001); err != nil {
				t.Fatal(err)
			}
			// An independent route mutation precedes this prepared publication.
			if err := s.Catalog().FlipCheckoutRouteSlot(t.Context(), FlipCheckoutRouteSlotRequest{
				CheckoutID: payloadCheckoutID, Slot: RouteSlotDirty, GenerationID: other,
				ExpectedRouteEpoch: 0, State: RouteActive,
			}); err != nil {
				t.Fatal(err)
			}
			if err := prepared.Publish(t.Context(), 1002); err != nil {
				t.Fatal(err)
			}
			if err := s.Catalog().FlipCheckoutRouteSlot(t.Context(), FlipCheckoutRouteSlotRequest{
				CheckoutID: payloadCheckoutID, Slot: RouteSlotDirty, GenerationID: id,
				ExpectedRouteEpoch: 0, State: RouteActive,
			}); !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("prepared publication bypassed route epoch: %v", err)
			}
			route, found, err := s.Catalog().GetCheckoutRoute(t.Context(), payloadCheckoutID)
			if err != nil || !found || route.DirtyGenerationID != other || route.RouteEpoch != 1 {
				t.Fatalf("stale route flip changed route: %+v/%v", route, err)
			}
		})
	}
}
