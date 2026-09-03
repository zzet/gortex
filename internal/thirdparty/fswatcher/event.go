package fswatcher

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// EventType represents a filesystem event type
type EventType uint32

// Cross-platform event types
const (
	EventUnknown EventType = iota
	EventCreate
	EventRemove
	EventMod
	EventRename
	EventChmod
	EventOverflow
)

// eventTypeNames maps EventType to its string representation
var eventTypeNames = [...]string{"Unknown", "Create", "Delete", "Edit", "Rename", "Chmod", "Overflow"}

// String returns the string representation of an EventType
func (e EventType) String() string {
	if e < EventUnknown || e > EventOverflow {
		return "Invalid"
	}
	return eventTypeNames[e]
}

// WatchEvent represents a single filesystem event
type WatchEvent struct {
	ID    uint64      // Platform-specific event ID
	Path  string      // Path of the file or directory
	Flags []string    // Platform-specific event flags
	Types []EventType // Cross-platform event types (Create, Remove, etc)
	Time  time.Time   // Time the event was received
}

// String returns a formatted string representation of a WatchEvent
func (we WatchEvent) String() string {
	var typeStrings []string
	for _, t := range we.Types {
		typeStrings = append(typeStrings, t.String())
	}
	return fmt.Sprintf("Event{ID: %d, Path: %q, Types: [%s], Flags: [%s], Time: %s}",
		we.ID, we.Path, strings.Join(typeStrings, ", "), strings.Join(we.Flags, ", "), we.Time.Format(time.RFC3339Nano))
}

// EventAggregator merges multiple events for the same path over a short duration
type EventAggregator struct {
	events        map[string]*aggregatedEvent
	cooldown      time.Duration
	watcher       *watcher
	mu            sync.Mutex
	isClosed      atomic.Bool
	started       atomic.Bool
	wake          chan struct{}
	done          chan struct{}
	runDone       chan struct{}
	closeDone     chan struct{}
	beforeAddLock func()
}

// aggregatedEvent holds merged data for events on the same path
type aggregatedEvent struct {
	path     string
	types    map[EventType]struct{}
	flags    map[string]struct{}
	lastSeen time.Time
	eventID  uint64
	deadline time.Time
	version  uint64
}

// newEventAggregator creates a new event aggregator
func newEventAggregator(w *watcher, cooldown time.Duration) *EventAggregator {
	return &EventAggregator{
		events:    make(map[string]*aggregatedEvent),
		cooldown:  cooldown,
		watcher:   w,
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
		runDone:   make(chan struct{}),
		closeDone: make(chan struct{}),
	}
}

// addEvent adds an event to the aggregator
func (ea *EventAggregator) addEvent(event WatchEvent) {
	if ea.isClosed.Load() {
		return
	}
	if ea.beforeAddLock != nil {
		ea.beforeAddLock()
	}

	ea.mu.Lock()
	defer ea.mu.Unlock()
	// The fast-path check above is only advisory. close serializes its state
	// transition with this lock, so an event that was already in flight cannot
	// be inserted after close has snapshotted the remaining events.
	if ea.isClosed.Load() {
		return
	}
	if ea.started.CompareAndSwap(false, true) {
		go ea.run()
	}

	now := time.Now()
	deadline := now.Add(ea.cooldown)

	existing, exists := ea.events[event.Path]
	if exists {
		// Update existing event
		existing.lastSeen = now
		existing.deadline = deadline
		existing.version++
		if event.ID > existing.eventID {
			existing.eventID = event.ID
		}

		for _, t := range event.Types {
			existing.types[t] = struct{}{}
		}
		for _, f := range event.Flags {
			existing.flags[f] = struct{}{}
		}
	} else {
		// Create a new aggregated event
		types := make(map[EventType]struct{}, len(event.Types))
		for _, t := range event.Types {
			types[t] = struct{}{}
		}
		flags := make(map[string]struct{}, len(event.Flags))
		for _, f := range event.Flags {
			flags[f] = struct{}{}
		}

		existing = &aggregatedEvent{
			path:     event.Path,
			types:    types,
			flags:    flags,
			lastSeen: now,
			eventID:  event.ID,
			deadline: deadline,
			version:  1,
		}
		ea.events[event.Path] = existing
	}

	ea.signal()
}

