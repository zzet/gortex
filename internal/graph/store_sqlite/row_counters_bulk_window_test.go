package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// returnsWithin runs fn and fails when it does not return within d: the
// maintenance writes this guards once waited forever for the writer pool's
// only connection while a generation bulk window pinned it, holding writeMu —
// which the window's end needs.
func returnsWithin(t *testing.T, d time.Duration, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("%s did not return within %s (writer pool pinned by a bulk window)", what, d)
		return nil
	}
}

// A generation bulk window opened while the counters' seed is counted (the
// window the lazy loop leaves between pinning its snapshot and adding the
// seed): the install must not wait on the pinned writer connection. It gives
// up while the window is open and installs exactly once the window ends.
func TestRowCountersSeedDoesNotDeadlockWithABulkWindow(t *testing.T) {
	ctx := context.Background()
	s, err := openPristine(t, filepath.Join(t.TempDir(), "window.sqlite"))
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	nodes, edges := rowCounterFixture("repo/a.go", 40)
	s.AddBatch(nodes, edges)
	const generation = int64(21)
	rowCountersAfterPinHook = func() {
		engaged, err := s.BeginGenerationBulkLoad(generation)
		require.NoError(t, err)
		require.True(t, engaged)
		wn, we := rowCounterFixture("repo/w.go", 9)
		require.NoError(t, s.AtGeneration(generation).AddBatchChecked(wn, we))
	}
	t.Cleanup(func() { rowCountersAfterPinHook = nil })
	err = returnsWithin(t, 3*time.Second, "EnsureRowCounters", func() error { return s.EnsureRowCounters(ctx) })
	rowCountersAfterPinHook = nil
	if err != nil {
		require.True(t, errors.Is(err, errRowCountersBulkWindow), "unexpected error %v", err)
		require.False(t, s.RowCountersReady())
	}
	require.NoError(t, returnsWithin(t, 3*time.Second, "EndGenerationBulkLoadFor", func() error {
		return s.EndGenerationBulkLoadFor(generation)
	}))
	require.NoError(t, s.EnsureRowCounters(ctx))
	requireCountersExact(t, s, 0, generation)
}

// The counters' repair and a derived correction's chunk and Finish, called
// while a bulk window is open, write through the window's connection instead
// of waiting for the pinned one.
func TestMaintenanceWritesUseTheBulkWindowConnection(t *testing.T) {
	ctx := context.Background()
	store, generationID := publishedStampGeneration(t)
	require.NoError(t, store.EnsureRowCounters(ctx))
	store.writeMu.Lock()
	_, err := store.writerDB.Exec(`UPDATE generation_row_counts SET nodes = nodes + 3 WHERE view_gen = 0`)
	store.writeMu.Unlock()
	require.NoError(t, err)
	const window = int64(99)
	engaged, err := store.BeginGenerationBulkLoad(window)
	require.NoError(t, err)
	require.True(t, engaged)
	check, err := func() (RowCounterCheck, error) {
		var out RowCounterCheck
		err := returnsWithin(t, 3*time.Second, "CheckRowCounters(repair)", func() error {
			var e error
			out, e = store.CheckRowCounters(ctx, true)
			return e
		})
		return out, err
	}()
	require.NoError(t, err)
	require.True(t, check.Repaired)
	c, err := store.BeginDerivedCorrection(ctx, DerivedCorrectionRequest{GenerationID: generationID, Pass: "capability", FromVersion: 1, ToVersion: 2,
		EdgeKinds: []graph.EdgeKind{graph.EdgeAccessesField}})
	require.NoError(t, err)
	require.NoError(t, returnsWithin(t, 3*time.Second, "DerivedCorrection.Finish", func() error {
		_, e := c.Finish(ctx)
		return e
	}))
	require.NoError(t, store.EndGenerationBulkLoadFor(window))
	requireCountersExact(t, store, 0)
}
