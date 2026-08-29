package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sgtdi/fswatcher"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type checkoutSourceSignalRecord struct {
	checkoutID string
	reason     string
	at         time.Time
}

type fakeCheckoutSourceSignalBackend struct {
	ready   chan struct{}
	events  chan fswatcher.WatchEvent
	dropped chan fswatcher.WatchEvent
	started chan struct{}
	stopped chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
}

func (b *fakeCheckoutSourceSignalBackend) Watch(ctx context.Context) error {
	b.startOnce.Do(func() {
		close(b.started)
		close(b.ready)
	})
	<-ctx.Done()
	b.stopOnce.Do(func() { close(b.stopped) })
	return nil
}

func (b *fakeCheckoutSourceSignalBackend) Events() <-chan fswatcher.WatchEvent {
	return b.events
}

func (b *fakeCheckoutSourceSignalBackend) Dropped() <-chan fswatcher.WatchEvent {
	return b.dropped
}

func (b *fakeCheckoutSourceSignalBackend) Close() {}

type fakeCheckoutSourceSignalFactory struct {
	mu       sync.Mutex
	backends []*fakeCheckoutSourceSignalBackend
}

func (f *fakeCheckoutSourceSignalFactory) New(
	_ string,
	ready chan struct{},
	events, dropped chan fswatcher.WatchEvent,
) (checkoutSourceSignalBackend, error) {
	backend := &fakeCheckoutSourceSignalBackend{
		ready:   ready,
		events:  events,
		dropped: dropped,
		started: make(chan struct{}),
		stopped: make(chan struct{}),
	}
	f.mu.Lock()
	f.backends = append(f.backends, backend)
	f.mu.Unlock()
	return backend, nil
}

func (f *fakeCheckoutSourceSignalFactory) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.backends)
}

func (f *fakeCheckoutSourceSignalFactory) Backend(index int) *fakeCheckoutSourceSignalBackend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backends[index]
}

func checkoutSourceSignalTestIdentity(root string, ordinal int) checkoutSourceSignalIdentity {
	return checkoutSourceSignalIdentity{
		checkoutID:     fmt.Sprintf("checkout-%d", ordinal),
		incarnation:    fmt.Sprintf("incarnation-%d", ordinal),
		familyID:       "family",
		requestedRoot:  root,
		primaryGraphID: "primary",
	}
}

func checkoutSourceSignalRecorder(buffer int) (func(string, string) bool, <-chan checkoutSourceSignalRecord) {
	records := make(chan checkoutSourceSignalRecord, buffer)
	return func(checkoutID, reason string) bool {
		select {
		case records <- checkoutSourceSignalRecord{checkoutID: checkoutID, reason: reason, at: time.Now()}:
			return true
		default:
			return false
		}
	}, records
}

func waitCheckoutSourceSignal(
	t testing.TB, records <-chan checkoutSourceSignalRecord, reason string, timeout time.Duration,
) checkoutSourceSignalRecord {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case record := <-records:
			if record.reason == reason {
				return record
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for checkout source signal %q", reason)
		}
	}
}

func TestCheckoutSourceSignalWatcherObservesUntrackedFilePromptly(t *testing.T) {
	root := t.TempDir()
	signal, records := checkoutSourceSignalRecorder(32)
	watchers := newCheckoutSourceSignalWatcherSet(nil, func(checkoutSourceSignalIdentity) bool {
		return true
	}, signal, zap.NewNop())
	t.Cleanup(watchers.StopAll)

	if err := watchers.Ensure(checkoutSourceSignalTestIdentity(root, 1)); err != nil {
		t.Fatalf("ensure source watcher: %v", err)
	}
	waitCheckoutSourceSignal(t, records, "source-watch-ready", 6*time.Second)

	started := time.Now()
	if err := os.WriteFile(filepath.Join(root, "new_untracked.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}
	record := waitCheckoutSourceSignal(t, records, "source-event", 2*time.Second)
	if latency := record.at.Sub(started); latency >= 2*time.Second {
		t.Fatalf("source event signal latency = %s, want < 2s", latency)
	}
}

func TestCheckoutSourceSignalWatcherEnsureIsIdempotentAndCoalesces(t *testing.T) {
	root := t.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(32)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool {
		return true
	}, signal, zap.NewNop())
	watchers.quietWindow = 20 * time.Millisecond
	t.Cleanup(watchers.StopAll)
	identity := checkoutSourceSignalTestIdentity(root, 1)

	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure source watcher: %v", err)
	}
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	for i := 0; i < 64; i++ {
		if err := watchers.Ensure(identity); err != nil {
			t.Fatalf("idempotent ensure %d: %v", i, err)
		}
	}
	if got := factory.Count(); got != 1 {
		t.Fatalf("backend creations = %d, want 1", got)
	}
	if got := watchers.Len(); got != 1 {
		t.Fatalf("live source watchers = %d, want 1", got)
	}

	backend := factory.Backend(0)
	for i := 0; i < 64; i++ {
		backend.events <- fswatcher.WatchEvent{Path: filepath.Join(root, fmt.Sprintf("file-%d.go", i))}
	}
	waitCheckoutSourceSignal(t, records, "source-event", time.Second)
	select {
	case duplicate := <-records:
		t.Fatalf("event burst produced duplicate signal: %+v", duplicate)
	case <-time.After(4 * watchers.quietWindow):
	}

	backend.dropped <- fswatcher.WatchEvent{Path: root}
	waitCheckoutSourceSignal(t, records, "source-events-dropped", time.Second)
}

