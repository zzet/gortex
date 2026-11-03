package main

import "context"

// The opt-in wiring is introduced with its actual transport-loss regression.
// Until implemented, enabling it has no effect on the existing call route.
func withReadSourceFailureEvidence(ctx context.Context, _ bool) context.Context { return ctx }
