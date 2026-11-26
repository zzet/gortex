package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"
)

// The contract-freshness scenario measures how long a primary-checkout
// contract change takes to read back fresh. Each witness edit adds one HTTP
// provider route whose contract ID exists nowhere else, through the public
// edit.file operation. Timed contract listings then report when an answer
// first carries that ID. Both operations belong to the compact surface the
// published releases also advertise, so a release and a candidate run the
// same requests and their freshness times compare directly.
const (
	contractFreshnessScenario   = "contract-freshness"
	contractFreshnessMaxEdits   = 200
	contractFreshnessMaxReaders = 16
)

// contractFreshnessPlan is the offered witness identity. It is part of the
// workload and therefore of the request plan hash.
type contractFreshnessPlan struct {
	Version        int     `json:"version"`
	Edits          int     `json:"edits"`
	ReadIntervalMS float64 `json:"read_interval_ms"`
	ListLimit      int     `json:"list_limit"`
	RoutePrefix    string  `json:"route_prefix"`
}

func newContractFreshnessPlan(edits int) contractFreshnessPlan {
	return contractFreshnessPlan{Version: 1, Edits: edits, ReadIntervalMS: 250, ListLimit: 10000, RoutePrefix: "/gortex-contract-witness/"}
}

func (plan contractFreshnessPlan) interval() time.Duration {
	return time.Duration(plan.ReadIntervalMS * float64(time.Millisecond))
}

// contractWitness is one edit's response identity: a new Go function that
// registers a route under its own name, and the contract ID that route mints.
type contractWitness struct {
	Symbol   string
	Route    string
	Identity string
}

func (plan contractFreshnessPlan) witness(ordinal int) contractWitness {
	return plan.witnessNamed(fmt.Sprintf("GortexContractWitnessE%d", ordinal))
}

func (plan contractFreshnessPlan) witnessNamed(symbol string) contractWitness {
	route := plan.RoutePrefix + symbol
	return contractWitness{Symbol: symbol, Route: route, Identity: "http::GET::" + route}
}

// readRequest lists the HTTP providers. Dispatch adds the primary
// repository's actual prefix as repo.
func (plan contractFreshnessPlan) readRequest() request {
	return request{Tool: "analyze", Operation: "contracts", Arguments: map[string]any{"kind": "contracts", "action": "list", "type": "http", "role": "provider", "limit": plan.ListLimit}}
}

// contractFreshnessEditRequest inserts the witness function after the
// corpus's own edit anchor, where the corpus template already inserts a
// top-level Go function.
func contractFreshnessEditRequest(corpus Corpus, witness contractWitness) request {
	declaration := fmt.Sprintf("\nfunc %s() {\n\thttp.HandleFunc(%q, %s)\n}\n", witness.Symbol, "GET "+witness.Route, witness.Symbol)
	return request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file", "target": map[string]any{"file": corpus.File}, "match": corpus.EditAnchor, "replacement": corpus.EditAnchor + declaration}}
}

func contractFreshnessOperations(corpus Corpus, o runOptions) ([]request, error) {
	var operations []request
	if o.Agents > 0 {
		var err error
		if operations, err = readingPlan(corpus); err != nil {
			return nil, err
		}
	}
	plan := newContractFreshnessPlan(o.ContractFreshnessEdits)
	return append(operations, plan.readRequest(), contractFreshnessEditRequest(corpus, plan.witnessNamed("{{marker}}"))), nil
}

func validateContractFreshnessOptions(o runOptions) error {
	if o.Scenario != contractFreshnessScenario {
		if o.ContractFreshnessEdits != 0 {
			return errors.New("--contract-freshness-edits requires --scenario contract-freshness")
		}
		return nil
	}
	if o.ContractFreshnessEdits < 1 || o.ContractFreshnessEdits > contractFreshnessMaxEdits || o.Editors != 1 || o.Agents < 0 || o.Agents > contractFreshnessMaxReaders || o.Worktrees != 0 || o.EditRepos != 1 || o.CPUProfile != 0 {
		return fmt.Errorf("contract-freshness witnesses one primary checkout: --contract-freshness-edits 1..%d, --edit-agents 1, --agents 0..%d, --worktrees 0, one repository and no CPU profile", contractFreshnessMaxEdits, contractFreshnessMaxReaders)
	}
	return nil
}

