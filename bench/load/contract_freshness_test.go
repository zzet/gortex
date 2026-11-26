package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func contractFreshnessCorpus() Corpus {
	c := sampleCorpus()
	c.EditAnchor, c.EditTemplate = "package sample\n", "\nfunc {{marker}}() {}\n"
	return c
}

// contractFreshnessStub answers the witness's two public operations. The
// listed contracts gain the witness identity only once fresh reports true for
// the time since that witness's edit was applied.
type contractFreshnessStub struct {
	mu        sync.Mutex
	plan      contractFreshnessPlan
	fresh     func(sinceEdit time.Duration) bool
	present   bool     // the identity is already listed before any edit
	refuse    bool     // refuse every edit
	warm      []string // answers to the first reads: incomplete, pending or invalid
	ackDelay  time.Duration
	edited    map[string]time.Time
	edits     int
	reads     int
	afterEdit int
	readers   int
	t         *testing.T
}

func (s *contractFreshnessStub) handle(frame stubFrame) (any, *rpcError) {
	args, _ := frame.Params["arguments"].(map[string]any)
	name, _ := frame.Params["name"].(string)
	if name == "edit" {
		s.mu.Lock()
		delay := s.ackDelay
		s.mu.Unlock()
		time.Sleep(delay) // the edit applies and acknowledges after delay
	}
	if name != "edit" && name != "analyze" {
		s.mu.Lock()
		s.readers++
		s.mu.Unlock()
		time.Sleep(2 * time.Millisecond) // a bounded reader population
		return stubContent(map[string]any{"ok": true}), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "edit" {
		s.edits++
		replacement, _ := args["replacement"].(string)
		if s.refuse {
			return nil, &rpcError{Code: -32602, Message: "invalid_argument: edit refused"}
		}
		for ordinal := range s.plan.Edits {
			witness := s.plan.witness(ordinal)
			declaration := `http.HandleFunc("GET ` + witness.Route + `", ` + witness.Symbol + `)`
			if args["operation"] == "file" && args["match"] == "package sample\n" && strings.HasPrefix(replacement, "package sample\n") && strings.Contains(replacement, declaration) {
				s.edited[witness.Identity] = time.Now()
				return stubContent(map[string]any{"status": "applied"}), nil
			}
		}
		return nil, &rpcError{Code: -32602, Message: "invalid_argument: unexpected contract edit"}
	}
	if args["kind"] != "contracts" || args["action"] != "list" || args["type"] != "http" || args["role"] != "provider" || args["repo"] != "sample" {
		return nil, &rpcError{Code: -32602, Message: "invalid_argument: unexpected contract read"}
	}
	s.reads++
	if len(s.warm) > 0 {
		answer := s.warm[0]
		s.warm = s.warm[1:]
		switch answer {
		case "incomplete":
			return contractToolError("required_capability_incomplete", "graph.contracts did not become complete within this request: context deadline exceeded; retry after 5s"), nil
		case "invalid":
			return contractToolError("invalid_argument", "unknown analyze kind"), nil
		case "pending":
			return stubContent(map[string]any{"by_repo": map[string]any{}, "truncated": false, "contract_analysis": map[string]any{"state": "building", "fresh": false}}), nil
		}
	}
	rows := []any{map[string]any{"id": "http::GET::/existing", "type": "http", "role": "provider"}}
	body := map[string]any{"truncated": false}
	pending := false
	for ordinal := range s.plan.Edits {
		identity := s.plan.witness(ordinal).Identity
		at, edited := s.edited[identity]
		switch {
		case s.present:
			rows = append(rows, map[string]any{"id": identity, "type": "http", "role": "provider"})
		case edited && s.fresh(time.Since(at)):
			rows = append(rows, map[string]any{"id": identity, "type": "http", "role": "provider"})
		case edited:
			pending = true
		}
	}
	if len(s.edited) > 0 {
		s.afterEdit++
		body["contract_analysis"] = map[string]any{"capability": "graph.contracts", "state": "complete", "fresh": true}
		if pending {
			body["contract_analysis"] = map[string]any{"capability": "graph.contracts", "state": "building", "fresh": false, "reason": "contract_analysis_pending"}
		}
	}
	body["total"] = len(rows)
	body["by_repo"] = map[string]any{"sample": map[string]any{"contracts": map[string]any{"http": rows}, "total": len(rows)}}
	return stubContent(body), nil
}

func contractToolError(code, message string) any {
	body, _ := json.Marshal(map[string]any{"error_code": code, "message": message})
	return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": string(body)}}}
}

func startContractFreshnessStub(t *testing.T, plan contractFreshnessPlan, fresh func(time.Duration) bool) (*contractFreshnessStub, string) {
	stub := &contractFreshnessStub{plan: plan, fresh: fresh, edited: make(map[string]time.Time), t: t}
	return stub, startStub(t, stub.handle)
}

func TestContractFreshnessWitnessRecordsFirstFreshAnswer(t *testing.T) {
	const lag, ackDelay = 120 * time.Millisecond, 60 * time.Millisecond
	plan := newContractFreshnessPlan(1)
	plan.ReadIntervalMS = 10
	stub, socket := startContractFreshnessStub(t, plan, func(since time.Duration) bool { return since >= lag })
	stub.mu.Lock()
	stub.ackDelay = ackDelay
	stub.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	observation, rows := observeContractFreshness(ctx, time.Second, plan, client, privateRepo{Corpus: contractFreshnessCorpus(), Prefix: "sample"}, 3, 0)
	witness := plan.witness(0)
	if witness.Identity != "http::GET::/gortex-contract-witness/GortexContractWitnessE0" || observation.Identity != witness.Identity || observation.Symbol != "GortexContractWitnessE0" {
		t.Fatalf("witness identity: %+v %+v", witness, observation)
	}
	if observation.Outcome != "fresh" || observation.Baseline != "absent" || observation.FreshMS.Value == nil || observation.FreshAfterAckMS.Value == nil || observation.FreshNotBeforeMS.Value == nil || observation.FreshReadMS.Value == nil || observation.AcknowledgedMS.Value == nil {
		t.Fatalf("fresh answer not recorded: %+v", observation)
	}
	freshMS, afterAck, notBefore := *observation.FreshMS.Value, *observation.FreshAfterAckMS.Value, *observation.FreshNotBeforeMS.Value
	if freshMS < float64(lag/time.Millisecond) || afterAck <= 0 || afterAck > freshMS || notBefore <= 0 || notBefore >= freshMS || *observation.FreshReadMS.Value > freshMS {
		t.Fatalf("fresh time %.1f ms (after ACK %.1f, not before %.1f) does not bracket the %v lag", freshMS, afterAck, notBefore, lag)
	}
	// The comparable figure runs from the edit's dispatch, so it includes
	// the time the edit took to acknowledge; a figure anchored at the ACK
	// would not.
	if freshMS-afterAck < float64(ackDelay/time.Millisecond) {
		t.Fatalf("fresh time %.1f ms is only %.1f ms past the after-ACK time; the %v acknowledgement is missing from it", freshMS, freshMS-afterAck, ackDelay)
	}
	stub.mu.Lock()
	afterEdit := stub.afterEdit
	stub.mu.Unlock()
	if observation.Reads != afterEdit || observation.ReadOutcomes["fresh"] != 1 || observation.ReadOutcomes["absent_flagged"] != afterEdit-1 || afterEdit < 2 {
		t.Fatalf("read census %d %v, daemon saw %d reads after the edit", observation.Reads, observation.ReadOutcomes, afterEdit)
	}
	if !strings.Contains(string(observation.FreshReport), `"fresh":true`) {
		t.Fatalf("fresh read's own contract report not kept: %s", observation.FreshReport)
	}
	if len(rows) != afterEdit+2 || rows[0].Tool != "analyze.contracts" || rows[0].Role != "probe" || rows[1].Tool != "edit.file" || rows[1].Role != "workload" || rows[1].Agent != 3 || rows[len(rows)-1].Role != "probe" {
		t.Fatalf("attempt rows: %+v", rows)
	}
}

func TestContractFreshnessWitnessOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, baseline string
		present, refuse         bool
		edits                   int
	}{
		{name: "never fresh", outcome: "never_fresh", baseline: "absent", edits: 1},
		{name: "identity listed before its edit", outcome: "identity_preexisting", baseline: "present", present: true},
		{name: "edit refused", outcome: "edit_error", baseline: "absent", refuse: true, edits: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := newContractFreshnessPlan(1)
			plan.ReadIntervalMS = 10
			stub, socket := startContractFreshnessStub(t, plan, func(time.Duration) bool { return false })
			stub.present, stub.refuse = tc.present, tc.refuse
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := openMCP(ctx, socket, "")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			observation, _ := observeContractFreshness(ctx, 150*time.Millisecond, plan, client, privateRepo{Corpus: contractFreshnessCorpus(), Prefix: "sample"}, 0, 0)
			stub.mu.Lock()
			defer stub.mu.Unlock()
			if observation.Outcome != tc.outcome || observation.Baseline != tc.baseline || observation.FreshMS.Value != nil || stub.edits != tc.edits {
				t.Fatalf("outcome %+v after %d edits", observation, stub.edits)
			}
			if tc.outcome == "never_fresh" && (observation.ReadOutcomes["absent_flagged"] < 2 || observation.ReadOutcomes["absent_flagged"]+observation.ReadOutcomes["deadline"] != observation.Reads) {
				t.Fatalf("unfresh reads not counted: %+v", observation)
			}
		})
	}
}

