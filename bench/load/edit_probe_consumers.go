package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"strconv"
	"strings"
	"time"
)

// Consumer observations have their own denominator. In particular, a truthful
// pending verdict is not the marker's first-correct time or graph completion.
type editConsumerObservation struct {
	State           string `json:"state"`
	RequestedMode   string `json:"requested_mode"`
	Attempts        int    `json:"attempts"`
	PostACKAttempts int    `json:"post_ack_attempts"`
	Assessed        int    `json:"post_ack_assessed"`
	Pending         int    `json:"post_ack_pending"`
	SubjectMatches  int    `json:"post_ack_subject_matches"`
	Errors          int    `json:"errors"`
	Unassessed      int    `json:"post_ack_unassessed"`
	SilentStale     Metric `json:"post_ack_silent_stale"`
	stale           int
	Reason          string `json:"reason,omitempty"`
}

type editConsumerBaseline struct {
	State    string `json:"state"`
	Symbol   string `json:"symbol"`
	Evidence string `json:"raw_evidence"`
}

var editConsumerNames = []string{"consumer_rename", "consumer_verify", "consumer_dead_code", "consumer_detect"}

func isEditConsumer(name string) bool { return strings.HasPrefix(name, "consumer_") }

func bindEditConsumerPlan(c Corpus, p *editProbePlan) error {
	if p.Kind != "add_intrafile_call" || p.Tool != "edit.file" || p.Sender != "" || p.Language != "go" || p.LinkedWorktrees != 0 {
		return errors.New("consumer probes require one Go intrafile call-addition fixture")
	}
	if p.Callee == "" || ast.IsExported(p.Callee) {
		return errors.New("consumer probes require an eligible unexported dead-code target")
	}
	if c.Symbol != c.File+"::"+p.Callee {
		return errors.New("consumer probes require the exact intrafile callee identity")
	}
	if c.ProposedSignature != fmt.Sprintf("func %s(probe int) int", p.Callee) {
		return errors.New("consumer probes require the explicit proposed signature func <callee>(probe int) int")
	}
	p.ConsumerProbes = true
	for i := range p.Steps {
		p.Steps[i].ConsumerProbes = true
	}
	p.NotSelected = append(p.NotSelected,
		"consumer probes require actual pre-edit unexported dead-code membership; no empty-list baseline",
		"consumer pending/error/missing/unsupported observations are separate from the seven visibility oracles; compact consumer decoding is unsupported, not JSON fallback coverage")
	return nil
}

func editConsumerOracles(c Corpus) []editProbeOracle {
	_, callee, _ := strings.Cut(c.Symbol, "::")
	return []editProbeOracle{
		{"consumer_rename", request{Tool: "refactor", Operation: "rename", Arguments: map[string]any{"operation": "rename", "target": map[string]any{"symbol": c.Symbol}, "new_name": callee + "Renamed", "dry_run": true}}},
		{"consumer_verify", request{Tool: "change", Operation: "verify", Arguments: map[string]any{"operation": "verify", "source": map[string]any{"changes": []map[string]any{{"symbol_id": c.Symbol, "new_signature": c.ProposedSignature}}}}}},
		{"consumer_dead_code", editConsumerDeadCodeRequest()},
		{"consumer_detect", request{Tool: "change", Operation: "detect", Arguments: map[string]any{"operation": "detect", "source": map[string]any{"scope": "unstaged"}}}},
	}
}

func editConsumerDeadCodeRequest() request {
	return request{Tool: "analyze", Operation: "dead_code", Arguments: map[string]any{"kind": "dead_code", "options": map[string]any{"limit": 1000}}}
}

func editConsumerUint(value any) (uint64, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	x, err := strconv.ParseUint(n.String(), 10, 64)
	return x, err == nil
}

func editConsumerRows(body map[string]any, key string) ([]any, bool) {
	v, exists := body[key]
	if !exists {
		return nil, false
	}
	if v == nil {
		return nil, true // producer's nil slice is an explicit empty collection
	}
	rows, ok := v.([]any)
	if !ok {
		return nil, false
	}
	for _, row := range rows {
		if _, ok := row.(map[string]any); !ok {
			return nil, false
		}
	}
	return rows, true
}

