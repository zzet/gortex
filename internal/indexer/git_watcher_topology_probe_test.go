package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/zzet/gortex/internal/gitstate"
	"go.uber.org/zap"
)

func TestGitWatcherRefWatchesUseOnlyExactFiles(t *testing.T) {
	watcher := newGitRefWatcherFixture(t)
	var mu sync.Mutex
	added := make(map[string]int)
	removed := make(map[string]int)
	watcher.refAdd = func(path string) error {
		mu.Lock()
		added[filepath.Clean(path)]++
		mu.Unlock()
		return nil
	}
	watcher.refRemove = func(path string) error {
		mu.Lock()
		removed[filepath.Clean(path)]++
		mu.Unlock()
		return nil
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("GitWatcher.Start: %v", err)
	}

	watcher.mu.Lock()
	gitDir := watcher.gitDir
	commonDir := watcher.commonDir
	watcher.mu.Unlock()
	headPath := filepath.Join(gitDir, "HEAD")
	packedRefs := filepath.Join(commonDir, "packed-refs")
	mainRef := filepath.Join(commonDir, "refs", "heads", "main")
	headsDir := filepath.Join(commonDir, "refs", "heads")
	assertExactRefPaths(t, watcher, headPath, packedRefs, mainRef)
	watcher.mu.Lock()
	_, watchedDirectory := watcher.refPaths[filepath.Clean(headsDir)]
	watcher.mu.Unlock()
	if watchedDirectory {
		t.Fatalf("registered recursive refs directory %s", headsDir)
	}

	featureRef := filepath.Join(commonDir, "refs", "heads", "feature")
	if err := os.WriteFile(featureRef, []byte(fmt.Sprintf("%040d\n", 0)), 0o644); err != nil {
		t.Fatalf("write feature ref: %v", err)
	}
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/feature\n"), 0o644); err != nil {
		t.Fatalf("switch symbolic HEAD: %v", err)
	}
	if err := watcher.refreshRequiredWatchesChecked(); err != nil {
		t.Fatalf("refresh exact ref watches: %v", err)
	}
	assertExactRefPaths(t, watcher, headPath, packedRefs, featureRef)
	mu.Lock()
	mainRemoved := removed[filepath.Clean(mainRef)]
	mu.Unlock()
	if mainRemoved != 1 {
		t.Fatalf("retired main ref removals = %d, want 1", mainRemoved)
	}

	if !watcher.invalidateRefWatch(featureRef) {
		t.Fatal("active feature ref was not registered before replacement")
	}
	if err := watcher.refreshRequiredWatchesChecked(); err != nil {
		t.Fatalf("re-register replaced active ref: %v", err)
	}
	mu.Lock()
	featureAdds := added[filepath.Clean(featureRef)]
	mu.Unlock()
	if featureAdds != 2 {
		t.Fatalf("feature ref registrations = %d, want initial plus replacement retry", featureAdds)
	}
}

func TestGitWatcherRefProbeDetectsInitiallyMissingActiveLooseRef(t *testing.T) {
	watcher := newGitRefWatcherFixture(t)
	watcher.debounce = time.Hour
	watcher.topologyProbeInterval = time.Hour
	activeRef := filepath.Join(watcher.repoPath, ".git", "refs", "heads", "main")
	if err := os.Remove(activeRef); err != nil {
		t.Fatalf("remove unborn active ref: %v", err)
	}
	var mu sync.Mutex
	adds := make(map[string]int)
	watcher.refAdd = func(path string) error {
		mu.Lock()
		adds[filepath.Clean(path)]++
		mu.Unlock()
		return nil
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("GitWatcher.Start: %v", err)
	}
	watcher.mu.Lock()
	activeRef = filepath.Join(watcher.commonDir, "refs", "heads", "main")
	_, initiallyWatched := watcher.refPaths[filepath.Clean(activeRef)]
	watcher.mu.Unlock()
	if initiallyWatched {
		t.Fatal("missing active loose ref was registered before it existed")
	}

	if err := os.WriteFile(activeRef, []byte(strings.Repeat("1", 40)+"\n"), 0o644); err != nil {
		t.Fatalf("create active loose ref without changing HEAD: %v", err)
	}
	watcher.mu.Lock()
	gitDir, commonDir, previousSignature := watcher.gitDir, watcher.commonDir, watcher.refProbeSignature
	watcher.mu.Unlock()
	observed, err := observeGitRefProbe(gitDir, commonDir)
	if err != nil {
		t.Fatalf("observe created active ref: %v", err)
	}
	if observed.signature == previousSignature {
		t.Fatalf("active-ref creation did not change control signature; desired=%#v", observed.desired)
	}
	epoch := topologyProbeEpochSnapshot(t, watcher)
	if err := watcher.probeGitStateOnce(context.Background(), epoch); err != nil {
		t.Fatalf("probe newly created active ref: %v", err)
	}
	watcher.mu.Lock()
	_, watched := watcher.refPaths[filepath.Clean(activeRef)]
	registered := clonePathSet(watcher.refPaths)
	currentSignature := watcher.refProbeSignature
	watcher.mu.Unlock()
	mu.Lock()
	activeAdds := adds[filepath.Clean(activeRef)]
	mu.Unlock()
	if !watched || activeAdds != 1 {
		t.Fatalf("created active ref registration = watched:%t adds:%d, want true/1; desired=%#v registered=%#v signature_published=%t", watched, activeAdds, observed.desired, registered, currentSignature == observed.signature)
	}
}

