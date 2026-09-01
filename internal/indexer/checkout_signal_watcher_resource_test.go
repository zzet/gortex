package indexer

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

const (
	checkoutSignalResourceLatencyLimit = 2 * time.Second
	checkoutSignalResourceCleanupWait  = 3 * time.Second
	checkoutSignalResourceMiB          = int64(1024 * 1024)
)

type checkoutSignalResourceEvent struct {
	checkoutID string
	reason     string
	at         time.Time
}

type checkoutSignalResourceRecorder struct {
	mu     sync.Mutex
	events []checkoutSignalResourceEvent
	wake   chan struct{}
}

func newCheckoutSignalResourceRecorder() *checkoutSignalResourceRecorder {
	return &checkoutSignalResourceRecorder{wake: make(chan struct{}, 1)}
}

func (r *checkoutSignalResourceRecorder) signal(checkoutID, reason string) bool {
	r.mu.Lock()
	r.events = append(r.events, checkoutSignalResourceEvent{
		checkoutID: checkoutID,
		reason:     reason,
		at:         time.Now(),
	})
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return true
}

func (r *checkoutSignalResourceRecorder) eventsFrom(cursor int) ([]checkoutSignalResourceEvent, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cursor < 0 || cursor > len(r.events) {
		cursor = 0
	}
	out := append([]checkoutSignalResourceEvent(nil), r.events[cursor:]...)
	return out, len(r.events)
}

func (r *checkoutSignalResourceRecorder) reset() {
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
	for {
		select {
		case <-r.wake:
		default:
			return
		}
	}
}

type checkoutSignalResourceSnapshot struct {
	fds        int
	goroutines int
	rssBytes   int64
	threads    int
}

type checkoutSignalResourceRun struct {
	baseline     checkoutSignalResourceSnapshot
	active       checkoutSignalResourceSnapshot
	cleanup      checkoutSignalResourceSnapshot
	readyMax     time.Duration
	eventMax     time.Duration
	rootCount    int
	filesPerRoot int
	race         bool
}

func TestCheckoutSourceSignalWatcherRealBackendResources(t *testing.T) {
	for _, roots := range []int{1, 8, 64} {
		roots := roots
		t.Run(fmt.Sprintf("roots_%d", roots), func(t *testing.T) {
			runCheckoutSignalResourceCase(t, roots, 1, fmt.Sprintf("resource-%d", roots))
		})
	}
}

func TestCheckoutSourceSignalWatcherRealBackendRepeated64RootCycles(t *testing.T) {
	cleanupRSS := make([]int64, 0, 3)
	cleanupGoroutines := make([]int, 0, 3)
	cleanupThreads := make([]int, 0, 3)
	for cycle := 0; cycle < 3; cycle++ {
		cycle := cycle
		t.Run(fmt.Sprintf("cycle_%d", cycle+1), func(t *testing.T) {
			result := runCheckoutSignalResourceCase(t, 64, 1, fmt.Sprintf("cycle-%d", cycle+1))
			cleanupRSS = append(cleanupRSS, result.cleanup.rssBytes)
			cleanupGoroutines = append(cleanupGoroutines, result.cleanup.goroutines)
			cleanupThreads = append(cleanupThreads, result.cleanup.threads)
		})
	}
	if runtime.GOOS != "darwin" || len(cleanupRSS) != 3 || len(cleanupGoroutines) != 3 || len(cleanupThreads) != 3 {
		t.Logf("64-root cleanup slope assertions skipped: goos=%s rss=%v goroutines=%v threads=%v",
			runtime.GOOS, cleanupRSS, cleanupGoroutines, cleanupThreads)
		return
	}

	goroutineGrowth := cleanupGoroutines[len(cleanupGoroutines)-1] - cleanupGoroutines[0]
	goroutineSlope := float64(goroutineGrowth) / float64(len(cleanupGoroutines)-1)
	threadGrowth := cleanupThreads[len(cleanupThreads)-1] - cleanupThreads[0]
	threadSlope := float64(threadGrowth) / float64(len(cleanupThreads)-1)
	t.Logf("64-root cleanup execution samples: goroutines=%v slope=%.2f/cycle; threads=%v slope=%.2f/cycle",
		cleanupGoroutines, goroutineSlope, cleanupThreads, threadSlope)

	rssSlope := (cleanupRSS[len(cleanupRSS)-1] - cleanupRSS[0]) / int64(len(cleanupRSS)-1)
	middleResidual := cleanupRSS[1] - (cleanupRSS[0]+cleanupRSS[2])/2
	t.Logf("64-root cleanup RSS samples=%v slope=%d bytes/cycle (%.2f MiB/cycle) middle_residual=%d bytes",
		cleanupRSS, rssSlope, float64(rssSlope)/float64(checkoutSignalResourceMiB), middleResidual)
	if rssSlope >= 2*checkoutSignalResourceMiB {
		if checkoutSignalResourceRaceEnabled() {
			t.Logf("RSS slope exceeded the 2 MiB/cycle normal-build target under -race; retained as a relaxed race metric")
		} else {
			t.Errorf("64-root cleanup RSS slope=%d bytes/cycle (%.2f MiB/cycle); want <2 MiB/cycle",
				rssSlope, float64(rssSlope)/float64(checkoutSignalResourceMiB))
		}
	}
}

