package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeProbeFrameReadsAdmissionAnswers(t *testing.T) {
	active := "When Gortex can serve the step: MUST use Gortex MCP.\n\nWorktree and branch routing:\n\n- A linked worktree is an automatic overlay; do not `track` it. Graph calls return `view_building` until discovery completes; retry them, no reconnect is needed. Do not run `gortex track`."
	for _, test := range []struct {
		name, frame, outcome string
		ok, advice           bool
		check                func(t *testing.T, s probeSample)
	}{
		{name: "loading refusal", outcome: "repo_loading",
			frame: `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"repository loading: app (resolving, 40%)","data":{"error_code":"repo_loading","retryable":true,"phase":"resolving","percent":40,"suggestion":"Gortex is still loading app after a restart; tools start working in place, no reconnect needed. Retry shortly. Do not change tracking configuration."}}}`,
			check: func(t *testing.T, s probeSample) {
				if s.Retryable == nil || !*s.Retryable || s.Phase != "resolving" || s.Percent == nil || *s.Percent != 40 {
					t.Fatalf("loading fields lost: %+v", s)
				}
			}},
		{name: "loading refusal without progress", outcome: "repo_loading(incomplete)",
			frame: `{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"repository loading","data":{"error_code":"repo_loading","retryable":true}}}`},
		{name: "untracked refusal advises tracking", outcome: "repo_not_tracked", advice: true,
			frame: "{\"jsonrpc\":\"2.0\",\"id\":2,\"error\":{\"code\":-32000,\"message\":\"repository not tracked: /w\",\"data\":{\"error_code\":\"repo_not_tracked\",\"path\":\"/w\",\"suggestion\":\"Run `gortex track /w` to include this repo in the shared graph.\"}}}"},
		{name: "unavailable is final", outcome: "repo_unavailable", advice: true,
			frame: "{\"jsonrpc\":\"2.0\",\"id\":2,\"error\":{\"code\":-32000,\"message\":\"repository unavailable\",\"data\":{\"error_code\":\"repo_unavailable\",\"retryable\":false,\"suggestion\":\"Gortex could not load /w after the restart. Run `gortex track /w` to index it again.\"}}}",
			check: func(t *testing.T, s probeSample) {
				if s.Retryable == nil || *s.Retryable {
					t.Fatalf("retryable=false must be kept: %+v", s)
				}
			}},
		{name: "busy without data", outcome: "rpc_-32002",
			frame: `{"jsonrpc":"2.0","id":2,"error":{"code":-32002,"message":"busy"}}`},
		{name: "loading index health answers", outcome: "ok:loading", ok: true,
			frame: `{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"{\"status\":\"loading\",\"phase\":\"starting\",\"percent\":0,\"repos\":[]}"}]}}`,
			check: func(t *testing.T, s probeSample) {
				if s.Phase != "starting" || s.Percent == nil || *s.Percent != 0 {
					t.Fatalf("a zero percent is an observation, not a missing one: %+v", s)
				}
			}},
		{name: "resource read", outcome: "ok:ok", ok: true,
			frame: `{"jsonrpc":"2.0","id":4,"result":{"contents":[{"uri":"gortex://index-health","text":"{\"status\":\"ok\"}"}]}}`},
		{name: "search results may quote the commands", outcome: "ok", ok: true,
			frame: "{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"{\\\"results\\\":[{\\\"doc\\\":\\\"run `gortex track .` first\\\"}]}\"}]}}"},
		{name: "tool error payload", outcome: "view_building",
			frame: `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[{"type":"text","text":"{\"error_code\":\"view_building\",\"message\":\"retry\"}"}]}}`},
		{name: "plain tool error", outcome: "tool_error",
			frame: `{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[{"type":"text","text":"tool not found"}]}}`},
		{name: "loading instructions", outcome: "loading", ok: true,
			frame: mustInitialize(t, "Gortex is LOADING app after a restart (starting, 0%). Graph tools return repo_loading (retryable) until it is ready; Do not re-track it or change tracking configuration: the repository is already configured.\n\n"+active)},
		{name: "admission pending instructions", outcome: "admission_pending", ok: true,
			frame: mustInitialize(t, "Gortex is LOADING graph admission after a restart (identity_resolution, 0%). Graph admission for this path is not yet established.")},
		{name: "unavailable instructions", outcome: "unavailable", ok: true, advice: true,
			frame: mustInitialize(t, "Gortex is connected but could not load /w after the restart: boom.\n\nGraph tools return repo_unavailable for it. To repair, run `gortex track /w` to index it again.")},
		{name: "untracked instructions", outcome: "untracked", ok: true, advice: true,
			frame: mustInitialize(t, "Gortex is connected but INACTIVE for this directory: \"/w\" is not covered by any tracked repository.\n\nTo activate: run `gortex track /w`, then retry graph calls; no reconnect is needed.")},
		{name: "active instructions forbid tracking", outcome: "active", ok: true,
			frame: mustInitialize(t, active)},
		{name: "not JSON", outcome: "invalid_response", frame: `daemon: serving`},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := decodeProbeFrame([]byte(test.frame))
			if got := outcomeKey(s); got != test.outcome || s.OK != test.ok || s.TrackAdvice != test.advice {
				t.Fatalf("outcome %q ok=%v advice=%v, want %q ok=%v advice=%v: %+v", got, s.OK, s.TrackAdvice, test.outcome, test.ok, test.advice, s)
			}
			if test.check != nil {
				test.check(t, s)
			}
		})
	}
}

