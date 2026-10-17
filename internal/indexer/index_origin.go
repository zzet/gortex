package indexer

import "context"

type indexOriginKey struct{}

// WithIndexOrigin labels full-index logs with the entry point that requested the work.
// The label is diagnostic only; it does not change indexing behavior.
func WithIndexOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, indexOriginKey{}, origin)
}

func indexOrigin(ctx context.Context) string {
	origin, _ := ctx.Value(indexOriginKey{}).(string)
	if origin == "" {
		return "unknown"
	}
	return origin
}