// validateContractFreshnessCorpus admits a Go fixture whose own edit template
// inserts a top-level function after its anchor: the witness function goes
// in the same place.
func validateContractFreshnessCorpus(c Corpus) error {
	if path.Ext(c.File) != ".go" {
		return fmt.Errorf("corpus %s: contract-freshness edits a Go file", c.Label)
	}
	if c.EditAnchor == "" || !strings.HasPrefix(strings.TrimLeft(c.EditTemplate, "\n"), "func {{marker}}(") {
		return fmt.Errorf("corpus %s: contract-freshness needs an edit_anchor after which edit_template inserts a top-level Go function", c.Label)
	}
	return nil
}

// ContractFreshnessObservation is one witness edit. Times are milliseconds
// from the edit's dispatch, except FreshAfterAckMS (from its acknowledgement)
// and FreshReadMS (the fresh read's own latency). The fresh answer appeared
// after FreshNotBeforeMS, when the last read that still lacked the identity
// began, and no later than FreshMS, when the first read carrying it returned.
type ContractFreshnessObservation struct {
	Ordinal          int             `json:"ordinal"`
	Identity         string          `json:"identity"`
	Symbol           string          `json:"symbol"`
	Baseline         string          `json:"baseline"`
	Outcome          string          `json:"outcome"` // fresh, never_fresh, phase_censored, edit_error, baseline_error, identity_preexisting
	Started          time.Time       `json:"started"`
	AcknowledgedMS   Metric          `json:"acknowledged_ms"`
	FreshMS          Metric          `json:"fresh_ms"`
	FreshAfterAckMS  Metric          `json:"fresh_after_ack_ms"`
	FreshNotBeforeMS Metric          `json:"fresh_not_before_ms"`
	FreshReadMS      Metric          `json:"fresh_read_ms"`
	Reads            int             `json:"reads"`
	ReadOutcomes     map[string]int  `json:"read_outcomes"`
	FreshReport      json.RawMessage `json:"fresh_contract_analysis,omitempty"`
	Acknowledgement  json.RawMessage `json:"acknowledgement"`
}

type contractFreshnessReport struct {
	Observations  []ContractFreshnessObservation `json:"observations"`
	Outcomes      map[string]int                 `json:"outcomes"`
	Acknowledged  Distribution                   `json:"acknowledged_ms"`
	Fresh         Distribution                   `json:"fresh_ms"`
	FreshAfterAck Distribution                   `json:"fresh_after_ack_ms"`
	FreshRead     Distribution                   `json:"fresh_read_ms"`
}

// classifyContractRead reports whether one contracts listing carries the
// identity as a listed contract ID. An echo of the identity in any other field
// is not a fresh answer. Absent answers that the product itself marks as not
// fresh or not complete are kept apart from unmarked ones, and a truncated
// listing proves nothing about the identity.
func classifyContractRead(result json.RawMessage, identity string) (string, json.RawMessage) {
	payload, err := textPayload(result, "content")
	if err != nil {
		return "unparsed", nil
	}
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return "unparsed", nil
	}
	object, _ := value.(map[string]any)
	var report json.RawMessage
	analysis, _ := object["contract_analysis"].(map[string]any)
	if analysis != nil {
		report, _ = json.Marshal(analysis)
	}
	if listsContractID(value, identity) {
		return "fresh", report
	}
	if truncated, _ := object["truncated"].(bool); truncated {
		return "truncated", report
	}
	if cursor, _ := object["next_cursor"].(string); cursor != "" {
		return "truncated", report
	}
	if analysis != nil {
		fresh, hasFresh := analysis["fresh"].(bool)
		state, hasState := analysis["state"].(string)
		if hasFresh && !fresh || hasState && state != "complete" {
			return "absent_flagged", report
		}
	}
	return "absent", report
}

