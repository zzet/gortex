package main

import (
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// buildLaneBusyReceiver is the store capability the background WAL
// checkpoints use to stay out of edit-driven cycles: while one holds the build
// lane, no checkpoint attempt starts and one in flight is cancelled (bounded:
// the periodic PASSIVE runs anyway once deferred long enough with the WAL over
// its threshold, and the reclaim runs anyway over the WAL ceiling).
type buildLaneBusyReceiver interface {
	SetBuildLaneBusy(busy func() bool)
}

// installBuildLaneBusy hands the store a predicate over the daemon's one
// build lane. A store without the capability (not SQLite) is left alone.
func installBuildLaneBusy(g graph.Store, gate *indexer.ViewBuildGate) {
	if g == nil || gate == nil {
		return
	}
	receiver, ok := g.(buildLaneBusyReceiver)
	if !ok {
		return
	}
	receiver.SetBuildLaneBusy(func() bool {
		st := gate.Stats()
		return st.Active && buildLaneHolderIsEditCycle(st.Holder)
	})
}

// buildLaneHolderIsEditCycle reports a lane holder that is an edit-driven
// mutation cycle: a checkout mutation (an edit's synchronous republish) or a
// checkout cycle admitted at interactive priority (one a refresh ticket, i.e.
// an edit, asked for). Background builds — a warm-up rebuild of an untouched
// checkout, a watcher-noticed cycle, propagation, a transition, dirty-chain
// compaction, retirement, or a holder that declared nothing — never pause the
// checkpoints.
func buildLaneHolderIsEditCycle(holder *indexer.ViewBuildLaneHolder) bool {
	if holder == nil {
		return false
	}
	switch holder.Kind {
	case "checkout_mutation":
		return true
	case "checkout_cycle":
		return holder.Priority == viewmetrics.BuildPriorityInteractive
	default:
		return false
	}
}