func TestGitWatcherTopologyFailedObservationDefersStableTicksToRetry(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	watcher.topologyProbeInterval = time.Hour
	watcher.debounce = time.Hour
	if err := watcher.refreshTopologyWatchesChecked(); err != nil {
		t.Fatalf("baseline topology refresh: %v", err)
	}
	epoch := topologyProbeEpochSnapshot(t, watcher)

	var attempts atomic.Int64
	var fail atomic.Bool
	fail.Store(true)
	watcher.inventory = func(context.Context, string) (*gitstate.FamilyInventory, error) {
		attempts.Add(1)
		if fail.Load() {
			return nil, errGitTopologyProbeUnstable
		}
		return fixture.inventory, nil
	}
	watcher.topologyRetryBase = time.Hour
	watcher.topologyRetryMax = time.Hour
	if err := os.RemoveAll(fixture.worktreesDir); err != nil {
		t.Fatalf("change topology signature: %v", err)
	}
	firstErr := watcher.probeTopologyOnce(context.Background(), epoch)
	if !errors.Is(firstErr, errGitTopologyProbeUnstable) {
		t.Fatalf("first changed observation error = %v, want unstable", firstErr)
	}
	watcher.recordTopologyRefresh(firstErr)
	for i := 0; i < 64; i++ {
		if err := watcher.probeTopologyOnce(context.Background(), epoch); err != nil {
			t.Fatalf("stable failed observation tick %d: %v", i, err)
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("stable failed observation inventories = %d, want one direct attempt", got)
	}

	// Recovery is owned by the same retry path, not by the 1s probe loop.
	watcher.cancelTopologyRetry()
	watcher.topologyRetryBase = 5 * time.Millisecond
	watcher.topologyRetryMax = 5 * time.Millisecond
	fail.Store(false)
	watcher.recordTopologyRefresh(firstErr)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if attempts.Load() >= 2 && watcher.topologyDegradedReason() == "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("retry recovery inventories = %d, want direct failure plus one retry", got)
	}
	if reason := watcher.topologyDegradedReason(); reason != "" {
		t.Fatalf("retry recovery remained degraded: %s", reason)
	}
}

func TestGitWatcherTopologyRefreshRejectsMidInventoryAdminAdd(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	watcher.topologyProbeInterval = time.Hour

	newRoot := filepath.Join(filepath.Dir(fixture.roots[0]), "worktree-new")
	newAdmin := filepath.Join(fixture.worktreesDir, "worktree-new")
	updated := *fixture.inventory
	updated.Records = append(append([]gitstate.WorktreeRecord(nil), fixture.inventory.Records...), gitstate.WorktreeRecord{
		Path:           newRoot,
		AdminName:      "worktree-new",
		RootAccessible: true,
	})
	var calls atomic.Int64
	watcher.inventory = func(context.Context, string) (*gitstate.FamilyInventory, error) {
		if calls.Add(1) == 1 {
			if err := os.MkdirAll(newRoot, 0o755); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(newAdmin, 0o755); err != nil {
				return nil, err
			}
			return fixture.inventory, nil
		}
		return &updated, nil
	}
	if err := watcher.refreshTopologyWatchesChecked(); !errors.Is(err, errGitTopologyProbeUnstable) {
		t.Fatalf("torn topology refresh error = %v, want unstable", err)
	}
	watcher.mu.Lock()
	publishedAfterTorn := watcher.topologySignature
	watcher.mu.Unlock()
	if publishedAfterTorn != "" {
		t.Fatal("torn inventory was published")
	}
	if err := watcher.refreshTopologyWatchesChecked(); err != nil {
		t.Fatalf("stable recovery refresh: %v", err)
	}
	t.Cleanup(watcher.stopTopologyProbe)
	watcher.mu.Lock()
	_, hasNewRoot := watcher.worktreeRoots[filepath.Clean(newRoot)]
	watcher.mu.Unlock()
	if !hasNewRoot || calls.Load() != 2 {
		t.Fatalf("stable recovery = new-root:%t inventories:%d, want true/2", hasNewRoot, calls.Load())
	}
}

