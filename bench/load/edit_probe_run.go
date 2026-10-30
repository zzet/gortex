package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const editProbeRawLimit = 8 << 20

var errEditProbeEvidenceIncomplete = errors.New("harness evidence incomplete")

type editProbeReadback struct {
	Kind            string `json:"kind"`
	Latency         Metric `json:"latency_ms"`
	Correct         bool   `json:"correct"`
	Error           string `json:"error"`
	ExpectedOutcome string `json:"expected_outcome,omitempty"`
	Refusal         string `json:"refusal,omitempty"`
}
type editProbeRecord struct {
	Ordinal                  int                       `json:"ordinal"`
	Marker                   string                    `json:"marker"`
	MutationID               string                    `json:"mutation_id"`
	PlannedDueNS             int64                     `json:"planned_due_ns"`
	ActivatedNS              *int64                    `json:"activated_ns"`
	StartedNS                *int64                    `json:"started_ns"`
	StartLatenessNS          *int64                    `json:"start_lateness_ns"`
	Outcome                  string                    `json:"outcome"`
	Receipt                  string                    `json:"mutation_receipt"`
	Batch                    *editProbeBatchReferences `json:"batch,omitempty"`
	Move                     *editProbeMoveReferences  `json:"move,omitempty"`
	MoveAccounting           *editProbeMoveAccounting  `json:"move_accounting,omitempty"`
	GraphTerminalBasis       string                    `json:"graph_terminal_basis,omitempty"`
	Ack                      Metric                    `json:"t_ack_ms"`
	Readbacks                []editProbeReadback       `json:"readbacks"`
	FirstCorrect             map[string]Metric         `json:"first_correct_ms"`
	Accounting               editProbeAccounting       `json:"accounting"`
	PendingReads             int                       `json:"reads_while_receipt_observed_pending"`
	ExpectedRefusals         int                       `json:"expected_refusals,omitempty"`
	ProbeErrors              int                       `json:"probe_errors"`
	StaleClaimedFresh        bool                      `json:"stale_claimed_fresh"`
	PrecommitFreshMismatches int                       `json:"precommit_fresh_mismatches_unassessed"`
	GraphTerminal            bool                      `json:"graph_terminal"`
	Evidence                 string                    `json:"raw_evidence"`
	EvidenceWriteMS          float64                   `json:"evidence_write_ms"`
	EvidenceEncodeMS         float64                   `json:"evidence_encode_ms"`
	EvidenceBytes            int                       `json:"evidence_bytes"`
	EvidenceLimit            int                       `json:"evidence_limit_bytes"`
	Errors                   []string                  `json:"errors"`
}
type editProbeReport struct {
	Plan               editProbePlan     `json:"plan"`
	PlanHash           string            `json:"plan_sha256"`
	AccountingRequired bool              `json:"accounting_required"`
	Records            []editProbeRecord `json:"edits"`
	RestoreMS          Metric            `json:"restore_batch_ms"`
	Restore            json.RawMessage   `json:"restore_batch"`
	StatsBefore        json.RawMessage   `json:"stats_before"`
	StatsAfter         json.RawMessage   `json:"stats_after"`
	Limitations        []string          `json:"limitations"`
}

type editProbeState struct {
	mu                                                sync.Mutex
	step                                              editProbeStep
	record                                            editProbeRecord
	client                                            *socketClient
	sink                                              io.WriteCloser
	sinkMu                                            sync.Mutex
	rawBytes                                          int
	rawAccepted                                       int
	rawPending                                        []byte
	rawWake                                           chan struct{}
	rawClosing                                        bool
	rawDone                                           chan struct{}
	rawOnce                                           sync.Once
	writeNS, encodeNS                                 int64
	sinkErr                                           error
	cancel                                            context.CancelFunc
	pairedContext                                     context.Context // shared per-edit probes; cleared after joined completion
	probes                                            sync.WaitGroup
	started, acknowledged, firstProbeWrite, editWrite time.Time
	absenceBefore                                     map[string]bool
}

type editProbeRuntime struct {
	options           runOptions
	p                 *privateRun
	phase             *Phase
	plan              editProbePlan
	editor            *socketClient
	states            []*editProbeState
	original          []byte
	moveOriginal      []byte
	origin            time.Time
	mu                sync.Mutex // Phase.Attempts only; request lifetimes are never serialized
	requireAccounting bool
	finalized         bool
}

func newEditProbeRecord(c Corpus, step editProbeStep) editProbeRecord {
	record := editProbeRecord{Ordinal: step.Ordinal, Marker: step.Marker, MutationID: step.MutationID, PlannedDueNS: step.DueNS, Outcome: "phase_censored", Ack: unavailable("ms", "edit not sent"), FirstCorrect: make(map[string]Metric), Accounting: editProbeAccounting{State: "not_observed"}, Errors: []string{"scenario never reached edit dispatch"}}
	for _, oracle := range editProbeOracles(c, step) {
		record.FirstCorrect[oracle.name] = unavailable("ms", "not observed")
	}
	return record
}

