package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"testing"
	"time"
)

// How fast a background chain compaction gives the build lane back.

// compactionPhaseRecord returns the newest background-compaction record of
// the checkout with a key not in seen.
func compactionPhaseRecord(checkoutID string, seen map[string]bool) (PublicationPhaseSnapshot, bool) {
	var newest PublicationPhaseSnapshot
	found := false
	for _, snap := range DefaultPublicationPhases().Snapshot(checkoutID) {
		if snap.Source != PublicationSourceBackgroundCompaction || seen[snap.Key] {
			continue
		}
		newest, found = snap, true
	}
	return newest, found
}

func snapshotHasPhase(snap PublicationPhaseSnapshot, phase PublicationPhase) bool {
	return slices.ContainsFunc(snap.Phases, func(p PublicationPhaseOffset) bool { return p.Phase == phase })
}

// TestDirtyChainCompactionYieldLatencyByStage measures, on the daemon's shape
// (one P, the go/types pass on), how long an interactive build of another
// checkout waits for the lane a compaction holds, by the stage the compaction
// has reached when the build queues.
func TestDirtyChainCompactionYieldLatencyByStage(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f, c, _ := semanticChainFixtureWith(t, semanticBindingTree(), false)
	gate := NewViewBuildGate()
	gate.Open()
	c.gate = gate
	coordinatorReconcile(t, c)
	for i := 0; i < 8; i++ {
		semanticWriteUnit(t, f.worktree, accumulatedDirtyIndependent, i, true)
	}
	coordinatorReconcile(t, c)

	stages := []PublicationPhase{PublicationAdmitted, PublicationPlanned, PublicationExtracted, PublicationSemanticDone, PublicationPayloadFlushed}
	seen := map[string]bool{}
	for _, snap := range DefaultPublicationPhases().Snapshot(f.checkoutID) {
		seen[snap.Key] = true
	}
	type sample struct {
		stage   PublicationPhase
		waited  time.Duration
		yielded bool
	}
	var samples []sample
	probe := 0
	probePath := filepath.Join(f.worktree, filepath.FromSlash(accumulatedDirtyUnitPath(accumulatedDirtyIndependent, 8)))
	for round := 0; round < 3; round++ {
		for _, stage := range stages {
			// Chain edits until the route stands at the soft depth: every
			// edit gives unit 8 a new trailing comment, a one-path delta over
			// a dirty set of nine units.
			var trigger CheckoutCycle
			for edits := 0; !trigger.CompactionScheduled; edits++ {
				if edits > 2*dirtyChainCompactionDepth {
					t.Fatalf("stage %s: %d edits never reached the soft depth: %+v", stage, edits, trigger)
				}
				probe++
				source := semanticBindingUnitSource(accumulatedDirtyIndependent, 8, true, false) + fmt.Sprintf("\n// probe edit %d\n", probe)
				if err := os.WriteFile(probePath, []byte(source), 0o644); err != nil {
					t.Fatal(err)
				}
				trigger = coordinatorReconcile(t, c)
			}
			done := make(chan DirtyChainCompaction, 1)
			go func() { done <- c.compactDirtyChain(context.Background(), trigger) }()
			var record PublicationPhaseSnapshot
			reached, ended := false, false
			for !reached && !ended {
				select {
				case report := <-done:
					// Too short to catch at this polling resolution: counted
					// as missed.
					ended = true
					t.Logf("stage %s: the compaction ended (%s in %v) before the test saw the stage; phases %v",
						stage, report.Outcome, report.Duration, record.Phases)
					continue
				default:
				}
				if snap, ok := compactionPhaseRecord(f.checkoutID, seen); ok {
					record = snap
					reached = snapshotHasPhase(snap, stage)
				}
				if !reached {
					time.Sleep(200 * time.Microsecond)
				}
			}
			if ended {
				for _, snap := range DefaultPublicationPhases().Snapshot(f.checkoutID) {
					seen[snap.Key] = true
				}
				samples = append(samples, sample{stage: stage, waited: -1})
				continue
			}
			queued := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			release, err := gate.Acquire(ctx, ViewBuildInteractive)
			waited := time.Since(queued)
			cancel()
			if err != nil {
				t.Fatalf("stage %s: the interactive build never got the lane: %v", stage, err)
			}
			release()
			report := <-done
			seen[record.Key] = true
			// A later attempt may have opened its own record.
			for _, snap := range DefaultPublicationPhases().Snapshot(f.checkoutID) {
				seen[snap.Key] = true
			}
			samples = append(samples, sample{stage: stage, waited: waited, yielded: slices.Contains(report.YieldedTo, "interactive_build")})
		}
	}
	byStage := map[PublicationPhase][]time.Duration{}
	for _, s := range samples {
		byStage[s.stage] = append(byStage[s.stage], s.waited)
	}
	var lines []string
	for _, stage := range stages {
		waits := byStage[stage]
		sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
		lines = append(lines, fmt.Sprintf("%s: %v (-1ns = stage missed)", stage, waits))
	}
	t.Logf("interactive lane wait behind a yielding compaction, by the stage it had reached (3 rounds, sorted): %v", lines)
}
