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
		return unavailable, errors.New("accepted owner/bytes/request/window relation unavailable")
	}
	causes, _ := r["reader_causes"].([]any)
	if len(causes) == 0 || len(causes) > 8 {
		return unavailable, errors.New("bounded reader admissions unavailable")
	}
	seen := map[uint64]bool{}
	for _, value := range causes {
		cause, _ := value.(map[string]any)
		generation, err := strconvEditProbeUint(cause["admission_generation"])
		session, _ := cause["session"].(string)
		key, _ := cause["request_id_key"].(string)
		if err != nil || generation == 0 || generation > owner || seen[generation] || session == "" || key == "" {
			return unavailable, errors.New("invalid reader admission identity")
		}
		seen[generation] = true
	}
	a, _ := r["phase_accounting"].(map[string]any)
	phaseOwner, phaseOwnerErr := strconvEditProbeUint(a["owner_generation"])
	phaseWall, phaseWallErr := strconvEditProbeUint(a["window_ns"])
	if phaseOwnerErr != nil || phaseOwner != owner || phaseWallErr != nil || phaseWall != wall ||
		a["scope"] != "read_assisted_reconcile" || a["clock"] != "monotonic" || a["sum_state"] != "partial" ||
		a["observed_partition_complete"] != true || a["additive_with_requested_background"] != false {
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
	window, err := checkEditProbeWindowNS(int64(wall), phases)
	subtotal, subtotalErr := strconvEditProbeUint(a["observed_subtotal_ns"])
	residual, residualErr := strconvEditProbeUint(a["unattributed_ns"])
	if err != nil || window.ResidualNS != 0 || subtotalErr != nil || subtotal != uint64(window.SumNS) || residualErr != nil || residual != 0 {
		return unavailable, errors.New("R scalar partition disagrees with its own window")
	}
	return window, nil
}
