package indexer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A published fold's catalog readback is not a payload mutation. Park at the
// actual after-publication dispatch, with the real payload already sealed, and
// require foreground admission plus SQL before the readback can return.
func TestImportFoldPublishedReadbackDoesNotHoldLane(t *testing.T) {
	f := newCoordinatorAncestryFixture(t)
	id, _, err := f.c.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
		OwnerKind: f.request.Identity.OwnerKind, GraphID: f.request.Identity.GraphID,
		CheckoutID: f.request.Identity.CheckoutID, GenerationKind: DirtyLayerGenerationKind,
		TreeOID: f.request.Identity.TreeOID, ConfigHash: f.request.Identity.ConfigHash,
		ExtractorVersions: f.request.Identity.ExtractorVersions, ResolverVersion: f.request.Identity.ResolverVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := NewViewBuildGate()
	gate.Open()
	ctx, cancel := context.WithCancel(t.Context())
	entered, resume, finished, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var unblock sync.Once
	var release func()
	lane := &importBuildLane{gate: gate, detached: true,
		detach: func() bool { release(); release = func() {}; return true },
		resume: func(ctx context.Context, _ bool) (context.Context, error) {
			var err error
			release, err = gate.Acquire(ctx, ViewBuildBackground)
			return ctx, err
		},
	}
	t.Cleanup(func() {
		cancel()
		unblock.Do(func() { close(resume) })
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("published readback did not join")
		}
	})
	go func() {
		defer close(finished)
		defer func() {
			if release != nil {
				release()
			}
		}()
		admission := &importFoldPublication{
			beforePublish: func(ctx context.Context) error { _, err := lane.reenter(ctx, false); return err },
			afterPublish: func() {
				lane.leave()
				close(entered)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			},
		}
		err := publishCopiedGeneration(ctx, id, admission, f.c.store.PreparePayloadGenerationPublication)
		if err == nil {
			row, found, readErr := f.c.catalog.GetViewGeneration(ctx, id)
			if readErr != nil {
				err = readErr
			} else if !found || row.State != store_sqlite.ViewGenerationReady {
				err = errors.New("published fold readback is not ready")
			}
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publication readback was not reached")
	}
	row, found, err := f.c.catalog.GetViewGeneration(t.Context(), id)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("parked published payload: %+v/%v", row, err)
	}
	probe, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	foregroundRelease, err := gate.Acquire(probe, ViewBuildInteractive)
	if err != nil {
		t.Fatalf("published readback held lane: %v", err)
	}
	err = f.c.store.AddBatchChecked([]*graph.Node{{ID: "postpublish-foreground", Kind: graph.KindFunction, Name: "Foreground"}}, nil)
	foregroundRelease()
	if err != nil {
		t.Fatal(err)
	}
	unblock.Do(func() { close(resume) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("published readback never finished")
	}
}

func TestImportRoutePrewarmAdmitsForegroundAndRetainsGuards(t *testing.T) {
	for _, mode := range []string{"success", "stale_route", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			gate := NewViewBuildGate()
			gate.Open()
			c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate})
			c.cycle(t.Context())
			route := f.route()
			before := route
			parent, found := f.generation(route.DirtyGenerationID)
			if !found || !servableGeneration(parent.State) {
				t.Fatal("initial route has no ready payload")
			}
			id, _, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{
				OwnerKind: parent.OwnerKind, GraphID: parent.GraphID, LayerID: parent.LayerID,
				CheckoutID: parent.CheckoutID, GenerationKind: parent.GenerationKind,
				BaseGenerationID: parent.BaseGenerationID, LowerViewFingerprint: "prewarm-candidate",
				TreeOID: parent.TreeOID, ConfigHash: parent.ConfigHash,
				ExtractorVersions: parent.ExtractorVersions, ResolverVersion: parent.ResolverVersion,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.PublishPayloadGeneration(t.Context(), id, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			release, err := gate.Acquire(ctx, ViewBuildBackground)
			if err != nil {
				t.Fatal(err)
			}
			entered, resume, finished, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var unblock sync.Once
			lane := &importBuildLane{gate: gate,
				detach: func() bool { release(); release = func() {}; return true },
				resume: func(ctx context.Context, _ bool) (context.Context, error) {
					next, err := gate.Acquire(ctx, ViewBuildBackground)
					if err == nil {
						release = next
					}
					return ctx, err
				},
			}
			ctx = context.WithValue(ctx, importBuildLaneKey{}, lane)
			c.prewarm = func(ctx context.Context, ids []int64) {
				if len(ids) != 2 || ids[0] != route.CommitGenerationID || ids[1] != id {
					t.Errorf("prewarm scope: %v", ids)
				}
				close(entered)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			}
			t.Cleanup(func() {
				cancel()
				unblock.Do(func() { close(resume) })
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("prewarm flip did not join")
				}
				release()
			})
			go func() { defer close(finished); done <- c.flip(ctx, &route, store_sqlite.RouteSlotDirty, id) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("prewarm was not reached")
			}
			probe, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
			foregroundRelease, err := gate.Acquire(probe, ViewBuildInteractive)
			stop()
			if err != nil {
				t.Fatalf("prewarm held lane: %v", err)
			}
			err = f.store.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "prewarm-foreground"})
			foregroundRelease()
			if err != nil {
				t.Fatal(err)
			}
			if got := f.route(); got != before {
				t.Fatalf("private prewarm changed route: %+v", got)
			}
			if mode == "stale_route" {
				err := f.catalog.FlipCheckoutRouteSlot(t.Context(), store_sqlite.FlipCheckoutRouteSlotRequest{CheckoutID: f.checkoutID,
					Slot: store_sqlite.RouteSlotDirty, GenerationID: before.DirtyGenerationID,
					ExpectedRouteEpoch: before.RouteEpoch, State: store_sqlite.RouteActive})
				if err != nil {
					t.Fatal(err)
				}
			} else if mode == "cancelled" {
				cancel()
			}
			unblock.Do(func() { close(resume) })
			select {
			case err := <-done:
				switch mode {
				case "success":
					if err != nil || f.route().DirtyGenerationID != id || !gate.Stats().Active {
						t.Fatalf("flip after prewarm: %v/%+v", err, f.route())
					}
				case "stale_route":
					if !errors.Is(err, errRouteMoved) || f.route().DirtyGenerationID != before.DirtyGenerationID {
						t.Fatalf("stale flip: %v/%+v", err, f.route())
					}
				case "cancelled":
					if !errors.Is(err, context.Canceled) || f.route() != before {
						t.Fatalf("cancelled flip: %v/%+v", err, f.route())
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("flip never finished")
			}
		})
	}
}
