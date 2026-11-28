package indexer

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
)

// requireCheckoutBackgroundRefusal checks a record's B is the frozen refusal
// of a checkout ticket, in that ticket's domain, under the record's current key.
func requireCheckoutBackgroundRefusal(t *testing.T, label string, record *PublicationPhaseRecord, ticket uint64, accepted string) {
	t.Helper()
	winner, refusal := record.CoalescedPublicationEvidence()
	want := RequestedBackgroundUnavailable{
		PublicationKey: record.Key(), Scope: "requested_background_observation", Reason: "serving_attempt_binding_mismatch",
		RequestedGeneration: ticket, LastObservedGeneration: ticket, ObservedNativeHandoffs: 0,
		BindingPredicate: "checkout_refresh_cycle", BackgroundStage: "returned", AcceptedAttemptStage: accepted,
	}
	if winner != nil || refusal == nil || *refusal != want {
		t.Fatalf("%s: B = %+v (winner %+v), want %+v", label, refusal, winner, want)
	}
	if background := record.RequestedBackground(); background != nil {
		t.Fatalf("%s: a checkout ticket has no measured B window: %+v", label, background)
	}
}

// A checkout ticket's completion records the cycle that served it as P, and
// the frozen refusal of a requested-background binding as B. The cycle's
// builder phases are not patch laps, so P's partition reads not_observed.
// Only a cycle that ran for the ticket gives it a P, and evidence already on a
// record, or a record the completion does not own, is never rewritten.
func TestCheckoutPublicationRecordsServingCycleAndBackgroundRefusal(t *testing.T) {
	recorder := NewPublicationPhaseRecorder(0, 0)
	origin := time.Now()
	open := func(key string, ticket uint64) *PublicationPhaseRecord {
		record := recorder.Begin("checkout", key, PublicationSourceMCPEdit, origin)
		record.BindTicket(ticket)
		return record
	}
	cycle, completed := origin.Add(2*time.Millisecond), origin.Add(9*time.Millisecond)

	owed := open("owed", 4)
	owed.recordCheckoutPublication(4, cycle, completed)
	window := owed.Snapshot().EditTimings().Window
	p := window.PatchAccounting
	if window.PatchWallMS == nil || *window.PatchWallMS != 7 || p == nil {
		t.Fatalf("the serving cycle's window is P: %+v", window)
	}
	if p.ExecutionDisposition != "checkout_refresh_cycle" || p.Scope != "accepted_winning_physical_attempt" || p.AttemptTicket != 4 || p.AttemptRetry != 0 ||
		p.SumState != "not_observed" || p.PartitionComplete || p.BackgroundCoverage != "not_established" || p.SubtotalMS != nil || p.UnattributedMS != nil {
		t.Fatalf("P names the checkout cycle with its patch laps unobserved: %+v", p)
	}
	for name, ms := range p.PhasesMS {
		if ms != nil || p.PhaseStates[name] != "not_observed" {
			t.Fatalf("checkout builder work relabelled as patch lap %s: %v %q", name, ms, p.PhaseStates[name])
		}
	}
	requireCheckoutBackgroundRefusal(t, "owed ticket", owed, 4, "physical_window_retained")
	// The refusal follows the key the receipt reads the record by.
	if !recorder.Rekey("owed", "receipt-owed") {
		t.Fatal("rekey refused")
	}
	requireCheckoutBackgroundRefusal(t, "rekeyed record", owed, 4, "physical_window_retained")
	// A second completion leaves the first evidence alone.
	owed.recordCheckoutPublication(4, origin.Add(5*time.Millisecond), completed)
	if again := owed.Snapshot().EditTimings().Window; again.PatchWallMS == nil || *again.PatchWallMS != 7 {
		t.Fatalf("a second completion rewrote P: %+v", again)
	}

	// A ticket that rode a cycle already in flight, and one whose cycle began
	// before its edit was received, get B but no P.
	for _, tc := range []struct {
		name   string
		served time.Time
	}{
		{"rider", time.Time{}},
		{"cycle before the edit", origin.Add(-time.Millisecond)},
	} {
		record := open(tc.name, 5)
		record.recordCheckoutPublication(5, tc.served, completed)
		if window := record.Snapshot().EditTimings().Window; window.PatchWallMS != nil || window.PatchAccounting != nil {
			t.Fatalf("%s: P borrowed a cycle that did not run for the ticket: %+v", tc.name, window)
		}
		requireCheckoutBackgroundRefusal(t, tc.name, record, 5, "no_accepted_witness")
	}

	// Records the completion does not own are left unchanged.
	foreign := open("foreign", 6)
	foreign.recordCheckoutPublication(7, cycle, completed)
	terminal := open("terminal", 8)
	terminal.MarkAt(PublicationTicketCompleted, completed)
	terminal.recordCheckoutPublication(8, cycle, completed)
	watcher := recorder.Begin("", "watcher", PublicationSourceMCPEdit, origin)
	if !watcher.bindCanonicalTicket(9, origin.Add(time.Millisecond)) {
		t.Fatal("canonical bind refused")
	}
	watcher.recordCheckoutPublication(9, cycle, completed)
	for name, record := range map[string]*PublicationPhaseRecord{"another ticket": foreign, "terminal": terminal, "watcher attempt": watcher} {
		_, refusal := record.CoalescedPublicationEvidence()
		if window := record.Snapshot().EditTimings().Window; refusal != nil || window.PatchAccounting != nil {
			t.Fatalf("%s: checkout accounting written: B %+v, P %+v", name, refusal, window.PatchAccounting)
		}
	}
}

