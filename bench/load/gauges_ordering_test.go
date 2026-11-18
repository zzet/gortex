package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The fixture's initial replay is synchronously sent before its subscribe
// reply. Its counter also catches moving series setup after warmup, even when
// a serialized wall clock cannot distinguish the two events.
func gaugeWarmupOrderError(rows []gaugeRow, attempts []Attempt) error {
	if len(rows) == 0 || rows[0].Received.IsZero() {
		return errors.New("no initial gauge observation")
	}
	var evidence struct {
		Initial bool    `json:"initial_replay"`
		Warmup  *uint64 `json:"fixture_warmup_calls"`
	}
	if err := json.Unmarshal(rows[0].Health, &evidence); err != nil || !evidence.Initial || evidence.Warmup == nil || *evidence.Warmup != 0 {
		return errors.New("initial gauge replay did not precede observed warmup calls")
	}
	first, probes := rows[0].Received, 0
	for _, attempt := range attempts {
		if attempt.Role != "probe" {
			continue
		}
		probes++
		// Equal serialized ticks preserve the causal replay proof above.
		if attempt.Started.IsZero() || first.After(attempt.Started) {
			return fmt.Errorf("series started after a warmup gate: first row %v, gate call %v", first, attempt.Started)
		}
	}
	if probes == 0 {
		return errors.New("no warmup gate calls recorded")
	}
	return nil
}

func TestGaugeWarmupOrderPreservesCausalityAcrossClockPrecision(t *testing.T) {
	base := time.Unix(1762000000, 123456000)
	for _, quantum := range []time.Duration{100 * time.Nanosecond, time.Microsecond, time.Millisecond} {
		t.Run(quantum.String(), func(t *testing.T) {
			// These ordered events fall in one wall-clock tick. JSON round
			// trips also discard Go's in-process monotonic component.
			first := base.Add(10 * time.Nanosecond).Truncate(quantum)
			gate := base.Add(90 * time.Nanosecond).Truncate(quantum)
			rows := []gaugeRow{{Received: first, Health: json.RawMessage(`{"initial_replay":true,"fixture_warmup_calls":0}`)}}
			raw, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &rows); err != nil {
				t.Fatal(err)
			}
			attempts := []Attempt{{Role: "probe", Started: gate}}
			rows[0].Received = gate.Add(-quantum)
			if err := gaugeWarmupOrderError(rows, attempts); err != nil {
				t.Errorf("earlier clock tick rejected: %v", err)
			}
			rows[0].Received = first
			if err := gaugeWarmupOrderError(rows, attempts); err != nil {
				t.Errorf("causally earlier replay rejected at equal clock precision: %v", err)
			}
			rows[0].Received = gate.Add(quantum)
			if err := gaugeWarmupOrderError(rows, attempts); err == nil {
				t.Error("a genuinely later observation was accepted")
			}
			rows[0].Received = gate
			rows[0].Health = json.RawMessage(`{"initial_replay":true,"fixture_warmup_calls":1}`)
			if err := gaugeWarmupOrderError(rows, attempts); err == nil {
				t.Error("late series setup hidden by an equal clock tick was accepted")
			}
		})
	}
}
