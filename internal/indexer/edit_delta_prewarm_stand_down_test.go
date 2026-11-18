package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// A delta that begins while a pre-warm directory scan is reading stops the
// scan at its next row; the scan reads nothing while the delta runs and is
// read again, whole, once it is done.
func TestPrewarmDirectoryScansStandDownForADelta(t *testing.T) {
	for _, scan := range []string{"directory_index", "directory_index_all"} {
		t.Run(scan, func(t *testing.T) {
			editDeltaPrewarmLowPriorityPause = 0
			t.Cleanup(func() { editDeltaPrewarmLowPriorityPause = 50 * time.Millisecond })
			resetEditDeltaGoInventories()
			resetEditDeltaProvides()
			resetEditDeltaDeps()
			t.Cleanup(resetEditDeltaGoInventories)
			t.Cleanup(resetEditDeltaProvides)
			t.Cleanup(resetEditDeltaDeps)
			f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
			c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
			coordinatorReconcile(t, c)
			view := chainMaterialize(t, f)
			defer view.Close()
			base := commitLayerBase{Reader: view.Reader, stack: []int64{2100001, 2100002}}

			var mu sync.Mutex
			var endEdit func()
			rows, rowsDuringEdit := 0, 0
			editDeltaPrewarmScanRow = func(s string) {
				if s != scan {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				rows++
				if endEdit != nil {
					rowsDuringEdit++
				}
				if rows == 1 {
					// A delta begins as the scan reads its first row.
					endEdit = editDeltaBegin()
				}
			}
			var steps []string
			editDeltaPrewarmStep = func(step string) {
				mu.Lock()
				steps = append(steps, step)
				mu.Unlock()
			}
			done := make(chan struct{})
			editDeltaPrewarmDone = func(string) { close(done) }
			t.Cleanup(func() { editDeltaPrewarmScanRow, editDeltaPrewarmStep, editDeltaPrewarmDone = nil, nil, nil })

			go c.builder.PrewarmEditDeltaStack(context.Background(), openFixed(base), c.repoPrefix, c.workspaceID, c.projectID, []string{"b/b.go"})
			deadline := time.Now().Add(120 * time.Second)
			for {
				mu.Lock()
				began := endEdit != nil
				mu.Unlock()
				if began || time.Now().After(deadline) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond)
			mu.Lock()
			if endEdit == nil {
				mu.Unlock()
				t.Fatalf("the %s scan never read a row", scan)
			}
			during, readBeforeEnd := rowsDuringEdit, rows
			finish := endEdit
			endEdit = nil
			mu.Unlock()
			if during != 0 {
				t.Fatalf("the %s scan read %d rows after it stood down for a delta", scan, during)
			}
			finish()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("the pre-warm never finished after the delta ended")
			}
			mu.Lock()
			defer mu.Unlock()
			if rows <= readBeforeEnd {
				t.Fatalf("the %s scan was not read again after the delta", scan)
			}
			found := false
			for _, s := range steps {
				found = found || s == scan
			}
			if !found {
				t.Fatalf("the pre-warm never completed its %s step: %v", scan, steps)
			}
		})
	}
}

// A likely file's name and referrer warm-up stands down for a delta: while
// one runs, it reads no name.
func TestPrewarmLikelyFileUnitsStandDownForADelta(t *testing.T) {
	resetEditDeltaGoInventories()
	resetEditDeltaProvides()
	resetEditDeltaDeps()
	t.Cleanup(resetEditDeltaGoInventories)
	t.Cleanup(resetEditDeltaProvides)
	t.Cleanup(resetEditDeltaDeps)
	f := newCoordinatorFixtureWithTree(t, sharedRowsTree())
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	coordinatorReconcile(t, c)
	view := chainMaterialize(t, f)
	defer view.Close()
	base := commitLayerBase{Reader: view.Reader, stack: []int64{4100001, 4100002}}
	key, _ := editDeltaBaseCacheKey(base, c.builder.Store)
	cache := editDeltaBaseCache(key)
	dw := graph.NewDeltaWriter(base, nil)
	dw.SetBaseProjectionCache(cache)
	file := c.repoPrefix + "/a/a.go"
	params := newEditDeltaParamIndex(key, nil)

	endEdit := editDeltaBegin()
	warmLikelyNames(dw, c.repoPrefix, file, time.Time{})
	warmLikelyReferrers(dw, c.repoPrefix, file, time.Time{}, params)
	hits, misses := cache.StackNameStats()
	endEdit()
	if hits+misses != 0 {
		t.Fatalf("the likely file's warm-up read %d names while a delta ran", hits+misses)
	}
	warmLikelyNames(dw, c.repoPrefix, file, time.Time{})
	if _, after := cache.StackNameStats(); after == 0 {
		t.Fatal("the likely file's warm-up read no name once the delta was done")
	}
}
