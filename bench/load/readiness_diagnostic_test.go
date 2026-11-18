package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReadinessFailureNamesLastObservedEndpoint(t *testing.T) {
	socket := startStub(t, func(stubFrame) (any, *rpcError) {
		return map[string]any{
			"ready": true, "enriched": false,
			"tracked_repos": []any{map[string]any{"path": "/owned/repo", "prefix": "fixture", "state": "ready"}},
			"loaded_roots":  []string{"/owned/repo"},
		}, nil
	})
	p := &privateRun{Socket: socket, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 350*time.Millisecond)
	defer cancel()
	gate, metrics, err := p.awaitReady(ctx, time.Now(), []privateRepo{{Path: "/owned/repo"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("un-enriched state passed or lost its deadline: %v", err)
	}
	if len(gate["readiness"]) == 0 || metrics["cold_enrichment_observed_upper_bound_seconds"].Value != nil {
		t.Fatal("failure must retain its probe and leave enrichment unavailable")
	}
	for _, detail := range []string{"ready=true", "enriched=false", "covered=true"} {
		if !strings.Contains(err.Error(), detail) {
			t.Errorf("readiness diagnostic lacks %q: %v", detail, err)
		}
	}
	if strings.Contains(err.Error(), "/owned/repo") {
		t.Fatal("endpoint summary must not print repository paths")
	}
}
