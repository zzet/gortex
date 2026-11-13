package indexer

import "slices"

// SetContractAnalysisCoordinator connects this lifecycle to its own installed
// producer. The daemon installs it before admitting checkouts; existing owners
// use the same atomic binding so an installation cannot race an attempt.
func (l *CheckoutLifecycle) SetContractAnalysisCoordinator(runtime *ContractAnalysisCoordinator) {
	l.coordMu.Lock()
	defer l.coordMu.Unlock()
	l.contractAnalysis.Store(runtime)
	for _, c := range l.coordinators {
		c.contractAnalysis.Store(runtime)
	}
}

// ContractInterest is called after the scheduler has released its locks. It
// targets only this checkout's currently armed semantic attempt, never its
// parent refresh ticket or another checkout's build.
func (l *CheckoutLifecycle) ContractInterest(actor ContractActor) {
	l.coordMu.Lock()
	c := l.coordinators[actor.Checkout]
	l.coordMu.Unlock()
	if c == nil || !slices.Contains(actor.Repos(), c.repoPrefix) || !c.pendingContractWork() {
		return
	}
	c.motion.mu.Lock()
	attempt := c.motion.semantic
	c.motion.mu.Unlock()
	if attempt != nil {
		attempt.deferForContractDemand()
	}
}

func (c *CheckoutCoordinator) pendingContractWork() bool {
	runtime := c.contractAnalysis.Load()
	return runtime != nil && runtime.PendingLinkedWork(c.checkoutID, c.repoPrefix)
}

// PendingLinkedWork includes accepted queued/capturing/running projections
// until their live interest ends. Inputs-pending, baseline and backoff actors
// cannot hold the semantic publication they might themselves need to proceed.
// Starting a fill does not authorize a follow-up to race its projection proof.
func (c *ContractAnalysisCoordinator) PendingLinkedWork(checkout, repo string) bool {
	if checkout == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	now := c.now()
	for _, a := range c.actors {
		if a.id.Checkout != checkout || !a.inCohort(repo) || a.want.inputs == nil || !a.want.inputs.State.Accepted || !now.Before(a.interestUntil) {
			continue
		}
		switch a.state {
		case contractActorQueued, contractActorCapturing, contractActorRunning:
			return true
		}
	}
	return false
}
