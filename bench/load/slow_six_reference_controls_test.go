//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func slowSixSyntheticPlan() (slowSixPlan, slowSixAuthority, []Corpus) {
	h := strings.Repeat("a", 64)
	base := filepath.Join(os.TempDir(), "slow-six-synthetic")
	p := slowSixPlan{Protocol: slowSixProtocol, Ready: true, Repetition: 1, Seed: 1, Binary: filepath.Join(base, "product"), Manifest: filepath.Join(base, "manifest"), HostReceipt: filepath.Join(base, "host"), Authority: filepath.Join(base, "authority"), Scratch: filepath.Join(base, "scratch"), Output: filepath.Join(base, "output"), Bindings: map[string]string{"product_sha256": h, "collector_sha256": h, "fixture_sha256": h, "host_receipt_sha256": h, "topology_authority_sha256": h}}
	var repos []Corpus
	for i := range 6 {
		repos = append(repos, Corpus{Label: fmt.Sprintf("repo%d", i), Source: filepath.Join(base, fmt.Sprintf("source%d", i)), Commit: strings.Repeat("b", 40), File: "file.go", Symbol: "file.go::Symbol", Query: "Symbol", ProposedSignature: "func Symbol()"})
	}
	a := slowSixAuthority{Issuer: "user", Purpose: "#1001 criterion6 six-repository inputs", ManifestSHA: h, Repositories: repos, NativeConfigs: map[string]slowSixNativeConfig{}}
	for _, repo := range repos {
		a.NativeConfigs[repo.Label] = slowSixNativeConfig{Absent: true}
		a.OriginalSources = append(a.OriginalSources, repo.Source)
	}
	a.GlobalNativeConfig = slowSixNativeConfig{Path: filepath.Join(base, "global.yaml"), SHA256: h}
	return p, a, repos
}
func TestSlowSixExactAuthorizedPopulation(t *testing.T) {
	p, a, repos := slowSixSyntheticPlan()
	if err := p.validateTopology(a, repos); err != nil {
		t.Fatalf("exact authorized six-repository population unsupported: %v", err)
	}
}
func TestSlowSixRejectsMissingForeignOrCollapsedPopulation(t *testing.T) {
	for _, kind := range []string{"five", "seven", "foreign", "duplicate", "unauthorized", "wrong-manifest", "wrong-seed"} {
		t.Run(kind, func(t *testing.T) {
			p, a, repos := slowSixSyntheticPlan()
			switch kind {
			case "five":
				repos = repos[:5]
			case "seven":
				repos = append(repos, repos[0])
			case "foreign":
				repos = append([]Corpus(nil), repos...)
				repos[0].Source = "/foreign"
			case "duplicate":
				repos = append([]Corpus(nil), repos...)
				repos[1] = repos[0]
			case "unauthorized":
				a.Issuer = "historical1013"
			case "wrong-manifest":
				a.ManifestSHA = strings.Repeat("c", 64)
			case "wrong-seed":
				p.Seed = 2
			}
			if err := p.validateTopology(a, repos); err == nil {
				t.Fatal("accepted unsupported input authority/topology")
			}
		})
	}
}

func TestSlowSixPhaseBoundaryDoesNotCancelAdmittedCall(t *testing.T) {
	stop := make(chan struct{})
	entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var n atomic.Int64
	run, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	go func() {
		done <- slowSixPersistent(run, stop, time.Now().Add(time.Hour), func(ctx context.Context, step int) error { n.Add(1); close(entered); <-finish; return ctx.Err() })
	}()
	<-entered
	close(stop)
	select {
	case <-done:
		t.Fatal("phase boundary cancelled/abandoned admitted slowcall")
	default:
	}
	close(finish)
	if err := <-done; err != nil || n.Load() != 1 {
		t.Fatalf("context/serialoffer changed: %v %d", err, n.Load())
	}
}
func TestSlowSixOriginalAlternatingPlanIsNotNestedBurst(t *testing.T) {
	p := slowSixRequests()
	if len(p) != 2 || p[0].name() != "analyze.dead_code" || p[1].name() != "explore.outline" {
		t.Fatalf("wrong actual slowplan: %+v", p)
	}
}