func newEditProbeReport(plan editProbePlan, c Corpus, required bool) *editProbeReport {
	hash, _ := hashJSON(plan)
	language := "Go"
	if plan.Language == "typescript" {
		language = "TypeScript"
	}
	if plan.Language == "python" {
		language = "Python"
	}
	out := &editProbeReport{Plan: plan, PlanHash: hash, AccountingRequired: required, RestoreMS: unavailable("ms", "post-window batch not performed"), Limitations: []string{fmt.Sprintf("one %s/%s/%s/%s cell only", language, plan.Kind, plan.Tool, strings.ToUpper(plan.Mode)), "per-window numeric checks do not establish 100-edit/reference acceptance", "missing/unsupported B stays in the complete offered denominator"}}
	if plan.Mode == "toon" {
		out.Limitations = append(out.Limitations, "TOON is evaluated from the requested text, never a structured JSON sibling; setup, timing-status and restore controls remain explicit JSON with exact numeric handling", "TOON traversal subgraphs omit JSON total_nodes/full untrimmed total_edges and signature metadata; summaries keep full fields, omitted traversal fields are not synthesized", "complete TOON subgraph checks require explicit nontruncation, exact declared node total and the emitted row census; an omitted full edge-count field is not reported as a measured value")
	}
	for _, step := range plan.Steps {
		out.Records = append(out.Records, newEditProbeRecord(c, step))
	}
	return out
}

func prepareEditProbe(ctx context.Context, o runOptions, p *privateRun, editor *socketClient, phase *Phase) (*editProbeRuntime, []*socketClient, error) {
	plan, err := makeEditProbePlan(p.Repos[0].Corpus, o.Seed, o.EditCount)
	if err != nil {
		return nil, nil, err
	}
	original, err := os.ReadFile(filepath.Join(p.Repos[0].Path, filepath.FromSlash(plan.File)))
	if err != nil {
		return nil, nil, err
	}
	if err := editProbeWriteAdmission(p.Repos[0].Corpus, original); err != nil {
		return nil, nil, err
	}
	if filepath.Ext(plan.File) == ".ts" {
		valid, _ := editProbeTypeScriptFixture(editProbeCrossFilePlain(p.Repos[0].Corpus, string(original)), "", "")
		if !valid {
			return nil, nil, errors.New("TypeScript cell requires an explicit numeric-function fixture")
		}
	}
	if filepath.Ext(plan.File) == ".py" {
		valid, _ := editProbePythonFixture(editProbeCrossFilePlain(p.Repos[0].Corpus, string(original)), "", "")
		if !valid {
			return nil, nil, errors.New("python cell requires an explicit top-level single-return fixture")
		}
	}
	if strings.Count(string(original), p.Repos[0].Corpus.EditAnchor) != 1 {
		return nil, nil, errors.New("edit-probe anchor must occur exactly once in original fixture")
	}
	for _, step := range plan.Steps {
		if step.BeforeSymbol == "" && strings.Contains(string(original), step.Marker) {
			return nil, nil, errors.New("planned marker already exists before edit")
		}
	}
	r := &editProbeRuntime{options: o, p: p, phase: phase, plan: plan, editor: editor, original: original, requireAccounting: !o.editProbeBaseline}
	if plan.Tool == "refactor.move" {
		r.moveOriginal, err = os.ReadFile(filepath.Join(p.Repos[0].Path, filepath.FromSlash(p.Repos[0].Corpus.EditDestination)))
		if err != nil {
			return r, nil, err
		}
		if string(r.moveOriginal) != p.Repos[0].Corpus.EditDestinationContent {
			return r, nil, errors.New("move destination bytes differ from offered fixture")
		}
	}
	phase.EditProbe = newEditProbeReport(plan, p.Repos[0].Corpus, r.requireAccounting)
	if p.Repos[0].Corpus.EditKind == "add_crossfile_call" {
		if err := r.admitCrossFileCallee(ctx); err != nil {
			return r, nil, err
		}
	}
	phase.editProbeRuntime = r
	dir := filepath.Join(p.Root, "edit-probe")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return r, nil, err
	}
	var clients []*socketClient
	setup := time.Now()
	for _, step := range plan.Steps {
		record := newEditProbeRecord(p.Repos[0].Corpus, step)
		record.Outcome = "not_started"
		record.Errors = nil
		state := &editProbeState{step: step, record: record}
		r.states = append(r.states, state)
		state.record.Evidence = filepath.Join(dir, fmt.Sprintf("edit-%03d.jsonl", step.Ordinal))
	}
	for _, state := range r.states {
		sink, err := os.OpenFile(state.record.Evidence, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return r, clients, err
		}
		state.sink = sink
		state.startRaw()
		state.client, err = openMCP(ctx, p.Socket, p.Repos[0].Path)
		if err != nil {
			return r, clients, err
		}
		clients = append(clients, state.client)
		if state.step.BeforeSymbol != "" {
			req := request{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": state.step.BeforeSymbol}, "options": map[string]any{"context_lines": 0}}}
			raw, identity, readErr := state.client.callWithIdentity(ctx, "tools/call", map[string]any{"name": req.Tool, "arguments": dispatchArgs(req, p.Repos[0])})
			if readErr == nil {
				readErr = checkToolResult(raw)
			}
			proven := readErr == nil && editProbePresenceProof(raw, p.Repos[0].Corpus, state.step.BeforeSymbol)
			if err := state.emit(map[string]any{"event": "before_presence_admission", "symbol": state.step.BeforeSymbol, "wire_id": identity.ID, "write_started": identity.WriteStarted, "finished": identity.Finished, "payload": raw, "proven": proven}); err != nil {
				return r, clients, err
			}
			if !proven {
				return r, clients, fmt.Errorf("original symbol presence admission failed: %s: %v", state.step.BeforeSymbol, readErr)
			}
			state.absenceBefore = map[string]bool{state.step.BeforeSymbol: true}
		}
	}
	phase.Metrics["edit_probe_session_setup_seconds"] = measured(time.Since(setup).Seconds(), "s")
	phase.Metrics["edit_probe_sessions"] = measured(float64(len(clients)), "sessions")
	phase.SessionModel = plan.SessionModel
	return r, clients, nil
}