func TestCheckoutSourceSignalWatcherRealBackendFileCountIndependentFDs(t *testing.T) {
	var sparse, dense checkoutSignalResourceRun
	t.Run("one_file", func(t *testing.T) {
		sparse = runCheckoutSignalResourceCase(t, 1, 1, "files-sparse")
	})
	t.Run("ten_thousand_files", func(t *testing.T) {
		dense = runCheckoutSignalResourceCase(t, 1, 10_000, "files-dense")
	})
	if runtime.GOOS != "darwin" {
		t.Logf("file-count FD independence assertion skipped on %s", runtime.GOOS)
		return
	}

	sparseDelta := sparse.active.fds - sparse.baseline.fds
	denseDelta := dense.active.fds - dense.baseline.fds
	difference := absCheckoutSignalResourceInt(sparseDelta - denseDelta)
	t.Logf("file-count FD deltas: files=1 delta=%d; files=10000 delta=%d; absolute_difference=%d",
		sparseDelta, denseDelta, difference)
	if difference > 1 {
		t.Fatalf("watcher FD count scaled with file count: sparse delta=%d dense delta=%d", sparseDelta, denseDelta)
	}
}

func TestCheckoutSourceSignalWatcherRealBackendFiniteBurst(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	burstFile := filepath.Join(root, "burst.go")
	if err := os.WriteFile(burstFile, []byte("package burst\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	warmCheckoutSignalResourceBackend(t, parent)
	stabilizeCheckoutSignalResourceProcess()
	baseline := mustCheckoutSignalResourceSnapshot(t)
	baselineWatcherStacks := checkoutSignalResourceWatcherStackCount(checkoutSignalResourceGoroutineProfile())
	recorder := newCheckoutSignalResourceRecorder()
	watchers := newCheckoutSourceSignalWatcherSet(
		nil,
		func(checkoutSourceSignalIdentity) bool { return true },
		recorder.signal,
		zap.NewNop(),
	)
	t.Cleanup(watchers.StopAll)

	identity := checkoutSourceSignalResourceIdentity(root, "burst", 0)
	started := time.Now()
	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure burst watcher: %v", err)
	}
	readyMax := waitCheckoutSignalResourceReason(
		t,
		recorder,
		map[string]time.Time{identity.checkoutID: started},
		func(event checkoutSignalResourceEvent) bool { return event.reason == "source-watch-ready" },
		checkoutSignalResourceLatencyLimit,
	)
	settleCheckoutSignalResourceRecorder(recorder)

	const syscalls = 10_000
	burstStarted := time.Now()
	for i := 0; i < syscalls; i++ {
		if err := os.Truncate(burstFile, int64(i&1)); err != nil {
			t.Fatalf("truncate syscall %d: %v", i, err)
		}
	}
	burstFinished := time.Now()
	settled := waitCheckoutSignalResourceEventAtOrAfter(
		t,
		recorder,
		identity.checkoutID,
		burstFinished,
		checkoutSignalResourceLatencyLimit,
	)
	settleLatency := settled.at.Sub(burstFinished)
	if settleLatency < 0 {
		settleLatency = 0
	}
	time.Sleep(checkoutSourceSignalQuietWindow + 50*time.Millisecond)
	events, _ := recorder.eventsFrom(0)
	signalCount := 0
	for _, event := range events {
		if event.checkoutID == identity.checkoutID && event.reason != "source-watch-ready" && !event.at.Before(burstStarted) {
			signalCount++
		}
	}
	if signalCount > 2 {
		t.Fatalf("10k-syscall burst produced %d watcher signals; want <=2", signalCount)
	}
	if settleLatency >= checkoutSignalResourceLatencyLimit {
		t.Fatalf("final settled burst signal latency=%s; want <%s", settleLatency, checkoutSignalResourceLatencyLimit)
	}

	active := mustCheckoutSignalResourceSnapshot(t)
	watchers.StopAll()
	cleanup := waitCheckoutSignalResourceCleanup(t, baseline, baselineWatcherStacks)
	t.Logf("burst metrics: syscalls=%d syscall_duration=%s ready=%s settled_latency=%s signals=%d fd_delta=%d goroutine_delta=%d rss_delta=%d thread_delta=%d cleanup_fd_delta=%d cleanup_goroutine_delta=%d",
		syscalls,
		burstFinished.Sub(burstStarted),
		readyMax,
		settleLatency,
		signalCount,
		active.fds-baseline.fds,
		active.goroutines-baseline.goroutines,
		active.rssBytes-baseline.rssBytes,
		active.threads-baseline.threads,
		cleanup.fds-baseline.fds,
		cleanup.goroutines-baseline.goroutines,
	)
}

func runCheckoutSignalResourceCase(t *testing.T, rootCount, filesPerRoot int, tag string) checkoutSignalResourceRun {
	t.Helper()
	parent := t.TempDir()
	roots := make(map[string]string, rootCount)
	identities := make([]checkoutSourceSignalIdentity, 0, rootCount)
	for i := 0; i < rootCount; i++ {
		root := filepath.Join(parent, fmt.Sprintf("root-%03d", i))
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatalf("create root %d: %v", i, err)
		}
		for fileIndex := 0; fileIndex < filesPerRoot; fileIndex++ {
			name := filepath.Join(root, fmt.Sprintf("seed-%05d.go", fileIndex))
			if err := os.WriteFile(name, []byte("package seed\n"), 0o644); err != nil {
				t.Fatalf("populate root %d file %d: %v", i, fileIndex, err)
			}
		}
		identity := checkoutSourceSignalResourceIdentity(root, tag, i)
		roots[identity.checkoutID] = root
		identities = append(identities, identity)
	}

	warmCheckoutSignalResourceBackend(t, parent)
	stabilizeCheckoutSignalResourceProcess()
	baseline := mustCheckoutSignalResourceSnapshot(t)
	baselineWatcherStacks := checkoutSignalResourceWatcherStackCount(checkoutSignalResourceGoroutineProfile())
	recorder := newCheckoutSignalResourceRecorder()
	watchers := newCheckoutSourceSignalWatcherSet(
		nil,
		func(checkoutSourceSignalIdentity) bool { return true },
		recorder.signal,
		zap.NewNop(),
	)
	t.Cleanup(watchers.StopAll)

	starts := make(map[string]time.Time, rootCount)
	for _, identity := range identities {
		starts[identity.checkoutID] = time.Now()
		if err := watchers.Ensure(identity); err != nil {
			t.Fatalf("ensure watcher %s: %v", identity.checkoutID, err)
		}
	}
	if got := watchers.Len(); got != rootCount {
		t.Fatalf("watcher registrations=%d; want %d", got, rootCount)
	}
	readyMax := waitCheckoutSignalResourceReason(
		t,
		recorder,
		starts,
		func(event checkoutSignalResourceEvent) bool { return event.reason == "source-watch-ready" },
		checkoutSignalResourceLatencyLimit,
	)
	settleCheckoutSignalResourceRecorder(recorder)

	eventStarts := make(map[string]time.Time, rootCount)
	for _, identity := range identities {
		eventStarts[identity.checkoutID] = time.Now()
		path := filepath.Join(roots[identity.checkoutID], "one-file-event.go")
		if err := os.WriteFile(path, []byte("package event\n"), 0o644); err != nil {
			t.Fatalf("write one-file event for %s: %v", identity.checkoutID, err)
		}
	}
	eventMax := waitCheckoutSignalResourceReason(
		t,
		recorder,
		eventStarts,
		func(event checkoutSignalResourceEvent) bool { return event.reason != "source-watch-ready" },
		checkoutSignalResourceLatencyLimit,
	)

	stabilizeCheckoutSignalResourceProcess()
	active := mustCheckoutSignalResourceSnapshot(t)
	result := checkoutSignalResourceRun{
		baseline:     baseline,
		active:       active,
		readyMax:     readyMax,
		eventMax:     eventMax,
		rootCount:    rootCount,
		filesPerRoot: filesPerRoot,
		race:         checkoutSignalResourceRaceEnabled(),
	}

	watchers.StopAll()
	if got := watchers.Len(); got != 0 {
		t.Fatalf("watcher registrations after StopAll=%d; want 0", got)
	}
	result.cleanup = waitCheckoutSignalResourceCleanup(t, baseline, baselineWatcherStacks)
	t.Logf("watcher resource metrics: roots=%d files_per_root=%d race=%t ready_max=%s event_max=%s fd_delta=%d goroutine_delta=%d rss_delta=%d bytes (%.2f MiB) thread_delta=%d cleanup_fd_delta=%d cleanup_goroutine_delta=%d cleanup_rss_delta=%d cleanup_thread_delta=%d",
		rootCount,
		filesPerRoot,
		result.race,
		readyMax,
		eventMax,
		active.fds-baseline.fds,
		active.goroutines-baseline.goroutines,
		active.rssBytes-baseline.rssBytes,
		float64(active.rssBytes-baseline.rssBytes)/float64(checkoutSignalResourceMiB),
		active.threads-baseline.threads,
		result.cleanup.fds-baseline.fds,
		result.cleanup.goroutines-baseline.goroutines,
		result.cleanup.rssBytes-baseline.rssBytes,
		result.cleanup.threads-baseline.threads,
	)
	assertCheckoutSignalResourceBounds(t, result)
	return result
}

