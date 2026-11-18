package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func TestContractFillTurnCoalescesOnlyProductiveCompetition(t *testing.T) {
	newTurn := func() *contractConvergence {
		return &contractConvergence{ctx: t.Context(), report: &ContractFollowupReport{Mode: viewmetrics.ContractModeFill},
			req: ContractFollowupRequest{delta: &contractDeltaSession{}, OtherActorWaiting: func() bool { return true }}}
	}
	t.Run("productive", func(t *testing.T) {
		c := newTurn()
		for range 3 {
			require.NoError(t, c.continueFill(16, true))
			require.False(t, c.req.delta.continued)
		}
		require.ErrorAs(t, c.continueFill(16, true), new(*ContractFillContinued))
		require.Equal(t, contractRoundFiles, c.fillTurnProven)
	})
	t.Run("partial", func(t *testing.T) {
		c := newTurn()
		require.ErrorAs(t, c.continueFill(7, true), new(*ContractFillContinued))
	})
	t.Run("no_progress", func(t *testing.T) {
		c := newTurn()
		require.NoError(t, c.continueFill(0, true))
		require.False(t, c.req.delta.continued)
	})
	t.Run("demand_after_competitor_question", func(t *testing.T) {
		c := newTurn()
		var asked bool
		c.req.OtherActorWaiting = func() bool { asked = true; return true }
		c.req.checkpointDemand = func() bool { require.True(t, asked); return true }
		require.ErrorAs(t, c.continueFill(16, true), new(*ContractFillContinued))
		require.Equal(t, 16, c.fillTurnProven)
	})
	t.Run("cancel_before_checkpoint", func(t *testing.T) {
		c := newTurn()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		c.ctx = ctx
		require.ErrorIs(t, c.continueFill(16, true), context.Canceled)
		require.False(t, c.req.delta.continued)
		require.Zero(t, c.fillTurnProven)
	})
}

func TestContractFillDemandCheckpointKeepsOverlapAndStrictDecision(t *testing.T) {
	for _, competitor := range []bool{false, true} {
		t.Run(map[bool]string{false: "demand_only", true: "competitor_overlap"}[competitor], func(t *testing.T) {
			h := newLinkedHoldProbe(t, ContractLinkedIdle, nil)
			a := &contractActor{id: NewContractActor("wt", []string{"r"})}
			job := &contractAnalysisJob{otherActorWaiting: func() bool { return competitor }}
			job.demandSeen.Store(true)
			h.c.linkJob(a, job)
			require.True(t, job.otherActorWaiting())
			require.Equal(t, !competitor, job.demandStop.Load(), "retain competitor-first metric classification")
			require.True(t, job.checkpointDemand(), "both causes retain immediate checkpoint release")
			if !competitor {
				h.c.mu.Lock()
				a.strictUntil = h.now().Add(time.Second)
				h.c.mu.Unlock()
				require.True(t, job.checkpointDemand(), "a subsequent strict sample cannot erase the recorded stop")
				require.False(t, job.otherActorWaiting(), "the next original question observes the new strict fence")
				require.False(t, job.checkpointDemand())
			}
		})
	}
}
