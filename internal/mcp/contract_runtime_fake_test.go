package mcp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/indexer"
)

// contractRuntimeFake is the strict consumer's scheduler in tests. A nil
// schedule or waitActor callback fails the test when called, so a test names
// exactly the calls it expects. InputEpoch and CapacityEpoch read the
// counters; WaitInputs and WaitCapacity wait for them to pass since.
// captures counts InputEpoch samples: the consumer takes one before each
// capture of its inputs.
type contractRuntimeFake struct {
	t         *testing.T
	acquire   func(context.Context, indexer.ContractActor) (*indexer.ContractPublishedCohort, func(), error)
	schedule  func(context.Context, indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error)
	waitActor func(context.Context, indexer.ContractActor, uint64) (indexer.ContractActorStatus, error)
	epoch     atomic.Uint64
	capacity  atomic.Uint64
	schedules atomic.Int64
	waits     atomic.Int64
	captures  atomic.Int64
}

func (f *contractRuntimeFake) runtime() *ContractAnalysisRuntime {
	waitFor := func(ctx context.Context, counter *atomic.Uint64, since uint64) error {
		for counter.Load() <= since {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
		return nil
	}
	runtime := &ContractAnalysisRuntime{
		Schedule: func(ctx context.Context, req indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
			f.schedules.Add(1)
			if f.schedule == nil {
				req.Source.Release()
				f.t.Error("unexpected contract analysis schedule")
				return indexer.ContractScheduleResult{}, errors.New("unexpected schedule")
			}
			return f.schedule(ctx, req)
		},
		WaitActor: func(ctx context.Context, actor indexer.ContractActor, seq uint64) (indexer.ContractActorStatus, error) {
			f.waits.Add(1)
			if f.waitActor == nil {
				f.t.Error("unexpected wait on contract analysis")
				return indexer.ContractActorStatus{}, errors.New("unexpected wait")
			}
			return f.waitActor(ctx, actor, seq)
		},
		WaitCapacity:  func(ctx context.Context, since uint64) error { return waitFor(ctx, &f.capacity, since) },
		CapacityEpoch: f.capacity.Load,
		InputEpoch: func() uint64 {
			f.captures.Add(1)
			return f.epoch.Load()
		},
		WaitInputs: func(ctx context.Context, since uint64) error { return waitFor(ctx, &f.epoch, since) },
	}
	runtime.AcquireLastPublishedCohort = f.acquire
	return runtime
}

// installForbiddenContractRuntime installs a runtime whose every schedule or
// wait fails the test: the requests it guards answer from attachments as
// they stand, or from the core alone.
func installForbiddenContractRuntime(t *testing.T, srv *Server) *contractRuntimeFake {
	t.Helper()
	fake := &contractRuntimeFake{t: t}
	srv.SetContractAnalysisRuntime(fake.runtime())
	return fake
}

// waitUntilClosed answers WaitActor published once release closes, or with
// the context's error.
func waitUntilClosed(release <-chan struct{}) func(context.Context, indexer.ContractActor, uint64) (indexer.ContractActorStatus, error) {
	return func(ctx context.Context, _ indexer.ContractActor, seq uint64) (indexer.ContractActorStatus, error) {
		select {
		case <-release:
			return indexer.ContractActorStatus{Result: indexer.ContractActorPublished, Seq: seq}, nil
		case <-ctx.Done():
			return indexer.ContractActorStatus{}, ctx.Err()
		}
	}
}

// scheduledAt answers Schedule scheduled at seq, releasing the source.
func scheduledAt(seq uint64) func(context.Context, indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
	return func(_ context.Context, req indexer.ContractScheduleRequest) (indexer.ContractScheduleResult, error) {
		req.Source.Release()
		return indexer.ContractScheduleResult{Result: indexer.ContractScheduled, Actor: req.Actor, ArrivalSeq: seq}, nil
	}
}
