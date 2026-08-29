package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sgtdi/fswatcher"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const (
	checkoutSourceSignalQuietWindow  = 100 * time.Millisecond
	checkoutSourceSignalMaxDelay     = time.Second
	checkoutSourceSignalReadyTimeout = 5 * time.Second
	checkoutSourceSignalRetryInitial = 100 * time.Millisecond
	checkoutSourceSignalRetryMax     = 30 * time.Second
)

var (
	errCheckoutSourceSignalWatchTimeout = errors.New("checkout source signal watcher readiness timed out")
	errCheckoutSourceSignalWatchExited  = errors.New("checkout source signal watcher exited")
	errCheckoutSourceSignalSetStopped   = errors.New("checkout source signal watcher set stopped")
)

// checkoutSourceSignalIdentity is the complete admission identity for one
// automatic checkout source watcher. requestedRoot preserves catalog spelling;
// canonicalRoot is the physical identity used by the backend and admission
// guard. The epoch on the live registration adds process-local ABA protection.
type checkoutSourceSignalIdentity struct {
	checkoutID     string
	incarnation    string
	familyID       string
	requestedRoot  string
	canonicalRoot  string
	primaryGraphID string
	coordinator    *CheckoutCoordinator
}

type checkoutSourceSignalBackend interface {
	Watch(context.Context) error
	Events() <-chan fswatcher.WatchEvent
	Dropped() <-chan fswatcher.WatchEvent
	Close()
}

type checkoutSourceSignalFactory func(
	root string,
	ready chan struct{},
	events, dropped chan fswatcher.WatchEvent,
) (checkoutSourceSignalBackend, error)

type checkoutSourceSignalRegistration struct {
	identity checkoutSourceSignalIdentity
	epoch    uint64
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

func (r *checkoutSourceSignalRegistration) stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(r.cancel)
}

type checkoutSourceSignalWatcherSet struct {
	mu         sync.Mutex
	watchers   map[string]*checkoutSourceSignalRegistration
	operations map[string]chan struct{}
	nextEpoch  uint64
	stopped    bool
	stopDone   chan struct{}

	factory checkoutSourceSignalFactory
	guard   func(checkoutSourceSignalIdentity) bool
	signal  func(checkoutID, reason string) bool
	logger  *zap.Logger

	quietWindow  time.Duration
	maxDelay     time.Duration
	readyTimeout time.Duration
	retryInitial time.Duration
	retryMax     time.Duration
}

func newCheckoutSourceSignalWatcherSet(
	factory checkoutSourceSignalFactory,
	guard func(checkoutSourceSignalIdentity) bool,
	signal func(checkoutID, reason string) bool,
	logger *zap.Logger,
) *checkoutSourceSignalWatcherSet {
	if factory == nil {
		factory = newCheckoutSourceSignalBackend
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &checkoutSourceSignalWatcherSet{
		watchers:     make(map[string]*checkoutSourceSignalRegistration),
		operations:   make(map[string]chan struct{}),
		factory:      factory,
		guard:        guard,
		signal:       signal,
		logger:       logger,
		quietWindow:  checkoutSourceSignalQuietWindow,
		maxDelay:     checkoutSourceSignalMaxDelay,
		readyTimeout: checkoutSourceSignalReadyTimeout,
		retryInitial: checkoutSourceSignalRetryInitial,
		retryMax:     checkoutSourceSignalRetryMax,
	}
}

func newCheckoutSourceSignalBackend(
	root string,
	ready chan struct{},
	events, dropped chan fswatcher.WatchEvent,
) (checkoutSourceSignalBackend, error) {
	return fswatcher.New(
		fswatcher.WithCustomChannels(events, dropped),
		fswatcher.WithCooldown(0),
		fswatcher.WithReadyChannel(ready),
		fswatcher.WithSeverity(fswatcher.SeverityError),
		fswatcher.WithPath(root, fswatcher.WithDepth(fswatcher.WatchNested)),
	)
}

func canonicalCheckoutSourceSignalRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve checkout source watcher root: %w", err)
	}
	absolute = filepath.Clean(absolute)
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve checkout source watcher root %s: %w", absolute, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat checkout source watcher root %s: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("checkout source watcher root %s is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func checkoutSourceSignalIdentityWithCurrentCanonical(
	candidate checkoutSourceSignalIdentity,
	current checkoutSourceSignalIdentity,
) checkoutSourceSignalIdentity {
	candidate.canonicalRoot = current.canonicalRoot
	return candidate
}

