package mcp

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestConcurrentLocalizationTerminalReplayIsStable(t *testing.T) {
	state := &localizationTerminalState{}
	completion := newLocalizationCompletion(true, "")
	completion.digest = testEvidenceDigest()
	state.armForTask(completion, "find the storage load implementations")
	baseline, _ := state.authorize("search", "symbols", nil)
	canonical, err := json.Marshal(baseline)
	if err != nil {
		t.Fatalf("marshal baseline replay: %v", err)
	}

	const workers = 24
	errors := make(chan string, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, reserved := state.authorize("read", "source", nil)
			if reserved || result == nil || result.IsError {
				errors <- "concurrent replay was not an immediate success"
				return
			}
			wire, err := json.Marshal(result)
			if err != nil || string(wire) != string(canonical) {
				errors <- "concurrent replay differed from the canonical result"
				return
			}
			host := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
			host.Evidence.Evidence[0].Signature = "mutated"
		}()
	}
	group.Wait()
	close(errors)
	for message := range errors {
		t.Error(message)
	}
	final, _ := state.authorize("relations", "callers", nil)
	wire, err := json.Marshal(final)
	if err != nil || string(wire) != string(canonical) {
		t.Fatalf("caller mutation changed later replay: %s (%v)", wire, err)
	}
}
