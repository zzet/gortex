package mcp

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type analysisPruneTestStore struct {
	graph.AnalysisGenerationStore
	prune func(context.Context, int, int) (int64, error)
}

func (s *analysisPruneTestStore) PruneAnalysisGenerations(ctx context.Context, keep, batch int) (int64, error) {
	return s.prune(ctx, keep, batch)
}

func TestAnalysisGenerationPruneRetriesDeadlinesUntilComplete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Server{}
		t.Cleanup(s.DrainBackground)
		start := time.Now()
		// The prune goroutine appends while the test reads between fake-clock
		// sleeps, which do not order the two goroutines for the race detector.
		var mu sync.Mutex
		var calls []time.Duration
		attempts := func() []time.Duration {
			mu.Lock()
			defer mu.Unlock()
			return append([]time.Duration(nil), calls...)
		}
		writer := &analysisPruneTestStore{prune: func(ctx context.Context, keep, batch int) (int64, error) {
			if keep != 2 || batch != 1000 {
				t.Errorf("keep=%d batch=%d", keep, batch)
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != 30*time.Second {
				t.Errorf("per-run deadline=%v present=%v", deadline, ok)
			}
			mu.Lock()
			calls = append(calls, time.Since(start))
			n := len(calls)
			mu.Unlock()
			if n == 7 {
				return 0, nil
			}
			<-ctx.Done()
			return 1, fmt.Errorf("interrupted prune: %w", ctx.Err())
		}}
		s.scheduleAnalysisGenerationPrune(writer)
		synctest.Wait()
		s.scheduleAnalysisGenerationPrune(writer) // coalesce an in-flight request
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if got := attempts(); !s.analysisPruneScheduled.Load() || len(got) != 1 {
			t.Fatalf("retry slot released early: scheduled=%v calls=%v", s.analysisPruneScheduled.Load(), got)
		}
		s.scheduleAnalysisGenerationPrune(writer) // coalesce during backoff too
		time.Sleep(1200 * time.Second)
		synctest.Wait()
		want := []time.Duration{0, 60 * time.Second, 150 * time.Second, 300 * time.Second, 570 * time.Second, 900 * time.Second, 1230 * time.Second}
		if got := attempts(); !reflect.DeepEqual(got, want) {
			t.Fatalf("attempt starts=%v want=%v", got, want)
		}
		if s.analysisPruneScheduled.Load() {
			t.Fatal("completed prune retained its slot")
		}
	})
}

func TestAnalysisGenerationPruneStopsAfterThreeIdlePasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		s := &Server{logger: zap.New(core)}
		t.Cleanup(s.DrainBackground)
		calls := 0
		writer := &analysisPruneTestStore{prune: func(ctx context.Context, _, _ int) (int64, error) {
			calls++
			<-ctx.Done()
			return 0, ctx.Err()
		}}
		s.scheduleAnalysisGenerationPrune(writer)
		time.Sleep(time.Hour)
		synctest.Wait()
		if calls != 3 || s.analysisPruneScheduled.Load() {
			t.Fatalf("idle prune: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
		}
		warnings := logs.FilterMessage("mcp: analysis generation prune made no progress; stopping until the next analysis run").All()
		if len(warnings) != 1 || warnings[0].ContextMap()["idle_passes"] != int64(3) {
			t.Fatalf("no-progress warnings=%v", warnings)
		}
		if got := logs.FilterMessage("mcp: analysis generation prune failed").Len(); got != 3 {
			t.Fatalf("per-pass warnings=%d want=3", got)
		}
		// A later analysis can acquire the released slot and start a new prune.
		s.scheduleAnalysisGenerationPrune(writer)
		time.Sleep(time.Hour)
		synctest.Wait()
		if calls != 6 || s.analysisPruneScheduled.Load() {
			t.Fatalf("rescheduled idle prune: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
		}
	})
}

func TestAnalysisGenerationPruneLogsProgressingPassesAtInfo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		s := &Server{logger: zap.New(core)}
		t.Cleanup(s.DrainBackground)
		calls := 0
		writer := &analysisPruneTestStore{prune: func(ctx context.Context, _, _ int) (int64, error) {
			calls++
			if calls == 3 {
				return 0, nil
			}
			<-ctx.Done()
			return 5, ctx.Err()
		}}
		s.scheduleAnalysisGenerationPrune(writer)
		time.Sleep(time.Hour)
		synctest.Wait()
		if calls != 3 {
			t.Fatalf("calls=%d want=3", calls)
		}
		if got := logs.FilterMessage("mcp: analysis generation prune failed").Len(); got != 0 {
			t.Fatalf("progressing passes logged %d failure warnings", got)
		}
		passes := logs.FilterMessage("mcp: analysis generation prune pass reached its deadline; retrying").All()
		if len(passes) != 2 {
			t.Fatalf("progress logs=%d want=2", len(passes))
		}
		for _, e := range passes {
			if e.Level != zap.InfoLevel || e.ContextMap()["removed"] != int64(5) {
				t.Fatalf("progress log=%v level=%v", e.ContextMap(), e.Level)
			}
		}
	})
}

