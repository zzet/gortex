package store_sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Issue #843: the periodic PASSIVE checkpoint runs under a fixed one-second
// window that kills an in-flight PRAGMA on multi-GiB stores, wasting the
// partial fold and restarting the same lottery on every retry. The policy
// tests below pin the adaptive behavior: grow only on an execute-phase
// deadline, cap the growth at the retry cadence's maximum, and reset on
// every other outcome.

func TestPassiveCheckpointWindowPolicyClamps(t *testing.T) {
	tests := []struct {
		name           string
		base, max      time.Duration
		wantBaseWindow time.Duration
	}{
		{name: "defaults", base: 0, max: 0, wantBaseWindow: walPassiveCheckpointTimeout},
		{name: "zero base falls back to default", base: 0, max: 30 * time.Second, wantBaseWindow: walPassiveCheckpointTimeout},
		{name: "explicit base wins", base: 3 * time.Second, max: 30 * time.Second, wantBaseWindow: 3 * time.Second},
		{name: "max below base is raised to base", base: 3 * time.Second, max: time.Second, wantBaseWindow: 3 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newPassiveCheckpointWindowPolicy(test.base, test.max)
			if got := p.window(); got != test.wantBaseWindow {
				t.Fatalf("initial window = %s, want %s", got, test.wantBaseWindow)
			}
		})
	}
}

func TestPassiveCheckpointWindowPolicyGrowsAndResets(t *testing.T) {
	p := newPassiveCheckpointWindowPolicy(walPassiveCheckpointTimeout, walCheckpointRetryMax)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, expected := range want {
		p.observeTimeout()
		if got := p.window(); got != expected {
			t.Fatalf("after timeout %d window = %s, want %s", i+1, got, expected)
		}
	}
	p.observeReset()
	if got := p.window(); got != walPassiveCheckpointTimeout {
		t.Fatalf("after reset window = %s, want base %s", got, walPassiveCheckpointTimeout)
	}
}

func TestWALCheckpointWindowFromEnv(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{name: "unset falls open to default", value: "", want: walPassiveCheckpointTimeout},
		{name: "milliseconds honored", value: "2500", want: 2500 * time.Millisecond},
		{name: "surrounding space trimmed", value: " 1500 ", want: 1500 * time.Millisecond},
		{name: "garbage falls open to default", value: "abc", want: walPassiveCheckpointTimeout},
		{name: "negative falls open to default", value: "-5", want: walPassiveCheckpointTimeout},
		{name: "zero falls open to default", value: "0", want: walPassiveCheckpointTimeout},
		{name: "absurd value clamps", value: "99999999", want: maxPassiveCheckpointWindow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GORTEX_WAL_CHECKPOINT_WINDOW_MS", test.value)
			if got := walCheckpointWindowFromEnv(); got != test.want {
				t.Fatalf("walCheckpointWindowFromEnv() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestObservePassiveCheckpointOutcomeClassifiesDeadline(t *testing.T) {
	s := &Store{storeCore: &storeCore{}}

	// An execute-phase deadline is the only outcome worth a wider window:
	// the PRAGMA started and was killed mid-fold, so the retry must get
	// more room instead of the same one-second lottery.
	s.observePassiveCheckpointOutcome(context.DeadlineExceeded, nil)
	if got, want := s.passiveWindow.window(), 2*walPassiveCheckpointTimeout; got != want {
		t.Fatalf("window after deadline = %s, want %s", got, want)
	}
	// A context-level deadline reported alongside a driver error grows too.
	s.observePassiveCheckpointOutcome(errors.New("driver stopped"), context.DeadlineExceeded)
	if got, want := s.passiveWindow.window(), 4*walPassiveCheckpointTimeout; got != want {
		t.Fatalf("window after ctx deadline = %s, want %s", got, want)
	}

	// Every other outcome — success, incomplete, hard driver failure —
	// resets, so one slow fold cannot permanently widen the window.
	s.observePassiveCheckpointOutcome(errors.New("disk I/O error"), nil)
	if got, want := s.passiveWindow.window(), walPassiveCheckpointTimeout; got != want {
		t.Fatalf("window after hard error = %s, want base %s", got, want)
	}
}

func TestCheckpointWALPassiveResetsWindowOnWriterGateDeferral(t *testing.T) {
	s, _ := openTempStore(t)
	s.passiveWindow = newPassiveCheckpointWindowPolicy(walPassiveCheckpointTimeout, walCheckpointRetryMax)
	s.passiveWindow.observeTimeout()
	if got, want := s.passiveWindow.window(), 2*walPassiveCheckpointTimeout; got != want {
		t.Fatalf("precondition: window = %s, want %s", got, want)
	}

	s.writeMu.Lock()
	captureLog(t, func() { s.checkpointWALPassive() })
	s.writeMu.Unlock()

	// A writer-gate deferral never started executing, so it must not keep
	// a grown window alive for a burst that may never time out again.
	if got, want := s.passiveWindow.window(), walPassiveCheckpointTimeout; got != want {
		t.Fatalf("window after writer-gate deferral = %s, want base %s", got, want)
	}
}
