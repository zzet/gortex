package indexer

import (
	"errors"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/runtimeactivity"
)

func TestIndexMemoryBudget(t *testing.T) {
	const (
		gib = uint64(1) << 30
		mib = uint64(1) << 20
	)
	tests := []struct {
		name        string
		hostRAM     uint64
		cgroupLimit uint64
		want        int64
	}{
		{
			name:    "host ram only, no cgroup",
			hostRAM: 32 * gib,
			want:    int64(16 * gib),
		},
		{
			name:        "cgroup below host wins",
			hostRAM:     32 * gib,
			cgroupLimit: 4 * gib,
			want:        int64(2 * gib),
		},
		{
			name:        "cgroup above host falls back to host",
			hostRAM:     8 * gib,
			cgroupLimit: 64 * gib,
			want:        int64(4 * gib),
		},
		{
			name:        "cgroup present, host unknown",
			hostRAM:     0,
			cgroupLimit: 6 * gib,
			want:        int64(3 * gib),
		},
		{
			name:    "host unknown and no cgroup yields no budget",
			hostRAM: 0,
			want:    0,
		},
		{
			name:    "zero everything yields no budget",
			hostRAM: 0,
			want:    0,
		},
		{
			name:        "absurd cgroup sentinel ignored, host used",
			hostRAM:     16 * gib,
			cgroupLimit: 1<<63 - 1, // cgroup v1 uncapped sentinel-class value
			want:        int64(8 * gib),
		},
		{
			name:        "absurd cgroup and unknown host yields no budget",
			hostRAM:     0,
			cgroupLimit: 1<<63 - 1,
			want:        0,
		},
		{
			name:    "tiny host below floor yields no budget",
			hostRAM: 512 * mib, // half == 256 MiB, below the 512 MiB floor
			want:    0,
		},
		{
			name:        "tiny cgroup below floor yields no budget",
			hostRAM:     32 * gib,
			cgroupLimit: 256 * mib,
			want:        0,
		},
		{
			name:    "exactly at floor is kept",
			hostRAM: 1 * gib, // half == 512 MiB == floor
			want:    int64(512 * mib),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := indexMemoryBudget(tt.hostRAM, tt.cgroupLimit)
			if got != tt.want {
				t.Fatalf("indexMemoryBudget(%d, %d) = %d, want %d",
					tt.hostRAM, tt.cgroupLimit, got, tt.want)
			}
		})
	}
}

