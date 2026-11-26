package main

import "time"

// A product deadline keeps its outcome. Its reason names what the receipt
// loop still awaited when the edit's own context expired, from that loop's
// latest evaluation of its exit predicate. A deadline whose graph was terminal
// and fresh, with every oracle first-correct and every assigned post-ACK
// witness present, waited only for accounting; it is told apart from a graph
// that never finished. Attribution only: neither the outcome, the errors nor
// any client time changes.
const (
	editProbeDeadlineReadbacks  = "immediate_readbacks_outstanding"
	editProbeDeadlineUnobserved = "receipt_status_unobserved"
	editProbeDeadlineGraph      = "graph_not_terminal"
	editProbeDeadlineOracles    = "oracles_not_correct"
	editProbeDeadlineCoverage   = "post_ack_coverage_outstanding"
	editProbeDeadlineTiers      = "qualified_tiers_not_fresh"
	editProbeDeadlineAccounting = "accounting_non_terminal"
)

// The receipt loop's latest decoded evaluation of its exit predicate.
type editProbeReceiptWait struct {
	entered    bool // the loop started before the edit's deadline
	observed   bool // at least one own status reply was evaluated
	terminal   bool
	allCorrect bool
	coverage   bool // relation consumers finished and assigned gate witnesses present
	qualified  bool // a coalesced or functional-unavailable candidate
	freshTiers bool
}

func editProbeDeadlineReason(wait editProbeReceiptWait) string {
	switch {
	case !wait.entered:
		return editProbeDeadlineReadbacks
	case !wait.observed:
		return editProbeDeadlineUnobserved
	case !wait.terminal:
		return editProbeDeadlineGraph
	case !wait.allCorrect:
		return editProbeDeadlineOracles
	case !wait.coverage:
		return editProbeDeadlineCoverage
	case wait.qualified && !wait.freshTiers:
		return editProbeDeadlineTiers
	default:
		// Every other conjunct of the exit predicate held, so the loop was
		// still waiting for its required accounting.
		return editProbeDeadlineAccounting
	}
}

// Dispatcher busy refusals (-32002) per call purpose, from one logical
// call's attempts. The admitted attempt and other errors are not counted.
func editProbeBusyRefusals(attempts []Attempt) int {
	busy := 0
	for _, attempt := range attempts {
		if attempt.Code == -32002 {
			busy++
		}
	}
	return busy
}

func (s *editProbeState) countBusy(purpose string, attempts []Attempt) {
	busy := editProbeBusyRefusals(attempts)
	if busy == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.BusyRefusals == nil {
		s.record.BusyRefusals = make(map[string]int)
	}
	s.record.BusyRefusals[purpose] += busy
}

// The caller holds s.mu. Client time from edit send to the first own status
// reply that reported the graph terminal.
func (s *editProbeState) noteGraphTerminalLocked(at time.Time) {
	if s.record.GraphTerminalMS != nil || at.IsZero() || s.started.IsZero() || at.Before(s.started) {
		return
	}
	metric := measured(float64(at.Sub(s.started))/float64(time.Millisecond), "ms")
	s.record.GraphTerminalMS = &metric
}

func summarizeEditProbeAttribution(out *editProbeSummary, records []editProbeRecord) {
	for _, record := range records {
		if record.Outcome == "product_deadline" && record.DeadlineReason != "" {
			if out.DeadlineReasons == nil {
				out.DeadlineReasons = make(map[string]int)
			}
			out.DeadlineReasons[record.DeadlineReason]++
		}
		if len(record.BusyRefusals) == 0 {
			continue
		}
		if out.BusyRefusals == nil {
			out.BusyRefusals = make(map[string]int)
		}
		for purpose, count := range record.BusyRefusals {
			out.BusyRefusals[purpose] += count
		}
	}
}
