package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

const pairedOwnACKTransactionMethod = "edit_own_ack_file_source_v1"

// One object belongs to one build's one offered edit. It never locks another
// editor stream or waits for the partner's ACK/publication/status controls.
type pairedOwnACKTransaction struct {
	mu              sync.Mutex
	once            sync.Once
	done            chan struct{}
	timing          pairedOwnACKTiming
	ended           time.Time
	outcome         string
	editError       string
	firstReadWrites map[string]time.Time
	readWriteCounts map[string]int
}

type pairedOwnACKTiming struct {
	EditStarted            time.Time          `json:"edit_started"`
	ACKStarted             time.Time          `json:"ack_logical_call_started"`
	ACKFinalAttemptStarted time.Time          `json:"ack_final_attempt_started"`
	ACKFinished            time.Time          `json:"ack_rpc_finished"`
	ACKLatency             Metric             `json:"ack_logical_latency_ms"`
	OwnACKDispatchDelay    Metric             `json:"own_ack_to_file_dispatch_ms"`
	Reads                  []pairedOwnACKRead `json:"reads"`
}

type pairedOwnACKRead struct {
	Kind                string    `json:"kind"`
	RequestHash         string    `json:"request_sha256"`
	Started             time.Time `json:"started"`
	WriteStarted        time.Time `json:"final_wire_write_started"`
	FirstWriteStarted   time.Time `json:"first_wire_write_started"`
	FinalAttemptStarted time.Time `json:"final_attempt_started"`
	FinalAttemptLatency Metric    `json:"final_attempt_latency_ms"`
	WireAttempts        int       `json:"wire_attempts"`
	Ended               time.Time `json:"ended"`
	WireID              int64     `json:"wire_id"`
	Latency             Metric    `json:"logical_rpc_latency_ms"`
	Outcome             string    `json:"outcome"`
	Code                int       `json:"code,omitempty"`
	Error               string    `json:"error,omitempty"`
}

func newPairedOwnACKTransaction(started time.Time) *pairedOwnACKTransaction {
	return &pairedOwnACKTransaction{done: make(chan struct{}), firstReadWrites: make(map[string]time.Time), readWriteCounts: make(map[string]int), timing: pairedOwnACKTiming{EditStarted: started,
		ACKLatency: unavailable("ms", "ACK endpoint missing"), OwnACKDispatchDelay: unavailable("ms", "file dispatch after own ACK not observed")}}
}

func (t *pairedOwnACKTransaction) acknowledge(identity wireCallIdentity, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timing.ACKStarted, t.timing.ACKFinished = t.timing.EditStarted, identity.Finished
	t.timing.ACKFinalAttemptStarted = identity.Started
	if !identity.Finished.IsZero() && !identity.Finished.Before(t.timing.EditStarted) {
		t.timing.ACKLatency = measured(float64(identity.Finished.Sub(t.timing.EditStarted))/float64(time.Millisecond), "ms")
	}
	if err != nil {
		t.editError = err.Error()
	}
}

func (t *pairedOwnACKTransaction) observeReadWrite(kind string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.firstReadWrites[kind].IsZero() {
		t.firstReadWrites[kind] = at
	}
	t.readWriteCounts[kind]++
}

func (t *pairedOwnACKTransaction) observeRead(req request, begin time.Time, identity wireCallIdentity, back editProbeReadback, readErr, contextErr error) {
	hash, err := hashJSON(req)
	row := pairedOwnACKRead{Kind: back.Kind, RequestHash: hash, Started: begin, WriteStarted: identity.WriteStarted, FinalAttemptStarted: identity.Started, Ended: identity.Finished,
		WireID: identity.ID, Latency: back.Latency, FinalAttemptLatency: unavailable("ms", "final RPC attempt endpoints unavailable"), Outcome: "success", Error: back.Error}
	if !identity.Started.IsZero() && !identity.Finished.Before(identity.Started) {
		row.FinalAttemptLatency = measured(float64(identity.Finished.Sub(identity.Started))/float64(time.Millisecond), "ms")
	}
	var refusal *rpcError
	switch {
	case errors.Is(contextErr, context.Canceled):
		row.Outcome = "phase_censored"
	case errors.Is(contextErr, context.DeadlineExceeded):
		row.Outcome = "deadline_censored"
	case errors.As(readErr, &refusal):
		row.Outcome = "refusal"
		row.Code = refusal.Code
	case readErr != nil || err != nil:
		row.Outcome = "error"
	case !back.Correct || row.Latency.Value == nil:
		row.Outcome = "incorrect_readback"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	row.FirstWriteStarted = t.firstReadWrites[back.Kind]
	row.WireAttempts = t.readWriteCounts[back.Kind]
	if back.Kind == "file" && !t.timing.ACKFinished.IsZero() && !row.FirstWriteStarted.IsZero() && !row.FirstWriteStarted.Before(t.timing.ACKFinished) {
		t.timing.OwnACKDispatchDelay = measured(float64(row.FirstWriteStarted.Sub(t.timing.ACKFinished))/float64(time.Millisecond), "ms")
	}
	t.timing.Reads = append(t.timing.Reads, row)
}

func (t *pairedOwnACKTransaction) finish(phase, call context.Context, ended time.Time, correct bool) {
	t.once.Do(func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.ended, t.outcome = ended, "incorrect_readback"
		switch {
		case phase.Err() != nil:
			t.outcome = "phase_censored"
		case call.Err() != nil:
			t.outcome = "product_deadline"
		case t.editError != "":
			t.outcome = "edit_error"
		case correct && len(t.timing.Reads) == 2 && t.timing.Reads[0].Kind == "file" && t.timing.Reads[1].Kind == "source" &&
			t.timing.Reads[0].Outcome == "success" && t.timing.Reads[1].Outcome == "success" &&
			!t.timing.ACKFinished.IsZero() && !ended.IsZero():
			t.outcome = "success"
		}
		close(t.done)
	})
}

