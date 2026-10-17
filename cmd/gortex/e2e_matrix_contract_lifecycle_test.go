//go:build acceptance

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/viewmetrics"
)

var e2eContractBullets = []string{
	"cold contract answer", "route edit under comment and function saves", "route removal",
	"restart persisted attachment", "worktree contract isolation", "strict freshness policy",
	"four actors capacity two retry then success", "optional computed contract result",
	"four native checkout actors complete with isolated routes",
}

func e2eContractRows(binary string) []e2eLifecycleRow {
	return []e2eLifecycleRow{
		{Name: "cold_answer", Bullet: e2eContractBullets[0], Gates: []string{e2eLifecycleGateSnapshot}, Run: func(t *testing.T, r *e2eLifecycleRecorder) { e2eContractCold(t, r, binary) }},
		{Name: "route_edit_writers", Bullet: e2eContractBullets[1], Gates: []string{e2eLifecycleGateSnapshot, e2eLifecycleGateBounded}, Run: func(t *testing.T, r *e2eLifecycleRecorder) { e2eContractEdits(t, r, binary) }},
		{Name: "route_removal", Bullet: e2eContractBullets[2], Gates: []string{e2eLifecycleGateSnapshot}, Run: func(t *testing.T, r *e2eLifecycleRecorder) { e2eContractRemoval(t, r, binary) }},
		{Name: "persisted_restart", Bullet: e2eContractBullets[3], Gates: []string{e2eLifecycleGateStorage, e2eLifecycleGateNoop}, Run: func(t *testing.T, r *e2eLifecycleRecorder) { e2eContractRestart(t, r, binary) }},
		{Name: "worktree_isolation", Bullet: e2eContractBullets[4], Gates: []string{e2eLifecycleGateAuthority, e2eLifecycleGateSnapshot}, Run: func(t *testing.T, r *e2eLifecycleRecorder) { e2eContractWorktree(t, r, binary) }},
		{Name: "strict_freshness_policy", Bullet: e2eContractBullets[5], Gates: []string{e2eLifecycleGateAuthority, e2eLifecycleGateSnapshot}, Run: func(t *testing.T, r *e2eLifecycleRecorder) { e2eContractStrictPolicy(t, r, binary) }},
		{Name: "overload_four_capacity_two", Bullet: e2eContractBullets[6], Gates: []string{e2eLifecycleGateBounded}, Unexercised: "NATIVE-CAPACITY: shipped actor admission capacity is eight with no public override; GORTEX_CONTRACT_JOB_SLOTS changes slots only. TestContractCapacityTwoFourActorsMCPRetryAndSuccess separately exercises the real daemon integration at constructor QueueCapacity=2; this native row earns no capacity-two credit"},
		{Name: "optional_computed", Bullet: e2eContractBullets[7], Gates: []string{e2eLifecycleGateSnapshot}, Run: func(t *testing.T, r *e2eLifecycleRecorder) { e2eContractOptionalComputed(t, r, binary) }},
		{Name: "four_native_checkout_actors", Bullet: e2eContractBullets[8], Gates: []string{e2eLifecycleGateSnapshot, e2eLifecycleGateAuthority}, Run: func(t *testing.T, r *e2eLifecycleRecorder) { e2eContractFourNativeActors(t, r, binary) }},
	}
}

func TestE2EMatrixContractLifecycle(t *testing.T) {
	binary := e2eLifecycleRequireBinary(t)
	e2eLifecycleRunMatrix(t, "contract_lifecycle", e2eContractRows(binary), e2eContractBullets, e2eLifecycleArtifactDir(t))
}