func TestAnalysisGenerationPruneProgressResetsIdlePasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Server{}
		t.Cleanup(s.DrainBackground)
		calls := 0
		writer := &analysisPruneTestStore{prune: func(ctx context.Context, _, _ int) (int64, error) {
			calls++
			if calls == 6 {
				return 0, nil
			}
			<-ctx.Done()
			if calls == 3 {
				return 1, ctx.Err()
			}
			return 0, ctx.Err()
		}}
		s.scheduleAnalysisGenerationPrune(writer)
		time.Sleep(time.Hour)
		synctest.Wait()
		if calls != 6 || s.analysisPruneScheduled.Load() {
			t.Fatalf("progress did not reset idle passes: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
		}
	})
}

func TestAnalysisGenerationPruneDrainCancelsRunAndBackoff(t *testing.T) {
	for _, backoff := range []bool{false, true} {
		t.Run(fmt.Sprintf("backoff-%v", backoff), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &Server{}
				t.Cleanup(s.DrainBackground)
				calls := 0
				writer := &analysisPruneTestStore{prune: func(ctx context.Context, _, _ int) (int64, error) {
					calls++
					<-ctx.Done()
					return 0, ctx.Err()
				}}
				s.scheduleAnalysisGenerationPrune(writer)
				synctest.Wait()
				if backoff {
					time.Sleep(30 * time.Second)
					synctest.Wait()
				}
				beforeDrain := time.Now()
				s.DrainBackground()
				synctest.Wait()
				if time.Now() != beforeDrain || s.analysisPruneScheduled.Load() {
					t.Fatal("drain waited for a deadline or left maintenance running")
				}
				s.scheduleAnalysisGenerationPrune(writer)
				time.Sleep(time.Hour)
				synctest.Wait()
				if calls != 1 || s.analysisPruneScheduled.Load() {
					t.Fatalf("prune escaped drain: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
				}
			})
		})
	}
}

func TestAnalysisGenerationPruneDoesNotRetryOtherErrors(t *testing.T) {
	for _, failure := range []error{errors.New("storage failure"), context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &Server{}
				t.Cleanup(s.DrainBackground)
				calls := 0
				writer := &analysisPruneTestStore{prune: func(context.Context, int, int) (int64, error) {
					calls++
					return 0, failure // not an expired per-run context
				}}
				s.scheduleAnalysisGenerationPrune(writer)
				time.Sleep(time.Hour)
				synctest.Wait()
				if calls != 1 || s.analysisPruneScheduled.Load() {
					t.Fatalf("non-retryable failure: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
				}
			})
		})
	}
}

func TestAnalysisGenerationPruneReplaysRequestFromLastPass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Server{}
		t.Cleanup(s.DrainBackground)
		release := make(chan struct{})
		calls := 0
		writer := &analysisPruneTestStore{prune: func(context.Context, int, int) (int64, error) {
			calls++
			if calls == 1 {
				<-release
			}
			return 0, nil
		}}
		s.scheduleAnalysisGenerationPrune(writer)
		synctest.Wait()
		// An analysis finishes while the only pass runs: the slot is taken.
		s.scheduleAnalysisGenerationPrune(writer)
		close(release)
		synctest.Wait()
		if calls != 2 || s.analysisPruneScheduled.Load() {
			t.Fatalf("request from the last pass: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
		}
	})
}

func TestAnalysisGenerationPruneReplayDoesNotRenewRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Server{}
		t.Cleanup(s.DrainBackground)
		calls := 0
		writer := &analysisPruneTestStore{prune: func(context.Context, int, int) (int64, error) {
			calls++
			return 0, errors.New("store unavailable")
		}}
		// A replacement owner took the slot and cleared the request before a
		// retiring owner's replay ran: the replay must leave both untouched.
		s.analysisPruneScheduled.Store(true)
		s.startAnalysisGenerationPrune(writer)
		synctest.Wait()
		if calls != 0 || s.analysisPruneRequested.Load() {
			t.Fatalf("replay renewed the request: calls=%d requested=%v", calls, s.analysisPruneRequested.Load())
		}
		// With the slot free, a replay runs one pass and, with no new
		// request, stops on the failing store instead of replaying again.
		s.analysisPruneScheduled.Store(false)
		s.startAnalysisGenerationPrune(writer)
		synctest.Wait()
		if calls != 1 || s.analysisPruneScheduled.Load() {
			t.Fatalf("replay on a failing store: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
		}
	})
}

func TestAnalysisGenerationPruneRetryRechecksEditLane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var busy atomic.Bool
		withEditLane(t, &busy)
		s := &Server{}
		t.Cleanup(s.DrainBackground)
		calls := 0
		writer := &analysisPruneTestStore{prune: func(ctx context.Context, _, _ int) (int64, error) {
			calls++
			<-ctx.Done()
			return 1, ctx.Err()
		}}
		before := analysisPruneStoodDown.Load()
		s.scheduleAnalysisGenerationPrune(writer)
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if calls != 1 || !s.analysisPruneScheduled.Load() {
			t.Fatalf("first pass: calls=%d scheduled=%v", calls, s.analysisPruneScheduled.Load())
		}
		busy.Store(true)
		time.Sleep(30*time.Second + analysisPruneLaneWait + analysisYieldPoll)
		synctest.Wait()
		if calls != 1 || s.analysisPruneScheduled.Load() || analysisPruneStoodDown.Load() != before+1 {
			t.Fatalf("busy retry: calls=%d scheduled=%v stood_down=%d", calls, s.analysisPruneScheduled.Load(), analysisPruneStoodDown.Load()-before)
		}
	})
}
