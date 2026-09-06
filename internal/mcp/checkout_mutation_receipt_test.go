package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/indexer"
)

type receiptCheckoutMutation struct {
	ticket            *indexer.CheckoutRefreshTicket
	enqueueErr        error
	enqueueContextErr error
	enqueuedPath      string
}

func (m *receiptCheckoutMutation) Prepare(context.Context) error { return nil }
func (m *receiptCheckoutMutation) Refresh(context.Context) (indexer.CheckoutCycle, error) {
	panic("queued checkout mutations must not reconcile while holding the request lease")
}
func (m *receiptCheckoutMutation) Identity() (string, string) {
	return m.ticket.CheckoutID, m.ticket.Incarnation
}
func (m *receiptCheckoutMutation) EnqueueRefresh(ctx context.Context, path string) (*indexer.CheckoutRefreshTicket, error) {
	m.enqueueContextErr = ctx.Err()
	m.enqueuedPath = path
	return m.ticket, m.enqueueErr
}

func newReceiptCheckoutMutation(t *testing.T) (*receiptCheckoutMutation, chan indexer.MutationResult, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "edit.go")
	if err := os.WriteFile(path, []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan indexer.MutationResult, 1)
	t.Cleanup(func() { close(done) })
	return &receiptCheckoutMutation{ticket: &indexer.CheckoutRefreshTicket{
		CheckoutID: "checkout-1", Incarnation: "incarnation-1", Root: root, RepoPrefix: "fixture",
		Ticket: &indexer.MutationTicket{Path: path, Generation: 7, Done: done},
	}}, done, path
}

func waitCheckoutReceipt(t *testing.T, s *Server, id string) *mutationReceipt {
	t.Helper()
	value, ok := s.mutationReceipts.Load(id)
	if !ok {
		t.Fatalf("missing freshness receipt %q", id)
	}
	receipt := value.(*mutationReceipt)
	select {
	case <-receipt.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("freshness receipt %q did not complete", id)
	}
	return receipt
}

func TestCheckoutMutationReceiptRemainsPendingUntilPublication(t *testing.T) {
	mutation, done, path := newReceiptCheckoutMutation(t)
	s := &Server{mutationReindexWait: time.Nanosecond}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withCheckoutMutation(ctx, mutation, filepath.Dir(path))
	cancel() // Committed bytes must still acquire a real publication ticket.
	outcome := s.mutationReindexState(ctx, path)
	if !outcome.Pending || outcome.Reindexed || outcome.Err != nil || outcome.Receipt == "" || !outcome.checkoutScoped {
		t.Fatalf("committed checkout should be pending, not failed/fresh: %+v", outcome)
	}
	if mutation.enqueueContextErr != nil || mutation.enqueuedPath != path {
		t.Fatalf("publication was not detached and scoped: %v %q", mutation.enqueueContextErr, mutation.enqueuedPath)
	}
	record := s.beginMutationCommit(ctx, "edit_file", "key", "fingerprint", "edit.go", path)
	record.markCommitted("written-sha", 16)
	record.recordGraph(outcome)
	payload := s.mutationStatusPayload(record)
	if payload["graph_status"] != mutationGraphPending || payload["graph_status_terminal"] != false || payload["disk_status"] != mutationDiskCommitted {
		t.Fatalf("pending receipt misclassified: %#v", payload)
	}
	if payload["checkout_id"] != "checkout-1" || payload["checkout_incarnation"] != "incarnation-1" || payload["reindex_receipt"] != outcome.Receipt {
		t.Fatalf("receipt lost publication identity: %#v", payload)
	}
	done <- indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 29, Reindexed: true}
	waitCheckoutReceipt(t, s, outcome.Receipt)
	payload = s.mutationStatusPayload(record)
	if payload["graph_status"] != mutationGraphFresh || payload["graph_status_terminal"] != true || payload["applied_generation"] != uint64(29) {
		t.Fatalf("published generation did not resolve receipt: %#v", payload)
	}
	if payload["new_sha"] != "written-sha" || payload["retry_safe"] != false {
		t.Fatalf("graph publication changed disk evidence: %#v", payload)
	}
	// A concurrent poller may still hold the original pending snapshot.
	record.recordGraph(outcome)
	if got := record.snapshot().GraphStatus; got != mutationGraphFresh {
		t.Fatalf("late pending observation regressed terminal receipt to %q", got)
	}
}

