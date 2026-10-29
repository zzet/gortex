package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/big"
	"strings"
)

type editProbeOracle struct {
	name string
	call request
}

func editProbeOracles(c Corpus, step editProbeStep) []editProbeOracle {
	marker := map[string]any{"symbol": c.File + "::" + step.Marker}
	file := map[string]any{"file": c.File}
	return []editProbeOracle{
		{"source", request{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": marker}}},
		{"summary", request{Tool: "read", Operation: "summary", Arguments: map[string]any{"operation": "summary", "target": file}}},
		{"editing_context", request{Tool: "read", Operation: "editing_context", Arguments: map[string]any{"operation": "editing_context", "target": file}}},
		{"search_visible", request{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": step.Marker}}},
		{"text", request{Tool: "search", Operation: "text", Arguments: map[string]any{"operation": "text", "query": step.Marker}}},
		{"callers", request{Tool: "relations", Operation: "callers", Arguments: map[string]any{"operation": "callers", "target": map[string]any{"symbol": c.Symbol}, "options": map[string]any{"depth": 1, "limit": 1000}}}},
		{"call_chain", request{Tool: "trace", Operation: "call_chain", Arguments: map[string]any{"operation": "call_chain", "target": marker, "options": map[string]any{"depth": 1, "limit": 1000}}}},
	}
}

func decodeEditProbeResult(raw json.RawMessage) (map[string]any, error) {
	payload, err := textPayload(raw, "content")
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func editProbeSourceCall(source, marker, callee string) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "probe.go", source, 0)
	if err != nil {
		file, err = parser.ParseFile(token.NewFileSet(), "probe.go", "package probe\n"+source, 0)
	}
	if err != nil {
		return false
	}
	found := false
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == marker && fn.Body != nil {
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if target, ok := call.Fun.(*ast.Ident); ok && target.Name == callee {
						found = true
					}
				}
				return true
			})
		}
	}
	return found
}

func editProbeSourceID(id, file, name string) bool {
	return !strings.HasPrefix(id, "ext::") && (id == file+"::"+name || strings.HasSuffix(id, "/"+file+"::"+name))
}

func editProbeCorrect(oracle string, body map[string]any, c Corpus, step editProbeStep, callee string) bool {
	// Collections are producer-specific and top-level. Nested guidance/examples
	// must never become proof that the requested graph fact was returned.
	collection := func(key string, accept func(map[string]any) bool) bool {
		rows, _ := body[key].([]any)
		for _, value := range rows {
			if row, ok := value.(map[string]any); ok && accept(row) {
				return true
			}
		}
		return false
	}
	switch oracle {
	case "source":
		source, _ := body["source"].(string)
		return body["name"] == step.Marker && editProbeSourceCall(source, step.Marker, callee)
	case "file":
		source, _ := body["content"].(string)
		return editProbeSourceCall(source, step.Marker, callee)
	case "text":
		return collection("matches", func(row map[string]any) bool {
			path, _ := row["path"].(string)
			text, _ := row["text"].(string)
			line, err := strconvEditProbeUint(row["line"])
			return err == nil && line > 0 && (path == c.File || strings.HasSuffix(path, "/"+c.File)) && strings.Contains(text, "func "+step.Marker)
		})
	case "callers", "call_chain":
		return collection("edges", func(row map[string]any) bool {
			from, _ := row["from"].(string)
			to, _ := row["to"].(string)
			return row["kind"] == "calls" && editProbeSourceID(from, c.File, step.Marker) && editProbeSourceID(to, c.File, callee)
		})
	case "summary", "editing_context", "search_visible":
		key := map[string]string{"summary": "nodes", "editing_context": "defines", "search_visible": "results"}[oracle]
		return collection(key, func(row map[string]any) bool {
			id, _ := row["id"].(string)
			return editProbeSourceID(id, c.File, step.Marker)
		})
	default:
		return false
	}
}

