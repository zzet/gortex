package graphview

// ReasonDeferredToFollowup is the reason a generation records, with the state
// incomplete, for a producer whose data it left behind for the
// post-publication follow-up: incomplete now, completed by a layer above that
// covers the same paths with the follow-up. It is a fixed token so code can
// tell it from a producer that is incomplete for good (a sparse generation's
// similarity); the writer, the debt's derivation and the rider compare it.
const ReasonDeferredToFollowup = "deferred_to_followup"

// ReasonContractsPending records external contract work separately from graph
// publication. Only acknowledgment of its exact work tokens clears the debt;
// replacing or deleting a file does not establish completion.
const ReasonContractsPending = "contracts_pending"
