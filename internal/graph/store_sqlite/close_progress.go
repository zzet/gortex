package store_sqlite

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Close progress and the learned close rate.
//
// Close's final TRUNCATE copies the whole backlog in one pass and publishes
// nothing until the pass ends (nBackfill moves only when a pass completes). A
// stop path that kills the process on a fixed deadline therefore throws the
// whole copy away — restart #8 on 2026-09-25 stopped after 211 s with a
// 22.4 GB WAL untouched, and the next open had to start the copy from zero.
//
// So while the close runs, the store writes a small progress file next to
// the database (CloseProgressPath), once a second: the backlog, the per-frame
// cost it is budgeted at, the elapsed time, an estimate of the frames copied
// so far, and — the signal the stop path acts on — when the database file
// last changed. The checkpoint writes the copied pages into the database
// file, so its modification time (and size, when the file grows) advances for
// as long as the copy advances; a close that is wedged leaves it still. The
// stop path (cmd/gortex) waits while that signal is fresh and gives up only
// after a bounded interval without it.
//
// A completed close also records its measured per-frame cost
// (closeRatePath); the next estimate uses it instead of the default, so the
// estimate corrects itself to the store and the disk it runs on.

// CloseProgress is the close's progress as the stop path reads it.
type CloseProgress struct {
	PID                  int     `json:"pid"`
	Phase                string  `json:"phase"` // draining | done | failed
	StartedUnixNano      int64   `json:"started_unix_nano"`
	UpdatedUnixNano      int64   `json:"updated_unix_nano"`
	LastProgressUnixNano int64   `json:"last_progress_unix_nano"`
	PendingFrames        int64   `json:"pending_frames"`
	PerFrameNanos        int64   `json:"per_frame_nanos"`
	EstimateMillis       int64   `json:"estimate_millis"`
	ElapsedMillis        int64   `json:"elapsed_millis"`
	FramesCopiedEstimate int64   `json:"frames_copied_estimate"`
	RateFramesPerSec     float64 `json:"rate_frames_per_sec"`
	RemainingMillis      int64   `json:"remaining_millis"`
}

// Since reports how long ago the close last showed progress.
func (p CloseProgress) Since(now time.Time) time.Duration {
	if p.LastProgressUnixNano == 0 {
		return now.Sub(time.Unix(0, p.StartedUnixNano))
	}
	return now.Sub(time.Unix(0, p.LastProgressUnixNano))
}

// CloseProgressPath is where a store at dbPath reports its close.
func CloseProgressPath(dbPath string) string { return dbPath + ".close-progress" }

func closeRatePath(dbPath string) string { return dbPath + ".close-rate" }

// ReadCloseProgress reads the close progress of the store at dbPath.
func ReadCloseProgress(dbPath string) (CloseProgress, bool) {
	var p CloseProgress
	blob, err := os.ReadFile(CloseProgressPath(dbPath))
	if err != nil || json.Unmarshal(blob, &p) != nil {
		return CloseProgress{}, false
	}
	return p, true
}

func writeJSONAtomic(path string, v any) {
	blob, err := json.Marshal(v)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, blob, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// Vars, not consts, only so the in-package cases can shorten them.
var (
	closeProgressInterval = time.Second
	// closeRateMinFrames: a close that copied fewer frames than this is
	// dominated by its fixed costs and teaches nothing about the rate.
	closeRateMinFrames int64 = 20_000
)

const (
	closeRateFloor   = 20 * time.Microsecond
	closeRateCeiling = 5 * time.Millisecond
)

type closeRate struct {
	PerFrameNanos int64 `json:"per_frame_nanos"`
	Frames        int64 `json:"frames"`
	DurationNanos int64 `json:"duration_nanos"`
	AtUnix        int64 `json:"at_unix"`
}

// closePerFrame is the per-frame cost the next close of dbPath is estimated
// at: the last measured close when one was recorded, else the default.
func closePerFrame(dbPath string) time.Duration {
	if dbPath == "" || isMemoryPath(dbPath) {
		return closeCheckpointPerFrame
	}
	blob, err := os.ReadFile(closeRatePath(dbPath))
	if err != nil {
		return closeCheckpointPerFrame
	}
	var r closeRate
	if json.Unmarshal(blob, &r) != nil || r.PerFrameNanos <= 0 {
		return closeCheckpointPerFrame
	}
	return min(max(time.Duration(r.PerFrameNanos), closeRateFloor), closeRateCeiling)
}

// recordCloseRate stores a completed close's per-frame cost when the close
// copied enough frames to measure it.
func recordCloseRate(dbPath string, frames int64, took time.Duration) (time.Duration, bool) {
	if dbPath == "" || isMemoryPath(dbPath) || frames < closeRateMinFrames || took <= 0 {
		return 0, false
	}
	per := min(max(took/time.Duration(frames), closeRateFloor), closeRateCeiling)
	writeJSONAtomic(closeRatePath(dbPath), closeRate{PerFrameNanos: int64(per), Frames: frames, DurationNanos: int64(took), AtUnix: time.Now().Unix()})
	return per, true
}

// fileMark is what the progress signal compares: a change in either means
// the checkpoint wrote to the file.
type fileMark struct {
	mod  int64
	size int64
}

func markOf(path string) fileMark {
	info, err := os.Stat(path)
	if err != nil {
		return fileMark{}
	}
	return fileMark{mod: info.ModTime().UnixNano(), size: info.Size()}
}

// startCloseProgress starts the progress reporter for a close of pending
// frames and returns its stop, which writes the final phase.
func startCloseProgress(dbPath string, pending int64, perFrame, estimate time.Duration, started time.Time) func(ok bool) {
	if dbPath == "" || isMemoryPath(dbPath) {
		return func(bool) {}
	}
	path := CloseProgressPath(dbPath)
	p := CloseProgress{
		PID:             os.Getpid(),
		Phase:           "draining",
		StartedUnixNano: started.UnixNano(),
		PendingFrames:   pending,
		PerFrameNanos:   int64(perFrame),
		EstimateMillis:  estimate.Milliseconds(),
	}
	last := markOf(dbPath)
	walLast := markOf(dbPath + "-wal")
	update := func(now time.Time) {
		if m := markOf(dbPath); m != last {
			last = m
			p.LastProgressUnixNano = now.UnixNano()
		}
		if m := markOf(dbPath + "-wal"); m != walLast {
			walLast = m
			p.LastProgressUnixNano = now.UnixNano()
		}
		elapsed := now.Sub(started)
		p.UpdatedUnixNano = now.UnixNano()
		p.ElapsedMillis = elapsed.Milliseconds()
		if perFrame > 0 && pending > 0 {
			p.FramesCopiedEstimate = min(pending, int64(elapsed/perFrame))
			p.RateFramesPerSec = float64(time.Second) / float64(perFrame)
			p.RemainingMillis = max(0, (time.Duration(pending-p.FramesCopiedEstimate) * perFrame).Milliseconds())
		}
		writeJSONAtomic(path, p)
	}
	update(started)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(closeProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				update(now)
			}
		}
	}()
	var once sync.Once
	return func(ok bool) {
		once.Do(func() {
			close(done)
			wg.Wait()
			now := time.Now()
			update(now)
			p.Phase = "done"
			if !ok {
				p.Phase = "failed"
			}
			p.LastProgressUnixNano = now.UnixNano()
			if pending > 0 && ok {
				p.FramesCopiedEstimate = pending
				p.RemainingMillis = 0
				if took := now.Sub(started); took > 0 {
					p.RateFramesPerSec = float64(pending) / took.Seconds()
				}
			}
			writeJSONAtomic(path, p)
		})
	}
}
