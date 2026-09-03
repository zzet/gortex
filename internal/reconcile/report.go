package reconcile

import "github.com/zzet/gortex/internal/graph/store_sqlite"

// CheckoutAction names what a reconciliation pass did about one checkout.
// The report exists so a caller — a later daemon wiring, or a test — can
// assert on decisions instead of scraping logs for them.
type CheckoutAction string

const (
	// ActionObserved means the checkout was seen and deliberately not
	// persisted. It is the ephemeral outcome.
	ActionObserved CheckoutAction = "observed"
	// ActionIdentityAllocated means a durable identity was minted for a
	// checkout the catalog had never seen.
	ActionIdentityAllocated CheckoutAction = "identity_allocated"
	// ActionReadyConfirmed means a ready checkout was still ready.
	ActionReadyConfirmed CheckoutAction = "ready_confirmed"
	// ActionAvailabilityRecovered means an unavailable-or-grace checkout
	// answered again and went back to ready under its original identity.
	ActionAvailabilityRecovered CheckoutAction = "availability_recovered"
	// ActionAvailabilityGraceStarted means the availability clock was started.
	ActionAvailabilityGraceStarted CheckoutAction = "availability_grace_started"
	// ActionAvailabilityHeld means the checkout is still unreachable and the
	// availability axis did not move this pass.
	ActionAvailabilityHeld CheckoutAction = "availability_held"
	// ActionMarkedUnavailable is retained for catalog/report compatibility with
	// older daemons. New passes retire an inaccessible checkout when its grace
	// expires instead of leaving this terminal state behind.
	ActionMarkedUnavailable CheckoutAction = "marked_unavailable"
	// ActionRemovalGraceStarted means a removal was evidenced and the removal
	// clock was started.
	ActionRemovalGraceStarted CheckoutAction = "removal_grace_started"
	// ActionRemovalHeld means the removal clock is running and has not expired.
	ActionRemovalHeld CheckoutAction = "removal_held"
	// ActionRemovalCancelled means the same incarnation came back before its
	// removal deadline, so the removal clock was cleared.
	ActionRemovalCancelled CheckoutAction = "removal_cancelled"
	// ActionForgotten means the removal deadline passed and the forget saga
	// removed the checkout.
	ActionForgotten CheckoutAction = "forgotten"
	// ActionPrimaryClosureRetired means the removed checkout owned its
	// family's primary graph, so the whole closure retired with it.
	ActionPrimaryClosureRetired CheckoutAction = "primary_closure_retired"
	// ActionGuardLost means another actor moved first and this pass declined
	// to act over the top of it: a guarded write or a guarded allocation
	// changed nothing, or a teardown was refused because the identity or the
	// family's primary had already moved on.
	ActionGuardLost CheckoutAction = "guard_lost"
)

// CheckoutReport is what one pass decided about one checkout.
type CheckoutReport struct {
	// AdminName is the administrative name the identity is keyed on.
	AdminName string
	// RootPath is the worktree root the pass looked at.
	RootPath string
	// Main is true for the family's main worktree.
	Main bool
	// CheckoutID is the durable identity, empty for an ephemeral observation.
	CheckoutID string
	// Incarnation is the identity's incarnation, empty when ephemeral.
	Incarnation string
	// Durable is true when the pass read or wrote a catalog row for this
	// checkout, false when it was only observed.
	Durable bool
	// Action is what the pass did.
	Action CheckoutAction
	// State is the checkout's lifecycle state after the pass. It is empty for
	// an ephemeral observation, which has no state to be in.
	State store_sqlite.CheckoutState
	// Classification is the verdict the action followed from.
	Classification Classification
	// RetryAt is the Unix deadline when this family must be reconciled again
	// even if no filesystem event arrives. It is set while a removal or
	// availability grace is active.
	RetryAt int64
	// Detail explains a pass-level decision the classification alone does not
	// — why an accessible checkout stayed ephemeral, for instance.
	Detail string
}

// FamilyReport is the result of reconciling one checkout family.
type FamilyReport struct {
	// FamilyID is the family that was reconciled.
	FamilyID string
	// CommonDir is the shared git directory the pass worked against.
	CommonDir string
	// InventoryUsable is false when git could not be trusted this pass. A first
	// such observation starts availability grace; a later pass at its deadline
	// may still retire the inaccessible checkout.
	InventoryUsable bool
	// PrimaryGraphID is the family's primary dedicated graph, empty when it
	// has none.
	PrimaryGraphID string
	// Code carries graphview.CodeNoPrimary when the family has no primary
	// dedicated graph, and is empty otherwise.
	Code string
	// Checkouts holds one entry per checkout the pass considered: first the
	// identities the catalog already held, then the records git reported that
	// no identity matched.
	Checkouts []CheckoutReport
}