func editProbeClaimsFresh(body map[string]any) bool {
	claims := func(row map[string]any) bool {
		if row["graph_freshness"] == "fresh" || row["freshness_state"] == "fresh" {
			return true
		}
		return row["state"] == "fresh" || row["fresh"] == true
	}
	freshness, _ := body["freshness"].(map[string]any)
	return body["freshness"] == "fresh" || claims(body) || claims(freshness)
}

func strconvEditProbeUint(value any) (uint64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("missing integer identity")
	}
	var n uint64
	if _, err := fmt.Sscanf(number.String(), "%d", &n); err != nil {
		return 0, err
	}
	if fmt.Sprint(n) != number.String() {
		return 0, errors.New("noncanonical integer identity")
	}
	return n, nil
}

func editProbeNS(value any) (int64, error) {
	if value == nil {
		return 0, errors.New("missing duration")
	}
	ms, ok := new(big.Rat).SetString(fmt.Sprint(value))
	if !ok || ms.Sign() < 0 {
		return 0, errors.New("invalid nonnegative duration")
	}
	ns := new(big.Rat).Mul(ms, big.NewRat(1000000, 1))
	if !ns.IsInt() || !ns.Num().IsInt64() {
		return 0, errors.New("nonintegral/out-of-range nanoseconds")
	}
	return ns.Num().Int64(), nil
}

type editProbeWindow struct {
	WallNS            int64 `json:"wall_ns"`
	SumNS             int64 `json:"sum_ns"`
	ResidualNS        int64 `json:"residual_ns"`
	WithinFivePercent bool  `json:"within_five_percent"`
}

func checkEditProbeWindow(wall any, phases map[string]any) (editProbeWindow, error) {
	ns, err := editProbeNS(wall)
	if err != nil {
		return editProbeWindow{WallNS: ns}, err
	}
	return checkEditProbeWindowNS(ns, phases)
}

func checkEditProbeWindowNS(ns int64, phases map[string]any) (editProbeWindow, error) {
	out := editProbeWindow{WallNS: ns}
	if ns < 0 {
		return out, errors.New("negative window")
	}
	if len(phases) == 0 {
		return out, errors.New("no observed phase values")
	}
	for name, value := range phases {
		if value == nil {
			continue
		}
		n, err := editProbeNS(value)
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		if out.SumNS > math.MaxInt64-n {
			return out, errors.New("phase subtotal overflow")
		}
		out.SumNS += n
	}
	if out.SumNS > out.WallNS {
		return out, errors.New("phase subtotal exceeds its wall")
	}
	out.ResidualNS = out.WallNS - out.SumNS
	out.WithinFivePercent = out.ResidualNS <= out.WallNS/20
	if !out.WithinFivePercent {
		return out, errors.New("accounting residual exceeds five percent")
	}
	return out, nil
}

type editProbeAccounting struct {
	State     string           `json:"state"`
	RPCID     string           `json:"bound_rpc_id"`
	Requested uint64           `json:"requested_generation"`
	Applied   uint64           `json:"applied_generation"`
	A         *editProbeWindow `json:"acknowledgement"`
	P         *editProbeWindow `json:"physical"`
	B         *editProbeWindow `json:"requested_background"`
	R         *editProbeWindow `json:"read_assisted_publisher,omitempty"`
	Reasons   []string         `json:"reasons"`
}

