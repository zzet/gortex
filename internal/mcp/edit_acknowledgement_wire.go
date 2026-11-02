package mcp

import (
	"strconv"

	"github.com/zzet/gortex/internal/indexer"
)

type acknowledgementLapCount struct {
	Started       uint64 `json:"started"`
	Completed     uint64 `json:"completed"`
	NotApplicable uint64 `json:"not_applicable"`
}

// Acknowledgement and patch walls have different owners and can overlap. The
// shared RPC identity lets a consumer deduplicate several physical file
// receipts; this object never claims its subtotal is a complete critical path.
type acknowledgementPayload struct {
	State                    string                             `json:"state"`
	RPCID                    *string                            `json:"rpc_id"`
	Scope                    string                             `json:"scope"`
	Denominator              string                             `json:"denominator"`
	Clock                    string                             `json:"clock"`
	ReplyWallMS              *float64                           `json:"caller_response_ready_ms"`
	ReplyState               string                             `json:"caller_response_state"`
	ReplyOutcome             string                             `json:"caller_response_outcome,omitempty"`
	ActualWallMS             *float64                           `json:"actual_handler_ms"`
	ActualState              string                             `json:"actual_handler_state"`
	ActualFinishedLate       bool                               `json:"actual_finished_after_reply"`
	PhasesMS                 map[string]*float64                `json:"phases_ms"`
	PhaseStates              map[string]string                  `json:"phase_states"`
	Counts                   map[string]acknowledgementLapCount `json:"counts"`
	ObservedExecutionMS      map[string]*float64                `json:"observed_execution_ms"`
	Disjoint                 bool                               `json:"disjoint_observed_intervals"`
	SubtotalMS               *float64                           `json:"observed_ack_subtotal_ms"`
	UnattributedMS           *float64                           `json:"ack_unattributed_ms"`
	SumState                 string                             `json:"sum_state"`
	CombinedAckPatchSumState string                             `json:"combined_ack_patch_sum_state"`
	ExistingPhaseScope       string                             `json:"existing_ack_phase_scope"`
	PublicationResidualState string                             `json:"publication_residual_state"`
	DispatcherScope          string                             `json:"dispatcher_scope"`
	DispatcherReplyMS        *float64                           `json:"dispatcher_to_caller_response_ready_ms"`
	DispatcherReplyState     string                             `json:"dispatcher_response_state"`
	ParserScope              string                             `json:"parse_scope"`
}

