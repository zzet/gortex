package indexer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/gitstate"
)

type topologyWatchFixture struct {
	commonDir    string
	worktreesDir string
	roots        []string
	inventory    *gitstate.FamilyInventory

	inventoryCalls    atomic.Int64
	registrationCalls atomic.Int64
	mu                sync.Mutex
	active            map[string]int
}

func newTopologyWatchFixture(tb testing.TB, worktrees int) *topologyWatchFixture {
	tb.Helper()
	base := tb.TempDir()
	commonDir := filepath.Join(base, "common.git")
	worktreesDir := filepath.Join(commonDir, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0o755); err != nil {
		tb.Fatal(err)
	}

	fixture := &topologyWatchFixture{
		commonDir:    commonDir,
		worktreesDir: worktreesDir,
		roots:        make([]string, 0, worktrees),
		active:       make(map[string]int),
	}
	records := make([]gitstate.WorktreeRecord, 0, worktrees)
	for i := 0; i < worktrees; i++ {
		name := fmt.Sprintf("worktree-%03d", i)
		root := filepath.Join(base, "roots", name)
		admin := filepath.Join(worktreesDir, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.MkdirAll(admin, 0o755); err != nil {
			tb.Fatal(err)
		}
		fixture.roots = append(fixture.roots, root)
		records = append(records, gitstate.WorktreeRecord{
			Path:           root,
			AdminName:      name,
			IsMain:         i == 0,
			RootAccessible: true,
		})
	}
	fixture.inventory = &gitstate.FamilyInventory{
		CommonDir: commonDir,
		GitDir:    commonDir,
		Records:   records,
	}
	return fixture
}

func (fixture *topologyWatchFixture) watcher(index int) *GitWatcher {
	root := fixture.roots[index%len(fixture.roots)]
	return &GitWatcher{
		repoPath:          root,
		logger:            zap.NewNop(),
		debounce:          5 * time.Millisecond,
		commonDir:         fixture.commonDir,
		worktreesDir:      fixture.worktreesDir,
		topologyPaths:     make(map[string]struct{}),
		worktreeRoots:     make(map[string]struct{}),
		worktreeAdminDirs: make(map[string]struct{}),
		inventory: func(context.Context, string) (*gitstate.FamilyInventory, error) {
			fixture.inventoryCalls.Add(1)
			return fixture.inventory, nil
		},
		topologyAdd: func(path string) error {
			path = filepath.Clean(path)
			fixture.registrationCalls.Add(1)
			fixture.mu.Lock()
			fixture.active[path]++
			fixture.mu.Unlock()
			return nil
		},
		topologyRemove: func(path string) error {
			path = filepath.Clean(path)
			fixture.mu.Lock()
			fixture.active[path]--
			if fixture.active[path] == 0 {
				delete(fixture.active, path)
			}
			fixture.mu.Unlock()
			return nil
		},
	}
}

func (fixture *topologyWatchFixture) activeStats() (unique, duplicates, registrations int) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, count := range fixture.active {
		if count > 0 {
			unique++
			registrations += count
		}
		if count > 1 {
			duplicates += count - 1
		}
	}
	return unique, duplicates, registrations
}

func (fixture *topologyWatchFixture) resetActive() {
	fixture.mu.Lock()
	fixture.active = make(map[string]int)
	fixture.mu.Unlock()
}

func newTopologyRegistry() *MultiWatcher {
	return &MultiWatcher{
		watchers:             make(map[string]*Watcher),
		gitWatchers:          make(map[string]*GitWatcher),
		started:              make(map[string]bool),
		startFailures:        make(map[string]string),
		topologyFamilies:     make(map[string]*topologyWatchFamily),
		topologyFamilyByRepo: make(map[string]string),
		logger:               zap.NewNop(),
		events:               make(chan GraphChangeEvent, 1),
		done:                 make(chan struct{}),
	}
}

func installTopologyWatcher(mw *MultiWatcher, prefix string, watcher *GitWatcher) {
	mw.mu.Lock()
	mw.installStartedGitWatcherLocked(prefix, watcher)
	mw.mu.Unlock()
}

