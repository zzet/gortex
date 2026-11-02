package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func startupTransitionFixture(t *testing.T) *startupTransitionEvidence {
	t.Helper()
	boot := time.Unix(1000, 0).UTC()
	rows := make([]map[string]any, 24)
	for i := range rows {
		rows[i] = map[string]any{"checkout_id": fmt.Sprintf("checkout-%d", i), "incarnation": fmt.Sprintf("inc-%d", i), "root_path": fmt.Sprintf("/owned/wt-%d", i), "transition": "promote", "coordinator_live": true}
	}
	rows = append(rows, map[string]any{"checkout_id": "primary", "incarnation": "primary-inc", "root_path": "/owned/primary", "coordinator_live": false})
	catalog, err := json.Marshal(map[string]any{"families": []any{map[string]any{"family_id": "family", "primary_repo_prefix": "app", "checkouts": rows}}})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := json.Marshal(map[string]any{"repo_path": "/owned/primary", "repo": "app", "state": "ready", "repo_state_since": boot.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return &startupTransitionEvidence{PrimaryRoot: "/owned/primary", Prefix: "app", FamilyID: "family", BootStarted: boot, PID: 123, ProcessBirthToken: "synthetic-native-birth", BaselineAt: boot.Add(time.Millisecond * 500), BaselineCatalog: catalog, Registration: registration, RegistrationPID: 123, RegistrationBirth: "synthetic-native-birth", Observations: []startupTransitionObservation{{At: boot.Add(2 * time.Second), PID: 123, ProcessBirthToken: "synthetic-native-birth", Catalog: append(json.RawMessage(nil), catalog...)}}}
}

func mutateStartupCatalog(t *testing.T, raw json.RawMessage, change func(*startupTransitionCatalog)) json.RawMessage {
	t.Helper()
	var catalog startupTransitionCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	change(&catalog)
	result, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStartupTransitionCensusRequiresExactNativeMembersAndAnchor(t *testing.T) {
	valid := startupTransitionFixture(t)
	if m := analyzeStartupTransitionCensus(valid); m.Value == nil || *m.Value != 24 {
		t.Fatalf("full native-shaped vector unavailable: %+v", m)
	}
	cases := map[string]func(*startupTransitionEvidence){
		"no anchor": func(e *startupTransitionEvidence) { e.Registration = nil },
		"announcement is not registration": func(e *startupTransitionEvidence) {
			e.Registration = []byte(`{"repo_path":"/owned/primary","repo":"app","state":"ready","ts":"1970-01-01T00:16:41Z"}`)
		},
		"foreign anchor":  func(e *startupTransitionEvidence) { e.RegistrationBirth = "other" },
		"foreign process": func(e *startupTransitionEvidence) { e.Observations[0].PID++ },
		"late":            func(e *startupTransitionEvidence) { e.Observations[0].At = e.BootStarted.Add(4 * time.Second) },
		"unknown time":    func(e *startupTransitionEvidence) { e.Observations[0].At = time.Time{} },
		"no observation":  func(e *startupTransitionEvidence) { e.Observations = nil },
		"aggregate only": func(e *startupTransitionEvidence) {
			e.Observations[0].Catalog = []byte(`{"views":{"coordinators":24}}`)
		},
		"missing member": func(e *startupTransitionEvidence) {
			e.Observations[0].Catalog = mutateStartupCatalog(t, e.Observations[0].Catalog, func(c *startupTransitionCatalog) { c.Families[0].Checkouts = c.Families[0].Checkouts[:23] })
		},
		"duplicate member": func(e *startupTransitionEvidence) {
			e.Observations[0].Catalog = mutateStartupCatalog(t, e.Observations[0].Catalog, func(c *startupTransitionCatalog) {
				c.Families[0].Checkouts = append(c.Families[0].Checkouts, c.Families[0].Checkouts[0])
			})
		},
		"foreign family": func(e *startupTransitionEvidence) {
			e.Observations[0].Catalog = mutateStartupCatalog(t, e.Observations[0].Catalog, func(c *startupTransitionCatalog) { c.Families[0].ID = "foreign" })
		},
		"rekeyed incarnation": func(e *startupTransitionEvidence) {
			e.Observations[0].Catalog = mutateStartupCatalog(t, e.Observations[0].Catalog, func(c *startupTransitionCatalog) { c.Families[0].Checkouts[0].Incarnation = "new" })
		},
		"foreign root": func(e *startupTransitionEvidence) {
			e.Observations[0].Catalog = mutateStartupCatalog(t, e.Observations[0].Catalog, func(c *startupTransitionCatalog) { c.Families[0].Checkouts[0].Root = "/foreign" })
		},
		"missing live field": func(e *startupTransitionEvidence) {
			e.Observations[0].Catalog = mutateStartupCatalog(t, e.Observations[0].Catalog, func(c *startupTransitionCatalog) { c.Families[0].Checkouts[0].Live = nil })
		},
		"not24 baseline": func(e *startupTransitionEvidence) {
			e.BaselineCatalog = mutateStartupCatalog(t, e.BaselineCatalog, func(c *startupTransitionCatalog) { c.Families[0].Checkouts = c.Families[0].Checkouts[:23] })
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := startupTransitionFixture(t)
			change(e)
			if m := analyzeStartupTransitionCensus(e); m.Value != nil {
				t.Fatalf("unproved census earned numeric credit: %+v", m)
			}
		})
	}
}

func TestStartupTransitionCensusKeepsObservedNonLiveAsFailureCount(t *testing.T) {
	e := startupTransitionFixture(t)
	e.Observations[0].Catalog = mutateStartupCatalog(t, e.Observations[0].Catalog, func(c *startupTransitionCatalog) { value := false; c.Families[0].Checkouts[0].Live = &value })
	if m := analyzeStartupTransitionCensus(e); m.Value == nil || *m.Value != 23 {
		t.Fatalf("actual false liveness must remain23, not missing or24: %+v", m)
	}
	metric := summarizeStartup(nil, []bootTiming{{Boot: 1, TransitionEvidence: e}}, "app", 220).metrics()["startup_coordinators_within_2s_min"]
	if metric.Value == nil || *metric.Value != 23 {
		t.Fatalf("summary lost verified member count: %+v", metric)
	}
}