func checkEditProbeAccounting(status map[string]any, receipt string) editProbeAccounting {
	out := editProbeAccounting{State: "unavailable"}
	fail := func(reason string) { out.Reasons = append(out.Reasons, reason) }
	if status["receipt"] != receipt {
		fail("receipt identity mismatch")
	}
	requested, err := strconvEditProbeUint(status["reindex_generation"])
	if err != nil || requested == 0 {
		fail("requested generation unavailable")
	}
	out.Requested = requested
	applied, err := strconvEditProbeUint(status["applied_generation"])
	if err != nil || applied < requested {
		fail("applied generation unavailable")
	}
	out.Applied = applied
	ack, _ := status["acknowledgement"].(map[string]any)
	out.RPCID, _ = ack["rpc_id"].(string)
	if out.RPCID == "" || ack["disjoint_observed_intervals"] != true {
		fail("bound disjoint A identity unavailable")
	}
	aPhases, _ := ack["observed_execution_ms"].(map[string]any)
	aStates, _ := ack["phase_states"].(map[string]any)
	for _, name := range []string{"caller_result", "disk_write", "edit_control", "middleware", "parse", "publish", "reindex_wait", "reply_prepare"} {
		if aPhases[name] == nil || aStates[name] != "measured" {
			fail("missing applicable A phase " + name)
		}
	}
	a, e := checkEditProbeWindow(ack["caller_response_ready_ms"], aPhases)
	if e != nil {
		fail("A: " + e.Error())
	} else {
		out.A = &a
	}
	original, _ := status["phase_timing"].(map[string]any)
	publication, _ := status["publication_phases"].(map[string]any)
	ownerKnown := status["receipt"] == receipt && requested > 0 && applied >= requested
	physical := original
	if applied > requested {
		if original["patch_wall_ms"] != nil || original["patch_accounting"] != nil {
			fail("coalesced receipt borrowed accepted P")
		}
		winner, _ := status["coalesced_publication"].(map[string]any)
		wReq, _ := strconvEditProbeUint(winner["requested_generation"])
		wApplied, _ := strconvEditProbeUint(winner["applied_generation"])
		pub, _ := winner["publication_phases"].(map[string]any)
		ticket, _ := strconvEditProbeUint(pub["ticket"])
		originalPublication, _ := status["publication_phases"].(map[string]any)
		if wReq != requested || wApplied != applied || ticket != applied || winner["scope"] != "independent_physical_winner_serving_coalesced_receipt" || pub["source"] != "canonical_coalesced_attempt" || pub["key"] == originalPublication["key"] {
			ownerKnown = false
			fail("independent serving identity mismatch")
		}
		publication = pub
		timings, _ := winner["timings"].(map[string]any)
		physical, _ = timings["phase_timing"].(map[string]any)
	}
	pAccounting, _ := physical["patch_accounting"].(map[string]any)
	pTicket, _ := strconvEditProbeUint(pAccounting["attempt_ticket"])
	pRetry, retryErr := strconvEditProbeUint(pAccounting["attempt_retry"])
	if pAccounting["observed_partition_complete"] != true || pTicket != applied || retryErr != nil || pRetry != 0 {
		fail("applicable physical P unavailable")
	}
	pPhases, _ := pAccounting["phases_ms"].(map[string]any)
	pStates, _ := pAccounting["phase_states"].(map[string]any)
	duplicate := pAccounting["execution_disposition"] == "native_mtime_duplicate" && ownerKnown && editProbeImmutablePartialPhysical(physical, publication, applied)
	if repair, present := status["read_assisted_publication"]; present {
		r, repairErr := checkEditProbeReadRepair(repair, status, receipt, requested)
		if repairErr != nil {
			fail("R: " + repairErr.Error())
		} else {
			out.R = &r
		}
	}
	if duplicate && out.R == nil {
		fail("actual read-assisted publisher deep coverage unavailable")
	}
	for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "attempt_setup", "patch_inspection", "executor_control", "attempt_finalize", "cohort_admission", "lane_wait"} {
		if duplicate && out.R != nil && (name == "executor_control" || isEditProbeRepairDeepPhase(name)) {
			continue // explicitly unexecuted in P; validated independently in R
		}
		if pPhases[name] == nil || pStates[name] != "measured" {
			fail("missing applicable P phase " + name)
		}
	}
	p, e := checkEditProbeWindow(physical["patch_wall_ms"], pPhases)
	if e != nil {
		fail("P: " + e.Error())
	} else {
		out.P = &p
	}
	background, _ := status["requested_background"].(map[string]any)
	bReq, _ := strconvEditProbeUint(background["requested_generation"])
	bApplied, _ := strconvEditProbeUint(background["applied_generation"])
	if background["observed_partition_complete"] != true || background["scope"] != "accepted_scheduling_call_entry_to_serving_attempt_return" || background["coverage"] != "one_canceled_timer_one_native_successor" || background["sum_state"] != "partial" || bReq != requested || bApplied != applied {
		fail("requested B unavailable for actual topology/identity")
	}
	bPhases, _ := background["phases_ms"].(map[string]any)
	if _, duplicate := bPhases["debounce"]; duplicate {
		fail("whole debounce double-counted in B")
	}
	for _, name := range requestedBackgroundLapNamesForProbe {
		if bPhases[name] == nil {
			fail("missing B phase " + name)
		}
	}
	for name, value := range pPhases {
		if value != nil && fmt.Sprint(value) != fmt.Sprint(bPhases[name]) {
			fail("B/P phase mismatch: " + name)
		}
	}
	b, e := checkEditProbeWindow(background["background_wall_ms"], bPhases)
	if e != nil {
		fail("B: " + e.Error())
	} else {
		out.B = &b
	}
	if len(out.Reasons) == 0 {
		out.State = "observed_single_window_checks_pass"
	} else if ownerKnown && editProbeImmutablePartialPhysical(physical, publication, applied) {
		out.State = "terminal_partial_physical"
	}
	return out
}

