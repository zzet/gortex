package hooks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/localizationauth"
)

func TestRunPostTask_RejectsWrongEvent(t *testing.T) {
	data := []byte(`{"hook_event_name":"PreToolUse"}`)
	out := captureStdout(t, func() { runPostTask(data, 0) })
	if out != "" {
		t.Errorf("expected silent no-op for wrong event, got: %q", out)
	}
}

func TestRunPostTask_StopHookActive_Skips(t *testing.T) {
	// stop_hook_active=true means we're already inside a Stop-hook loop;
	// firing again would recurse.
	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":true}`)
	out := captureStdout(t, func() { runPostTask(data, 1) })
	if out != "" {
		t.Errorf("expected no output when stop_hook_active, got: %q", out)
	}
}

func TestRunPostTask_NoBridge(t *testing.T) {
	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, 1) })
	if out != "" {
		t.Errorf("expected no output when bridge unreachable, got: %q", out)
	}
}

func TestRunPostTask_NoChanges_Silent(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":[],"changed_symbols":[],"risk":"NONE","summary":"no indexed symbols affected"}`,
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })
	if out != "" {
		t.Errorf("expected silent no-op when nothing changed, got: %q", out)
	}
}

func TestRunPostTask_RendersDiagnostics(t *testing.T) {
	changedJSON := `{
		"changed_files":["internal/foo.go","internal/bar.go"],
		"changed_symbols":[
			{"id":"internal/foo.go::Foo","name":"Foo","kind":"function"},
			{"id":"internal/bar.go::Bar","name":"Bar","kind":"method"}
		],
		"risk":"MEDIUM",
		"summary":"2 symbols touched"
	}`
	srv := newFakeServer(map[string]string{
		"detect_changes":   changedJSON,
		"get_test_targets": "internal/foo_test.go::TestFoo\ninternal/bar_test.go::TestBar",
		"check_guards":     "boundary my-rule cross-layer import violates ui→db\n",
		"analyze":          "function Orphan internal/foo.go::Foo unused fan_in=0\n",
		"contracts":        "matched: 0 pairs\norphan providers: 1\n  [gortex] http::GET::/api/ghost api.go:12\norphan consumers: 0\n",
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, port) })

	if out == "" {
		t.Fatal("expected diagnostic output when changes are present")
	}
	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output not valid HookOutput JSON: %v\n%s", err, out)
	}
	if payload.HookSpecificOutput == nil || payload.HookSpecificOutput.HookEventName != "Stop" {
		t.Fatal("hookSpecificOutput missing or wrong event name")
	}

	ac := payload.HookSpecificOutput.AdditionalContext
	mustContain := []string{
		"Working-Tree Diagnostics",
		"risk `MEDIUM`",
		"Tests Covering These Symbols",
		"internal/foo_test.go::TestFoo",
		"Guard Violations",
		"boundary my-rule",
		"Potential Dead Code",
		"internal/foo.go::Foo", // only the changed-symbol intersection
		"API Contract Issues",
		"orphan provider",
	}
	for _, frag := range mustContain {
		if !strings.Contains(ac, frag) {
			t.Errorf("briefing missing %q\n---\n%s", frag, ac)
		}
	}
}

