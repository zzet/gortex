package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

// An immediate readback's typed disposition explains why a read was or was
// not correct. It never changes Correct, the record's errors or its outcome:
// a truthful pending answer and wrong content both stay incorrect, but they
// are no longer indistinguishable in the report.
type editProbeReadbackDisposition struct {
	Kind                string         `json:"kind"`
	ErrorCode           string         `json:"error_code,omitempty"`
	Retriable           *bool          `json:"retriable,omitempty"`
	RetryScope          string         `json:"retry_scope,omitempty"`
	Reason              string         `json:"reason,omitempty"`
	RPCCode             int            `json:"rpc_code,omitempty"`
	FreshnessState      string         `json:"freshness_state,omitempty"`
	FreshnessReason     string         `json:"freshness_reason,omitempty"`
	FreshnessComplete   *bool          `json:"freshness_complete,omitempty"`
	NodeCount           *int           `json:"node_count,omitempty"`
	ProjectionPredicate map[string]any `json:"projection_predicate,omitempty"`
}

// Dispositions, in the order they are decided.
const (
	editProbeReadbackExpectedAbsence = "expected_absence"
	editProbeReadbackTypedRefusal    = "typed_refusal"
	editProbeReadbackUntypedError    = "untyped_tool_error"
	editProbeReadbackRPCError        = "rpc_error"
	editProbeReadbackDeadline        = "deadline"
	editProbeReadbackTransportError  = "transport_error"
	editProbeReadbackUndecodable     = "undecodable_answer"
	editProbeReadbackAnswer          = "answer"
)

func editProbeReadbackDispositionOf(kind string, raw json.RawMessage, payload map[string]any, readErr, decodeErr error) *editProbeReadbackDisposition {
	if _, expected := readErr.(*editProbeExpectedAbsence); expected {
		return &editProbeReadbackDisposition{Kind: editProbeReadbackExpectedAbsence}
	}
	if readErr != nil {
		var remote *rpcError
		if code, retriable, data, typed := editProbeStructuredRefusal(raw); typed {
			out := &editProbeReadbackDisposition{Kind: editProbeReadbackTypedRefusal, ErrorCode: code, Retriable: &retriable}
			out.RetryScope, _ = data["retry_scope"].(string)
			out.Reason, _ = data["reason"].(string)
			if errors.As(readErr, &remote) {
				out.ProjectionPredicate = editProbeSummaryRefusalTrace(kind, remote.Data, raw)
			}
			return out
		}
		if errors.As(readErr, &remote) {
			if editProbeToolError(raw) {
				return &editProbeReadbackDisposition{Kind: editProbeReadbackUntypedError}
			}
			out := &editProbeReadbackDisposition{Kind: editProbeReadbackRPCError, RPCCode: remote.Code}
			out.Reason, _ = remote.Data["outcome"].(string)
			if retryable, known := remote.Data["retryable"].(bool); known {
				out.Retriable = &retryable
			}
			return out
		}
		if errors.Is(readErr, context.DeadlineExceeded) || errors.Is(readErr, context.Canceled) {
			return &editProbeReadbackDisposition{Kind: editProbeReadbackDeadline}
		}
		return &editProbeReadbackDisposition{Kind: editProbeReadbackTransportError}
	}
	if decodeErr != nil {
		return &editProbeReadbackDisposition{Kind: editProbeReadbackUndecodable}
	}
	out := &editProbeReadbackDisposition{Kind: editProbeReadbackAnswer}
	out.FreshnessState, out.FreshnessReason, out.FreshnessComplete = editProbeAnswerFreshness(payload)
	if nodes, ok := payload["nodes"].([]any); ok {
		count := len(nodes)
		out.NodeCount = &count
	}
	return out
}

// Only the product's StructuredError text body is typed; prose errors are not.
func editProbeStructuredRefusal(raw json.RawMessage) (string, bool, map[string]any, bool) {
	if !editProbeToolError(raw) {
		return "", false, nil, false
	}
	payload, err := textPayload(raw, "content")
	if err != nil {
		return "", false, nil, false
	}
	var body struct {
		Code      string         `json:"error_code"`
		Retriable bool           `json:"retriable"`
		Data      map[string]any `json:"data"`
	}
	if json.Unmarshal(payload, &body) != nil || body.Code == "" {
		return "", false, nil, false
	}
	return body.Code, body.Retriable, body.Data, true
}

func editProbeToolError(raw json.RawMessage) bool {
	var envelope struct {
		IsError bool `json:"isError"`
	}
	return len(raw) != 0 && json.Unmarshal(raw, &envelope) == nil && envelope.IsError
}

