package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultProducerWithdrawalAttemptTimeout = 2 * time.Second
	defaultProducerWithdrawalInitialBackoff = 25 * time.Millisecond
	defaultProducerWithdrawalMaxBackoff     = 5 * time.Second
	defaultProducerWithdrawalShutdownBudget = 3 * time.Second
)

type producerWithdrawalKey struct {
	generationID int64
	producer     string
}

type producerWithdrawalDisposition uint8

const (
	producerWithdrawalSatisfied producerWithdrawalDisposition = iota + 1
	producerWithdrawalTransient
	producerWithdrawalPersistent
)

func (d producerWithdrawalDisposition) valid() bool {
	return d >= producerWithdrawalSatisfied && d <= producerWithdrawalPersistent
}

func (d producerWithdrawalDisposition) String() string {
	switch d {
	case producerWithdrawalSatisfied:
		return "satisfied"
	case producerWithdrawalTransient:
		return "transient"
	case producerWithdrawalPersistent:
		return "persistent"
	default:
		return "unknown"
	}
}

// producerWithdrawalFunc and producerWithdrawalClassifyFunc run synchronously
// on the manager's sole worker. Neither callback may call Close on its manager.
// Both must honor ctx promptly; classification (including a satisfied-state
// readback) must not block independently of ctx. Close cancels an active normal
// attempt, then joins the worker while one shared shutdown deadline remains in
// force.
type producerWithdrawalFunc func(context.Context, int64, string, string) error

type producerWithdrawalClassifyFunc func(
	context.Context,
	int64,
	string,
	error,
) (producerWithdrawalDisposition, error)

// producerWithdrawalObserver must be non-blocking and must never call Close on
// its manager. It runs synchronously on the sole worker after manager locks are
// released; Close re-entry would wait for that same worker and deadlock.
type producerWithdrawalObserver func(producerWithdrawalEvent)

type producerWithdrawalAttemptPhase uint8

const (
	producerWithdrawalNormalAttempt producerWithdrawalAttemptPhase = iota + 1
	producerWithdrawalShutdownAttempt
)

type producerWithdrawalEvent struct {
	Key         producerWithdrawalKey
	Reason      string
	Attempt     int
	Err         error
	Disposition producerWithdrawalDisposition
	Backoff     time.Duration
	Shutdown    bool
	Final       bool
}

type producerWithdrawalConfig struct {
	attemptTimeout time.Duration
	initialBackoff time.Duration
	maxBackoff     time.Duration
	shutdownBudget time.Duration
	observe        producerWithdrawalObserver
}

func (c producerWithdrawalConfig) normalized() producerWithdrawalConfig {
	if c.attemptTimeout <= 0 {
		c.attemptTimeout = defaultProducerWithdrawalAttemptTimeout
	}
	if c.initialBackoff <= 0 {
		c.initialBackoff = defaultProducerWithdrawalInitialBackoff
	}
	if c.maxBackoff < c.initialBackoff {
		c.maxBackoff = defaultProducerWithdrawalMaxBackoff
		if c.maxBackoff < c.initialBackoff {
			c.maxBackoff = c.initialBackoff
		}
	}
	if c.shutdownBudget <= 0 {
		c.shutdownBudget = defaultProducerWithdrawalShutdownBudget
	}
	return c
}

type producerWithdrawalTask struct {
	key             producerWithdrawalKey
	reason          string
	attempt         int
	nextAttempt     time.Time
	sequence        uint64
	lastErr         error
	lastDisposition producerWithdrawalDisposition
}

// producerWithdrawalManager makes generation-producer capability withdrawal
// eventual for the lifetime of one open store. Scheduling never waits for the
// SQLite writer: equal generation/producer keys coalesce and a single worker
// serializes actual Catalog withdrawal attempts.
//
// A generation's producer set is immutable except for withdrawal. completed is
// therefore a store-lifetime tombstone set: once one attempt observes the
// invariant satisfied, later schedules for that key are accepted as no-ops and
// can never restore or re-run the producer. Duplicate pending schedules retain
// the first non-empty reason so diagnostics are stable under concurrency.
type producerWithdrawalManager struct {
	withdraw producerWithdrawalFunc
	classify producerWithdrawalClassifyFunc
	config   producerWithdrawalConfig

	mu           sync.Mutex
	tasks        map[producerWithdrawalKey]*producerWithdrawalTask
	completed    map[producerWithdrawalKey]struct{}
	nextSequence uint64
	accept       bool
	closing      bool
	deadline     time.Time
	shutdownCtx  context.Context
	shutdownEnd  context.CancelFunc
	activeID     uint64
	activeCancel context.CancelFunc
	wake         chan struct{}
	done         chan struct{}
	closeOne     sync.Once
}

