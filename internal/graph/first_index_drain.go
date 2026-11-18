package graph

import "time"

// FirstIndexDrain is the last full shadow persisted for one repository during
// this store's lifetime. It is an observation, not a readiness or durability
// verdict. A coordinated window defers its final seal to the outer owner.
type FirstIndexDrain struct {
	Window          string    `json:"window"`
	Nodes           int       `json:"nodes"`
	Edges           int       `json:"edges"`
	DrainMS         float64   `json:"drain_ms"`
	SealMS          float64   `json:"seal_ms"`
	CompletedAt     time.Time `json:"completed_at"`
	Error           string    `json:"error,omitempty"`
	WALPeakBytes    int64     `json:"wal_peak_bytes,omitempty"`
	WALPeakObserved bool      `json:"wal_peak_observed"`
	// WAL peak is sampled at bounded drain/FTS chunks and seal boundaries.
	// The WAL is shared: overlapping work may contribute to these observations.
	WALPeakScope string `json:"wal_peak_scope"`
}