func checkoutSourceSignalResourceIdentity(root, tag string, ordinal int) checkoutSourceSignalIdentity {
	checkoutID := fmt.Sprintf("checkout-%s-%03d", tag, ordinal)
	return checkoutSourceSignalIdentity{
		checkoutID:     checkoutID,
		incarnation:    "incarnation-" + checkoutID,
		familyID:       "family-" + tag,
		requestedRoot:  root,
		primaryGraphID: "graph-" + tag,
	}
}

func waitCheckoutSignalResourceReason(
	t *testing.T,
	recorder *checkoutSignalResourceRecorder,
	started map[string]time.Time,
	matches func(checkoutSignalResourceEvent) bool,
	limit time.Duration,
) time.Duration {
	t.Helper()
	seen := make(map[string]bool, len(started))
	cursor := 0
	var maxLatency time.Duration
	deadline := time.Now()
	for _, at := range started {
		if candidate := at.Add(limit); candidate.After(deadline) {
			deadline = candidate
		}
	}

	for len(seen) < len(started) {
		events, next := recorder.eventsFrom(cursor)
		cursor = next
		for _, event := range events {
			start, wanted := started[event.checkoutID]
			if !wanted || seen[event.checkoutID] || !matches(event) {
				continue
			}
			latency := event.at.Sub(start)
			if latency < 0 {
				latency = 0
			}
			if latency >= limit {
				t.Fatalf("watcher signal for %s reason=%s latency=%s; want <%s", event.checkoutID, event.reason, latency, limit)
			}
			seen[event.checkoutID] = true
			if latency > maxLatency {
				maxLatency = latency
			}
		}
		if len(seen) == len(started) {
			return maxLatency
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for watcher signals; missing=%v", missingCheckoutSignalResourceIDs(started, seen))
		}
		wait := 25 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-recorder.wake:
		case <-time.After(wait):
		}
	}
	return maxLatency
}

