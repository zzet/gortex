package graph

import (
	"context"
	"time"
)

// NodeKindReadTiming observes one physical lookup batch. Gate is nested inside
// QueryStart; Total also includes construction and connection return. No SQL or
// identities are retained. Observers are synchronous and request-local.
type NodeKindReadTiming struct {
	Pool, Gate, QueryStart, Drain, Total time.Duration
	Batches, InputIDs, Rows, Errors      int
}

func (t *NodeKindReadTiming) Add(o NodeKindReadTiming) {
	t.Pool += o.Pool
	t.Gate += o.Gate
	t.QueryStart += o.QueryStart
	t.Drain += o.Drain
	t.Total += o.Total
	t.Batches += o.Batches
	t.InputIDs += o.InputIDs
	t.Rows += o.Rows
	t.Errors += o.Errors
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
