package main

import (
	"encoding/json"
	"errors"
	"math"
	"time"
)

// Only explicit v2 edit drains carry these receipts. Wall timestamps remain
// trace identities; elapsed budgets never combine them with monotonic durations.
type prEditClock struct {
	Semantics          string             `json:"semantics"`
	ElapsedNS          int64              `json:"elapsed_ns"`
	OfferedNS          int64              `json:"offered_ns"`
	DrainNS            int64              `json:"drain_ns"`
	PhaseWindowStartNS int64              `json:"phase_window_start_ns"`
	Admissions         []prEditClockPoint `json:"admissions"`
	Attempts           []prEditClockPoint `json:"attempts"`
	Edits              []prEditClockPoint `json:"edits"`
}
type prEditClockPoint struct {
	Agent    int       `json:"agent"`
	Step     int       `json:"step"`
	Slot     int       `json:"slot"`
	Retry    int       `json:"retry"`
	Role     string    `json:"role"`
	Tool     string    `json:"tool"`
	At       time.Time `json:"at"`
	OffsetNS int64     `json:"offset_ns"`
}

func capturePREditClock(phase Phase, observed prDrainObservation) *prEditClock {
	c := &prEditClock{Semantics: "live_monotonic_offsets_nanoseconds", ElapsedNS: observed.Finished.Sub(observed.Started).Nanoseconds(), OfferedNS: observed.OfferDeadline.Sub(observed.Started).Nanoseconds(), DrainNS: observed.DrainDeadline.Sub(observed.Started).Nanoseconds()}
	if !phase.Started.IsZero() {
		c.PhaseWindowStartNS = observed.Started.Sub(phase.Started).Nanoseconds()
	}
	for _, a := range observed.Admissions {
		c.Admissions = append(c.Admissions, prEditClockPoint{Agent: a.Agent, Step: a.Step, Slot: a.Slot, Role: "admission", Tool: a.Tool, At: a.OfferedAt, OffsetNS: a.OfferedAt.Sub(observed.Started).Nanoseconds()})
	}
	for _, a := range phase.Attempts {
		c.Attempts = append(c.Attempts, prEditClockPoint{Agent: a.Agent, Step: a.Step, Slot: a.Slot, Retry: a.Retry, Role: a.Role, Tool: a.Tool, At: a.Started, OffsetNS: a.Started.Sub(observed.Started).Nanoseconds()})
	}
	for _, e := range phase.Edits {
		c.Edits = append(c.Edits, prEditClockPoint{Role: "edit", Tool: e.Marker, At: e.Started, OffsetNS: e.Started.Sub(observed.Started).Nanoseconds()})
	}
	return c
}
func validatePREditClock(phase Phase, o prDrainObservation) error {
	c := o.EditClock
	if o.Version != 2 || c == nil || c.Semantics != "live_monotonic_offsets_nanoseconds" || c.ElapsedNS < 0 || c.OfferedNS <= 0 || c.DrainNS < c.OfferedNS || c.ElapsedNS < c.OfferedNS || c.ElapsedNS > c.DrainNS || c.PhaseWindowStartNS < 0 || len(c.Admissions) != len(o.Admissions) || len(c.Attempts) != len(phase.Attempts) || len(c.Edits) != len(phase.Edits) {
		return errors.New("edit drain lacks valid same-clock interval receipts")
	}
	if math.IsNaN(o.ElapsedSeconds) || math.IsInf(o.ElapsedSeconds, 0) || o.ElapsedSeconds != time.Duration(c.ElapsedNS).Seconds() {
		return errors.New("edit drain elapsed receipt differs from same-clock interval")
	}
	for i, a := range o.Admissions {
		p := c.Admissions[i]
		if p.Agent != a.Agent || p.Step != a.Step || p.Slot != a.Slot || p.Retry != 0 || p.Role != "admission" || p.Tool != a.Tool || !p.At.Equal(a.OfferedAt) || p.At.IsZero() || p.OffsetNS < 0 || p.OffsetNS >= c.OfferedNS {
			return errors.New("edit drain missing, mismatched or late same-clock admission")
		}
	}
	for i, a := range phase.Attempts {
		p := c.Attempts[i]
		if p.Agent != a.Agent || p.Step != a.Step || p.Slot != a.Slot || p.Retry != a.Retry || p.Role != a.Role || p.Tool != a.Tool || !p.At.Equal(a.Started) || p.At.IsZero() || p.OffsetNS < 0 || p.OffsetNS > c.ElapsedNS || a.DurationMS < 0 || math.IsNaN(a.DurationMS) || math.IsInf(a.DurationMS, 0) || a.DurationMS > float64(c.ElapsedNS-p.OffsetNS)/float64(time.Millisecond) {
			return errors.New("edit drain attempt has invalid elapsed time or ends after same-clock phase completion")
		}
	}
	for i, e := range phase.Edits {
		p := c.Edits[i]
		if p.Role != "edit" || p.Tool != e.Marker || !p.At.Equal(e.Started) || p.At.IsZero() || p.OffsetNS < 0 || p.OffsetNS > c.ElapsedNS {
			return errors.New("edit drain marker lacks its same-clock receipt")
		}
	}
	return nil
}
func validatePREditPhaseClock(phase Phase, o prDrainObservation, config *PRSmokeDrain) error {
	if err := validatePREditClock(phase, o); err != nil {
		return err
	}
	c := o.EditClock
	if math.IsNaN(config.OfferedSeconds) || math.IsInf(config.OfferedSeconds, 0) || math.IsNaN(config.MaxDrainSeconds) || math.IsInf(config.MaxDrainSeconds, 0) || math.IsNaN(phase.DurationSec) || math.IsInf(phase.DurationSec, 0) || phase.DurationSec < 0 || time.Duration(c.OfferedNS).Seconds() != config.OfferedSeconds || time.Duration(c.DrainNS-c.OfferedNS).Seconds() != config.MaxDrainSeconds || float64(c.PhaseWindowStartNS)/float64(time.Second) > phase.DurationSec || float64(c.ElapsedNS)/float64(time.Second) > phase.DurationSec-float64(c.PhaseWindowStartNS)/float64(time.Second) {
		return errors.New("edit drain same-clock interval exceeds phase or configured budget")
	}
	if o.AfterOfferDeadlineSeconds != time.Duration(c.ElapsedNS-c.OfferedNS).Seconds() {
		return errors.New("edit drain same-clock after-offer interval mismatch")
	}
	return nil
}

// Missing numeric offsets are unavailable, never silently measured zero.
func (c *prEditClock) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("missing edit clock")
	}
	if err := requireFields(data, prEditClock{}); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"admissions", "attempts", "edits"} {
		var rows []json.RawMessage
		if string(fields[name]) == "null" {
			return errors.New("missing edit clock census")
		}
		if err := json.Unmarshal(fields[name], &rows); err != nil {
			return err
		}
		for _, row := range rows {
			if err := requireFields(row, prEditClockPoint{}); err != nil {
				return err
			}
			var values map[string]json.RawMessage
			if err := json.Unmarshal(row, &values); err != nil {
				return err
			}
			for _, value := range values {
				if string(value) == "null" {
					return errors.New("null edit clock point")
				}
			}
		}
	}
	for _, name := range []string{"semantics", "elapsed_ns", "offered_ns", "drain_ns", "phase_window_start_ns"} {
		if string(fields[name]) == "null" {
			return errors.New("null edit clock interval")
		}
	}
	type plain prEditClock
	return json.Unmarshal(data, (*plain)(c))
}
