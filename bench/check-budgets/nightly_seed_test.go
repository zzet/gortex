package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNightlySeedRejectsForeignAndUnsupportedContracts(t *testing.T) {
	c := fixtureLaneConfig()
	c.Lane = "nightly"
	c.HostClass = "reference_linux"
	valid := nightlyWorkloadSeed{Version: 1, Kind: "search-delay-v1", CandidateRevision: c.Builds["candidate"].Revision}
	for _, change := range []func(*laneConfig){
		func(c *laneConfig) { c.Lane = "smoke" },
		func(c *laneConfig) { c.NightlySeed.Version = 2 },
		func(c *laneConfig) { c.NightlySeed.Kind = "unknown" },
		func(c *laneConfig) { c.NightlySeed.CandidateRevision = strings.Repeat("f", 40) },
		func(c *laneConfig) { c.EditGateSeed = "ack-wait" },
	} {
		bad := c
		seed := valid
		bad.NightlySeed = &seed
		change(&bad)
		if err := validateNightlySeed(bad); err == nil {
			t.Fatal("unsupported or foreign seed accepted")
		}
	}
	c.NightlySeed = &valid
	if err := validateNightlySeed(c); err != nil {
		t.Fatal(err)
	}
}

func TestNightlySeedIsCandidateOnlyAndDoesNotShrinkMatrix(t *testing.T) {
	c := fixtureLaneConfig()
	c.Lane, c.HostClass = "nightly", "reference_linux"
	p := testPolicy()
	p.Budgets = []budget{{Scenario: "load-8", Metric: "read_p95", Mode: "block", HostClass: c.HostClass, Lane: []string{"nightly"}}}
	plain, err := makeLanePlan(p, c)
	if err != nil { t.Fatal(err) }
	c.NightlySeed = &nightlyWorkloadSeed{Version: 1, Kind: "search-delay-v1", CandidateRevision: c.Builds["candidate"].Revision}
	seeded, err := makeLanePlan(p, c)
	if err != nil { t.Fatal(err) }
	if len(seeded.Tasks) != len(plain.Tasks) || seeded.PolicySHA256 != plain.PolicySHA256 { t.Fatal("seed changed policy or task census") }
	for _, task := range seeded.Tasks {
		args := taskArgs(c, task, "manifest", "state", "report")
		hasSeed := strings.Contains(strings.Join(args, " "), "--nightly-workload-seed search-delay-v1")
		if hasSeed != (task.Role == "candidate" && task.Scenario == "load-8") { t.Fatalf("foreign seed route: %+v %v", task, args) }
	}
	var out bytes.Buffer
	item := nightlyEvidence{day: "2026-11-03", receipt: laneReceipt{Plan: seeded, BudgetPass: true, Workflow: workflowProvenance{Revision: c.Builds["candidate"].Revision, RunURL: "https://github.com/zzet/gortex/actions/runs/1", Attempt: 1, Event: "workflow_dispatch"}}}
	if historyVerdict([]nightlyEvidence{item}, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), []string{c.HostClass}, c.Builds["candidate"].Revision, &out) { t.Fatal("seeded run supplied GREEN credit") }
}

func TestNightlySeedConfigStrictDecode(t *testing.T) {
	// Unknown nested fields cannot become an undocumented injection mode.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"nightly_workload_seed":{"schema_version":1,"kind":"search-delay-v1","extra":true}}`), 0600); err != nil { t.Fatal(err) }
	if _, err := readLaneConfig(path); err == nil { t.Fatal("unknown seed field accepted") }
}