func removeTopologyWatcher(mw *MultiWatcher, prefix string) {
	mw.mu.Lock()
	mw.unregisterTopologyWatcherLocked(prefix)
	delete(mw.gitWatchers, prefix)
	mw.mu.Unlock()
}

func topologyFamilySnapshot(mw *MultiWatcher) (families int, owner string, members int) {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	families = len(mw.topologyFamilies)
	for _, family := range mw.topologyFamilies {
		owner = family.owner
		members = len(family.members)
		break
	}
	return families, owner, members
}

func watcherTopologySnapshot(watcher *GitWatcher) (owned bool, paths int) {
	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	return watcher.topologyOwned, len(watcher.topologyPaths)
}

func waitForTopologyCount(t *testing.T, count *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if count.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("topology callback count = %d, want at least %d", count.Load(), want)
}

func assertOneTopologyCallback(t *testing.T, count *atomic.Int64, debounce time.Duration) {
	t.Helper()
	waitForTopologyCount(t, count, 1)
	time.Sleep(4 * debounce)
	if got := count.Load(); got != 1 {
		t.Fatalf("topology callback count = %d, want exactly 1", got)
	}
}

func TestMultiWatcherTopologyFamilySharesInventoryAndRegistrations(t *testing.T) {
	for _, prefixes := range []int{1, 8, 64} {
		t.Run(fmt.Sprintf("prefixes_%d", prefixes), func(t *testing.T) {
			fixture := newTopologyWatchFixture(t, prefixes)
			mw := newTopologyRegistry()
			watchers := make([]*GitWatcher, 0, prefixes)
			for i := 0; i < prefixes; i++ {
				watcher := fixture.watcher(i)
				watchers = append(watchers, watcher)
				installTopologyWatcher(mw, fmt.Sprintf("repo-%03d", i), watcher)
			}

			families, owner, members := topologyFamilySnapshot(mw)
			if families != 1 || members != prefixes || owner != "repo-000" {
				t.Fatalf("family state = families:%d owner:%q members:%d", families, owner, members)
			}
			if got := fixture.inventoryCalls.Load(); got != 1 {
				t.Fatalf("inventory calls = %d, want 1", got)
			}
			expectedPaths := 2*prefixes + 2 // common dir, worktrees dir, roots, admin dirs
			unique, duplicates, registrations := fixture.activeStats()
			if unique != expectedPaths || registrations != expectedPaths || duplicates != 0 {
				t.Fatalf("topology registrations = unique:%d registrations:%d duplicates:%d, want %d/%d/0", unique, registrations, duplicates, expectedPaths, expectedPaths)
			}

			owners := 0
			for _, watcher := range watchers {
				owned, paths := watcherTopologySnapshot(watcher)
				if owned {
					owners++
					if paths != expectedPaths {
						t.Fatalf("owner paths = %d, want %d", paths, expectedPaths)
					}
				} else if paths != 0 {
					t.Fatalf("follower registered %d topology paths", paths)
				}
			}
			if owners != 1 {
				t.Fatalf("topology owners = %d, want 1", owners)
			}

			var callbacks atomic.Int64
			mw.OnWorktreeChange(func(string, string) { callbacks.Add(1) })
			assertOneTopologyCallback(t, &callbacks, watchers[0].debounce)
			callbacks.Store(0)
			for _, watcher := range watchers {
				watcher.scheduleTopologyChange("test-family-event")
			}
			assertOneTopologyCallback(t, &callbacks, watchers[0].debounce)
			if got := fixture.inventoryCalls.Load(); got != 2 {
				t.Fatalf("inventory calls after one family event = %d, want 2 total (startup + event)", got)
			}

			for i := range watchers {
				removeTopologyWatcher(mw, fmt.Sprintf("repo-%03d", i))
			}
			if unique, duplicates, registrations := fixture.activeStats(); unique != 0 || duplicates != 0 || registrations != 0 {
				t.Fatalf("topology paths remained after family removal: %d/%d/%d", unique, duplicates, registrations)
			}
		})
	}
}