// This helper self-test is ungated: it never starts a fixture or requires a
// binary. Named source gaps remain not_exercised in the native outcome table.
func TestE2EMatrixContractRowsCoverEveryBulletAndNameTheirGates(t *testing.T) {
	rows := e2eContractRows("/nonexistent/gortex")
	require.NoError(t, e2eLifecycleValidateRows("contract_lifecycle", rows, e2eContractBullets))
	require.Len(t, rows, len(e2eContractBullets))
	for _, row := range rows {
		if row.Name == "overload_four_capacity_two" {
			require.Nil(t, row.Run, "native default8 cannot claim a constructor-only capacity2 run")
			require.Contains(t, row.Unexercised, "TestContractCapacityTwoFourActorsMCPRetryAndSuccess")
		} else {
			require.NotNil(t, row.Run, "every supported native bullet needs a real positive body")
			require.Empty(t, row.Unexercised)
		}
		if row.Run == nil {
			out := e2eLifecycleRecordOutcome("contract_lifecycle", row, false, true, 0, nil, "")
			require.Equal(t, e2eLifecycleStatusNotExercised, out.Status)
			require.NotEmpty(t, out.Reason)
		}
	}
}

func e2eContractRoute(route string) string {
	return "package fixture\nimport \"net/http\"\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET " + route + "\", handle) }\nfunc handle(w http.ResponseWriter, r *http.Request) {}\n"
}

func e2eContractFixture(t *testing.T, binary string, linked bool, configure ...func(*issue767Fixture)) *issue767Fixture {
	t.Helper()
	f := newIssue767FixtureWithCorpus(t, binary, func(f *issue767Fixture) {
		f.write(filepath.Join(f.primary, ".gortex.yaml"), "semantic:\n  enabled: false\n")
		f.write(filepath.Join(f.primary, "go.mod"), "module example.test/contractnative\n\ngo 1.22\n")
		f.write(filepath.Join(f.primary, "marker.go"), "package fixture\nfunc ContractNativeMarker() int { return 1 }\n")
		f.write(filepath.Join(f.primary, "routes.go"), e2eContractRoute("/before"))
		for i := 0; i < 32; i++ {
			f.write(filepath.Join(f.primary, fmt.Sprintf("filler%02d.go", i)), fmt.Sprintf("package fixture\nfunc Filler%d() int { return %d }\n", i, i))
		}
		for _, option := range configure {
			option(f)
		}
	})
	f.env = append(f.env, "GORTEX_TOOLS=full", "GORTEX_ASYNC_CONTRACTS=1")
	if linked {
		f.git(f.primary, "worktree", "add", "-b", "contract-linked", f.linked)
	}
	f.start()
	f.awaitSymbolIn(f.primary, "ContractNativeMarker", filepath.Join(f.primary, "marker.go"), 2*time.Minute)
	return f
}

