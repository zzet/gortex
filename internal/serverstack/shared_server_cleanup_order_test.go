package serverstack

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestSharedServerCleanupContinuesLIFOAndRetainsEveryError(t *testing.T) {
	var order []string
	backendErr := errors.New("backend close failed")
	lifecycleErr := errors.New("lifecycle cleanup failed after joining")
	sidecarErr := errors.New("sidecar close failed")
	core, logs := observer.New(zap.InfoLevel)
	s := &SharedServer{logger: zap.New(core), sidecarPaths: []string{"first", "second"}}
	s.addCleanup("backend", func() error { order = append(order, "backend"); return backendErr })
	s.addCleanup("multi-indexer", func() error { order = append(order, "multi-indexer"); return nil })
	s.addCleanup("lifecycle", func() error { order = append(order, "lifecycle"); return lifecycleErr })
	s.addCleanup("mcp-background", func() error { order = append(order, "mcp-background"); return nil })
	s.closeSidecar = func(path string) error {
		order = append(order, path)
		if path == "first" {
			return sidecarErr
		}
		return nil
	}
	// Every concurrent/later caller must see the same cached result and no
	// cleanup callback may rerun against a closed handle.
	results := make(chan error, 8)
	var callers sync.WaitGroup
	for range 8 {
		callers.Go(func() { results <- s.Close() })
	}
	callers.Wait()
	close(results)
	first := s.Close()
	for err := range results {
		if err != first || !errors.Is(err, backendErr) || !errors.Is(err, lifecycleErr) || !errors.Is(err, sidecarErr) {
			t.Fatalf("lost or changed cleanup result: %v", err)
		}
	}
	want := []string{"mcp-background", "lifecycle", "multi-indexer", "backend", "first", "second"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("cleanup order=%v, want %v", order, want)
	}
	if !strings.Contains(first.Error(), "backend: ") || !strings.Contains(first.Error(), "sidecar first: ") {
		t.Fatalf("errors lack phase identity: %v", first)
	}
	if logs.FilterMessage("serverstack: shutdown phase start").Len() != len(want) || logs.FilterMessage("serverstack: shutdown phase complete").Len() != len(want) {
		t.Fatalf("missing cleanup phase receipts: %v", logs.All())
	}
}
