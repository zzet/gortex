package mcp

import (
	"context"
	"sync"
)

type contractCoreReadErrorKey struct{}

type contractCoreReadErrors struct {
	mu  sync.Mutex
	err error
}

// Reader adjacency is errorless. Install this only for requests eligible for
// lateExactnessRefusalIsSafe: potential writers retain the original classifier
// so no checked read failure can withdraw an already committed write.
// Preserve checked classification failures across
// the request's short-lived reader wrappers so an empty partial graph cannot
// become a successful fresh answer at the handler boundary.
func withContractCoreReadErrors(ctx context.Context) context.Context {
	return context.WithValue(ctx, contractCoreReadErrorKey{}, &contractCoreReadErrors{})
}

func recordContractCoreReadError(ctx context.Context, err error) {
	if ctx == nil || err == nil {
		return
	}
	if state, _ := ctx.Value(contractCoreReadErrorKey{}).(*contractCoreReadErrors); state != nil {
		state.mu.Lock()
		if state.err == nil {
			state.err = err
		}
		state.mu.Unlock()
	}
}

func contractCoreReadError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(contractCoreReadErrorKey{}).(*contractCoreReadErrors)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.err
}

func hasContractCoreReadErrors(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	state, _ := ctx.Value(contractCoreReadErrorKey{}).(*contractCoreReadErrors)
	return state != nil
}
