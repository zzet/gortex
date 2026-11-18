package store_sqlite

import "testing"

// BenchmarkWriteGateLockUnlock measures the write gate's own cost for an
// ordinary holder, alone and with every P contending, since every store
// mutation pays it. The holder bumps a counter so the hold is not empty.
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
}