func checkoutSourceSignalIdentityWithCurrentRequested(
	candidate checkoutSourceSignalIdentity,
	current checkoutSourceSignalIdentity,
) checkoutSourceSignalIdentity {
	candidate.requestedRoot = current.requestedRoot
	return candidate
}

func (s *checkoutSourceSignalWatcherSet) beginOperation(checkoutID string) (func(), error) {
	for {
		s.mu.Lock()
		if s.stopped {
			done := s.stopDone
			s.mu.Unlock()
			if done != nil {
				<-done
			}
			return nil, errCheckoutSourceSignalSetStopped
		}
		if running := s.operations[checkoutID]; running != nil {
			s.mu.Unlock()
			<-running
			continue
		}
		done := make(chan struct{})
		s.operations[checkoutID] = done
		s.mu.Unlock()
		return func() {
			s.mu.Lock()
			if s.operations[checkoutID] == done {
				delete(s.operations, checkoutID)
				close(done)
			}
			s.mu.Unlock()
		}, nil
	}
}

func (s *checkoutSourceSignalWatcherSet) Ensure(identity checkoutSourceSignalIdentity) error {
	if s == nil {
		return errors.New("checkout source signal watcher set is nil")
	}
	if identity.checkoutID == "" {
		return errors.New("checkout source signal watcher needs a checkout ID")
	}
	if identity.requestedRoot == "" {
		return errors.New("checkout source signal watcher needs a root")
	}

	// This is the hot reconciliation path: preserve a lock-only, allocation-free
	// idempotent return when the logical catalog identity is unchanged.
	s.mu.Lock()
	current := s.watchers[identity.checkoutID]
	if current != nil && current.identity == checkoutSourceSignalIdentityWithCurrentCanonical(identity, current.identity) {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	finish, err := s.beginOperation(identity.checkoutID)
	if err != nil {
		return err
	}
	defer finish()

	s.mu.Lock()
	current = s.watchers[identity.checkoutID]
	if current != nil && current.identity == checkoutSourceSignalIdentityWithCurrentCanonical(identity, current.identity) {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	canonicalRoot, err := canonicalCheckoutSourceSignalRoot(identity.requestedRoot)
	if err != nil {
		return err
	}
	identity.canonicalRoot = canonicalRoot

	ctx, cancel := context.WithCancel(context.Background())
	registration := &checkoutSourceSignalRegistration{
		identity: identity,
		cancel:   cancel,
		done:     make(chan struct{}),
	}

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		cancel()
		return errCheckoutSourceSignalSetStopped
	}
	current = s.watchers[identity.checkoutID]
	if current != nil && current.identity == checkoutSourceSignalIdentityWithCurrentRequested(identity, current.identity) {
		s.mu.Unlock()
		cancel()
		return nil
	}
	s.nextEpoch++
	registration.epoch = s.nextEpoch
	s.watchers[identity.checkoutID] = registration
	go s.run(ctx, registration, nil, nil, nil)
	s.mu.Unlock()

	if current != nil {
		current.stop()
		<-current.done
	}
	return nil
}

func (s *checkoutSourceSignalWatcherSet) run(
	ctx context.Context,
	registration *checkoutSourceSignalRegistration,
	_ <-chan struct{},
	_, _ <-chan fswatcher.WatchEvent,
) {
	defer func() {
		s.removeIfCurrent(registration)
		close(registration.done)
	}()

	retryDelay := positiveCheckoutSourceSignalDuration(s.retryInitial, checkoutSourceSignalRetryInitial)
	for {
		if ctx.Err() != nil || !s.registrationCurrent(registration) {
			return
		}
		if s.guard != nil && !s.guard(registration.identity) {
			return
		}

		readyFor, err := s.runAttempt(ctx, registration)
		if ctx.Err() != nil || !s.registrationCurrent(registration) {
			return
		}
		if err != nil {
			s.logger.Warn("automatic checkout source watcher will retry",
				zap.String("checkout_id", registration.identity.checkoutID),
				zap.String("root", registration.identity.canonicalRoot),
				zap.Duration("retry_after", retryDelay),
				zap.Error(err))
		}
		if readyFor >= positiveCheckoutSourceSignalDuration(s.maxDelay, checkoutSourceSignalMaxDelay) {
			retryDelay = positiveCheckoutSourceSignalDuration(s.retryInitial, checkoutSourceSignalRetryInitial)
		}
		if !waitCheckoutSourceSignalRetry(ctx, retryDelay) {
			return
		}
		retryDelay = nextCheckoutSourceSignalRetry(
			retryDelay,
			positiveCheckoutSourceSignalDuration(s.retryMax, checkoutSourceSignalRetryMax),
		)
	}
}

func (s *checkoutSourceSignalWatcherSet) runAttempt(
	ctx context.Context,
	registration *checkoutSourceSignalRegistration,
) (time.Duration, error) {
	ready := make(chan struct{})
	events := make(chan fswatcher.WatchEvent, 256)
	dropped := make(chan fswatcher.WatchEvent, 16)
	backend, err := s.factory(registration.identity.canonicalRoot, ready, events, dropped)
	if err != nil {
		return 0, fmt.Errorf("create checkout source signal watcher: %w", err)
	}

	attemptCtx, cancel := context.WithCancel(ctx)
	watchErr := make(chan error, 1)
	go func() { watchErr <- backend.Watch(attemptCtx) }()
	watchFinished := false
	defer func() {
		cancel()
		backend.Close()
		if !watchFinished {
			<-watchErr
		}
	}()

	readyTimeout := positiveCheckoutSourceSignalDuration(s.readyTimeout, checkoutSourceSignalReadyTimeout)
	readyTimer := time.NewTimer(readyTimeout)
	defer stopCheckoutSourceSignalTimer(readyTimer)

	select {
	case <-ready:
		stopCheckoutSourceSignalTimer(readyTimer)
		s.signalIfCurrent(registration, "source-watch-ready")
	case err := <-watchErr:
		watchFinished = true
		if err == nil {
			err = errCheckoutSourceSignalWatchExited
		}
		return 0, fmt.Errorf("watcher stopped before ready: %w", err)
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-readyTimer.C:
		return 0, errCheckoutSourceSignalWatchTimeout
	}
	readyAt := time.Now()

	quietWindow := s.quietWindow
	if quietWindow < 0 {
		quietWindow = 0
	}
	maxDelay := positiveCheckoutSourceSignalDuration(s.maxDelay, checkoutSourceSignalMaxDelay)
	var quietTimer, maxTimer *time.Timer
	var quiet, maximum <-chan time.Time
	pendingReason := ""
	maxSignaled := false
	defer func() {
		stopCheckoutSourceSignalTimer(quietTimer)
		stopCheckoutSourceSignalTimer(maxTimer)
	}()

	schedule := func(reason string) {
		if reason == "source-events-dropped" || pendingReason == "" {
			pendingReason = reason
		}
		if quietTimer == nil {
			quietTimer = time.NewTimer(quietWindow)
		} else {
			resetCheckoutSourceSignalTimer(quietTimer, quietWindow)
		}
		quiet = quietTimer.C
		if maxTimer == nil && !maxSignaled {
			maxTimer = time.NewTimer(maxDelay)
			maximum = maxTimer.C
		}
	}
	finishBurst := func() {
		pendingReason = ""
		maxSignaled = false
		quiet = nil
		maximum = nil
		stopCheckoutSourceSignalTimer(maxTimer)
		maxTimer = nil
	}

	for {
		select {
		case _, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			schedule("source-event")
		case _, ok := <-dropped:
			if !ok {
				dropped = nil
				continue
			}
			schedule("source-events-dropped")
		case <-maximum:
			maximum = nil
			maxSignaled = true
			s.signalIfCurrent(registration, "source-event-max-delay")
		case <-quiet:
			if len(events) != 0 || len(dropped) != 0 {
				resetCheckoutSourceSignalTimer(quietTimer, quietWindow)
				quiet = quietTimer.C
				continue
			}
			reason := pendingReason
			if maxSignaled {
				reason = "source-event-final"
			}
			finishBurst()
			s.signalIfCurrent(registration, reason)
		case err := <-watchErr:
			watchFinished = true
			if err == nil {
				err = errCheckoutSourceSignalWatchExited
			}
			return time.Since(readyAt), fmt.Errorf("watcher stopped after ready: %w", err)
		case <-ctx.Done():
			return time.Since(readyAt), ctx.Err()
		}
	}
}

func positiveCheckoutSourceSignalDuration(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func nextCheckoutSourceSignalRetry(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}

func waitCheckoutSourceSignalRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer stopCheckoutSourceSignalTimer(timer)
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func stopCheckoutSourceSignalTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func resetCheckoutSourceSignalTimer(timer *time.Timer, delay time.Duration) {
	stopCheckoutSourceSignalTimer(timer)
	timer.Reset(delay)
}

func (s *checkoutSourceSignalWatcherSet) registrationCurrent(
	registration *checkoutSourceSignalRegistration,
) bool {
	if s == nil || registration == nil {
		return false
	}
	s.mu.Lock()
	current := s.watchers[registration.identity.checkoutID]
	admitted := !s.stopped && current == registration && current.epoch == registration.epoch
	s.mu.Unlock()
	return admitted
}

func (s *checkoutSourceSignalWatcherSet) signalIfCurrent(
	registration *checkoutSourceSignalRegistration, reason string,
) bool {
	if reason == "" || !s.registrationCurrent(registration) {
		return false
	}
	if s.guard != nil && !s.guard(registration.identity) {
		return false
	}
	if !s.registrationCurrent(registration) || s.signal == nil {
		return false
	}
	return s.signal(registration.identity.checkoutID, reason)
}

func (s *checkoutSourceSignalWatcherSet) removeIfCurrent(
	registration *checkoutSourceSignalRegistration,
) {
	if s == nil || registration == nil {
		return
	}
	s.mu.Lock()
	if current := s.watchers[registration.identity.checkoutID]; current == registration && current.epoch == registration.epoch {
		delete(s.watchers, registration.identity.checkoutID)
	}
	s.mu.Unlock()
}

func (s *checkoutSourceSignalWatcherSet) Drop(checkoutID string) {
	if s == nil || checkoutID == "" {
		return
	}
	finish, err := s.beginOperation(checkoutID)
	if err != nil {
		return
	}
	defer finish()

	s.mu.Lock()
	registration := s.watchers[checkoutID]
	if registration != nil {
		delete(s.watchers, checkoutID)
	}
	s.mu.Unlock()
	if registration != nil {
		registration.stop()
		<-registration.done
	}
}

func (s *checkoutSourceSignalWatcherSet) StopAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		done := s.stopDone
		s.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	s.stopped = true
	s.stopDone = make(chan struct{})
	stopDone := s.stopDone
	registrations := make([]*checkoutSourceSignalRegistration, 0, len(s.watchers))
	for checkoutID, registration := range s.watchers {
		delete(s.watchers, checkoutID)
		registrations = append(registrations, registration)
	}
	operations := make([]chan struct{}, 0, len(s.operations))
	for _, operation := range s.operations {
		operations = append(operations, operation)
	}
	s.mu.Unlock()

	for _, registration := range registrations {
		registration.stop()
	}
	for _, registration := range registrations {
		<-registration.done
	}
	for _, operation := range operations {
		<-operation
	}
	close(stopDone)
}

func (s *checkoutSourceSignalWatcherSet) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watchers)
}