// A batch's ticket, a batch edit's or a journal recovery's, publishes its whole
// file set and keeps its own accounting: its completion records neither the
// serving cycle as P nor the B refusal, which only a committed single-file
// write's receipt closes from its cycle. A batch ticket keeps it even if it
// named one file's content, as a single-file write's ticket does.
func TestCheckoutBatchAndRecoveryTicketsKeepTheirAccounting(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "batch_edit"
		if recovery {
			name = "journal_recovery"
		}
		t.Run(name, func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			ctx := t.Context()
			var m *CheckoutMutation
			var err error
			if recovery {
				checkout, _, getErr := f.catalog.GetCheckout(ctx, f.checkoutID)
				if getErr != nil {
					t.Fatal(getErr)
				}
				m, err = l.BeginCheckoutRecovery(ctx, f.checkoutID, checkout.Incarnation, f.worktree,
					gitstate.SamplePathEvidence(f.worktree).RootIdentity, f.route().DirtyGenerationID, "", "", "")
				if err == nil {
					err = m.PrepareRecovery(ctx)
				}
			} else {
				m, err = l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch)
				if err == nil {
					err = m.Prepare(ctx)
				}
			}
			if m != nil {
				defer m.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
			path := filepath.Join(f.worktree, "helper.go")
			origin := time.Now()
			record := DefaultPublicationPhases().Begin(f.checkoutID, fmt.Sprintf("%s-%d", name, origin.UnixNano()), PublicationSourceMCPEdit, origin)
			ticket, err := m.EnqueueBatchRefresh(WithPublicationRecord(ctx, record), []CheckoutBatchFile{{Path: path, SHA256: checkoutBatchHash(t, path)}})
			m.Close()
			if err != nil {
				t.Fatal(err)
			}
			c.refreshMu.Lock()
			request := c.refreshWaiters[ticket.Ticket.Generation]
			c.refreshMu.Unlock()
			if request == nil || request.record != record || len(request.batchFiles) != 1 || request.ticket.Ticket.TierState().AckAt.IsZero() {
				t.Fatalf("the batch ticket is not bound to its record with its acknowledgement: %+v", request)
			}

			c.cycle(ctx)
			if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
				t.Fatalf("the batch did not publish: %+v", result)
			}
			snapshot := record.Snapshot()
			if _, completed := phaseOffsets(snapshot)[PublicationTicketCompleted]; !completed {
				t.Fatalf("the batch's record never completed: %+v", snapshot.Phases)
			}
			_, refusal := record.CoalescedPublicationEvidence()
			if window := snapshot.EditTimings().Window; refusal != nil || window.PatchWallMS != nil || window.PatchAccounting != nil {
				t.Fatalf("a batch ticket took a single-file write's accounting: B %+v, P %+v", refusal, window)
			}
			named := *request
			named.contentHash = request.batchFiles[0].SHA256
			if named.accountsCheckoutPublication() {
				t.Fatal("a batch ticket that names a file's content would take a single-file write's accounting")
			}
		})
	}
}
