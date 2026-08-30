package hooks

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/profiles"
)

func withFakeStatus(t *testing.T, fn func() (*daemon.StatusResponse, error)) {
	t.Helper()
	prev := sessionStartStatusFn
	sessionStartStatusFn = fn
	t.Cleanup(func() { sessionStartStatusFn = prev })
}

func TestRunSessionStart_RejectsWrongEvent(t *testing.T) {
	data := []byte(`{"hook_event_name":"PreCompact"}`)
	out := captureStdout(t, func() { runSessionStart(data, 0) })
	if out != "" {
		t.Errorf("expected no-op for non-SessionStart, got: %q", out)
	}
}

func TestRulePreambleRoutesByOutcomeAndPreservesExactIdentifiers(t *testing.T) {
	briefing := rulePreamble()
	if got := strings.Count(briefing, profiles.WorktreeBranchRoutingPolicy); got != 1 {
		t.Fatalf("rule preamble embeds canonical worktree policy %d times, want once", got)
	}
	for _, required := range []string{
		"For an explicitly named file",
		"options:{new_user_task:true}",
		"choose by requested output",
		"requested output is files, symbols, or supporting evidence",
		"localize task may be concise",
		"faithfully preserve the issue title",
		"every user-supplied technical identifier, path, literal, error, symptom, and stated hypothesis",
		"never invent a causal hypothesis",
		"clearly framed problem section may be restored exactly at execution",
		"only for a clearly lossy model task",
		"evidence can reflect details beyond a concise tool argument",
		"only when work will actually continue beyond localization into diagnosis, relationship analysis, or implementation",
		"For `needs_recovery`, make one accepted, bounded Gortex MCP `search` or `read` call",
		"preserves the recovery allowance",
		"the rejected request does not count as the accepted recovery",
		"Do not call host Read, Grep, Glob, or Bash",
		"one aligned file/symbol tuple",
		"preserve the PRIMARY file and symbol identities",
		"SUPPORTING rows are optional context",
		"intentionally not executed and replays the same retained terminal payload",
		"not stale or canned output or an integration failure",
		"Outside an active localization contract",
	} {
		if !strings.Contains(briefing, required) {
			t.Fatalf("rule preamble missing %q: %s", required, briefing)
		}
	}
	for _, forced := range []string{
		"including a request framed as diagnosis or a why question",
		"Call `explore` first for code discovery, diagnosis",
	} {
		if strings.Contains(briefing, forced) {
			t.Fatalf("rule preamble contains forced-localize wording %q: %s", forced, briefing)
		}
	}
}

func sessionGuidanceGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestRenderCwdCoverageWaitsForAutomaticFamilyCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	main := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionGuidanceGit(t, main, "init", "-q", "-b", "main")
	sessionGuidanceGit(t, main, "config", "user.email", "test@example.com")
	sessionGuidanceGit(t, main, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(main, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionGuidanceGit(t, main, "add", ".")
	sessionGuidanceGit(t, main, "commit", "-q", "-m", "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	sessionGuidanceGit(t, main, "worktree", "add", "-q", "-b", "feature", linked)

	status := &daemon.StatusResponse{TrackedRepos: []daemon.TrackedRepoStatus{{Name: "main", Path: main}}}
	got := renderCwdCoverage(linked, status)
	if !strings.Contains(got, "awaiting automatic discovery") || !strings.Contains(got, "Do not run `gortex track`") {
		t.Fatalf("linked worktree received no neutral discovery guidance:\n%s", got)
	}
	if strings.Contains(got, "`gortex track "+linked+"`") {
		t.Fatalf("linked worktree was told to become a dedicated graph:\n%s", got)
	}
	reverse := &daemon.StatusResponse{TrackedRepos: []daemon.TrackedRepoStatus{{Name: "linked", Path: linked}}}
	reverseGuidance := renderCwdCoverage(main, reverse)
	if !strings.Contains(reverseGuidance, "awaiting automatic discovery") || strings.Contains(reverseGuidance, "`gortex track "+main+"`") {
		t.Fatalf("primary checkout was not treated as automatic when a linked family member is tracked:\n%s", reverseGuidance)
	}

	unrelated := t.TempDir()
	unrelatedGuidance := renderCwdCoverage(unrelated, status)
	if !strings.Contains(unrelatedGuidance, "gortex track "+unrelated) {
		t.Fatalf("truly unrelated directory lost explicit-track guidance:\n%s", unrelatedGuidance)
	}
}

func TestRunSessionStartEmitsNeutralRoutingAndIdentifierGuidance(t *testing.T) {
	configureLocalizationTerminalTestHome(t)
	withFakeStatus(t, func() (*daemon.StatusResponse, error) { return nil, errDaemonUnreachable })
	payload := mustJSON(t, map[string]any{
		"hook_event_name": "SessionStart",
		"session_id":      "routing-event",
		"cwd":             t.TempDir(),
	})
	output := captureHookStdout(t, func() { runSessionStart(payload, 0) })
	var decoded HookOutput
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("decode SessionStart output: %v; output=%q", err, output)
	}
	if decoded.Decision != "" || decoded.Reason != "" || decoded.SystemMessage != "" || decoded.HookSpecificOutput == nil {
		t.Fatalf("unexpected SessionStart output shape: %#v", decoded)
	}
	if decoded.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Fatalf("hook event = %q, want SessionStart", decoded.HookSpecificOutput.HookEventName)
	}
	context := decoded.HookSpecificOutput.AdditionalContext
	for _, required := range []string{
		"choose by requested output",
		"localize task may be concise",
		"faithfully preserve the issue title",
		"clearly framed problem section may be restored exactly at execution",
		"only for a clearly lossy model task",
		"evidence can reflect details beyond a concise tool argument",
		"For `needs_recovery`",
		"one accepted, bounded Gortex MCP `search` or `read` call",
		"preserves the recovery allowance",
		"the rejected request does not count as the accepted recovery",
		"Do not call host Read, Grep, Glob, or Bash",
		"At `answer_ready`",
		"respond from `completion.final_response`",
		"one aligned file/symbol tuple",
		"preserve the PRIMARY file and symbol identities",
		"SUPPORTING rows are optional context",
		"intentionally not executed and replays the same retained terminal payload",
		"not stale or canned output or an integration failure",
	} {
		if !strings.Contains(context, required) {
			t.Fatalf("SessionStart event missing %q: %s", required, context)
		}
	}
	if strings.Contains(context, "including a request framed as diagnosis or a why question") {
		t.Fatalf("SessionStart event contains forced-localize diagnosis wording: %s", context)
	}
}

