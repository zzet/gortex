package mcp

import (
	"reflect"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// walReclaimSnapshotReader is intentionally smaller than daemon storage status:
// WALReclaimStats copies counters under a short mutex plus atomic loads. It
// does not stat files, read the wal-index, query SQL or wait for the writer.
type walReclaimSnapshotReader interface {
	WALReclaimStats() store_sqlite.WALReclaimStats
}

type walReclaimObservation struct {
	Scope                 string `json:"scope"`
	ObservationEpoch      string `json:"observation_epoch"`
	EpochScope            string `json:"epoch_scope"`
	SnapshotAvailable     bool   `json:"snapshot_available"`
	Reason                string `json:"reason,omitempty"`
	CancelledWriterParked *int64 `json:"cancelled_writer_parked"`
	CancelledWriteIntent  *int64 `json:"cancelled_write_intent"`
	CancelledOther        *int64 `json:"cancelled_other"`
	ObservedPeakWALBytes  *int64 `json:"observed_peak_wal_bytes"`
	WALPeakObserved       bool   `json:"wal_peak_observed"`
	PeakScope             string `json:"peak_scope"`
}

// walReclaimObservation uses the Server's constructor-bound physical store,
// never a selected/composed reader or an unwrapped base. The Server epoch is
// only an observation-binding correlation, not a durable store-open identity.
func (s *Server) walReclaimObservation() walReclaimObservation {
	result := walReclaimObservation{
		Scope: "physical_store_since_open", EpochScope: "server_binding",
		PeakScope: "sampled_since_store_open_lower_bound",
	}
	if s == nil || s.graph == nil {
		result.Reason = "store_unavailable"
		return result
	}
	if s.editCPU != nil {
		result.ObservationEpoch = s.editCPU.epoch
	}
	reader, ok := s.graph.(walReclaimSnapshotReader)
	if !ok {
		result.Reason = "unsupported_store"
		return result
	}
	if value := reflect.ValueOf(reader); value.Kind() == reflect.Pointer && value.IsNil() {
		result.Reason = "store_unavailable"
		return result
	}
	stats := reader.WALReclaimStats() // exactly one existing, I/O-free snapshot
	parked, intent, other := stats.CancelledWriterParked, stats.CancelledWriteIntent, stats.CancelledOther
	result.SnapshotAvailable = true
	result.CancelledWriterParked, result.CancelledWriteIntent, result.CancelledOther = &parked, &intent, &other
	result.WALPeakObserved = stats.WALPeakObserved
	if stats.WALPeakObserved {
		peak := stats.ObservedPeakWALBytes
		result.ObservedPeakWALBytes = &peak
	}
	return result
}
