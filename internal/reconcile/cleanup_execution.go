package reconcile

import (
	"context"
	"fmt"
	"sync"
)

// Stable cleanup ID, NOT attempt token: a successor attempt cannot enter while
// an older attempt is still inside an external hook. Catalog() returns new
// handles, so this process-shared registry deliberately does not key on one.
// Identical IDs in independent Store copies conservatively serialize too.
var cleanupExecutions cleanupExecutionRegistry

type cleanupExecutionRegistry struct {
	mu      sync.Mutex
	entries map[string]*cleanupExecutionEntry
}

type cleanupExecutionEntry struct {
	refs    int // one reference per holder or admitted waiter
	active  bool
	changed chan struct{}
}

func (r *cleanupExecutionRegistry) acquire(ctx context.Context, cleanupID string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cleanupID == "" {
		return nil, fmt.Errorf("%w: empty cleanup execution id", ErrSagaTarget)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	if r.entries == nil {
		r.entries = make(map[string]*cleanupExecutionEntry)
	}
	entry := r.entries[cleanupID]
	if entry == nil {
		entry = &cleanupExecutionEntry{changed: make(chan struct{})}
		r.entries[cleanupID] = entry
	}
	entry.refs++
	for {
		if err := ctx.Err(); err != nil {
			r.dropLocked(cleanupID, entry)
			r.mu.Unlock()
			return nil, err
		}
		if !entry.active {
			entry.active = true
			r.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					r.mu.Lock()
					entry.active = false
					close(entry.changed)
					if entry.refs > 1 {
						entry.changed = make(chan struct{})
					}
					r.dropLocked(cleanupID, entry)
					r.mu.Unlock()
				})
			}, nil
		}
		changed := entry.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		r.mu.Lock()
	}
}

func (r *cleanupExecutionRegistry) dropLocked(cleanupID string, entry *cleanupExecutionEntry) {
	entry.refs--
	if entry.refs == 0 && r.entries[cleanupID] == entry {
		delete(r.entries, cleanupID)
		if len(r.entries) == 0 {
			r.entries = nil
		}
	}
}