func mustInitialize(t *testing.T, instructions string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"protocolVersion": "2024-11-05", "instructions": instructions}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestTrackAdviceIgnoresGuidanceAgainstIt(t *testing.T) {
	for text, want := range map[string]bool{
		"Run `gortex track /w` to include this repo":                          true,
		"the gortex daemon does not track /w — add it with `gortex track /w`": true,
		"run `gortex repos reconcile /w` and retry":                           true,
		"start it, then track this repo with gortex track /w":                 true,
		"Do not run `gortex track`.":                                          false,
		"Don't run gortex track here":                                         false,
		"never gortex repos reconcile a loading family":                       false,
		"remove it with `gortex untrack /w`":                                  false,
		"gortex tracks hundreds of repositories":                              false,
		"the repository is already tracked; retry shortly":                    false,
		"Do not run `gortex track`. To activate: run `gortex track /w`.":      true,
	} {
		if got := adviceIn(text); got != want {
			t.Errorf("adviceIn(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestCLIAndHookAnswers(t *testing.T) {
	for _, test := range []struct {
		exit int
		text string
		ok   bool
		code string
	}{
		{0, `{"results":[]}`, true, ""},
		{75, "the gortex daemon is still loading app after a restart (starting, 0%)", false, "repo_loading"},
		{1, "the gortex daemon does not track /w — add it with `gortex track /w`", false, "repo_not_tracked"},
		{1, "no gortex daemon is running — start it with `gortex daemon start --detach`", false, "no_daemon"},
		{2, "unknown flag", false, "exit_2"},
	} {
		if ok, code := classifyCLI(test.exit, test.text); ok != test.ok || code != test.code {
			t.Errorf("classifyCLI(%d, %q) = %v %q, want %v %q", test.exit, test.text, ok, code, test.ok, test.code)
		}
	}
	for output, want := range map[string]string{
		"": "allow",
		`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"use read(operation:\"file\")"}}`: "deny",
		`{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"app is still loading (repo_loading); the native Read is allowed"}}`: "allow",
		`{"decision":"block","reason":"indexed"}`: "deny",
		`not json`: "unparsed",
	} {
		if got, _ := decodeHookOutput([]byte(output)); got != want {
			t.Errorf("decodeHookOutput(%q) = %q, want %q", output, got, want)
		}
	}
}

// roundTrip must skip pushes the daemon interleaves before the response.
func TestProbeRoundTripSkipsNotifications(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		defer server.Close()
		request, err := bufio.NewReader(server).ReadString('\n')
		if err != nil || !strings.Contains(request, `"method":"tools/call"`) {
			return
		}
		_, _ = server.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/message","params":{"data":"loading"}}` + "\n" +
			`{"jsonrpc":"2.0","id":7,"method":"roots/list"}` + "\n" +
			`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{}"}]}}` + "\n"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &probeConn{conn: client, reader: bufio.NewReader(client)}
	frame, err := c.roundTrip(ctx, "tools/call", map[string]any{"name": "search_symbols"})
	if err != nil || !strings.Contains(string(frame), `"id":1`) {
		t.Fatalf("roundTrip = %s, %v", frame, err)
	}
}

func TestFirstOKUsesEarliestCompletionInsideTheBoot(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	samples := []probeSample{
		{CWD: "primary", Surface: "search_symbols", Started: t0, OK: true},                                        // previous boot
		{CWD: "primary", Surface: "search_symbols", Started: t0.Add(11 * time.Second), LatencyMS: 3000, OK: true}, // slow
		{CWD: "primary", Surface: "search_symbols", Started: t0.Add(12 * time.Second), LatencyMS: 100, OK: true},  // first to finish
		{CWD: "worktree", Surface: "search_symbols", Started: t0.Add(11 * time.Second), LatencyMS: 10, OK: true},  // other cwd
		{CWD: "primary", Surface: "search_symbols", Started: t0.Add(10500 * time.Millisecond), LatencyMS: 5, Code: "repo_loading"},
		{CWD: "primary", Surface: "search_symbols", Started: t0.Add(30 * time.Second), LatencyMS: 1, OK: true}, // next boot
	}
	first, ok := firstOK(samples, "search_symbols", "primary", t0.Add(10*time.Second), t0.Add(20*time.Second))
	if want := t0.Add(12100 * time.Millisecond); !ok || !first.Equal(want) {
		t.Fatalf("firstOK = %v %v, want %v", first, ok, want)
	}
	if _, ok := firstOK(samples, "cli_pipe", "primary", t0, time.Time{}); ok {
		t.Fatal("no sample of the surface is not a first OK")
	}
}

func TestSummarizeStartupCountsPerWindowAndRestart(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	coordinators := func(n int) *int { return &n }
	boots := []bootTiming{
		{Boot: 0, Started: at(0), SocketAt: at(2000), ReadyAt: map[string]time.Time{"app": at(5000)}, Stopping: at(20000), Coordinators: coordinators(3)},
		{Boot: 1, Started: at(21000), SocketAt: at(21500), ReadyAt: map[string]time.Time{"app": at(22000)}, Stopping: at(30000), Coordinators: coordinators(4)},
		{Boot: 2, Started: at(31000), SocketAt: at(31200), ReadyAt: map[string]time.Time{"app": at(31600)}, Coordinators: coordinators(2)},
	}
	yes, percent := true, 10
	samples := []probeSample{
		{CWD: "primary", Surface: "connect", Started: at(-500), Code: "no_socket"}, // before the first start
		{CWD: "primary", Surface: "cli_pipe", Started: at(1000), Code: "no_daemon", TrackAdvice: true},
		{CWD: "primary", Surface: "initialize", Started: at(3000), OK: true, Variant: "loading"},
		{CWD: "router", Surface: "initialize", Started: at(3000), OK: true, Variant: "untracked", TrackAdvice: true},
		{CWD: "primary", Surface: "search_symbols", Started: at(3000), Code: "repo_loading", Retryable: &yes, Phase: "starting", Percent: &percent},
		{CWD: "primary", Surface: "search_symbols", Started: at(4000), Code: "repo_loading", Retryable: &yes},
		{CWD: "worktree", Surface: "search_symbols", Started: at(3000), Code: "repo_not_tracked", TrackAdvice: true},
		{CWD: "primary", Surface: "workspace_index", Started: at(3000), OK: true, Status: "loading"},
		{CWD: "primary", Surface: "hook_read", Started: at(3000), OK: true, Variant: "allow"},
		{CWD: "primary", Surface: "hook_read", Started: at(6000), OK: true, Variant: "deny"},
		{CWD: "primary", Surface: "search_symbols", Started: at(5500), LatencyMS: 100, OK: true},
		{CWD: "primary", Surface: "search_symbols", Started: at(20500), Code: "no_socket"}, // between boots
		{CWD: "primary", Surface: "search_symbols", Started: at(23000), LatencyMS: 50, OK: true},
		{CWD: "primary", Surface: "search_symbols", Started: at(31500), LatencyMS: 50, OK: true}, // before the stamp
	}
	sum := summarizeStartup(samples, boots, "app", 24)
	if got := sum.Outcomes["search_symbols/primary"]["loading"]; got["repo_loading"] != 1 || got["repo_loading(incomplete)"] != 1 || got["ok"] != 1 {
		t.Fatalf("loading outcomes = %v", got)
	}
	if got := sum.Outcomes["search_symbols/primary"]["ready"]["ok"]; got != 2 {
		t.Fatalf("ready successes = %d", got)
	}
	if sum.Outcomes["connect/primary"]["no_socket"]["no_socket"] != 1 || sum.BetweenBoots != 1 || sum.TrackAdvice["cli_pipe/primary"]["no_socket"] != 1 || sum.TrackAdvice["search_symbols/worktree"]["loading"] != 1 {
		t.Fatalf("between=%d advice=%v", sum.BetweenBoots, sum.TrackAdvice)
	}
	if first := sum.Boots[0].FirstOK["search_symbols/primary"]; *first.FromStartMS.Value != 5600 || *first.FromReadyMS.Value != 600 {
		t.Fatalf("initial first OK = %+v", first)
	}
	if d := sum.RestartReadyToFirstOK["search_symbols/primary"]; d.Count != 2 || *d.P95.Value != 1050 || *d.P50.Value != 0 {
		t.Fatalf("restart ready to first OK = %+v", d)
	}
	metrics := sum.metrics()
	for name, want := range map[string]float64{
		"startup_boots":                            3,
		"startup_linked_worktrees":                 24,
		"startup_repo_not_tracked_before_ready":    2,
		"startup_nonconforming_loading_refusals":   2,
		"startup_hook_denies_while_loading":        0,
		"startup_hook_denies_after_ready":          1,
		"startup_track_advice_while_loading":       2,
		"startup_track_advice_without_socket":      1,
		"startup_initial_time_to_ready_ms":         5000,
		"startup_initial_first_ok_ms":              5600,
		"startup_restart_time_to_ready_p95_ms":     1000,
		"startup_restart_ready_to_first_ok_p95_ms": 1050,
		"startup_coordinators_within_2s_min":       2,
	} {
		if m := metrics[name]; m.Value == nil || *m.Value != want {
			t.Errorf("%s = %+v, want %v", name, m, want)
		}
	}

	// A restart that never answered must not vanish from the p95.
	sum = summarizeStartup(samples[:len(samples)-1], boots, "app", 24)
	if m := sum.metrics()["startup_restart_ready_to_first_ok_p95_ms"]; m.Value != nil || !strings.Contains(m.Reason, "1 of 2 restarts") {
		t.Fatalf("missing restart observation = %+v", m)
	}
	boots[2].Coordinators = nil
	if m := summarizeStartup(samples, boots, "app", 24).metrics()["startup_coordinators_within_2s_min"]; m.Value != nil {
		t.Fatalf("a boot without a census cannot report a minimum: %+v", m)
	}
}

func TestStartupOptionsStayScoped(t *testing.T) {
	base := []string{"--binary", "/b", "--manifest", "/m", "--scratch", "/s"}
	o, err := parseRun(append(base, "--scenario", "startup", "--restarts", "10", "--seed-store", "/v21.db"))
	if err != nil || o.Startup.Restarts != 10 || o.Startup.SeedStore != "/v21.db" {
		t.Fatalf("startup options = %+v, %v", o.Startup, err)
	}
	if scenarios, _ := selectedScenarios(o); len(scenarios) != 1 || scenarios[0] != "startup" {
		t.Fatalf("startup scenarios = %v", scenarios)
	}
	o, err = parseRun(append(base, "--scenario", "all"))
	if scenarios, _ := selectedScenarios(o); err != nil || strings.Contains(strings.Join(scenarios, ","), "startup") {
		t.Fatalf("all must not restart daemons on its own: %v %v", scenarios, err)
	}
	for _, args := range [][]string{{"--scenario", "read", "--restarts", "1"}, {"--scenario", "cold", "--seed-store", "/v21.db"}, {"--scenario", "startup", "--restarts", "-1"}} {
		if _, err := parseRun(append(base, args...)); err == nil {
			t.Errorf("parseRun(%v) accepted startup options outside their scope", args)
		}
	}
}

// The whole scenario against the fixture daemon: probing starts before the
// first socket, keeps sampling through a restart, and the gate, metrics and
// per-second log come from that one run.
func TestStartupScenarioProbesAcrossARestart(t *testing.T) {
	t.Setenv("LOAD_DAEMON_FIXTURE", "serve")
	corpus := gitFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	options := runOptions{Binary: binary, Profile: "agent", Host: "native", Agents: 1, Worktrees: 1, Duration: time.Second, Deadline: 2 * time.Second, WarmTimeout: 20 * time.Second, StopTimeout: 5 * time.Second, KeepState: true, Startup: startupOptions{Restarts: 1}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	phase, _, artifacts, crashes, version, err := runPhase(ctx, options, startupScenario, []Corpus{corpus}, "", t.TempDir())
	if err != nil {
		t.Fatalf("startup scenario: %v %+v", err, phase.Errors)
	}
	var summary startupSummary
	if err := json.Unmarshal(phase.Gate["startup_probe"], &summary); err != nil {
		t.Fatal(err)
	}
	if version != "fixture-v1" || len(summary.Boots) != 2 || len(phase.Status) != 1 || len(phase.Stats) != 1 || crashes["panic_lines"] != 0 {
		t.Fatalf("version=%q boots=%d status=%d stats=%d crashes=%v", version, len(summary.Boots), len(phase.Status), len(phase.Stats), crashes)
	}
	for _, key := range []string{"initialize/primary", "initialize_facade/worktree", "search_symbols/primary", "search_symbols/worktree", "search_symbols/router", "index_health_resource/router", "workspace_index/primary", "hook_read/worktree", "cli_pipe/primary"} {
		answered := 0
		for _, outcomes := range summary.Outcomes[key] {
			answered += outcomes["ok"] + outcomes["allow"]
		}
		if answered == 0 {
			t.Errorf("%s has no successful answer: %v", key, summary.Outcomes[key])
		}
	}
	for _, name := range []string{"startup_initial_first_ok_ms", "startup_restart_ready_to_first_ok_p95_ms", "startup_restart_time_to_ready_p95_ms"} {
		if phase.Metrics[name].Value == nil {
			t.Errorf("%s unavailable: %s", name, phase.Metrics[name].Reason)
		}
	}
	if m := phase.Metrics["startup_coordinators_within_2s_min"]; m.Value == nil || *m.Value != 1 || *phase.Metrics["startup_linked_worktrees"].Value != 1 {
		t.Errorf("coordinator census = %+v", m)
	}
	log, err := os.ReadFile(artifacts["startup_probe_log"])
	if err != nil || strings.Count(string(log), "\n") < 10 {
		t.Fatalf("per-second log: %d bytes, %v", len(log), err)
	}
	roster, err := os.ReadFile(filepath.Join(artifacts["private_root"], "config", "gortex", "servers.toml"))
	if err != nil || !strings.Contains(string(roster), startupRosterWorkspace) {
		t.Fatalf("router roster: %s %v", roster, err)
	}
	claim, err := os.ReadFile(filepath.Join(artifacts["private_root"], "repos", corpus.Label, ".gortex-probe-router", ".gortex.yaml"))
	if err != nil || string(claim) != "workspace: "+startupRosterWorkspace+"\n" {
		t.Fatalf("router cwd claim: %q %v", claim, err)
	}
}