func listsContractID(value any, identity string) bool {
	switch row := value.(type) {
	case map[string]any:
		if id, _ := row["id"].(string); id == identity {
			return true
		}
		for _, child := range row {
			if listsContractID(child, identity) {
				return true
			}
		}
	case []any:
		for _, child := range row {
			if listsContractID(child, identity) {
				return true
			}
		}
	}
	return false
}

func contractReadArgs(plan contractFreshnessPlan, repo privateRepo) map[string]any {
	args := dispatchArgs(plan.readRequest(), repo)
	args["repo"] = repo.Prefix
	return args
}

func milliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// awaitContractAnswer is untimed setup before the first witness edit. The
// primary checkout's listing must answer complete, without the product
// marking it pending, so a first contract initialization is reported here and
// never as a witness edit's freshness time. Only an incomplete-capability
// refusal or a pending answer is waited out; any other answer fails at once.
func awaitContractAnswer(ctx context.Context, client toolCaller, plan contractFreshnessPlan, repo privateRepo, deadline time.Duration) (json.RawMessage, []Attempt, error) {
	started := time.Now()
	read := plan.readRequest()
	receipt := struct {
		Reads     int            `json:"reads"`
		ElapsedMS float64        `json:"elapsed_ms"`
		Outcomes  map[string]int `json:"outcomes"`
	}{Outcomes: make(map[string]int)}
	var attempts []Attempt
	for {
		result, rows, err := callTool(ctx, client, read, contractReadArgs(plan, repo), "warm", 0, 0, receipt.Reads, deadline, waitContext)
		attempts = append(attempts, rows...)
		receipt.Reads++
		outcome := "error"
		var refusal *rpcError
		switch {
		case err == nil:
			outcome, _ = classifyContractRead(result, plan.witness(0).Identity)
		case errors.As(err, &refusal) && strings.HasPrefix(refusal.Message, "required_capability_incomplete"):
			outcome = "incomplete"
		}
		receipt.Outcomes[outcome]++
		receipt.ElapsedMS = milliseconds(time.Since(started))
		gate, _ := json.Marshal(receipt)
		switch outcome {
		case "absent":
			return gate, attempts, nil
		case "incomplete", "absent_flagged":
			if waitContext(ctx, plan.interval()) != nil {
				return gate, attempts, fmt.Errorf("primary contract listing never answered complete inside the warm budget: %w", ctx.Err())
			}
		case "fresh":
			return gate, attempts, errors.New("contract witness identity listed before any witness edit")
		default:
			return gate, attempts, fmt.Errorf("primary contract listing cannot serve the witness (%s): %v", outcome, err)
		}
	}
}