func TestCheckoutMutationReceiptRetainsTerminalCause(t *testing.T) {
	for _, cause := range []string{"checkout was deleted", "target content was superseded by branch switch"} {
		t.Run(cause, func(t *testing.T) {
			mutation, done, path := newReceiptCheckoutMutation(t)
			s := &Server{}
			ctx := withCheckoutMutation(context.Background(), mutation, filepath.Dir(path))
			outcome := s.mutationReindexState(ctx, path)
			record := s.beginMutationCommit(ctx, "write_file", "", "", "edit.go", path)
			record.markCommitted("disk-sha", 4)
			record.recordGraph(outcome)
			failure := errors.New(cause)
			done <- indexer.MutationResult{RequestedGeneration: 7, Err: failure}
			waitCheckoutReceipt(t, s, outcome.Receipt)
			state, ok := s.mutationReceiptState(outcome.Receipt)
			if !ok || !errors.Is(state.Err, failure) || state.Pending || state.Reindexed {
				t.Fatalf("terminal ticket evidence lost: %+v", state)
			}
			payload := s.mutationStatusPayload(record)
			if payload["graph_status"] != mutationGraphFailed || payload["graph_status_terminal"] != true || payload["error"] != cause || payload["disk_status"] != mutationDiskCommitted {
				t.Fatalf("terminal graph failure lost original cause/disk verdict: %#v", payload)
			}
		})
	}
}

func TestCheckoutMutationReceiptCannotBeRepairedByCanonicalSweeps(t *testing.T) {
	mutation, done, path := newReceiptCheckoutMutation(t)
	s := &Server{}
	receipt := s.trackCheckoutRefreshTicket(mutation.ticket)
	failure := errors.New("checkout incarnation disappeared")
	done <- indexer.MutationResult{RequestedGeneration: 7, Err: failure}
	waitCheckoutReceipt(t, s, receipt.id)
	s.resolveSupersededFailedReceipts(path, 100, indexer.MutationResult{AppliedGeneration: 100, Reindexed: true})
	if eligible := s.failedReceiptsBefore([]string{path}, filepath.Dir(path)); len(eligible) != 0 {
		t.Fatalf("canonical reindex admitted checkout receipt: %#v", eligible)
	}
	s.resolveReindexedPathReceipts(path, map[string]struct{}{receipt.id: {}})
	if got := receipt.outcome(false); got.Reindexed || !errors.Is(got.Err, failure) {
		t.Fatalf("unrelated canonical apply falsely certified checkout bytes: %+v", got)
	}
}

func TestCheckoutMutationReceiptScopeAndSummary(t *testing.T) {
	mutation, done, path := newReceiptCheckoutMutation(t)
	s := &Server{}
	receipt := s.trackCheckoutRefreshTicket(mutation.ticket)
	ctx := withCheckoutMutation(context.Background(), mutation, filepath.Dir(path))
	if pending, err := s.mutationFreshnessSummaryForRepos(ctx, "fixture"); !pending || err != nil {
		t.Fatalf("selected pending ticket missing: pending=%t err=%v", pending, err)
	}
	if pending, err := s.mutationFreshnessSummaryForRepos(context.Background(), "fixture"); pending || err != nil {
		t.Fatalf("checkout ticket leaked into canonical prefix scope: pending=%t err=%v", pending, err)
	}
	record := s.beginMutationCommit(ctx, "edit_file", "", "", "edit.go", path)
	if !mutationCommitInScope(ctx, record) || mutationCommitInScope(context.Background(), record) {
		t.Fatal("receipt identity was not stamped before disk publication")
	}
	other := *mutation.ticket
	other.Incarnation = "incarnation-2"
	otherCtx := withCheckoutMutation(context.Background(), &receiptCheckoutMutation{ticket: &other}, filepath.Dir(path))
	if mutationCommitInScope(otherCtx, record) {
		t.Fatal("recreated checkout inherited previous incarnation receipt")
	}
	failure := errors.New("publication failed with original detail")
	done <- indexer.MutationResult{RequestedGeneration: 7, Err: failure}
	waitCheckoutReceipt(t, s, receipt.id)
	if pending, err := s.mutationFreshnessSummaryForRepos(ctx, "fixture"); pending || !errors.Is(err, failure) {
		t.Fatalf("summary did not preserve terminal cause: pending=%t err=%v", pending, err)
	}
	if err := s.awaitMutationFreshnessForRepos(context.Background(), "fixture"); err != nil {
		t.Fatalf("sibling checkout failure blocked canonical graph: %v", err)
	}
}