func TestGitWatcherTopologyProbeAndRetrySerializeInventory(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	watcher.topologyProbeInterval = time.Hour
	watcher.debounce = time.Hour
	if err := watcher.refreshTopologyWatchesChecked(); err != nil {
		t.Fatalf("baseline topology refresh: %v", err)
	}
	epoch := topologyProbeEpochSnapshot(t, watcher)
	if err := os.RemoveAll(fixture.worktreesDir); err != nil {
		t.Fatalf("change topology signature: %v", err)
	}

	var calls atomic.Int64
	var active atomic.Int64
	var maximum atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	watcher.inventory = func(context.Context, string) (*gitstate.FamilyInventory, error) {
		call := calls.Add(1)
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		defer active.Add(-1)
		if call == 1 {
			close(entered)
			<-release
		}
		return fixture.inventory, nil
	}
	probeResult := make(chan error, 1)
	go func() { probeResult <- watcher.probeTopologyOnce(context.Background(), epoch) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("direct topology inventory did not enter")
	}
	watcher.topologyRetryBase = time.Millisecond
	watcher.topologyRetryMax = time.Millisecond
	watcher.recordTopologyRefresh(errors.New("force serialized retry"))
	time.Sleep(10 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("retry inventory overlapped blocked probe: calls=%d", got)
	}
	close(release)
	if err := <-probeResult; err != nil {
		t.Fatalf("direct topology probe: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent inventories = %d, want 1", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("serialized inventory calls = %d, want probe plus retry", got)
	}
}

func TestGitWatcherStopJoinsEnteredReconcileCallbackAndConcurrentCallers(t *testing.T) {
	watcher := newGitRefWatcherFixture(t)
	watcher.topologyProbeInterval = time.Hour
	watcher.debounce = time.Millisecond
	if err := watcher.Start(); err != nil {
		t.Fatalf("GitWatcher.Start: %v", err)
	}
	watcher.mu.Lock()
	headPath := filepath.Join(watcher.gitDir, "HEAD")
	watcher.mu.Unlock()
	if !watcher.invalidateRefWatch(headPath) {
		t.Fatal("HEAD watch was not registered before callback test")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int64
	watcher.refAdd = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(headPath) {
			attempts.Add(1)
			close(entered)
			<-release
		}
		return nil
	}
	watcher.scheduleReconcile("joined-stop-test")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reconcile callback did not enter ref refresh")
	}

	firstStop := make(chan error, 1)
	secondStop := make(chan error, 1)
	go func() { firstStop <- watcher.Stop() }()
	go func() { secondStop <- watcher.Stop() }()
	for _, result := range []<-chan error{firstStop, secondStop} {
		select {
		case err := <-result:
			t.Fatalf("Stop returned before entered callback released: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	close(release)
	for _, result := range []<-chan error{firstStop, secondStop} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("GitWatcher.Stop: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Stop did not join entered reconcile callback")
		}
	}
	attemptsAfterStop := attempts.Load()
	time.Sleep(10 * time.Millisecond)
	if got := attempts.Load(); got != attemptsAfterStop {
		t.Fatalf("ref registration continued after Stop: %d -> %d", attemptsAfterStop, got)
	}
}

func TestGitWatcherTopologyRefreshDoesNotStartControlProbeBeforeStart(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	if err := watcher.refreshTopologyWatchesChecked(); err != nil {
		t.Fatalf("refresh topology baseline: %v", err)
	}
	watcher.topologyProbeMu.Lock()
	probeRunning := watcher.topologyProbeCancel != nil || watcher.topologyProbeDone != nil
	watcher.topologyProbeMu.Unlock()
	if probeRunning {
		t.Fatal("topology refresh started the control probe before GitWatcher.Start published its SHA baseline")
	}
}

func TestGitWatcherAtomicRefReplacementDoesNotScheduleTopologyRetry(t *testing.T) {
	watcher := newGitRefWatcherFixture(t)
	watcher.topologyProbeInterval = time.Hour
	watcher.debounce = 5 * time.Millisecond
	watcher.topologyRetryBase = time.Hour
	watcher.topologyRetryMax = time.Hour
	activeRefSuffix := filepath.Join("refs", "heads", "main")
	var activeAdds atomic.Int64
	watcher.refAdd = func(path string) error {
		err := watcher.fsw.Add(path)
		if err == nil && strings.HasSuffix(filepath.Clean(path), activeRefSuffix) {
			activeAdds.Add(1)
		}
		return err
	}
	watcher.refRemove = watcher.fsw.Remove
	originalInventory := watcher.inventory
	var inventoryCalls atomic.Int64
	watcher.inventory = func(ctx context.Context, path string) (*gitstate.FamilyInventory, error) {
		inventoryCalls.Add(1)
		return originalInventory(ctx, path)
	}
	if err := watcher.Start(); err != nil {
		t.Fatalf("GitWatcher.Start: %v", err)
	}
	if got := activeAdds.Load(); got != 1 {
		t.Fatalf("initial active-ref registrations = %d, want 1", got)
	}
	watcher.mu.Lock()
	activeRef := filepath.Join(watcher.commonDir, activeRefSuffix)
	watcher.mu.Unlock()

	replacement := filepath.Join(filepath.Dir(activeRef), ".main-replacement")
	if err := os.WriteFile(replacement, []byte(strings.Repeat("1", 40)+"\n"), 0o644); err != nil {
		t.Fatalf("write replacement ref: %v", err)
	}
	if err := os.Rename(replacement, activeRef); err != nil {
		t.Fatalf("atomically replace active ref: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && activeAdds.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	if got := activeAdds.Load(); got != 2 {
		t.Fatalf("active-ref registrations after atomic replacement = %d, want 2", got)
	}
	if reason := watcher.topologyDegradedReason(); reason != "" {
		t.Fatalf("normal atomic ref replacement entered topology degradation: %s", reason)
	}
	if watcher.topologyRetryPending() {
		t.Fatal("normal atomic ref replacement scheduled the topology retry path")
	}
	if got := inventoryCalls.Load(); got != 0 {
		t.Fatalf("normal atomic ref replacement inventories = %d, want 0", got)
	}
}

func TestGitWatcherStopSuppressesQueuedTopologyCallback(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	watcher.debounce = 25 * time.Millisecond
	var callbacks atomic.Int64
	watcher.OnWorktreeChange(func(string) { callbacks.Add(1) })
	watcher.scheduleTopologyChange("queued-before-stop")
	if err := watcher.Stop(); err != nil {
		t.Fatalf("GitWatcher.Stop: %v", err)
	}
	time.Sleep(2 * watcher.debounce)
	if got := callbacks.Load(); got != 0 {
		t.Fatalf("queued topology callbacks after Stop = %d, want 0", got)
	}
}

func TestGitWatcherStopJoinsAdmittedCoalescedReconcileContinuation(t *testing.T) {
	root := t.TempDir()
	runGitWatcherTestCommand(t, root, "init", "-q")
	sourcePath := filepath.Join(root, "coalesced.go")
	if err := os.WriteFile(sourcePath, []byte("package coalesced\n\nfunc Value() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("write initial source: %v", err)
	}
	runGitWatcherTestCommand(t, root, "add", "coalesced.go")
	runGitWatcherTestCommand(t, root,
		"-c", "user.name=Gortex Test", "-c", "user.email=gortex@example.invalid",
		"commit", "-qm", "initial")
	initialSHA := strings.TrimSpace(runGitWatcherTestOutput(t, root, "rev-parse", "HEAD"))
	if err := os.WriteFile(sourcePath, []byte("package coalesced\n\nfunc Value() int { return 2 }\n"), 0o644); err != nil {
		t.Fatalf("write changed source: %v", err)
	}
	runGitWatcherTestCommand(t, root, "add", "coalesced.go")
	runGitWatcherTestCommand(t, root,
		"-c", "user.name=Gortex Test", "-c", "user.email=gortex@example.invalid",
		"commit", "-qm", "changed")

	watcher, err := NewGitWatcher(root, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("NewGitWatcher: %v", err)
	}
	t.Cleanup(func() {
		if err := watcher.Stop(); err != nil {
			t.Errorf("GitWatcher.Stop: %v", err)
		}
	})
	watcher.mu.Lock()
	watcher.lastSHA = initialSHA
	watcher.mu.Unlock()
	batchEntered := make(chan struct{})
	batchRelease := make(chan struct{})
	var batchCalls atomic.Int64
	watcher.batchReindex = func([]string) (*IndexResult, error) {
		if batchCalls.Add(1) == 1 {
			close(batchEntered)
			<-batchRelease
		}
		return nil, errors.New("intentional batch stop barrier")
	}
	continuationAdmitted := make(chan struct{})
	continuationRelease := make(chan struct{})
	watcher.reconcileContinuationAdmitted = func() {
		close(continuationAdmitted)
		<-continuationRelease
	}
	initialDone := make(chan struct{})
	go func() {
		watcher.reconcile("initial")
		close(initialDone)
	}()
	select {
	case <-batchEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("initial reconcile did not enter batch reindex")
	}
	watcher.reconcile("coalesce-request")
	close(batchRelease)
	select {
	case <-continuationAdmitted:
	case <-time.After(2 * time.Second):
		t.Fatal("coalesced continuation was not admitted")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- watcher.Stop() }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned before the admitted continuation launched: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(continuationRelease)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("GitWatcher.Stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not join the admitted coalesced continuation")
	}
	select {
	case <-initialDone:
	case <-time.After(time.Second):
		t.Fatal("initial reconcile did not return after continuation admission")
	}
	if got := batchCalls.Load(); got != 1 {
		t.Fatalf("batch reindexes after Stop = %d, want only the admitted initial run", got)
	}
	time.Sleep(10 * time.Millisecond)
	if got := batchCalls.Load(); got != 1 {
		t.Fatalf("post-Stop coalesced reconcile mutated the graph: batch calls=%d", got)
	}
}

func assertExactRefPaths(t *testing.T, watcher *GitWatcher, want ...string) {
	t.Helper()
	watcher.mu.Lock()
	got := clonePathSet(watcher.refPaths)
	watcher.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("exact ref path count = %d, want %d: %#v", len(got), len(want), got)
	}
	for _, path := range want {
		if _, ok := got[filepath.Clean(path)]; !ok {
			t.Fatalf("exact ref path %s is not registered: %#v", path, got)
		}
	}
}

func assertOnlyExactRefWatches(tb testing.TB, watcher *GitWatcher) int {
	tb.Helper()
	watcher.mu.Lock()
	fsw := watcher.fsw
	refs := clonePathSet(watcher.refPaths)
	watcher.mu.Unlock()
	if fsw == nil {
		tb.Fatal("fsnotify watcher is unavailable")
	}
	watchList := fsw.WatchList()
	if len(watchList) != len(refs) {
		tb.Fatalf("actual fsnotify registrations = %#v, exact refs = %#v", watchList, refs)
	}
	for _, path := range watchList {
		if _, exactRef := refs[filepath.Clean(path)]; !exactRef {
			tb.Fatalf("non-ref fsnotify registration %s; all=%#v", path, watchList)
		}
	}
	return len(watchList)
}

func writeTopologyProbeFile(tb testing.TB, path, contents string) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		tb.Fatal(err)
	}
}

