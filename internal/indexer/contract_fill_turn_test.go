package indexer

import (
	"context"
	"fmt"
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
	t.Run("revalidated_then_partial", func(t *testing.T) {
		c := newTurn()
		require.NoError(t, c.continueRevalidated(3, true), "a few suspects revalidated before any extraction are not partial progress")
		require.False(t, c.req.delta.continued)
		require.ErrorAs(t, c.continueFill(7, true), new(*ContractFillContinued), "a partial extraction round still hands off")
		require.Equal(t, 10, c.fillTurnProven)
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

// TestContractFillTurnExtractsAfterRevalidatedSuspects: a contended fill turn
// that starts with suspects revalidates them and goes on to extract before it
// hands the slot over, rather than ending with nothing extracted; the fill
// still converges to the cold result.
func TestContractFillTurnExtractsAfterRevalidatedSuspects(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts}
	// The helpers sort after consts.go and routes.go, so the first turn
	// proves both.
	for i := range 80 {
		files[fmt.Sprintf("zz/helper%02d.go", i)] = fmt.Sprintf("package zz\nfunc Helper%d() int { return %d }\n", i, i)
	}
	f := newContractJobFixture(t, files)
	var cache *contractDeltaCache
	turn := func() (ContractFollowupReport, error) {
		f.layers++
		return runFixtureContractJob(t, f.options, contractFixtureJob{cache: &cache, otherActorWaiting: func() bool { return true }, layer: f.layers, epoch: f.epoch.Load, wait: f.waitPublished})
	}
	report, err := turn()
	require.ErrorAs(t, err, new(*ContractFillContinued))
	require.Equal(t, contractRoundFiles, report.Files)
	// consts.go moves without changing the route routes.go reads: the units
	// the first turn proved become suspects their revalidation proves again.
	files["consts.go"] = "package fixture\n// moved\nconst RoutePath = \"/before\"\n"
	f.save("consts.go", files["consts.go"])
	report, err = turn()
	require.ErrorAs(t, err, new(*ContractFillContinued))
	require.Positive(t, report.Revalidated)
	require.GreaterOrEqual(t, report.Files, 16, "the turn extracts after its suspects (revalidated %d)", report.Revalidated)
	for range 4 {
		if err == nil {
			break
		}
		require.ErrorAs(t, err, new(*ContractFillContinued))
		report, err = turn()
	}
	require.NoError(t, err)
	require.True(t, report.Published)
	require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(f.contracts()))
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
