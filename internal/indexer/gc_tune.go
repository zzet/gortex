package indexer

import (
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/runtimeactivity"
)

// Cold-index GC tuning.
//
// A full (cold) index allocates hard: every parsed file produces nodes,
// edges, and tree-sitter C scratch that churns the Go heap. The default GC
// pacing collects often and keeps RSS low, which is the wrong trade during
// this one-shot burst — we would rather run fewer, larger GC cycles and let
// RSS climb toward a sane ceiling. These knobs are installed for the duration
// of IndexCtx and restored on exit, so they never leak into the long-running
// daemon's steady state.
//
// The knobs are GC-timing only: they change when collection happens and how
// high RSS is allowed to climb, never what the indexer produces — node and
// edge counts are identical with tuning on or off. Set GORTEX_INDEX_GC_TUNE=0
// to skip the tuning entirely (for A/B measurement against the untuned run).

const (
	// defaultIndexGCPercent raises the GC percent window during a cold index
	// so collection runs less often (fewer, larger cycles) than the runtime
	// default of 100. Override via GORTEX_INDEX_GC_PERCENT.
	defaultIndexGCPercent = 300

	// budgetDivisor halves the available-memory figure to derive the soft
	// memory limit. Half leaves headroom for the off-heap working set the Go
	// memory limit does not account for — tree-sitter C allocations and the
	// disk backend's buffers — so the process trends toward the budget without
	// the limit forcing a GC death-spiral.
	budgetDivisor = 2

	// minIndexMemoryBudget is the floor below which a soft limit would force
	// near-constant GC and hurt more than it helps; below it we skip the
	// memory-limit knob (GC percent still applies).
	minIndexMemoryBudget = 512 << 20 // 512 MiB

	// maxPlausibleMemoryBytes bounds a sane physical-memory / cgroup figure.
	// cgroup v1 reports a near-int64-max sentinel (~9.2e18) when uncapped;
	// anything above this ceiling is treated as "unset" rather than a real
	// limit.
	maxPlausibleMemoryBytes = 1 << 50 // 1 PiB

	// maxPlausibleCPUCores bounds a sane cgroup CPU-quota core count: a quota
	// far larger than any real machine signals a malformed file and is treated
	// as "unset" (no clamp) rather than a real allotment.
	maxPlausibleCPUCores = 1 << 16 // 65536
)

// gcTuneEnabled reports whether cold-index GC tuning is active. On by default;
// GORTEX_INDEX_GC_TUNE=0 (or "false") disables it so a run can be A/B-compared
// against the untuned baseline.
func gcTuneEnabled() bool {
	v := os.Getenv("GORTEX_INDEX_GC_TUNE")
	if v == "" {
		return true
	}
	return v != "0" && !strings.EqualFold(v, "false")
}

