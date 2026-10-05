package graph

import (
	"context"
	"time"
)

// NodeKindReadTiming observes one physical lookup batch. Gate is nested inside
// QueryStart; PreDriver includes pool/acquisition/setup before first driver
// entry, not pure pool wait. Later retries remain inside QueryStart. Total also
// includes construction and cursor closure. No SQL or
// identities are retained. Observers are synchronous and request-local.
type NodeKindReadTiming struct {
	PreDriver, Gate, QueryStart, Drain, Total      time.Duration
	Batches, InputIDs, Rows, Errors, DriverEntries int
}

func (t *NodeKindReadTiming) Add(o NodeKindReadTiming) {
	t.PreDriver += o.PreDriver
	t.Gate += o.Gate
	t.QueryStart += o.QueryStart
	t.Drain += o.Drain
	t.Total += o.Total
	t.Batches += o.Batches
	t.InputIDs += o.InputIDs
	t.Rows += o.Rows
	t.Errors += o.Errors
	t.DriverEntries += o.DriverEntries
}

type nodeKindReadObserverKey struct{}

func WithNodeKindReadObserver(ctx context.Context, observer func(NodeKindReadTiming)) context.Context {
	if ctx == nil || observer == nil {
		return ctx
	}
	return context.WithValue(ctx, nodeKindReadObserverKey{}, observer)
}
func NodeKindReadObserver(ctx context.Context) func(NodeKindReadTiming) {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(nodeKindReadObserverKey{}).(func(NodeKindReadTiming))
	return observer
}
