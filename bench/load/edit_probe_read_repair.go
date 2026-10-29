package main

import (
	"encoding/hex"
	"errors"
	"math"
)

var editProbeRepairDeepPhases = [...]string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived"}

func isEditProbeRepairDeepPhase(name string) bool {
	for _, phase := range editProbeRepairDeepPhases {
		if name == phase {
			return true
		}
	}
	return false
}

// R is checked against its own accepted owner/bytes/window. It is never added
// to B or used to replace P's null deep values. Wire/session fields are bounded
// admission causes, not proof of a uniquely identified reader RPC.
func checkEditProbeReadRepair(raw any, status map[string]any, receipt string, requested uint64) (editProbeWindow, error) {
	unavailable := editProbeWindow{}
	r, owner, wall, err := editProbeReadRepairIdentity(raw, status, receipt, requested)
	if err != nil {
		return unavailable, err
	}
	a, _ := r["phase_accounting"].(map[string]any)
	if !editProbeReadRepairPhaseOwner(a, owner, wall) || a["sum_state"] != "partial" || a["observed_partition_complete"] != true {
		return unavailable, errors.New("own-window phase evidence unavailable")
	}
	phases, _ := a["phases_ms"].(map[string]any)
	states, _ := a["phase_states"].(map[string]any)
	for _, name := range append(editProbeRepairDeepPhases[:], "repair_control") {
		if phases[name] == nil || states[name] != "measured" {
			return unavailable, errors.New("missing applicable publisher phase " + name)
		}
	}
	for name, value := range phases {
		if value == nil && states[name] == "measured" {
			return unavailable, errors.New("missing measured R value " + name)
		}
		if value != nil && (!isEditProbeRepairDeepPhase(name) && name != "repair_control" && name != "patch_analysis" || states[name] != "measured") {
			return unavailable, errors.New("unexpected/unmeasured R phase " + name)
		}
	}
	window, err := checkEditProbeWindowNS(wall, phases)
	subtotal, subtotalErr := strconvEditProbeUint(a["observed_subtotal_ns"])
	residual, residualErr := strconvEditProbeUint(a["unattributed_ns"])
	if err != nil || window.ResidualNS != 0 || subtotalErr != nil || subtotal != uint64(window.SumNS) || residualErr != nil || residual != 0 {
		return unavailable, errors.New("read-repair scalar partition disagrees with its own window")
	}
	return window, nil
}

func editProbeReadRepairIdentity(raw any, status map[string]any, receipt string, requested uint64) (map[string]any, uint64, int64, error) {
	r, _ := raw.(map[string]any)
	owner, ownerErr := strconvEditProbeUint(r["owner_generation"])
	req, reqErr := strconvEditProbeUint(r["requested_generation"])
	wall, wallErr := strconvEditProbeUint(r["window_ns"])
	blob, _ := r["accepted_blob_sha"].(string)
	_, blobErr := hex.DecodeString(blob)
	if ownerErr != nil || owner == 0 || reqErr != nil || req != requested || r["requested_receipt"] != receipt ||
		wallErr != nil || wall > math.MaxInt64 || len(blob) != 40 || blobErr != nil || blob != status["new_sha"] ||
		r["scope"] != "read_assisted_reconcile" || r["clock"] != "monotonic" ||
		r["additive_with_requested_background"] != false || r["background_relationship"] != "unavailable" {
		return nil, 0, 0, errors.New("accepted owner/bytes/request/window relation unavailable")
	}
	causes, _ := r["reader_causes"].([]any)
	if len(causes) == 0 || len(causes) > 8 {
		return nil, 0, 0, errors.New("bounded reader admissions unavailable")
	}
	seen := map[uint64]bool{}
	for _, value := range causes {
		cause, _ := value.(map[string]any)
		generation, err := strconvEditProbeUint(cause["admission_generation"])
		session, _ := cause["session"].(string)
		key, _ := cause["request_id_key"].(string)
		if err != nil || generation == 0 || generation > owner || seen[generation] || session == "" || key == "" {
			return nil, 0, 0, errors.New("invalid reader admission identity")
		}
		seen[generation] = true
	}
	return r, owner, int64(wall), nil
}

func editProbeReadRepairPhaseOwner(a map[string]any, owner uint64, wall int64) bool {
	phaseOwner, phaseOwnerErr := strconvEditProbeUint(a["owner_generation"])
	phaseWall, phaseWallErr := strconvEditProbeUint(a["window_ns"])
	return phaseOwnerErr == nil && phaseOwner == owner && phaseWallErr == nil && phaseWall == uint64(wall) &&
		a["scope"] == "read_assisted_reconcile" && a["clock"] == "monotonic" && a["additive_with_requested_background"] == false
}

// Unlike absent late R/B, an owned frozen R phase failure cannot gain laps.
func editProbeImmutablePartialReadRepair(raw any, status map[string]any, receipt string, requested uint64) bool {
	r, owner, wall, err := editProbeReadRepairIdentity(raw, status, receipt, requested)
	if err != nil {
		return false
	}
	a, _ := r["phase_accounting"].(map[string]any)
	return editProbeReadRepairPhaseOwner(a, owner, wall) && a["observed_partition_complete"] == false &&
		(a["sum_state"] == "missing_laps" || a["sum_state"] == "invalid_order" || a["sum_state"] == "unclassified_gap")
}