// indexGCPercent returns the GC percent target for the cold-index window.
// GORTEX_INDEX_GC_PERCENT overrides the default; non-numeric or non-positive
// values fall back to the default (a value <= 0 is rejected — disabling GC
// outright is never the right call for a long-lived daemon).
func indexGCPercent() int {
	if v := os.Getenv("GORTEX_INDEX_GC_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultIndexGCPercent
}

// indexMemoryBudget computes the soft memory-limit budget (in bytes) for the
// cold-index window from the host's physical RAM and an optional cgroup memory
// limit (0 = no cgroup limit detected). The cgroup limit wins when it is a
// real cap below host RAM (a container's ceiling is the true budget);
// otherwise the host figure is used. Returns 0 when no sane budget can be
// derived — the caller then leaves the runtime memory limit untouched.
//
// Pure and deterministic: every input is a parameter, so it is exhaustively
// testable without touching the real host.
func indexMemoryBudget(hostRAM, cgroupLimit uint64) int64 {
	effective := hostRAM
	if cgroupLimit > 0 && cgroupLimit <= maxPlausibleMemoryBytes {
		if effective == 0 || cgroupLimit < effective {
			effective = cgroupLimit
		}
	}
	if effective == 0 || effective > maxPlausibleMemoryBytes {
		return 0
	}
	budget := int64(effective / budgetDivisor)
	if budget < minIndexMemoryBudget {
		return 0
	}
	return budget
}

// boundedIndexMemoryBudget prevents the cold-index window from weakening an
// already-lower process memory limit (the daemon standing limit or GOMEMLIMIT).
// A non-positive calculated budget means "leave the runtime untouched". The
// runtime's default effectively-unbounded MaxInt64 value naturally selects the
// calculated budget, while a configured lower ceiling is preserved exactly.
func boundedIndexMemoryBudget(calculated, current int64) int64 {
	if calculated <= 0 {
		return 0
	}
	if current > 0 && current < calculated {
		return current
	}
	return calculated
}

// cgroupMemoryLimit returns the active cgroup memory ceiling in bytes, or 0
// when the process is not under a cgroup memory limit (or detection fails).
// The process's mounted cgroup v2 ancestry is consulted first, followed by
// the conventional v2 and v1 root paths. Missing files, the literal "max",
// and implausible sentinels all degrade to 0.
func cgroupMemoryLimit() uint64 {
	return cgroupMemoryLimitFrom(os.ReadFile)
}

// cgroupMemoryLimitFrom is cgroupMemoryLimit with an injectable file reader,
// so the cgroup-detection logic is testable without a real cgroup hierarchy.
func cgroupMemoryLimitFrom(readFile func(string) ([]byte, error)) uint64 {
	if limit := cgroupV2MemoryLimit(readFile); limit != 0 {
		return limit
	}
	for _, path := range []string{
		"/sys/fs/cgroup/memory.max",                   // cgroup v2 unified hierarchy
		"/sys/fs/cgroup/memory/memory.limit_in_bytes", // cgroup v1
	} {
		if v, ok := parseCgroupMemoryLimit(readFile, path); ok {
			return v
		}
	}
	return 0
}

// parseCgroupMemoryLimit reads and parses one cgroup memory-limit file. It
// reports ok=false for a missing/empty file, the literal "max", a zero value,
// a non-numeric body, or an implausibly large sentinel (cgroup v1 reports a
// near-int64-max value when uncapped).
func parseCgroupMemoryLimit(readFile func(string) ([]byte, error), path string) (uint64, bool) {
	b, err := readFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 || n > maxPlausibleMemoryBytes {
		return 0, false
	}
	return n, true
}

// Cold-index worker clamp.
//
// idx.config.Workers defaults to the host's runtime.NumCPU() and sizes the
// parse worker pool. In a CPU-limited container the host core count exceeds the
// allotted cgroup CPU quota, so the pool over-subscribes and the CFS scheduler
// throttles it — fewer, larger time slices and worse throughput than sizing the
// pool to the real quota. When a finite quota is present the effective worker
// count is clamped down to it. The clamp only ever LOWERS the count (never
// raises it) and never drops below 1; it changes scheduling pressure only, not
// what the indexer produces — node and edge counts are identical whether the
// clamp is active or not. Set GORTEX_INDEX_CPU_CLAMP=0 to skip it.

// cpuClampEnabled reports whether the cold-index worker pool is clamped to the
// cgroup CPU quota. On by default; GORTEX_INDEX_CPU_CLAMP=0 (or "false")
// disables it so a run can be A/B-compared against the unclamped baseline.
func cpuClampEnabled() bool {
	v := os.Getenv("GORTEX_INDEX_CPU_CLAMP")
	if v == "" {
		return true
	}
	return v != "0" && !strings.EqualFold(v, "false")
}

// cgroupCPUQuota returns the active cgroup CPU quota as an integer
// core-equivalent (at least 1), or 0 when the process is not under a finite CPU
// quota (or detection fails). cgroup v2 (`cpu.max`) is consulted first, then v1
// (`cpu/cpu.cfs_quota_us` + `cpu/cpu.cfs_period_us`). An unlimited quota ("max"
// / -1), missing files, and unparsable bodies all degrade to 0 — no clamp.
// Linux-only in practice; on other platforms the files are absent and this
// returns 0.
func cgroupCPUQuota() int {
	return cgroupCPUQuotaFrom(os.ReadFile)
}

// cgroupCPUQuotaFrom is cgroupCPUQuota with an injectable file reader, so the
// cgroup-detection logic is testable without a real cgroup hierarchy.
func cgroupCPUQuotaFrom(readFile func(string) ([]byte, error)) int {
	if cores, ok := parseCgroupCPUMaxV2(readFile, "/sys/fs/cgroup/cpu.max"); ok {
		return cores
	}
	if cores, ok := parseCgroupCPUQuotaV1(readFile,
		"/sys/fs/cgroup/cpu/cpu.cfs_quota_us",
		"/sys/fs/cgroup/cpu/cpu.cfs_period_us"); ok {
		return cores
	}
	return 0
}

// parseCgroupCPUMaxV2 reads a cgroup v2 `cpu.max` file, whose body is
// "<quota> <period>" (microseconds) or "max <period>" when unlimited. Reports
// ok=false for a missing/empty/malformed file, the literal "max" quota
// (unlimited), or a non-positive quota/period.
func parseCgroupCPUMaxV2(readFile func(string) ([]byte, error), path string) (int, bool) {
	b, err := readFile(path)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 || fields[0] == "max" {
		return 0, false // missing/malformed, or unlimited
	}
	quota, qerr := strconv.ParseInt(fields[0], 10, 64)
	period, perr := strconv.ParseInt(fields[1], 10, 64)
	if qerr != nil || perr != nil {
		return 0, false
	}
	return cpuQuotaCores(quota, period)
}

// parseCgroupCPUQuotaV1 reads the cgroup v1 `cpu.cfs_quota_us` and
// `cpu.cfs_period_us` files (microseconds). A quota of -1 means unlimited.
// Reports ok=false for missing/malformed files, an unlimited (non-positive)
// quota, or a non-positive period.
func parseCgroupCPUQuotaV1(readFile func(string) ([]byte, error), quotaPath, periodPath string) (int, bool) {
	qb, err := readFile(quotaPath)
	if err != nil {
		return 0, false
	}
	quota, err := strconv.ParseInt(strings.TrimSpace(string(qb)), 10, 64)
	if err != nil || quota <= 0 {
		return 0, false // -1 (or 0) means unlimited / unset
	}
	pb, err := readFile(periodPath)
	if err != nil {
		return 0, false
	}
	period, err := strconv.ParseInt(strings.TrimSpace(string(pb)), 10, 64)
	if err != nil {
		return 0, false
	}
	return cpuQuotaCores(quota, period)
}

// cpuQuotaCores converts a (quota, period) microsecond pair into an integer
// core count. It rounds UP — ceil(quota/period) — so a fractional allotment
// like 1.5 cores sizes the pool to 2 rather than starving it at 1; the result
// is floored at 1. Reports ok=false when quota or period is non-positive (the
// period guard also rules out divide-by-zero) or the rounded count exceeds a
// sane ceiling (a malformed file masquerading as an enormous quota).
func cpuQuotaCores(quota, period int64) (int, bool) {
	if quota <= 0 || period <= 0 {
		return 0, false
	}
	cores := (quota + period - 1) / period // ceil(quota/period)
	if cores < 1 {
		cores = 1
	}
	if cores > maxPlausibleCPUCores {
		return 0, false
	}
	return int(cores), true
}

// clampWorkersToCPUQuota returns the effective parse-worker count after
// clamping `configured` down to the cgroup CPU quota `quotaCores` (an integer
// core count, 0 when no finite quota was detected). The clamp applies to the
// effective value regardless of whether Workers came from the runtime.NumCPU()
// default or an explicit config override — both over-subscribe a quota and
// invite CFS throttling — but it only ever LOWERS the count, never raises it,
// and never drops below 1. quotaCores<=0 leaves `configured` unchanged, so a
// non-limited host behaves exactly as before.
func clampWorkersToCPUQuota(configured, quotaCores int) int {
	if configured < 1 {
		configured = 1
	}
	if quotaCores > 0 && quotaCores < configured {
		return quotaCores
	}
	return configured
}

// gcTune state guards the process-global GC knobs across concurrent index
// calls. Multi-repo warmup runs IndexCtx in parallel goroutines, so the knobs
// are reference-counted: the first concurrent applier captures the prior
// settings and installs the tuned ones; the last to finish restores them.
var (
	gcTuneMu          sync.Mutex
	gcTuneDepth       int
	gcTunePrevPct     int
	gcTunePctSet      bool
	gcTunePrevLim     int64
	gcTuneNativeStart uint64
)

// shouldRaiseGCPercent decides whether the cold-index window may relax the
// GC percent (fewer, larger cycles). Relaxed pacing is only safe when the
// effective memory limit gives the window its own calculated budget: under a
// clamped ceiling (an operator GOMEMLIMIT or standing limit below the window
// budget) a raised percent hands pacing to the soft limit, and the burst
// degrades into back-to-back assist/mark storms at the ceiling. calculated<=0
// means no sane budget could be derived — treat the limit as authoritative
// only when one is actually installed.
func shouldRaiseGCPercent(calculated, installed int64) bool {
	if installed <= 0 {
		return true
	}
	if calculated <= 0 {
		return false
	}
	return installed >= calculated
}

// applyIndexGCTuning installs the cold-index GC knobs and returns a closure
// that restores the prior settings. Defer the returned closure; it reverts at
// most once. Returns a no-op closure when tuning is disabled.
//
// Because debug.SetGCPercent / debug.SetMemoryLimit are process-global and
// IndexCtx can run concurrently across repos, the knobs are reference-counted:
// only the first concurrent caller mutates the runtime, and only the last
// restore reverts it — so a sibling index can't clobber another's restore.
func applyIndexGCTuning(logger *zap.Logger) func() {
	return applyIndexGCTuningWithBudget(
		logger, indexMemoryBudget(hostPhysicalMemory(), cgroupMemoryLimit()))
}

// applyIndexGCTuningWithBudget is applyIndexGCTuning with the window's memory
// budget passed in rather than read from the host. The budget is the single
// input that decides whether the GC-percent knob is installed at all, and
// hostPhysicalMemory has no portable reader everywhere — it returns 0 on any
// platform without one, which makes the knob a property of the runner rather
// than of this code. Supplying it keeps that decision testable, the same way
// indexMemoryBudget takes its own inputs as parameters.
func applyIndexGCTuningWithBudget(logger *zap.Logger, budget int64) func() {
	if !gcTuneEnabled() {
		return func() {}
	}

	gcPct := indexGCPercent()

	gcTuneMu.Lock()
	if gcTuneDepth == 0 {
		// Capture native-parser progress for this shared indexing window. The
		// last restore uses the delta to avoid a process-wide allocator walk
		// after ordinary incremental indexes that did not parse C-family code.
		gcTuneNativeStart = nativeParseCompletionGeneration.Load()

		// SetMemoryLimit(-1) reads the current limit without changing it.
		// Capture it before mutating so the restore is exact.
		calculated := budget
		gcTunePrevLim = debug.SetMemoryLimit(-1)
		if ColdIndexMemoryLimitRaiseAllowed() {
			budget = raisedIndexMemoryBudget(budget, gcTunePrevLim)
		} else {
			budget = boundedIndexMemoryBudget(budget, gcTunePrevLim)
		}
		installed := gcTunePrevLim
		if budget > 0 {
			debug.SetMemoryLimit(budget)
			installed = budget
		}
		// Relax the GC percent only when the window got its own budget: with
		// the limit clamped below it, the runtime's default percent keeps
		// live-heap pacing meaningful under the ceiling instead of letting
		// the soft limit drive continuous assist storms.
		gcTunePctSet = shouldRaiseGCPercent(calculated, installed)
		if gcTunePctSet {
			gcTunePrevPct = debug.SetGCPercent(gcPct)
		}
		if logger != nil {
			logger.Debug("indexer: cold-index GC tuning applied",
				zap.Int("gc_percent", gcPct),
				zap.Bool("gc_percent_applied", gcTunePctSet),
				zap.Int("prev_gc_percent", gcTunePrevPct),
				zap.Int64("mem_limit_bytes", budget),
				zap.Int64("prev_mem_limit_bytes", gcTunePrevLim),
			)
		}
	}
	gcTuneDepth++
	gcTuneMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			gcTuneMu.Lock()
			gcTuneDepth--
			closed := gcTuneDepth == 0
			releaseNative := false
			if closed {
				if gcTunePctSet {
					debug.SetGCPercent(gcTunePrevPct)
					gcTunePctSet = false
				}
				debug.SetMemoryLimit(gcTunePrevLim)
				releaseNative = nativeParseCompletionGeneration.Load() != gcTuneNativeStart
			}
			gcTuneMu.Unlock()
			// Scavenge the cold-index burst's heap high-water once the
			// window is fully closed — outside the lock, since FreeOSMemory
			// is a full GC cycle that must not serialise sibling index
			// calls waiting on gcTuneMu. Native zone walks use their own
			// process-wide lock and only run for a window that parsed C-family
			// code.
			if closed {
				freeOSMemoryAfterColdIndex(logger, releaseNative)
			}
		})
	}
}

