package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The retirement edit-yield counters reach the status payload and render on
// the storage block beside the edit-cycle yield counters.
func TestStatusRendersTheRetirementEditYieldCounters(t *testing.T) {
	reclaim := walReclaimStatus(store_sqlite.WALReclaimStats{
		ThresholdBytes: 256 << 20, RetirementEditYields: 9, RetirementEditYieldTimeouts: 3,
	})
	if reclaim.RetirementEditYields != 9 || reclaim.RetirementEditYieldTimeouts != 3 {
		t.Fatalf("counters lost on the way to the payload: %+v", reclaim)
	}
	st := countersStatus()
	st.Storage = &daemon.StorageStatus{DBBytes: 1 << 30, WALBytes: 1 << 20, WALReclaim: reclaim}
	var buf bytes.Buffer
	if err := renderDaemonStatusTo(&buf, st, "text"); err != nil {
		t.Fatal(err)
	}
	if want := "retirement edit yield: waits=9  timeouts=3"; !strings.Contains(buf.String(), want) {
		t.Fatalf("status text lacks %q:\n%s", want, buf.String())
	}
}