func prepareLinkedWorktreeHead(
	tb testing.TB, fixture *topologyWatchFixture, index int, branch, commit string,
) (headPath, refPath string) {
	tb.Helper()
	name := fmt.Sprintf("worktree-%03d", index)
	adminDir := filepath.Join(fixture.worktreesDir, name)
	refName := "refs/heads/" + branch
	headPath = filepath.Join(adminDir, "HEAD")
	refPath = filepath.Join(fixture.commonDir, filepath.FromSlash(refName))
	writeTopologyProbeFile(tb, filepath.Join(adminDir, "gitdir"), filepath.Join(fixture.roots[index], ".git")+"\n")
	writeTopologyProbeFile(tb, filepath.Join(adminDir, "commondir"), "../..\n")
	writeTopologyProbeFile(tb, headPath, "ref: "+refName+"\n")
	writeTopologyProbeFile(tb, refPath, commit)
	return headPath, refPath
}

func topologyFixtureRoots(fixture *topologyWatchFixture) map[string]struct{} {
	roots := make(map[string]struct{}, len(fixture.roots))
	for _, root := range fixture.roots {
		roots[filepath.Clean(root)] = struct{}{}
	}
	return roots
}

func TestGitWatcherTopologyProbeDispatchesLinkedWorktreeHeadTransitions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *topologyWatchFixture, string, string)
	}{
		{
			name: "symbolic branch switch",
			mutate: func(t *testing.T, fixture *topologyWatchFixture, headPath, _ string) {
				featureRef := filepath.Join(fixture.commonDir, "refs", "heads", "feature")
				writeTopologyProbeFile(t, featureRef, strings.Repeat("a", 40)+"\n")
				writeTopologyProbeFile(t, headPath, "ref: refs/heads/feature\n")
			},
		},
		{
			name: "detached HEAD move",
			mutate: func(t *testing.T, _ *topologyWatchFixture, headPath, _ string) {
				writeTopologyProbeFile(t, headPath, strings.Repeat("b", 40)+"\n")
			},
		},
		{
			name: "same branch loose ref advance",
			mutate: func(t *testing.T, _ *topologyWatchFixture, _, refPath string) {
				writeTopologyProbeFile(t, refPath, strings.Repeat("c", 40)+"\n")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newTopologyWatchFixture(t, 1)
			headPath, refPath := prepareLinkedWorktreeHead(
				t, fixture, 0, "worktree-000", strings.Repeat("a", 40)+"\n",
			)
			watcher := fixture.watcher(0)
			watcher.topologyOwned = true
			watcher.topologyOwnerEpoch = 1
			watcher.topologyProbeInterval = time.Hour
			watcher.debounce = 2 * time.Millisecond
			callbacks := make(chan time.Time, 2)
			watcher.OnWorktreeChange(func(string) { callbacks <- time.Now() })
			if err := watcher.refreshTopologyWatchesChecked(); err != nil {
				t.Fatalf("baseline topology refresh: %v", err)
			}
			t.Cleanup(watcher.stopTopologyProbe)
			epoch := topologyProbeEpochSnapshot(t, watcher)
			baselineInventories := fixture.inventoryCalls.Load()

			started := time.Now()
			tt.mutate(t, fixture, headPath, refPath)
			if err := watcher.probeTopologyOnce(context.Background(), epoch); err != nil {
				t.Fatalf("probe linked-worktree HEAD transition: %v", err)
			}
			waitForPromptTopologyCallback(t, callbacks, started)
			if got := fixture.inventoryCalls.Load(); got != baselineInventories+1 {
				t.Fatalf("transition inventories = %d, want baseline + 1 (%d)", got, baselineInventories+1)
			}

			// The accepted bytes are the new gate. Stable ticks do no inventory
			// and publish no second callback, even under a burst of reconciles.
			for i := 0; i < 64; i++ {
				if err := watcher.probeTopologyOnce(context.Background(), epoch); err != nil {
					t.Fatalf("stable probe %d: %v", i, err)
				}
			}
			if got := fixture.inventoryCalls.Load(); got != baselineInventories+1 {
				t.Fatalf("stable inventories = %d, want %d", got, baselineInventories+1)
			}
			select {
			case <-callbacks:
				t.Fatal("stable linked-worktree HEAD bytes dispatched a second callback")
			case <-time.After(5 * watcher.debounce):
			}
		})
	}
}