func (l *CheckoutLifecycle) beginCheckoutSourceSignalClose() {
	for {
		l.checkoutSignalWatchMu.Lock()
		if !l.checkoutSignalWatchClosing {
			l.checkoutSignalWatchClosing = true
			l.checkoutSignalWatchCloseDone = make(chan struct{})
			l.checkoutSignalWatchMu.Unlock()
			return
		}
		done := l.checkoutSignalWatchCloseDone
		l.checkoutSignalWatchMu.Unlock()
		if done != nil {
			<-done
		}
	}
}

func (l *CheckoutLifecycle) finishCheckoutSourceSignalClose() {
	l.checkoutSignalWatchMu.Lock()
	done := l.checkoutSignalWatchCloseDone
	l.checkoutSignalWatchClosing = false
	l.checkoutSignalWatchCloseDone = nil
	if done != nil {
		close(done)
	}
	l.checkoutSignalWatchMu.Unlock()
}

func (l *CheckoutLifecycle) admitCheckoutSourceSignalEnsure() bool {
	l.checkoutSignalWatchMu.Lock()
	if l.checkoutSignalWatchClosing {
		l.checkoutSignalWatchMu.Unlock()
		return false
	}
	l.retryMu.Lock()
	if l.retryClosing {
		l.retryMu.Unlock()
		l.checkoutSignalWatchMu.Unlock()
		return false
	}
	l.retryWG.Add(1)
	l.retryMu.Unlock()
	l.checkoutSignalWatchMu.Unlock()
	return true
}

