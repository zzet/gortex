package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
)

const repoSnapshotTTL = 5 * time.Second

type repoStatsContextReader interface {
	RepoStatsContext(context.Context) (map[string]graph.GraphStats, error)
}

type repoSnapshotFlight struct {
	done       chan struct{}
	cancel     context.CancelFunc
	waiters    int
	rows       []repoEntry
	err        error
	panicValue any
}

type repoSnapshotCache struct {
	mu        sync.Mutex
	rows      []repoEntry
	collected time.Time
	flight    *repoSnapshotFlight
}

func (c *repoSnapshotCache) get(ctx context.Context, g graph.Store, pin *graphview.BasePin) ([]repoEntry, time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	c.mu.Lock()
	if !c.collected.IsZero() && time.Since(c.collected) < repoSnapshotTTL {
		rows, age := c.rows, time.Since(c.collected)
		c.mu.Unlock()
		return rows, age, nil
	}
	f := c.flight
	if f == nil {
		// Cancellation ends a wait, not necessarily the store read. Join the
		// request's hold before launching work that can outlive every waiter.
		hold := pin.Handoff()
		if pin != nil && hold == nil {
			c.mu.Unlock()
			return nil, 0, errors.New("repo snapshot: base pin already released")
		}
		queryCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &repoSnapshotFlight{done: make(chan struct{}), cancel: cancel}
		c.flight = f
		go func() {
			defer cancel()
			var stats map[string]graph.GraphStats
			var err error
			var rows []repoEntry
			// Preserve request-level panic recovery and lease release even though
			// the shared query runs off the HTTP handler's goroutine.
			func() {
				defer hold.Release()
				defer func() { f.panicValue = recover() }()
				if reader, ok := g.(repoStatsContextReader); ok {
					stats, err = reader.RepoStatsContext(queryCtx)
				} else {
					stats = g.RepoStats()
					err = queryCtx.Err()
				}
				rows = repoEntries(stats)
			}()
			c.mu.Lock()
			f.rows, f.err = rows, err
			if c.flight == f {
				if err == nil && f.panicValue == nil {
					c.rows, c.collected = rows, time.Now()
				}
				c.flight = nil
			}
			close(f.done)
			c.mu.Unlock()
		}()
	}
	f.waiters++
	c.mu.Unlock()
	select {
	case <-f.done:
		if f.panicValue != nil {
			panic(f.panicValue)
		}
		return f.rows, 0, f.err
	case <-ctx.Done():
		c.mu.Lock()
		f.waiters--
		if f.waiters == 0 && c.flight == f {
			c.flight = nil
			f.cancel()
		}
		c.mu.Unlock()
		return nil, 0, ctx.Err()
	}
}