func (s *editProbeState) emit(value any) error {
	started := time.Now()
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: encode: %v", errEditProbeEvidenceIncomplete, err)
	}
	s.sinkMu.Lock()
	defer func() { s.encodeNS += time.Since(started).Nanoseconds(); s.sinkMu.Unlock() }()
	if s.sinkErr != nil {
		return s.sinkErr
	}
	if s.rawClosing {
		return fmt.Errorf("%w: raw writer already closing", errEditProbeEvidenceIncomplete)
	}
	if s.rawAccepted+len(data)+1 > editProbeRawLimit {
		s.sinkErr = fmt.Errorf("%w: per-edit raw evidence limit exceeded", errEditProbeEvidenceIncomplete)
		return s.sinkErr
	}
	// Pending and in-flight payloads share the unchanged total byte cap. A
	// burst of small rows need not wait for the writer's next timeslice.
	s.rawPending = append(s.rawPending, data...)
	s.rawPending = append(s.rawPending, '\n')
	s.rawAccepted += len(data) + 1
	select {
	case s.rawWake <- struct{}{}:
	default: // Wake already pending; these bytes are still owned above.
	}
	return nil
}

// One owned writer per edit keeps persistence off response/probe timing paths.
// All pending/in-flight bytes share the per-edit cap; exhaustion fails evidence.
func (s *editProbeState) startRaw() {
	s.rawWake, s.rawDone = make(chan struct{}, 1), make(chan struct{})
	go func() {
		defer close(s.rawDone)
		failed := false
		for range s.rawWake {
			for {
				s.sinkMu.Lock()
				data := s.rawPending
				s.rawPending = nil
				s.sinkMu.Unlock()
				if len(data) == 0 {
					break
				}
				if failed {
					continue // Never credit bytes after a failed write.
				}
				start := time.Now()
				n, err := s.sink.Write(data)
				if err == nil && n != len(data) {
					err = io.ErrShortWrite
				}
				s.sinkMu.Lock()
				s.writeNS += time.Since(start).Nanoseconds()
				s.rawBytes += n
				if err != nil {
					s.sinkErr = errors.Join(s.sinkErr, fmt.Errorf("%w: raw write: %v", errEditProbeEvidenceIncomplete, err))
					failed = true
				}
				s.sinkMu.Unlock()
			}
		}
	}()
}

func (s *editProbeState) finishRaw() {
	if s.rawWake != nil {
		s.rawOnce.Do(func() {
			s.sinkMu.Lock()
			s.rawClosing = true
			close(s.rawWake)
			s.sinkMu.Unlock()
		})
		<-s.rawDone
	}
}

func editProbeOffset(at, origin time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at.Sub(origin).Nanoseconds()
}

type editProbeCaller struct {
	runtime *editProbeRuntime
	state   *editProbeState
	client  *socketClient
	purpose string
	request request
	last    wireCallIdentity
	role    string
}

func (c *editProbeCaller) responseReturnedAt() time.Time { return c.last.Finished }

