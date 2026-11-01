package indexer

// ReadRepairPhaseAccounting retains only immutable scalar evidence from the
// coordinator's existing executor window. It has no collector, record or owner
// pointer and cannot extend a receipt's lifetime or rewrite earlier snapshots.
type ReadRepairPhaseAccounting struct {
	generation uint64
	windowNS   int64
	evidence   canonicalAttemptEvidence
}

func (o *ReadRepairObservation) beginPhaseAccounting() {
	o.collector = &PublicationStamps{patchLaps: &canonicalPatchLaps{}}
	o.collector.startAttemptControlKind(o.started, patchExecutorControl)
	o.collector.markAt(PublicationPatchExecutionStarted, o.started)
}

func (o *ReadRepairObservation) finishPhaseAccounting() {
	s := o.collector
	if s == nil {
		return
	}
	s.markAt(PublicationPatchExecutionFinished, o.finished)
	s.mu.Lock()
	s.patchLaps.finishAttemptControlLocked(o.finished)
	o.accounting = ReadRepairPhaseAccounting{
		generation: o.generation, windowNS: o.finished.Sub(o.started).Nanoseconds(),
		evidence: s.freezeAttemptEvidence(o.started),
	}
	s.mu.Unlock()
	o.collector = nil // no mutable carrier reaches completion callbacks
}

// PhaseAccounting is independent of the accepted receipt's A, B and no-op P.
// AcceptedFor must still establish actual owner/bytes before ledger retention.
func (o ReadRepairObservation) PhaseAccounting() ReadRepairPhaseAccounting { return o.accounting }

// Payload allocates caller-owned projection maps only when status is read.
// Missing publisher phases stay null, never inferred from the enclosing wall.
func (a ReadRepairPhaseAccounting) Payload() map[string]any {
	w := canonicalAttemptWindow{endNS: a.windowNS, accounting: a.evidence}
	p := w.evaluateAccountingFor(true)
	phases, states := map[string]any{}, map[string]string{}
	for i, name := range canonicalAttemptLapNames {
		kind := canonicalPatchLap(i)
		if kind >= canonicalDeepLapCount && kind != patchExecutorControl {
			continue
		}
		if kind == patchExecutorControl {
			name = "repair_control"
		}
		phases[name], states[name] = nil, p.states[i]
		if p.states[i] == "measured" {
			phases[name] = p.values[i]
		}
	}
	out := map[string]any{
		"scope": "read_assisted_reconcile", "clock": "monotonic",
		"owner_generation": a.generation, "sum_state": p.state,
		"observed_partition_complete": p.complete,
		"phases_ms":                   phases, "phase_states": states,
		"additive_with_requested_background": false,
	}
	if a.evidence.observed {
		out["window_ns"] = a.windowNS // actual frozen bounds remain known even for invalid laps
	}
	if p.state != "not_observed" && p.state != "invalid_order" {
		out["window_ns"], out["observed_subtotal_ns"], out["unattributed_ns"] = a.windowNS, p.sumNS, a.windowNS-p.sumNS
	}
	return out
}
