package indexer

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestMutationResolveMutexSubsetAggregatesWithoutChangingPatchLaps(t *testing.T) {
	r, s, origin := scalarPatchFixture(t)
	at := func(ms int) time.Time { return origin.Add(time.Duration(ms) * time.Millisecond) }
	span := s.beginPatchLapAt(patchResolve, at(3))
	before := s.patchLaps.laps
	observe := s.mutationResolveMutexObserver()
	observe(at(4), at(6))
	observe(at(7), at(8))
	if before != s.patchLaps.laps {
		t.Fatal("diagnostic was added to disjoint patch laps")
	}
	span.endAt(at(12))
	span = s.beginPatchLapAt(patchResolve, at(13))
	observe(at(14), at(14)) // A real zero acquisition remains measured.
	span.endAt(at(18))
	s.markAt(PublicationPatchExecutionFinished, at(20))
	r.Absorb(s)
	r.MarkAt(PublicationTicketCompleted, at(21))
	snapshot := r.Snapshot()
	out := snapshot.EditTimings()
	d := out.Window.ResolveMutexWait
	if d == nil || d.State != "measured" || d.SubsetOf != "resolve" || d.Acquisitions != 3 || d.Milliseconds == nil || *d.Milliseconds != 3 {
		t.Fatalf("mutation diagnostic lost: %+v", d)
	}
	if out.Milliseconds["resolve"] == nil || *out.Milliseconds["resolve"] != 14 {
		t.Fatal("diagnostic changed enclosing resolve duration")
	}
	if _, present := out.Milliseconds["resolve_mutex_wait"]; present {
		t.Fatal("diagnostic entered phases_ms")
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	diagnostic := wire["phase_timing"].(map[string]any)["resolve_mutex_wait"].(map[string]any)
	if diagnostic["subset_of"] != "resolve" || diagnostic["acquisitions"] != float64(3) {
		t.Fatal("wrong wire contract")
	}
	s.observeMutationResolveMutexWait(at(30), at(31))
	if r.Snapshot().EditTimings().Window.ResolveMutexWait.Acquisitions != 3 {
		t.Fatal("later collector mutated published diagnostic")
	}
}

func TestMutationResolveMutexSubsetRejectsWrongSpanWithoutPoisoningAccounting(t *testing.T) {
	for _, kind := range []string{"before", "after", "backwards", "wrong_lap"} {
		t.Run(kind, func(t *testing.T) {
			r, s, origin := scalarPatchFixture(t)
			at := func(ms int) time.Time { return origin.Add(time.Duration(ms) * time.Millisecond) }
			lap := patchResolve
			if kind == "wrong_lap" {
				lap = patchParse
			}
			span := s.beginPatchLapAt(lap, at(3))
			a, b := 4, 5
			switch kind {
			case "before":
				a = 2
			case "after":
				b = 10
			case "backwards":
				b = 3
			}
			s.mutationResolveMutexObserver()(at(a), at(b))
			span.endAt(at(7))
			// A later span cannot legitimize the earlier acquisition's out-of-span end.
			scalarSpan(s, patchResolve, origin, 11*time.Millisecond, 15*time.Millisecond)
			s.markAt(PublicationPatchExecutionFinished, at(16))
			r.Absorb(s)
			r.MarkAt(PublicationTicketCompleted, at(17))
			out := r.Snapshot().EditTimings()
			d := out.Window.ResolveMutexWait
			if d == nil || d.State != "invalid_order" || d.Milliseconds != nil || d.Acquisitions != 1 {
				t.Fatalf("invalid diagnostic accepted: %+v", d)
			}
			if out.States["resolve"] != "measured" {
				t.Fatal("diagnostic poisoned disjoint accounting")
			}
		})
	}
}

func TestMutationResolveMutexSubsetNilAndZeroCompatibility(t *testing.T) {
	var absent *PublicationStamps
	if absent.mutationResolveMutexObserver() != nil || (&PublicationStamps{}).mutationResolveMutexObserver() != nil {
		t.Fatal("unobserved execution allocated callback")
	}
	r, s, origin := scalarPatchFixture(t)
	at := func(ms int) time.Time { return origin.Add(time.Duration(ms) * time.Millisecond) }
	span := s.beginPatchLapAt(patchResolve, at(3))
	span.endAt(at(7))
	s.markAt(PublicationPatchExecutionFinished, at(8))
	r.Absorb(s)
	r.MarkAt(PublicationTicketCompleted, at(9))
	if r.Snapshot().EditTimings().Window.ResolveMutexWait != nil {
		t.Fatal("missing observer became measured zero")
	}
	r, s, origin = scalarPatchFixture(t)
	span = s.beginPatchLapAt(patchResolve, at(3))
	s.mutationResolveMutexObserver()(at(4), at(4))
	span.endAt(at(7))
	s.markAt(PublicationPatchExecutionFinished, at(8))
	r.Absorb(s)
	r.MarkAt(PublicationTicketCompleted, at(9))
	d := r.Snapshot().EditTimings().Window.ResolveMutexWait
	if d == nil || d.State != "measured" || d.Milliseconds == nil || *d.Milliseconds != 0 || d.Acquisitions != 1 {
		t.Fatalf("actualzero lost: %+v", d)
	}
}

func TestMutationResolveMutexSubsetStaysWithPhysicalCoalescedWinner(t *testing.T) {
	origin := time.Now()
	at := func(ms int) time.Time { return origin.Add(time.Duration(ms) * time.Millisecond) }
	recorder := NewPublicationPhaseRecorder(8, 1)
	defer runtime.KeepAlive(recorder)
	first := recorder.Begin("", "first", PublicationSourceMCPEdit, origin)
	second := recorder.Begin("", "second", PublicationSourceMCPEdit, origin)
	if !first.bindCanonicalTicket(1, origin) || !second.bindCanonicalTicket(2, origin) {
		t.Fatal("ticket binding failed")
	}
	early := first.Snapshot()
	winner := first.beginCoalescedAttempt(3, 0, at(1))
	if winner == nil {
		t.Fatal("no physical winner")
	}
	stamps := winner.newCanonicalPatchStamps()
	stamps.markAt(PublicationPatchExecutionStarted, at(2))
	span := stamps.beginPatchLapAt(patchResolve, at(3))
	stamps.mutationResolveMutexObserver()(at(4), at(6))
	span.endAt(at(7))
	stamps.markAt(PublicationPatchExecutionFinished, at(8))
	winner.Absorb(stamps)
	winner.MarkAt(PublicationTicketCompleted, at(9))
	first.linkCoalescedWinner(winner, 1, 3)
	second.linkCoalescedWinner(winner, 2, 3)
	a, b := first.CoalescedPublication(), second.CoalescedPublication()
	if a == nil || b == nil || a.Publication.Key != b.Publication.Key || a.AppliedGeneration != 3 || b.AppliedGeneration != 3 {
		t.Fatal("physical owner linkage changed")
	}
	if early.EditTimings().Window.ResolveMutexWait != nil || first.Snapshot().EditTimings().Window.ResolveMutexWait != nil || second.Snapshot().EditTimings().Window.ResolveMutexWait != nil {
		t.Fatal("coalesced receipt borrowed physical mutex evidence")
	}
	d := a.Timings.Window.ResolveMutexWait
	if d == nil || d.State != "measured" || d.Acquisitions != 1 || d.Milliseconds == nil || *d.Milliseconds != 2 {
		t.Fatalf("winner lost diagnostic: %+v", d)
	}
	if b.Timings.Window.ResolveMutexWait.Acquisitions != d.Acquisitions {
		t.Fatal("shared execution invented another acquisition")
	}
	stamps.observeMutationResolveMutexWait(at(10), at(11))
	if a.Timings.Window.ResolveMutexWait.State != "measured" || winner.Snapshot().EditTimings().Window.ResolveMutexWait.Acquisitions != 1 {
		t.Fatal("later observer mutated physical snapshot")
	}
}

func TestMutationResolveMutexSubsetActualPointExecutors(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "multi"}[multi], func(t *testing.T) {
			record := newPointPublication(t, "actual-mutex")
			if !record.bindCanonicalTicket(1, time.Now()) {
				t.Fatal("ticket binding failed")
			}
			stamps := record.newCanonicalPatchStamps()
			var execute func() error
			if multi {
				mi, _, roots := repoTopologyFixture(t)
				path := filepath.Join(roots["app"], "main.go")
				writeFile(t, path, "package app\nfunc Run() {}\nfunc Start() { Run() }\nfunc Added() { Run() }\n")
				execute = func() error { _, err := mi.incrementalPointRepoRawObserved("app", path, stamps); return err }
			} else {
				root, idx, w := inertTestWatcher(t, "edit.go", "package p\nfunc Callee() {}\n")
				w.degradedNoFsnotify = true
				t.Cleanup(func() { _ = w.Stop() })
				path := filepath.Join(root, "edit.go")
				bumpMtime(t, path, "package p\nfunc Callee() {}\nfunc Added() { Callee() }\n")
				execute = func() error { _, err := idx.incrementalPointWatcherPathObserved(root, path, stamps); return err }
			}
			stamps.markAt(PublicationPatchExecutionStarted, time.Now())
			if err := execute(); err != nil {
				t.Fatal(err)
			}
			stamps.markAt(PublicationPatchExecutionFinished, time.Now())
			record.Absorb(stamps)
			record.Mark(PublicationTicketCompleted)
			out := record.Snapshot().EditTimings()
			d := out.Window.ResolveMutexWait
			minimum := uint64(1)
			if multi {
				minimum = 2
			}
			if d == nil || d.State != "measured" || d.Milliseconds == nil || d.Acquisitions < minimum {
				t.Fatalf("actual executor did not retain scoped acquisitions: %+v resolve=%s", d, out.States["resolve"])
			}
			if out.Milliseconds["resolve"] == nil || *d.Milliseconds > *out.Milliseconds["resolve"] {
				t.Fatal("sequential mutex subset exceeded enclosing resolve wall")
			}
		})
	}
}