func (t *pairedOwnACKTransaction) wait(phase, call context.Context) {
	select {
	case <-t.done:
	case <-call.Done():
		t.finish(phase, call, time.Time{}, false)
	}
}

func (t *pairedOwnACKTransaction) sample(ack pairedSample) pairedSample {
	t.mu.Lock()
	defer t.mu.Unlock()
	timing := t.timing
	timing.Reads = append([]pairedOwnACKRead(nil), timing.Reads...)
	ack.Transaction = &timing
	ack.Started, ack.Ended, ack.Outcome = t.timing.EditStarted, t.ended, t.outcome
	ack.Latency = unavailable("ms", "whole transaction failed, censored, or incomplete")
	if t.outcome == "success" && !t.ended.Before(t.timing.EditStarted) {
		ack.Latency = measured(float64(t.ended.Sub(t.timing.EditStarted))/float64(time.Millisecond), "ms")
	}
	return ack
}

// The edit's original paired record remains its separate ACK measurement.
// These extra records share that transaction's order/ordinal, rather than
// pretending that individual reads rotated independently at RPC boundaries.
func (c *pairedCoordinator) recordOwnACKTransactions(edit pairedRecord, transactions map[string]*pairedOwnACKTransaction) {
	whole := edit
	whole.Tuple.Role, whole.Tuple.Tool = "transaction", pairedOwnACKTransactionMethod
	whole.TupleHash, _ = hashJSON(whole.Tuple)
	whole.Samples = make(map[string]pairedSample)
	for _, label := range c.labels {
		if t := transactions[label]; t != nil {
			whole.Samples[label] = t.sample(edit.Samples[label])
		} else {
			sample := edit.Samples[label]
			sample.Outcome = "harness_incomplete"
			sample.Latency = unavailable("ms", "transaction could not be prepared")
			whole.Samples[label] = sample
		}
	}
	records := []pairedRecord{whole}
	for slot, kind := range []string{"file", "source"} {
		r := pairedRecord{Tuple: edit.Tuple, Order: edit.Order, Samples: make(map[string]pairedSample)}
		r.Tuple.Role, r.Tuple.Tool, r.Tuple.Slot = "transaction_readback", "read."+kind, 100+slot
		r.Tuple.RequestHash = "" // explicit unknown when neither build dispatched this read
		for _, label := range c.labels {
			sample := whole.Samples[label]
			sample.Transaction = nil
			sample.Started, sample.Ended = time.Time{}, time.Time{}
			sample.RequestHash = ""
			sample.Latency = unavailable("ms", "read not dispatched after failed or censored edit")
			if tx := whole.Samples[label].Transaction; tx != nil {
				for _, read := range tx.Reads {
					if read.Kind == kind {
						sample.Started, sample.Ended, sample.RequestHash = read.Started, read.Ended, read.RequestHash
						sample.Latency, sample.Outcome, sample.Error, sample.Code = read.Latency, read.Outcome, read.Error, read.Code
						if r.Tuple.RequestHash == "" {
							r.Tuple.RequestHash = read.RequestHash
						}
					}
				}
			}
			r.Samples[label] = sample
		}
		r.TupleHash, _ = hashJSON(r.Tuple)
		records = append(records, r)
	}
	c.mu.Lock()
	c.records = append(c.records, records...)
	c.mu.Unlock()
}

type pairedTransactionSummary struct {
	Transactions int            `json:"transactions"`
	Outcomes     map[string]int `json:"outcomes"`
	Latency      Distribution   `json:"whole_transaction_latency_ms"`
}

func ownACKTransactionSummaries(labels []string, records []pairedRecord) map[string]pairedTransactionSummary {
	result := make(map[string]pairedTransactionSummary)
	for _, label := range labels {
		summary := pairedTransactionSummary{Outcomes: make(map[string]int)}
		var values []float64
		for _, r := range records {
			if r.Tuple.Role == "transaction" {
				s := r.Samples[label]
				summary.Transactions++
				summary.Outcomes[s.Outcome]++
				if s.Outcome == "success" && s.Latency.Value != nil {
					values = append(values, *s.Latency.Value)
				}
			}
		}
		summary.Latency = distribution(values, "ms")
		if len(values) != summary.Transactions {
			// Keep the completed count, but never offer a success-only p95 as
			// the latency of this population when failures/censors remain.
			summary.Latency = distribution(nil, "ms")
			summary.Latency.Count = len(values)
		}
		result[label] = summary
	}
	return result
}
