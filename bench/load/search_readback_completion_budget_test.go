package main

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"
)

// Short virtual turns exercise the real arrival scheduler, editor FIFO and
// transaction contexts. Their four waits stand for the required first-response
// continuation; the existing real-client controls own the response oracles.
func completionBudgetPlan(editors int) editProbePlan {
	p := editProbePlan{SearchReadbackAcceptance: true, Editors: editors, NoEditWindow: int64(3 * time.Millisecond)}
	for turn := range 3 {
		for editor := range editors {
			due := int64(time.Duration(5+turn) * time.Millisecond)
			p.Steps = append(p.Steps, editProbeStep{Ordinal: len(p.Steps), Editor: editor, ActivateNS: due, DueNS: due})
		}
	}
	p.Count = len(p.Steps)
	return p
}

type completionBudgetResult struct {
	complete, privateTimeouts, phaseCensored int
	err error
	elapsed time.Duration
	deadlines []time.Duration
	records []editProbeRecord
}

func exerciseCompletionFIFO(t *testing.T, plan editProbePlan, deadline, phaseBudget, work, outer time.Duration, callerCancel bool) completionBudgetResult {
	t.Helper()
	parent, stopParent := context.WithCancel(context.Background())
	defer stopParent()
	if outer > 0 {
		if callerCancel {
			go func() { time.Sleep(outer); stopParent() }()
		} else {
			var stopOuter context.CancelFunc
			parent, stopOuter = context.WithTimeout(parent, outer)
			defer stopOuter()
		}
	}
	phase, stopPhase := context.WithTimeout(parent, phaseBudget)
	defer stopPhase()
	started := time.Now()
	r := &editProbeRuntime{plan: plan, options: runOptions{Deadline: deadline}, origin: started.Add(time.Duration(plan.NoEditWindow))}
	for _, step := range plan.Steps {
		zero := int64(0)
		r.states = append(r.states, &editProbeState{step: step, record: editProbeRecord{Ordinal: step.Ordinal, Editor: step.Editor, ActivatedNS: &zero}, ackReady: make(chan struct{})})
	}
	type turnResult struct { complete, privateTimeout bool; deadline time.Duration }
	turns := make(chan turnResult, len(plan.Steps))
	err := runEditProbeArrivals(phase, plan, r.origin, func(context.Context, editProbeStep) {}, func(ctx context.Context, step editProbeStep) {
		release, admitted := r.waitSearchReadbackTurn(ctx, step)
		defer release()
		if !admitted { return }
		state, transaction, cancel, begin := r.beginEdit(ctx, step)
		defer cancel()
		end, _ := transaction.Deadline()
		result := turnResult{deadline: end.Sub(begin)}
		var failure error
		for range 4 {
			if failure = waitContext(transaction, work/4); failure != nil { break }
		}
		state.mu.Lock()
		switch {
		case ctx.Err() != nil:
			state.record.Outcome = "phase_censored"
		case failure != nil || transaction.Err() != nil:
			state.record.Outcome = "product_deadline"
			result.privateTimeout = true
		default:
			state.record.Outcome = "complete"
			result.complete = true
		}
		state.mu.Unlock()
		turns <- result
	}, waitContext)
	close(turns)
	out := completionBudgetResult{err: err, elapsed: time.Since(started)}
	for turn := range turns {
		if turn.complete { out.complete++ }
		if turn.privateTimeout { out.privateTimeouts++ }
		out.deadlines = append(out.deadlines, turn.deadline)
	}
	for _, state := range r.states {
		out.records = append(out.records, state.record)
		if state.record.Outcome == "phase_censored" { out.phaseCensored++ }
	}
	return out
}

func TestSearchCompletionBudgetDoesNotClipValidFIFO(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "completion_budget"
		if legacy { name = "legacy_tail_clips" }
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				plan := completionBudgetPlan(1)
				const deadline = 40 * time.Millisecond
				budget, err := plan.budget(deadline, 1)
				if err != nil { t.Fatal(err) }
				if legacy { budget = time.Duration(plan.NoEditWindow+plan.Steps[len(plan.Steps)-1].DueNS)+deadline }
				got := exerciseCompletionFIFO(t, plan, deadline, budget, 30*time.Millisecond, 0, false)
				if legacy {
					if !errors.Is(got.err, context.DeadlineExceeded) || got.complete != 1 || got.phaseCensored != 2 {
						t.Fatalf("legacy ceiling did not expose the real queued-turn clipping: %+v", got)
					}
					return
				}
				if got.err != nil || got.complete != 3 || got.privateTimeouts != 0 || got.phaseCensored != 0 {
					t.Fatalf("valid FIFO turns were clipped by the phase instead of receiving their own original deadline: %+v", got)
				}
				for _, actual := range got.deadlines { if actual != deadline { t.Fatalf("transaction deadline changed: %s", actual) } }
				if got.elapsed != 98*time.Millisecond || got.elapsed >= budget { t.Fatalf("completion waited for the watchdog: elapsed=%s ceiling=%s", got.elapsed, budget) }
			})
		})
	}
}

