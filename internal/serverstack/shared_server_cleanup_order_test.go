package serverstack

import (
	"errors"
	"reflect"
	"testing"
)

func TestSharedServerCleanupContinuesLIFOAfterHandledLifecycleError(t *testing.T) {
	var order []string
	cleanupError := errors.New("handled lifecycle cleanup error")
	var observed error
	s := &SharedServer{cleanup: []func(){
		func() { order = append(order, "backend") },
		func() { order = append(order, "multi-indexer") },
		func() { order = append(order, "lifecycle"); observed = cleanupError },
		func() { order = append(order, "mcp-background") },
	}}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"mcp-background", "lifecycle", "multi-indexer", "backend"}
	if !reflect.DeepEqual(order, want) || observed != cleanupError {
		t.Fatalf("order=%v handled=%v", order, observed)
	}
	// Callbacks have no error return; production lifecycle callback logs its
	// error and returns normally. Panic recovery is not part of Close's contract.
}
