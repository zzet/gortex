package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestShouldCompactStore pins the boot-compaction trigger: all three gates
// (majority-dead file, absolute reclaimable floor, disk headroom for the
// VACUUM copy) must hold, and each boundary is exclusive — equality on any
// gate means "don't". Pure-function table so the policy is exercised without
// a store or a filesystem.
func TestShouldCompactStore(t *testing.T) {
	const (
		gib = int64(1) << 30
		tib = uint64(1) << 40
	)
	cases := []struct {
		name  string
		free  int64
		total int64
		avail uint64
		want  bool
	}{
		{name: "all gates hold", free: 2 * gib, total: 3 * gib, avail: tib, want: true},
		{name: "the observed live store shape (4.4/6.8 GB)", free: 4707074048, total: 7301444403, avail: tib, want: true},

		// Fraction gate: freelist must be a strict MAJORITY of the file.
		{name: "exactly half free — no", free: 2 * gib, total: 4 * gib, avail: tib, want: false},
		{name: "just under half free — no", free: 2*gib - 1, total: 4 * gib, avail: tib, want: false},
		{name: "just over half free — yes", free: 2*gib + 1, total: 4 * gib, avail: tib, want: true},

		// Absolute floor: a small file's majority is still not worth minutes
		// of exclusive I/O.
		{name: "90% free but only 900 MiB — no", free: 900 << 20, total: 1 << 30, avail: tib, want: false},
		{name: "exactly 1 GiB free — no (floor is exclusive)", free: gib, total: gib + 2, avail: tib, want: false},

		// Headroom gate: available disk must strictly exceed total × 1.5,
		// because VACUUM transiently needs up to a full extra copy.
		{name: "avail exactly 1.5× — no", free: 2 * gib, total: 3 * gib, avail: uint64(3*gib) + uint64(3*gib)/2, want: false},
		{name: "avail just over 1.5× — yes", free: 2 * gib, total: 3 * gib, avail: uint64(3*gib) + uint64(3*gib)/2 + 1, want: true},
		{name: "tight disk — no", free: 2 * gib, total: 3 * gib, avail: uint64(3 * gib), want: false},

		// Degenerate inputs: an unreadable store reports zeros; never fire.
		{name: "zero stats", free: 0, total: 0, avail: tib, want: false},
		{name: "zero free", free: 0, total: 4 * gib, avail: tib, want: false},
		{name: "zero total", free: 2 * gib, total: 0, avail: tib, want: false},
		{name: "negative total", free: 2 * gib, total: -1, avail: tib, want: false},
		{name: "zero avail", free: 2 * gib, total: 3 * gib, avail: 0, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldCompactStore(tc.free, tc.total, tc.avail); got != tc.want {
				t.Errorf("shouldCompactStore(free=%d, total=%d, avail=%d) = %v, want %v",
					tc.free, tc.total, tc.avail, got, tc.want)
			}
		})
	}
}

// The store the daemon actually runs against must be the thing the boot
// compaction probes for. This is the wiring link between warmupDaemonState's
// maybeCompactStore(state.graph, …) call and the maintenance lane: a *Store
// that stopped satisfying storeCompactor would silently make boot compaction a
// no-op instead of failing to build.
var _ storeCompactor = (*store_sqlite.Store)(nil)

// compactStub is a graph.Store whose only real methods are the compaction
// capability. The embedded nil interface satisfies the rest: maybeCompactStore
// touches nothing else, and a call that reached through would panic loudly
// rather than pass silently.
type compactStub struct {
	graph.Store
	free, total int64
	path        string
	compactErr  error
	compacts    int
}

func (c *compactStub) Path() string                      { return c.path }
func (c *compactStub) CompactStats() (free, total int64) { return c.free, c.total }
func (c *compactStub) Compact() error                    { c.compacts++; return c.compactErr }

// A deferral is not a failure. The maintenance lane refuses to rewrite the file
// underneath a publish or a build, and boot must report that as the routine
// yield it is — the freelist is still there to reclaim next boot. Logging it at
// warn is what would make the real warning (a VACUUM that tried and lost)
// unreadable.
func TestMaybeCompactStore_DeferralIsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	// The smallest shape that clears all three trigger gates: the freelist is
	// over the 1 GiB floor and the majority of a 2 GiB file, so the headroom
	// gate asks for 3 GiB of free space rather than a machine-sized figure.
	const (
		total = int64(2) << 30
		free  = int64(1)<<30 + 1
	)

	cases := []struct {
		name      string
		err       error
		wantLevel zapcore.Level
		wantMsg   string
	}{
		{
			name:      "lane deferral",
			err:       fmt.Errorf("%w: vacuum: payload build in flight: generation 7", store_sqlite.ErrMaintenanceBusy),
			wantLevel: zapcore.InfoLevel,
			wantMsg:   "daemon: store compaction deferred — the store was busy",
		},
		{
			name:      "real failure",
			err:       errors.New("disk I/O error"),
			wantLevel: zapcore.WarnLevel,
			wantMsg:   "daemon: store compaction failed — continuing boot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			stub := &compactStub{
				free:       free,
				total:      total,
				path:       filepath.Join(dir, "graph.sqlite"),
				compactErr: tc.err,
			}
			maybeCompactStore(stub, zap.New(core))

			if stub.compacts != 1 {
				t.Fatalf("Compact called %d times, want 1: the trigger must have fired", stub.compacts)
			}
			entries := logs.FilterMessage(tc.wantMsg).All()
			if len(entries) != 1 {
				t.Fatalf("want exactly one %q entry, got %d (all: %v)", tc.wantMsg, len(entries), logs.All())
			}
			if entries[0].Level != tc.wantLevel {
				t.Errorf("%q logged at %v, want %v", tc.wantMsg, entries[0].Level, tc.wantLevel)
			}
		})
	}
}