func TestCheckoutMutationReceiptAdmissionFailureKeepsOriginalCause(t *testing.T) {
	mutation, _, path := newReceiptCheckoutMutation(t)
	mutation.enqueueErr = errors.New("checkout stopped during publication admission")
	s := &Server{}
	ctx := withCheckoutMutation(context.Background(), mutation, filepath.Dir(path))
	outcome := s.mutationReindexState(ctx, path)
	if !errors.Is(outcome.Err, mutation.enqueueErr) || outcome.Pending || outcome.Reindexed {
		t.Fatalf("admission failed without original cause: %+v", outcome)
	}
	if !strings.Contains(outcome.Err.Error(), "after disk commit") {
		t.Fatalf("admission failure obscures committed bytes: %v", outcome.Err)
	}
}

func TestCheckoutMutationReceiptRejectsMismatchedTicket(t *testing.T) {
	for _, change := range []string{"checkout", "incarnation", "path", "missing completion"} {
		t.Run(change, func(t *testing.T) {
			mutation, _, path := newReceiptCheckoutMutation(t)
			ctx := withCheckoutMutation(context.Background(), mutation, filepath.Dir(path))
			switch change {
			case "checkout":
				mutation.ticket.CheckoutID = "other-checkout"
			case "incarnation":
				mutation.ticket.Incarnation = "other-incarnation"
			case "path":
				mutation.ticket.Ticket.Path = filepath.Join(filepath.Dir(path), "other.go")
			case "missing completion":
				mutation.ticket.Ticket.Done = nil
			}
			outcome := (&Server{}).mutationReindexState(ctx, path)
			if outcome.Err == nil || outcome.Pending || outcome.Reindexed || outcome.Receipt != "" {
				t.Fatalf("unproven ticket was accepted: %+v", outcome)
			}
		})
	}
}

func TestCheckoutMutationReceiptRecoveryTicketHasNoDiskVerdict(t *testing.T) {
	mutation, done, path := newReceiptCheckoutMutation(t)
	s := &Server{}
	ctx := withCheckoutMutation(context.Background(), mutation, filepath.Dir(path))
	receipt := s.trackCheckoutRefreshTicket(mutation.ticket)
	payload, ok := s.graphRefreshReceiptPayload(ctx, receipt.id)
	if !ok || payload["graph_status"] != mutationGraphPending || payload["disk_status"] != "unrecorded" || payload["receipt_kind"] != "graph_refresh" {
		t.Fatalf("recovery receipt invented disk evidence: %#v", payload)
	}
	if _, ok := s.graphRefreshReceiptPayload(context.Background(), receipt.id); ok {
		t.Fatal("canonical request could inspect another checkout's ticket")
	}
	done <- indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 29, Reindexed: true}
	waitCheckoutReceipt(t, s, receipt.id)
	payload, ok = s.graphRefreshReceiptPayload(ctx, receipt.id)
	if !ok || payload["graph_status"] != mutationGraphFresh || payload["graph_status_terminal"] != true || payload["disk_status"] != "unrecorded" {
		t.Fatalf("recovery completion changed disk verdict: %#v", payload)
	}
}

func TestCheckoutMutationReceiptPathLookupSelectsOwnHistory(t *testing.T) {
	mutation, _, path := newReceiptCheckoutMutation(t)
	s := &Server{}
	ctx := withCheckoutMutation(context.Background(), mutation, filepath.Dir(path))
	wanted := s.beginMutationCommit(ctx, "edit_file", "", "", "edit.go", path)
	// More recent primary and sibling mutations must not hide the selected
	// checkout's older record for the same repo-relative source path.
	s.beginMutationCommit(context.Background(), "edit_file", "", "", "edit.go", path)
	other := *mutation.ticket
	other.CheckoutID = "other-checkout"
	otherCtx := withCheckoutMutation(context.Background(), &receiptCheckoutMutation{ticket: &other}, filepath.Dir(path))
	s.beginMutationCommit(otherCtx, "edit_file", "", "", "edit.go", path)
	if got, ok := s.recentMutationCommitInScope(ctx, "edit.go"); !ok || got != wanted {
		t.Fatalf("same relative path selected another checkout record: got=%p want=%p", got, wanted)
	}
}

