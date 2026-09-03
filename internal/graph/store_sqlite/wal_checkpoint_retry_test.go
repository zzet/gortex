package store_sqlite

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPassiveCheckpointTransientDeferralsRequestRetry(t *testing.T) {
	t.Run("writer gate", func(t *testing.T) {
		s, _ := openTempStore(t)
		s.writeMu.Lock()
		var retry bool
		out := captureLog(t, func() { retry = s.checkpointWALPassive() })
		s.writeMu.Unlock()
		if !retry || !strings.Contains(out, "reason=writer_gate") {
			t.Fatalf("writer-gate checkpoint = retry %t log %q", retry, out)
		}
	})

	t.Run("bulk writer", func(t *testing.T) {
		s, _ := openTempStore(t)
		s.BeginBulkLoad()
		if s.bulkConn == nil {
			t.Fatal("bulk writer did not engage")
		}
		var retry bool
		out := captureLog(t, func() { retry = s.checkpointWALPassive() })
		if !retry || !strings.Contains(out, "reason=bulk_writer") {
			t.Fatalf("bulk-writer checkpoint = retry %t log %q", retry, out)
		}
		if err := s.AbortCoordinatedBulkLoad(); err != nil {
			t.Fatalf("abort bulk writer: %v", err)
		}
	})

	t.Run("complete", func(t *testing.T) {
		s, _ := openTempStore(t)
		if retry := s.checkpointWALPassive(); retry {
			t.Fatal("complete passive checkpoint requested retry")
		}
	})
}

func TestPassiveCheckpointRetryClassification(t *testing.T) {
	plainTimeout := timeoutOnlyError{}
	tests := []struct {
		name   string
		err    error
		ctxErr error
		want   bool
	}{
		{name: "success", want: false},
		{name: "incomplete", err: errSQLiteCheckpointIncomplete, want: true},
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "context deadline", err: errors.New("driver stopped"), ctxErr: context.DeadlineExceeded, want: true},
		{name: "plain timeout is not sqlite contention", err: plainTimeout, want: false},
		{name: "disk IO", err: errors.New("disk I/O error"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldRetryPassiveCheckpoint(test.err, test.ctxErr); got != test.want {
				t.Fatalf("shouldRetryPassiveCheckpoint(%v, %v) = %t, want %t", test.err, test.ctxErr, got, test.want)
			}
		})
	}
}

type timeoutOnlyError struct{}

func (timeoutOnlyError) Error() string   { return "non-SQLite timeout" }
func (timeoutOnlyError) Timeout() bool   { return true }
func (timeoutOnlyError) Temporary() bool { return true }

func TestWALCheckpointRetryBackoffCaps(t *testing.T) {
	delay := time.Second
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, expected := range want {
		delay = growWALCheckpointRetry(delay, 30*time.Second)
		if delay != expected {
			t.Fatalf("retry step %d = %s, want %s", i, delay, expected)
		}
	}
}

func TestCheckpointLoopRetriesPromptlyThenReturnsToNormalCadence(t *testing.T) {
	s := &Store{storeCore: &storeCore{
		stopCheckpoint: make(chan struct{}),
		checkpointDone: make(chan struct{}),
	}}
	var attempts atomic.Int32
	go s.runCheckpointLoopWithAttempt(
		time.Millisecond,
		time.Millisecond,
		5*time.Millisecond,
		func() bool {
			n := attempts.Add(1)
			if n == 2 {
				close(s.stopCheckpoint)
			}
			return n == 1
		},
	)
	select {
	case <-s.checkpointDone:
	case <-time.After(time.Second):
		t.Fatal("checkpoint loop did not perform prompt retry")
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("checkpoint attempts = %d, want transient attempt plus one retry", got)
	}
}

func TestCheckpointLoopStopCancelsPendingRetry(t *testing.T) {
	s := &Store{storeCore: &storeCore{
		stopCheckpoint: make(chan struct{}),
		checkpointDone: make(chan struct{}),
	}}
	started := make(chan struct{})
	var attempts atomic.Int32
	go s.runCheckpointLoopWithAttempt(
		time.Millisecond,
		time.Hour,
		time.Hour,
		func() bool {
			if attempts.Add(1) == 1 {
				close(started)
			}
			return true
		},
	)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("checkpoint loop did not start first attempt")
	}
	close(s.stopCheckpoint)
	select {
	case <-s.checkpointDone:
	case <-time.After(time.Second):
		t.Fatal("checkpoint loop did not stop with retry pending")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("checkpoint attempts after stop = %d, want 1", got)
	}
}

func BenchmarkWALCheckpointRetryPolicy(b *testing.B) {
	delay := time.Second
	b.ReportAllocs()
	for range b.N {
		delay = growWALCheckpointRetry(delay, 30*time.Second)
		if delay == 30*time.Second {
			delay = time.Second
		}
	}
}