// Freshness is read where producers place it: a freshness object, a decoded
// compact/TOON rider, or a scalar label. Absent fields stay absent.
func editProbeAnswerFreshness(body map[string]any) (string, string, *bool) {
	freshness, _ := body["freshness"].(map[string]any)
	state := ""
	for _, value := range []any{freshness["graph_freshness"], freshness["freshness_state"], freshness["state"], body["graph_freshness"], body["freshness_state"], body["freshness"]} {
		if text, ok := value.(string); ok && text != "" {
			state = text
			break
		}
	}
	reason, _ := freshness["reason"].(string)
	var complete *bool
	if value, ok := freshness["complete"].(bool); ok {
		complete = &value
	}
	return state, reason, complete
}

// A concurrent same-file commit witness lists other edits of the same file
// whose own edit RPC overlapped this readback's wire window. ACKInWindow marks
// the edits whose acknowledgement returned between this read's send and
// receive; the others were in flight and may have committed inside it.
type editProbeCommitWitness struct {
	State   string                      `json:"state"`
	Commits []editProbeConcurrentCommit `json:"commits,omitempty"`
}

type editProbeConcurrentCommit struct {
	Ordinal     int    `json:"ordinal"`
	Editor      int    `json:"editor"`
	CommitSeq   uint64 `json:"commit_seq,omitempty"`
	Receipt     string `json:"receipt,omitempty"`
	EditWriteNS *int64 `json:"edit_write_ns,omitempty"`
	ACKNS       *int64 `json:"ack_ns,omitempty"`
	ACKInWindow bool   `json:"ack_in_window"`
}

type editProbeCommitCandidate struct {
	ordinal, editor         int
	target                  string
	editWrite, acknowledged time.Time
	commitSeq               uint64
	receipt                 string
}

func editProbeCommitWitnessFor(self int, target string, sent, received, origin time.Time, candidates []editProbeCommitCandidate) *editProbeCommitWitness {
	if target == "" || sent.IsZero() || received.IsZero() || received.Before(sent) {
		return &editProbeCommitWitness{State: "unavailable"}
	}
	out := &editProbeCommitWitness{State: "none_observed"}
	offset := func(at time.Time) *int64 {
		if at.IsZero() {
			return nil
		}
		ns := at.Sub(origin).Nanoseconds()
		return &ns
	}
	for _, other := range candidates {
		if other.ordinal == self || other.target != target || other.editWrite.IsZero() || other.editWrite.After(received) {
			continue
		}
		if !other.acknowledged.IsZero() && other.acknowledged.Before(sent) {
			continue // Acknowledged before this read was sent.
		}
		inWindow := !other.acknowledged.IsZero() && !other.acknowledged.After(received)
		out.Commits = append(out.Commits, editProbeConcurrentCommit{Ordinal: other.ordinal, Editor: other.editor, CommitSeq: other.commitSeq, Receipt: other.receipt, EditWriteNS: offset(other.editWrite), ACKNS: offset(other.acknowledged), ACKInWindow: inWindow})
	}
	if len(out.Commits) != 0 {
		out.State = "observed"
		slices.SortFunc(out.Commits, func(a, b editProbeConcurrentCommit) int { return a.Ordinal - b.Ordinal })
	}
	return out
}

func (r *editProbeRuntime) editTarget(step editProbeStep) string {
	if r.p == nil {
		return "" // No registered checkout: the witness is unavailable.
	}
	repo, err := r.repoFor(step)
	if err != nil {
		return ""
	}
	file := repo.Corpus.File
	if step.File != "" {
		file = step.File
	}
	return repo.Path + "\x00" + file
}

// Run after every edit has ended. Only report attribution changes here.
func (r *editProbeRuntime) attributeConcurrentCommits() {
	candidates := make([]editProbeCommitCandidate, 0, len(r.states))
	targets := make([]string, len(r.states))
	for i, s := range r.states {
		targets[i] = r.editTarget(s.step)
		s.mu.Lock()
		candidates = append(candidates, editProbeCommitCandidate{ordinal: s.step.Ordinal, editor: s.step.Editor, target: targets[i], editWrite: s.editWrite, acknowledged: s.acknowledged, commitSeq: s.record.ACKCommitSeq, receipt: s.record.Receipt})
		s.mu.Unlock()
	}
	for i, s := range r.states {
		s.mu.Lock()
		for j := range s.record.Readbacks {
			back := &s.record.Readbacks[j]
			if back.sent.IsZero() && back.received.IsZero() {
				continue
			}
			back.ConcurrentCommits = editProbeCommitWitnessFor(s.step.Ordinal, targets[i], back.sent, back.received, r.origin, candidates)
		}
		s.mu.Unlock()
	}
}