func waitCheckoutSignalResourceEventAtOrAfter(
	t *testing.T,
	recorder *checkoutSignalResourceRecorder,
	checkoutID string,
	notBefore time.Time,
	limit time.Duration,
) checkoutSignalResourceEvent {
	t.Helper()
	cursor := 0
	deadline := notBefore.Add(limit)
	for {
		events, next := recorder.eventsFrom(cursor)
		cursor = next
		for _, event := range events {
			if event.checkoutID == checkoutID && event.reason != "source-watch-ready" && !event.at.Before(notBefore) {
				return event
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for final settled event for %s", checkoutID)
		}
		wait := 25 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-recorder.wake:
		case <-time.After(wait):
		}
	}
}

func warmCheckoutSignalResourceBackend(t *testing.T, parent string) {
	t.Helper()
	root := filepath.Join(parent, "warmup-root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("create real-backend warmup root: %v", err)
	}
	recorder := newCheckoutSignalResourceRecorder()
	watchers := newCheckoutSourceSignalWatcherSet(
		nil,
		func(checkoutSourceSignalIdentity) bool { return true },
		recorder.signal,
		zap.NewNop(),
	)
	defer watchers.StopAll()
	identity := checkoutSourceSignalResourceIdentity(root, "warmup", 0)
	started := time.Now()
	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure real-backend warmup watcher: %v", err)
	}
	waitCheckoutSignalResourceReason(
		t,
		recorder,
		map[string]time.Time{identity.checkoutID: started},
		func(event checkoutSignalResourceEvent) bool { return event.reason == "source-watch-ready" },
		checkoutSignalResourceLatencyLimit,
	)
	watchers.StopAll()
	if got := watchers.Len(); got != 0 {
		t.Fatalf("warmup watcher registrations after StopAll=%d; want 0", got)
	}
	time.Sleep(100 * time.Millisecond)
}