func editConsumerPayloadValid(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	if _, exists := body["error"]; exists {
		return false
	}
	if value, exists := body["errors"]; exists && value != nil {
		rows, ok := value.([]any)
		if !ok || len(rows) != 0 {
			return false
		}
	}
	for _, key := range []string{"truncated", "_truncated_by_budget"} {
		if value, exists := body[key]; exists && value != false {
			return false
		}
	}
	return true
}

func editConsumerSymbol(row map[string]any, key, file, name string) bool {
	id, _ := row[key].(string)
	path, _ := row["file_path"].(string)
	return editProbeSourceID(id, file, name) && editProbeFixturePath(path, file)
}

func editConsumerSubject(oracle string, body map[string]any, c Corpus, step editProbeStep, callee string) bool {
	if !editConsumerPayloadValid(body) {
		return false
	}
	switch oracle {
	case "consumer_verify":
		return editConsumerVerify(body, c.File, step.Marker)
	case "consumer_detect":
		return editConsumerDetect(body, c.File, step.Marker)
	case "consumer_rename":
		return editConsumerRename(body, c.File, step.Marker, callee)
	case "consumer_dead_code":
		return !ast.IsExported(callee) && editConsumerDeadPending(body, c.File, callee)
	default:
		return false
	}
}

func editConsumerVerify(body map[string]any, file, marker string) bool {
	checked, ok := editConsumerUint(body["checked_callers"])
	rows, valid := editConsumerRows(body, "violations")
	if body["clean"] != false || !ok || checked == 0 || !valid {
		return false
	}
	for _, item := range rows {
		row := item.(map[string]any)
		line, valid := editConsumerUint(row["line"])
		if valid && line > 0 && row["kind"] == "caller_mismatch" && editConsumerSymbol(row, "symbol_id", file, marker) {
			return true
		}
	}
	return false
}

func editConsumerFileListed(value any, file string) bool {
	files, ok := value.([]any)
	if !ok {
		return false
	}
	for _, value := range files {
		if path, ok := value.(string); ok && editProbeFixturePath(path, file) {
			return true
		}
	}
	return false
}

func editConsumerDetect(body map[string]any, file, marker string) bool {
	rows, valid := editConsumerRows(body, "changed_symbols")
	if !valid || body["scope"] != "unstaged" || !editConsumerFileListed(body["changed_files"], file) {
		return false
	}
	for _, item := range rows {
		if row := item.(map[string]any); row["name"] == marker && editConsumerSymbol(row, "id", file, marker) {
			return true
		}
	}
	return false
}

func editConsumerRename(body map[string]any, file, marker, callee string) bool {
	rows, valid := editConsumerRows(body, "edits")
	if !valid || body["dry_run"] != true || body["old_name"] != callee || body["new_name"] != callee+"Renamed" {
		return false
	}
	for _, item := range rows {
		row := item.(map[string]any)
		line, valid := editConsumerUint(row["line"])
		path, _ := row["file"].(string)
		before, _ := row["old_text"].(string)
		after, _ := row["new_text"].(string)
		confidence := row["confidence"]
		if valid && line > 0 && editProbeFixturePath(path, file) && (confidence == "high" || confidence == "provisional") &&
			editProbeSourceCall(before, marker, callee) && editProbeSourceCall(after, marker, callee+"Renamed") {
			return true
		}
	}
	return false
}

func editConsumerDeadRows(body map[string]any, key, count string) ([]any, bool) {
	rows, ok := editConsumerRows(body, key)
	n, valid := editConsumerUint(body[count])
	return rows, ok && valid && n == uint64(len(rows)) && body["truncated"] == false
}

func editConsumerDeadContains(rows []any, file, callee string) bool {
	for _, item := range rows {
		if row := item.(map[string]any); row["name"] == callee && editConsumerSymbol(row, "id", file, callee) {
			return true
		}
	}
	return false
}

func editConsumerDeadPending(body map[string]any, file, callee string) bool {
	dead, valid := editConsumerDeadRows(body, "dead_code", "total")
	unknown, complete := editConsumerDeadRows(body, "pending_unknown", "pending_unknown_count")
	return valid && complete && !editConsumerDeadContains(dead, file, callee) && editConsumerDeadContains(unknown, file, callee) &&
		(body["graph_freshness"] == "pending" || body["graph_freshness"] == "partial") && body["freshness_scope"] == "derived" && body["complete"] == false
}