func (ea *EventAggregator) signal() {
	select {
	case ea.wake <- struct{}{}:
	default:
	}
}

func (ea *EventAggregator) run() {
	defer close(ea.runDone)

	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ea.done:
			if timer != nil {
				timer.Stop()
			}
			return
		default:
		}

		next, ok := ea.nextDeadline()
		if !ok {
			if timer != nil {
				timer.Stop()
				timer = nil
			}
			select {
			case <-ea.wake:
				continue
			case <-ea.done:
				return
			}
		}

		delay := time.Until(next)
		if delay <= 0 {
			ea.flushDue(time.Now())
			continue
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(delay)
		}
		timerC = timer.C

		select {
		case <-timerC:
			ea.flushDue(time.Now())
		case <-ea.wake:
		case <-ea.done:
			if timer != nil {
				timer.Stop()
			}
			return
		}
	}
}

func (ea *EventAggregator) nextDeadline() (time.Time, bool) {
	ea.mu.Lock()
	defer ea.mu.Unlock()

	var next time.Time
	for _, event := range ea.events {
		if next.IsZero() || event.deadline.Before(next) {
			next = event.deadline
		}
	}
	return next, !next.IsZero()
}

func (ea *EventAggregator) flushDue(now time.Time) {
	var ready []struct {
		path    string
		version uint64
	}

	ea.mu.Lock()
	if ea.isClosed.Load() {
		ea.mu.Unlock()
		return
	}
	for path, event := range ea.events {
		if !event.deadline.After(now) {
			ready = append(ready, struct {
				path    string
				version uint64
			}{path: path, version: event.version})
		}
	}
	ea.mu.Unlock()

	for _, item := range ready {
		ea.flushPath(item.path, item.version, false)
	}
}

// flushPath sends a single aggregated event and removes it from the map
func (ea *EventAggregator) flushPath(path string, version uint64, force bool) {
	ea.mu.Lock()
	if !force && ea.isClosed.Load() {
		ea.mu.Unlock()
		return
	}

	aggregated, exists := ea.events[path]
	if !exists {
		ea.mu.Unlock()
		return
	}
	if !force && aggregated.version != version {
		ea.mu.Unlock()
		return
	}

	// Remove from map before sending to avoid race with new events
	delete(ea.events, path)

	// Collect unique types and flags
	types := make([]EventType, 0, len(aggregated.types))
	for t := range aggregated.types {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		return types[i] < types[j]
	})

	flags := make([]string, 0, len(aggregated.flags))
	for f := range aggregated.flags {
		flags = append(flags, f)
	}
	sort.Strings(flags)

	event := WatchEvent{
		ID:    aggregated.eventID,
		Path:  aggregated.path,
		Types: types,
		Flags: flags,
		Time:  aggregated.lastSeen,
	}

	ea.mu.Unlock()
	ea.watcher.sendToChannel(event)
}

// close stops the aggregator and flushes any remaining events
func (ea *EventAggregator) close() {
	ea.mu.Lock()
	if ea.isClosed.Load() {
		ea.mu.Unlock()
		<-ea.closeDone
		return
	}
	ea.isClosed.Store(true)
	close(ea.done)

	// Copy paths to avoid modification during iteration
	paths := make([]string, 0, len(ea.events))
	for path := range ea.events {
		paths = append(paths, path)
	}
	started := ea.started.Load()
	ea.mu.Unlock()

	// Flush everything immediately
	for _, path := range paths {
		ea.flushPath(path, 0, true)
	}
	if started {
		<-ea.runDone
	}
	close(ea.closeDone)
}

// uniqueEventTypes removes duplicate EventType values from a slice
func uniqueEventTypes(s []EventType) []EventType {
	if len(s) < 2 {
		return s
	}

	// The max value is EventOverflow (6)
	const maxEventType = EventOverflow + 1
	var seen [maxEventType]bool

	j := 0
	for _, v := range s {
		// Ignore any invalid event types
		if v < maxEventType && !seen[v] {
			seen[v] = true
			s[j] = v
			j++
		}
	}
	return s[:j]
}