func (c *editProbeCaller) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var onWriteStart func(time.Time)
	if c.purpose == "edit" {
		onWriteStart = func(at time.Time) {
			c.state.mu.Lock()
			if c.state.editWrite.IsZero() {
				c.state.editWrite = at
			}
			c.state.mu.Unlock()
		}
	}
	data, identity, err := c.client.callWithWriteObserver(ctx, method, params, onWriteStart)
	c.last = identity
	c.state.mu.Lock()
	if c.purpose == "edit" && c.state.editWrite.IsZero() && !identity.WriteStarted.IsZero() {
		c.state.editWrite = identity.WriteStarted
	}
	if strings.HasPrefix(c.purpose, "oracle:") && !identity.WriteStarted.IsZero() && (c.state.firstProbeWrite.IsZero() || identity.WriteStarted.Before(c.state.firstProbeWrite)) {
		c.state.firstProbeWrite = identity.WriteStarted
	}
	c.state.mu.Unlock()
	if err == nil {
		if expected := c.expectedAbsence(data, identity); expected != nil {
			err = expected
		}
	}
	row := map[string]any{"event": "rpc", "purpose": c.purpose, "tool": c.request.name(), "wire_id": identity.ID, "call_start_ns": editProbeOffset(identity.Started, c.runtime.origin), "write_start_ns": editProbeOffset(identity.WriteStarted, c.runtime.origin), "write_end_ns": editProbeOffset(identity.WriteFinished, c.runtime.origin), "reply_delivered_ns": editProbeOffset(identity.ReplyDelivered, c.runtime.origin), "call_end_ns": editProbeOffset(identity.Finished, c.runtime.origin), "output_mode": editProbeRequestOutput(c.request), "payload": data}
	if err != nil {
		row["error"] = err.Error()
		if expected, ok := err.(*editProbeExpectedAbsence); ok {
			row["expected_outcome"] = "symbol_absent"
			row["expected_symbol"] = expected.symbol
		}
	}
	if evidenceErr := c.state.emit(row); evidenceErr != nil {
		return data, errors.Join(err, evidenceErr)
	}
	return data, err
}

func editProbeRetrySleep(seed int64, ordinal, stream int) func(context.Context, time.Duration) error {
	rng := rand.New(rand.NewPCG(uint64(seed)^uint64(ordinal+1)*0x9e3779b97f4a7c15, uint64(stream+1)^0x8ebc6af09c88c6e3))
	return func(ctx context.Context, delay time.Duration) error {
		// Independent per logical call; backoff never consumes arrival RNG state.
		jitter := time.Duration(rng.Int64N(int64(delay/2) + 1))
		return waitContext(ctx, delay+jitter)
	}
}

func (r *editProbeRuntime) perform(ctx context.Context, s *editProbeState, client *socketClient, req request, purpose, role string, stream int) (json.RawMessage, wireCallIdentity, error) {
	data, identity, _, err := r.performObserved(ctx, s, client, req, purpose, role, stream)
	return data, identity, err
}

func (r *editProbeRuntime) performObserved(ctx context.Context, s *editProbeState, client *socketClient, req request, purpose, role string, stream int) (json.RawMessage, wireCallIdentity, []Attempt, error) {
	budget := r.options.Deadline
	if strings.HasPrefix(purpose, "oracle:") || purpose == "status" {
		budget = min(budget, 20*time.Second)
	}
	if end, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(end))
	}
	caller := &editProbeCaller{runtime: r, state: s, client: client, purpose: purpose, request: req, role: role}
	data, attempts, err := callEditProbeTool(ctx, caller, req, dispatchArgs(req, r.p.Repos[0]), role, 0, s.step.Ordinal, stream, budget, editProbeRetrySleep(r.options.Seed, s.step.Ordinal, stream))
	r.mu.Lock()
	r.phase.Attempts = append(r.phase.Attempts, attempts...)
	r.mu.Unlock()
	evidenceErr := s.emit(map[string]any{"event": "logical_call", "purpose": purpose, "tool": req.name(), "attempts": attempts})
	if evidenceErr != nil {
		return data, caller.last, attempts, errors.Join(err, evidenceErr)
	}
	return data, caller.last, attempts, err
}

func (s *editProbeState) noteProbeStart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := !s.started.IsZero() && !s.record.GraphTerminal
	if pending {
		s.record.PendingReads++
	}
	return pending
}

func (r *editProbeRuntime) activateProbeState(step editProbeStep, cancel context.CancelFunc) *editProbeState {
	s := r.states[step.Ordinal]
	s.mu.Lock()
	at := time.Since(r.origin).Nanoseconds()
	s.record.ActivatedNS, s.cancel = &at, cancel
	s.mu.Unlock()
	return s
}