func TestGitWatcherTopologyRefreshRejectsMidInventoryWorktreeHeadAdvance(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	_, refPath := prepareLinkedWorktreeHead(
		t, fixture, 0, "worktree-000", strings.Repeat("a", 40)+"\n",
	)
	watcher := fixture.watcher(0)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	watcher.topologyProbeInterval = time.Hour
	originalInventory := watcher.inventory
	var calls atomic.Int64
	watcher.inventory = func(ctx context.Context, path string) (*gitstate.FamilyInventory, error) {
		if calls.Add(1) == 1 {
			writeTopologyProbeFile(t, refPath, strings.Repeat("b", 40)+"\n")
		}
		return originalInventory(ctx, path)
	}
	if err := watcher.refreshTopologyWatchesChecked(); !errors.Is(err, errGitTopologyProbeUnstable) {
		t.Fatalf("mid-inventory HEAD advance error = %v, want unstable", err)
	}
	watcher.mu.Lock()
	publishedAfterTorn := watcher.topologySignature
	watcher.mu.Unlock()
	if publishedAfterTorn != "" {
		t.Fatal("torn linked-worktree HEAD inventory was published")
	}
	if err := watcher.refreshTopologyWatchesChecked(); err != nil {
		t.Fatalf("stable HEAD recovery refresh: %v", err)
	}
	t.Cleanup(watcher.stopTopologyProbe)
	if got := calls.Load(); got != 2 {
		t.Fatalf("HEAD recovery inventories = %d, want torn + stable", got)
	}
}

