package store_sqlite

import (
	"context"
	"log"
	"time"
)

// Retirement yields to the WAL reclaim.
//
// A retirement sweep commits one bounded chunk after another with no pause
// between them. Each chunk is small, but back to back they leave the WAL
// reclaim no writer-idle moment: its capped backfill runs out of time, it
// defers and backs off, and the log grows (once from 8 KB to 2.6 GB in seven
// minutes of warm-up rebuilds plus retirement). So before each chunk, while
// the log is over the retirement mark, the sweep asks for the reclaim and
// waits — holding nothing — for the log to come back under the mark, up to
// retirementWALWaitMax (retirementWALWaitEditingMax while a checkout is being
// edited). Then it proceeds regardless: retirement must always finish, so
// the wait delays a chunk and never refuses it.

// Vars, not consts, only so the in-package cases can shorten them;
// production never assigns them.
var (
	retirementWALWaitMax  = 30 * time.Second
	retirementWALWaitPoll = 100 * time.Millisecond
	// retirementWALWaitEditingMax: while a checkout is being edited the sweep
	// waits this long for the log to come under its mark, not
	// retirementWALWaitMax: the edits' own writes keep the log over the mark
	// for the whole burst, and every chunk the sweep adds then is log the
	// edits' reads pay for and a reset has to copy. Retirement can wait for a
	// burst; it must still finish, so the wait ends here too.
	retirementWALWaitEditingMax = 10 * time.Minute
)

// retirementWALMark is the log size at which a retirement chunk waits for the
// reclaim: the reclaim threshold (256 MiB by default), or the ceiling if that
// is lower. It was four times the threshold (1 GiB): a startup sweep slice
// wrote 772 MB of log in 15 s just before an edit burst, and the burst's
// folds were then refused for minutes over their mark.
func retirementWALMark(threshold, ceiling int64) int64 {
	if threshold > 0 && ceiling > 0 {
		return min(ceiling, threshold)
	}
	return ceiling
}

// retirementWALEpisode is one sweep's over-ceiling state: an episode starts
// when a chunk finds the log over the ceiling and ends at the first chunk that
// finds it under; its line is written once.
type retirementWALEpisode struct {
	active  bool
	started time.Time
}

// walReclaimBounds is the reclaim's resolved threshold and ceiling (zero
// when the reclaim is off or not running).
func (s *Store) walReclaimBounds() (threshold, ceiling int64) {
	if s.coreless() {
		return 0, 0
	}
	s.walReclaim.mu.Lock()
	defer s.walReclaim.mu.Unlock()
	return s.walReclaim.stats.ThresholdBytes, s.walReclaim.stats.CeilingBytes
}

// awaitWALUnderCeiling runs before a retirement chunk. It returns at once
// when the log is under the ceiling (or no ceiling is known), else nudges the
// reclaim and waits up to retirementWALWaitMax for the log to fall under it.
// It never fails: a timeout or a cancelled context only ends the wait (the
// caller checks ctx itself).
func (s *Store) awaitWALUnderCeiling(ctx context.Context, generationID int64, episode *retirementWALEpisode) {
	threshold, ceiling := s.walReclaimBounds()
	ceiling = retirementWALMark(threshold, ceiling)
	if ceiling <= 0 || s.dbPath == "" {
		return
	}
	size := s.retirementLogBytes()
	if size < ceiling {
		if episode.active {
			log.Printf("store_sqlite: retirement resumed generation=%d wal_bytes=%d ceiling=%d episode=%s",
				generationID, size, ceiling, time.Since(episode.started).Round(time.Millisecond))
			*episode = retirementWALEpisode{}
		}
		return
	}
	if !episode.active {
		*episode = retirementWALEpisode{active: true, started: time.Now()}
		log.Printf("store_sqlite: retirement waiting for the wal reclaim generation=%d wal_bytes=%d ceiling=%d max_wait=%s",
			generationID, size, ceiling, retirementWALWaitMax)
	}
	s.walReclaim.cycle.retirementWaits.Add(1)
	s.RequestWALReclaim()
	waitMax := retirementWALWaitMax
	if s.buildLaneBusy() || s.walCopy.editing(time.Now()) {
		waitMax = retirementWALWaitEditingMax
	}
	deadline := time.NewTimer(waitMax)
	defer deadline.Stop()
	poll := time.NewTicker(retirementWALWaitPoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			s.walReclaim.cycle.retirementTimeouts.Add(1)
			return
		case <-poll.C:
			if s.retirementLogBytes() < ceiling {
				return
			}
			s.RequestWALReclaim()
		}
	}
}

// retirementLogBytes is the log as the reclaim measures it: its frames since
// the last reset (a log reset in place keeps its file's size until it is
// shrunk, which the file's size would read as over the mark).
func (s *Store) retirementLogBytes() int64 {
	mark := s.WALWriteMark()
	if !mark.Valid {
		return walFileSize(s.dbPath + "-wal")
	}
	return int64(mark.MxFrame) * (int64(mark.PageSize) + walFrameHeaderBytes)
}

// Retirement yields the write gate to the edit path.
//
// An edit's mutation announces itself (AnnounceWrite) before it waits on
// anything and holds the announcement until it has committed or failed; its
// writes (the receipt's route-withdrawal row, the delta's payload write, the
// publish) are separate transactions. A retirement sweep commits a chunk,
// takes the gate again for the next one, and lands between those
// transactions, so each of them waited for a whole chunk (one measured wait
// 154 ms). At its next chunk boundary the sweep now waits while a mutation is
// announced or a writer is parked on the gate, up to retirementEditYieldMax
// per chunk, then proceeds regardless — retirement must always finish.

// Vars, not consts, only so the in-package cases can change them.
var (
	retirementEditYieldMax  = 5 * time.Second
	retirementEditYieldPoll = time.Millisecond
)

// yieldToEditWriters runs before a retirement chunk, holding nothing.
func (s *Store) yieldToEditWriters(ctx context.Context) {
	if s.coreless() || !s.writeWanted() {
		return
	}
	s.walReclaim.cycle.retirementEditYields.Add(1)
	deadline := time.Now().Add(retirementEditYieldMax)
	for s.writeWanted() && ctx.Err() == nil {
		if !time.Now().Before(deadline) {
			s.walReclaim.cycle.retirementEditYieldTimeouts.Add(1)
			return
		}
		time.Sleep(retirementEditYieldPoll)
	}
}