func TestMultiWatcherTopologyOwnerTransfersAndCleansFamily(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 4)
	mw := newTopologyRegistry()
	watchers := make(map[string]*GitWatcher)
	for i := 0; i < 4; i++ {
		prefix := fmt.Sprintf("repo-%02d", i)
		watchers[prefix] = fixture.watcher(i)
		installTopologyWatcher(mw, prefix, watchers[prefix])
	}
	if got := fixture.inventoryCalls.Load(); got != 1 {
		t.Fatalf("initial inventory calls = %d, want 1", got)
	}

	var callbacks atomic.Int64
	mw.OnWorktreeChange(func(string, string) { callbacks.Add(1) })
	assertOneTopologyCallback(t, &callbacks, watchers["repo-00"].debounce)
	callbacks.Store(0)
	watchers["repo-00"].scheduleTopologyChange("queued-before-transfer")

	removeTopologyWatcher(mw, "repo-00")
	families, owner, members := topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-01" || members != 3 {
		t.Fatalf("transferred family = families:%d owner:%q members:%d", families, owner, members)
	}
	if got := fixture.inventoryCalls.Load(); got != 2 {
		t.Fatalf("inventory calls after transfer = %d, want 2", got)
	}
	if owned, paths := watcherTopologySnapshot(watchers["repo-00"]); owned || paths != 0 {
		t.Fatalf("removed owner state = owned:%t paths:%d", owned, paths)
	}
	if unique, duplicates, registrations := fixture.activeStats(); unique != 10 || duplicates != 0 || registrations != 10 {
		t.Fatalf("active transfer registrations = %d/%d/%d, want 10/0/10", unique, duplicates, registrations)
	}

	for prefix, watcher := range watchers {
		if prefix != "repo-00" {
			watcher.scheduleTopologyChange("owner-transfer")
		}
	}
	assertOneTopologyCallback(t, &callbacks, watchers["repo-01"].debounce)

	for _, prefix := range []string{"repo-03", "repo-02", "repo-01"} {
		removeTopologyWatcher(mw, prefix)
	}
	families, _, members = topologyFamilySnapshot(mw)
	if families != 0 || members != 0 || len(mw.topologyFamilyByRepo) != 0 {
		t.Fatalf("family registry survived last removal: families:%d members:%d reverse:%d", families, members, len(mw.topologyFamilyByRepo))
	}
	if unique, duplicates, registrations := fixture.activeStats(); unique != 0 || duplicates != 0 || registrations != 0 {
		t.Fatalf("topology paths survived last removal: %d/%d/%d", unique, duplicates, registrations)
	}
}

func TestMultiWatcherTopologyFamiliesRemainIndependent(t *testing.T) {
	first := newTopologyWatchFixture(t, 8)
	second := newTopologyWatchFixture(t, 8)
	mw := newTopologyRegistry()
	for i := 0; i < 8; i++ {
		installTopologyWatcher(mw, fmt.Sprintf("first-%02d", i), first.watcher(i))
		installTopologyWatcher(mw, fmt.Sprintf("second-%02d", i), second.watcher(i))
	}
	mw.mu.Lock()
	families := len(mw.topologyFamilies)
	owners := 0
	for _, family := range mw.topologyFamilies {
		if family.owner != "" {
			owners++
		}
	}
	mw.mu.Unlock()
	if families != 2 || owners != 2 {
		t.Fatalf("independent family state = families:%d owners:%d", families, owners)
	}
	if first.inventoryCalls.Load() != 1 || second.inventoryCalls.Load() != 1 {
		t.Fatalf("inventory calls = first:%d second:%d, want 1 each", first.inventoryCalls.Load(), second.inventoryCalls.Load())
	}
	for name, fixture := range map[string]*topologyWatchFixture{"first": first, "second": second} {
		if unique, duplicates, registrations := fixture.activeStats(); unique != 18 || duplicates != 0 || registrations != 18 {
			t.Fatalf("%s registrations = %d/%d/%d, want 18/0/18", name, unique, duplicates, registrations)
		}
	}
}

