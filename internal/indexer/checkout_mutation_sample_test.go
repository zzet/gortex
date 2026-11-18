package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/gitstate"
)

type admissionSampleControl struct {
	t     *testing.T
	calls int
	fresh func(context.Context, time.Time, int) (gitstate.DirtySnapshot, error)
}

func (s *admissionSampleControl) Sample(ctx context.Context) (gitstate.DirtySnapshot, error) {
	s.calls++
	return s.fresh(ctx, time.Time{}, s.calls)
}
func (s *admissionSampleControl) SampleSince(ctx context.Context, since time.Time) (gitstate.DirtySnapshot, error) {
	s.calls++
	return s.fresh(ctx, since, s.calls)
}
func admissionTornSample() error {
	return fmt.Errorf("checked sample: %w: %w", gitstate.ErrDirtyUnavailable, gitstate.ErrDirtyMoved)
}

func TestMutationAdmissionResampleStartsAfterFailedCut(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	for _, initialSince := range []time.Time{{}, time.Now()} {
		var failedAt time.Time
		s := &admissionSampleControl{t: t, fresh: func(got context.Context, bound time.Time, n int) (gitstate.DirtySnapshot, error) {
			require.Same(t, ctx, got)
			d, _ := got.Deadline()
			require.Equal(t, deadline, d)
			if n == 1 {
				require.Equal(t, initialSince, bound)
				failedAt = time.Now()
				return gitstate.DirtySnapshot{}, admissionTornSample()
			}
			require.Equal(t, 2, n)
			require.False(t, bound.IsZero())
			require.False(t, bound.Before(failedAt), "never reuse pre-failure cached success")
			return gitstate.DirtySnapshot{Fingerprint: "stable-new-cut"}, nil
		}}
		sample, err := sampleMutationAdmission(ctx, s, initialSince)
		require.NoError(t, err)
		require.Equal(t, "stable-new-cut", sample.Fingerprint)
		require.Equal(t, 2, s.calls)
	}
}

func TestMutationAdmissionResampleIsExactlyOneAndKeepsTypedRefusal(t *testing.T) {
	s := &admissionSampleControl{t: t, fresh: func(context.Context, time.Time, int) (gitstate.DirtySnapshot, error) {
		return gitstate.DirtySnapshot{}, admissionTornSample()
	}}
	sample, err := sampleMutationAdmission(t.Context(), s, time.Time{})
	require.ErrorIs(t, err, gitstate.ErrDirtyUnavailable)
	require.ErrorIs(t, err, gitstate.ErrDirtyMoved)
	require.Empty(t, sample.Fingerprint)
	require.Equal(t, 2, s.calls)
}

func TestMutationAdmissionResampleDoesNotRetryOtherFailuresOrDeadCaller(t *testing.T) {
	permanent := errors.New("permanent sampling failure")
	for _, kind := range []string{"unavailable_only", "moved_only", "permanent", "canceled", "deadline", "retry_canceled"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			expected := permanent
			s := &admissionSampleControl{t: t, fresh: func(got context.Context, _ time.Time, n int) (gitstate.DirtySnapshot, error) {
				switch kind {
				case "unavailable_only":
					expected = gitstate.ErrDirtyUnavailable
				case "moved_only":
					expected = gitstate.ErrDirtyMoved
				case "permanent":
					expected = permanent
				case "canceled":
					cancel()
					expected = admissionTornSample()
				case "deadline":
					expired, stop := context.WithDeadline(got, time.Now().Add(-time.Second))
					defer stop()
					expected = expired.Err()
				case "retry_canceled":
					if n == 1 {
						return gitstate.DirtySnapshot{}, admissionTornSample()
					}
					cancel()
					expected = got.Err()
				}
				return gitstate.DirtySnapshot{}, expected
			}}
			_, err := sampleMutationAdmission(ctx, s, time.Time{})
			if kind == "canceled" {
				require.ErrorIs(t, err, gitstate.ErrDirtyMoved)
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			} else {
				require.ErrorIs(t, err, expected)
			}
			want := 1
			if kind == "retry_canceled" {
				want = 2
			}
			require.Equal(t, want, s.calls)
		})
	}
}

func TestMutationAdmissionStableResampleStillRefusesMovedRoutedSource(t *testing.T) {
	f, c, _ := newCheckoutMutationFixture(t)
	route := f.route()
	target := filepath.Join(f.worktree, "helper.go")
	before, err := os.ReadFile(target)
	require.NoError(t, err)
	builderWriteFile(t, f.worktree, "unrelated.go", "package fixture\nfunc DifferentWholeCheckout(){}\n")
	s := &admissionSampleControl{t: t, fresh: func(ctx context.Context, since time.Time, n int) (gitstate.DirtySnapshot, error) {
		if n == 1 {
			return gitstate.DirtySnapshot{}, admissionTornSample()
		}
		return c.sampler.SampleSince(ctx, since)
	}}
	sample, err := sampleMutationAdmission(t.Context(), s, time.Time{})
	require.NoError(t, err)
	require.ErrorIs(t, c.checkRoutedSnapshot(t.Context(), route, sample), ErrCheckoutMutationDiskMoved, "stable newer bytes still must match the exact routed whole checkout")
	after, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, route, f.route(), "resampling alone admits no write or route mutation")
}