func TestCheckoutMutationReceiptSupersededDoesNotPoisonCurrentView(t *testing.T) {
	mutation, done, path := newReceiptCheckoutMutation(t)
	s := &Server{}
	ctx := withCheckoutMutation(context.Background(), mutation, filepath.Dir(path))
	receipt := s.trackCheckoutRefreshTicket(mutation.ticket)
	record := s.beginMutationCommit(ctx, "edit_file", "", "", "edit.go", path)
	record.markCommitted("original-bytes-sha", 4)
	record.recordGraph(receipt.outcome(true))
	done <- indexer.MutationResult{RequestedGeneration: 7, Err: fmt.Errorf("%w: branch switched", indexer.ErrCheckoutRefreshSuperseded)}
	waitCheckoutReceipt(t, s, receipt.id)
	if pending, err := s.mutationFreshnessSummaryForRepos(ctx, "fixture"); pending || err != nil {
		t.Fatalf("historical branch poisoned current-view diagnostics: pending=%t err=%v", pending, err)
	}
	if err := s.awaitMutationFreshnessForRepos(ctx, "fixture"); err != nil {
		t.Fatalf("historical branch poisoned current-view barrier: %v", err)
	}
	payload := s.mutationStatusPayload(record)
	if payload["graph_status"] != mutationGraphFailed || payload["graph_status_terminal"] != true || payload["new_sha"] != "original-bytes-sha" {
		t.Fatalf("historical receipt was falsely certified by current-view exemption: %#v", payload)
	}
}