func TestMultiWatcherTopologyCanonicalizesCommonDirectoryAliases(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 2)
	alias := filepath.Join(t.TempDir(), "common-alias")
	if err := os.Symlink(fixture.commonDir, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	first := fixture.watcher(0)
	second := fixture.watcher(1)
	second.commonDir = alias
	second.worktreesDir = filepath.Join(alias, "worktrees")

	mw := newTopologyRegistry()
	installTopologyWatcher(mw, "first", first)
	installTopologyWatcher(mw, "second", second)
	families, _, members := topologyFamilySnapshot(mw)
	if families != 1 || members != 2 {
		t.Fatalf("aliased common dirs formed %d families with %d members", families, members)
	}
	if got := fixture.inventoryCalls.Load(); got != 1 {
		t.Fatalf("aliased common dirs ran %d inventories, want 1", got)
	}
}

func TestMultiWatcherTopologyStartFailureCannotClaimOwnership(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 2)
	mw := newTopologyRegistry()
	mw.mu.Lock()
	mw.installStartedGitWatcherLocked("repo-00-failed", nil)
	mw.mu.Unlock()
	installTopologyWatcher(mw, "repo-01", fixture.watcher(0))
	installTopologyWatcher(mw, "repo-02", fixture.watcher(1))

	families, owner, members := topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-01" || members != 2 {
		t.Fatalf("post-failure family = families:%d owner:%q members:%d", families, owner, members)
	}
	if _, exists := mw.gitWatchers["repo-00-failed"]; exists {
		t.Fatal("failed watcher was installed")
	}
	if got := fixture.inventoryCalls.Load(); got != 1 {
		t.Fatalf("post-failure inventory calls = %d, want 1", got)
	}
}

func TestMultiWatcherTopologyRegistryConcurrentLifecycle(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 16)
	mw := newTopologyRegistry()
	watchers := make([]*GitWatcher, 16)
	for i := range watchers {
		watchers[i] = fixture.watcher(i)
	}

	var wg sync.WaitGroup
	for i, watcher := range watchers {
		prefix := fmt.Sprintf("repo-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 20; iteration++ {
				installTopologyWatcher(mw, prefix, watcher)
				removeTopologyWatcher(mw, prefix)
			}
		}()
	}
	wg.Wait()
	if families, _, members := topologyFamilySnapshot(mw); families != 0 || members != 0 {
		t.Fatalf("concurrent lifecycle left families:%d members:%d", families, members)
	}
	if unique, duplicates, registrations := fixture.activeStats(); unique != 0 || duplicates != 0 || registrations != 0 {
		t.Fatalf("concurrent lifecycle left topology paths: %d/%d/%d", unique, duplicates, registrations)
	}

	stopped := newTopologyRegistry()
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := stopped.Stop(); err != nil {
				t.Errorf("concurrent Stop: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := stopped.Stop(); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
}

func resetTopologyWatcherForBenchmark(watcher *GitWatcher) {
	watcher.mu.Lock()
	if watcher.topologyTimer != nil {
		watcher.topologyTimer.Stop()
	}
	watcher.topologyTimer = nil
	watcher.topologyOwned = false
	watcher.topologyPaths = make(map[string]struct{})
	watcher.worktreeRoots = make(map[string]struct{})
	watcher.worktreeAdminDirs = make(map[string]struct{})
	watcher.mu.Unlock()
}

func newRealTopologyFamilyBenchmark(b *testing.B, prefixes int) (string, []string) {
	b.Helper()
	base := b.TempDir()
	repo := filepath.Join(base, "main")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		b.Fatal(err)
	}
	runTopologyBenchmarkGitCommand(b, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	runTopologyBenchmarkGitCommand(b, repo, "add", "seed.txt")
	runTopologyBenchmarkGitCommand(b, repo,
		"-c", "user.name=Gortex Benchmark", "-c", "user.email=gortex@example.invalid",
		"commit", "-qm", "seed")

	roots := []string{repo}
	for i := 1; i < prefixes; i++ {
		root := filepath.Join(base, "linked", fmt.Sprintf("worktree-%03d", i))
		runTopologyBenchmarkGitCommand(b, repo, "worktree", "add", "-q", "-b", fmt.Sprintf("benchmark-%03d", i), root, "HEAD")
		roots = append(roots, root)
	}
	return filepath.Join(repo, ".git"), roots
}

func runTopologyBenchmarkGitCommand(b *testing.B, dir string, args ...string) {
	b.Helper()
	commandArgs := append([]string{"-C", dir}, args...)
	command := exec.Command("git", commandArgs...)
	if output, err := command.CombinedOutput(); err != nil {
		b.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func BenchmarkMultiWatcherTopologyFamilyRealInventory(b *testing.B) {
	for _, prefixes := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("prefixes_%d", prefixes), func(b *testing.B) {
			commonDir, roots := newRealTopologyFamilyBenchmark(b, prefixes)
			var inventoryCalls atomic.Int64
			watchers := make([]*GitWatcher, prefixes)
			for i := range watchers {
				watchers[i] = &GitWatcher{
					repoPath:          roots[i],
					logger:            zap.NewNop(),
					commonDir:         commonDir,
					worktreesDir:      filepath.Join(commonDir, "worktrees"),
					topologyPaths:     make(map[string]struct{}),
					worktreeRoots:     make(map[string]struct{}),
					worktreeAdminDirs: make(map[string]struct{}),
					inventory: func(ctx context.Context, dir string) (*gitstate.FamilyInventory, error) {
						inventoryCalls.Add(1)
						return gitstate.Inventory(ctx, dir)
					},
					topologyAdd:    func(string) error { return nil },
					topologyRemove: func(string) error { return nil },
				}
			}

			inventoryCalls.Store(0)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				mw := newTopologyRegistry()
				for i, watcher := range watchers {
					resetTopologyWatcherForBenchmark(watcher)
					installTopologyWatcher(mw, fmt.Sprintf("repo-%03d", i), watcher)
				}
			}
			b.StopTimer()

			inventories := inventoryCalls.Load()
			if inventories != int64(b.N) {
				b.Fatalf("inventory calls = %d, want %d", inventories, b.N)
			}
			owned, paths := watcherTopologySnapshot(watchers[0])
			if !owned {
				b.Fatal("first successfully installed watcher was not owner")
			}
			expectedPaths := 2
			if prefixes > 1 {
				expectedPaths = 2*prefixes + 1
			}
			if paths != expectedPaths {
				b.Fatalf("owner paths = %d, want %d", paths, expectedPaths)
			}
			for _, follower := range watchers[1:] {
				if owned, paths := watcherTopologySnapshot(follower); owned || paths != 0 {
					b.Fatalf("follower state = owned:%t paths:%d", owned, paths)
				}
			}
			b.ReportMetric(float64(inventories)/float64(b.N), "inventory/op")
			b.ReportMetric(float64(paths), "topology-paths/op")
			b.ReportMetric(0, "duplicate-paths/op")
		})
	}
}

