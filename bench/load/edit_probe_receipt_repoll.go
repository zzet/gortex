package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"time"
)

// editProbePostWorkloadReceipt is the one status read LOAD makes, after the
// workload and before the restore, of a receipt that was not graph-terminal
// when its edit's deadline ended. It is post-workload evidence only: the
// record's outcome, errors, in-window timings and workload tiers stay as the
// workload observed them. Only a terminal read lets the restore past the record.
type editProbePostWorkloadReceipt struct {
	// terminal: the receipt reached a terminal graph state. pending: it has
	// not. unavailable: no usable reply before the restore deadline, or a
	// reply for another checkout or mutation.
	State         string              `json:"state"`
	Reason        string              `json:"reason,omitempty"`
	GraphStatus   string              `json:"graph_status,omitempty"`
	MutationTiers *editProbeLiveTiers `json:"mutation_tiers,omitempty"`
	Attempts      int                 `json:"attempts"`
	PollMS        Metric              `json:"poll_ms"`
}

func (p *editProbePostWorkloadReceipt) terminal() bool {
	return p != nil && p.State == "terminal"
}

// editProbeReceiptPendingAtWorkloadEnd reports whether the ordinary receipt
// loop left this edit's receipt before its graph was terminal. Batches, moves,
// native writes and restarts keep their own evidence and are not read again.
func editProbeReceiptPendingAtWorkloadEnd(record editProbeRecord) bool {
	return record.Receipt != "" && !record.GraphTerminal && record.Batch == nil && record.BatchSet == nil &&
		record.Move == nil && record.NativeWrite == nil && record.Restart == nil
}

// repollPendingReceipts reads the status of every receipt the workload left
// pending, once each, concurrently and inside ctx, the restore's deadline. Each
// read uses the edit's fixed editor session (on a routed run, the one the
// restore writes that checkout through) and the workload's read budget. The
// prober session is not used: a probe canceled mid-write when the edit ended
// closes it for good.
func (r *editProbeRuntime) repollPendingReceipts(ctx context.Context) {
	records := r.phase.EditProbe.Records
	polls := make([]*editProbePostWorkloadReceipt, len(records))
	var wg sync.WaitGroup
	for i, record := range records {
		if !editProbeReceiptPendingAtWorkloadEnd(record) || record.Ordinal < 0 || record.Ordinal >= len(r.states) {
			continue
		}
		s := r.states[record.Ordinal]
		if s.step.Ordinal != record.Ordinal {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			polls[i] = r.repollReceipt(ctx, s, record)
		}()
	}
	wg.Wait()
	for i, poll := range polls {
		if poll != nil {
			records[i].PostWorkloadReceipt = poll
		}
	}
}

func (r *editProbeRuntime) repollReceipt(ctx context.Context, s *editProbeState, record editProbeRecord) *editProbePostWorkloadReceipt {
	poll := &editProbePostWorkloadReceipt{State: "unavailable", PollMS: unavailable("ms", "no status request sent")}
	repo, err := r.repoFor(s.step)
	if err != nil {
		poll.Reason = err.Error()
		return poll
	}
	client := r.editorFor(s.step)
	if client == nil || len(r.p.Repos) > 1 && filepath.Clean(client.cwd) != filepath.Clean(repo.Path) {
		poll.Reason = "no session for the edit's checkout"
		return poll
	}
	req := request{Tool: "change", Operation: "receipt", Arguments: map[string]any{"operation": "receipt", "options": map[string]any{"receipt": record.Receipt}}}
	started := time.Now()
	raw, attempts, err := callEditProbeTool(ctx, client, req, dispatchArgs(req, repo), "cleanup", 0, record.Ordinal, 0, min(r.options.Deadline, editProbeReadBudget), editProbeRetrySleep(r.options.Seed, record.Ordinal, 200))
	poll.PollMS = measured(float64(time.Since(started))/float64(time.Millisecond), "ms")
	poll.Attempts = len(attempts)
	if err != nil {
		poll.Reason = err.Error()
		return poll
	}
	return classifyEditProbePostWorkloadStatus(poll, record, raw)
}

// classifyEditProbePostWorkloadStatus applies the receipt loop's identity
// checks before it trusts a terminal state: the reply must name the edit's own
// mutation and, on a linked route, its admitted checkout.
func classifyEditProbePostWorkloadStatus(poll *editProbePostWorkloadReceipt, record editProbeRecord, raw json.RawMessage) *editProbePostWorkloadReceipt {
	status, err := decodeEditProbeResult(raw)
	switch {
	case err != nil:
		poll.Reason = "status reply is not a JSON object: " + err.Error()
	case record.CheckoutID != "" && status["checkout_id"] != record.CheckoutID:
		poll.Reason = "status checkout identity does not match the admitted linked view"
	case status["mutation_id"] != record.MutationID:
		poll.Reason = "status mutation identity mismatch"
	default:
		poll.GraphStatus, _ = status["graph_status"].(string)
		poll.MutationTiers = editProbeStatusTiers(status)
		poll.State = "pending"
		if status["graph_status_terminal"] == true {
			poll.State = "terminal"
		}
	}
	return poll
}
