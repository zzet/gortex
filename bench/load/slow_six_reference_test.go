//go:build acceptance

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"
)

const slowSixProtocol = "six-repository-slow3-v1"

type slowSixAuthority struct {
	Issuer             string                         `json:"issuer"`
	Purpose            string                         `json:"purpose"`
	ManifestSHA        string                         `json:"manifest_sha256"`
	Repositories       []Corpus                       `json:"repositories"`
	NativeConfigs      map[string]slowSixNativeConfig `json:"native_configs"`
	GlobalNativeConfig slowSixNativeConfig            `json:"global_native_config"`
	OriginalSources    []string                       `json:"original_sources"`
}
type slowSixNativeConfig struct {
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Absent bool   `json:"absent"`
}
type slowSixPlan struct {
	Protocol    string            `json:"protocol"`
	Ready       bool              `json:"ready"`
	Repetition  int               `json:"repetition"`
	Seed        uint64            `json:"seed"`
	Binary      string            `json:"binary"`
	Manifest    string            `json:"manifest"`
	HostReceipt string            `json:"host_receipt"`
	Authority   string            `json:"topology_authority"`
	Scratch     string            `json:"scratch"`
	Output      string            `json:"output"`
	Bindings    map[string]string `json:"bindings"`
}

func (p slowSixPlan) validateTopology(a slowSixAuthority, corpora []Corpus) error {
	if !p.Ready || p.Protocol != slowSixProtocol || p.Repetition < 1 || p.Repetition > 2 || p.Seed != uint64(p.Repetition) {
		return errors.New("unbound six-repository repetition/seed")
	}
	for _, k := range []string{"product_sha256", "collector_sha256", "fixture_sha256", "host_receipt_sha256", "topology_authority_sha256"} {
		if len(p.Bindings[k]) != 64 {
			return fmt.Errorf("unbound %s", k)
		}
		if _, err := hex.DecodeString(p.Bindings[k]); err != nil {
			return err
		}
	}
	for _, v := range []string{p.Binary, p.Manifest, p.HostReceipt, p.Authority, p.Scratch, p.Output} {
		if !filepath.IsAbs(v) {
			return errors.New("absolute owned/input paths required")
		}
	}
	if a.Issuer != "user" || a.Purpose != "#1001 criterion6 six-repository inputs" || a.ManifestSHA != p.Bindings["fixture_sha256"] || len(corpora) != 6 || !reflect.DeepEqual(a.Repositories, corpora) {
		return errors.New("exact authorized original six-repository manifest unavailable")
	}
	if a.GlobalNativeConfig.Absent || !filepath.IsAbs(a.GlobalNativeConfig.Path) || len(a.GlobalNativeConfig.SHA256) != 64 || len(a.OriginalSources) != 6 {
		return errors.New("original global native profile binding required")
	}
	if len(a.NativeConfigs) != 6 {
		return errors.New("six explicit native configuration bindings required")
	}
	seenOriginal := map[string]bool{}
	for _, v := range a.OriginalSources {
		if !filepath.IsAbs(v) || seenOriginal[v] {
			return errors.New("original source topology is not six distinct roots")
		}
		seenOriginal[v] = true
	}
	seenSource, seenLabel := map[string]bool{}, map[string]bool{}
	for _, c := range corpora {
		cfg, ok := a.NativeConfigs[c.Label]
		if !ok || cfg.Absent && (cfg.Path != "" || cfg.SHA256 != "") || !cfg.Absent && (!filepath.IsAbs(cfg.Path) || len(cfg.SHA256) != 64) {
			return errors.New("missing or contradictory native config binding")
		}
		if err := validateCorpus(c); err != nil {
			return err
		}
		if !filepath.IsAbs(c.Source) || seenSource[c.Source] || seenLabel[c.Label] {
			return errors.New("duplicate/foreign repository topology")
		}
		seenSource[c.Source], seenLabel[c.Label] = true, true
	}
	return nil
}

// stopOffers ends NEW offers only. An admitted slow call retains the RUN
// context and original callTool120s deadline, never the phase cancellation.
func slowSixPersistent(run context.Context, stopOffers <-chan struct{}, end time.Time, call func(context.Context, int) error) error {
	for step := 0; ; step++ {
		if !time.Now().Before(end) {
			return nil
		}
		select {
		case <-run.Done():
			return run.Err()
		case <-stopOffers:
			return nil
		default:
		}
		if err := call(run, step); err != nil {
			return err
		}
	}
}
func slowSixRequests() []request {
	return []request{
		{Tool: "analyze", Operation: "dead_code", Arguments: map[string]any{"kind": "dead_code"}},
		{Tool: "explore", Operation: "outline", Arguments: map[string]any{"operation": "outline"}},
	}
}

// Retain full public status and sample errors. No metric defaults to zero.
type slowSixSample struct {
	StartedNS  int64           `json:"started_ns"`
	FinishedNS int64           `json:"finished_ns"`
	Raw        json.RawMessage `json:"raw"`
	Error      string          `json:"error"`
}