func TestParseCgroupMemoryLimit(t *testing.T) {
	const path = "/sys/fs/cgroup/memory.max"
	reader := func(body string, err error) func(string) ([]byte, error) {
		return func(string) ([]byte, error) {
			if err != nil {
				return nil, err
			}
			return []byte(body), nil
		}
	}

	tests := []struct {
		name   string
		read   func(string) ([]byte, error)
		want   uint64
		wantOK bool
	}{
		{name: "valid numeric", read: reader("4294967296\n", nil), want: 4294967296, wantOK: true},
		{name: "literal max", read: reader("max\n", nil), wantOK: false},
		{name: "empty body", read: reader("", nil), wantOK: false},
		{name: "missing file", read: reader("", errors.New("no such file")), wantOK: false},
		{name: "zero value", read: reader("0", nil), wantOK: false},
		{name: "non-numeric", read: reader("garbage", nil), wantOK: false},
		{name: "absurd sentinel", read: reader("9223372036854771712", nil), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCgroupMemoryLimit(tt.read, path)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Fatalf("parseCgroupMemoryLimit() = (%d, %v), want (%d, %v)",
					got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestCgroupMemoryLimitFrom(t *testing.T) {
	// v2 present and capped: used directly, v1 never consulted.
	got := cgroupMemoryLimitFrom(func(p string) ([]byte, error) {
		if p == "/proc/self/cgroup" {
			return nil, os.ErrNotExist
		}
		if p == "/sys/fs/cgroup/memory.max" {
			return []byte("2147483648"), nil
		}
		t.Fatalf("v1 path should not be read when v2 is capped, got read of %q", p)
		return nil, nil
	})
	if got != 2147483648 {
		t.Fatalf("v2 capped: got %d, want 2147483648", got)
	}

	// v2 reports "max" (uncapped): fall through to v1.
	got = cgroupMemoryLimitFrom(func(p string) ([]byte, error) {
		switch p {
		case "/sys/fs/cgroup/memory.max":
			return []byte("max"), nil
		case "/sys/fs/cgroup/memory/memory.limit_in_bytes":
			return []byte("1073741824"), nil
		}
		return nil, errors.New("unexpected path")
	})
	if got != 1073741824 {
		t.Fatalf("v2 uncapped -> v1: got %d, want 1073741824", got)
	}

	// Neither hierarchy present: no limit.
	got = cgroupMemoryLimitFrom(func(string) ([]byte, error) {
		return nil, errors.New("no such file")
	})
	if got != 0 {
		t.Fatalf("no cgroup: got %d, want 0", got)
	}
}

func TestGCTuneEnabled(t *testing.T) {
	tests := []struct {
		val  string
		set  bool
		want bool
	}{
		{set: false, want: true}, // unset -> default on
		{val: "1", set: true, want: true},
		{val: "0", set: true, want: false},
		{val: "false", set: true, want: false},
		{val: "FALSE", set: true, want: false},
		{val: "true", set: true, want: true},
		{val: "anything", set: true, want: true},
	}
	for _, tt := range tests {
		name := "unset"
		if tt.set {
			name = tt.val
		}
		t.Run(name, func(t *testing.T) {
			if tt.set {
				t.Setenv("GORTEX_INDEX_GC_TUNE", tt.val)
			} else {
				os.Unsetenv("GORTEX_INDEX_GC_TUNE")
			}
			if got := gcTuneEnabled(); got != tt.want {
				t.Fatalf("gcTuneEnabled() with %q = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}

func TestIndexGCPercent(t *testing.T) {
	tests := []struct {
		val  string
		set  bool
		want int
	}{
		{set: false, want: defaultIndexGCPercent},
		{val: "150", set: true, want: 150},
		{val: "0", set: true, want: defaultIndexGCPercent},   // non-positive rejected
		{val: "-1", set: true, want: defaultIndexGCPercent},  // negative rejected
		{val: "abc", set: true, want: defaultIndexGCPercent}, // non-numeric rejected
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			if tt.set {
				t.Setenv("GORTEX_INDEX_GC_PERCENT", tt.val)
			} else {
				os.Unsetenv("GORTEX_INDEX_GC_PERCENT")
			}
			if got := indexGCPercent(); got != tt.want {
				t.Fatalf("indexGCPercent() with %q = %d, want %d", tt.val, got, tt.want)
			}
		})
	}
}

// TestApplyIndexGCTuningRestores asserts the tuning round-trips: after the
// returned closure runs, the GC percent and memory limit are exactly what they
// were before applyIndexGCTuning was called.
func TestApplyIndexGCTuningRestores(t *testing.T) {
	// Pin a known prior GC percent for the duration of the test, then restore
	// it at the end regardless of the assertions below.
	const priorPct = 137
	originalPct := debug.SetGCPercent(priorPct)
	defer debug.SetGCPercent(originalPct)
	originalLimit := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(originalLimit)
	// Pin both inputs the GC-percent knob keys on instead of inheriting them
	// from the host: an unbounded standing limit, and a budget the window can
	// own. Reading the budget from the machine makes the assertion below a
	// property of the runner — a host with no physical-memory reader (Windows)
	// derives no budget at all, and the knob is then never installed.
	standingLimit := int64(math.MaxInt64)
	debug.SetMemoryLimit(standingLimit)
	const windowBudget = int64(2) << 30

	t.Setenv("GORTEX_INDEX_GC_TUNE", "1")
	t.Setenv("GORTEX_INDEX_GC_PERCENT", "275")

	restore := applyIndexGCTuningWithBudget(nil, windowBudget)

	// While tuned, the GC percent must be the configured value, not the prior.
	if cur := debug.SetGCPercent(275); cur != 275 {
		// SetGCPercent(275) returns the value that was in effect — should be
		// 275 because the tuning installed it. (We immediately put it back.)
		debug.SetGCPercent(cur)
		t.Fatalf("expected GC percent 275 while tuned, got %d", cur)
	}
	debug.SetGCPercent(275)

	restore()

	// After restore, the GC percent is back to the pinned prior.
	if cur := debug.SetGCPercent(priorPct); cur != priorPct {
		debug.SetGCPercent(cur)
		t.Fatalf("expected GC percent restored to %d, got %d", priorPct, cur)
	}
	debug.SetGCPercent(priorPct)

	// And the memory limit is back to the standing limit the window captured.
	if cur := debug.SetMemoryLimit(-1); cur != standingLimit {
		t.Fatalf("expected memory limit restored to %d, got %d", standingLimit, cur)
	}
}

// TestApplyIndexGCTuningDisabled asserts that with tuning disabled the runtime
// knobs are left untouched and the returned closure is a harmless no-op.
func TestApplyIndexGCTuningDisabled(t *testing.T) {
	const priorPct = 91
	originalPct := debug.SetGCPercent(priorPct)
	defer debug.SetGCPercent(originalPct)

	t.Setenv("GORTEX_INDEX_GC_TUNE", "0")

	restore := applyIndexGCTuning(nil)
	if cur := debug.SetGCPercent(priorPct); cur != priorPct {
		debug.SetGCPercent(cur)
		t.Fatalf("tuning disabled should not change GC percent; got %d, want %d", cur, priorPct)
	}
	restore() // must not panic or alter state
	if cur := debug.SetGCPercent(priorPct); cur != priorPct {
		debug.SetGCPercent(cur)
		t.Fatalf("no-op restore changed GC percent; got %d, want %d", cur, priorPct)
	}
}

func TestCPUQuotaCores(t *testing.T) {
	tests := []struct {
		name   string
		quota  int64
		period int64
		want   int
		wantOK bool
	}{
		{name: "one and a half cores rounds up", quota: 150000, period: 100000, want: 2, wantOK: true},
		{name: "exactly two cores", quota: 200000, period: 100000, want: 2, wantOK: true},
		{name: "exactly one core", quota: 100000, period: 100000, want: 1, wantOK: true},
		{name: "half a core floored to one", quota: 50000, period: 100000, want: 1, wantOK: true},
		{name: "tiny fraction floored to one", quota: 1, period: 100000, want: 1, wantOK: true},
		{name: "zero quota rejected", quota: 0, period: 100000, wantOK: false},
		{name: "negative quota rejected", quota: -1, period: 100000, wantOK: false},
		{name: "zero period guarded", quota: 100000, period: 0, wantOK: false},
		{name: "negative period guarded", quota: 100000, period: -100, wantOK: false},
		{name: "absurd quota rejected", quota: 1 << 40, period: 1, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cpuQuotaCores(tt.quota, tt.period)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Fatalf("cpuQuotaCores(%d, %d) = (%d, %v), want (%d, %v)",
					tt.quota, tt.period, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseCgroupCPUMaxV2(t *testing.T) {
	const path = "/sys/fs/cgroup/cpu.max"
	reader := func(body string, err error) func(string) ([]byte, error) {
		return func(string) ([]byte, error) {
			if err != nil {
				return nil, err
			}
			return []byte(body), nil
		}
	}
	tests := []struct {
		name   string
		read   func(string) ([]byte, error)
		want   int
		wantOK bool
	}{
		{name: "one and a half cores", read: reader("150000 100000\n", nil), want: 2, wantOK: true},
		{name: "two cores", read: reader("200000 100000", nil), want: 2, wantOK: true},
		{name: "literal max is unlimited", read: reader("max 100000\n", nil), wantOK: false},
		{name: "missing file", read: reader("", errors.New("no such file")), wantOK: false},
		{name: "empty body", read: reader("", nil), wantOK: false},
		{name: "single field malformed", read: reader("150000", nil), wantOK: false},
		{name: "non-numeric quota", read: reader("abc 100000", nil), wantOK: false},
		{name: "non-numeric period", read: reader("150000 xyz", nil), wantOK: false},
		{name: "zero period guarded", read: reader("150000 0", nil), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCgroupCPUMaxV2(tt.read, path)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Fatalf("parseCgroupCPUMaxV2() = (%d, %v), want (%d, %v)",
					got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestParseCgroupCPUQuotaV1(t *testing.T) {
	const (
		quotaPath  = "/sys/fs/cgroup/cpu/cpu.cfs_quota_us"
		periodPath = "/sys/fs/cgroup/cpu/cpu.cfs_period_us"
	)
	// reader serves quota/period bodies keyed by path; a "" body with a non-nil
	// err simulates a missing file.
	reader := func(quota, period string, quotaErr, periodErr error) func(string) ([]byte, error) {
		return func(p string) ([]byte, error) {
			switch p {
			case quotaPath:
				if quotaErr != nil {
					return nil, quotaErr
				}
				return []byte(quota), nil
			case periodPath:
				if periodErr != nil {
					return nil, periodErr
				}
				return []byte(period), nil
			}
			return nil, errors.New("unexpected path")
		}
	}
	tests := []struct {
		name   string
		read   func(string) ([]byte, error)
		want   int
		wantOK bool
	}{
		{name: "one and a half cores", read: reader("150000", "100000", nil, nil), want: 2, wantOK: true},
		{name: "two cores", read: reader("200000", "100000", nil, nil), want: 2, wantOK: true},
		{name: "unlimited quota -1", read: reader("-1", "100000", nil, nil), wantOK: false},
		{name: "zero quota rejected", read: reader("0", "100000", nil, nil), wantOK: false},
		{name: "missing quota file", read: reader("", "100000", errors.New("nope"), nil), wantOK: false},
		{name: "missing period file", read: reader("150000", "", nil, errors.New("nope")), wantOK: false},
		{name: "non-numeric quota", read: reader("abc", "100000", nil, nil), wantOK: false},
		{name: "non-numeric period", read: reader("150000", "xyz", nil, nil), wantOK: false},
		{name: "zero period guarded", read: reader("150000", "0", nil, nil), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCgroupCPUQuotaV1(tt.read, quotaPath, periodPath)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Fatalf("parseCgroupCPUQuotaV1() = (%d, %v), want (%d, %v)",
					got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestCgroupCPUQuotaFrom(t *testing.T) {
	const (
		v2Path   = "/sys/fs/cgroup/cpu.max"
		v1Quota  = "/sys/fs/cgroup/cpu/cpu.cfs_quota_us"
		v1Period = "/sys/fs/cgroup/cpu/cpu.cfs_period_us"
	)

	// v2 present and capped: used directly, v1 never consulted.
	got := cgroupCPUQuotaFrom(func(p string) ([]byte, error) {
		if p == v2Path {
			return []byte("200000 100000"), nil
		}
		t.Fatalf("v1 paths should not be read when v2 is capped, got read of %q", p)
		return nil, nil
	})
	if got != 2 {
		t.Fatalf("v2 capped: got %d cores, want 2", got)
	}

	// v2 reports "max" (unlimited): fall through to v1.
	got = cgroupCPUQuotaFrom(func(p string) ([]byte, error) {
		switch p {
		case v2Path:
			return []byte("max 100000"), nil
		case v1Quota:
			return []byte("150000"), nil
		case v1Period:
			return []byte("100000"), nil
		}
		return nil, errors.New("unexpected path")
	})
	if got != 2 {
		t.Fatalf("v2 unlimited -> v1: got %d cores, want 2", got)
	}

	// Neither hierarchy present: no quota, no clamp.
	got = cgroupCPUQuotaFrom(func(string) ([]byte, error) {
		return nil, errors.New("no such file")
	})
	if got != 0 {
		t.Fatalf("no cgroup: got %d, want 0", got)
	}
}

func TestClampWorkersToCPUQuota(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		quotaCores int
		want       int
	}{
		{name: "quota below numcpu clamps down", configured: 8, quotaCores: 2, want: 2},
		{name: "no quota leaves numcpu untouched", configured: 8, quotaCores: 0, want: 8},
		{name: "quota equal to numcpu unchanged", configured: 8, quotaCores: 8, want: 8},
		{name: "quota above numcpu never raises", configured: 8, quotaCores: 16, want: 8},
		{name: "floor of one when configured is zero", configured: 0, quotaCores: 0, want: 1},
		{name: "floor of one when configured is negative", configured: -4, quotaCores: 0, want: 1},
		{name: "quota of one clamps to one", configured: 8, quotaCores: 1, want: 1},
		{name: "single core host single core quota", configured: 1, quotaCores: 1, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampWorkersToCPUQuota(tt.configured, tt.quotaCores); got != tt.want {
				t.Fatalf("clampWorkersToCPUQuota(%d, %d) = %d, want %d",
					tt.configured, tt.quotaCores, got, tt.want)
			}
		})
	}
}

func TestCPUClampEnabled(t *testing.T) {
	tests := []struct {
		val  string
		set  bool
		want bool
	}{
		{set: false, want: true}, // unset -> default on
		{val: "1", set: true, want: true},
		{val: "0", set: true, want: false},
		{val: "false", set: true, want: false},
		{val: "FALSE", set: true, want: false},
		{val: "true", set: true, want: true},
		{val: "anything", set: true, want: true},
	}
	for _, tt := range tests {
		name := "unset"
		if tt.set {
			name = tt.val
		}
		t.Run(name, func(t *testing.T) {
			if tt.set {
				t.Setenv("GORTEX_INDEX_CPU_CLAMP", tt.val)
			} else {
				os.Unsetenv("GORTEX_INDEX_CPU_CLAMP")
			}
			if got := cpuClampEnabled(); got != tt.want {
				t.Fatalf("cpuClampEnabled() with %q = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}

func TestMemReleaseEnabled(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"", true}, // unset => on by default
		{"1", true},
		{"true", true},
		{"anything", true},
		{"0", false}, // kill-switch
		{"false", false},
		{"FALSE", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("GORTEX_DAEMON_MEMRELEASE", tt.val)
			if got := memReleaseEnabled(); got != tt.want {
				t.Fatalf("memReleaseEnabled() with %q = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}

func TestFreeOSMemoryAfterColdIndex(t *testing.T) {
	// Kill-switch: a no-op that must not panic (logger is nil-safe).
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "0")
	freeOSMemoryAfterColdIndex(nil, false)

	// Enabled: forces a scavenge; HeapReleased must not go backwards.
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "1")
	sink := make([]byte, 8<<20)
	for i := range sink {
		sink[i] = byte(i)
	}
	// Force the writes to land (so the allocation is not elided), then let the
	// reference drop here: sink is dead past this point, so the scavenge below
	// has heap to return.
	runtime.KeepAlive(sink)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	freeOSMemoryAfterColdIndex(nil, false)
	runtime.ReadMemStats(&after)
	if after.HeapReleased < before.HeapReleased {
		t.Fatalf("HeapReleased went backwards: before=%d after=%d",
			before.HeapReleased, after.HeapReleased)
	}
}

func TestFreeOSMemoryAfterColdIndexDefersWhileTrackedWorkActive(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_MEMRELEASE", "1")
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	runtimeactivity.Begin("mcp")
	defer runtimeactivity.End("mcp")
	freeOSMemoryAfterColdIndex(logger, false)

	if got := logs.FilterMessage("indexer: deferred cold-index heap release until process idle").Len(); got != 1 {
		t.Fatalf("deferred-release logs = %d, want 1", got)
	}
}