func (r *editProbeRuntime) observeProbe(s *editProbeState, oracle editProbeOracle, sent time.Time, data json.RawMessage, identity wireCallIdentity, err error, pending bool) error {
	ended := identity.Finished
	if !identity.WriteStarted.IsZero() {
		sent = identity.WriteStarted
	}
	body, decodeErr := decodeEditProbeOutput(data, editProbeRequestOutput(oracle.call), oracle.call.name())
	absence, exactRefusal := err.(*editProbeExpectedAbsence)
	expectedAbsence := oracle.call.ExpectedAbsentSymbol != "" && exactRefusal && absence.symbol == r.p.Repos[0].Prefix+"/"+oracle.call.ExpectedAbsentSymbol
	correct := expectedAbsence || err == nil && decodeErr == nil && editProbeCorrect(oracle.name, body, r.p.Repos[0].Corpus, s.step, r.plan.Callee)
	fresh := decodeErr == nil && editProbeClaimsFresh(body)
	s.mu.Lock()
	if expectedAbsence {
		s.record.ExpectedRefusals++
	} else if err != nil {
		s.record.ProbeErrors++
	}
	if correct {
		if s.started.IsZero() || ended.IsZero() || ended.Before(s.started) {
			s.record.Errors = append(s.record.Errors, "marker visible before its own edit")
		} else if s.record.FirstCorrect[oracle.name].Value == nil {
			s.record.FirstCorrect[oracle.name] = measured(float64(ended.Sub(s.started))/float64(time.Millisecond), "ms")
		}
	}
	if !correct && fresh {
		if !s.acknowledged.IsZero() && !sent.Before(s.acknowledged) {
			s.record.StaleClaimedFresh = true
		} else {
			s.record.PrecommitFreshMismatches++
		}
	}
	s.mu.Unlock()
	return s.emit(map[string]any{"event": "probe", "oracle": oracle.name, "start_ns": editProbeOffset(sent, r.origin), "end_ns": editProbeOffset(ended, r.origin), "correct": correct, "claims_fresh": fresh, "receipt_observed_pending": pending})
}