func (l *CheckoutLifecycle) sourceSignalWatcherSet() *checkoutSourceSignalWatcherSet {
	l.checkoutSignalWatchMu.Lock()
	defer l.checkoutSignalWatchMu.Unlock()
	if l.checkoutSignalWatchClosing {
		return nil
	}
	if l.checkoutSignalWatchers == nil {
		l.checkoutSignalWatchers = newCheckoutSourceSignalWatcherSet(
			nil,
			l.checkoutSourceSignalCurrent,
			l.SignalCheckout,
			l.logger,
		)
	}
	return l.checkoutSignalWatchers
}

func (l *CheckoutLifecycle) ensureCheckoutSourceSignalWatcher(
	checkout store_sqlite.Checkout, primaryGraphID string,
) {
	if l == nil || !l.admitCheckoutSourceSignalEnsure() {
		return
	}
	defer l.retryWG.Done()

	if checkout.State != store_sqlite.CheckoutStateReady ||
		checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic ||
		checkout.CheckoutID == "" || checkout.RootPath == "" || primaryGraphID == "" {
		l.dropCheckoutSourceSignalWatcher(checkout.CheckoutID)
		return
	}
	l.coordMu.Lock()
	coordinator := l.coordinators[checkout.CheckoutID]
	l.coordMu.Unlock()
	if coordinator == nil || coordinator.graphID != primaryGraphID {
		l.dropCheckoutSourceSignalWatcher(checkout.CheckoutID)
		return
	}
	identity := checkoutSourceSignalIdentity{
		checkoutID:     checkout.CheckoutID,
		incarnation:    fmt.Sprint(checkout.Incarnation),
		familyID:       checkout.FamilyID,
		requestedRoot:  checkout.RootPath,
		primaryGraphID: primaryGraphID,
		coordinator:    coordinator,
	}
	watchers := l.sourceSignalWatcherSet()
	if watchers == nil {
		return
	}
	l.coordMu.Lock()
	current := l.coordinators[checkout.CheckoutID]
	l.coordMu.Unlock()
	if current != coordinator || current.graphID != primaryGraphID {
		return
	}
	if err := watchers.Ensure(identity); err != nil && !errors.Is(err, errCheckoutSourceSignalSetStopped) {
		l.logger.Warn("automatic checkout source watcher unavailable; retry supervisor remains active",
			zap.String("checkout_id", checkout.CheckoutID),
			zap.String("root", checkout.RootPath),
			zap.Error(err))
	}
}