func slowSixSampleStatus(ctx context.Context, socket string, origin time.Time) slowSixSample {
	start := time.Now()
	s := slowSixSample{StartedNS: start.Sub(origin).Nanoseconds()}
	sample, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	raw, err := control(sample, socket, "status", nil)
	s.Raw = raw
	s.FinishedNS = time.Since(origin).Nanoseconds()
	if err != nil {
		s.Error = err.Error()
	}
	return s
}

func slowSixSampleOracle(samples []slowSixSample, end int64) error {
	if end <= 0 || len(samples) == 0 {
		return errors.New("missing measured status population")
	}
	expected := end / int64(200*time.Millisecond)
	valid, bad, observed := int64(0), int64(0), int64(0)
	last := int64(0)
	for _, s := range samples {
		if s.StartedNS < 0 || s.StartedNS >= end {
			continue
		}
		observed++
		if s.StartedNS < last || s.StartedNS-last > int64(2*time.Second) {
			return errors.New("status sample gap/order exceeds contract")
		}
		last = s.StartedNS
		var root map[string]json.RawMessage
		var dispatch map[string]json.RawMessage
		var lanes map[string]json.RawMessage
		var slow map[string]json.RawMessage
		err := json.Unmarshal(s.Raw, &root)
		if err == nil {
			err = json.Unmarshal(root["mcp_dispatch"], &dispatch)
		}
		if err == nil {
			err = json.Unmarshal(dispatch["lanes"], &lanes)
		}
		if err == nil {
			err = json.Unmarshal(lanes["slow"], &slow)
		}
		if s.Error != "" || err != nil || s.FinishedNS < s.StartedNS || s.FinishedNS > end {
			bad++
			continue
		}
		var held [2]int
		complete := true
		for i, key := range []string{"in_flight", "cancelled_held"} {
			raw, ok := slow[key]
			if !ok || json.Unmarshal(raw, &held[i]) != nil || held[i] < 0 || string(raw) == "null" {
				complete = false
			}
		}
		if !complete {
			bad++
			continue
		}
		if held[0]+held[1] > 2 {
			return errors.New("observed slow in_flight plus cancelled_held exceeds two")
		}
		valid++
	}
	if observed == 0 || bad*100 > observed || valid*100 < expected*80 || end-last > int64(2*time.Second) {
		return errors.New("status errors/coverage/boundary gap unqualified")
	}
	return nil
}
func slowSixFastRatios(control, treatment map[string]Distribution, expected []string) error {
	if len(expected) == 0 || len(control) != len(expected) || len(treatment) != len(expected) {
		return errors.New("fast population missing/different")
	}
	seen := map[string]bool{}
	for _, key := range expected {
		if seen[key] {
			return errors.New("duplicate frozen fast operation")
		}
		seen[key] = true
		a, present := control[key]
		b, ok := treatment[key]
		if !present {
			return errors.New("expected fast operation missing")
		}
		if !ok || a.Count == 0 || b.Count == 0 || a.P95.Value == nil || b.P95.Value == nil {
			return errors.New("fast p95 unavailable")
		}
		if math.IsNaN(*a.P95.Value) || math.IsNaN(*b.P95.Value) || math.IsInf(*a.P95.Value, 0) || math.IsInf(*b.P95.Value, 0) || *a.P95.Value < 0 || *b.P95.Value < 0 {
			return errors.New("invalid measured p95")
		}
		if *b.P95.Value > 1.5**a.P95.Value {
			return fmt.Errorf("fast p95 ratio exceeds1.5: %s", key)
		}
	}
	return nil
}

type slowSixWireRow struct {
	Phase     string               `json:"phase"`
	SessionID string               `json:"session_id"`
	Instance  string               `json:"daemon_instance"`
	Attempt   Attempt              `json:"attempt"`
	Wire      *fairnessNoisy50Wire `json:"wire"`
}