func e2eContractPayload(value any) map[string]any {
	switch v := value.(type) {
	case map[string]any:
		if v["isError"] == true || v["error"] != nil {
			return nil
		}
		if _, ok := v["contract_analysis"].(map[string]any); ok {
			if _, ok := v["by_repo"].(map[string]any); ok {
				return v
			}
		}
		for _, key := range e2eViewsSortedKeys(v) {
			if found := e2eContractPayload(v[key]); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range v {
			if found := e2eContractPayload(child); found != nil {
				return found
			}
		}
	case string:
		if child, ok := e2eViewsEmbeddedJSON(v); ok {
			return e2eContractPayload(child)
		}
	}
	return nil
}

func TestE2EMatrixContractPayloadRefusesErrorEnvelopes(t *testing.T) {
	body := `{"contract_analysis":{"state":"complete"},"by_repo":{}}`
	wrapped := map[string]any{"result": map[string]any{"content": []any{map[string]any{"text": body}}}}
	require.NotNil(t, e2eContractPayload(wrapped))
	for _, rejected := range []any{
		map[string]any{"error": map[string]any{"code": -1}, "result": wrapped},
		map[string]any{"isError": true, "content": []any{map[string]any{"text": body}}},
		map[string]any{"contract_analysis": map[string]any{"state": "complete"}},
	} {
		require.Nil(t, e2eContractPayload(rejected))
	}
}

func e2eContractAwait(f *issue767Fixture, root string, present, absent string) {
	f.t.Helper()
	f.await("complete contracts with exact route replacement", 2*time.Minute, func() bool {
		args, err := json.Marshal(map[string]any{"action": "list", "type": "http", "limit": 1000, "format": "json", "wait_deadline": time.Now().Add(10 * time.Second).Format(time.RFC3339Nano)})
		if err != nil {
			f.t.Fatal(err)
		}
		out, err := f.tryCommand(20*time.Second, root, "call", "contracts", "--index", root, "--json", string(args), "--format", "json")
		if err != nil {
			return false
		}
		var value any
		if json.Unmarshal(out, &value) != nil {
			return false
		}
		payload := e2eContractPayload(value)
		if payload == nil {
			return false
		}
		status := payload["contract_analysis"].(map[string]any)
		if status["state"] != "complete" {
			return false
		}
		ids := map[string]bool{}
		for _, group := range payload["by_repo"].(map[string]any) {
			repo, ok := group.(map[string]any)
			if !ok {
				return false
			}
			kinds, ok := repo["contracts"].(map[string]any)
			if !ok {
				return false
			}
			for _, raw := range kinds {
				rows, ok := raw.([]any)
				if !ok {
					return false
				}
				for _, raw := range rows {
					record, ok := raw.(map[string]any)
					if !ok {
						return false
					}
					id, ok := record["id"].(string)
					if !ok {
						return false
					}
					ids[id] = true
				}
			}
		}
		return (present == "" || ids["http::GET::"+present]) && (absent == "" || !ids["http::GET::"+absent])
	})
}

// Derive the singleton primary key from its physical accepted witness, rather
// than reading an arbitrary latest attachment. Linked view authority is tested
// through its public selected root instead of reconstructing its ancestry here.
func e2eContractPrimaryPayload(f *issue767Fixture) int64 {
	f.t.Helper()
	db := f.openReadOnly()
	defer db.Close()
	state := graph.ContractInputState{RepoPrefix: issue767FixturePrefix}
	require.NoError(f.t, db.QueryRowContext(f.t.Context(), `SELECT input_version,input_fingerprint,accepted FROM generation_contract_input_state WHERE view_gen=0 AND repo_prefix=? AND checkout_id=''`, issue767FixturePrefix).Scan(&state.InputVersion, &state.InputFingerprint, &state.Accepted))
	require.True(f.t, state.Accepted)
	composed, err := graph.ComposeContractInputState(issue767FixturePrefix, "", []graph.ContractInputWitness{{GenerationID: 0, State: state, Found: true}})
	require.NoError(f.t, err)
	var payload int64
	require.NoError(f.t, db.QueryRowContext(f.t.Context(), `SELECT a.payload_generation FROM contract_attachments a JOIN view_generations v ON v.generation_id=a.payload_generation WHERE a.repo_prefix=? AND a.checkout_id='' AND a.input_version=? AND a.input_fingerprint=? AND v.state='ready'`, issue767FixturePrefix, composed.InputVersion, composed.InputFingerprint).Scan(&payload))
	require.Positive(f.t, payload)
	return payload
}

func e2eContractCold(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	f := e2eContractFixture(t, binary, false)
	e2eContractAwait(f, f.primary, "/before", "")
	rec.note("cold public complete route /before; accepted primary payload %d; D fresh wire policy remains a separate named gap", e2eContractPrimaryPayload(f))
}

func e2eContractEdits(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	f := e2eContractFixture(t, binary, false)
	e2eContractAwait(f, f.primary, "/before", "")
	previous := "/before"
	for i, suffix := range []string{"\n// unrelated comment save\n", "\nfunc AddedDuringRouteEdit() {}\n"} {
		route := fmt.Sprintf("/edit-%d", i)
		f.write(filepath.Join(f.primary, "routes.go"), e2eContractRoute(route))
		filler := "package fixture\nfunc Filler0() int { return 0 }\n" + suffix
		f.write(filepath.Join(f.primary, "filler00.go"), filler)
		db := f.openReadOnly()
		f.await("writer source accepted through the native watcher", time.Minute, func() bool {
			var fingerprint string
			var accepted bool
			err := db.QueryRowContext(t.Context(), `SELECT source_fingerprint,accepted FROM generation_contract_boundary_receipt WHERE view_gen=0 AND repo_prefix=? AND checkout_id='' AND file_path=?`, issue767FixturePrefix, issue767FixturePrefix+"/filler00.go").Scan(&fingerprint, &accepted)
			return err == nil && accepted && fingerprint == fmt.Sprintf("%x", sha256.Sum256([]byte(filler)))
		})
		require.NoError(t, db.Close())
		e2eContractAwait(f, f.primary, route, previous)
		e2eContractPrimaryPayload(f)
		rec.note("route %s replaces %s under actual watcher-delivered %s save", route, previous, []string{"comment", "function"}[i])
		previous = route
	}
}

func e2eContractRemoval(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	f := e2eContractFixture(t, binary, false)
	e2eContractAwait(f, f.primary, "/before", "")
	require.NoError(t, os.Remove(filepath.Join(f.primary, "routes.go")))
	e2eContractAwait(f, f.primary, "", "/before")
	rec.note("real route existed before removal and disappeared from a complete public answer; current payload %d", e2eContractPrimaryPayload(f))
}

func e2eContractRestart(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	f := e2eContractFixture(t, binary, false)
	e2eContractAwait(f, f.primary, "/before", "")
	before := e2eContractPrimaryPayload(f)
	f.stop()
	warmLog, err := os.ReadFile(f.logPath())
	require.NoError(t, err)
	warmStarted, warmFinished, _, err := e2eContractClosedJobs(warmLog)
	require.NoError(t, err)
	require.Positive(t, len(warmStarted), "the first fresh fixture must positively exercise native job logging")
	require.Positive(t, len(warmFinished)) // initial provisional and final modes may differ
	f.start()
	e2eContractAwait(f, f.primary, "/before", "")
	require.Equal(t, before, e2eContractPrimaryPayload(f), "restart must reuse the persisted exact attachment")
	e := &e2eLifecycleEnv{t: t, f: f}
	status := e.status()
	require.NotNil(t, status.Views, "missing view telemetry is unavailable, never zero jobs")
	require.Positive(t, status.Views.Generations["ready"], "the live census must observe actual ready payloads")
	counters := status.Views.Counters
	for _, mode := range []string{"cold", "delta"} {
		require.Zero(t, e2eLifecycleCounterWith(counters, "views_contract_analysis_jobs_total", "mode="+mode), "unchanged restart must not analyze contracts again")
	}
	// The public status has no contract actor/queue census. The owned stop
	// joins this process and its coordinator; read its complete log AFTER
	// that barrier, rather than turning a momentary zero into a drain claim.
	f.stop()
	closedLog, err := os.ReadFile(f.logPath())
	require.NoError(t, err)
	started, finished, provisionalFill, err := e2eContractClosedJobs(closedLog)
	require.NoError(t, err)
	for _, mode := range []string{"cold", "delta"} {
		require.Zero(t, finished[mode], "including a late job that finished during owned shutdown")
	}
	rec.note("persisted payload %d reused, zero observed cold/delta jobs through owned shutdown; starts=%v finishes=%v provisional-starts-completed-as-fill=%v; no ordinary scheduler-drained or absent queued future-work claim", before, started, finished, provisionalFill)
}

// Logs belong to one joined private process with the default single job slot.
// Finished records have no job ID, so ordered start/finish accounting is used;
// malformed, unmatched or unfinished records refuse closure evidence.
func e2eContractClosedJobs(body []byte) (map[string]int, map[string]int, map[string]int, error) {
	started, finished, provisionalFill := map[string]int{}, map[string]int{}, map[string]int{}
	active, structured := 0, 0
	startMode := ""
	for _, line := range strings.Split(string(body), "\n") {
		var row struct {
			Message string `json:"msg"`
			Mode    string `json:"mode"`
			Job     string `json:"job"`
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			if strings.Contains(line, "contract analysis job") {
				return nil, nil, nil, fmt.Errorf("malformed native contract job log: %w", err)
			}
			continue
		}
		if row.Message == "" {
			continue
		}
		structured++
		if row.Message != "contract analysis job started" && row.Message != "contract analysis job" {
			if strings.HasPrefix(row.Message, "contract analysis job") {
				return nil, nil, nil, fmt.Errorf("unclosed native job event %q", row.Message)
			}
			continue
		}
		if row.Mode != "fill" && row.Mode != "cold" && row.Mode != "delta" {
			return nil, nil, nil, fmt.Errorf("unknown native job mode %q", row.Mode)
		}
		if row.Message == "contract analysis job started" {
			if row.Job == "" || active != 0 {
				return nil, nil, nil, fmt.Errorf("missing identity or overlapping native job start")
			}
			active++
			startMode = row.Mode
			started[row.Mode]++
		} else {
			if active != 1 || row.Outcome != viewmetrics.ContractJobPublished && row.Outcome != viewmetrics.ContractJobFillContinued {
				return nil, nil, nil, fmt.Errorf("unmatched or unsuccessful native job finish: %q", row.Outcome)
			}
			if row.Mode == "fill" && startMode != "fill" {
				// Start classification precedes the actual worker plan; only
				// its matched successful final mode establishes analysis work.
				provisionalFill[startMode]++
			}
			active--
			startMode = ""
			finished[row.Mode]++
		}
	}
	if structured == 0 || active != 0 {
		return nil, nil, nil, fmt.Errorf("native job log missing or unfinished: structured=%d active=%d", structured, active)
	}
	return started, finished, provisionalFill, nil
}

func TestE2EMatrixContractRestartLogClosureRejectsMissingAndIncompleteEvidence(t *testing.T) {
	start := `{"msg":"contract analysis job started","mode":"fill","job":"fixture"}`
	finish := `{"msg":"contract analysis job","mode":"fill","outcome":"published"}`
	started, finished, provisionalFill, err := e2eContractClosedJobs([]byte(start + "\n" + finish))
	require.NoError(t, err)
	require.Equal(t, map[string]int{"fill": 1}, started)
	require.Equal(t, started, finished)
	require.Empty(t, provisionalFill)
	_, finished, provisionalFill, err = e2eContractClosedJobs([]byte(strings.Replace(start, `"fill"`, `"cold"`, 1) + "\n" + finish))
	require.NoError(t, err, "a provisional cold start can successfully complete extraction-only fill")
	require.Equal(t, map[string]int{"fill": 1}, finished)
	require.Equal(t, map[string]int{"cold": 1}, provisionalFill)
	for _, mode := range []string{"cold", "delta"} {
		started, finished, _, err := e2eContractClosedJobs([]byte(strings.ReplaceAll(start+"\n"+finish, `"fill"`, `"`+mode+`"`)))
		require.NoError(t, err)
		require.Equal(t, 1, started[mode], "late forbidden starts must remain observable")
		require.Equal(t, 1, finished[mode], "late forbidden finishes must remain observable")
	}
	for _, body := range []string{"", "null", "{}", start, finish, start + "\n" + start, start + "\n" + `{"msg":"contract analysis job panicked"}`, `{"msg":"contract analysis job started"`, strings.Replace(start, `"fill"`, `"unknown"`, 1), start + "\n" + strings.Replace(finish, `"published"`, `"failed"`, 1), start + "\n" + strings.Replace(finish, `"published"`, `"panic"`, 1), start + "\n" + strings.Replace(finish, `"published"`, `"unclassified"`, 1)} {
		_, _, _, err := e2eContractClosedJobs([]byte(body))
		require.Error(t, err)
	}
}

func e2eContractWorktree(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	f := e2eContractFixture(t, binary, true)
	f.awaitSymbolAs(f.linked, "ContractNativeMarker", filepath.Join(f.linked, "marker.go"), 2*time.Minute, issue767AsAutomaticWorktree)
	e2eContractAwait(f, f.primary, "/before", "")
	e2eContractAwait(f, f.linked, "/before", "")
	f.write(filepath.Join(f.primary, "routes.go"), e2eContractRoute("/primary-only"))
	e2eContractAwait(f, f.primary, "/primary-only", "/before")
	e2eContractAwait(f, f.linked, "/before", "/primary-only")
	rec.note("automatic linked root still serves its committed route while primary serves its changed route")
}

func e2eContractHasRoute(payload map[string]any, route string) bool {
	groups, _ := payload["by_repo"].(map[string]any)
	for _, raw := range groups {
		group, _ := raw.(map[string]any)
		kinds, _ := group["contracts"].(map[string]any)
		for _, raw := range kinds {
			rows, _ := raw.([]any)
			for _, raw := range rows {
				row, _ := raw.(map[string]any)
				if row["id"] == "http::GET::"+route {
					return true
				}
			}
		}
	}
	return false
}

func e2eContractPolicyArgs(root string) map[string]any {
	return map[string]any{"action": "list", "type": "http", "limit": 1000, "format": "json",
		"view": map[string]any{"kind": "worktree", "path": root}, "require_fresh": true,
		"wait_deadline": time.Now().Add(10 * time.Second).Format(time.RFC3339Nano)}
}

// Poll only supported exact working-copy routes. Every failed response is
// retained in the fixture log, never counted as a positive or hidden by an
// IsError envelope containing an older successful payload.
func e2eContractAwaitPolicy(t *testing.T, f *issue767Fixture, h *e2eViewsHTTP, root, present, absent string, exact bool) map[string]any {
	t.Helper()
	db := f.openReadOnly()
	defer db.Close()
	var checkoutID string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT checkout_id FROM checkouts WHERE root_path=?`, root).Scan(&checkoutID))
	require.NotEmpty(t, checkoutID)
	var result map[string]any
	var last any
	f.await("current exact contract policy and edited content", 2*time.Minute, func() bool {
		args := e2eContractPolicyArgs(root)
		args["require_exact"] = exact
		value, err := h.tool(t.Context(), "contracts", args)
		last = value
		if err != nil {
			t.Logf("contract policy intermediate transport refusal: %v", err)
			return false
		}
		payload := e2eContractPayload(value)
		freshness := e2eViewsFreshness(value)
		if payload == nil || freshness == nil {
			t.Logf("contract policy intermediate response: %s", e2eViewsJSONTail(value))
			return false
		}
		analysis, _ := payload["contract_analysis"].(map[string]any)
		if analysis["state"] != "complete" || analysis["fresh"] != true || freshness["fresh"] != true || freshness["exact"] != true || freshness["checkout_id"] != checkoutID || !e2eContractHasRoute(payload, present) || absent != "" && e2eContractHasRoute(payload, absent) {
			t.Logf("contract policy intermediate answer: %s", e2eViewsJSONTail(value))
			return false
		}
		result = payload
		return true
	})
	require.NotNil(t, result, "%v", last)
	return result
}

func e2eContractStrictPolicy(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	addr := e2eViewsFreeAddr(t)
	f := e2eContractFixture(t, binary, true, func(f *issue767Fixture) { f.binary = e2eViewsHTTPWrapper(t, f.root, binary, addr) })
	f.awaitSymbolAs(f.linked, "ContractNativeMarker", filepath.Join(f.linked, "marker.go"), 2*time.Minute, issue767AsAutomaticWorktree)
	h := e2eViewsNewHTTP(t, addr, f.linked)
	require.NoError(t, h.initialize(t.Context(), time.Minute))
	e2eContractAwaitPolicy(t, f, h, f.linked, "/before", "", true)
	f.write(filepath.Join(f.linked, "routes.go"), e2eContractRoute("/strict-current"))
	e2eContractAwaitPolicy(t, f, h, f.linked, "/strict-current", "/before", false)
	e2eContractAwaitPolicy(t, f, h, f.linked, "/strict-current", "/before", true)

	// A public unsaved buffer is a deterministic authority mismatch. It is
	// not a timing race against a very fast worker, or a forged receipt.
	buffer := e2eContractRoute("/buffer-not-accepted")
	pushed, _, err := e2eViewsToolOrFacade(t.Context(), h, "overlay_push", "overlay", "push", map[string]any{"path": filepath.Join(f.linked, "routes.go"), "content": buffer})
	require.NoError(t, err)
	require.NotEqual(t, true, pushed.(map[string]any)["isError"])
	var dropOnce sync.Once
	drop := func() {
		dropOnce.Do(func() {
			value, _, err := e2eViewsToolOrFacade(context.Background(), h, "overlay_drop", "overlay", "drop", map[string]any{})
			require.NoError(t, err)
			require.NotEqual(t, true, value.(map[string]any)["isError"])
		})
	}
	t.Cleanup(drop)
	args := e2eContractPolicyArgs(f.linked)
	args["require_exact"] = true
	refused, err := h.tool(t.Context(), "contracts", args)
	require.NoError(t, err)
	encoded, err := json.Marshal(refused)
	require.NoError(t, err)
	require.Nil(t, e2eContractPayload(refused), "an unaccepted buffer cannot earn an exact contract answer")
	require.Contains(t, string(encoded), "required_capability_incomplete")
	require.Contains(t, string(encoded), "unpublished editor buffers")
	onDisk, err := os.ReadFile(filepath.Join(f.linked, "routes.go"))
	require.NoError(t, err)
	require.NotEqual(t, buffer, string(onDisk))
	drop()
	e2eContractAwaitPolicy(t, f, h, f.linked, "/strict-current", "/buffer-not-accepted", true)
	e2eContractAwait(f, f.primary, "/before", "/strict-current")
	rec.note("native linked default and require_exact twins returned complete+fresh current route, replaced route absent; exact refused actual unsaved buffer and succeeded after drop; primary isolated. Queue/deadline predecessor twins are separate integration evidence")
}

// Find an actual computed object through the existing JSON-in-text transport.
// Reject an error wrapper before considering its nested content.
func e2eContractComputedObject(value any, field string) map[string]any {
	switch value := value.(type) {
	case map[string]any:
		if value["isError"] == true || value["error"] != nil {
			return nil
		}
		if _, ok := value[field]; ok {
			return value
		}
		for _, key := range e2eViewsSortedKeys(value) {
			if body := e2eContractComputedObject(value[key], field); body != nil {
				return body
			}
		}
	case []any:
		for _, child := range value {
			if body := e2eContractComputedObject(child, field); body != nil {
				return body
			}
		}
	case string:
		if child, ok := e2eViewsEmbeddedJSON(value); ok {
			return e2eContractComputedObject(child, field)
		}
	}
	return nil
}

func e2eContractOptionalComputed(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	addr := e2eViewsFreeAddr(t)
	f := e2eContractFixture(t, binary, false, func(f *issue767Fixture) {
		f.binary = e2eViewsHTTPWrapper(t, f.root, binary, addr)
		f.write(filepath.Join(f.primary, "routes.go"), "package fixture\nimport \"net/http\"\ntype Reply struct { Name string `json:\"name\"` }\nfunc register(mux *http.ServeMux) { mux.HandleFunc(\"GET /before\", handle) }\nfunc handle(w http.ResponseWriter,r *http.Request) { response := Reply{Name:\"ok\"}; WriteJSON(w,200,response) }\nfunc WriteJSON(w http.ResponseWriter,code int,body any) {}\n")
	})
	e2eContractAwait(f, f.primary, "/before", "")
	db := f.openReadOnly()
	defer db.Close()
	var typeID string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT id FROM nodes WHERE view_gen=0 AND repo_prefix=? AND name='Reply' AND kind=?`, issue767FixturePrefix, string(graph.KindType)).Scan(&typeID))
	require.NotEmpty(t, typeID)
	h := e2eViewsNewHTTP(t, addr, f.primary)
	require.NoError(t, h.initialize(t.Context(), time.Minute))
	var computed map[string]any
	f.await("optional impact computed from the published response type", time.Minute, func() bool {
		value, err := h.tool(t.Context(), "explain_change_impact", map[string]any{"ids": typeID, "format": "json"})
		if err != nil {
			t.Logf("optional intermediate transport refusal: %v", err)
			return false
		}
		computed = e2eContractComputedObject(value, "contract_impact")
		if computed == nil {
			t.Logf("optional intermediate answer: %s", e2eViewsJSONTail(value))
			return false
		}
		status, _ := computed["contract_analysis"].(map[string]any)
		return status["state"] == "complete"
	})
	impact := computed["contract_impact"].(map[string]any)
	entries := impact["affected"].([]any)
	require.NotEmpty(t, entries)
	matched := false
	for _, raw := range entries {
		entry := raw.(map[string]any)
		if entry["contract_id"] == "http::GET::/before" && entry["type_id"] == typeID && entry["position"] == "response" && entry["role"] == "provider" {
			matched = true
		}
	}
	require.True(t, matched, "actual computed provider/response dependency must name the real type and route: %v", entries)
	status := computed["contract_analysis"].(map[string]any)
	require.Equal(t, "graph.contracts", status["capability"])
	require.IsType(t, true, status["fresh"])
	// A core-only name search must not pretend it consulted contracts.
	value, err := h.tool(t.Context(), "search_symbols", map[string]any{"query": "ContractNativeMarker", "format": "json", "limit": 10})
	require.NoError(t, err)
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	require.NotEqual(t, true, value.(map[string]any)["isError"])
	require.True(t, issue767JSONSource(value, "ContractNativeMarker", filepath.Join(f.primary, "marker.go")), "core-only search must return the actual marker node")
	require.NotContains(t, string(encoded), "contract_analysis")
	rec.note("native optional impact computed nonempty HTTP provider/response dependency for actual %s, consulted complete rider present; core-only search omitted rider. Primary/base fresh value is reported as %v, not relabeled true", typeID, status["fresh"])
}

func e2eContractFourNativeActors(t *testing.T, rec *e2eLifecycleRecorder, binary string) {
	addr := e2eViewsFreeAddr(t)
	f := e2eContractFixture(t, binary, true, func(f *issue767Fixture) { f.binary = e2eViewsHTTPWrapper(t, f.root, binary, addr) })
	roots := []string{f.linked}
	for i := 1; i < 4; i++ {
		root := filepath.Join(f.root, fmt.Sprintf("contract-actor-%d", i))
		f.git(f.primary, "worktree", "add", "-b", fmt.Sprintf("contract-actor-%d", i), root)
		roots = append(roots, root)
	}
	handles := make([]*e2eViewsHTTP, len(roots))
	for i, root := range roots {
		f.awaitSymbolAs(root, "ContractNativeMarker", filepath.Join(root, "marker.go"), 2*time.Minute, issue767AsAutomaticWorktree)
		handles[i] = e2eViewsNewHTTP(t, addr, root)
		require.NoError(t, handles[i].initialize(t.Context(), time.Minute))
		e2eContractAwaitPolicy(t, f, handles[i], root, "/before", "", true)
	}
	for i, root := range roots {
		f.write(filepath.Join(root, "routes.go"), e2eContractRoute(fmt.Sprintf("/actor-%d", i)))
	}
	type answer struct {
		index int
		value any
		err   error
	}
	done := make(chan answer, len(roots))
	for i, root := range roots {
		go func(i int, root string) {
			args := e2eContractPolicyArgs(root)
			args["require_exact"] = true
			value, err := handles[i].tool(t.Context(), "contracts", args)
			done <- answer{i, value, err}
		}(i, root)
	}
	for range roots {
		answer := <-done
		t.Logf("native actor %d first concurrent response error=%v body=%s", answer.index, answer.err, e2eViewsJSONTail(answer.value))
	}
	db := f.openReadOnly()
	defer db.Close()
	routes := e2eViewsRoutes(t, t.Context(), db)
	actors := map[string]bool{}
	for i, root := range roots {
		payload := e2eContractAwaitPolicy(t, f, handles[i], root, fmt.Sprintf("/actor-%d", i), "/before", true)
		for j := range roots {
			if j != i {
				require.False(t, e2eContractHasRoute(payload, fmt.Sprintf("/actor-%d", j)), "another checkout's route leaked into actor %d", i)
			}
		}
		identity := routes[filepath.Clean(root)].CheckoutID
		require.NotEmpty(t, identity)
		require.False(t, actors[identity])
		actors[identity] = true
		rec.note("native checkout actor=%s root=%s route=/actor-%d completed exact+fresh after concurrent requests", identity, root, i)
	}
	require.Len(t, actors, 4)
	for i := range roots {
		e2eContractAwait(f, f.primary, "/before", fmt.Sprintf("/actor-%d", i))
	}
	rec.note("four distinct native checkout actors completed with isolated current route payloads under shipped default actor capacity8; no capacity2, queue-full or retry claim")
}