// observeContractFreshness runs one witness edit. A read before the edit must
// lack the identity. After the acknowledgement, reads repeat every plan
// interval until one carries the identity or the product deadline, measured
// from the edit's dispatch, passes.
func observeContractFreshness(ctx context.Context, deadline time.Duration, plan contractFreshnessPlan, client toolCaller, repo privateRepo, agent, ordinal int) (ContractFreshnessObservation, []Attempt) {
	witness := plan.witness(ordinal)
	notObserved := unavailable("ms", "not observed")
	observation := ContractFreshnessObservation{Ordinal: ordinal, Identity: witness.Identity, Symbol: witness.Symbol, Outcome: "baseline_error", AcknowledgedMS: notObserved, FreshMS: notObserved, FreshAfterAckMS: notObserved, FreshNotBeforeMS: notObserved, FreshReadMS: notObserved, ReadOutcomes: make(map[string]int)}
	read := plan.readRequest()
	result, attempts, err := callTool(ctx, client, read, contractReadArgs(plan, repo), "probe", agent, ordinal, 0, deadline, waitContext)
	observation.Baseline = "error"
	if err == nil {
		observation.Baseline, _ = classifyContractRead(result, witness.Identity)
	}
	switch observation.Baseline {
	case "fresh":
		observation.Baseline, observation.Outcome = "present", "identity_preexisting"
		return observation, attempts
	case "absent", "absent_flagged":
	default:
		if ctx.Err() != nil {
			observation.Outcome = "phase_censored"
		}
		return observation, attempts
	}

	edit := contractFreshnessEditRequest(repo.Corpus, witness)
	start := time.Now()
	observation.Started, observation.Outcome = start.UTC(), "edit_error"
	ack, rows, err := callTool(ctx, client, edit, dispatchArgs(edit, repo), "workload", agent, ordinal, 0, deadline, waitContext)
	acknowledged := time.Now()
	attempts = append(attempts, rows...)
	observation.Acknowledgement = ack
	if err != nil {
		if ctx.Err() != nil {
			observation.Outcome = "phase_censored"
		}
		return observation, attempts
	}
	observation.AcknowledgedMS = measured(milliseconds(acknowledged.Sub(start)), "ms")
	readCtx, cancel := context.WithDeadline(ctx, start.Add(deadline))
	defer cancel()
	notBefore := 0.0 // before its dispatch the edit cannot be read back
	for slot := 1; readCtx.Err() == nil; slot++ {
		readStarted := time.Now()
		result, rows, err := callTool(ctx, client, read, contractReadArgs(plan, repo), "probe", agent, ordinal, slot, time.Until(start.Add(deadline)), waitContext)
		readFinished := time.Now()
		attempts = append(attempts, rows...)
		observation.Reads++
		outcome, report := "error", json.RawMessage(nil)
		switch {
		case err == nil:
			outcome, report = classifyContractRead(result, witness.Identity)
		case readCtx.Err() != nil:
			outcome = "deadline" // cut off by the product deadline or the phase
		}
		observation.ReadOutcomes[outcome]++
		if outcome == "fresh" {
			observation.Outcome = "fresh"
			observation.FreshMS = measured(milliseconds(readFinished.Sub(start)), "ms")
			observation.FreshAfterAckMS = measured(milliseconds(readFinished.Sub(acknowledged)), "ms")
			observation.FreshNotBeforeMS = measured(notBefore, "ms")
			observation.FreshReadMS = measured(milliseconds(readFinished.Sub(readStarted)), "ms")
			observation.FreshReport = report
			return observation, attempts
		}
		if outcome == "absent" || outcome == "absent_flagged" {
			notBefore = milliseconds(readStarted.Sub(start))
		}
		if waitContext(readCtx, plan.interval()) != nil {
			break
		}
	}
	observation.Outcome = "never_fresh"
	if end, ok := ctx.Deadline(); ok && end.Before(start.Add(deadline)) || ctx.Err() == context.Canceled {
		observation.Outcome = "phase_censored"
	}
	return observation, attempts
}

// validateContractFreshnessReport ties the witness report to its offered
// plan: only the contract-freshness scenario carries one, and a complete run
// read every witness edit back fresh.
func validateContractFreshnessReport(r Report) error {
	plan := r.Workload.ContractFreshness
	if (r.Workload.Scenario == contractFreshnessScenario) != (plan != nil) {
		return errors.New("contract-freshness plan is missing from its scenario or present in another")
	}
	if plan != nil && (plan.Edits < 1 || plan.Edits > contractFreshnessMaxEdits || *plan != newContractFreshnessPlan(plan.Edits) || r.Workload.LinkedWorktrees != 0) {
		return errors.New("invalid contract-freshness plan")
	}
	for _, phase := range r.Phases {
		report := phase.ContractFreshness
		if report == nil {
			if r.Complete && phase.Name == contractFreshnessScenario {
				return errors.New("complete contract-freshness phase has no witness report")
			}
			continue
		}
		if phase.Name != contractFreshnessScenario || plan == nil || len(report.Observations) > plan.Edits {
			return errors.New("contract witness report does not match its plan")
		}
		total := 0
		for _, count := range report.Outcomes {
			if count < 0 {
				return errors.New("negative contract witness outcome count")
			}
			total += count
		}
		if total != len(report.Observations) || r.Complete && report.Outcomes["fresh"] != plan.Edits {
			return errors.New("contract witness outcomes disagree with its observations")
		}
		for _, d := range []Distribution{report.Acknowledged, report.Fresh, report.FreshAfterAck, report.FreshRead} {
			if err := validDistribution(d); err != nil {
				return err
			}
		}
		if report.Fresh.Count != report.Outcomes["fresh"] {
			return errors.New("contract freshness distribution disagrees with its fresh outcomes")
		}
	}
	return nil
}