// TestPostTaskBriefing_DisclosesWholeTreeScope pins the honesty contract: the
// briefing must name the tree it diffed, admit it does not attribute changes to
// a session, admit untracked files are invisible, and must not tell the agent to
// run tests for work that may not be its own.
func TestPostTaskBriefing_DisclosesWholeTreeScope(t *testing.T) {
	srv := newRecordingFakeServer(map[string]string{
		"detect_changes": `{
			"changed_files":["internal/foo.go"],
			"changed_symbols":[{"id":"internal/foo.go::Foo","name":"Foo","kind":"function","file_path":"gortex/internal/foo.go"}],
			"risk":"LOW","summary":"1 symbol",
			"scope":"all","repo":"gortex","repo_root":"/tmp/checkout-a"
		}`,
		"get_test_targets": "internal/foo_test.go TestFoo\n",
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })
	if out == "" {
		t.Fatal("expected a briefing")
	}

	for _, frag := range []string{
		"/tmp/checkout-a",    // names the tree actually diffed
		"does not attribute", // admits the limitation
		"whole working tree", // states the real scope
		"Untracked",          // discloses the blind spot
		"parallel session",   // names the concurrent-session case
		"uncommitted (staged + unstaged)",
	} {
		if !strings.Contains(out, frag) {
			t.Errorf("briefing missing %q\n---\n%s", frag, out)
		}
	}
	for _, banned := range []string{
		"Run the tests above", // the old imperative
		"**Changed:**",        // the old session-implying claim
		"Post-Task",           // the old title
	} {
		if strings.Contains(out, banned) {
			t.Errorf("briefing still contains %q\n---\n%s", banned, out)
		}
	}

	// The scope we request must match the scope we claim in prose.
	calls := srv.argsFor("detect_changes")
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 detect_changes call, got %d", len(calls))
	}
	if got := calls[0]["scope"]; got != postTaskDiffScope {
		t.Errorf("detect_changes scope = %v, want %q", got, postTaskDiffScope)
	}
	if got := calls[0]["summary_only"]; got != true {
		t.Errorf("detect_changes summary_only = %v, want true", got)
	}
}

