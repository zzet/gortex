package graph

import (
	"testing"
	"time"
)

// A write that waits for the delta's write lock is accounted; an uncontended
// one is not.
func TestDeltaWriterAccountsWriteLockWait(t *testing.T) {
	dw := NewDeltaWriter(New(), nil)
	held := 0
	dw.writeMu.Lock()
	held++
	dw.writeMu.Unlock()
	if w := dw.WriteLockWait(); w != 0 || held != 1 {
		t.Fatalf("an uncontended lock accounted %s of wait", w)
	}
	dw.writeMu.Lock()
	done := make(chan struct{})
	var got int
	go func() {
		dw.writeMu.Lock()
		got = held
		dw.writeMu.Unlock()
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	dw.writeMu.Unlock()
	<-done
	if got != 1 {
		t.Fatal("the waiting write did not run after the lock was released")
	}
	if w := dw.WriteLockWait(); w < 40*time.Millisecond {
		t.Fatalf("a write held back 50ms accounted %s of wait", w)
	}
}