func buildAcknowledgementPayload(snapshot *editAcknowledgementSnapshot, state string) acknowledgementPayload {
	out := acknowledgementPayload{
		State: state, Scope: "shared_rpc_mcp_bound_call", Clock: "unavailable",
		Denominator: "bound_tool_entry_to_outer_return_before_transport_serialization",
		ReplyState:  state, ActualState: state, SumState: "unavailable",
		DispatcherScope: "queue_and_preamble_precede_bound_call_subtotal", DispatcherReplyState: state, ParserScope: "unobserved",
		CombinedAckPatchSumState: "not_additive_overlapping_lifetimes",
		ExistingPhaseScope:       "legacy_per_receipt_publication", PublicationResidualState: "legacy_per_receipt_publication",
		PhasesMS: make(map[string]*float64, 11), PhaseStates: make(map[string]string, 11),
		Counts: make(map[string]acknowledgementLapCount, 8), ObservedExecutionMS: make(map[string]*float64, 8),
	}
	for _, name := range []string{"dispatcher_queue", "dispatcher_preamble", "middleware", "parse", "disk_write", "publish", "reindex_wait", "edit_control", "reply_prepare", "caller_result", "transport"} {
		out.PhasesMS[name], out.PhaseStates[name] = nil, state
	}
	if snapshot == nil {
		return out
	}
	out.State = "observed"
	id := "rpc-" + strconv.FormatUint(snapshot.RPCID, 10)
	out.RPCID, out.Clock = &id, "monotonic"
	out.ReplyWallMS, out.ReplyState, out.ReplyOutcome = snapshot.ReplyWallMS, snapshot.ReplyState, snapshot.ReplyOutcome
	out.ActualWallMS, out.ActualState = snapshot.ActualWallMS, snapshot.ActualState
	out.ActualFinishedLate, out.Disjoint = snapshot.ActualFinishedLate, snapshot.LapsDisjoint
	out.DispatcherReplyMS, out.DispatcherReplyState = snapshot.DispatcherReplyMS, snapshot.DispatcherReplyState
	if out.DispatcherReplyState == "" {
		out.DispatcherReplyState = "unavailable_dispatcher_interface"
	}
	if snapshot.ParserScope != "" {
		out.ParserScope = snapshot.ParserScope
	}
	out.ExistingPhaseScope, out.PublicationResidualState = "shared_rpc", "unavailable_shared_rpc_scope"
	for _, name := range []string{"dispatcher_queue", "dispatcher_preamble"} {
		out.PhaseStates[name] = "unavailable_dispatcher_interface"
	}
	if snapshot.DispatcherQueueState != "" {
		out.PhasesMS["dispatcher_queue"], out.PhaseStates["dispatcher_queue"] = snapshot.DispatcherQueueMS, snapshot.DispatcherQueueState
	}
	if snapshot.DispatcherState != "" {
		out.PhasesMS["dispatcher_preamble"], out.PhaseStates["dispatcher_preamble"] = snapshot.DispatcherPreambleMS, snapshot.DispatcherState
	}
	out.PhaseStates["transport"] = "unavailable_boundary"
	var subtotal float64
	var subtotalNS int64
	exact := snapshot.hasReplyNS
	observed, invalid := 0, false
	if exact && (snapshot.replyNS < 0 || snapshot.ReplyWallMS == nil || float64(snapshot.replyNS)/1e6 != *snapshot.ReplyWallMS) {
		invalid = true
	}
	for name, lap := range map[string]editAcknowledgementLapSnapshot{
		"middleware": snapshot.Middleware, "reply_prepare": snapshot.ReplyPrepare, "caller_result": snapshot.CallerResult,
		"parse": snapshot.ParseGate, "disk_write": snapshot.DiskWrite,
		"publish": snapshot.TicketAdmission, "reindex_wait": snapshot.ReindexWait, "edit_control": snapshot.EditControl,
	} {
		if lap.State == "" {
			lap.State = "unobserved"
		}
		out.PhasesMS[name], out.PhaseStates[name] = lap.AckMS, lap.State
		out.Counts[name] = acknowledgementLapCount{lap.Started, lap.Completed, lap.NotApplicable}
		out.ObservedExecutionMS[name] = lap.ObservedMS
		if lap.Completed > lap.Started || lap.NotApplicable > lap.Completed ||
			((lap.AckMS != nil || lap.ObservedMS != nil) && (lap.Started == 0 || lap.Completed != lap.Started)) ||
			(lap.AckMS != nil && (lap.ObservedMS == nil || *lap.AckMS != *lap.ObservedMS)) {
			out.PhasesMS[name], out.ObservedExecutionMS[name] = nil, nil
			out.PhaseStates[name], invalid = "invalid_count", true
			continue
		}
		if lap.State == "invalid_interval" || lap.State == "outside_execution_window" {
			invalid = true
		}
		if lap.AckMS != nil {
			if !lap.hasAckNS {
				exact = false // legacy synthetic snapshots have no actual nanosecond evidence
			} else if lap.ackNS < 0 || float64(lap.ackNS)/1e6 != *lap.AckMS || subtotalNS > (1<<63-1)-lap.ackNS {
				invalid = true
			} else {
				subtotalNS += lap.ackNS
			}
			subtotal += *lap.AckMS
			observed++
		}
	}
	if exact {
		// Compare/sum observed integers, not rounded per-lap milliseconds.
		// No tolerance, clamp or unobserved remainder is introduced.
		subtotal = float64(subtotalNS) / 1e6
	}
	switch {
	case !snapshot.LapsDisjoint:
		out.SumState = "invalid_overlap"
	case invalid:
		out.SumState = "invalid_interval"
	case snapshot.ReplyState != "measured" || snapshot.ReplyWallMS == nil:
		out.SumState = "pending_or_unavailable_reply"
	case observed == 0:
		out.SumState = "no_ack_intervals_observed"
	case exact && (subtotalNS < 0 || subtotalNS > snapshot.replyNS) || !exact && (subtotal < 0 || subtotal > *snapshot.ReplyWallMS):
		out.SumState = "invalid_total"
	default:
		unattributed := *snapshot.ReplyWallMS - subtotal
		if exact {
			unattributed = float64(snapshot.replyNS-subtotalNS) / 1e6
		}
		out.SubtotalMS, out.UnattributedMS, out.SumState = &subtotal, &unattributed, "partial"
	}
	return out
}

func attachAcknowledgementPayload(payload map[string]any, snapshot *editAcknowledgementSnapshot, state string) {
	ack := buildAcknowledgementPayload(snapshot, state)
	payload["acknowledgement"] = ack
	if snapshot == nil {
		return // preserve independently valid legacy per-file evidence
	}
	phases, _ := payload["phases_ms"].(map[string]*float64)
	states, _ := payload["phase_states"].(map[string]string)
	for _, name := range []string{"dispatcher_queue", "middleware", "parse", "disk_write", "publish", "reindex_wait", "edit_control", "caller_result"} {
		phases[name], states[name] = ack.PhasesMS[name], ack.PhaseStates[name]
	}
	window := payload["phase_timing"].(indexer.EditTimingWindow)
	window.Clock, window.Coverage = "monotonic", "partial"
	window.AcknowledgementWallMS = ack.ReplyWallMS
	// The shared RPC origin and per-file publication origin are different.
	// Keep the original receipt wall/patch evidence, but do not subtract the
	// replacement shared-RPC laps from its per-file publication interval.
	window.PublicationUnattributedWallMS = nil
	payload["phase_timing"] = window
}