func TestRunPostTask_DeadCodeFiltersToChanged(t *testing.T) {
	// Dead-code results that don't overlap with changed symbols should
	// NOT be included — we only flag what the current task left orphaned.
	changedJSON := `{
		"changed_files":["foo.go"],
		"changed_symbols":[{"id":"foo.go::Foo","name":"Foo","kind":"function"}],
		"risk":"LOW","summary":"1 symbol"
	}`
	srv := newFakeServer(map[string]string{
		"detect_changes":   changedJSON,
		"get_test_targets": "",
		"check_guards":     "",
		"analyze":          "function SomethingElse unrelated/path.go fan_in=0\n",
		// Realistic clean output: the compact shape always leads with the
		// matched pairs and reports zero orphans at the end.
		"contracts": "matched: 2 pairs\n  c1: [a] a.go:1 -> [b] b.go:2\n  c2: [a] a.go:9 -> [b] b.go:4\norphan providers: 0\norphan consumers: 0\n",
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if strings.Contains(out, "Potential Dead Code") {
		t.Errorf("dead-code section should be omitted when no overlap with changed symbols:\n%s", out)
	}
	if strings.Contains(out, "API Contract Issues") {
		t.Errorf("contract section should be omitted when clean:\n%s", out)
	}

	// A changed symbol whose liveness is unknown while edits are pending is
	// never reported as dead.
	pending := newFakeServer(map[string]string{
		"analyze": "relations: pending (1 files)\npending_unknown function foo.go::Foo foo.go:3\n",
	})
	defer pending.Close()
	if hits := renderDeadCodeHits(portFromURL(t, pending.URL), []string{"foo.go::Foo"}); hits != "" {
		t.Errorf("pending_unknown symbol reported as dead:\n%s", hits)
	}
}

// While the graph is still publishing edits, detect_changes answers with a
// labelled partial. Both briefings must say so and name the changed files
// rather than render diagnostics over lagging relations or fall silent.
func TestPostTaskBriefingPrintsPendingRelations(t *testing.T) {
	for name, detect := range map[string]string{
		"primary": `{"changed_files":["a.go","b.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],
			"risk":"LOW","complete":false,"graph_freshness":"pending","pending_file_count":2,"pending_files":["a.go","b.go"]}`,
		"worktree": `{"changed_files":["a.go"],"changed_symbols":[],"risk":"UNKNOWN","complete":false,"graph_status":"pending"}`,
	} {
		srv := newRecordingFakeServer(map[string]string{"detect_changes": detect, "get_test_targets": "a_test.go TestA\n"})
		port := portFromURL(t, srv.URL)
		started := time.Now()
		for builder, briefing := range map[string]string{
			"stop":     buildPostTaskBriefing(port, sessionScope{}),
			"mutation": buildMutationBriefing(port, sessionScope{}),
		} {
			want := "relations: pending (2 files)"
			if name == "worktree" {
				want = "relations: pending (1 files)"
			}
			if !strings.Contains(briefing, want) || !strings.Contains(briefing, "`a.go`") {
				t.Errorf("%s %s briefing missing %q and the changed files:\n%s", name, builder, want, briefing)
			}
		}
		if n := srv.callCount("get_test_targets"); n != 0 {
			t.Errorf("%s: diagnostics ran over lagging relations (%d get_test_targets calls)", name, n)
		}
		// The daemon waits for pending edits until wait_deadline and labels
		// the rest. Without one it waits past the budget, and the labelled
		// answer arrives after the briefing has given up on it.
		for _, args := range srv.argsFor("detect_changes") {
			deadline, err := time.Parse(time.RFC3339Nano, fmt.Sprint(args["wait_deadline"]))
			if err != nil || !deadline.After(started) || !deadline.Before(started.Add(detectChangesBudget)) {
				t.Errorf("%s: detect_changes wait_deadline %v does not end inside the %v budget", name, args["wait_deadline"], detectChangesBudget)
			}
		}
		srv.Close()
	}
}

// A detect_changes that does not answer within its budget is reported, and
// the briefing returns well inside the half-second hook budget. So is one the
// daemon reached only after its wait_deadline had passed, which it refuses.
// A session that recorded no writes is not told about a slow daemon.
func TestPostTaskBriefingBoundedWhenDetectStalls(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	saveSessionState("writer", sessionState{WrittenPaths: []string{"a.go"}})
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		http.NotFound(w, r)
	}))
	defer stalled.Close()
	defer close(release)
	late := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if deadline, err := time.Parse(time.RFC3339Nano, fmt.Sprint(req.Arguments["wait_deadline"])); err == nil {
			time.Sleep(time.Until(deadline) + 10*time.Millisecond)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"wait_deadline is not in the future"}],"isError":true}`))
	}))
	defer late.Close()

	for name, srv := range map[string]*httptest.Server{"stalled": stalled, "refused late": late} {
		port := portFromURL(t, srv.URL)
		for builder, build := range map[string]func(int, sessionScope) string{
			"stop":     buildPostTaskBriefing,
			"mutation": buildMutationBriefing,
		} {
			for session, report := range map[string]bool{"": true, "writer": true, "reader": false} {
				if name != "stalled" && session != "" {
					continue
				}
				started := time.Now()
				briefing := build(port, sessionScope{SessionID: session})
				if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
					t.Errorf("%s %s briefing took %v", name, builder, elapsed)
				}
				switch {
				case !report && briefing != "":
					t.Errorf("%s %s briefing warned session %q, which wrote nothing:\n%q", name, builder, session, briefing)
				case report && !strings.Contains(briefing, "relations: pending"):
					t.Errorf("%s %s briefing did not report the busy detect_changes to session %q:\n%q", name, builder, session, briefing)
				case report && strings.Contains(briefing, "publishing"):
					t.Errorf("%s %s briefing claims edits are publishing without an answer:\n%q", name, builder, briefing)
				}
			}
		}
	}
}

// contractsCompact builds a realistic `contracts check --compact` payload:
// matched pairs first, orphan counts and rows last.
func contractsCompact(matchedPairs, orphanProviders int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "matched: %d pairs\n", matchedPairs)
	for i := 0; i < matchedPairs; i++ {
		fmt.Fprintf(&b, "  c%d: [a] a.go:%d -> [b] b.go:%d\n", i, i, i)
	}
	fmt.Fprintf(&b, "orphan providers: %d\n", orphanProviders)
	for i := 0; i < orphanProviders; i++ {
		fmt.Fprintf(&b, "  [repo] http::GET::/ghost%d api.go:%d\n", i, i)
	}
	b.WriteString("orphan consumers: 0\n")
	return b.String()
}

// TestPostTaskBriefing_ContractOrphansSurviveManyMatchedPairs is the
// regression test for the truncation bug: the orphan block sits at the END of
// the compact payload, so a leading line cap dropped it entirely once enough
// contracts matched.
func TestPostTaskBriefing_ContractOrphansSurviveManyMatchedPairs(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
		"contracts":      contractsCompact(60, 1),
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if !strings.Contains(out, "API Contract Issues") {
		t.Fatalf("expected the contract section when an orphan exists:\n%s", out)
	}
	if !strings.Contains(out, "/ghost0") {
		t.Errorf("orphan row was truncated away by the matched-pair rows:\n%s", out)
	}
	if strings.Contains(out, "c30:") {
		t.Errorf("matched pairs should not be rendered at all:\n%s", out)
	}
}

// TestPostTaskBriefing_ContractSectionOmittedWhenNoOrphans pins that a clean
// contracts check renders no heading, even though its payload is non-empty.
func TestPostTaskBriefing_ContractSectionOmittedWhenNoOrphans(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
		"contracts":      contractsCompact(60, 0),
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if strings.Contains(out, "API Contract Issues") {
		t.Errorf("contract section should be omitted when both orphan counts are 0:\n%s", out)
	}
}

// TestRenderGuardViolations_SuppressesNoRulesConfigured covers the other clean
// sentinel: guards are configured nowhere, so there is nothing to report.
func TestRenderGuardViolations_SuppressesNoRulesConfigured(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
		"check_guards":   "no guard rules configured\n",
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if strings.Contains(out, "Guard Violations") {
		t.Errorf("guard section should be omitted when no rules are configured:\n%s", out)
	}
}

// TestRenderTestTargets_CapsBytes guards the briefing budget against a tool
// that ignores `compact` and returns one enormous newline-free payload — the
// exact shape that leaked a raw JSON blob into the briefing.
func TestRenderTestTargets_CapsBytes(t *testing.T) {
	blob := `{"test_targets":[` + strings.Repeat(`{"file":"x_test.go","functions":["TestX"]},`, 2000) + `]}`
	if strings.Contains(blob, "\n") {
		t.Fatal("fixture must be a single line to exercise the byte cap")
	}
	srv := newFakeServer(map[string]string{
		"detect_changes":   `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
		"get_test_targets": blob,
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if len(out) > 6000 {
		t.Errorf("briefing grew to %d bytes; the byte cap did not bind", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected a truncation marker in the capped section:\n%s", out)
	}
}

