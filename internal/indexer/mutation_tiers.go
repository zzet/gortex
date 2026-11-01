package indexer

import "time"

// MutationTierState is the publication evidence for one admitted path. An
// absent timestamp is unobserved, rather than evidence of a fresh tier.
type MutationTierState struct {
	Generation  uint64    `json:"generation"`
	AckAt       time.Time `json:"ack_at"`
	FileGraphAt time.Time `json:"file_graph_at,omitempty"`
	RelationsAt time.Time `json:"relations_at,omitempty"`
	DerivedAt   time.Time `json:"derived_at,omitempty"`
	Failed      bool      `json:"failed,omitempty"`
	Partial     bool      `json:"partial,omitempty"`
}

// TierState reads a ticket's live stamps; the ticket survives waiter removal.
func (t *MutationTicket) TierState() MutationTierState {
	if t == nil {
		return MutationTierState{}
	}
	t.tierMu.RLock()
	defer t.tierMu.RUnlock()
	return t.tiers
}
