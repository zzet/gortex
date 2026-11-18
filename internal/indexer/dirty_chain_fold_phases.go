package indexer

import (
	"context"

	"go.uber.org/zap"
)

// inlineFoldPhases is local to one debug-observed inline attempt. Its laps
// are exclusive; a failure closes its operation before abandonment cleanup.
// Cleanup inside a copier remains part of copy, since its callback owns it.
type inlineFoldPhases struct {
	clock       phaseClock
	phases      []GenerationPhase
	stage       string
	failedStage string
}

type inlineFoldPhasesKey struct{}

func inlineFoldPhasesFrom(ctx context.Context) *inlineFoldPhases {
	if ctx == nil {
		return nil
	}
	phases, _ := ctx.Value(inlineFoldPhasesKey{}).(*inlineFoldPhases)
	return phases
}

func (p *inlineFoldPhases) next(stage string) {
	if p == nil {
		return
	}
	p.clock.lap(p.stage)
	p.stage = stage
}

func (p *inlineFoldPhases) fail() {
	if p == nil || p.failedStage != "" {
		return
	}
	p.failedStage = p.stage
	p.next("cleanup")
}

func (p *inlineFoldPhases) log(logger *zap.Logger, checkout string, top int64, err error) {
	if p == nil {
		return
	}
	// Early planning/reservation failures do not run abandonment cleanup.
	if err != nil && p.failedStage == "" {
		p.failedStage = p.stage
	}
	p.clock.lap(p.stage)
	fields := []zap.Field{
		zap.String("checkout", checkout), zap.Int64("chain_top", top),
		zap.String("failed_stage", p.failedStage), zap.Error(err),
	}
	fields = append(fields, phaseFields("phase_fold_", p.phases)...)
	if p.failedStage != "" {
		for _, phase := range p.phases {
			if phase.Name == p.failedStage {
				fields = append(fields, zap.Float64("failed_stage_elapsed_ms", float64(phase.Duration.Microseconds())/1000))
				break
			}
		}
	}
	logger.Debug("checkout coordinator: inline fold phases", fields...)
}