// incidentChangeSet mirrors the reported failure: a session that edited one Go
// file while a sibling session on the same checkout left three release-config
// files dirty.
func incidentChangeSet() string {
	return `{
	"changed_files":["internal/hooks/posttask.go",".goreleaser.yml",".github/workflows/ci.yml",".github/workflows/release.yml"],
	"changed_symbols":[
		{"id":"gortex/internal/hooks/posttask.go::runPostTask","name":"runPostTask","kind":"function","file_path":"gortex/internal/hooks/posttask.go"},
		{"id":"gortex/.goreleaser.yml::builds","name":"builds","kind":"config","file_path":"gortex/.goreleaser.yml"},
		{"id":"gortex/.github/workflows/ci.yml::jobs","name":"jobs","kind":"config","file_path":"gortex/.github/workflows/ci.yml"},
		{"id":"gortex/.github/workflows/release.yml::jobs","name":"jobs","kind":"config","file_path":"gortex/.github/workflows/release.yml"}
	],
	"risk":"HIGH","summary":"4 symbols touched",
	"scope":"all","repo":"gortex","repo_root":"` + jsonPathFixture(repoFixtureRoot) + `"
}`
}

// TestRunPostTask_AttributesOnlyThisSessionsEdits is the regression test for
// the reported bug: session A's Stop hook reported session B's in-flight edits
// and told A to run tests for files it never touched.
func TestRunPostTask_AttributesOnlyThisSessionsEdits(t *testing.T) {
	withSessionDir(t)
	withTrackedRepos(t, daemon.TrackedRepoStatus{Prefix: "gortex", Path: repoFixtureRoot})
	saveSessionState("sess-a", sessionState{
		WrittenPaths: []string{filepath.Join(repoFixtureRoot, "internal", "hooks", "posttask.go")},
	})

	srv := newRecordingFakeServer(map[string]string{
		"detect_changes":        incidentChangeSet(),
		"get_test_targets":      "internal/hooks/posttask_test.go TestRunPostTask\nrun: go test ./internal/hooks/\n",
		"explain_change_impact": `{"risk":"LOW","summary":"1 symbol","total_affected":2}`,
	})
	defer srv.Close()

	data := mustJSON(t, map[string]any{"hook_event_name": "Stop", "stop_hook_active": false, "session_id": "sess-a", "cwd": repoFixtureRoot})
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })
	if out == "" {
		t.Fatal("expected a briefing for the session's own edit")
	}

	// The session's own work is reported...
	for _, want := range []string{
		"Post-Task Diagnostics",
		"Your edits this session:** 1 symbol(s) across 1 file(s)",
		"internal/hooks/posttask_test.go",
		"risk `LOW`", // the owned risk, NOT the tree-wide HIGH
	} {
		if !strings.Contains(out, want) {
			t.Errorf("briefing missing %q\n---\n%s", want, out)
		}
	}
	// ...the sibling's files appear only as unattributed, never as work to do...
	if !strings.Contains(out, "Also dirty, not attributed to this session") {
		t.Errorf("sibling files were not disclosed as unattributed\n---\n%s", out)
	}
	// ...and the tree-wide risk must not leak in as if it were the session's.
	if strings.Contains(out, "risk `HIGH`") {
		t.Errorf("reported another session's risk tier\n---\n%s", out)
	}

	// Downstream diagnostics must be scoped to the owned symbol only. This is
	// the concrete harm in the report: tests requested for someone else's files.
	testCalls := srv.argsFor("get_test_targets")
	if len(testCalls) != 1 {
		t.Fatalf("expected 1 get_test_targets call, got %d", len(testCalls))
	}
	gotIDs, _ := testCalls[0]["ids"].(string)
	if gotIDs != "gortex/internal/hooks/posttask.go::runPostTask" {
		t.Errorf("get_test_targets ids = %q; must carry only the session's own symbol", gotIDs)
	}
	for _, banned := range []string{"goreleaser", "ci.yml", "release.yml"} {
		if strings.Contains(gotIDs, banned) {
			t.Errorf("ids leaked another session's symbol %q: %s", banned, gotIDs)
		}
	}
}

