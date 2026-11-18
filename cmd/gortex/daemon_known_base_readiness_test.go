package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

type startupBaseJoinFunc func(context.Context) ([]indexer.InitialBasePublication, error)

func (f startupBaseJoinFunc) JoinScheduled(ctx context.Context) ([]indexer.InitialBasePublication, error) {
	return f(ctx)
}

func TestKnownStartupBaseJoinKeepsCoreReadyAndLinkedGateHeld(t *testing.T) {
	controller := &realController{}
	controller.MarkReady(0)
	gate := indexer.NewViewBuildGate()
	entered, release := make(chan struct{}), make(chan struct{})
	joiner := startupBaseJoinFunc(func(ctx context.Context) ([]indexer.InitialBasePublication, error) {
		close(entered)
		select {
		case <-release:
			return []indexer.InitialBasePublication{{RepoPrefix: "known", GenerationID: 7}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	type result struct {
		report knownBasePreparation
		err    error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { report, err := joinKnownStartupBases(ctx, joiner, gate); done <- result{report, err} }()
	<-entered
	require.True(t, controller.IsReady(), "primary core readiness precedes the optional committed-base join")
	require.False(t, controller.IsEnriched())
	require.False(t, gate.IsOpen(), "linked builds must not compete with a held known publication")
	close(release)
	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, 1, got.report.published)
	require.Equal(t, "prepared", got.report.outcome(got.err))
	require.False(t, gate.IsOpen(), "successful join retains the original warmup-tail opening point")
	controller.MarkEnriched(0)
	gate.Open()
	require.True(t, gate.IsOpen())
}

func TestKnownStartupBaseJoinOpensGateOnFailureOrCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  indexer.InitialBasePublication
		err  error
	}{
		{name: "publication failure", out: indexer.InitialBasePublication{Err: errors.New("copy failed")}},
		{name: "canceled attempt", out: indexer.InitialBasePublication{Err: context.Canceled}},
		{name: "unprepared skip", out: indexer.InitialBasePublication{Skipped: "no ready dedicated graph"}},
		{name: "unknown uncommitted reason", out: indexer.InitialBasePublication{Skipped: "owner has no committed tree: unknown"}},
		{name: "missing positive generation", out: indexer.InitialBasePublication{}},
		{name: "shutdown", err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := indexer.NewViewBuildGate()
			report, err := joinKnownStartupBases(t.Context(), startupBaseJoinFunc(func(context.Context) ([]indexer.InitialBasePublication, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return []indexer.InitialBasePublication{tc.out}, nil
			}), gate)
			require.Error(t, err)
			require.Equal(t, "incomplete", report.outcome(err))
			require.Equal(t, tc.err != nil, report.interrupted, "an attempted publication cancellation is not daemon shutdown")
			require.True(t, gate.IsOpen(), "prewarm failure must not permanently block linked recovery")
		})
	}
}

func TestKnownStartupBaseJoinOwnerOnlyDoesNotScheduleAnotherCopy(t *testing.T) {
	calls := 0
	gate := indexer.NewViewBuildGate()
	report, err := joinKnownStartupBases(t.Context(), startupBaseJoinFunc(func(context.Context) ([]indexer.InitialBasePublication, error) {
		calls++
		return []indexer.InitialBasePublication{{RepoPrefix: "owner", Skipped: "no dependent checkout"}}, nil
	}), gate)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Zero(t, report.published)
	require.Equal(t, 1, report.ownerOnly)
	require.Equal(t, "skipped_owner_only", report.outcome(err))
	require.False(t, gate.IsOpen())
}

func TestKnownStartupBaseJoinUncommittedOwnerHasNoPreparedBase(t *testing.T) {
	gate := indexer.NewViewBuildGate()
	report, err := joinKnownStartupBases(t.Context(), startupBaseJoinFunc(func(context.Context) ([]indexer.InitialBasePublication, error) {
		return []indexer.InitialBasePublication{
			{RepoPrefix: "uncommitted", Skipped: "owner has no committed tree"},
			{RepoPrefix: "owner-only", Skipped: "no dependent checkout"},
		}, nil
	}), gate)
	require.NoError(t, err)
	require.Zero(t, report.published, "a normal skip gives no positive-base preparation credit")
	require.Zero(t, report.failed)
	require.Equal(t, 1, report.uncommitted)
	require.Equal(t, 1, report.ownerOnly)
	require.Equal(t, "skipped_uncommitted", report.outcome(err))
	require.False(t, gate.IsOpen(), "the unchanged warmup tail still opens the gate")
}

func TestKnownStartupBaseJoinCancellationReleasesHeldGate(t *testing.T) {
	gate := indexer.NewViewBuildGate()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := joinKnownStartupBases(ctx, startupBaseJoinFunc(func(ctx context.Context) ([]indexer.InitialBasePublication, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}), gate)
		done <- err
	}()
	<-entered
	require.False(t, gate.IsOpen())
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.True(t, gate.IsOpen())
}
