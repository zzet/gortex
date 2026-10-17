package indexer

import (
	"fmt"
	"testing"
)

// TestEditDeltaLargeChangeSetIsOneDeltaMatchingACleanIndex builds a
// working-tree change far past the coordinator's import threshold as ONE
// delta, direct over the committed state — the shape a caller that does not
// import file by file hands the builder — and requires it to be built by the
// per-file delta path and to serve exactly a clean index, in both layouts.
func TestEditDeltaLargeChangeSetIsOneDeltaMatchingACleanIndex(t *testing.T) {
	const changed = 3 * editDeltaLargeChangeUnit
	for _, layout := range []accumulatedDirtyLayout{accumulatedDirtyIndependent, accumulatedDirtySamePackage} {
		t.Run(string(layout), func(t *testing.T) {
			store := builderOpenStore(t, fmt.Sprintf("large-%s", layout))
			repoDir := accumulatedDirtyRepo(t, layout, store)
			chains := newDirtyChainBuilder(t, builderNewBuilder(store), store, repoDir, false)
			for i := 0; i < changed; i++ {
				accumulatedDirtyWriteUnit(t, repoDir, layout, i, true, i%3 == 0)
			}
			recordLastEditDelta(nil)
			_, _, chain := chains.build()
			delta := LastEditDeltaReport()
			if delta == nil {
				t.Fatalf("%s: the %d-path change set was not built by the delta path", layout, changed)
			}
			if len(delta.Paths) != changed {
				t.Fatalf("%s: the delta covers %d paths, want %d", layout, len(delta.Paths), changed)
			}
			result := assertCleanIndexParityChain(t, store, chain, repoDir, fmt.Sprintf("large-%s", layout), true)
			if !result.ok() {
				t.Errorf("%s: the %d-path delta does not serve a clean index: %v", layout, changed, result.Diffs)
			}
		})
	}
}

// editDeltaLargeChangeUnit is the coordinator's interactive bound: a larger
// working-tree change is imported file by file there.
const editDeltaLargeChangeUnit = 32