func newProducerWithdrawalManager(
	withdraw producerWithdrawalFunc,
	classify producerWithdrawalClassifyFunc,
	config producerWithdrawalConfig,
) *producerWithdrawalManager {
	if withdraw == nil {
		panic("store_sqlite: nil producer withdrawal function")
	}
	m := &producerWithdrawalManager{
		withdraw:  withdraw,
		classify:  classify,
		config:    config.normalized(),
		tasks:     make(map[producerWithdrawalKey]*producerWithdrawalTask),
		completed: make(map[producerWithdrawalKey]struct{}),
		accept:    true,
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	go m.run()
	return m
}

// schedule records an eventual withdrawal without waiting for the writer.
// false means shutdown has closed admission or the key is invalid. A completed
// immutable key returns true without queueing work.
func (m *producerWithdrawalManager) schedule(generationID int64, producer, reason string) bool {
	if generationID <= 0 || producer == "" {
		return false
	}
	key := producerWithdrawalKey{generationID: generationID, producer: producer}
	now := time.Now()

	m.mu.Lock()
	if !m.accept {
		m.mu.Unlock()
		return false
	}
	if _, done := m.completed[key]; done {
		m.mu.Unlock()
		return true
	}
	if existing := m.tasks[key]; existing == nil {
		m.nextSequence++
		m.tasks[key] = &producerWithdrawalTask{
			key:         key,
			reason:      reason,
			nextAttempt: now,
			sequence:    m.nextSequence,
		}
	} else if existing.reason == "" && reason != "" {
		// Keep the first non-empty diagnostic reason; later duplicates may not
		// rewrite it and make retry telemetry depend on arrival order.
		existing.reason = reason
	}
	m.mu.Unlock()
	m.signal()
	return true
}

func (m *producerWithdrawalManager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *producerWithdrawalManager) observe(event producerWithdrawalEvent) {
	if m.config.observe != nil {
		m.config.observe(event)
	}
}

func (m *producerWithdrawalManager) run() {
	defer close(m.done)
	for {
		task, wait, final, stop := m.nextTask()
		for _, event := range final {
			m.observe(event)
		}
		if stop {
			return
		}
		if task != nil {
			m.attempt(task)
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-m.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

func (m *producerWithdrawalManager) nextTask() (
	*producerWithdrawalTask,
	time.Duration,
	[]producerWithdrawalEvent,
	bool,
) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if len(m.tasks) == 0 {
		if m.closing {
			m.completed = make(map[producerWithdrawalKey]struct{})
			return nil, 0, nil, true
		}
		return nil, time.Hour, nil, false
	}
	if m.closing && (!now.Before(m.deadline) || (m.shutdownCtx != nil && m.shutdownCtx.Err() != nil)) {
		final := m.finalSnapshotLocked()
		return nil, 0, final, true
	}

	var selected *producerWithdrawalTask
	for _, task := range m.tasks {
		if selected == nil || task.nextAttempt.Before(selected.nextAttempt) ||
			(task.nextAttempt.Equal(selected.nextAttempt) && task.sequence < selected.sequence) {
			selected = task
		}
	}
	if selected.nextAttempt.After(now) {
		wait := selected.nextAttempt.Sub(now)
		if m.closing {
			if remaining := m.deadline.Sub(now); remaining < wait {
				wait = remaining
			}
		}
		if wait < 0 {
			wait = 0
		}
		return nil, wait, nil, false
	}
	copy := *selected
	return &copy, 0, nil, false
}

func (m *producerWithdrawalManager) finalSnapshotLocked() []producerWithdrawalEvent {
	final := make([]producerWithdrawalEvent, 0, len(m.tasks))
	for _, task := range m.tasks {
		disposition := task.lastDisposition
		if !disposition.valid() || disposition == producerWithdrawalSatisfied {
			disposition = producerWithdrawalPersistent
		}
		err := task.lastErr
		if err == nil {
			err = context.DeadlineExceeded
		} else {
			err = errors.Join(err, context.DeadlineExceeded)
		}
		final = append(final, producerWithdrawalEvent{
			Key:         task.key,
			Reason:      task.reason,
			Attempt:     task.attempt,
			Err:         err,
			Disposition: disposition,
			Shutdown:    true,
			Final:       true,
		})
	}
	// No admission remains after Close. Drop all task and tombstone storage
	// only after the final diagnostic snapshot is complete.
	m.tasks = make(map[producerWithdrawalKey]*producerWithdrawalTask)
	m.completed = make(map[producerWithdrawalKey]struct{})
	return final
}

func (m *producerWithdrawalManager) attempt(task *producerWithdrawalTask) {
	ctx, attemptID, cancel, phase, ok := m.attemptContext()
	if !ok {
		return
	}

	err := m.withdraw(ctx, task.key.generationID, task.key.producer, task.reason)
	disposition := producerWithdrawalSatisfied
	if err != nil {
		disposition = producerWithdrawalTransient
		if m.classify != nil {
			classified, classifyErr := m.classify(ctx, task.key.generationID, task.key.producer, err)
			if classifyErr != nil {
				err = errors.Join(err, classifyErr)
				disposition = producerWithdrawalPersistent
			} else {
				disposition = classified
			}
		}
	}
	if !disposition.valid() {
		err = errors.Join(err, fmt.Errorf("invalid producer withdrawal disposition %d", disposition))
		disposition = producerWithdrawalPersistent
	}

	m.releaseAttempt(attemptID, cancel)
	m.finish(task.key, err, disposition, phase)
}

func (m *producerWithdrawalManager) attemptContext() (
	context.Context,
	uint64,
	context.CancelFunc,
	producerWithdrawalAttemptPhase,
	bool,
) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.activeID++
	attemptID := m.activeID
	if m.closing {
		if m.shutdownCtx == nil || m.shutdownCtx.Err() != nil {
			return nil, attemptID, nil, producerWithdrawalShutdownAttempt, false
		}
		// Every final-flush attempt shares the one Close deadline. Do not add
		// per-attempt timers that could extend or replace that budget.
		return m.shutdownCtx, attemptID, nil, producerWithdrawalShutdownAttempt, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.config.attemptTimeout)
	m.activeCancel = cancel
	return ctx, attemptID, cancel, producerWithdrawalNormalAttempt, true
}

func (m *producerWithdrawalManager) releaseAttempt(attemptID uint64, cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	m.mu.Lock()
	if m.activeID == attemptID {
		m.activeCancel = nil
	}
	m.mu.Unlock()
}

func (m *producerWithdrawalManager) finish(
	key producerWithdrawalKey,
	err error,
	disposition producerWithdrawalDisposition,
	phase producerWithdrawalAttemptPhase,
) {
	m.mu.Lock()
	current := m.tasks[key]
	if current == nil {
		m.mu.Unlock()
		return
	}
	current.attempt++
	current.lastErr = err
	current.lastDisposition = disposition
	attempt := current.attempt
	reason := current.reason
	backoff := time.Duration(0)
	final := disposition == producerWithdrawalSatisfied

	switch {
	case disposition == producerWithdrawalSatisfied:
		delete(m.tasks, key)
		m.completed[key] = struct{}{}
	case phase == producerWithdrawalShutdownAttempt:
		// A shutdown attempt is the final try for this key. It already used the
		// shared Close deadline, so normal or persistent backoff cannot enqueue
		// work beyond that lifetime.
		delete(m.tasks, key)
		final = true
	case m.closing:
		// Close canceled this normal attempt. Hand it directly to exactly one
		// shutdown attempt instead of applying its ordinary retry disposition.
		backoff = 0
	case disposition == producerWithdrawalTransient:
		backoff = m.transientBackoff(attempt)
	case disposition == producerWithdrawalPersistent:
		backoff = m.config.maxBackoff
	}
	if !final {
		m.nextSequence++
		current.sequence = m.nextSequence
		current.nextAttempt = time.Now().Add(backoff)
	}
	observer := m.config.observe
	m.mu.Unlock()

	if observer != nil {
		observer(producerWithdrawalEvent{
			Key:         key,
			Reason:      reason,
			Attempt:     attempt,
			Err:         err,
			Disposition: disposition,
			Backoff:     backoff,
			Shutdown:    phase == producerWithdrawalShutdownAttempt,
			Final:       final,
		})
	}
	m.signal()
}

func (m *producerWithdrawalManager) transientBackoff(attempt int) time.Duration {
	backoff := m.config.initialBackoff
	for i := 1; i < attempt && backoff < m.config.maxBackoff; i++ {
		if backoff > m.config.maxBackoff/2 {
			return m.config.maxBackoff
		}
		backoff *= 2
	}
	if backoff > m.config.maxBackoff {
		return m.config.maxBackoff
	}
	return backoff
}

// close rejects admission, cancels an active normal attempt, and joins the
// worker after a bounded final flush under one shared shutdown deadline. The
// callback context contract above is required for that bound to hold.
func (m *producerWithdrawalManager) close() {
	m.closeOne.Do(func() {
		m.mu.Lock()
		m.accept = false
		m.closing = true
		m.deadline = time.Now().Add(m.config.shutdownBudget)
		m.shutdownCtx, m.shutdownEnd = context.WithDeadline(context.Background(), m.deadline)
		activeCancel := m.activeCancel
		m.activeCancel = nil
		for _, task := range m.tasks {
			task.nextAttempt = time.Time{}
		}
		m.mu.Unlock()

		if activeCancel != nil {
			activeCancel()
		}
		m.signal()
		<-m.done
		m.shutdownEnd()
	})
}

func (m *producerWithdrawalManager) pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tasks)
}
