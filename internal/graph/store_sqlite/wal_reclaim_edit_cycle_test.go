package store_sqlite

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// A mutation that never takes the build lane (an undo flips the route without
// a build) still stands background work down from its announcement to its
// release; a leaked announcement stops doing so after the bound.
func TestAnnouncedMutationStandsBackgroundWorkDown(t *testing.T) {
	for _, checkout := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkout=%t", checkout), func(t *testing.T) {
			t.Setenv("GORTEX_SQLITE_WAL_RECLAIM_MB", "0")
			s, path := openWALReclaimStore(t)
			defer func() { _ = s.Close() }()
			seedWALChurnTable(t, s)
			growWAL(t, s, 8)
			lane := &fakeBuildLane{} // installed, never held
			lane.install(s)
			ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer ckpt.Close()
			cfg := walReclaimConfig{thresholdBytes: 1 << 20, drainDeadline: defaultWALReclaimDrainDeadline, truncateBudget: walReclaimTruncateBudget, readerWait: defaultWALReclaimReaderWait}

			announce := s.AnnounceWrite
			if checkout {
				announce = s.AnnounceCheckoutRefresh
			}
			release := announce()
			defer release()
			res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
			if res.outcome != walReclaimSkipped || res.reason != "build_lane_busy" {
				t.Fatalf("reclaim during an announced undo: outcome=%s reason=%q, want skipped build_lane_busy", res.outcome, res.reason)
			}
			release()
			if res := s.reclaimWALOnce(cfg, ckpt, path+"-wal"); res.outcome != walReclaimReset {
				t.Fatalf("reclaim after the release: outcome=%s reason=%q, want reset", res.outcome, res.reason)
			}

		})
	}
}

// The leak guard: an announcement that is never released stops standing
// background work down after editIntentStandDownMax (2 min in production), and
// the WARN is logged once for the episode.
func TestLeakedAnnouncementStopsStandingDownAndWarnsOnce(t *testing.T) {
	if editIntentStandDownMax != 2*time.Minute {
		t.Fatalf("production bound = %s, want 2m", editIntentStandDownMax)
	}
	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	lane := &fakeBuildLane{}
	lane.install(s)
	logs := captureReclaimLog(t)
	prev := editIntentStandDownMax
	editIntentStandDownMax = 50 * time.Millisecond
	t.Cleanup(func() { editIntentStandDownMax = prev })
	leaked := s.AnnounceWrite() // never released within the bound
	defer leaked()
	if !s.buildLaneBusy() {
		t.Fatal("a fresh announcement must stand background work down")
	}
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if s.buildLaneBusy() {
			t.Fatal("a leaked announcement stood background work down past its bound")
		}
	}
	if n := strings.Count(logs.String(), "write announcements held for"); n != 1 {
		t.Fatalf("leak WARN logged %d times, want once per episode:\n%s", n, logs.String())
	}
}
