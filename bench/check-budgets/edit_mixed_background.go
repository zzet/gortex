package main

import (
	"encoding/json"
	"math/big"
	"strings"

	"github.com/zzet/gortex/bench/receiptproof"
)

var mixedPhysicalPhases = []string{"apply", "attempt_finalize", "attempt_setup", "batch_admission", "cohort_admission", "derived", "executor_control", "gate_wait", "lane_wait", "mtimes", "patch_analysis", "patch_inspection", "patch_parse", "resolve", "search_lap"}
var mixedAcknowledgementPhases = []string{"caller_result", "disk_write", "edit_control", "middleware", "parse", "publish", "reindex_wait", "reply_prepare"}

type mixedBackgroundStatus struct {
	Receipt     string            `json:"receipt"`
	Mutation    string            `json:"mutation_id"`
	Requested   uint64            `json:"reindex_generation"`
	Applied     uint64            `json:"applied_generation"`
	Tool        string            `json:"tool"`
	Disk        string            `json:"disk_status"`
	Commit      uint64            `json:"commit_seq"`
	SHA256      string            `json:"content_sha256"`
	Graph       string            `json:"graph_status"`
	Terminal    bool              `json:"graph_status_terminal"`
	Tiers       map[string]string `json:"tier_state"`
	ACKAt       string            `json:"ack_at"`
	FileGraphAt string            `json:"file_graph_at"`
	RelationsAt string            `json:"relations_at"`
	DerivedAt   string            `json:"derived_at"`
	Coalesced   json.RawMessage   `json:"coalesced_publication"`
	Repair      json.RawMessage   `json:"read_assisted_publication"`
	Background  json.RawMessage   `json:"requested_background"`
	ACK         struct {
		RPC      string                  `json:"rpc_id"`
		Clock    string                  `json:"clock"`
		Disjoint bool                    `json:"disjoint_observed_intervals"`
		Wall     json.Number             `json:"caller_response_ready_ms"`
		Phases   map[string]*json.Number `json:"observed_execution_ms"`
		States   map[string]string       `json:"phase_states"`
	} `json:"acknowledgement"`
	Publication struct {
		Key      string `json:"key"`
		Ticket   uint64 `json:"ticket"`
		Source   string `json:"source"`
		Clock    string `json:"clock"`
		Terminal bool   `json:"terminal"`
	} `json:"publication_phases"`
	Refusal struct {
		Key       string  `json:"publication_key"`
		Scope     string  `json:"scope"`
		Reason    string  `json:"reason"`
		Requested uint64  `json:"requested_generation"`
		Last      uint64  `json:"last_observed_generation"`
		Handoffs  *uint64 `json:"observed_native_handoffs"`
	} `json:"requested_background_unavailable"`
	Physical struct {
		Clock      string      `json:"clock"`
		Wall       json.Number `json:"patch_wall_ms"`
		Accounting struct {
			Disposition string                  `json:"execution_disposition"`
			Ticket      uint64                  `json:"attempt_ticket"`
			Retry       *uint64                 `json:"attempt_retry"`
			Scope       string                  `json:"scope"`
			Coverage    string                  `json:"background_coverage"`
			Complete    bool                    `json:"observed_partition_complete"`
			SumState    string                  `json:"sum_state"`
			Phases      map[string]*json.Number `json:"phases_ms"`
			States      map[string]string       `json:"phase_states"`
		} `json:"patch_accounting"`
	} `json:"phase_timing"`
}