func TestCheckoutMutationReceiptBindsBytesActuallyCommitted(t *testing.T) {
	for _, overwritten := range []bool{false, true} {
		t.Run(fmt.Sprintf("overwritten=%t", overwritten), func(t *testing.T) {
			mutation, done, path := newReceiptCheckoutMutation(t)
			s := &Server{}
			ctx := withCheckoutMutation(context.Background(), mutation, filepath.Dir(path))
			data := []byte("package committed\n")
			record, err := s.commitFileMutation(ctx, "write_file", "", "", "edit.go", path, data, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			captured := data
			if overwritten {
				captured = []byte("package concurrent_writer\n")
				if err := os.WriteFile(path, captured, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sum := sha256.Sum256(captured)
			mutation.ticket.ContentHash = hex.EncodeToString(sum[:])
			outcome := s.mutationReindexState(ctx, path)
			record.recordGraph(outcome)
			if overwritten {
				if !errors.Is(outcome.Err, indexer.ErrCheckoutRefreshSuperseded) || outcome.Pending || outcome.Reindexed || outcome.Receipt != "" {
					t.Fatalf("another writer's bytes were accepted for original edit: %+v", outcome)
				}
				if got := record.snapshot(); got.DiskStatus != mutationDiskCommitted || got.NewSHA != gitBlobSHA(data) || got.GraphStatus != mutationGraphFailed {
					t.Fatalf("original committed evidence lost: %+v", got)
				}
				return
			}
			if !outcome.Pending || outcome.Err != nil {
				t.Fatalf("matching committed bytes were rejected: %+v", outcome)
			}
			done <- indexer.MutationResult{RequestedGeneration: 7, AppliedGeneration: 29, Reindexed: true}
			waitCheckoutReceipt(t, s, outcome.Receipt)
		})
	}
}

func TestCheckoutMutationReceiptRecoveryRetiresOnlyPriorBarrierFailures(t *testing.T) {
	for _, laterFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("later_failure=%t", laterFailure), func(t *testing.T) {
			mutation, done, path := newReceiptCheckoutMutation(t)
			s := &Server{}
			ctx := withCheckoutMutation(context.Background(), mutation, filepath.Dir(path))
			failure := errors.New("original storage failure")
			old := &mutationReceipt{
				id: "old", repo: "fixture", path: path, generation: 1, done: make(chan struct{}), completed: true,
				checkoutScoped: true, checkoutID: "checkout-1", checkoutIncarnation: "incarnation-1",
				result: indexer.MutationResult{RequestedGeneration: 1, Err: failure},
			}
			close(old.done)
			s.mutationReceipts.Store(old.id, old)
			record := &mutationCommitRecord{id: "commit-old", disk: mutationDiskCommitted, newSHA: "original-sha"}
			record.recordGraph(old.outcome(false))
			var pending *mutationReceipt
			if laterFailure {
				pending = &mutationReceipt{id: "pending-before-recovery", repo: "fixture", path: path, generation: 2, done: make(chan struct{}), checkoutScoped: true, checkoutID: "checkout-1", checkoutIncarnation: "incarnation-1"}
				s.mutationReceipts.Store(pending.id, pending)
			}
			// Snapshot is deliberately taken before root recovery admission.
			eligible := s.failedCheckoutRefreshReceiptsBefore("checkout-1", "incarnation-1")
			if len(eligible) != 1 {
				t.Fatalf("pending ticket included in recovery snapshot: %#v", eligible)
			}
			mutation.ticket.Ticket.Path = mutation.ticket.Root
			mutation.ticket.Ticket.Generation = 10
			recovery := s.trackCheckoutRecoveryTicket(mutation.ticket, eligible)
			if laterFailure {
				pending.mu.Lock()
				pending.completed = true
				pending.result = indexer.MutationResult{RequestedGeneration: 2, Err: errors.New("failed only after recovery began")}
				pending.mu.Unlock()
				close(pending.done)
				eligible[pending.id] = struct{}{}
				for _, later := range []*mutationReceipt{
					{id: "newer", repo: "fixture", path: path, generation: 11, checkoutScoped: true, checkoutID: "checkout-1", checkoutIncarnation: "incarnation-1"},
					{id: "sibling", repo: "fixture", path: path, generation: 3, checkoutScoped: true, checkoutID: "checkout-2", checkoutIncarnation: "incarnation-1"},
					{id: "recreated", repo: "fixture", path: path, generation: 3, checkoutScoped: true, checkoutID: "checkout-1", checkoutIncarnation: "incarnation-2"},
				} {
					later.done = make(chan struct{})
					later.completed = true
					later.result = indexer.MutationResult{Err: errors.New(later.id + " failure")}
					close(later.done)
					s.mutationReceipts.Store(later.id, later)
					// Mutating the caller's snapshot after tracking cannot expand
					// the recovery worker's eligibility set.
					eligible[later.id] = struct{}{}
				}
			}
			done <- indexer.MutationResult{RequestedGeneration: 10, AppliedGeneration: 29, Reindexed: true}
			waitCheckoutReceipt(t, s, recovery.id)
			if laterFailure {
				// Even an overly broad caller snapshot cannot cross identity or
				// admission ordering boundaries at the final retirement gate.
				s.resolveCheckoutRecoveryReceipts(recovery, map[string]struct{}{"newer": {}, "sibling": {}, "recreated": {}}, 29)
			}
			old.mu.RLock()
			covered := old.barrierRecoveredGeneration
			old.mu.RUnlock()
			if covered != 29 {
				t.Fatalf("verified root recovery did not retire prior barrier: %d", covered)
			}
			if got := old.outcome(false); got.Reindexed || !errors.Is(got.Err, failure) {
				t.Fatalf("recovery rewrote historical publication evidence: %+v", got)
			}
			if got := s.mutationStatusPayload(record); got["graph_status"] != mutationGraphFailed || got["error"] != failure.Error() || got["new_sha"] != "original-sha" {
				t.Fatalf("recovery rewrote original commit receipt: %#v", got)
			}
			isPending, err := s.mutationFreshnessSummaryForRepos(ctx, "fixture")
			if isPending || (!laterFailure && err != nil) || (laterFailure && err == nil) {
				t.Fatalf("current-view barrier lost failure boundary: pending=%t err=%v", isPending, err)
			}
			if laterFailure {
				for _, id := range []string{"pending-before-recovery", "newer", "sibling", "recreated"} {
					value, _ := s.mutationReceipts.Load(id)
					r := value.(*mutationReceipt)
					r.mu.RLock()
					covered := r.barrierRecoveredGeneration
					r.mu.RUnlock()
					if covered != 0 {
						t.Fatalf("unrelated or later failure %q was retired", id)
					}
				}
			} else if err := s.awaitMutationFreshnessForRepos(ctx, "fixture"); err != nil {
				t.Fatalf("verified root recovery left current-view barrier poisoned: %v", err)
			}
		})
	}
}

func BenchmarkCheckoutMutationReceiptPendingPoll(b *testing.B) {
	s := &Server{}
	receipt := &mutationReceipt{id: "mutation-benchmark", generation: 7, done: make(chan struct{}), checkoutScoped: true, checkoutID: "checkout-1", checkoutIncarnation: "incarnation-1"}
	s.mutationReceipts.Store(receipt.id, receipt)
	record := &mutationCommitRecord{id: "commit-benchmark", disk: mutationDiskCommitted, graph: mutationGraphPending, graphRecorded: true, reindexReceipt: receipt.id}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		payload := s.mutationStatusPayload(record)
		if payload["graph_status"] != mutationGraphPending {
			b.Fatal(fmt.Sprint(payload))
		}
	}
}