func TestContractFreshnessReadClassification(t *testing.T) {
	const identity = "http::GET::/gortex-contract-witness/GortexContractWitnessE7"
	wrap := func(body string) json.RawMessage {
		data, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": body}}})
		return data
	}
	for _, tc := range []struct{ name, body, want string }{
		{"listed", `{"by_repo":{"r":{"contracts":{"http":[{"id":"` + identity + `","type":"http","role":"provider"}]}}},"truncated":false}`, "fresh"},
		{"listed while flagged", `{"by_repo":{"r":{"contracts":{"http":[{"id":"` + identity + `"}]}}},"contract_analysis":{"state":"building","fresh":false}}`, "fresh"},
		{"another witness", `{"by_repo":{"r":{"contracts":{"http":[{"id":"` + identity + `0"}]}}}}`, "absent"},
		{"only echoed", `{"message":"` + identity + `","symbol_id":"` + identity + `","by_repo":{}}`, "absent"},
		{"truncated", `{"by_repo":{},"truncated":true,"limit":1}`, "truncated"},
		{"next page", `{"by_repo":{},"next_cursor":"abc"}`, "truncated"},
		{"flagged stale", `{"by_repo":{},"contract_analysis":{"state":"complete","fresh":false}}`, "absent_flagged"},
		{"flagged building", `{"by_repo":{},"contract_analysis":{"state":"building"}}`, "absent_flagged"},
		{"unflagged", `{"by_repo":{},"truncated":false}`, "absent"},
		{"not JSON", `no contracts found`, "unparsed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := classifyContractRead(wrap(tc.body), identity); got != tc.want {
				t.Fatalf("classified %s as %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestContractFreshnessScenarioRunsWitnessBesideReaders(t *testing.T) {
	plan := newContractFreshnessPlan(2)
	stub, socket := startContractFreshnessStub(t, plan, func(since time.Duration) bool { return since >= 30*time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := &privateRun{Repos: []privateRepo{{Corpus: contractFreshnessCorpus(), Prefix: "sample"}}}
	var clients []*socketClient
	for range 2 {
		client, err := openMCP(ctx, socket, "")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		clients = append(clients, client)
	}
	o := runOptions{Profile: "saturation", Agents: 1, Editors: 1, ContractFreshnessEdits: 2, Duration: 15 * time.Second, Deadline: 5 * time.Second}
	phase := &Phase{Name: contractFreshnessScenario}
	started := time.Now()
	if err := measureContractFreshness(ctx, o, p, clients, phase); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	readerCalls := func() int {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		return stub.readers
	}
	report := phase.ContractFreshness
	if report == nil || len(report.Observations) != 2 || report.Outcomes["fresh"] != 2 || report.Fresh.Count != 2 || report.FreshAfterAck.Count != 2 || report.Acknowledged.Count != 2 || len(phase.Errors) != 0 {
		t.Fatalf("witness report %+v errors %v", report, phase.Errors)
	}
	census := make(map[string]int)
	for _, attempt := range phase.Attempts {
		census[attempt.Role+":"+attempt.Tool]++
		if attempt.Agent < o.Agents && attempt.Role == "workload" {
			census["reader"]++
		}
		if attempt.Tool == "edit.file" && attempt.Agent != o.Agents {
			t.Fatalf("witness edit attributed to agent %d", attempt.Agent)
		}
	}
	if census["workload:edit.file"] != 2 || census["probe:analyze.contracts"] < 4 || census["reader"] == 0 {
		t.Fatalf("attempt census %v", census)
	}
	// Readers share the witness's window and stop when it finishes, not at
	// --duration. Frames already sent may still land; none follow them.
	time.Sleep(20 * time.Millisecond)
	settled := readerCalls()
	time.Sleep(100 * time.Millisecond)
	if later := readerCalls(); elapsed >= o.Duration || settled == 0 || later != settled {
		t.Fatalf("readers outlived the witness: %v elapsed, %d then %d reader calls", elapsed, settled, later)
	}
}

func TestContractFreshnessRunOptionsAndPlan(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", contractFreshnessScenario}
	valid := append(append([]string(nil), base...), "--agents", "0", "--edit-agents", "1", "--worktrees", "0", "--contract-freshness-edits", "20")
	o, err := parseRun(valid)
	if err != nil || o.ContractFreshnessEdits != 20 {
		t.Fatalf("valid witness run refused: %+v %v", o, err)
	}
	for name, extra := range map[string][]string{
		"no edit count":     {"--agents", "0", "--edit-agents", "1", "--worktrees", "0"},
		"too many edits":    {"--agents", "0", "--edit-agents", "1", "--worktrees", "0", "--contract-freshness-edits", "201"},
		"two editors":       {"--agents", "0", "--edit-agents", "2", "--worktrees", "0", "--contract-freshness-edits", "20"},
		"default worktrees": {"--agents", "0", "--edit-agents", "1", "--contract-freshness-edits", "20"},
		"too many readers":  {"--agents", "17", "--edit-agents", "1", "--worktrees", "0", "--contract-freshness-edits", "20"},
	} {
		if _, err := parseRun(append(append([]string(nil), base...), extra...)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := parseRun([]string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", "read", "--contract-freshness-edits", "20"}); err == nil {
		t.Fatal("witness edit count accepted outside its scenario")
	}

	corpus := contractFreshnessCorpus()
	report, err := newRunReport(o, []Corpus{corpus}, strings.Repeat("b", 40), Host{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if plan := report.Workload.ContractFreshness; plan == nil || *plan != newContractFreshnessPlan(20) || report.Workload.LinkedWorktrees != 0 || report.Workload.EditAgents != 1 {
		t.Fatalf("workload identity: %+v", report.Workload)
	}
	names := make(map[string]request)
	for _, operation := range report.Workload.Operations {
		names[operation.name()] = operation
	}
	replacement, _ := names["edit.file"].Arguments["replacement"].(string)
	if len(report.Workload.Operations) != 2 || names["analyze.contracts"].Arguments["kind"] != "contracts" || !strings.Contains(replacement, `http.HandleFunc("GET /gortex-contract-witness/{{marker}}", {{marker}})`) {
		t.Fatalf("offered operations: %+v", report.Workload.Operations)
	}
	readers := o
	readers.Agents = 2
	if report, err := newRunReport(readers, []Corpus{corpus}, strings.Repeat("b", 40), Host{}, t.TempDir()); err != nil || len(report.Workload.Operations) <= 2 {
		t.Fatalf("reader operations missing from the offered plan: %v", err)
	}

	// Other scenarios keep their request plan hashes: the witness identity is
	// absent from their workload.
	read := o
	read.Scenario, read.ContractFreshnessEdits, read.Agents, read.Editors = "read", 0, 8, 0
	other, err := newRunReport(read, []Corpus{corpus}, strings.Repeat("b", 40), Host{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := json.Marshal(other.Workload); strings.Contains(string(data), "contract_freshness") {
		t.Fatalf("read workload carries the witness identity: %s", data)
	}

	for name, mutate := range map[string]func(*Corpus){
		"not Go":               func(c *Corpus) { c.File = "sample.py" },
		"no anchor":            func(c *Corpus) { c.EditAnchor = "" },
		"nested edit template": func(c *Corpus) { c.EditTemplate = "\n\t{{marker}}()\n" },
	} {
		broken := contractFreshnessCorpus()
		mutate(&broken)
		if err := validateContractFreshnessCorpus(broken); err == nil {
			t.Fatalf("%s fixture accepted", name)
		}
	}
	if err := validateContractFreshnessCorpus(corpus); err != nil {
		t.Fatal(err)
	}
}

// sampleContractFreshnessReport is a complete one-edit witness report whose
// edit read back fresh after freshMS.
func sampleContractFreshnessReport(t *testing.T, freshMS float64) Report {
	t.Helper()
	r := sampleReport(t)
	plan := newContractFreshnessPlan(1)
	corpus := contractFreshnessCorpus()
	operations, err := contractFreshnessOperations(corpus, runOptions{ContractFreshnessEdits: 1})
	if err != nil {
		t.Fatal(err)
	}
	r.Corpus = []Corpus{corpus}
	r.Workload.Scenario, r.Workload.Agents, r.Workload.EditAgents, r.Workload.ContractFreshness, r.Workload.Operations = contractFreshnessScenario, 0, 1, &plan, operations
	witness := &contractFreshnessReport{Observations: []ContractFreshnessObservation{{
		Identity: plan.witness(0).Identity, Symbol: plan.witness(0).Symbol, Baseline: "absent", Outcome: "fresh", Started: r.Started,
		AcknowledgedMS: measured(3, "ms"), FreshMS: measured(freshMS, "ms"), FreshAfterAckMS: measured(freshMS-3, "ms"), FreshNotBeforeMS: measured(0, "ms"), FreshReadMS: measured(2, "ms"),
		Reads: 2, ReadOutcomes: map[string]int{"absent": 1, "fresh": 1}, Acknowledgement: json.RawMessage(`{"status":"applied"}`),
	}}}
	witness.finish()
	r.Phases[0].Name, r.Phases[0].Operations, r.Phases[0].ContractFreshness = contractFreshnessScenario, operations, witness
	hash, err := hashJSON(r.Workload)
	if err != nil {
		t.Fatal(err)
	}
	r.RequestPlan = hash
	return r
}

func TestContractFreshnessReportValidation(t *testing.T) {
	r := sampleContractFreshnessReport(t, 40)
	path := t.TempDir() + "/report.json"
	if err := writeReport(path, r); err != nil {
		t.Fatal(err)
	}
	if got, err := readReport(path); err != nil || got.Phases[0].ContractFreshness == nil || got.Phases[0].ContractFreshness.Fresh.Count != 1 {
		t.Fatalf("witness report did not round-trip: %v", err)
	}
	for name, mutate := range map[string]func(*Report){
		"plan missing":        func(r *Report) { r.Workload.ContractFreshness = nil },
		"plan elsewhere":      func(r *Report) { r.Workload.Scenario = "edit" },
		"plan altered":        func(r *Report) { r.Workload.ContractFreshness.ReadIntervalMS = 50 },
		"report missing":      func(r *Report) { r.Phases[0].ContractFreshness = nil },
		"outcomes miscounted": func(r *Report) { r.Phases[0].ContractFreshness.Outcomes["fresh"] = 2 },
		"complete while stale": func(r *Report) {
			r.Phases[0].ContractFreshness.Observations[0].Outcome = "never_fresh"
			r.Phases[0].ContractFreshness.finish()
		},
	} {
		broken := sampleContractFreshnessReport(t, 40)
		mutate(&broken)
		if hash, err := hashJSON(broken.Workload); err == nil {
			broken.RequestPlan = hash
		}
		data, _ := json.Marshal(broken)
		if err := validateReport(data, broken); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestContractFreshnessDeltaComparesFreshTimes(t *testing.T) {
	outcomes := []string{"fresh", "never_fresh", "phase_censored", "edit_error", "baseline_error", "identity_preexisting"}
	before, after := sampleContractFreshnessReport(t, 40), sampleContractFreshnessReport(t, 50)
	for _, outcome := range outcomes {
		if _, ok := after.Phases[0].ContractFreshness.Outcomes[outcome]; !ok {
			t.Fatalf("witness report omits the %s count: %v", outcome, after.Phases[0].ContractFreshness.Outcomes)
		}
	}
	// A report that names only the outcomes it observed still gets every row.
	before.Phases[0].ContractFreshness.Outcomes = map[string]int{"fresh": 1}
	var output strings.Builder
	if err := printDelta(&output, before, after); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, row := range []string{"contract-freshness.contract_fresh.p95", "contract-freshness.contract_fresh_after_ack.p50", "contract-freshness.contract_acknowledged.p95"} {
		if !strings.Contains(text, row) {
			t.Fatalf("delta lacks %s:\n%s", row, text)
		}
	}
	rows := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			rows[fields[0]] = strings.Join(fields[1:], " ")
		}
	}
	for _, outcome := range outcomes {
		want := "0 edits 0 edits n/a"
		if outcome == "fresh" {
			want = "1 edits 1 edits +0.00%"
		}
		if got := rows["contract-freshness.contract_outcomes."+outcome]; got != want {
			t.Fatalf("contract_outcomes.%s row is %q, want %q:\n%s", outcome, got, want, text)
		}
	}
	worse := text[strings.Index(text, "more than 15% worse"):]
	if !strings.Contains(worse, "contract-freshness.contract_fresh.p95") || strings.Contains(worse, "contract_acknowledged") {
		t.Fatalf("freshness regression verdict wrong:\n%s", worse)
	}
}

func TestContractFreshnessWarmGateWaitsForACompleteAnswer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		warm  []string
		fails bool
		reads int
	}{
		{name: "first initialization", warm: []string{"incomplete", "incomplete", "pending"}, reads: 4},
		{name: "already complete", reads: 1},
		{name: "unsupported request", warm: []string{"invalid", "incomplete"}, fails: true, reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := newContractFreshnessPlan(1)
			plan.ReadIntervalMS = 5
			stub, socket := startContractFreshnessStub(t, plan, func(time.Duration) bool { return false })
			stub.warm = tc.warm
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := openMCP(ctx, socket, "")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			gate, rows, err := awaitContractAnswer(ctx, client, plan, privateRepo{Corpus: contractFreshnessCorpus(), Prefix: "sample"}, time.Second)
			var receipt struct {
				Reads     int            `json:"reads"`
				ElapsedMS float64        `json:"elapsed_ms"`
				Outcomes  map[string]int `json:"outcomes"`
			}
			if json.Unmarshal(gate, &receipt) != nil || (err != nil) != tc.fails || receipt.Reads != tc.reads || len(rows) != tc.reads || rows[0].Role != "warm" {
				t.Fatalf("gate %s rows %d err %v", gate, len(rows), err)
			}
			if !tc.fails && (receipt.Outcomes["absent"] != 1 || receipt.Outcomes["incomplete"]+receipt.Outcomes["absent_flagged"] != tc.reads-1) {
				t.Fatalf("gate outcomes %v", receipt.Outcomes)
			}
		})
	}
}