func TestRunSessionStart_DaemonDown(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return nil, errDaemonUnreachable
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x","source":"startup"}`)
	out := captureStdout(t, func() { runSessionStart(data, 0) })
	if out == "" {
		t.Fatal("expected briefing output even when daemon is down")
	}

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "graph transport is unreachable") {
		t.Errorf("expected transport-down notice, got:\n%s", ac)
	}
	if !strings.Contains(ac, "MCP integration failure") {
		t.Errorf("expected integration-failure direction, got:\n%s", ac)
	}
	if strings.Contains(ac, "gortex daemon start") || strings.Contains(ac, "gortex call ") {
		t.Errorf("native MCP guidance must not advertise a manual fallback, got:\n%s", ac)
	}
	if !strings.Contains(ac, "Rule:") {
		t.Errorf("rule preamble missing, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonReady_CwdExactMatch(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.15.0",
			UptimeSeconds: 3600,
			Ready:         true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Name: "gortex", Path: "/tmp/gortex", Workspace: "gortex", Nodes: 6604, Edges: 27403},
				{Name: "cloud_web", Path: "/tmp/cloud_web", Workspace: "cloud_web", Nodes: 265, Edges: 276},
			},
			Workspaces: []daemon.WorkspaceSummary{
				{Slug: "gortex"}, {Slug: "cloud_web"},
			},
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/gortex"}`)
	out := captureStdout(t, func() { runSessionStart(data, 0) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "daemon ready") {
		t.Errorf("expected ready marker, got:\n%s", ac)
	}
	if !strings.Contains(ac, "is tracked** as repo `gortex`") {
		t.Errorf("expected exact-match cwd line, got:\n%s", ac)
	}
	if !strings.Contains(ac, "uptime 1h") {
		t.Errorf("expected formatted uptime, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonReady_CwdContainsRepos(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.15.0",
			UptimeSeconds: 60,
			Ready:         true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Name: "gortex", Path: "/tmp/gortex"},
				{Name: "cloud_web", Path: "/tmp/cloud_web"},
				{Name: "project1", Path: "/opt/project1"}, // unrelated: NOT under cwd /tmp
			},
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp"}`)
	out := captureStdout(t, func() { runSessionStart(data, 0) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "is a workspace root** containing 2 tracked repo(s)") {
		t.Errorf("expected workspace-root summary, got:\n%s", ac)
	}
	if !strings.Contains(ac, "cloud_web") || !strings.Contains(ac, "gortex") {
		t.Errorf("expected sub-repo names, got:\n%s", ac)
	}
	if strings.Contains(ac, "project1") {
		t.Errorf("unrelated repo leaked into briefing:\n%s", ac)
	}
	if !strings.Contains(ac, "fans out across") {
		t.Errorf("expected multi-repo fan-out guidance, got:\n%s", ac)
	}
	if !strings.Contains(ac, "prefix file paths with the repo name") {
		t.Errorf("expected repo-prefix routing guidance, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonReady_CwdNotTracked(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "0.15.0",
			Ready:   true,
			TrackedRepos: []daemon.TrackedRepoStatus{
				{Name: "gortex", Path: "/tmp/gortex"},
			},
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/playground"}`)
	out := captureStdout(t, func() { runSessionStart(data, 0) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "is not covered by any tracked repo") {
		t.Errorf("expected untracked notice, got:\n%s", ac)
	}
	if !strings.Contains(ac, "gortex track /tmp/playground") {
		t.Errorf("expected actionable track command, got:\n%s", ac)
	}
}

func TestRunSessionStart_DaemonWarmup(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version:       "0.15.0",
			Ready:         false,
			WarmupSeconds: 30,
			TrackedRepos:  []daemon.TrackedRepoStatus{},
		}, nil
	})
	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x"}`)
	out := captureStdout(t, func() { runSessionStart(data, 0) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "warming up") {
		t.Errorf("expected warmup notice, got:\n%s", ac)
	}
}

// TestRunSessionStart_CompactSourceAddsReinjectionAdvisory pins the one place
// Gortex can speak after a compaction. PreCompact cannot inject context, so if
// this branch regresses the agent wakes up holding re-injected whole-file
// content with nothing telling it not to read those files again.
func TestRunSessionStart_CompactSourceAddsReinjectionAdvisory(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "v0.63.2", Ready: true, EnrichmentComplete: true,
			TrackedRepos: []daemon.TrackedRepoStatus{},
		}, nil
	})
	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x","source":"compact"}`)
	out := captureStdout(t, func() { runSessionStart(data, 1) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	for _, want := range []string{
		"Gortex Post-Compaction Snapshot",
		"Do not re-read indexed files.",
		"Rule:", // the ordinary orientation block must survive alongside it
	} {
		if !strings.Contains(ac, want) {
			t.Errorf("compact-source briefing missing %q, got:\n%s", want, ac)
		}
	}
}

func TestRunSessionStart_NonCompactSourcesStayUnchanged(t *testing.T) {
	for _, source := range []string{"startup", "resume", "clear", "fork", ""} {
		t.Run(source, func(t *testing.T) {
			withFakeStatus(t, func() (*daemon.StatusResponse, error) {
				return &daemon.StatusResponse{
					Version: "v0.63.2", Ready: true, EnrichmentComplete: true,
					TrackedRepos: []daemon.TrackedRepoStatus{},
				}, nil
			})
			data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x","source":"` + source + `"}`)
			out := captureStdout(t, func() { runSessionStart(data, 1) })

			if strings.Contains(out, "Post-Compaction") {
				t.Errorf("source %q must not get the post-compaction block, got:\n%s", source, out)
			}
		})
	}
}

func TestRunSessionStart_DaemonError(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return nil, errors.New("synthetic transport failure")
	})
	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x"}`)
	out := captureStdout(t, func() { runSessionStart(data, 0) })

	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid HookOutput JSON: %v\n%s", err, out)
	}
	ac := payload.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ac, "status query failed") {
		t.Errorf("expected error surface, got:\n%s", ac)
	}
	if !strings.Contains(ac, "Rule:") {
		t.Errorf("rule preamble must still appear on error path, got:\n%s", ac)
	}
}