func TestGitWatcherTopologyProbeStableTickDoesNotReinventory(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	watcher.topologyProbeInterval = time.Hour
	if err := watcher.refreshTopologyWatchesChecked(); err != nil {
		t.Fatalf("start topology probe: %v", err)
	}
	t.Cleanup(watcher.stopTopologyProbe)
	epoch := topologyProbeEpochSnapshot(t, watcher)

	for i := 0; i < 128; i++ {
		if err := watcher.probeTopologyOnce(context.Background(), epoch); err != nil {
			t.Fatalf("stable probe tick %d: %v", i, err)
		}
	}
	if got := fixture.inventoryCalls.Load(); got != 1 {
		t.Fatalf("stable probe inventories = %d, want baseline only", got)
	}
	if _, paths := watcherTopologySnapshot(watcher); paths != 0 {
		t.Fatalf("topology fsnotify paths = %d, want 0", paths)
	}
}

func TestGitWatcherTopologyProbeDetectsFirstDirectoryTransitionsPromptly(t *testing.T) {
	if defaultGitTopologyProbeInterval != time.Second {
		t.Fatalf("default topology probe interval = %s, want 1s", defaultGitTopologyProbeInterval)
	}
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	watcher.topologyProbeInterval = 10 * time.Millisecond
	watcher.debounce = 5 * time.Millisecond
	callbacks := make(chan time.Time, 4)
	watcher.OnWorktreeChange(func(string) { callbacks <- time.Now() })
	if err := watcher.refreshTopologyWatchesChecked(); err != nil {
		t.Fatalf("start topology probe: %v", err)
	}
	_ = topologyProbeEpochSnapshot(t, watcher)
	t.Cleanup(watcher.stopTopologyProbe)

	started := time.Now()
	if err := os.RemoveAll(fixture.worktreesDir); err != nil {
		t.Fatalf("remove first worktrees directory: %v", err)
	}
	waitForPromptTopologyCallback(t, callbacks, started)
	callsAfterRemoval := fixture.inventoryCalls.Load()
	time.Sleep(8 * watcher.topologyProbeInterval)
	if got := fixture.inventoryCalls.Load(); got != callsAfterRemoval {
		t.Fatalf("stable absent-directory inventories = %d, want %d", got, callsAfterRemoval)
	}

	started = time.Now()
	if err := os.MkdirAll(fixture.worktreesDir, 0o755); err != nil {
		t.Fatalf("recreate first worktrees directory: %v", err)
	}
	waitForPromptTopologyCallback(t, callbacks, started)
}

func waitForPromptTopologyCallback(t *testing.T, callbacks <-chan time.Time, started time.Time) {
	t.Helper()
	select {
	case observed := <-callbacks:
		if elapsed := observed.Sub(started); elapsed > 250*time.Millisecond {
			t.Fatalf("topology transition latency = %s, want <=250ms with 10ms test cadence", elapsed)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("topology probe did not publish the transition promptly")
	}
}

func TestGitWatcherTopologyProbeStopCancelsAndJoinsInventory(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	watcher.topologyProbeInterval = 5 * time.Millisecond
	originalInventory := watcher.inventory
	var calls atomic.Int64
	entered := make(chan struct{})
	exited := make(chan struct{})
	var enteredOnce sync.Once
	var exitedOnce sync.Once
	watcher.inventory = func(ctx context.Context, path string) (*gitstate.FamilyInventory, error) {
		if calls.Add(1) == 1 {
			return originalInventory(ctx, path)
		}
		enteredOnce.Do(func() { close(entered) })
		<-ctx.Done()
		exitedOnce.Do(func() { close(exited) })
		return nil, ctx.Err()
	}
	if err := watcher.refreshTopologyWatchesChecked(); err != nil {
		t.Fatalf("start topology probe: %v", err)
	}
	_ = topologyProbeEpochSnapshot(t, watcher)
	if err := os.RemoveAll(fixture.worktreesDir); err != nil {
		t.Fatalf("change topology gate: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("probe inventory did not enter")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- watcher.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("GitWatcher.Stop: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("GitWatcher.Stop did not cancel and join the probe")
	}
	select {
	case <-exited:
	default:
		t.Fatal("Stop returned before the canceled inventory exited")
	}
}

func TestGitWatcherTopologyRegistrationsStayBoundedWithLargeCheckout(t *testing.T) {
	for _, files := range []int{1, 2000} {
		t.Run(fmt.Sprintf("files_%d", files), func(t *testing.T) {
			fixture := newTopologyWatchFixture(t, 1)
			populateTopologyRoot(t, fixture.roots[0], files)
			prepareExactRefFixture(t, fixture.commonDir)
			watcher := fixture.watcher(0)
			watcher.topologyOwned = true
			watcher.topologyOwnerEpoch = 1
			watcher.topologyProbeInterval = time.Hour
			fsw, err := fsnotify.NewWatcher()
			if err != nil {
				t.Fatalf("fsnotify.NewWatcher: %v", err)
			}
			watcher.fsw = fsw
			watcher.refAdd = nil
			t.Cleanup(func() { _ = fsw.Close() })
			if err := watcher.refreshRequiredWatchesChecked(); err != nil {
				t.Fatalf("refresh bounded watches: %v", err)
			}
			if got := assertOnlyExactRefWatches(t, watcher); got != 3 {
				t.Fatalf("actual fsnotify registrations = %d, want HEAD + packed-refs + active ref", got)
			}
			if _, paths := watcherTopologySnapshot(watcher); paths != 0 {
				t.Fatalf("topology fsnotify paths for %d files = %d, want 0", files, paths)
			}
		})
	}
}

func prepareExactRefFixture(tb testing.TB, commonDir string) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Join(commonDir, "refs", "heads"), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commonDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commonDir, "packed-refs"), []byte("# packed\n"), 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commonDir, "refs", "heads", "main"), []byte(fmt.Sprintf("%040d\n", 0)), 0o644); err != nil {
		tb.Fatal(err)
	}
}

