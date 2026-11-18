package main

import (
	"context"
	"sync"
)

// The stop function cancels an in-flight sweep and joins the janitor itself.
// One shared Once protects repeated/concurrent server shutdown callbacks.
func newReconcileJanitorLifetime() (context.Context, chan struct{}, func()) {
	ctx, done, _, stop := newCancelableReconcileJanitorLifetime()
	return ctx, done, stop
}

func newCancelableReconcileJanitorLifetime() (context.Context, chan struct{}, func(), func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(cancel); <-done }
	return ctx, done, func() { once.Do(cancel) }, stop
}