// Activation only starts owned loops and returns. It never waits for a probe
// RPC/result; a blocked prober cannot shift this or any other edit's due time.
func (r *editProbeRuntime) activate(ctx context.Context, step editProbeStep) {
	probeCtx, cancel := context.WithCancel(ctx)
	s := r.activateProbeState(step, cancel)
	for index, oracle := range editProbeOracles(r.p.Repos[0].Corpus, step) {
		s.probes.Go(func() {
			ticker := time.NewTicker(editProbePeriod)
			defer ticker.Stop()
			for probeCtx.Err() == nil {
				sent := time.Now()
				pending := s.noteProbeStart()
				data, identity, err := r.perform(probeCtx, s, s.client, oracle.call, "oracle:"+oracle.name, "probe", 10+index)
				if err := r.observeProbe(s, oracle, sent, data, identity, err, pending); err != nil {
					cancel()
					return
				}
				select {
				case <-probeCtx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	}
}

func (s *editProbeState) stopProbes() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.probes.Wait()
}
func (r *editProbeRuntime) beginEdit(ctx context.Context, step editProbeStep) (*editProbeState, context.Context, context.CancelFunc, time.Time) {
	s := r.states[step.Ordinal]
	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, r.options.Deadline)
	s.mu.Lock()
	s.started = started
	ns := started.Sub(r.origin).Nanoseconds()
	late := ns - step.DueNS
	s.record.StartedNS = &ns
	s.record.StartLatenessNS = &late
	s.record.Outcome = "in_flight"
	if s.record.ActivatedNS == nil {
		s.record.Errors = append(s.record.Errors, "missing pre-edit activation")
	}
	s.mu.Unlock()
	return s, callCtx, cancel, started
}

func (r *editProbeRuntime) doEdit(ctx context.Context, step editProbeStep) {
	s, callCtx, cancel, started := r.beginEdit(ctx, step)
	defer s.stopProbes()
	defer cancel()
	ack, identity, err := r.perform(callCtx, s, r.editor, step.Request, "edit", "workload", 0)
	r.finishEditAfterACK(ctx, callCtx, s, step, started, ack, identity, err)
}

// The paired dispatcher can start this owned continuation on this build's own
// ACK, without a barrier on its partner. Standalone retains the same order.
func (r *editProbeRuntime) finishEditAfterACK(ctx, callCtx context.Context, s *editProbeState, step editProbeStep, started time.Time, ack json.RawMessage, identity wireCallIdentity, err error) {
	if err != nil {
		s.mu.Lock()
		s.record.Outcome = "edit_error"
		if ctx.Err() != nil {
			s.record.Outcome = "phase_censored"
		}
		s.record.Errors = append(s.record.Errors, err.Error())
		s.mu.Unlock()
		return
	}
	body, err := decodeEditProbeOutput(ack, r.plan.Mode, r.plan.Tool)
	if err != nil {
		s.mu.Lock()
		s.record.Outcome = "invalid_ack"
		s.record.Errors = append(s.record.Errors, err.Error())
		s.mu.Unlock()
		return
	}
	receipt, _ := body["mutation_receipt"].(string)
	var move *editProbeMoveReferences
	var batch *editProbeBatchReferences
	var batchOffered []byte
	if r.plan.Tool == "edit.batch" {
		transaction, offered, requestErr := editProbeBatchIntent(step.Request)
		if requestErr == nil {
			refs, ackErr := decodeEditProbeBatchAcknowledgement(body, transaction, r.p.Repos[0].File, filepath.ToSlash(filepath.Join(r.p.Repos[0].Prefix, r.plan.File)), offered)
			requestErr = ackErr
			if ackErr == nil {
				batch = &refs
				batchOffered = offered
			}
		}
		if requestErr != nil {
			s.mu.Lock()
			s.record.Outcome = "invalid_ack"
			s.record.Errors = append(s.record.Errors, requestErr.Error())
			s.mu.Unlock()
			return
		}
	}
	if step.Relocated {
		refs, moveErr := decodeEditProbeMoveAcknowledgement(body, r.p.Repos[0].Prefix+"/"+step.BeforeSymbol, r.p.Repos[0].Prefix+"/"+step.TargetSymbol, r.movePaths())
		if moveErr != nil {
			s.mu.Lock()
			s.record.Outcome = "invalid_ack"
			s.record.Errors = append(s.record.Errors, moveErr.Error())
			s.mu.Unlock()
			return
		}
		move = &refs
	}
	s.mu.Lock()
	s.record.Receipt = receipt
	if batch != nil {
		s.record.Batch = batch
		if batch.TransactionError != "" {
			s.record.Errors = append(s.record.Errors, batch.TransactionError)
		}
		s.record.Receipt = ""
		s.record.MutationID = ""
	}
	if move != nil {
		s.record.Move = move
		s.record.Receipt, s.record.MutationID = "", ""
		s.record.Accounting = editProbeAccounting{State: "not_applicable_direct_move"}
	}
	if receipt == "" && batch == nil && move == nil {
		s.record.Errors = append(s.record.Errors, "ACK mutation receipt unavailable")
	} else if !identity.Finished.IsZero() {
		s.record.Ack = measured(float64(identity.Finished.Sub(started))/float64(time.Millisecond), "ms")
		s.acknowledged = identity.Finished
	}
	s.mu.Unlock()
	// Immediate same editing session, with no sleep/receipt/freshness barrier.
	readbackKinds := []string{"file", "source"}
	if step.BeforeSymbol != "" && !step.Deleted && step.TargetSymbol == "" {
		readbackKinds = append(readbackKinds, "old_source_absent")
	}
	if step.Relocated {
		readbackKinds = append(readbackKinds, "old_source_absent", "source_file_after_move")
	}
	for index, kind := range readbackKinds {
		target := map[string]any{"file": r.plan.File}
		if kind == "file" && step.Relocated {
			target["file"] = step.DestinationFile
		}
		if kind == "source" {
			target = map[string]any{"symbol": r.plan.File + "::" + step.Marker}
			if step.TargetSymbol != "" {
				target = map[string]any{"symbol": step.TargetSymbol}
			}
		}
		req := request{Tool: "read", Operation: kind, Arguments: map[string]any{"operation": kind, "target": target}}
		if kind == "source" && step.TargetSymbol != "" {
			req.Arguments["options"] = map[string]any{"context_lines": 0}
		}
		if kind == "old_source_absent" || kind == "source" && step.Deleted {
			req.Operation = "source"
			req.ExpectedAbsentSymbol = step.BeforeSymbol
			req.Arguments = map[string]any{"operation": "source", "target": map[string]any{"symbol": step.BeforeSymbol}}
		}
		if kind == "source_file_after_move" {
			req.Operation = "file"
			req.Arguments["operation"] = "file"
		}
		req = editProbeWithOutput(req, r.plan.Mode)
		begin := time.Now()
		raw, identity, readErr := r.perform(callCtx, s, r.editor, req, "readback:"+kind, "readback", 100+index)
		payload, decodeErr := decodeEditProbeOutput(raw, editProbeRequestOutput(req), req.name())
		back := editProbeReadback{Kind: kind, Latency: unavailable("ms", "response endpoint unavailable"), Correct: readErr == nil && decodeErr == nil && editProbeCorrect(kind, payload, r.p.Repos[0].Corpus, step, r.plan.Callee)}
		if !identity.Finished.IsZero() {
			back.Latency = measured(float64(identity.Finished.Sub(begin))/float64(time.Millisecond), "ms")
		}
		absence, exactRefusal := readErr.(*editProbeExpectedAbsence)
		if req.ExpectedAbsentSymbol != "" && exactRefusal && absence.symbol == r.p.Repos[0].Prefix+"/"+req.ExpectedAbsentSymbol {
			back.Correct = true
			back.ExpectedOutcome = "symbol_absent"
			back.Refusal = absence.message
		} else {
			if readErr != nil {
				back.Error = readErr.Error()
			}
			if decodeErr != nil {
				back.Error = decodeErr.Error()
			}
		}
		s.mu.Lock()
		s.record.Readbacks = append(s.record.Readbacks, back)
		if !back.Correct {
			s.record.Errors = append(s.record.Errors, "immediate "+kind+" readback incorrect")
		}
		s.mu.Unlock()
	}
	if move != nil {
		r.collectMoveEvidence(callCtx, s, *move, identity.ID)
	} else if batch != nil {
		r.collectBatchEvidence(callCtx, s, *batch, batchOffered)
	} else {
		options := map[string]any{"mutation_id": step.MutationID}
		if receipt != "" {
			options = map[string]any{"receipt": receipt}
		}
		statusReq := request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": options}}
		for callCtx.Err() == nil {
			raw, _, statusErr := r.perform(callCtx, s, s.client, statusReq, "status", "probe", 200)
			if errors.Is(statusErr, errEditProbeEvidenceIncomplete) {
				break
			}
			if statusErr == nil {
				if status, decodeErr := decodeEditProbeResult(raw); decodeErr == nil {
					accounting := checkEditProbeAccountingForTool(status, receipt, r.plan.Tool)
					s.mu.Lock()
					s.record.Accounting = accounting
					s.record.GraphTerminal = status["graph_status_terminal"] == true
					all := true
					for _, value := range s.record.FirstCorrect {
						if value.Value == nil {
							all = false
						}
					}
					terminal := s.record.GraphTerminal
					if terminal && status["graph_status"] != "fresh" {
						s.record.Errors = append(s.record.Errors, "terminal graph status is not fresh")
					}
					s.mu.Unlock()
					if terminal && status["graph_status"] != "fresh" {
						break
					}
					if status["mutation_id"] != step.MutationID {
						s.mu.Lock()
						s.record.Errors = append(s.record.Errors, "status mutation identity mismatch")
						s.mu.Unlock()
						break
					}
					if editProbeFinished(terminal, all, r.requireAccounting, accounting) {
						break
					}
				}
			}
			if waitContext(callCtx, editProbePeriod) != nil {
				break
			}
		}
	}
	s.mu.Lock()
	if s.firstProbeWrite.IsZero() || s.editWrite.IsZero() || !s.firstProbeWrite.Before(s.editWrite) {
		s.record.Errors = append(s.record.Errors, "no actual prober wire start before edit wire")
	}
	for name, value := range s.record.FirstCorrect {
		if value.Value == nil {
			s.record.Errors = append(s.record.Errors, "first-correct oracle unavailable: "+name)
		}
	}
	if !s.record.GraphTerminal && (move == nil || r.requireAccounting || !movePublicFactsComplete(s.record)) {
		s.record.Errors = append(s.record.Errors, "graph receipt terminal unavailable")
	}
	if r.requireAccounting && move != nil && (s.record.MoveAccounting == nil || s.record.MoveAccounting.State != "observed_move_windows_checks_pass") {
		s.record.Errors = append(s.record.Errors, "move shared A/per-file write/direct accounting unavailable")
	}
	if r.requireAccounting && move == nil && s.record.Accounting.State != "observed_single_window_checks_pass" {
		s.record.Errors = append(s.record.Errors, "A/P/B accounting unavailable: "+strings.Join(s.record.Accounting.Reasons, "; "))
	}
	if s.record.StaleClaimedFresh {
		s.record.Errors = append(s.record.Errors, "stale answer explicitly claimed fresh after acknowledgement")
	}
	if ctx.Err() != nil {
		s.record.Outcome = "phase_censored"
	} else if callCtx.Err() != nil {
		s.record.Outcome = "product_deadline"
	} else if len(s.record.Errors) == 0 {
		s.record.Outcome = "complete"
	} else {
		s.record.Outcome = "diagnostic_failure"
	}
	s.mu.Unlock()
}

func (r *editProbeRuntime) run(ctx context.Context) error {
	budget, err := r.plan.budget(r.options.Deadline, 1)
	if err != nil {
		return err
	}
	captureReceipt(ctx, r.p, r.editor, r.phase)
	before, statsErr := r.stats(ctx)
	r.phase.EditProbe.StatsBefore = before
	if statsErr != nil {
		return statsErr
	}
	r.origin = time.Now()
	r.phase.Started = r.origin
	measuredCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	err = runEditProbeArrivals(measuredCtx, r.plan, r.origin, r.activate, r.doEdit, waitContext)
	finished := r.finish()
	after, statsErr := r.stats(measuredCtx)
	r.phase.EditProbe.StatsAfter = after
	return errors.Join(err, finished, statsErr)
}

func (r *editProbeRuntime) finish() error {
	var failures []error
	if r.finalized {
		return nil
	}
	r.finalized = true
	r.phase.EditProbe.Records = nil
	for _, s := range r.states {
		s.stopProbes()
		s.finishRaw()
		s.mu.Lock()
		s.pairedContext = nil
		if s.record.Outcome == "not_started" {
			s.record.Outcome = "phase_censored"
			s.record.Errors = append(s.record.Errors, "planned edit never dispatched")
		}
		if s.record.Outcome == "complete" && len(s.record.Errors) != 0 {
			// A coupled partner can lose evidence after this continuation's
			// own result completed, but before all paired probes are joined.
			s.record.Outcome = "diagnostic_failure"
		}
		record := s.record
		s.mu.Unlock()
		s.sinkMu.Lock()
		sinkErr := s.sinkErr
		record.EvidenceWriteMS = float64(s.writeNS) / 1e6
		record.EvidenceEncodeMS = float64(s.encodeNS) / 1e6
		record.EvidenceBytes, record.EvidenceLimit = s.rawBytes, editProbeRawLimit
		s.sinkMu.Unlock()
		if sinkErr != nil {
			record.Outcome = "harness_evidence_incomplete"
			record.Errors = append(record.Errors, sinkErr.Error())
		}
		r.phase.EditProbe.Records = append(r.phase.EditProbe.Records, record)
		if record.Outcome != "complete" || len(record.Errors) > 0 {
			failures = append(failures, fmt.Errorf("edit %d: %s (%s)", record.Ordinal, record.Outcome, strings.Join(record.Errors, "; ")))
		}
	}
	for _, step := range r.plan.Steps[len(r.states):] {
		record := newEditProbeRecord(r.p.Repos[0].Corpus, step)
		r.phase.EditProbe.Records = append(r.phase.EditProbe.Records, record)
		failures = append(failures, fmt.Errorf("edit %d: setup never completed", step.Ordinal))
	}
	finishEditProbeMetrics(r.phase)
	return errors.Join(failures...)
}

// The typed offered records retain failed/missing/censored ACKs. They must not
// be converted to the legacy scalar WriteMS zero or a never-visible success.
func finishEditProbeMetrics(phase *Phase) {
	var ack, visible []float64
	phase.NeverSeen, phase.Censored = 0, 0
	for _, record := range phase.EditProbe.Records {
		if record.Ack.Value != nil {
			ack = append(ack, *record.Ack.Value)
		}
		if value := record.FirstCorrect["search_visible"].Value; value != nil {
			visible = append(visible, *value)
		}
		if record.Outcome == "phase_censored" {
			phase.Censored++
		}
		if record.Outcome == "product_deadline" && record.Ack.Value != nil && record.FirstCorrect["search_visible"].Value == nil {
			phase.NeverSeen++
		}
	}
	phase.EditLatency, phase.Visibility = distribution(ack, "ms"), distribution(visible, "ms")
}

func (r *editProbeRuntime) close() error {
	var failures []error
	if !r.finalized {
		failures = append(failures, r.finish())
	}
	for _, s := range r.states {
		s.stopProbes()
		s.finishRaw()
		if s.sink != nil {
			failures = append(failures, s.sink.Close())
		}
	}
	return errors.Join(failures...)
}

func (r *editProbeRuntime) restore(ctx context.Context) error {
	// Do not race unknown/uncompleted source effects with a cleanup rewrite.
	for _, record := range r.phase.EditProbe.Records {
		if (record.Receipt == "" && record.Batch == nil && record.Move == nil) || (!record.GraphTerminal && (record.Move == nil || r.requireAccounting || !movePublicFactsComplete(record))) {
			return errors.New("restore batch unavailable while mutation outcome is unknown/pending")
		}
	}
	current, err := os.ReadFile(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(r.plan.File)))
	if err != nil {
		return err
	}
	changes := []any{map[string]any{"op": "edit_file", "path": r.p.Repos[0].Prefix + "/" + r.plan.File, "old_string": string(current), "new_string": string(r.original)}}
	if r.plan.Tool == "refactor.move" {
		destination := r.p.Repos[0].Corpus.EditDestination
		currentDestination, readErr := os.ReadFile(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(destination)))
		if readErr != nil {
			return readErr
		}
		changes = append(changes, map[string]any{"op": "edit_file", "path": r.p.Repos[0].Prefix + "/" + destination, "old_string": string(currentDestination), "new_string": string(r.moveOriginal)})
	}
	req := request{Tool: "edit", Operation: "batch", Arguments: map[string]any{"operation": "batch", "changes": changes, "options": map[string]any{"transaction_id": fmt.Sprintf("load-restore-%016x", uint64(r.options.Seed))}, "output": map[string]any{"format": "json", "max_bytes": 0}}}
	start := time.Now()
	data, _, err := callEditProbeTool(ctx, r.editor, req, req.Arguments, "cleanup", 0, len(r.states), 0, r.options.Deadline, editProbeRetrySleep(r.options.Seed, len(r.states), 0))
	r.phase.EditProbe.RestoreMS = measured(float64(time.Since(start))/float64(time.Millisecond), "ms")
	r.phase.EditProbe.Restore = data
	if err != nil {
		return err
	}
	restored, err := os.ReadFile(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(r.plan.File)))
	if err != nil {
		return err
	}
	if string(restored) != string(r.original) {
		return errors.New("timed public restore batch did not restore original bytes")
	}
	if r.plan.Tool == "refactor.move" {
		destination, readErr := os.ReadFile(filepath.Join(r.p.Repos[0].Path, filepath.FromSlash(r.p.Repos[0].Corpus.EditDestination)))
		if readErr != nil {
			return readErr
		}
		if string(destination) != string(r.moveOriginal) {
			return errors.New("timed public restore batch did not restore destination bytes")
		}
	}
	return nil
}

func (r *editProbeRuntime) stats(ctx context.Context) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := r.editor.call(ctx, "resources/read", map[string]any{"uri": "gortex://stats"})
	if err != nil {
		return data, err
	}
	return textPayload(data, "contents")
}