func populateTopologyRoot(tb testing.TB, root string, files int) {
	tb.Helper()
	for i := 0; i < files; i++ {
		path := filepath.Join(root, fmt.Sprintf("source-%05d.go", i))
		if err := os.WriteFile(path, []byte("package scaling\n"), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
}

func topologyProbeEpochSnapshot(tb testing.TB, watcher *GitWatcher) uint64 {
	tb.Helper()
	watcher.startTopologyProbe()
	watcher.topologyProbeMu.Lock()
	defer watcher.topologyProbeMu.Unlock()
	if watcher.topologyProbeCancel == nil {
		tb.Fatal("topology probe is not running")
	}
	return watcher.topologyProbeEpoch
}

func BenchmarkGitWatcherLinkedWorktreeHeadProbe(b *testing.B) {
	for _, worktrees := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("stable/worktrees_%d", worktrees), func(b *testing.B) {
			fixture := newTopologyWatchFixture(b, worktrees)
			for i := 0; i < worktrees; i++ {
				prepareLinkedWorktreeHead(
					b, fixture, i, fmt.Sprintf("worktree-%03d", i), fmt.Sprintf("%040x\n", i+1),
				)
			}
			roots := topologyFixtureRoots(fixture)
			baseline, err := observeGitTopologyProbe(fixture.worktreesDir, roots)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				observed, err := observeGitTopologyProbe(fixture.worktreesDir, roots)
				if err != nil {
					b.Fatal(err)
				}
				if observed.signature != baseline.signature {
					b.Fatal("stable linked-worktree HEAD signature changed")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(worktrees), "worktrees")
			b.ReportMetric(0, "inventory/op")
			b.ReportMetric(0, "topology-watch-paths")
		})

		b.Run(fmt.Sprintf("transition/worktrees_%d", worktrees), func(b *testing.B) {
			fixture := newTopologyWatchFixture(b, worktrees)
			var firstRef string
			for i := 0; i < worktrees; i++ {
				_, refPath := prepareLinkedWorktreeHead(
					b, fixture, i, fmt.Sprintf("worktree-%03d", i), fmt.Sprintf("%040x\n", i+1),
				)
				if i == 0 {
					firstRef = refPath
				}
			}
			roots := topologyFixtureRoots(fixture)
			previous, err := observeGitTopologyProbe(fixture.worktreesDir, roots)
			if err != nil {
				b.Fatal(err)
			}
			next := 2
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				writeTopologyProbeFile(b, firstRef, fmt.Sprintf("%040x\n", next))
				next = 3 - next
				b.StartTimer()
				observed, err := observeGitTopologyProbe(fixture.worktreesDir, roots)
				if err != nil {
					b.Fatal(err)
				}
				if observed.signature == previous.signature {
					b.Fatal("linked-worktree HEAD transition kept the stable signature")
				}
				previous = observed
			}
			b.StopTimer()
			b.ReportMetric(float64(worktrees), "worktrees")
			b.ReportMetric(0, "inventory/op")
			b.ReportMetric(0, "topology-watch-paths")
		})
	}
}