func TestSearchCompletionBudgetUsesPerEditorMaximum(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := completionBudgetPlan(2)
		budget, err := plan.budget(40*time.Millisecond, 1)
		if err != nil || budget != 168*time.Millisecond { t.Fatalf("not the independent editor-chain maximum plus original drain: %s %v", budget, err) }
		got := exerciseCompletionFIFO(t, plan, 40*time.Millisecond, budget, 30*time.Millisecond, 0, false)
		if got.err != nil || got.complete != 6 || got.elapsed != 98*time.Millisecond { t.Fatalf("another editor was serialized or offers clipped: %+v", got) }
	})
}

func TestSearchCompletionBudgetPreservesTimeoutAndOuterCancellation(t *testing.T) {
	for _, mode := range []string{"transaction_deadline", "outer_watchdog", "caller_cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				plan := completionBudgetPlan(1)
				budget, err := plan.budget(40*time.Millisecond, 1)
				if err != nil { t.Fatal(err) }
				work, outer := 30*time.Millisecond, time.Duration(0)
				if mode == "transaction_deadline" { work = 60*time.Millisecond } else { outer = 20*time.Millisecond }
				got := exerciseCompletionFIFO(t, plan, 40*time.Millisecond, budget, work, outer, mode == "caller_cancel")
				if len(got.records) != len(plan.Steps) || got.complete != 0 { t.Fatalf("failed turns disappeared or earned completion: %+v", got) }
				if mode == "transaction_deadline" {
					if got.err != nil || got.privateTimeouts != 3 || got.phaseCensored != 0 { t.Fatalf("larger phase excused a transaction timeout: %+v", got) }
					for _, actual := range got.deadlines { if actual != 40*time.Millisecond { t.Fatalf("deadline reset: %s", actual) } }
				} else {
					want := context.DeadlineExceeded
					if mode == "caller_cancel" { want = context.Canceled }
					if !errors.Is(got.err, want) || got.phaseCensored != 3 || got.privateTimeouts != 0 { t.Fatalf("outer stop was not retained as incomplete/censored: %+v", got) }
				}
			})
		})
	}
}

func TestSearchCompletionBudgetReturnsBeforeProductionDeadlineCeiling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		plan := completionBudgetPlan(1)
		const deadline = 120 * time.Second
		budget, err := plan.budget(deadline, 1)
		if err != nil || budget < 4*time.Minute { t.Fatalf("missing finite serialized ceiling: %s %v", budget, err) }
		got := exerciseCompletionFIFO(t, plan, deadline, budget, 30*time.Millisecond, 0, false)
		if got.err != nil || got.complete != 3 || got.elapsed != 98*time.Millisecond { t.Fatalf("short completed turns waited for the theoretical ceiling: %+v", got) }
		for _, actual := range got.deadlines { if actual != deadline { t.Fatalf("original120s deadline changed: %s", actual) } }
	})
}

func TestSearchCompletionBudgetPreservesFrozenPlansAndGenericProfiles(t *testing.T) {
	for _, editors := range []int{1, 4, 8} {
		corpus := editProbeRenameCorpus(t, "go", 1000)
		corpus.EditTool = "edit.file"
		plan, err := planEditProbe(corpus, runOptions{Seed: 42, EditCount: 1000, Editors: editors, EditMean: time.Second, SearchReadbackAcceptance: true})
		if err != nil { t.Fatal(err) }
		before, err := hashJSON(plan)
		if err != nil { t.Fatal(err) }
		budget, err := plan.budget(120*time.Second, 1)
		if err != nil { t.Fatal(err) }
		after, err := hashJSON(plan)
		if err != nil || before != after || plan.Count != 1000 || len(plan.Steps) != 1000 || plan.MeanNS != int64(time.Second) { t.Fatal("budget changed a frozen offered plan") }
		counts := make(map[int]int)
		for _, step := range plan.Steps { counts[step.Editor]++ }
		for _, count := range counts { if budget < time.Duration(count)*120*time.Second { t.Fatalf("editor chain cannot fit its original deadlines: %d %s", count, budget) } }
	}
	for _, builds := range []int{1, 2} {
		plan, err := makeEditProbePlan(editProbeCorpus(), 42, 100)
		if err != nil { t.Fatal(err) }
		plan.NoEditWindow = int64(time.Second)
		budget, err := plan.budget(120*time.Second, builds)
		want := time.Duration(plan.NoEditWindow+plan.Steps[len(plan.Steps)-1].DueNS)+time.Duration(builds)*120*time.Second
		if err != nil || budget != want { t.Fatalf("generic profile changed: %s want%s %v", budget, want, err) }
	}
}

func TestSearchCompletionBudgetRejectsOverflow(t *testing.T) {
	plan := completionBudgetPlan(1)
	for _, deadline := range []time.Duration{time.Duration(math.MaxInt64), time.Duration(math.MaxInt64/3)} {
		if _, err := plan.budget(deadline, 1); err == nil { t.Fatalf("serialized sum/drain overflow accepted: %s", deadline) }
	}
	plan.Steps[0].DueNS = -1
	if _, err := plan.budget(time.Second, 1); err == nil { t.Fatal("negative frozen due accepted") }
}
