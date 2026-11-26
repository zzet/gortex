package indexer

import (
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// SQLite writer time of one physical attempt.
//
// An observed point attempt runs inside a store_sqlite writer attribution, so
// the receipt's accepted physical attempt reports the SQLite xWrite time of
// the write-gate holds its own goroutine took, rather than a process-wide
// counter that every concurrent writer adds to. Other editors' attempts and
// acknowledgements, maintenance and goroutines the attempt starts are never
// counted, and attempts open at the same time each keep their own holds.

// writerAttributionState is the attempt's attribution and its reading when
// the derived lap began. Guarded by PublicationStamps.mu.
type writerAttributionState struct {
	attribution   *store_sqlite.WriterAttribution
	beforeDerived *store_sqlite.WriterAttributionSnapshot
}

// openWriterAttribution creates the attempt's attribution; nil when the
// attempt is unobserved or already has one.
func (s *PublicationStamps) openWriterAttribution() *store_sqlite.WriterAttribution {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.patchLaps
	if p == nil || p.writer.attribution != nil {
		return nil
	}
	p.writer.attribution = &store_sqlite.WriterAttribution{}
	return p.writer.attribution
}

// bindWriterGraph names the graph the attempt patches. The caller holds the
// repository lane and the watcher's patch lock, so g is the graph the
// attempt's writes reach and the store's other holds from now on overlap the
// attempt's own patch.
func (s *PublicationStamps) bindWriterGraph(g graph.Store) {
	if s == nil {
		return
	}
	s.mu.Lock()
	var a *store_sqlite.WriterAttribution
	if s.patchLaps != nil {
		a = s.patchLaps.writer.attribution
	}
	s.mu.Unlock()
	if a == nil {
		return
	}
	store, _ := g.(*store_sqlite.Store)
	a.BindStore(store)
}

// cutWriterBeforeDerivedLocked keeps the attempt's reading at the start of
// its derived lap. Caller holds s.mu.
func (p *canonicalPatchLaps) cutWriterBeforeDerivedLocked() {
	if p.writer.attribution != nil && p.writer.beforeDerived == nil {
		cut := p.writer.attribution.Snapshot()
		p.writer.beforeDerived = &cut
	}
}

// canonicalWriterEvidence is the attempt's writer reading frozen at its
// return, with hold boundaries as offsets from the receipt origin.
type canonicalWriterEvidence struct {
	opened                  bool
	total                   store_sqlite.WriterAttributionSnapshot
	beforeDerived           store_sqlite.WriterAttributionSnapshot
	derivedCut              bool
	firstHoldNS, lastHoldNS int64
}

func (p *canonicalPatchLaps) freezeWriterEvidence(origin time.Time) canonicalWriterEvidence {
	if p.writer.attribution == nil {
		return canonicalWriterEvidence{}
	}
	out := canonicalWriterEvidence{opened: true, total: p.writer.attribution.Snapshot()}
	if out.total.Available && out.total.OpenHolds > 0 {
		out.total.Available, out.total.Reason = false, "owned_hold_open_at_attempt_return"
	}
	if cut := p.writer.beforeDerived; cut != nil {
		out.beforeDerived, out.derivedCut = *cut, true
	}
	if out.total.Holds > 0 {
		out.firstHoldNS = out.total.FirstHoldStart.Sub(origin).Nanoseconds()
		out.lastHoldNS = out.total.LastHoldEnd.Sub(origin).Nanoseconds()
	}
	return out
}

// EditWriterWrite is the SQLite writer time of one accepted physical attempt:
// the xWrite calls of the store's writer connection, on the main database
// file and the log, made under the write-gate holds the attempt's own
// goroutine took. Holds of any other goroutine are excluded and holds never
// overlap, so every sum is of disjoint intervals. Every number is null unless
// State is "measured"; a measured zero means the attempt took no owned hold.
type EditWriterWrite struct {
	State  string  `json:"state"`
	Reason *string `json:"reason"`
	Scope  string  `json:"scope"`
	Clock  string  `json:"clock"`
	// Owned xWrite time on the main database file and the log, together and
	// apart, with the call and byte counts. Main-file writes can include the
	// pages of an automatic checkpoint that one of the attempt's commits ran.
	XWriteMS     *float64 `json:"xwrite_ms"`
	MainXWriteMS *float64 `json:"main_xwrite_ms"`
	WALXWriteMS  *float64 `json:"wal_xwrite_ms"`
	XWriteCalls  *int64   `json:"xwrite_calls"`
	XWriteBytes  *int64   `json:"xwrite_bytes"`
	// Owned holds: their count, their summed acquisition-to-release wall
	// (the commit windows the xWrite time lies in), and the first
	// acquisition and last release as offsets from the attempt's start, the
	// window patch_wall_ms measures.
	Holds                  *int64   `json:"holds"`
	HoldMS                 *float64 `json:"hold_ms"`
	FirstHoldStartOffsetMS *float64 `json:"first_hold_start_offset_ms"`
	LastHoldEndOffsetMS    *float64 `json:"last_hold_end_offset_ms"`
	// The owned xWrite time and hold wall completed before the attempt's
	// derived lap began; DerivedCut says which boundary applied: the derived
	// lap's start, or the attempt's return when it entered no derived lap.
	DerivedCut            string   `json:"derived_cut"`
	BeforeDerivedXWriteMS *float64 `json:"before_derived_xwrite_ms"`
	BeforeDerivedHoldMS   *float64 `json:"before_derived_hold_ms"`
	// Holds of the same store's writer gate completed while the attempt
	// patched (from its patch lock to its return) that it did not own: other
	// writers sharing the store, or goroutines the attempt started. Zero
	// means the attempt's own holds were the store's only writes meanwhile.
	UnownedStoreHolds *int64 `json:"unowned_store_holds"`
}

func durationMS(d time.Duration) *float64 {
	ms := float64(d.Nanoseconds()) / 1e6
	return &ms
}

func (w canonicalAttemptWindow) projectWriterWrite() EditWriterWrite {
	e := w.accounting.writer
	out := EditWriterWrite{State: "not_observed", Scope: "write_gate_holds_acquired_by_attempt_goroutine", Clock: "monotonic", DerivedCut: "unavailable"}
	reason := ""
	switch {
	case !e.opened:
		reason = "attempt_not_attributed"
	case !e.total.Available:
		out.State, reason = "unavailable", e.total.Reason
	case e.total.Holds > 0 && (e.firstHoldNS < w.startNS || e.lastHoldNS > w.endNS || e.lastHoldNS < e.firstHoldNS):
		out.State, reason = "invalid_order", "hold_outside_attempt_window"
	case e.total.UnownedStoreHolds < 0:
		out.State, reason = "invalid_order", "store_holds_below_owned_holds"
	}
	if reason != "" {
		out.Reason = &reason
		return out
	}
	t := e.total
	calls, bytes, holds, unowned := t.MainWrites+t.WALWrites, t.MainWriteBytes+t.WALWriteBytes, t.Holds, t.UnownedStoreHolds
	out.State = "measured"
	out.XWriteMS, out.MainXWriteMS, out.WALXWriteMS = durationMS(t.WriteTime()), durationMS(t.MainWriteTime), durationMS(t.WALWriteTime)
	out.XWriteCalls, out.XWriteBytes, out.Holds, out.HoldMS = &calls, &bytes, &holds, durationMS(t.HoldTime)
	out.UnownedStoreHolds = &unowned
	if holds > 0 {
		out.FirstHoldStartOffsetMS = durationMS(time.Duration(e.firstHoldNS - w.startNS))
		out.LastHoldEndOffsetMS = durationMS(time.Duration(e.lastHoldNS - w.startNS))
	}
	switch {
	case !e.derivedCut:
		out.DerivedCut = "derived_lap_not_entered"
		out.BeforeDerivedXWriteMS, out.BeforeDerivedHoldMS = out.XWriteMS, out.HoldMS
	case e.beforeDerived.Available && e.beforeDerived.OpenHolds == 0:
		out.DerivedCut = "derived_lap_started"
		out.BeforeDerivedXWriteMS, out.BeforeDerivedHoldMS = durationMS(e.beforeDerived.WriteTime()), durationMS(e.beforeDerived.HoldTime)
	}
	return out
}