func TestCheckoutSourceSignalWatcherRejectsStaleABARegistration(t *testing.T) {
	root := t.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(16)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool {
		return true
	}, signal, zap.NewNop())
	t.Cleanup(watchers.StopAll)
	identity := checkoutSourceSignalTestIdentity(root, 1)

	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure first registration: %v", err)
	}
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	watchers.mu.Lock()
	first := watchers.watchers[identity.checkoutID]
	watchers.mu.Unlock()
	watchers.Drop(identity.checkoutID)
	select {
	case <-factory.Backend(0).stopped:
	default:
		t.Fatal("Drop returned before the first backend stopped")
	}

	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure replacement registration: %v", err)
	}
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	watchers.mu.Lock()
	second := watchers.watchers[identity.checkoutID]
	watchers.mu.Unlock()
	if second == nil || second == first || second.epoch <= first.epoch {
		t.Fatalf("replacement registration = %#v, first epoch = %d", second, first.epoch)
	}
	if watchers.signalIfCurrent(first, "stale-aba-event") {
		t.Fatal("stale registration inherited the replacement admission")
	}
	select {
	case record := <-records:
		t.Fatalf("stale registration emitted signal: %+v", record)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestCheckoutSourceSignalWatcherRootReplacementJoinsOldBackend(t *testing.T) {
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(16)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool {
		return true
	}, signal, zap.NewNop())
	t.Cleanup(watchers.StopAll)

	identity := checkoutSourceSignalTestIdentity(firstRoot, 1)
	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure first root: %v", err)
	}
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	identity.requestedRoot = secondRoot
	if err := watchers.Ensure(identity); err != nil {
		t.Fatalf("ensure replacement root: %v", err)
	}
	select {
	case <-factory.Backend(0).stopped:
	default:
		t.Fatal("root replacement returned before old backend stopped")
	}
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	if got := factory.Count(); got != 2 {
		t.Fatalf("backend creations = %d, want 2", got)
	}
	if got := watchers.Len(); got != 1 {
		t.Fatalf("live source watchers = %d, want 1", got)
	}
}

func TestCheckoutSourceSignalWatcherStopAllCancelsAndJoins(t *testing.T) {
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(32)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool {
		return true
	}, signal, zap.NewNop())

	for i := 0; i < 8; i++ {
		root := filepath.Join(t.TempDir(), fmt.Sprintf("checkout-%d", i))
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("create root %d: %v", i, err)
		}
		if err := watchers.Ensure(checkoutSourceSignalTestIdentity(root, i)); err != nil {
			t.Fatalf("ensure root %d: %v", i, err)
		}
	}
	for i := 0; i < 8; i++ {
		waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	}
	watchers.StopAll()
	if got := watchers.Len(); got != 0 {
		t.Fatalf("live source watchers after StopAll = %d, want 0", got)
	}
	for i := 0; i < factory.Count(); i++ {
		select {
		case <-factory.Backend(i).stopped:
		default:
			t.Fatalf("StopAll returned before backend %d stopped", i)
		}
	}
}