func TestMutationResolveMutexDiagnosticCannotChangePhysicalAccounting(t *testing.T) {
	for _, change := range []string{"diagnostic_only", "lap_changed", "invalid_changed"} {
		t.Run(change, func(t *testing.T) {
			stamps, origin, _ := completeControlledAttempt(t)
			recorder := NewPublicationPhaseRecorder(2, 1)
			defer runtime.KeepAlive(recorder)
			record := recorder.Begin("", "accounting-owner", PublicationSourceMCPEdit, origin)
			if !record.bindCanonicalTicket(1, origin) {
				t.Fatal("binding failed")
			}
			record.Absorb(stamps)
			record.MarkAt(PublicationTicketCompleted, origin.Add(32*time.Millisecond))
			switch change {
			case "diagnostic_only":
				stamps.observeMutationResolveMutexWait(origin.Add(20*time.Millisecond), origin.Add(21*time.Millisecond))
			case "lap_changed":
				stamps.patchLaps.laps[patchResolve].total += time.Nanosecond
			case "invalid_changed":
				stamps.patchLaps.invalid = true
			}
			window, ok := record.completePhysicalAttempt(stamps, origin.Add(time.Millisecond), origin.Add(31*time.Millisecond), 0)
			if !ok {
				t.Fatal("physical owner was rejected")
			}
			if window.accounting.invalid != (change != "diagnostic_only") {
				t.Fatalf("diagnostic isolation weakened canonical identity: %s invalid=%t", change, window.accounting.invalid)
			}
			if record.Snapshot().EditTimings().Window.ResolveMutexWait != nil {
				t.Fatal("late diagnostic rewrote the frozen publication snapshot")
			}
		})
	}
}
