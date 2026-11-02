package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// These are retained native catalog/RepoLoad records supplied by the owned
// fixture/report channel. Neither aggregate status nor a client's first-ready
// observation establishes membership or the registration clock.
type startupTransitionEvidence struct {
	InvalidReason     string                         `json:"invalid_reason,omitempty"`
	PrimaryRoot       string                         `json:"primary_root"`
	Prefix            string                         `json:"prefix"`
	FamilyID          string                         `json:"family_id"`
	BootStarted       time.Time                      `json:"boot_started"`
	PID               int                            `json:"pid"`
	ProcessBirthToken string                         `json:"process_birth_token"`
	BaselineAt        time.Time                      `json:"baseline_at"`
	BaselineCatalog   json.RawMessage                `json:"baseline_catalog"`
	Registration      json.RawMessage                `json:"native_repo_load"`
	RegistrationPID   int                            `json:"registration_pid"`
	RegistrationBirth string                         `json:"registration_birth"`
	Observations      []startupTransitionObservation `json:"observations"`
}

type startupTransitionObservation struct {
	At                time.Time       `json:"at"`
	PID               int             `json:"pid"`
	ProcessBirthToken string          `json:"process_birth_token"`
	Catalog           json.RawMessage `json:"catalog"`
}

type startupTransitionCheckout struct {
	ID          string `json:"checkout_id"`
	Incarnation string `json:"incarnation"`
	Root        string `json:"root_path"`
	Transition  string `json:"transition"`
	Live        *bool  `json:"coordinator_live"`
}

type startupTransitionCatalog struct {
	Families []struct {
		Prefix    string                      `json:"primary_repo_prefix"`
		ID        string                      `json:"family_id"`
		Checkouts []startupTransitionCheckout `json:"checkouts"`
	} `json:"families"`
}

func analyzeStartupTransitionCensus(e *startupTransitionEvidence) Metric {
	unknown := func(reason string) Metric { return unavailable("count", reason) }
	if e == nil {
		return unknown("no native transition membership/registration evidence")
	}
	if e.InvalidReason != "" {
		return unknown(e.InvalidReason)
	}
	if e.PrimaryRoot == "" || e.Prefix == "" || e.FamilyID == "" || e.PID <= 0 || e.ProcessBirthToken == "" || e.BootStarted.IsZero() || e.BaselineAt.IsZero() || e.BaselineAt.Before(e.BootStarted) {
		return unknown("unbound native fixture/boot identity")
	}
	var registration struct {
		Root   string    `json:"repo_path"`
		Prefix string    `json:"repo"`
		State  string    `json:"state"`
		Since  time.Time `json:"repo_state_since"`
	}
	if len(e.Registration) > 64<<10 || json.Unmarshal(e.Registration, &registration) != nil || registration.State != "ready" || registration.Root != e.PrimaryRoot || registration.Prefix != e.Prefix || registration.Since.IsZero() || registration.Since.Before(e.BootStarted) || e.RegistrationPID != e.PID || e.RegistrationBirth != e.ProcessBirthToken {
		return unknown("missing/mismatched native repo_state_since registration anchor")
	}
	if e.BaselineAt.After(registration.Since) {
		return unknown("no exact membership captured before native registration")
	}
	if len(e.Observations) == 0 || len(e.Observations) > 64 || len(e.BaselineCatalog) > 8<<20 {
		return unknown("missing or oversized transition observations")
	}
	var baseline startupTransitionCatalog
	if json.Unmarshal(e.BaselineCatalog, &baseline) != nil {
		return unknown("invalid baseline catalog")
	}
	members := make(map[string]startupTransitionCheckout)
	roots := make(map[string]bool)
	families := 0
	primarySeen := false
	baselineIDs := make(map[string]bool)
	for _, family := range baseline.Families {
		if family.ID != e.FamilyID {
			continue
		}
		families++
		if family.Prefix != e.Prefix {
			return unknown("baseline primary prefix differs")
		}
		for _, row := range family.Checkouts {
			if row.ID == "" || row.Incarnation == "" || row.Root == "" || baselineIDs[row.ID] {
				return unknown("invalid/duplicate baseline catalog identity")
			}
			baselineIDs[row.ID] = true
			primarySeen = primarySeen || row.Root == e.PrimaryRoot
			if row.Transition == "" {
				continue
			}
			if row.ID == "" || row.Incarnation == "" || row.Root == "" || roots[row.Root] {
				return unknown("incomplete/duplicate native transition member")
			}
			if _, exists := members[row.ID]; exists {
				return unknown("duplicate native transition checkout ID")
			}
			members[row.ID], roots[row.Root] = row, true
		}
	}
	if families != 1 || !primarySeen || len(members) != 24 {
		return unknown("baseline does not identify exactly24 transition members")
	}
	live := make(map[string]bool)
	deadline := registration.Since.Add(startupCoordinatorWindow)
	bytes := len(e.BaselineCatalog) + len(e.Registration)
	for _, observation := range e.Observations {
		bytes += len(observation.Catalog)
		if bytes > 8<<20 || observation.PID != e.PID || observation.ProcessBirthToken != e.ProcessBirthToken || observation.At.Before(registration.Since) || observation.At.After(deadline) {
			return unknown("foreign/late/unbounded transition observation")
		}
		var catalog startupTransitionCatalog
		if json.Unmarshal(observation.Catalog, &catalog) != nil {
			return unknown("invalid transition catalog observation")
		}
		seen := make(map[string]bool)
		catalogIDs := make(map[string]bool)
		for _, family := range catalog.Families {
			for _, row := range family.Checkouts {
				if row.ID == "" || row.Incarnation == "" || row.Root == "" || catalogIDs[row.ID] {
					return unknown("invalid/duplicate observed catalog identity")
				}
				catalogIDs[row.ID] = true
				expected, wanted := members[row.ID]
				if !wanted {
					continue
				}
				if seen[row.ID] || family.ID != e.FamilyID || row.Incarnation != expected.Incarnation || row.Root != expected.Root || row.Live == nil {
					return unknown("foreign/duplicate/mismatched transition member")
				}
				seen[row.ID] = true
				live[row.ID] = live[row.ID] || *row.Live
			}
		}
		if len(seen) != len(members) {
			return unknown("transition observation omits expected members")
		}
	}
	count := 0
	for _, observed := range live {
		if observed {
			count++
		}
	}
	result := measured(float64(count), "count")
	if count != 24 {
		result.Reason = fmt.Sprintf("only%d of24 exact members witnessed live within2s", count)
	}
	return result
}