// memReleaseEnabled reports whether post-burst heap release is active. On by
// default; GORTEX_DAEMON_MEMRELEASE=0 (or "false") disables it. The check is
// duplicated here (rather than shared) because the canonical release helper
// lives in the cmd layer, which this package must not import.
func memReleaseEnabled() bool {
	v := os.Getenv("GORTEX_DAEMON_MEMRELEASE")
	return v != "0" && !strings.EqualFold(v, "false")
}

// freeOSMemoryAfterColdIndex returns the cold-index burst's Go and native
// allocator high-water to the OS once the shared tuning window has fully
// closed. debug.FreeOSMemory handles Go heap pages; a dirty-gated libmalloc
// pressure-relief pass handles freed tree-sitter pages that Go cannot see.
func freeOSMemoryAfterColdIndex(logger *zap.Logger, releaseNative bool) {
	if !memReleaseEnabled() {
		return
	}
	start := time.Now()
	ran, _ := runtimeactivity.RunIfQuiet(0, debug.FreeOSMemory)
	forced := false
	if !ran {
		// Not quiet — but leaving the burst's high-water heap in place is
		// only safe when the ceiling has room. Back-to-back index bursts
		// (a batch of tracks, a bench, an agent loop) never go quiet, and
		// each burst then starts at the previous one's high-water inside a
		// clamped limit — the collector ends up pacing against the ceiling
		// for the whole next burst. Under pressure, pay the one forced
		// release now.
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if forcedHeapReleaseNeeded(int64(ms.HeapInuse), debug.SetMemoryLimit(-1)) {
			debug.FreeOSMemory()
			ran, forced = true, true
		}
	}

	// Go's scavenger cannot return pages retained by tree-sitter/libmalloc.
	// Sweep native zones once the shared index window is closed, but only when
	// its generation proves that C-family parsing actually occurred. Darwin's
	// reported byte count is a lower bound: tiny/small regions may be madvised
	// without contributing to malloc_zone_pressure_relief's return value.
	nativeRan := releaseNative && nativeAllocatorPressureReliefSupported
	var nativeReported uintptr
	var nativeElapsed time.Duration
	if nativeRan {
		nativeStart := time.Now()
		nativeReported = runNativeAllocatorPressureRelief()
		nativeElapsed = time.Since(nativeStart)
	}

	if logger == nil {
		return
	}
	if !ran {
		snapshot := runtimeactivity.Current()
		logger.Debug("indexer: deferred cold-index heap release until process idle",
			zap.Int64("active_work", snapshot.Active),
			zap.Any("active_by_kind", snapshot.ByKind),
			zap.Bool("native_pressure_relief_ran", nativeRan),
			zap.Uint64("native_reported_released_bytes", uint64(nativeReported)),
			zap.Duration("native_elapsed", nativeElapsed))
		return
	}
	logger.Debug("indexer: released heap to OS after cold index",
		zap.Bool("forced_by_pressure", forced),
		zap.Bool("native_pressure_relief_ran", nativeRan),
		zap.Uint64("native_reported_released_bytes", uint64(nativeReported)),
		zap.Duration("native_elapsed", nativeElapsed),
		zap.Duration("elapsed", time.Since(start)))
}

// forcedHeapReleaseNeeded reports whether the post-burst release must run
// even though the process is not quiet: the heap high-water occupies more
// than half of an installed finite memory limit. Below that share, deferring
// to the idle scheduler is fine; above it, the next burst would start inside
// the limit-pacing regime the release exists to prevent.
func forcedHeapReleaseNeeded(heapInuse, limit int64) bool {
	if limit <= 0 || limit == math.MaxInt64 {
		return false
	}
	return heapInuse > limit/2
}