func editConsumerPending(body map[string]any, file, scope string) bool {
	if body["graph_freshness"] != "pending" && body["graph_freshness"] != "partial" {
		return false
	}
	n, ok := editConsumerUint(body["pending_file_count"])
	return ok && n > 0 && body["complete"] == false && body["freshness_scope"] == scope && editConsumerFileListed(body["pending_files"], file)
}

func editConsumerFresh(body map[string]any) bool {
	if value, exists := body["graph_freshness"]; exists && value != "fresh" {
		return false
	}
	if value, exists := body["complete"]; exists && value != true {
		return false
	}
	return true // relation consumers omit the pending label for a fresh answer
}

func (r *editProbeRuntime) admitConsumerBaseline(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, min(r.options.Deadline, 20*time.Second))
	defer cancel()
	s := r.states[0]
	repo, err := r.repoFor(s.step)
	if err != nil {
		return err
	}
	req := editConsumerDeadCodeRequest()
	raw, identity, callErr := s.client.callWithIdentity(ctx, "tools/call", map[string]any{"name": req.Tool, "arguments": dispatchArgs(req, repo)})
	if callErr == nil {
		callErr = checkToolResult(raw)
	}
	body, decodeErr := decodeEditProbeOutput(raw, "json", req.name())
	dead, complete := editConsumerDeadRows(body, "dead_code", "total")
	proven := callErr == nil && decodeErr == nil && editConsumerPayloadValid(body) && complete && editConsumerFresh(body) && editConsumerDeadContains(dead, repo.Corpus.File, r.plan.Callee)
	if err := s.emit(map[string]any{"event": "consumer_dead_code_baseline", "request": req, "wire_id": identity.ID, "write_started": identity.WriteStarted, "finished": identity.Finished, "payload": raw, "proven": proven}); err != nil {
		return err
	}
	if !proven {
		return fmt.Errorf("consumer fixture lacks actual pre-edit unexported dead-code membership: %w", errors.Join(callErr, decodeErr, errors.New("baseline absent, pending, malformed or truncated")))
	}
	r.phase.EditProbe.ConsumerBaseline = &editConsumerBaseline{State: "OBSERVED_DEAD_BEFORE_EDIT", Symbol: repo.Prefix + "/" + repo.Corpus.Symbol, Evidence: s.record.Evidence}
	return nil
}

func (r *editProbeRuntime) observeConsumer(s *editProbeState, oracle editProbeOracle, data json.RawMessage, identity wireCallIdentity, callErr error) error {
	repo, err := r.repoFor(s.step)
	if err != nil {
		return err
	}
	mode := r.probeOutput(oracle, s.step)
	body, decodeErr := decodeEditProbeOutput(data, mode, oracle.call.name())
	state := "malformed"
	if callErr != nil {
		state = "error"
	} else if decodeErr == nil && editConsumerPayloadValid(body) {
		state = r.consumerVerdict(oracle.name, body, repo.Corpus, s.step)
	}
	s.mu.Lock()
	row := s.record.Consumers[oracle.name]
	row.Attempts++
	postACK := !s.acknowledged.IsZero() && !identity.WriteStarted.IsZero() && !identity.WriteStarted.Before(s.acknowledged)
	if callErr != nil || decodeErr != nil || state == "malformed" {
		row.Errors++
	}
	if postACK {
		row.PostACKAttempts++
		row.State = state
		if state == "unassessed" {
			row.Reason = "rename rows do not identify this caller; no stale or clean coverage"
		}
		switch state {
		case "pending":
			row.Pending++
			row.Assessed++
		case "subject_observed", "fresh_membership_transition":
			row.SubjectMatches++
			row.Assessed++
		case "silent_stale":
			row.stale++
			row.Assessed++
		default:
			row.Unassessed++
		}
	}
	row.SilentStale = unavailable("answers", "consumer coverage missing, unsupported, failed or unassessed")
	if row.PostACKAttempts > 0 && row.Unassessed == 0 && row.Errors == 0 && row.Assessed == row.PostACKAttempts {
		row.SilentStale = measured(float64(row.stale), "answers")
	}
	s.record.Consumers[oracle.name] = row
	s.mu.Unlock()
	return s.emit(map[string]any{"event": "consumer_probe", "oracle": oracle.name, "requested_mode": r.stepProbeMode(s.step), "output_mode": mode, "wire_id": identity.ID, "start_ns": editProbeOffset(identity.WriteStarted, r.origin), "end_ns": editProbeOffset(identity.Finished, r.origin), "post_ack": postACK, "state": state})
}