func settleCheckoutSignalResourceRecorder(recorder *checkoutSignalResourceRecorder) {
	time.Sleep(checkoutSourceSignalQuietWindow + 50*time.Millisecond)
	recorder.reset()
}

func missingCheckoutSignalResourceIDs(started map[string]time.Time, seen map[string]bool) []string {
	missing := make([]string, 0)
	for checkoutID := range started {
		if !seen[checkoutID] {
			missing = append(missing, checkoutID)
		}
	}
	sort.Strings(missing)
	return missing
}

func assertCheckoutSignalResourceBounds(t *testing.T, result checkoutSignalResourceRun) {
	t.Helper()
	if result.readyMax >= checkoutSignalResourceLatencyLimit {
		t.Fatalf("max watcher readiness=%s; want <%s", result.readyMax, checkoutSignalResourceLatencyLimit)
	}
	if result.eventMax >= checkoutSignalResourceLatencyLimit {
		t.Fatalf("max one-file event latency=%s; want <%s", result.eventMax, checkoutSignalResourceLatencyLimit)
	}
	if runtime.GOOS != "darwin" {
		t.Logf("strict FD/goroutine/RSS bounds skipped on %s; operational latency and StopAll still validated", runtime.GOOS)
		return
	}

	fdLimit := 12*result.rootCount + 4
	goroutineLimit := 6*result.rootCount + 8
	rssLimit := int64(64) * checkoutSignalResourceMiB
	if result.race {
		fdLimit = 12*result.rootCount + 8
		goroutineLimit = 8*result.rootCount + 16
		rssLimit = 256 * checkoutSignalResourceMiB
	}
	fdDelta := result.active.fds - result.baseline.fds
	goroutineDelta := result.active.goroutines - result.baseline.goroutines
	if fdDelta > fdLimit {
		t.Errorf("watcher FD delta=%d for %d roots; limit=%d", fdDelta, result.rootCount, fdLimit)
	}
	if goroutineDelta > goroutineLimit {
		t.Errorf("watcher goroutine delta=%d for %d roots; limit=%d", goroutineDelta, result.rootCount, goroutineLimit)
	}
	if result.rootCount == 64 {
		rssDelta := result.active.rssBytes - result.baseline.rssBytes
		if rssDelta >= rssLimit {
			t.Errorf("64-root watcher RSS delta=%d bytes (%.2f MiB); want <%d bytes (race=%t)",
				rssDelta, float64(rssDelta)/float64(checkoutSignalResourceMiB), rssLimit, result.race)
		}
	}
}

