package indexer

// EditPhaseTimings projects duration evidence from a single immutable
// publication snapshot. Missing measurements are null, never measured zero.
// This is deliberately partial: publication timestamps do not yet delimit
// dispatcher admission, the response/ack, or the complete patch-lap split.
type EditPhaseTimings struct {
	Milliseconds map[string]*float64 `json:"phases_ms"`
	States       map[string]string   `json:"phase_states"`
	Window       EditTimingWindow    `json:"phase_timing"`
}

// EditTimingWindow distinguishes the receipt's publication interval from the
// acknowledgement and physical patch intervals. Those lifetimes can overlap;
// adding their walls would double-count work. The publication residual includes
// uninstrumented transitions and waits inside that interval; it does not
// identify the acknowledgement's default reindex wait. It is not a phase
// measurement or a claim of complete phase accounting.
type EditTimingWindow struct {
	Clock                         string               `json:"clock"`
	Coverage                      string               `json:"coverage"`
	AcknowledgementWallMS         *float64             `json:"ack_wall_ms"`
	PatchWallMS                   *float64             `json:"patch_wall_ms"`
	PatchAccounting               *EditPatchAccounting `json:"patch_accounting,omitempty"`
	PublicationWallMS             *float64             `json:"publication_wall_ms"`
	PublicationUnattributedWallMS *float64             `json:"publication_unattributed_ms"`
}

var editAckPhaseNames = [...]string{
	"dispatcher_queue", "middleware", "parse", "disk_write", "publish", "reindex_wait", "edit_control", "caller_result",
}

var editPatchPhaseNames = [...]string{
	"debounce", "cohort_admission", "batch_admission", "lane_wait", "gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived",
}

// EditTimings uses nanosecond offsets from the same-process monotonic clock,
// rather than the rounded milliseconds or wall-clock strings in the snapshot.
// It does not interpret cycle_started→admitted as pure lane wait, nor split
// structural apply into overlapping nested spans. Those require explicit
// instrumentation at their actual boundaries.
func (s PublicationPhaseSnapshot) EditTimings() EditPhaseTimings {
	out := EditPhaseTimings{
		Milliseconds: make(map[string]*float64, len(editAckPhaseNames)+len(editPatchPhaseNames)),
		States:       make(map[string]string, len(editAckPhaseNames)+len(editPatchPhaseNames)),
		Window:       EditTimingWindow{Clock: "unavailable", Coverage: "partial"},
	}
	for _, name := range editAckPhaseNames {
		out.Milliseconds[name] = nil
		out.States[name] = "not_observed"
		if s.Source == PublicationSourceFreshRequest || s.Source == PublicationSourceBackgroundCompaction {
			out.States[name] = "not_applicable"
		}
	}
	for _, name := range editPatchPhaseNames {
		out.Milliseconds[name] = nil
		out.States[name] = "not_observed"
	}
	if s.Clock != "monotonic" {
		return out
	}
	out.Window.Clock = s.Clock
	if s.attemptWindow != nil {
		// P is independent of receipt/ACK lifetime and may finish later.
		ms := float64(s.attemptWindow.endNS-s.attemptWindow.startNS) / 1e6
		out.Window.PatchWallMS = &ms
		out.Window.PatchAccounting = s.attemptWindow.projectAccounting()
	}
	offsets := make(map[PublicationPhase]int64, len(s.Phases))
	for _, phase := range s.Phases {
		offsets[phase.Phase] = phase.OffsetNS
	}
	if s.Source == PublicationSourceMCPEdit {
		out.Milliseconds["disk_write"], out.States["disk_write"] = editPhaseInterval(
			offsets, PublicationDiskWriteStarted, PublicationReceiptCommitted, s.Terminal)
		out.Milliseconds["publish"], out.States["publish"] = editPhaseInterval(
			offsets, PublicationReceiptCommitted, PublicationTicketEnqueued, s.Terminal)
	}
	out.Milliseconds["derived"], out.States["derived"] = editDerivedInterval(offsets, s.Terminal)
	if s.patchLaps != nil {
		s.patchLaps.project(offsets, s.Terminal, &out)
	}
	if s.admissions != nil {
		s.admissions.project(offsets, s.Terminal, s.laneWait, &out)
	}
	if s.debounce != nil {
		s.debounce.project(offsets, s.Terminal, &out)
	}
	if s.laneWait != nil {
		s.laneWait.project(offsets, s.Terminal, &out)
	}
	terminal := PublicationTicketCompleted
	if _, failed := offsets[PublicationTicketFailed]; failed {
		terminal = PublicationTicketFailed
	}
	if !s.Terminal {
		return out
	}
	wall, state := editPhaseInterval(offsets, PublicationReceived, terminal, true)
	if state != "measured" {
		return out
	}
	out.Window.PublicationWallMS = wall
	// Only subtract disjoint, wholly contained measured intervals. An invalid
	// order leaves the residual unknown; never clamp it to a plausible zero.
	var measured float64
	for _, name := range []string{"disk_write", "publish", "debounce", "cohort_admission", "batch_admission", "lane_wait", "gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived"} {
		if out.States[name] == "invalid_order" || out.States[name] == "pending" {
			return out
		}
		if ms := out.Milliseconds[name]; ms != nil {
			measured += *ms
		}
	}
	if enqueued, ok := offsets[PublicationTicketEnqueued]; ok && enqueued > offsets[terminal] {
		return out
	}
	if committed, ok := offsets[PublicationReceiptCommitted]; ok && committed > offsets[terminal] {
		return out
	}
	if measured <= *wall {
		residual := *wall - measured
		out.Window.PublicationUnattributedWallMS = &residual
	}
	return out
}

// Only an observed derived call inside the accepted canonical executor can
// populate this lap. A pair without its ownership boundaries remains unknown;
// contradictory order is refused, not clamped or charged to an earlier receipt.
func editDerivedInterval(offsets map[PublicationPhase]int64, terminal bool) (*float64, string) {
	ms, state := editPhaseInterval(offsets, PublicationPatchDerivedStarted, PublicationPatchDerivedFinished, terminal)
	if state != "measured" {
		return ms, state
	}
	var previous int64
	missing := false
	for _, phase := range []PublicationPhase{PublicationTicketEnqueued, PublicationPatchExecutionStarted,
		PublicationPatchDerivedStarted, PublicationPatchDerivedFinished, PublicationPatchExecutionFinished} {
		offset, ok := offsets[phase]
		if !ok {
			missing = true
			continue
		}
		if offset < previous {
			return nil, "invalid_order"
		}
		previous = offset
	}
	if terminal {
		end := PublicationTicketCompleted
		if _, failed := offsets[PublicationTicketFailed]; failed {
			end = PublicationTicketFailed
		}
		if offset, ok := offsets[end]; ok && previous > offset {
			return nil, "invalid_order"
		}
	}
	if missing {
		return nil, "not_observed"
	}
	return ms, state
}

func editPhaseInterval(offsets map[PublicationPhase]int64, start, end PublicationPhase, terminal bool) (*float64, string) {
	begin, hasStart := offsets[start]
	finish, hasEnd := offsets[end]
	if hasStart && begin < 0 || hasEnd && finish < 0 || hasStart && hasEnd && finish < begin {
		return nil, "invalid_order"
	}
	if !hasStart {
		return nil, "not_observed"
	}
	if !hasEnd {
		if !terminal {
			return nil, "pending"
		}
		return nil, "not_observed"
	}
	ms := float64(finish-begin) / 1e6
	return &ms, "measured"
}