func BenchmarkMultiWatcherTopologyFamilyRegistration(b *testing.B) {
	for _, prefixes := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("prefixes_%d", prefixes), func(b *testing.B) {
			fixture := newTopologyWatchFixture(b, prefixes)
			watchers := make([]*GitWatcher, prefixes)
			for i := range watchers {
				watchers[i] = fixture.watcher(i)
			}
			fixture.inventoryCalls.Store(0)
			fixture.registrationCalls.Store(0)

			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				fixture.resetActive()
				mw := newTopologyRegistry()
				for i, watcher := range watchers {
					resetTopologyWatcherForBenchmark(watcher)
					installTopologyWatcher(mw, fmt.Sprintf("repo-%03d", i), watcher)
				}
			}
			b.StopTimer()

			inventories := fixture.inventoryCalls.Load()
			registrations := fixture.registrationCalls.Load()
			expectedPaths := 2*prefixes + 2
			unique, duplicates, active := fixture.activeStats()
			if inventories != int64(b.N) {
				b.Fatalf("inventory calls = %d, want %d", inventories, b.N)
			}
			if unique != expectedPaths || duplicates != 0 || active != expectedPaths {
				b.Fatalf("final registrations = %d/%d/%d, want %d/0/%d", unique, duplicates, active, expectedPaths, expectedPaths)
			}
			b.ReportMetric(float64(inventories)/float64(b.N), "inventory/op")
			b.ReportMetric(float64(registrations)/float64(b.N), "topology-paths/op")
			b.ReportMetric(0, "duplicate-paths/op")
		})
	}
}