// TestRunPostTask_SessionOwnsNothing_Silent is the exact scenario from the
// report: the session touched nothing in the repo while a sibling edited it.
// Stop fires every turn, so this must be silent rather than a repeated notice.
func TestRunPostTask_SessionOwnsNothing_Silent(t *testing.T) {
	withSessionDir(t)
	withTrackedRepos(t, daemon.TrackedRepoStatus{Prefix: "gortex", Path: repoFixtureRoot})
	// The session wrote only outside the repo — scratchpad and memory files.
	saveSessionState("sess-a", sessionState{
		WrittenPaths: []string{fixtureAbs("/tmp/scratch/report.html"), fixtureAbs("/Users/dev/.claude/memory/MEMORY.md")},
	})

	srv := newRecordingFakeServer(map[string]string{
		"detect_changes":   incidentChangeSet(),
		"get_test_targets": "should-never-be-called\n",
	})
	defer srv.Close()

	data := mustJSON(t, map[string]any{"hook_event_name": "Stop", "stop_hook_active": false, "session_id": "sess-a", "cwd": repoFixtureRoot})
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if out != "" {
		t.Errorf("expected silence when the session owns none of the diff, got:\n%s", out)
	}
	if n := srv.callCount("get_test_targets"); n != 0 {
		t.Errorf("asked for test targets %d times for work this session did not do", n)
	}
}

