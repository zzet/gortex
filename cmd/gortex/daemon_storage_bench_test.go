package main

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// BenchmarkStorageStatusBlock measures the actual cheap physical-store block,
// including its existing filesystem observations and both counter snapshots.
// Run exclusively for timing evidence; ordinary test runs do not execute it.
func BenchmarkStorageStatusBlock(b *testing.B) {
	store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "status.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Error(err)
		}
	})
	samples := make([]time.Duration, 0, 10000)
	b.ReportAllocs()
	for b.Loop() {
		started := time.Now()
		block := storageStatusFor(store)
		samples = append(samples, time.Since(started))
		if block == nil || block.WALReclaim == nil || block.WALCheckpoint == nil {
			b.Fatal("physical-store counters missing")
		}
	}
	if len(samples) > 0 {
		slices.Sort(samples)
		rank := (99*len(samples)+99)/100 - 1
		b.ReportMetric(float64(samples[rank]), "p99-ns")
	}
}
