package store_sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func openAttributionStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "attribution.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	if reason := s.writerAttributionUnavailable(); reason != "" {
		t.Skip("writer attribution unavailable: " + reason)
	}
	return s
}

// attributionProbeWrite inserts one row of n bytes in its own write-gate hold.
func attributionProbeWrite(s *Store, n int) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.writerDB.Exec(`INSERT INTO attribution_probe(payload) VALUES (?)`, make([]byte, n))
	return err
}

// attributedWriter runs writes, one per size received, inside a on its own
// goroutine, until turns closes; finished closes when the attribution has.
type attributedWriter struct {
	turns    chan int
	acks     chan error
	finished chan struct{}
}

func startAttributedWriter(s *Store, a *WriterAttribution) attributedWriter {
	w := attributedWriter{turns: make(chan int), acks: make(chan error), finished: make(chan struct{})}
	go func() {
		defer close(w.finished)
		AttributeWriterWrites(a, func() {
			for n := range w.turns {
				w.acks <- attributionProbeWrite(s, n)
			}
		})
	}()
	return w
}

// Two attributions open at once on different goroutines write to one store
// in strict alternation, with an unattributed writer committing a larger
// transaction between their turns: each keeps exactly its own holds and the
// log bytes written under them.
func TestWriterAttributionOwnsOnlyItsGoroutinesHolds(t *testing.T) {
	s := openAttributionStore(t)
	s.writeMu.Lock()
	_, err := s.writerDB.Exec(`CREATE TABLE attribution_probe (payload BLOB)`)
	s.writeMu.Unlock()
	require.NoError(t, err)

	const large, small, foreign = 256 << 10, 1 << 10, 2 << 20
	a, b := &WriterAttribution{}, &WriterAttribution{}
	a.BindStore(s)
	b.BindStore(s)
	wa, wb := startAttributedWriter(s, a), startAttributedWriter(s, b)
	turn := func(w attributedWriter, n int) {
		w.turns <- n
		require.NoError(t, <-w.acks)
	}
	turn(wa, large)
	require.NoError(t, attributionProbeWrite(s, foreign)) // this goroutine carries no attribution
	turn(wb, small)
	turn(wa, large)
	turn(wb, small)
	close(wa.turns)
	close(wb.turns)
	<-wa.finished
	<-wb.finished

	ra, rb := a.Snapshot(), b.Snapshot()
	t.Logf("a=%+v b=%+v", ra, rb)
	for name, r := range map[string]WriterAttributionSnapshot{"a": ra, "b": rb} {
		require.True(t, r.Available, "%s: %s", name, r.Reason)
		require.EqualValues(t, 2, r.Holds, name)
		require.Zero(t, r.OpenHolds, name)
		require.Positive(t, r.WALWrites, name)
		require.LessOrEqual(t, r.WriteTime(), r.HoldTime, "%s: xWrite runs inside the holds", name)
		require.False(t, r.LastHoldEnd.Before(r.FirstHoldStart), name)
		require.Less(t, r.MainWriteBytes+r.WALWriteBytes, int64(foreign), "%s never counts the unattributed commit", name)
		// The unattributed commit and the other attribution's two holds.
		require.GreaterOrEqual(t, r.UnownedStoreHolds, int64(3), name)
	}
	require.GreaterOrEqual(t, ra.WALWriteBytes, int64(2*large), "a keeps both of its large rows")
	require.Less(t, rb.MainWriteBytes+rb.WALWriteBytes, int64(large), "b never counts a's rows")
}

// An attribution that cannot be complete says why, rather than reading as a
// measured zero; one that is complete and wrote nothing is a measured zero.
func TestWriterAttributionUnavailableReasons(t *testing.T) {
	file, other := openAttributionStore(t), openAttributionStore(t)
	memory, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, memory.Close()) })
	for _, tc := range []struct {
		name, reason string
		run          func(*WriterAttribution)
	}{
		{"measured_zero", "", func(a *WriterAttribution) {
			a.BindStore(file)
			AttributeWriterWrites(a, func() {})
		}},
		{"not_sqlite", "not_a_sqlite_store", func(a *WriterAttribution) { a.BindStore(nil) }},
		{"shared_connection", "no_dedicated_writer_connection", func(a *WriterAttribution) { a.BindStore(memory) }},
		{"never_bound", "store_not_bound", func(a *WriterAttribution) { AttributeWriterWrites(a, func() {}) }},
		{"second_store", "writes_to_multiple_stores", func(a *WriterAttribution) {
			a.BindStore(file)
			AttributeWriterWrites(a, func() {
				release, err := other.HoldWriteGate(context.Background())
				require.NoError(t, err)
				release()
			})
		}},
		{"hold_open_at_return", "owned_hold_open_at_return", func(a *WriterAttribution) {
			a.BindStore(file)
			var release func()
			AttributeWriterWrites(a, func() {
				var err error
				release, err = file.HoldWriteGate(context.Background())
				require.NoError(t, err)
			})
			release()
		}},
		{"slots_exhausted", "attribution_slots_exhausted", func(a *WriterAttribution) {
			filler := &WriterAttribution{}
			for i := range writerAttributions.slots {
				if writerAttributions.slots[i].CompareAndSwap(nil, filler) {
					defer writerAttributions.slots[i].Store(nil)
				}
			}
			a.BindStore(file)
			AttributeWriterWrites(a, func() {})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &WriterAttribution{}
			tc.run(a)
			got := a.Snapshot()
			require.Equal(t, tc.reason, got.Reason)
			require.Equal(t, tc.reason == "", got.Available)
			if got.Available {
				require.Zero(t, got.Holds)
				require.Zero(t, got.WriteTime())
			}
		})
	}
}
