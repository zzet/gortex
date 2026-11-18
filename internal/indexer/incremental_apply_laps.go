package indexer

import (
	"time"

	"go.uber.org/zap"
)

// applyLaps times the steps of one structural graph apply
// (commitIncrementalStages), with the major page faults of each, and logs
// them once per apply. The reconcile phase log times the apply as a whole;
// these say where inside it the time goes.
type applyLaps struct {
	last   time.Time
	faults int64
	split  applyLapSplit
	fields []zap.Field
}

func (idx *Indexer) startApplyLaps() {
	idx.applyLaps = &applyLaps{last: time.Now(), faults: editDeltaProcessIO().majorFaults, split: idx.applyLapSplitMark()}
}

// applyLap closes the running step under name. A no-op outside an apply.
func (idx *Indexer) applyLap(name string) {
	laps := idx.applyLaps
	if laps == nil {
		return
	}
	now, faults, split := time.Now(), editDeltaProcessIO().majorFaults, idx.applyLapSplitMark()
	laps.fields = append(laps.fields,
		zap.Float64(name+"_ms", float64(now.Sub(laps.last).Microseconds())/1000),
		zap.Int64(name+"_faults", faults-laps.faults))
	laps.fields = append(laps.fields, applyLapSplitFields(name, laps.split, split)...)
	laps.last, laps.faults, laps.split = now, faults, split
}

func (idx *Indexer) finishApplyLaps(staged int, inEdges, outEdges int) {
	laps := idx.applyLaps
	idx.applyLaps = nil
	if laps == nil || idx.logger == nil {
		return
	}
	fields := append([]zap.Field{zap.String("repo", idx.repoPrefix), zap.Int("staged_files", staged),
		zap.Int("prior_in_edges", inEdges), zap.Int("prior_out_edges", outEdges)}, laps.fields...)
	idx.logger.Info("indexer: graph apply steps", fields...)
}