// mixedBackgroundRecord produces a separate qualification. It never mutates
// the original diagnostic outcome, measured intervals or historical errors.
func mixedBackgroundRecord(wire editLoopWire, index int, status json.RawMessage) bool {
	if index < 0 || index >= len(wire.Edits) {
		return false
	}
	r := wire.Edits[index]
	reason := "A/P/B accounting unavailable: " + strings.Join(r.Accounting.Reasons, "; ")
	if !sharedFunctionalEditLoopObservations(wire, index) || r.Outcome != "diagnostic_failure" || r.CoalescedFunctional != nil || r.FunctionalUnavailable != nil ||
		r.Receipt == "" || r.Accounting.State != "terminal_unavailable_background" || r.Accounting.Requested == 0 || r.Accounting.Applied != r.Accounting.Requested ||
		!absentEditLoopTiming(r.Accounting.B) || !absentEditLoopTiming(r.Accounting.R) || !absentEditLoopTiming(r.Accounting.Shared) ||
		len(r.Errors) != 1 || r.Errors[0] != reason || !receiptproof.MixedBackgroundTimingOnlyError(reason) {
		return false
	}
	var p receiptproof.Window
	if json.Unmarshal(r.Accounting.P, &p) != nil || !p.Complete() {
		return false
	}
	var s mixedBackgroundStatus
	if json.Unmarshal(status, &s) != nil {
		return false
	}
	pub, bg, physical := s.Publication, s.Refusal, s.Physical.Accounting
	if s.Receipt != r.Receipt || s.Mutation != r.MutationID || s.Requested != r.Accounting.Requested || s.Applied != s.Requested || s.Tool != "edit_file" ||
		s.Disk != "committed" || s.Commit != r.ACKCommitSeq || s.SHA256 != r.ACKContentSHA256 || s.Graph != "fresh" || !s.Terminal ||
		!absentEditLoopTiming(s.Coalesced) || !absentEditLoopTiming(s.Repair) || !absentEditLoopTiming(s.Background) ||
		pub.Key == "" || pub.Ticket != s.Requested || pub.Source != "mcp_edit" || pub.Clock != "monotonic" || !pub.Terminal ||
		bg.Key != pub.Key || bg.Scope != "requested_background_observation" || bg.Reason != "preexisting_path_work" || bg.Requested != s.Requested || bg.Last != s.Requested || bg.Handoffs == nil || *bg.Handoffs != 0 ||
		s.ACK.RPC != r.Accounting.RPCID || s.ACK.RPC == "" || s.ACK.Clock != "monotonic" || !s.ACK.Disjoint ||
		s.Physical.Clock != "monotonic" || physical.Ticket != s.Requested || physical.Retry == nil || *physical.Retry != 0 || physical.Scope != "accepted_winning_physical_attempt" ||
		physical.Coverage != "not_established" || !physical.Complete || physical.SumState != "partial" || physical.Disposition != "not_observed" {
		return false
	}
	if !orderedEditLoopTierStamps(s.ACKAt, s.FileGraphAt, s.RelationsAt, s.DerivedAt) {
		return false
	}
	for _, name := range []string{"file_graph", "relations", "derived"} {
		if s.Tiers[name] != "fresh" {
			return false
		}
	}
	a, ok := mixedMeasuredWindow(s.ACK.Wall, s.ACK.Phases, s.ACK.States, mixedAcknowledgementPhases, false)
	if !ok || a.WallNS != r.Accounting.A.WallNS || a.SumNS != r.Accounting.A.SumNS || a.ResidualNS != r.Accounting.A.ResidualNS {
		return false
	}
	observed, ok := mixedMeasuredWindow(s.Physical.Wall, physical.Phases, physical.States, mixedPhysicalPhases, true)
	if !ok || observed != p {
		return false
	}
	for _, name := range mixedPhysicalPhases {
		if phase, present := r.PhasesMS[name]; present {
			if phase.Value == nil || phase.Unit != "ms" || phase.Reason != "" {
				return false
			}
			encoded, err := json.Marshal(*phase.Value)
			raw, valid := mixedMillisecondsNS(json.Number(encoded))
			actual, actualValid := mixedMillisecondsNS(*physical.Phases[name])
			if err != nil || !valid || !actualValid || raw != actual {
				return false
			}
		}
	}
	return true
}

func mixedMeasuredWindow(wall json.Number, phases map[string]*json.Number, states map[string]string, required []string, exactStates bool) (receiptproof.Window, bool) {
	w := receiptproof.Window{}
	var ok bool
	if w.WallNS, ok = mixedMillisecondsNS(wall); !ok || len(phases) != len(required) || exactStates && len(states) != len(required) {
		return w, false
	}
	for _, name := range required {
		v := phases[name]
		if v == nil || states[name] != "measured" {
			return w, false
		}
		ns, valid := mixedMillisecondsNS(*v)
		if !valid || ns > w.WallNS-w.SumNS {
			return w, false
		}
		w.SumNS += ns
	}
	w.ResidualNS = w.WallNS - w.SumNS
	w.WithinFivePercent = w.ResidualNS <= w.WallNS/20
	return w, w.Complete()
}

// JSON decimal milliseconds are converted exactly; rounding cannot manufacture
// a measured nanosecond or turn an over-limit residual into a passing one.
func mixedMillisecondsNS(value json.Number) (int64, bool) {
	r, ok := new(big.Rat).SetString(string(value))
	if !ok || r.Sign() < 0 {
		return 0, false
	}
	r.Mul(r, big.NewRat(1000000, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	return r.Num().Int64(), true
}