func (r *editProbeRuntime) consumerVerdict(name string, body map[string]any, c Corpus, step editProbeStep) string {
	if !editConsumerPayloadValid(body) || !editConsumerShape(name, body, r.plan.Callee) {
		return "malformed"
	}
	scope := "relations"
	if name == "consumer_dead_code" {
		scope = "derived"
	}
	if editConsumerPending(body, c.File, scope) {
		if name != "consumer_dead_code" || editConsumerDeadPending(body, c.File, r.plan.Callee) {
			return "pending"
		}
		return "malformed"
	}
	if !editConsumerFresh(body) {
		return "malformed"
	}
	if name == "consumer_dead_code" {
		dead, complete := editConsumerDeadRows(body, "dead_code", "total")
		baseline := r.phase.EditProbe.ConsumerBaseline
		if !complete || baseline == nil || baseline.State != "OBSERVED_DEAD_BEFORE_EDIT" || !editProbeSourceID(baseline.Symbol, c.File, r.plan.Callee) {
			return "malformed"
		}
		if value, exists := body["pending_unknown"]; exists && value != nil {
			unknown, valid := editConsumerDeadRows(body, "pending_unknown", "pending_unknown_count")
			if !valid || len(unknown) != 0 {
				return "malformed"
			}
		}
		if !editConsumerDeadContains(dead, c.File, r.plan.Callee) {
			return "fresh_membership_transition"
		}
		return "silent_stale"
	}
	if editConsumerSubject(name, body, c, step, r.plan.Callee) {
		return "subject_observed"
	}
	if name == "consumer_rename" {
		// A valid rename can return a call-site-only line from a multiline
		// caller. Without caller identity that line cannot prove this marker
		// absent, and a sparse edit row cannot do so either.
		return "unassessed"
	}
	return "silent_stale"
}

func editConsumerShape(name string, body map[string]any, callee string) bool {
	switch name {
	case "consumer_verify":
		rows, valid := editConsumerRows(body, "violations")
		_, count := editConsumerUint(body["checked_callers"])
		clean, boolean := body["clean"].(bool)
		return valid && count && boolean && clean == (len(rows) == 0)
	case "consumer_detect":
		_, valid := editConsumerRows(body, "changed_symbols")
		files, array := body["changed_files"].([]any)
		if !valid || !array || body["scope"] != "unstaged" {
			return false
		}
		for _, file := range files {
			if _, ok := file.(string); !ok {
				return false
			}
		}
		return true
	case "consumer_rename":
		_, valid := editConsumerRows(body, "edits")
		return valid && body["dry_run"] == true && body["old_name"] == callee && body["new_name"] == callee+"Renamed"
	case "consumer_dead_code":
		_, complete := editConsumerDeadRows(body, "dead_code", "total")
		return complete
	default:
		return false
	}
}

func editConsumersFinished(rows map[string]editConsumerObservation) bool {
	if len(rows) == 0 {
		return true // the unchanged default profile has no consumer denominator
	}
	if len(rows) != len(editConsumerNames) {
		return false
	}
	for _, name := range editConsumerNames {
		row, exists := rows[name]
		if !exists {
			return false
		}
		if row.PostACKAttempts == 0 && row.State != "unsupported" {
			return false
		}
	}
	return true
}

func editConsumersFailure(rows map[string]editConsumerObservation) bool {
	if !editConsumersFinished(rows) {
		return true
	}
	for _, row := range rows {
		if row.PostACKAttempts == 0 || row.Assessed == 0 || row.Errors > 0 || row.Unassessed > 0 || row.stale > 0 {
			return true
		}
	}
	return false
}

// Only compact is explicitly unsupported here. No call is dispatched and no
// JSON answer is substituted for the missing consumer compact adapter.
func (s *editProbeState) markUnsupportedConsumer(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.record.Consumers[name]
	row.State, row.Reason = "unsupported", "no actual compact consumer decoder; no format or silent-stale credit"
	s.record.Consumers[name] = row
}