func TestDispatch_RoutesSessionStart(t *testing.T) {
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "0.15.0",
			Ready:   true,
		}, nil
	})

	data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp"}`)
	withStdin(t, data, func() {
		out := captureStdout(t, func() { Run(0, ModeDeny) })
		if !strings.Contains(out, "Gortex Session Orientation") {
			t.Errorf("Run did not route SessionStart:\n%s", out)
		}
	})
}

func TestHasPathPrefix(t *testing.T) {
	cases := []struct {
		path, prefix string
		want         bool
	}{
		{"/foo/bar", "/foo", true},
		{"/foo/bar", "/foo/bar", true},
		{"/foo/barbaz", "/foo/bar", false}, // not a real subpath
		{"/foo", "/foo/bar", false},
		{"/foo/bar/baz", "/foo/bar", true},
		{"/foo", "/", true},
	}
	for _, c := range cases {
		got := hasPathPrefix(c.path, c.prefix)
		if got != c.want {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", c.path, c.prefix, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, "0s"},
		{45, "45s"},
		{60, "1m"},
		{125, "2m5s"},
		{3600, "1h"},
		{3660, "1h1m"},
	}
	for _, c := range cases {
		got := formatDuration(c.secs)
		if got != c.want {
			t.Errorf("formatDuration(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

// TestRunSessionStart_DoesNotDoubleVersionPrefix guards the "vv0.63.2" render
// reported in issue #70. StatusResponse.Version arrives already v-prefixed, and
// every readiness line adds its own literal "v".
func TestRunSessionStart_DoesNotDoubleVersionPrefix(t *testing.T) {
	cases := []struct {
		name   string
		status *daemon.StatusResponse
	}{
		{"ready", &daemon.StatusResponse{Version: "v0.63.2", Ready: true, EnrichmentComplete: true}},
		{"enriching", &daemon.StatusResponse{Version: "v0.63.2", Ready: true}},
		{"warmup", &daemon.StatusResponse{Version: "v0.63.2", WarmupSeconds: 30}},
		{"unprefixed", &daemon.StatusResponse{Version: "0.63.2", Ready: true, EnrichmentComplete: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.status.TrackedRepos = []daemon.TrackedRepoStatus{}
			withFakeStatus(t, func() (*daemon.StatusResponse, error) { return tc.status, nil })

			data := []byte(`{"hook_event_name":"SessionStart","cwd":"/tmp/x"}`)
			out := captureStdout(t, func() { runSessionStart(data, 0) })

			if strings.Contains(out, "vv") {
				t.Errorf("doubled version prefix in briefing:\n%s", out)
			}
			if !strings.Contains(out, "v0.63.2") {
				t.Errorf("version missing from briefing:\n%s", out)
			}
		})
	}
}

func TestDaemonVersionLabel(t *testing.T) {
	for in, want := range map[string]string{
		"v0.63.2":            "0.63.2",
		"0.63.2":             "0.63.2",
		"v0.63.2-39-gabcdef": "0.63.2-39-gabcdef",
		"":                   "",
	} {
		if got := daemonVersionLabel(in); got != want {
			t.Errorf("daemonVersionLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