func slowSixPressure(rows []slowSixWireRow, sessions []string, instance string) error {
	if len(sessions) != 3 || instance == "" {
		return errors.New("three actual slow sessions required")
	}
	ids := map[string]int{}
	for i, id := range sessions {
		if id == "" {
			return errors.New("missing actual slow session")
		}
		if _, ok := ids[id]; ok {
			return errors.New("shared slow session")
		}
		ids[id] = i
	}
	type event struct {
		at    int64
		delta int
	}
	var events []event
	per := map[string][]fairnessNoisy50Wire{}
	tools := map[string]map[string]bool{}
	rpc := map[string]bool{}
	for _, r := range rows {
		if r.Phase != "treatment" || r.Attempt.Role != "slow" {
			continue
		}
		w := r.Wire
		slot, ok := ids[r.SessionID]
		if !ok || w == nil || r.Instance != instance || w.SessionID != r.SessionID || w.DaemonInstance != instance || w.Role != "slow" || w.Slot != slot || w.Step < 0 || w.RPCID <= 0 || r.Attempt.Tool != slowSixRequests()[(slot+w.Step)%2].name() {
			return errors.New("foreign/missing/nonalternating slow wire identity")
		}
		// Only actual answered writes establish this pressure observation; other
		// raw records stay in the denominator and cannot manufacture concurrency.
		if w.WriteFinishedNS <= 0 || w.WriteFinishedNS >= int64(120*time.Second) || w.ReplyNS <= w.WriteFinishedNS {
			continue
		}
		key := fmt.Sprintf("%s/%d", w.SessionID, w.RPCID)
		if rpc[key] {
			return errors.New("duplicate slow RPC")
		}
		rpc[key] = true
		per[w.SessionID] = append(per[w.SessionID], *w)
		events = append(events, event{w.WriteFinishedNS, 1}, event{min(w.ReplyNS, int64(120*time.Second)), -1})
		if r.Attempt.Outcome == "success" {
			if tools[w.SessionID] == nil {
				tools[w.SessionID] = map[string]bool{}
			}
			tools[w.SessionID][r.Attempt.Tool] = true
		}
	}
	for _, id := range sessions {
		v := per[id]
		sort.Slice(v, func(i, j int) bool { return v[i].WriteFinishedNS < v[j].WriteFinishedNS })
		for i := 1; i < len(v); i++ {
			if v[i].WriteFinishedNS < v[i-1].ReplyNS {
				return errors.New("nested/concurrent slow offers on one caller")
			}
		}
		if !tools[id]["analyze.dead_code"] || !tools[id]["explore.outline"] {
			return errors.New("successful alternating slow treatment unproved")
		}
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].at != events[j].at {
			return events[i].at < events[j].at
		}
		return events[i].delta < events[j].delta
	})
	active, peak := 0, 0
	for _, e := range events {
		active += e.delta
		if active > peak {
			peak = active
		}
	}
	if peak != 3 {
		return errors.New("three actual overlapping slow callers unproved")
	}
	return nil
}
func slowSixAnalyze(path string, sessions, fastSessions []string, instance string, expected []string) (map[string]any, error) {
	allowedFast := map[string]bool{}
	for _, id := range fastSessions {
		if id == "" || allowedFast[id] {
			return nil, errors.New("eight distinct actual fast sessions required")
		}
		allowedFast[id] = true
	}
	if len(allowedFast) != 8 {
		return nil, errors.New("eight actual fast sessions required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var rows []slowSixWireRow
	values := map[string]map[string][]float64{"control": {}, "treatment": {}}
	counts := map[string]map[string]int{"control": {}, "treatment": {}}
	for {
		var r slowSixWireRow
		if e := dec.Decode(&r); e != nil {
			if errors.Is(e, io.EOF) {
				break
			}
			return nil, e
		}
		if r.Phase != "control" && r.Phase != "treatment" {
			return nil, errors.New("unknown measured phase")
		}
		rows = append(rows, r)
		counts[r.Phase][r.Attempt.Outcome]++
		if r.Attempt.Role == "fast" && (!allowedFast[r.SessionID] || r.Instance != instance || r.Wire != nil && (r.Wire.SessionID != r.SessionID || r.Wire.DaemonInstance != instance || r.Wire.Role != "fast")) {
			return nil, errors.New("foreign fast session/instance")
		}
		if r.Attempt.Role == "fast" && r.Attempt.Outcome == "success" && r.Wire != nil && r.Wire.ReplyNS > r.Wire.WriteFinishedNS && r.Wire.FinishedNS <= int64(120*time.Second) {
			values[r.Phase][r.Attempt.Tool] = append(values[r.Phase][r.Attempt.Tool], r.Attempt.LogicalMS)
		}
	}
	a, b := map[string]Distribution{}, map[string]Distribution{}
	for k, v := range values["control"] {
		a[k] = distribution(v, "ms")
	}
	for k, v := range values["treatment"] {
		b[k] = distribution(v, "ms")
	}
	err = errors.Join(slowSixPressure(rows, sessions, instance), slowSixFastRatios(a, b, expected))
	return map[string]any{"control_fast": a, "treatment_fast": b, "all_raw_record_outcomes_including_synthetic_terminals": counts, "boundary_policy": "successful completion within120s only in p95; ALL terminal errors/busy/drain/censored records retained separately; no censored latency invented", "error": fmt.Sprint(err)}, err
}

func slowSixStopOutcome(forced bool, stopErr error) error {
	if forced {
		return errors.Join(stopErr, errors.New("forced native stop: RUNFAIL even if physical closure becomes known"))
	}
	return stopErr
}

func slowSixGlobalProfile(raw []byte, sources []string) error {
	var original struct {
		Embedding *struct {
			Enabled *bool `yaml:"enabled"`
		} `yaml:"embedding"`
		Repos []struct {
			Path string `yaml:"path"`
		} `yaml:"repos"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&original); err != nil {
		return err
	}
	if original.Embedding == nil || original.Embedding.Enabled == nil || *original.Embedding.Enabled || len(original.Repos) != 6 || len(sources) != 6 {
		return errors.New("original embedding-disabled six-repo profile unavailable")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("global config has trailing document")
	}
	for i, repo := range original.Repos {
		if repo.Path != sources[i] {
			return errors.New("original global repository order/identity changed")
		}
	}
	return nil
}