// TestRunPostTask_OwnsAll_SkipsImpactCall keeps the single-session case free of
// an extra round-trip: when the session owns the whole diff, detect_changes'
// own risk already describes it.
func TestRunPostTask_OwnsAll_SkipsImpactCall(t *testing.T) {
	withSessionDir(t)
	withTrackedRepos(t, daemon.TrackedRepoStatus{Prefix: "gortex", Path: repoFixtureRoot})
	saveSessionState("sess-a", sessionState{WrittenPaths: []string{filepath.Join(repoFixtureRoot, "internal", "foo.go")}})

	srv := newRecordingFakeServer(map[string]string{
		"detect_changes": `{
			"changed_files":["internal/foo.go"],
			"changed_symbols":[{"id":"gortex/internal/foo.go::Foo","name":"Foo","kind":"function","file_path":"gortex/internal/foo.go"}],
			"risk":"MEDIUM","summary":"1","scope":"all","repo":"gortex","repo_root":"` + jsonPathFixture(repoFixtureRoot) + `"
		}`,
		"get_test_targets": "internal/foo_test.go TestFoo\n",
	})
	defer srv.Close()

	data := mustJSON(t, map[string]any{"hook_event_name": "Stop", "stop_hook_active": false, "session_id": "sess-a", "cwd": repoFixtureRoot})
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if n := srv.callCount("explain_change_impact"); n != 0 {
		t.Errorf("recomputed risk %d times when the session owns the whole diff", n)
	}
	if !strings.Contains(out, "risk `MEDIUM`") {
		t.Errorf("expected detect_changes' own risk to be reused:\n%s", out)
	}
	if strings.Contains(out, "Also dirty") {
		t.Errorf("nothing should be unattributed here:\n%s", out)
	}
}

func claimCheckTestInput(t *testing.T, message string) PostTaskInput {
	t.Helper()
	primary := []string{"repo/a.go::Writer.write", "repo/b.go::Reader.read", "repo/c.go::Store.load", "repo/d.go::Cache.get", "repo/e.go::Index.find"}
	evidence := append(append([]string(nil), primary...), "repo/f.go::Helper.close")
	return claimCheckTestInputWithEvidence(t, message, primary, evidence)
}

func claimCheckTestInputWithEvidence(t *testing.T, message string, primary, evidence []string) PostTaskInput {
	t.Helper()
	configureLocalizationTerminalTestHome(t)
	identity := beginTestLocalizationTurn(t, t.Name(), "prompt", t.TempDir())
	if !markLocalizationTerminalReceipt(identity, localizationauth.Receipt{
		FinalResponse: "answer", PrimaryIDs: primary, EvidenceIDs: evidence,
		ContractVersion: localizationTerminalContractV2, Enforceable: true,
	}) {
		t.Fatal("terminal marker was not written")
	}
	return PostTaskInput{
		HookEventName: "Stop", SessionID: identity.SessionID, PromptID: identity.PromptID,
		AgentID: identity.AgentID, CWD: identity.CWD, LastAssistantMessage: message,
	}
}

func TestLocalizationClaimCheckAcceptsAuthenticatedClaim(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- write")
	if got := localizationClaimCheck(input); got != "" {
		t.Fatalf("matching evidence was challenged: %q", got)
	}
	input.LastAssistantMessage = "The implementation is in repo/a.go."
	if got := localizationClaimCheck(input); got != "" {
		t.Fatalf("a file path in prose was treated as a symbol claim: %q", got)
	}
}

