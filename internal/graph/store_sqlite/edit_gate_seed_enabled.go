//go:build gortex_edit_gate_seed

package store_sqlite

import (
	"context"
	"os"
	"time"
)

func editGateClosedReclaimSeed(s *Store) {
	if os.Getenv("GORTEX_EDIT_GATE_SEED") != "closed-read-gate" || s.coreless() || s.readGate == nil {
		return
	}
	db, err := openWALReclaimCheckpointDB(s.dbPath)
	if err != nil {
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg := resolveWALReclaimConfig()
	// Force the existing closed-admission path even with a small/empty log;
	// no WAL size or counter sample is fabricated by this selection fault.
	cfg.readerWait, cfg.thresholdBytes = 0, -1
	res := walReclaimResult{urgent: true, bytesBefore: s.observedWALFileSize(s.dbPath + "-wal")}
	_ = s.reclaimWALInLane(ctx, cfg, db, &res)
	if res.pauseClosed {
		s.walReclaim.update(func(stats *WALReclaimStats) { stats.notePause(res.pause) })
		s.walReclaim.pauseDrain.observe(res.pauseDrain)
		s.walReclaim.pauseTruncate.observe(res.pauseTruncate)
	}
}
