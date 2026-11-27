package indexer

import (
	"context"
	"time"
)

// Dispatch checks are advisory cancellation, not publication authorization.
// Bound Git fallback work on platforms without usable HEAD metadata; phase
// boundaries still call checkNeeded directly, and publication re-samples HEAD.
func rateLimitedDispatchCheck(check func(context.Context) error, now func() time.Time) func(context.Context) error {
	if check == nil {
		return nil
	}
	var next time.Time
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		at := now()
		if at.Before(next) {
			return nil
		}
		if err := check(ctx); err != nil {
			return err
		}
		next = now().Add(time.Second)
		return nil
	}
}
