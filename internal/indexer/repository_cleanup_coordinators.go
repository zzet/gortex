package indexer

import "github.com/zzet/gortex/internal/viewmetrics"

// Snapshot every started actor, including off-route transition actors which
// have not yet entered coordinators. No Close or workspace join runs under
// coordMu. The caller must close repository admission and repeat this snapshot
// after constructor pins drain before declaring producer quiescence.
func (l *CheckoutLifecycle) closeRepositoryCoordinators(prefix string) {
	l.coordMu.Lock()
	actors := make(map[*CheckoutCoordinator]struct{})
	for _, started := range l.started {
		for _, actor := range started {
			if actor != nil && actor.repoPrefix == prefix {
				actors[actor] = struct{}{}
			}
		}
	}
	for _, actor := range l.coordinators {
		if actor != nil && actor.repoPrefix == prefix {
			actors[actor] = struct{}{}
		}
	}
	l.coordMu.Unlock()
	for actor := range actors {
		_ = actor.Close()
		l.oweRetirement(actor.DrainRetirements()...)
		l.stopCheckoutWorkspaces(actor.root)
	}
	l.coordMu.Lock()
	for checkoutID, actor := range l.coordinators {
		if _, closed := actors[actor]; closed {
			delete(l.coordinators, checkoutID)
			delete(l.coordinatorHeads, checkoutID)
		}
	}
	for checkoutID, started := range l.started {
		retained := started[:0]
		for _, actor := range started {
			if _, closed := actors[actor]; !closed {
				retained = append(retained, actor)
			}
		}
		if len(retained) == 0 {
			delete(l.started, checkoutID)
		} else {
			l.started[checkoutID] = retained
		}
	}
	viewmetrics.SetGauge(viewmetrics.Coordinators, int64(len(l.coordinators)))
	l.coordMu.Unlock()
}