func slowSixSyntheticSamples() []slowSixSample {
	var rows []slowSixSample
	for i := range 50 {
		raw := json.RawMessage(`{"mcp_dispatch":{"lanes":{"slow":{"in_flight":1,"cancelled_held":1}}}}`)
		rows = append(rows, slowSixSample{StartedNS: int64(i) * int64(200*time.Millisecond), FinishedNS: int64(i)*int64(200*time.Millisecond) + 1, Raw: raw})
	}
	return rows
}
func TestSlowSixActualStatusOracleRejectsMissingHeldCoverageAndErrors(t *testing.T) {
	if err := slowSixSampleOracle(slowSixSyntheticSamples(), int64(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"missing", "three-held", "coverage", "gap", "errors"} {
		t.Run(kind, func(t *testing.T) {
			s := slowSixSyntheticSamples()
			switch kind {
			case "missing":
				for i := range s {
					s[i].Raw = json.RawMessage(`{}`)
				}
			case "three-held":
				s[0].Raw = json.RawMessage(`{"mcp_dispatch":{"lanes":{"slow":{"in_flight":2,"cancelled_held":1}}}}`)
			case "coverage":
				s = s[:35]
			case "gap":
				s = append(s[:10], s[25:]...)
			case "errors":
				s[0].Error = "retained status failure"
			}
			if slowSixSampleOracle(s, int64(10*time.Second)) == nil {
				t.Fatal("missing/failed producer evidence credited")
			}
		})
	}
}
func TestSlowSixFastRatioRequiresEachRealDistribution(t *testing.T) {
	a := map[string]Distribution{"read": distribution([]float64{10, 10, 10}, "ms")}
	b := map[string]Distribution{"read": distribution([]float64{15, 15, 15}, "ms")}
	if err := slowSixFastRatios(a, b, []string{"read"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []map[string]Distribution{{}, {"read": distribution(nil, "ms")}, {"read": distribution([]float64{16}, "ms")}} {
		if slowSixFastRatios(a, v, []string{"read"}) == nil {
			t.Fatal("unproved fast ratio accepted")
		}
	}
}

func slowSixSyntheticPressure() ([]slowSixWireRow, []string) {
	ids := []string{"session0", "session1", "session2"}
	var rows []slowSixWireRow
	for slot, id := range ids {
		for step := range 2 {
			start := int64(1 + step*20)
			wire := &fairnessNoisy50Wire{Role: "slow", SessionID: id, DaemonInstance: "instance", Slot: slot, Step: step, RPCID: int64(step + 1), WriteFinishedNS: start, ReplyNS: start + 10}
			rows = append(rows, slowSixWireRow{Phase: "treatment", SessionID: id, Instance: "instance", Attempt: Attempt{Role: "slow", Tool: slowSixRequests()[(slot+step)%2].name(), Outcome: "success"}, Wire: wire})
		}
	}
	return rows, ids
}
func TestSlowSixPressureRequiresActualThreeSerialAlternatingCallers(t *testing.T) {
	rows, ids := slowSixSyntheticPressure()
	if err := slowSixPressure(rows, ids, "instance"); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"missing", "foreign", "nested", "no-reply", "wrong-tool", "no-success", "after-window"} {
		t.Run(kind, func(t *testing.T) {
			rows, ids := slowSixSyntheticPressure()
			switch kind {
			case "missing":
				rows = rows[:4]
			case "foreign":
				rows[0].SessionID = "foreign"
			case "nested":
				rows[1].Wire.WriteFinishedNS = 2
			case "no-reply":
				for _, row := range rows {
					row.Wire.ReplyNS = 0
				}
			case "wrong-tool":
				rows[0].Attempt.Tool = "explore.context"
			case "no-success":
				for i := range rows {
					rows[i].Attempt.Outcome = "busy"
				}
			case "after-window":
				for _, row := range rows {
					row.Wire.WriteFinishedNS += int64(121 * time.Second)
					row.Wire.ReplyNS += int64(121 * time.Second)
				}
			}
			if slowSixPressure(rows, ids, "instance") == nil {
				t.Fatal("unproved slow pressure credited")
			}
		})
	}
}

func TestSlowSixMissingFromBothFastArmsIsNotCredit(t *testing.T) {
	a := map[string]Distribution{"read": distribution([]float64{10}, "ms")}
	if slowSixFastRatios(a, a, []string{"read", "summary"}) == nil {
		t.Fatal("missing expected operation accepted in both arms")
	}
}
func TestSlowSixExpiredWindowNeverOffersDespiteLaggingTimer(t *testing.T) {
	var n int
	err := slowSixPersistent(t.Context(), make(chan struct{}), time.Now().Add(-time.Second), func(context.Context, int) error { n++; return nil })
	if err != nil || n != 0 {
		t.Fatalf("expired absolutewindow offered: %v %d", err, n)
	}
}

func TestSlowSixPostWindowSamplesCannotCreditCoverage(t *testing.T) {
	rows := slowSixSyntheticSamples()
	for i := range rows {
		rows[i].FinishedNS = int64(11 * time.Second)
	}
	if slowSixSampleOracle(rows, int64(10*time.Second)) == nil {
		t.Fatal("postwindow producer snapshots credited measured coverage")
	}
}

func TestSlowSixForcedClosedIsRunFailure(t *testing.T) {
	if slowSixStopOutcome(true, nil) == nil {
		t.Fatal("forced closed stop claimed successful run")
	}
	if slowSixStopOutcome(false, nil) != nil {
		t.Fatal("normal stop rejected")
	}
	sentinel := errors.New("unknown stop")
	if !errors.Is(slowSixStopOutcome(true, sentinel), sentinel) || !errors.Is(slowSixStopOutcome(false, sentinel), sentinel) {
		t.Fatal("unknown stop error discarded")
	}
}

func TestSlowSixOriginalGlobalProfilePreservesOnlySelectedNonPathSetting(t *testing.T) {
	_, a, _ := slowSixSyntheticPlan()
	raw := "embedding:\n  enabled: false\nrepos:\n"
	for _, p := range a.OriginalSources {
		raw += "  - path: " + p + "\n"
	}
	if err := slowSixGlobalProfile([]byte(raw), a.OriginalSources); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(raw, "enabled: false", "enabled: true", 1), strings.Replace(raw, "source0", "foreign", 1), raw + "other: true\n", "repos: []\n"} {
		if slowSixGlobalProfile([]byte(bad), a.OriginalSources) == nil {
			t.Fatal("changed original profile accepted")
		}
	}
}
