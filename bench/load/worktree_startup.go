package main

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

// An exact rider alone can describe a predecessor snapshot. Require the
// requested marker and absence of every predecessor in the same indexed reply.
func worktreeStartupReady(raw json.RawMessage, expected string, linked bool) (bool, string, error) {
	var reply daemon.SearchSymbolsResult
	if err := json.Unmarshal(raw, &reply); err != nil {
		return false, "", err
	}
	checkout := ""
	if reply.View != nil && ((linked && (reply.View.Kind == daemon.ProbeViewWorktree || reply.View.Kind == daemon.ProbeViewUnrouted)) || (!linked && reply.View.Kind == daemon.ProbeViewBase)) {
		checkout = reply.View.CheckoutID
	}
	if reply.View == nil || !reply.View.Exact {
		return false, checkout, nil
	}
	if linked && (reply.View.Kind != daemon.ProbeViewWorktree || reply.View.CheckoutID == "") {
		return false, checkout, nil
	}
	if !linked && reply.View.Kind != daemon.ProbeViewBase {
		return false, "", nil
	}
	found := false
	for _, hit := range reply.Hits {
		if hit.Name == expected {
			found = true
		}
		for _, marker := range []string{"StartupBaseMarker", "StartupInitialMarker", "StartupFinalMarker", "StartupSeedMarker"} {
			if hit.Name == marker && marker != expected {
				return false, checkout, nil
			}
		}
	}
	return found, checkout, nil
}

func attachStartupPhases(row *worktreeStartupCase, status json.RawMessage) {
	row.PhaseEvidence = "unavailable: no unambiguous checkout identity observed"
	if row.CheckoutID == "" {
		return
	}
	var observed struct {
		Phases []json.RawMessage `json:"publication_phases"`
	}
	if json.Unmarshal(status, &observed) != nil {
		row.PhaseEvidence = "unavailable: invalid publication phase status"
		return
	}
	var matched []json.RawMessage
	for _, phase := range observed.Phases {
		var identity struct {
			Checkout string `json:"checkout_id"`
		}
		if json.Unmarshal(phase, &identity) == nil && identity.Checkout == row.CheckoutID {
			matched = append(matched, phase)
		}
	}
	row.PhaseEvidence = "unavailable: daemon returned no matching checkout phase record"
	if len(matched) > 0 {
		row.PublicationPhases, _ = json.Marshal(matched)
		row.PhaseEvidence = "observed: daemon monotonic offsets, wall origin used only for cross-process correlation"
	}
}

// Correlate the owned Git switch with the first observed publication, not a
// sleep. Offsets are monotonic inside the daemon; OriginWall bridges processes.
func worktreeStartupRace(raw json.RawMessage, checkout string, created, switchStart, switchEnd time.Time) error {
	var status struct {
		Phases []daemon.PublicationPhaseStatus `json:"publication_phases"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return err
	}
	var first *daemon.PublicationPhaseStatus
	var firstOrigin time.Time
	for i, record := range status.Phases {
		origin, err := time.Parse(time.RFC3339Nano, record.OriginWall)
		if err != nil || record.CheckoutID != checkout || record.Clock != "monotonic" || origin.Before(created) {
			continue
		}
		if failedBeforeStartupPlanning(record, origin, switchStart) {
			continue
		}
		if first == nil || origin.Before(firstOrigin) {
			first, firstOrigin = &status.Phases[i], origin
		}
	}
	if first != nil {
		var admitted, planned, ended time.Time
		for _, phase := range first.Phases {
			at := firstOrigin.Add(time.Duration(phase.OffsetNS))
			if phase.Phase == "admitted" {
				admitted = at
			}
			if phase.Phase == "planned" {
				planned = at
			}
			if phase.Phase == "published" || phase.Phase == "ticket_failed" || phase.Phase == "ticket_completed" {
				if ended.IsZero() || at.Before(ended) {
					ended = at
				}
			}
		}
		if !admitted.IsZero() && !planned.Before(admitted) && !ended.IsZero() && switchStart.After(planned) && switchEnd.Before(ended) {
			return nil
		}
	}
	return errors.New("missed or unproven HEAD race: switch must start after initial planning and finish before its first publication/terminal event")
}

// A failed capture/admission before planning is not an initial build. Keep
// incomplete/unknown traces and every record that reached work: they must still
// be the first cycle tested, so a later success cannot hide a missed race.
func failedBeforeStartupPlanning(record daemon.PublicationPhaseStatus, origin, switchStart time.Time) bool {
	if !record.Terminal {
		return false
	}
	var failed time.Time
	for _, phase := range record.Phases {
		switch phase.Phase {
		case "received", "change_observed", "admitted", "ticket_enqueued", "cycle_started":
		case "ticket_failed":
			at := origin.Add(time.Duration(phase.OffsetNS))
			if failed.IsZero() || at.Before(failed) {
				failed = at
			}
		default:
			return false
		}
	}
	return !failed.IsZero() && failed.Before(switchStart)
}
