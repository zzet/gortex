package hub

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

// Hub fans out watcher events to multiple subscribers.
type Hub struct {
	subscribers map[string]chan indexer.GraphChangeEvent
	mu          sync.RWMutex
	nextID      int
	done        chan struct{}
	logger      *zap.Logger
	stderr      io.Writer
}

// New creates a new Hub.
func New() *Hub {
	h := NewWithLogger(nil)
	h.stderr = os.Stderr
	return h
}

// NewWithLogger routes watcher diagnostics through the caller's logger.
func NewWithLogger(logger *zap.Logger) *Hub {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Hub{
		subscribers: make(map[string]chan indexer.GraphChangeEvent),
		done:        make(chan struct{}),
		logger:      logger,
	}
}

// Run reads from the watcher events channel and broadcasts to all subscribers.
// It logs each event to stderr, or at Debug when created with NewWithLogger.
// Blocks until the events channel is closed or Stop is called.
func (h *Hub) Run(events <-chan indexer.GraphChangeEvent) {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			if h.stderr != nil {
				fmt.Fprintf(h.stderr, "[gortex watch] %-10s %s  +%d nodes  +%d edges  -%d nodes  -%d edges  (%dms)\n",
					ev.Kind, ev.FilePath, ev.NodesAdded, ev.EdgesAdded, ev.NodesRemoved, ev.EdgesRemoved, ev.DurationMs)
			} else {
				h.logger.Debug("gortex watch",
					zap.String("kind", string(ev.Kind)), zap.String("file_path", ev.FilePath),
					zap.Int("nodes_added", ev.NodesAdded), zap.Int("edges_added", ev.EdgesAdded),
					zap.Int("nodes_removed", ev.NodesRemoved), zap.Int("edges_removed", ev.EdgesRemoved),
					zap.Int64("duration_ms", ev.DurationMs))
			}

			h.broadcast(ev)
		case <-h.done:
			return
		}
	}
}

func (h *Hub) broadcast(ev indexer.GraphChangeEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subscribers {
		select {
		case ch <- ev:
		default:
			// Slow subscriber — drop event to avoid blocking
		}
	}
}

// Subscribe creates a new subscriber channel. Returns an ID for unsubscribing
// and a receive-only channel that will receive graph change events.
func (h *Hub) Subscribe() (string, <-chan indexer.GraphChangeEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := fmt.Sprintf("sub-%d", h.nextID)
	ch := make(chan indexer.GraphChangeEvent, 16)
	h.subscribers[id] = ch
	return id, ch
}

// Unsubscribe removes a subscriber and closes its channel.
func (h *Hub) Unsubscribe(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subscribers[id]; ok {
		close(ch)
		delete(h.subscribers, id)
	}
}

// SubscriberCount returns the number of active subscribers. Tests use this
// to wait for an SSE client to finish registering before publishing events
// that the client needs to observe — broadcast() drops messages silently
// when a subscriber hasn't registered yet, so timing races are easy to
// write without a way to synchronize.
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// Stop signals the hub to stop processing events.
func (h *Hub) Stop() {
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}