func TestCheckoutLifecycleSourceSignalWatcherAttachDropAndReusableClose(t *testing.T) {
	root := t.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	signal, records := checkoutSourceSignalRecorder(16)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool {
		return true
	}, signal, zap.NewNop())
	coordinator := &CheckoutCoordinator{root: root, graphID: "primary"}
	lifecycle := &CheckoutLifecycle{
		logger:                 zap.NewNop(),
		coordinators:           map[string]*CheckoutCoordinator{"automatic-checkout": coordinator},
		checkoutSignalWatchers: watchers,
	}
	checkout := store_sqlite.Checkout{
		CheckoutID:    "automatic-checkout",
		FamilyID:      "family",
		RootPath:      root,
		State:         store_sqlite.CheckoutStateReady,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
	}

	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	if got := factory.Count(); got != 1 {
		t.Fatalf("lifecycle idempotent backend creations = %d, want 1", got)
	}

	lifecycle.dropCheckoutSourceSignalWatcher(checkout.CheckoutID)
	if got := watchers.Len(); got != 0 {
		t.Fatalf("live source watchers after lifecycle drop = %d, want 0", got)
	}
	select {
	case <-factory.Backend(0).stopped:
	default:
		t.Fatal("lifecycle drop returned before backend stopped")
	}

	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	waitCheckoutSourceSignal(t, records, "source-watch-ready", time.Second)
	watchers.mu.Lock()
	reappeared := watchers.watchers[checkout.CheckoutID]
	watchers.mu.Unlock()
	if reappeared == nil || reappeared.epoch <= 1 {
		t.Fatalf("reappeared registration epoch = %#v, want a fresh epoch", reappeared)
	}

	lifecycle.stopCheckoutSourceSignalWatchers()
	if got := watchers.Len(); got != 0 {
		t.Fatalf("live source watchers after lifecycle close = %d, want 0", got)
	}
	lifecycle.checkoutSignalWatchMu.Lock()
	retained := lifecycle.checkoutSignalWatchers
	lifecycle.checkoutSignalWatchMu.Unlock()
	if retained != nil {
		t.Fatal("lifecycle close retained a stopped watcher set")
	}

	replacementFactory := &fakeCheckoutSourceSignalFactory{}
	replacementSignal, replacementRecords := checkoutSourceSignalRecorder(4)
	replacement := newCheckoutSourceSignalWatcherSet(replacementFactory.New, func(checkoutSourceSignalIdentity) bool {
		return true
	}, replacementSignal, zap.NewNop())
	lifecycle.checkoutSignalWatchMu.Lock()
	lifecycle.checkoutSignalWatchers = replacement
	lifecycle.checkoutSignalWatchMu.Unlock()
	lifecycle.ensureCheckoutSourceSignalWatcher(checkout, "primary")
	waitCheckoutSourceSignal(t, replacementRecords, "source-watch-ready", time.Second)
	lifecycle.stopCheckoutSourceSignalWatchers()
	if got := replacementFactory.Count(); got != 1 {
		t.Fatalf("reused lifecycle backend creations = %d, want 1", got)
	}
}

func BenchmarkCheckoutSourceSignalWatcherStableEnsure(b *testing.B) {
	for _, roots := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("roots_%d", roots), func(b *testing.B) {
			factory := &fakeCheckoutSourceSignalFactory{}
			signal, records := checkoutSourceSignalRecorder(roots * 2)
			watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool {
				return true
			}, signal, zap.NewNop())
			b.Cleanup(watchers.StopAll)
			identities := make([]checkoutSourceSignalIdentity, 0, roots)
			for i := 0; i < roots; i++ {
				root := filepath.Join(b.TempDir(), fmt.Sprintf("checkout-%d", i))
				if err := os.MkdirAll(root, 0o755); err != nil {
					b.Fatalf("create root %d: %v", i, err)
				}
				identity := checkoutSourceSignalTestIdentity(root, i)
				identities = append(identities, identity)
				if err := watchers.Ensure(identity); err != nil {
					b.Fatalf("initial ensure %d: %v", i, err)
				}
			}
			for i := 0; i < roots; i++ {
				waitCheckoutSourceSignal(b, records, "source-watch-ready", time.Second)
			}
			baselineCreations := factory.Count()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, identity := range identities {
					if err := watchers.Ensure(identity); err != nil {
						b.Fatalf("stable ensure: %v", err)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(roots), "roots/op")
			b.ReportMetric(float64(factory.Count()-baselineCreations)/float64(b.N), "backend-starts/op")
		})
	}
}

func BenchmarkCheckoutSourceSignalWatcherEventCoalescing(b *testing.B) {
	root := b.TempDir()
	factory := &fakeCheckoutSourceSignalFactory{}
	var signals atomic.Int64
	records := make(chan checkoutSourceSignalRecord, 4)
	watchers := newCheckoutSourceSignalWatcherSet(factory.New, func(checkoutSourceSignalIdentity) bool {
		return true
	}, func(checkoutID, reason string) bool {
		signals.Add(1)
		select {
		case records <- checkoutSourceSignalRecord{checkoutID: checkoutID, reason: reason, at: time.Now()}:
			return true
		default:
			return false
		}
	}, zap.NewNop())
	watchers.quietWindow = time.Millisecond
	b.Cleanup(watchers.StopAll)
	if err := watchers.Ensure(checkoutSourceSignalTestIdentity(root, 1)); err != nil {
		b.Fatalf("initial ensure: %v", err)
	}
	waitCheckoutSourceSignal(b, records, "source-watch-ready", time.Second)
	baselineSignals := signals.Load()
	backend := factory.Backend(0)

	const eventsPerBurst = 32
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for event := 0; event < eventsPerBurst; event++ {
			backend.events <- fswatcher.WatchEvent{Path: filepath.Join(root, "changed.go")}
		}
		waitCheckoutSourceSignal(b, records, "source-event", time.Second)
	}
	b.StopTimer()
	b.ReportMetric(eventsPerBurst, "events/op")
	b.ReportMetric(float64(signals.Load()-baselineSignals)/float64(b.N), "signals/op")
}
