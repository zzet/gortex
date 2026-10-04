package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// Installed only after inventory planning on the unwrapped Git source.
// This blocks real runPass intake, not preparation or PrePublish.
type privateRetirementReadBarrier struct {
	source.ContentSource
	ctx     context.Context
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
	opens   *atomic.Int32
	closes  *atomic.Int32
}

func (s *privateRetirementReadBarrier) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	if filepath.Base(path) == "base.go" {
		s.opens.Add(1)
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-s.ctx.Done():
			return nil, source.FileMeta{}, s.ctx.Err()
		}
	}
	return s.ContentSource.Open(path)
}

func (s *privateRetirementReadBarrier) Close() error {
	s.closes.Add(1)
	return s.ContentSource.Close()
}

func TestPrivatePhysicalBuilderRetirementFence(t *testing.T) {
	// A red race is meaningful only after the same real producer succeeds.
	for _, mode := range []string{"healthy", "retiring_during_source_read"} {
		t.Run(mode, func(t *testing.T) {
			builder, fixture, _ := privateDedicatedBuilderFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			identity := fixture.Identity
			identity.GenerationKind = "commit"
			id, _, adopted, err := builder.Store.BeginPayloadGenerationWithStatus(ctx, store_sqlite.PayloadGenerationRequest{
				OwnerKind: identity.OwnerKind, GraphID: identity.GraphID, CheckoutID: identity.CheckoutID,
				GenerationKind: identity.GenerationKind, LayerID: "private-physical-retirement",
				TreeOID:    identity.TreeOID,
				ConfigHash: identity.ConfigHash, ExtractorVersions: identity.ExtractorVersions, ResolverVersion: identity.ResolverVersion,
			})
			if err != nil || adopted || id <= 0 {
				t.Fatalf("ordinary unclaimed allocation: id=%d adopted=%v err=%v", id, adopted, err)
			}
			// Explicit qualification: this owns the shared physical helper
			// boundary, not public Build/claimed-generation ingress.
			handle, err := builder.Store.AtManagedGeneration(id)
			if err != nil {
				t.Fatal(err)
			}
			baseID := fixture.RepoPrefix + "/base-sentinel.go::BaseSentinel"
			builder.Store.AddBatch([]*graph.Node{{ID: baseID, Name: "BaseSentinel", Kind: graph.KindFunction,
				FilePath: fixture.RepoPrefix + "/base-sentinel.go", RepoPrefix: fixture.RepoPrefix}}, nil)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var work sync.WaitGroup
			// Registered after fixture Close: drain all participants first.
			t.Cleanup(func() { unblock(); cancel(); work.Wait() })
			var preparations, opens, closes atomic.Int32
			prepare := func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
				preparations.Add(1)
				target, err := source.NewGitTreeSource(ctx, fixture.RootPath, identity.TreeOID)
				if err != nil {
					return nil, buildPlan{}, BuildReport{}, err
				}
				plan, report, err := planDedicatedSnapshot(ctx, target)
				if err != nil {
					_ = target.Close()
					return nil, buildPlan{}, BuildReport{}, err
				}
				return &privateRetirementReadBarrier{ContentSource: target, ctx: ctx, entered: entered,
					release: release, opens: &opens, closes: &closes}, plan, report, nil
			}
			req := BuildRequest{Identity: identity, Base: graph.New(), RootPath: fixture.RootPath,
				RepoPrefix: fixture.RepoPrefix, WorkspaceID: fixture.WorkspaceID, ProjectID: fixture.ProjectID}
			type outcome struct {
				id     int64
				report BuildReport
				err    error
				panic  any
			}
			var leader outcome
			leaderDone := make(chan struct{})
			followerDone := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			followerErrors := [2]error{}
			started, followers := false, 0
			unreadID := fixture.RepoPrefix + "/retirement-unread.go::Unread"
			startAndPin := func() error {
				if started {
					return errors.New("physical fixture leader started twice")
				}
				started = true
				work.Add(1)
				go func() {
					defer work.Done()
					defer close(leaderDone)
					defer func() { leader.panic = recover() }()
					leader.id, leader.report, leader.err = builder.buildReservedGenerationWithPreparation(
						ctx, req, buildPlan{}, BuildReport{}, time.Now(), id, handle, false, prepare)
				}()
				select {
				case <-entered:
				case <-leaderDone:
					return fmt.Errorf("physical builder exited before source barrier: %+v", leader)
				case <-ctx.Done():
					return fmt.Errorf("physical source barrier: %w", ctx.Err())
				}
				if !builder.Store.PayloadBuildFlightActive(id) {
					return errors.New("source barrier lacks an actual active build flight")
				}
				// Seed after intake begins, avoiding earlier initial eviction;
				// first read is after Retire returns.
				handle.AddBatch([]*graph.Node{{ID: unreadID, Name: "Unread", Kind: graph.KindFunction,
					FilePath: fixture.RepoPrefix + "/retirement-unread.go", RepoPrefix: fixture.RepoPrefix}}, nil)
				for i := range followerDone {
					flight, isLeader, ready, err := builder.Store.JoinPayloadBuildFlight(ctx, id, true)
					if err != nil || flight == nil || isLeader || ready {
						return fmt.Errorf("follower %d admission: leader=%v ready=%v err=%v", i, isLeader, ready, err)
					}
					followers++
					work.Add(1)
					go func(i int) {
						defer work.Done()
						defer close(followerDone[i])
						followerErrors[i] = flight.Wait(ctx)
					}(i)
				}
				return nil
			}
			if builder.Store.PayloadBuildFlightActive(id) {
				t.Fatal("unexpected owner before retirement precheck")
			}
			var setupErr error
			if mode == "healthy" {
				setupErr = startAndPin()
			} else {
				calls := 0
				retireErr := builder.Store.RetirePayloadGeneration(ctx, id, func(generationID int64) bool {
					calls++
					if generationID != id {
						setupErr = fmt.Errorf("callback generation=%d want=%d", generationID, id)
						return true
					}
					if calls == 1 {
						setupErr = startAndPin()
					}
					// Deliberately stale false: the Store's own decisive
					// flight check must protect the owner after fencing.
					return setupErr != nil
				})
				if setupErr == nil && calls == 0 {
					setupErr = fmt.Errorf("legal late-flight barrier not reached: %v", retireErr)
				}
				if setupErr == nil {
					if !errors.Is(retireErr, store_sqlite.ErrPayloadGenerationInUse) {
						t.Errorf("active physical owner did not stop sweep: %v", retireErr)
					}
					row, found, err := builder.Store.Catalog().GetViewGeneration(ctx, id)
					if err != nil || !found || row.State != store_sqlite.ViewGenerationRetiring {
						t.Errorf("fence before release: found=%v row=%+v err=%v", found, row, err)
					}
					if builder.Store.AtGeneration(id).GetNode(unreadID) == nil {
						t.Error("unread payload swept while physical owner remained blocked")
					}
					if !builder.Store.PayloadBuildFlightActive(id) {
						t.Error("retirement released the blocked flight")
					}
				}
			}
			if setupErr != nil {
				t.Fatal(setupErr)
			}
			unblock()
			select {
			case <-leaderDone:
			case <-ctx.Done():
				t.Fatal("leader did not drain after release")
			}
			for i := 0; i < followers; i++ {
				select {
				case <-followerDone[i]:
				case <-ctx.Done():
					t.Fatalf("follower %d did not drain", i)
				}
			}
			if leader.panic != nil {
				t.Errorf("physical pass escaped storage boundary: %T %v", leader.panic, leader.panic)
			}
			if leader.id != id {
				t.Errorf("generation identity=%d want=%d", leader.id, id)
			}
			if mode == "healthy" {
				if leader.err != nil || leader.report.Coalesced || leader.report.NodeCount == 0 {
					t.Errorf("healthy real physical build failed: %+v", leader)
				}
				if handle.GetNode(fixture.RepoPrefix+"/base.go::Committed") == nil {
					t.Error("healthy pass did not persist committed function")
				}
				for i, err := range followerErrors {
					if err != nil {
						t.Errorf("healthy follower %d: %v", i, err)
					}
				}
			} else {
				if !errors.Is(leader.err, store_sqlite.ErrPayloadGenerationSealed) {
					t.Errorf("leader did not return classified sealed refusal: %v", leader.err)
				}
				for i, err := range followerErrors {
					if !errors.Is(err, store_sqlite.ErrPayloadGenerationSealed) {
						t.Errorf("follower %d lost sealed cause: %v", i, err)
					}
				}
				row, found, err := builder.Store.Catalog().GetViewGeneration(ctx, id)
				if err != nil || !found || row.State != store_sqlite.ViewGenerationRetiring {
					t.Errorf("abandonment lost retiring fence: found=%v row=%+v err=%v", found, row, err)
				}
			}
			if preparations.Load() != 1 || opens.Load() == 0 || closes.Load() != 1 || followers != 2 {
				t.Errorf("ownership: prepares=%d opens=%d closes=%d followers=%d", preparations.Load(), opens.Load(), closes.Load(), followers)
			}
			if builder.Store.PayloadBuildFlightActive(id) {
				t.Error("completed leader retained its flight")
			}
			if builder.Store.GetNode(baseID) == nil {
				t.Error("candidate changed generation-zero sentinel")
			}
			if err := builder.Store.RetirePayloadGeneration(ctx, id, builder.Store.PayloadBuildFlightActive); err != nil {
				t.Errorf("post-drain retirement: %v", err)
			}
			if _, found, err := builder.Store.Catalog().GetViewGeneration(ctx, id); err != nil || found {
				t.Errorf("post-drain generation remains: found=%v err=%v", found, err)
			}
			if builder.Store.AtGeneration(id).GetNode(unreadID) != nil || builder.Store.AtGeneration(id).GetNode(fixture.RepoPrefix+"/base.go::Committed") != nil {
				t.Error("post-drain physical payload remains")
			}
			t.Logf("mode=%s generation=%d prepares=%d opens=%d closes=%d followers=%d leader_err=%v", mode, id, preparations.Load(), opens.Load(), closes.Load(), followers, leader.err)
		})
	}
}
