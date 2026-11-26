package store_sqlite

import (
	"path/filepath"
	"testing"
)

// BenchmarkWriteGateLockUnlock measures the write gate's own cost for an
// ordinary holder, alone and with every P contending, since every store
// mutation pays it, and what a writer attribution (writer_attribution.go)
// adds to a store writer gate's hold while one is open on another goroutine
// and while it owns the hold. The holder bumps a counter so the hold is not
// empty.
func BenchmarkWriteGateLockUnlock(b *testing.B) {
	b.Run("uncontended", func(b *testing.B) {
		var g sqliteWriteGate
		held := 0
		for b.Loop() {
			g.Lock()
			held++
			g.Unlock()
		}
		_ = held
	})
	b.Run("parallel", func(b *testing.B) {
		var g sqliteWriteGate
		held := 0
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				g.Lock()
				held++
				g.Unlock()
			}
		})
		_ = held
	})
	for _, mode := range []string{"attribution_open_elsewhere", "attribution_owned"} {
		b.Run(mode, func(b *testing.B) {
			benchmarkStore(b) // installs the VFS hooks an owned hold needs
			var g sqliteWriteGate
			g.writerTLS.Store(1) // a store writer gate
			held := 0
			loop := func() {
				for b.Loop() {
					g.Lock()
					held++
					g.Unlock()
				}
			}
			runWithAttribution(b, mode, loop)
			_ = held
		})
	}
}

// BenchmarkWriterAttributionStoreWrite measures one small committed write
// through a store's writer gate, with no attribution open, with one open on
// another goroutine, and inside the attribution that owns its hold: the
// relative cost an attempt's attribution adds to real SQLite writes.
func BenchmarkWriterAttributionStoreWrite(b *testing.B) {
	for _, mode := range []string{"no_attribution", "attribution_open_elsewhere", "attribution_owned"} {
		b.Run(mode, func(b *testing.B) {
			s := benchmarkStore(b)
			s.writeMu.Lock()
			_, err := s.writerDB.Exec(`CREATE TABLE IF NOT EXISTS attribution_probe (payload BLOB)`)
			s.writeMu.Unlock()
			if err != nil {
				b.Fatal(err)
			}
			payload := make([]byte, 256)
			loop := func() {
				for b.Loop() {
					s.writeMu.Lock()
					_, err := s.writerDB.Exec(`INSERT INTO attribution_probe(payload) VALUES (?)`, payload)
					s.writeMu.Unlock()
					if err != nil {
						b.Fatal(err)
					}
				}
			}
			if mode == "no_attribution" {
				loop()
				return
			}
			runWithAttribution(b, mode, loop)
		})
	}
}

func benchmarkStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "bench.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if reason := s.writerAttributionUnavailable(); reason != "" {
		b.Skip("writer attribution unavailable: " + reason)
	}
	return s
}

// runWithAttribution runs loop on this goroutine with an attribution open on
// a parked goroutine (attribution_open_elsewhere) or around loop itself
// (attribution_owned).
func runWithAttribution(b *testing.B, mode string, loop func()) {
	b.Helper()
	if mode == "attribution_owned" {
		a := &WriterAttribution{}
		AttributeWriterWrites(a, loop)
		if got := a.Snapshot(); got.Holds == 0 {
			b.Fatalf("holds not attributed: %+v", got)
		}
		return
	}
	parked, release := make(chan struct{}), make(chan struct{})
	go AttributeWriterWrites(&WriterAttribution{}, func() { close(parked); <-release })
	<-parked
	defer close(release)
	loop()
}