func BenchmarkGitWatcherControlProbeStableTick(b *testing.B) {
	for _, files := range []int{1, 2000} {
		b.Run(fmt.Sprintf("files_%d", files), func(b *testing.B) {
			fixture := newTopologyWatchFixture(b, 1)
			populateTopologyRoot(b, fixture.roots[0], files)
			prepareExactRefFixture(b, fixture.commonDir)
			watcher := fixture.watcher(0)
			watcher.topologyOwned = true
			watcher.topologyOwnerEpoch = 1
			watcher.topologyProbeInterval = time.Hour
			watcher.refAdd = func(string) error { return nil }
			if err := watcher.refreshRequiredWatchesChecked(); err != nil {
				b.Fatalf("start control probe: %v", err)
			}
			b.Cleanup(watcher.stopTopologyProbe)
			epoch := topologyProbeEpochSnapshot(b, watcher)
			baselineInventories := fixture.inventoryCalls.Load()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := watcher.probeGitStateOnce(context.Background(), epoch); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if got := fixture.inventoryCalls.Load(); got != baselineInventories {
				b.Fatalf("stable inventories = %d, want %d", got, baselineInventories)
			}
			watcher.mu.Lock()
			refPaths := len(watcher.refPaths)
			watcher.mu.Unlock()
			if refPaths != 3 {
				b.Fatalf("exact ref paths = %d, want 3", refPaths)
			}
			b.ReportMetric(0, "inventory/op")
			b.ReportMetric(float64(refPaths), "exact-ref-paths")
			b.ReportMetric(0, "topology-watch-paths")
			b.ReportMetric(float64(defaultGitTopologyProbeInterval/time.Millisecond), "max-detection-ms")
		})
	}
}

func BenchmarkGitWatcherTopologyProbeStableTick(b *testing.B) {
	for _, files := range []int{1, 2000} {
		b.Run(fmt.Sprintf("files_%d", files), func(b *testing.B) {
			fixture := newTopologyWatchFixture(b, 1)
			populateTopologyRoot(b, fixture.roots[0], files)
			watcher := fixture.watcher(0)
			watcher.topologyOwned = true
			watcher.topologyOwnerEpoch = 1
			watcher.topologyProbeInterval = time.Hour
			if err := watcher.refreshTopologyWatchesChecked(); err != nil {
				b.Fatalf("start topology probe: %v", err)
			}
			b.Cleanup(watcher.stopTopologyProbe)
			epoch := topologyProbeEpochSnapshot(b, watcher)
			baselineInventories := fixture.inventoryCalls.Load()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := watcher.probeTopologyOnce(context.Background(), epoch); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if got := fixture.inventoryCalls.Load(); got != baselineInventories {
				b.Fatalf("stable inventories = %d, want %d", got, baselineInventories)
			}
			b.ReportMetric(0, "inventory/op")
			b.ReportMetric(0, "topology-watch-paths")
			b.ReportMetric(float64(defaultGitTopologyProbeInterval/time.Millisecond), "max-detection-ms")
		})
	}
}

func BenchmarkGitWatcherTopologyProbeTransition(b *testing.B) {
	fixture := newTopologyWatchFixture(b, 1)
	watcher := fixture.watcher(0)
	watcher.topologyOwned = true
	watcher.topologyOwnerEpoch = 1
	watcher.topologyProbeInterval = time.Hour
	if err := watcher.refreshTopologyWatchesChecked(); err != nil {
		b.Fatalf("start topology probe: %v", err)
	}
	b.Cleanup(watcher.stopTopologyProbe)
	epoch := topologyProbeEpochSnapshot(b, watcher)
	present := true
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if present {
			if err := os.RemoveAll(fixture.worktreesDir); err != nil {
				b.Fatal(err)
			}
		} else if err := os.MkdirAll(fixture.worktreesDir, 0o755); err != nil {
			b.Fatal(err)
		}
		present = !present
		b.StartTimer()
		if err := watcher.probeTopologyOnce(context.Background(), epoch); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(1, "inventory/transition")
	b.ReportMetric(0, "topology-watch-paths")
	b.ReportMetric(float64(defaultGitTopologyProbeInterval/time.Millisecond), "max-detection-ms")
}

func BenchmarkGitWatcherBoundedRegistrationByCheckoutSize(b *testing.B) {
	for _, files := range []int{1, 2000} {
		b.Run(fmt.Sprintf("files_%d", files), func(b *testing.B) {
			fixture := newTopologyWatchFixture(b, 1)
			populateTopologyRoot(b, fixture.roots[0], files)
			prepareExactRefFixture(b, fixture.commonDir)
			watcher := fixture.watcher(0)
			watcher.topologyOwned = true
			watcher.topologyOwnerEpoch = 1
			watcher.topologyProbeInterval = time.Hour
			fsw, err := fsnotify.NewWatcher()
			if err != nil {
				b.Fatalf("fsnotify.NewWatcher: %v", err)
			}
			watcher.fsw = fsw
			watcher.refAdd = nil
			b.Cleanup(func() { _ = fsw.Close() })
			if err := watcher.refreshRequiredWatchesChecked(); err != nil {
				b.Fatalf("initial bounded registration: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				watcher.topologyRefreshMu.Lock()
				err := watcher.refreshRefWatchesLocked()
				watcher.topologyRefreshMu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			refPaths := assertOnlyExactRefWatches(b, watcher)
			topologyPaths := len(watcherTopologyWatchList(watcher))
			if refPaths != 3 || topologyPaths != 0 {
				b.Fatalf("registered paths = refs:%d topology:%d, want 3/0", refPaths, topologyPaths)
			}
			b.ReportMetric(float64(refPaths), "exact-ref-paths")
			b.ReportMetric(float64(topologyPaths), "topology-watch-paths")
		})
	}
}