func waitCheckoutSignalResourceCleanup(
	t *testing.T,
	baseline checkoutSignalResourceSnapshot,
	baselineWatcherStacks int,
) checkoutSignalResourceSnapshot {
	t.Helper()
	debug.FreeOSMemory()
	deadline := time.Now().Add(checkoutSignalResourceCleanupWait)
	var snapshot checkoutSignalResourceSnapshot
	var profile string
	for {
		snapshot = mustCheckoutSignalResourceSnapshot(t)
		profile = checkoutSignalResourceGoroutineProfile()
		fdClean := runtime.GOOS != "darwin" || snapshot.fds == baseline.fds
		watcherStacks := checkoutSignalResourceWatcherStackCount(profile)
		watcherStacksClean := watcherStacks <= baselineWatcherStacks
		if fdClean && watcherStacksClean {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Errorf("watcher resources remained live after StopAll: baseline=%+v current=%+v fd_exact=%t watcher_stacks=%d baseline_watcher_stacks=%d\n%s",
				baseline, snapshot, fdClean, watcherStacks, baselineWatcherStacks, profile)
			return snapshot
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func checkoutSignalResourceGoroutineProfile() string {
	var profile bytes.Buffer
	if goroutines := pprof.Lookup("goroutine"); goroutines != nil {
		_ = goroutines.WriteTo(&profile, 2)
	}
	return profile.String()
}

func checkoutSignalResourceWatcherStackCount(profile string) int {
	needles := []string{
		"checkoutSourceSignalWatcherSet",
		"checkoutSourceSignalBackend",
		"checkoutSourceSignalAggregator",
		"github.com/zzet/gortex/internal/thirdparty/fswatcher",
	}
	count := 0
	for _, stack := range strings.Split(profile, "\n\n") {
		for _, needle := range needles {
			if strings.Contains(stack, needle) {
				count++
				break
			}
		}
	}
	return count
}

func stabilizeCheckoutSignalResourceProcess() {
	debug.FreeOSMemory()
	time.Sleep(25 * time.Millisecond)
}

func mustCheckoutSignalResourceSnapshot(t *testing.T) checkoutSignalResourceSnapshot {
	t.Helper()
	snapshot, err := checkoutSignalResourceSnapshotNow()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func checkoutSignalResourceSnapshotNow() (checkoutSignalResourceSnapshot, error) {
	snapshot := checkoutSignalResourceSnapshot{
		fds:        -1,
		goroutines: runtime.NumGoroutine(),
		rssBytes:   -1,
		threads:    -1,
	}
	entries, err := os.ReadDir("/dev/fd")
	if err == nil {
		snapshot.fds = len(entries)
	} else if runtime.GOOS == "darwin" {
		return snapshot, fmt.Errorf("count /dev/fd: %w", err)
	}
	if runtime.GOOS != "darwin" {
		return snapshot, nil
	}

	pid := strconv.Itoa(os.Getpid())
	rssOutput, err := exec.Command("ps", "-o", "rss=", "-p", pid).Output()
	if err != nil {
		return snapshot, fmt.Errorf("read RSS with ps: %w", err)
	}
	rssFields := strings.Fields(string(rssOutput))
	if len(rssFields) == 0 {
		return snapshot, fmt.Errorf("read RSS with ps: empty output")
	}
	rssKiB, err := strconv.ParseInt(rssFields[0], 10, 64)
	if err != nil {
		return snapshot, fmt.Errorf("parse ps RSS %q: %w", rssFields[0], err)
	}
	snapshot.rssBytes = rssKiB * 1024

	threadOutput, err := exec.Command("ps", "-M", "-p", pid).Output()
	if err != nil {
		return snapshot, fmt.Errorf("read thread count with ps: %w", err)
	}
	threadLines := strings.Split(strings.TrimSpace(string(threadOutput)), "\n")
	if len(threadLines) < 2 {
		return snapshot, fmt.Errorf("read thread count with ps: unexpected output %q", strings.TrimSpace(string(threadOutput)))
	}
	snapshot.threads = len(threadLines) - 1
	return snapshot, nil
}

func checkoutSignalResourceRaceEnabled() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, setting := range info.Settings {
		if setting.Key == "-race" && setting.Value == "true" {
			return true
		}
	}
	return false
}

func absCheckoutSignalResourceInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
