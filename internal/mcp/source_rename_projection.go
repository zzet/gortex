package mcp

import (
	"context"
	"fmt"
	"sync"
)

// Parse at most two candidates concurrently, then authenticate metadata after
// their single wall-time lap. The call stack owns preparations only until the
// physical frontier; graphs never enter the mutation ledger or journal state.
func (s *Server) finishRenamePlans(ctx context.Context, writes []*renameFileWrite, allowParseErrors bool, priorError error) ([]*renameFileWrite, error) {
	if len(writes) == 0 {
		return writes, priorError
	}
	gated := parseGateEnabled()
	var lap editAcknowledgementLap
	if gated || s.indexer != nil || s.multiIndexer != nil {
		scope := "source_projection"
		if gated {
			scope = "source_projection_parse_gate"
		}
		s.noteAcknowledgementParser(ctx, scope)
		lap = s.beginAcknowledgementLap(ctx, editAckParseGate)
	}
	var budgetMu sync.Mutex
	var bytes int64
	files := 0
	jobs := make(chan *renameFileWrite)
	var workers sync.WaitGroup
	for range min(2, len(writes)) {
		workers.Go(func() {
			for write := range jobs {
				candidate, syntax := s.buildFileProjection(ctx, write.AbsPath, write.New)
				if gated {
					write.Gate = projectedParseGate(write.RelPath, write.Old, write.New, syntax)
				}
				budgetMu.Lock()
				if candidate.entry != nil {
					if files >= projectionMaxFiles || bytes+candidate.entry.size > projectionMaxBytes {
						candidate.entry = nil
						candidate.refusal = "cohort_preparation_limit"
					} else {
						files++
						bytes += candidate.entry.size
					}
				}
				write.projection = candidate
				budgetMu.Unlock()
			}
		})
	}
	for _, write := range writes {
		jobs <- write
	}
	close(jobs)
	workers.Wait()
	checked := false
	for _, write := range writes {
		checked = checked || write.Gate.Checked
	}
	lap.applicable = !gated || checked
	lap.finish()
	for _, write := range writes {
		if write.Gate.Blocked && !allowParseErrors {
			return nil, fmt.Errorf("%s", parseGateError(write.RelPath, write.Gate))
		}
		write.GateInfo = parseGateInfo(write.Gate, allowParseErrors)
	}
	if priorError != nil {
		return nil, priorError
	}
	for _, write := range writes {
		candidate := write.projection
		if candidate.entry == nil {
			continue
		}
		previousSize := candidate.entry.size
		s.prepareFileViewEvidence(ctx, candidate, baselineForPreparedWrite(write.AbsPath, write.Old), write.New)
		if bytes+candidate.entry.size-previousSize > projectionMaxBytes {
			bytes -= previousSize
			candidate.entry = nil
			candidate.refusal = "cohort_preparation_limit"
		} else {
			bytes += candidate.entry.size - previousSize
		}
	}
	return writes, nil
}