// Receipt terminality can precede the actual serving return. Only the immutable
// late window proves that missing deep laps cannot arrive. A complete P with
// absent B must still wait: background publication follows physical publication.
// An explicit native duplicate is terminal failed deep coverage even when its
// own smaller no-executor P partition is complete.
func editProbeImmutablePartialPhysical(physical, publication map[string]any, applied uint64) bool {
	if _, err := editProbeNS(physical["patch_wall_ms"]); err != nil || physical["clock"] != "monotonic" || publication["clock"] != "monotonic" || publication["terminal"] != true {
		return false
	}
	key, _ := publication["key"].(string)
	ticket, err := strconvEditProbeUint(publication["ticket"])
	if key == "" || err != nil || ticket != applied || (publication["source"] != "mcp_edit" && publication["source"] != "canonical_coalesced_attempt") {
		return false
	}
	accounting, _ := physical["patch_accounting"].(map[string]any)
	owner, err := strconvEditProbeUint(accounting["attempt_ticket"])
	retry, retryErr := strconvEditProbeUint(accounting["attempt_retry"])
	if err != nil || owner != applied || retryErr != nil || accounting["scope"] != "accepted_winning_physical_attempt" {
		return false
	}
	if accounting["observed_partition_complete"] == false {
		return accounting["sum_state"] == "missing_laps" || accounting["sum_state"] == "invalid_order"
	}
	if accounting["observed_partition_complete"] != true || accounting["execution_disposition"] != "native_mtime_duplicate" || accounting["sum_state"] != "partial" || retry != 0 {
		return false
	}
	phases, _ := accounting["phases_ms"].(map[string]any)
	states, _ := accounting["phase_states"].(map[string]any)
	for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis", "executor_control"} {
		if phases[name] != nil || states[name] != "not_applicable" {
			return false
		}
	}
	for _, name := range []string{"attempt_setup", "patch_inspection", "attempt_finalize", "cohort_admission", "lane_wait"} {
		if phases[name] == nil || states[name] != "measured" {
			return false
		}
	}
	window, err := checkEditProbeWindow(physical["patch_wall_ms"], phases)
	return err == nil && window.ResidualNS == 0
}

func editProbeFinished(graphTerminal, allCorrect, requireAccounting bool, accounting editProbeAccounting) bool {
	return graphTerminal && allCorrect && (!requireAccounting || accounting.State == "observed_single_window_checks_pass" || accounting.State == "terminal_partial_physical")
}

var requestedBackgroundLapNamesForProbe = []string{"scheduling_admission_setup", "initial_timer_arm", "canceled_quiet_wait", "timer_stop", "coalescing_control", "successor_timer_arm", "successor_quiet_wait"}
