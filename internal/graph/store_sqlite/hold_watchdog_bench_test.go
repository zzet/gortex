package store_sqlite

import (
	"testing"
	"time"
)

// BenchmarkHoldRecord is the per-hold cost the watchdog adds to every write
// gate and read transaction while it is enabled.
func BenchmarkHoldRecord(b *testing.B) {
	prev := holdWatchdogThreshold
	holdWatchdogThreshold = 2 * time.Second
	defer func() { holdWatchdogThreshold = prev }()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = newHoldRecord()
	}
}
