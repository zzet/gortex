package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"log"
)

// passiveCheckpointer owns the background loop's lazily opened private pool.
// Its SQL/admission policy is shared with the bounded transition tests.
type passiveCheckpointer struct {
	store *Store
	db    *sql.DB
}

func (p *passiveCheckpointer) run(forced bool) (complete, retry bool) {
	policy := checkpointYieldsToCycle
	if forced {
		policy = checkpointIgnoresCycle
	}
	attempt, err := p.store.beginBackgroundCheckpointAttempt(policy)
	if err != nil {
		p.store.walPassive.refused(err)
		return false, true
	}
	defer p.store.finishBackgroundCheckpointAttempt(attempt)
	p.store.walPassive.update(func(st *WALPassiveStats) { st.Attempts++ })
	ctx := attempt.ctx
	var out passiveCheckpointResult
	if ctx.Err() != nil {
		out.stopped, out.retry = true, true
	} else {
		if p.db == nil {
			db, openErr := openSQLiteStoreDB(sqliteCheckpointDSN(p.store.dbPath))
			if openErr != nil {
				if ctx.Err() == nil {
					log.Printf("store_sqlite: wal checkpoint deferred mode=PASSIVE reason=open error=%q", openErr)
				}
				out.err, out.retry = openErr, ctx.Err() != nil
			} else {
				configureWriterPool(db)
				p.db = db
			}
		}
		if p.db != nil {
			out = p.store.passiveCheckpointResultContext(ctx, p.db)
		}
	}
	// Settle exactly once after the same final coordination-cause check as
	// runBackgroundCheckpointAttemptWith; context cleanup is not a yield.
	if cause := context.Cause(ctx); errors.Is(cause, errWALCheckpointDeferredBulk) || errors.Is(cause, errWALCheckpointYieldedToCycle) {
		p.store.walPassive.yielded()
		return false, true
	}
	p.store.walPassive.observed(out)
	return out.complete, out.retry
}

type passiveCheckpointResult struct {
	complete, retry, stopped bool
	result                   walCheckpointResult
	err                      error
	frames                   int64
	framesKnown              bool
}

func (s *Store) passiveCheckpointResultContext(ctx context.Context, db *sql.DB) passiveCheckpointResult {
	if ctx == nil {
		ctx = context.Background()
	}
	before, mark := passiveBackfillMark(s.dbPath)
	result, err := checkpointWALOnceOn(ctx, db, "PASSIVE")
	after := readWALWriteMark(s.dbPath)
	out := passiveCheckpointResult{result: result, err: err}
	// SQLite returns a cumulative backfilled prefix, not this call's work.
	// A reset/unreadable witness stays explicitly unobserved, never zero.
	if mark.Valid && after.Valid && mark.Salt == after.Salt && after.MxFrame >= mark.MxFrame && result.CheckpointedFrames >= int(before) {
		out.frames = int64(result.CheckpointedFrames) - int64(before)
		out.framesKnown = true
	}
	ctxErr := ctx.Err()
	if cause := context.Cause(ctx); errors.Is(cause, errWALCheckpointDeferredBulk) || errors.Is(cause, errWALCheckpointYieldedToCycle) {
		out.retry = true
		return out
	}
	if err == nil {
		out.complete = true
		return out
	}
	select {
	case <-s.stopCheckpoint:
		out.stopped = true
		return out
	default:
	}
	log.Print(passiveCheckpointReport(result, err, ctxErr))
	out.retry = shouldRetryPassiveCheckpoint(err, ctxErr)
	return out
}

// Read the cumulative prefix and reset identity from one header observation;
// separate reads could combine an old prefix with a new WAL generation.
func passiveBackfillMark(path string) (uint32, WALWriteMark) {
	var header [100]byte
	if err := readWALIndexHeader(path, header[:]); err != nil {
		return 0, WALWriteMark{}
	}
	mark := WALWriteMark{Valid: true, MxFrame: binary.NativeEndian.Uint32(header[16:20])}
	copy(mark.Salt[:], header[32:40])
	return binary.NativeEndian.Uint32(header[96:100]), mark
}

func (p *passiveCheckpointer) close() {
	if p.db == nil {
		return
	}
	if err := p.db.Close(); err != nil {
		log.Printf("store_sqlite: close background WAL checkpoint pool: %v", err)
	}
}
