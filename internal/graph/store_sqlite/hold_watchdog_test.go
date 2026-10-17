package store_sqlite

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"
)

// With the watchdog on, a write-gate hold and a read connection's active
// interval past the threshold are each reported once, naming the holder's
// stack (and the reader's statement); a released hold is not reported, and
// with the watchdog off nothing is recorded.
func TestHoldWatchdogNamesTheHolder(t *testing.T) {
	var out bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	previous := holdWatchdogThreshold
	t.Cleanup(func() { holdWatchdogThreshold = previous })

	holdWatchdogThreshold = 0
	var off sqliteWriteGate
	off.Lock()
	if off.holder.Load() != nil {
		t.Fatal("a hold was recorded with the watchdog off")
	}
	off.Unlock()

	holdWatchdogThreshold = 20 * time.Millisecond
	core := &storeCore{readGate: newSQLiteReadGate()}
	watchHolds(core)
	t.Cleanup(func() { unwatchHolds(core) })
	core.writeMu.Lock()
	reader := &gatedConn{gate: core.readGate}
	if err := reader.enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader.noteStatement("SELECT id FROM nodes WHERE file_path = ?")
	released := &gatedConn{gate: core.readGate}
	if err := released.enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	released.release()

	sweepHolds(time.Now())
	if out.Len() != 0 {
		t.Fatalf("a hold under the threshold was reported: %s", out.String())
	}
	later := time.Now().Add(time.Second)
	sweepHolds(later)
	sweepHolds(later)
	got := out.String()
	if strings.Count(got, "write gate held") != 1 || strings.Count(got, "read transaction held") != 1 {
		t.Fatalf("want one report of each hold, got:\n%s", got)
	}
	if !strings.Contains(got, "TestHoldWatchdogNamesTheHolder") || !strings.Contains(got, "SELECT id FROM nodes") {
		t.Fatalf("the report does not name the holder or its statement:\n%s", got)
	}
	core.writeMu.Unlock()
	reader.release()
	if core.writeMu.holder.Load() != nil || reader.hold.Load() != nil {
		t.Fatal("a released hold stayed recorded")
	}
	got = out.String()
	if strings.Count(got, "write gate released after") != 1 || strings.Count(got, "read transaction released after") != 1 {
		t.Fatalf("want each reported hold's total logged once at release, got:\n%s", got)
	}
	// A hold never reported (under the threshold) logs nothing at release.
	out.Reset()
	if !core.writeMu.TryLock() {
		t.Fatal("the write gate stayed held")
	}
	core.writeMu.Unlock()
	if out.Len() != 0 {
		t.Fatalf("an unreported hold logged at release: %s", out.String())
	}
}