func TestLocalizationClaimCheckNormalizesCommonMethodNotation(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- Writer.write()")
	if got := localizationClaimCheck(input); got != "" {
		t.Fatalf("qualified method call was challenged: %q", got)
	}
	input.LastAssistantMessage = "SYMBOLS:\n- (*Writer).write"
	if got := localizationClaimCheck(input); got != "" {
		t.Fatalf("pointer-receiver method was challenged: %q", got)
	}
}

func TestLocalizationClaimCheckRequiresEveryMaterialClaim(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- Writer.write\n- Fabricated.flush")
	if got := localizationClaimCheck(input); got == "" {
		t.Fatal("a fabricated claim beside an authenticated claim was not challenged")
	}
}

func TestLocalizationClaimCheckAcceptsExplicitNoneFitsOnly(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- none fits")
	if got := localizationClaimCheck(input); got != "" {
		t.Fatalf("explicit none-fits response was challenged: %q", got)
	}
	input.LastAssistantMessage = "SYMBOLS:\n- none fits\n- flush"
	if got := localizationClaimCheck(input); got == "" {
		t.Fatal("an unsupported claim hidden beside none-fits was not challenged")
	}
}

func TestLocalizationClaimCheckChallengesWrongBareSymbolWithinBound(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- flush")
	got := localizationClaimCheck(input)
	if got == "" {
		t.Fatal("wrong structured bare symbol was not challenged")
	}
	if len(got) > localizationClaimCheckMaxChars {
		t.Fatalf("claim check is %d chars, max %d", len(got), localizationClaimCheckMaxChars)
	}
	for _, want := range []string{"repo/a.go::Writer.write", "repo/e.go::Index.find", "explicitly confirm", "Do not retrieve"} {
		if !strings.Contains(got, want) {
			t.Errorf("claim check missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "repo/f.go::Helper.close") {
		t.Fatalf("claim check exposed non-PRIMARY evidence: %s", got)
	}
}

func TestLocalizationClaimCheckFailsOpenWithoutMessageAuthorityOrOnRetry(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- wrong")
	input.StopHookActive = true
	if got := localizationClaimCheck(input); got != "" {
		t.Fatalf("retry was challenged twice: %q", got)
	}
	input.StopHookActive = false
	input.LastAssistantMessage = ""
	if got := localizationClaimCheck(input); got != "" {
		t.Fatalf("missing final message did not fail open: %q", got)
	}
	input.LastAssistantMessage = "SYMBOLS:\n- wrong"
	input.SessionID = "unsupported-host-without-authority"
	if got := localizationClaimCheck(input); got != "" {
		t.Fatalf("missing terminal authority did not fail open: %q", got)
	}
}

func TestRunPostTaskClaimCheckBlocksOnceWithoutDaemonRetrieval(t *testing.T) {
	input := claimCheckTestInput(t, "SYMBOLS:\n- flush")
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { runPostTask(data, 0) })
	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode claim-check output %q: %v", out, err)
	}
	if payload.Decision != "block" || !strings.Contains(payload.Reason, "claim_check") {
		t.Fatalf("Stop was not blocked with a claim check: %#v", payload)
	}
	input.StopHookActive = true
	data, _ = json.Marshal(input)
	if retry := captureStdout(t, func() { runPostTask(data, 0) }); retry != "" {
		t.Fatalf("second Stop response was not accepted: %q", retry)
	}
}

func TestDispatch_RoutesStop(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runFromBytes(t, data, port) })
	if !strings.Contains(out, "Working-Tree Diagnostics") {
		t.Errorf("Run did not route to PostTask handler:\n%s", out)
	}
}

// runFromBytes feeds raw bytes into Run() by temporarily swapping stdin.
func runFromBytes(t *testing.T, data []byte, port int) {
	t.Helper()
	withStdin(t, data, func() { Run(port, ModeDeny) })
}