func (l *CheckoutLifecycle) checkoutSourceSignalCurrent(identity checkoutSourceSignalIdentity) bool {
	if l == nil || l.catalog == nil || identity.canonicalRoot == "" || identity.primaryGraphID == "" {
		return false
	}
	ctx := context.Background()
	checkout, found, err := l.catalog.GetCheckout(ctx, identity.checkoutID)
	if err != nil || !found {
		if err != nil && l.logger != nil {
			l.logger.Debug("rejecting automatic checkout source watcher signal",
				zap.String("checkout_id", identity.checkoutID), zap.Error(err))
		}
		return false
	}
	checkoutRoot, err := canonicalCheckoutSourceSignalRoot(checkout.RootPath)
	if err != nil || checkout.CheckoutID != identity.checkoutID ||
		fmt.Sprint(checkout.Incarnation) != identity.incarnation ||
		checkout.FamilyID != identity.familyID ||
		checkout.State != store_sqlite.CheckoutStateReady ||
		checkout.EffectiveMode != store_sqlite.CheckoutModeAutomatic ||
		checkoutRoot != identity.canonicalRoot {
		return false
	}
	graph, graphFound, graphErr := l.catalog.GetDedicatedGraph(ctx, identity.primaryGraphID)
	if graphErr != nil || !graphFound || graph.GraphID != identity.primaryGraphID ||
		graph.FamilyID != identity.familyID || !graph.IsPrimaryBase {
		return false
	}

	l.coordMu.Lock()
	coordinator := l.coordinators[identity.checkoutID]
	l.coordMu.Unlock()
	if coordinator == nil || coordinator != identity.coordinator || coordinator.graphID != identity.primaryGraphID {
		return false
	}
	coordinatorRoot, err := canonicalCheckoutSourceSignalRoot(coordinator.root)
	return err == nil && coordinatorRoot == identity.canonicalRoot
}

func (l *CheckoutLifecycle) dropCheckoutSourceSignalWatcher(checkoutID string) {
	if l == nil || checkoutID == "" {
		return
	}
	l.checkoutSignalWatchMu.Lock()
	watchers := l.checkoutSignalWatchers
	l.checkoutSignalWatchMu.Unlock()
	if watchers != nil {
		watchers.Drop(checkoutID)
	}
}

func (l *CheckoutLifecycle) stopCheckoutSourceSignalWatchers() {
	if l == nil {
		return
	}
	owner := false
	l.checkoutSignalWatchMu.Lock()
	if !l.checkoutSignalWatchClosing {
		owner = true
		l.checkoutSignalWatchClosing = true
		l.checkoutSignalWatchCloseDone = make(chan struct{})
	}
	watchers := l.checkoutSignalWatchers
	l.checkoutSignalWatchers = nil
	l.checkoutSignalWatchMu.Unlock()
	if watchers != nil {
		watchers.StopAll()
	}
	if owner {
		l.finishCheckoutSourceSignalClose()
	}
}