func (report *contractFreshnessReport) finish() {
	report.Outcomes = make(map[string]int)
	var acknowledged, fresh, afterAck, read []float64
	for _, observation := range report.Observations {
		report.Outcomes[observation.Outcome]++
		if value := observation.AcknowledgedMS.Value; value != nil {
			acknowledged = append(acknowledged, *value)
		}
		if observation.Outcome == "fresh" {
			fresh = append(fresh, *observation.FreshMS.Value)
			afterAck = append(afterAck, *observation.FreshAfterAckMS.Value)
			read = append(read, *observation.FreshReadMS.Value)
		}
	}
	report.Acknowledged, report.Fresh = distribution(acknowledged, "ms"), distribution(fresh, "ms")
	report.FreshAfterAck, report.FreshRead = distribution(afterAck, "ms"), distribution(read, "ms")
}

// measureContractFreshness runs the witness edits one after another on the
// last session, paced by the profile. Optional pure readers share the
// primary checkout and stop when the witness finishes. Every witness edit
// must read back fresh for a complete phase.
func measureContractFreshness(parent context.Context, o runOptions, p *privateRun, clients []*socketClient, phase *Phase) error {
	plan := newContractFreshnessPlan(o.ContractFreshnessEdits)
	ctx, cancel := context.WithTimeout(parent, o.Duration)
	defer cancel()
	readersCtx, stopReaders := context.WithCancel(ctx)
	defer stopReaders()
	var readers sync.WaitGroup
	if o.Agents > 0 {
		readers.Go(func() { _ = runAgents(readersCtx, o, p, clients[:o.Agents], false, phase) })
	}
	report := &contractFreshnessReport{}
	var attempts []Attempt
	var fault error
	pace := newPacing(profile(o.Profile), uint64(o.Seed)+uint64(o.Agents))
	for ordinal := range plan.Edits {
		if waitContext(ctx, pace.think()) != nil {
			break
		}
		observation, rows := observeContractFreshness(ctx, o.Deadline, plan, clients[o.Agents], p.Repos[0], o.Agents, ordinal)
		report.Observations = append(report.Observations, observation)
		attempts = append(attempts, rows...)
		if observation.Outcome == "identity_preexisting" {
			fault = fmt.Errorf("contract witness %s was listed before its edit", observation.Identity)
			break
		}
	}
	stopReaders()
	readers.Wait()
	phase.Attempts = append(phase.Attempts, attempts...)
	report.finish()
	phase.ContractFreshness = report
	if fresh := report.Outcomes["fresh"]; fresh != plan.Edits && fault == nil {
		var outcomes []string
		for _, name := range []string{"never_fresh", "phase_censored", "edit_error", "baseline_error"} {
			if count := report.Outcomes[name]; count > 0 {
				outcomes = append(outcomes, fmt.Sprintf("%d %s", count, name))
			}
		}
		fault = fmt.Errorf("%d of %d contract witness edits read back fresh (%s; %d not offered inside the phase)", fresh, plan.Edits, strings.Join(outcomes, ", "), plan.Edits-len(report.Observations))
	}
	return errors.Join(fault, parent.Err())
}
