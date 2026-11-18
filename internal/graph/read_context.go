package graph

import "context"

// ReadContextBinder is implemented by readers whose long whole-store reads
// can honour a caller's context: BindReadContext returns a reader over the
// same data whose paged reads stop within one page once ctx ends (and so stop
// pinning the store's WAL snapshot). A request handler binds its reader
// before handing it to an analysis walk, so an abandoned request stops
// reading shortly after its deadline instead of running to completion.
type ReadContextBinder interface {
	BindReadContext(ctx context.Context) Reader
}

// BindReadContext returns r bound to ctx when r supports it, r otherwise.
func BindReadContext(r Reader, ctx context.Context) Reader {
	if r == nil || ctx == nil {
		return r
	}
	if b, ok := r.(ReadContextBinder); ok {
		return b.BindReadContext(ctx)
	}
	return r
}
