package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/hooks"
)

func codexAgent() AgentHooks {
	return AgentHooks{
		Agent:             "codex",
		Configured:        map[string]int{"SessionStart": 1, "UserPromptSubmit": 1, "PreToolUse": 2, "PostToolUse": 1},
		RequiresTrust:     true,
		TrustRemedy:       "run `/hooks` inside Codex",
		InstructionsPath:  "/home/u/.codex/AGENTS.md",
		InstructionsWired: true,
	}
}

func activity(events map[string]hooks.EventActivity) hooks.EffectivenessSummary {
	if events == nil {
		events = map[string]hooks.EventActivity{}
	}
	return hooks.EffectivenessSummary{Present: true, Events: events}
}

func findingWith(t *testing.T, findings []Finding, substr string) Finding {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f.Summary, substr) {
			return f
		}
	}
	t.Fatalf("no finding containing %q in %+v", substr, findings)
	return Finding{}
}

func assertNoFindingWith(t *testing.T, findings []Finding, substr string) {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f.Summary, substr) {
			t.Fatalf("unexpected finding %q in %+v", substr, findings)
		}
	}
}

// TestConfiguredButNeverRanIsABlocker is the reported failure: every hook is
// on disk and none of them runs, because Codex skips hooks it has not been
// told to trust. Config alone cannot distinguish this from a healthy install,
// so the finding has to carry the remedy.
func TestConfiguredButNeverRanIsABlocker(t *testing.T) {
	findings := Diagnose(codexAgent(), activity(nil), Adoption{}, time.Now())

	got := findingWith(t, findings, "none has run")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if !strings.Contains(got.Remedy, "/hooks") {
		t.Errorf("remedy should name /hooks, got %q", got.Remedy)
	}
	if !HasBlocker(findings) {
		t.Error("HasBlocker should be true so the command exits non-zero")
	}
	if findings[0].Severity != SeverityBlocker {
		t.Errorf("blockers must sort first, got %+v", findings)
	}
}

// TestNeverRanWithoutTrustGateAvoidsTheTrustRemedy keeps the Codex-specific
// explanation from being handed to agents that do not gate on trust — a
// wrong remedy costs more than a vague one.
func TestNeverRanWithoutTrustGateAvoidsTheTrustRemedy(t *testing.T) {
	agent := codexAgent()
	agent.Agent = "claude-code"
	agent.RequiresTrust = false
	agent.TrustRemedy = ""

	findings := Diagnose(agent, activity(nil), Adoption{}, time.Now())
	got := findingWith(t, findings, "none has run")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if strings.Contains(got.Summary, "trusted") {
		t.Errorf("trust wording leaked to a non-gating agent: %q", got.Summary)
	}
}

// TestSingleStaleHookIsReported covers the upgrade case: trust is recorded
// per hook hash, so changing one hook's definition silences only that hook.
func TestSingleStaleHookIsReported(t *testing.T) {
	now := time.Now()
	findings := Diagnose(codexAgent(), activity(map[string]hooks.EventActivity{
		"SessionStart":     {Runs: 0, LastSeen: now.Add(-72 * time.Hour)},
		"UserPromptSubmit": {Runs: 5, Emitted: 5},
		"PreToolUse":       {Runs: 40, Emitted: 12},
		"PostToolUse":      {Runs: 3, Emitted: 1},
	}), Adoption{GortexCalls: 10, ShellCalls: 1}, now)

	got := findingWith(t, findings, "SessionStart is configured but never ran")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if !strings.Contains(got.Summary, "3d ago") {
		t.Errorf("should date the last run so a stale hook is distinguishable: %q", got.Summary)
	}
	assertNoFindingWith(t, findings, "none has run")
}

// TestHookThatRunsButNeverInjectsIsAWarn separates "never fires" from "fires
// and has nothing to say" — the same symptom, entirely different causes.
func TestHookThatRunsButNeverInjectsIsAWarn(t *testing.T) {
	findings := Diagnose(codexAgent(), activity(map[string]hooks.EventActivity{
		"SessionStart":     {Runs: 4, Emitted: 4, DaemonKnown: 4, DaemonUp: 4},
		"UserPromptSubmit": {Runs: 9, Emitted: 0, DaemonKnown: 9, DaemonUp: 9},
		"PreToolUse":       {Runs: 9, Emitted: 3, DaemonKnown: 9, DaemonUp: 9},
		"PostToolUse":      {Runs: 9, Emitted: 3, DaemonKnown: 9, DaemonUp: 9},
	}), Adoption{GortexCalls: 20, ShellCalls: 2}, time.Now())

	got := findingWith(t, findings, "UserPromptSubmit ran 9")
	if got.Severity != SeverityWarn {
		t.Errorf("severity=%s want WARN", got.Severity)
	}
	if HasBlocker(findings) {
		t.Errorf("a silent-but-live hook is not a blocker: %+v", findings)
	}
}

// TestUnreachableDaemonOutranksSilence: a hook that cannot reach the daemon
// is why it is silent, so reporting both would bury the cause under a
// symptom.
func TestUnreachableDaemonOutranksSilence(t *testing.T) {
	findings := Diagnose(codexAgent(), activity(map[string]hooks.EventActivity{
		"SessionStart":     {Runs: 3, Emitted: 0, DaemonKnown: 3, DaemonUp: 0},
		"UserPromptSubmit": {Runs: 3, Emitted: 3, DaemonKnown: 3, DaemonUp: 3},
		"PreToolUse":       {Runs: 3, Emitted: 3, DaemonKnown: 3, DaemonUp: 3},
		"PostToolUse":      {Runs: 3, Emitted: 3, DaemonKnown: 3, DaemonUp: 3},
	}), Adoption{GortexCalls: 5, ShellCalls: 1}, time.Now())

	got := findingWith(t, findings, "never reached the daemon")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if !strings.Contains(got.Remedy, "daemon start") {
		t.Errorf("remedy should start the daemon, got %q", got.Remedy)
	}
	assertNoFindingWith(t, findings, "SessionStart ran 3 time(s) and injected context 0")
}

func TestInstructionsFindings(t *testing.T) {
	healthy := activity(map[string]hooks.EventActivity{"SessionStart": {Runs: 2, Emitted: 2}})

	t.Run("missing block", func(t *testing.T) {
		agent := codexAgent()
		agent.InstructionsWired = false
		got := findingWith(t, Diagnose(agent, healthy, Adoption{}, time.Now()), "No Gortex rule block")
		if got.Severity != SeverityWarn || got.Remedy != "gortex install" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("shadowed block", func(t *testing.T) {
		agent := codexAgent()
		agent.InstructionsShadowed = "/home/u/.codex/AGENTS.override.md"
		findings := Diagnose(agent, healthy, Adoption{}, time.Now())
		got := findingWith(t, findings, "instead of")
		if got.Severity != SeverityWarn {
			t.Errorf("severity=%s want WARN", got.Severity)
		}
		// A wired-but-shadowed block must not also be reported as missing:
		// the fix is different and the pair reads as contradictory.
		assertNoFindingWith(t, findings, "No Gortex rule block")
	})
}

func TestAdoptionFindings(t *testing.T) {
	healthy := activity(map[string]hooks.EventActivity{
		"SessionStart": {Runs: 1, Emitted: 1}, "UserPromptSubmit": {Runs: 1, Emitted: 1},
		"PreToolUse": {Runs: 1, Emitted: 1}, "PostToolUse": {Runs: 1, Emitted: 1},
	})

	cases := []struct {
		name     string
		adoption Adoption
		want     Severity
		substr   string
	}{
		{"all shell", Adoption{FilesFound: 3, InWindow: 3, ShellCalls: 20}, SeverityBlocker, "zero Gortex tool calls"},
		{"mostly shell", Adoption{FilesFound: 3, InWindow: 3, GortexCalls: 3, ShellCalls: 20, ShellReads: 15}, SeverityWarn, "13% of tool calls"},
		{"healthy", Adoption{FilesFound: 3, InWindow: 3, GortexCalls: 90, ShellCalls: 10}, SeverityOK, "90% of tool calls"},
		{"no transcripts", Adoption{}, SeverityInfo, "could not be measured"},
		{"no tool calls", Adoption{FilesFound: 4}, SeverityInfo, "could not be measured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findingWith(t, Diagnose(codexAgent(), healthy, tc.adoption, time.Now()), tc.substr)
			if got.Severity != tc.want {
				t.Errorf("severity=%s want %s (%q)", got.Severity, tc.want, got.Summary)
			}
		})
	}
}

// TestNoHooksConfiguredIsNotATrustProblem: with nothing installed there is
// nothing to trust, so the remedy is the installer.
func TestNoHooksConfiguredIsNotATrustProblem(t *testing.T) {
	agent := codexAgent()
	agent.Configured = map[string]int{}

	findings := Diagnose(agent, activity(nil), Adoption{}, time.Now())
	got := findingWith(t, findings, "no Gortex lifecycle hooks")
	if got.Severity != SeverityWarn || got.Remedy != "gortex install" {
		t.Errorf("got %+v", got)
	}
	assertNoFindingWith(t, findings, "trusted")
}

// TestDiagnoseScopesEvidenceToTheAgent is the reason the invocation log
// gained an agent dimension: on a machine running Claude Code and Codex side
// by side, a busy Claude session must not vouch for hooks Codex is skipping.
func TestDiagnoseScopesEvidenceToTheAgent(t *testing.T) {
	now := time.Now()
	busyClaude := hooks.EffectivenessSummary{
		Present:    true,
		Attributed: true,
		Events: map[string]hooks.EventActivity{
			"SessionStart": {Runs: 40, Emitted: 40}, "UserPromptSubmit": {Runs: 40, Emitted: 40},
			"PreToolUse": {Runs: 400, Emitted: 90}, "PostToolUse": {Runs: 40, Emitted: 8},
		},
		ByAgent: map[string]map[string]hooks.EventActivity{
			"claude-code": {
				"SessionStart": {Runs: 40, Emitted: 40}, "UserPromptSubmit": {Runs: 40, Emitted: 40},
				"PreToolUse": {Runs: 400, Emitted: 90}, "PostToolUse": {Runs: 40, Emitted: 8},
			},
		},
	}

	findings := Diagnose(codexAgent(), busyClaude, Adoption{FilesFound: 2, InWindow: 2, ShellCalls: 9}, now)
	got := findingWith(t, findings, "none has run")
	if got.Severity != SeverityBlocker {
		t.Fatalf("severity=%s want BLOCKER — codex's silence was masked by another agent", got.Severity)
	}

	// The same evidence, asked about the agent that *was* busy, is healthy.
	claude := codexAgent()
	claude.Agent = "claude-code"
	claude.RequiresTrust = false
	assertNoFindingWith(t, Diagnose(claude, busyClaude, Adoption{}, now), "none has run")
}

// TestDiagnoseFallsBackOnUnattributedLogs: right after an upgrade every row
// predates attribution, and doctor must not turn that missing metadata into a
// blocker against a working install.
func TestDiagnoseFallsBackOnUnattributedLogs(t *testing.T) {
	legacy := hooks.EffectivenessSummary{
		Present:          true,
		Attributed:       false,
		UnattributedRows: 4,
		Events: map[string]hooks.EventActivity{
			"SessionStart": {Runs: 4, Emitted: 4}, "UserPromptSubmit": {Runs: 4, Emitted: 4},
			"PreToolUse": {Runs: 4, Emitted: 2}, "PostToolUse": {Runs: 4, Emitted: 2},
		},
	}

	findings := Diagnose(codexAgent(), legacy, Adoption{FilesFound: 1, InWindow: 1, GortexCalls: 9, ShellCalls: 1}, time.Now())
	assertNoFindingWith(t, findings, "none has run")
	if HasBlocker(findings) {
		t.Errorf("a pre-attribution log is not evidence of failure: %+v", findings)
	}
}

// TestMixedAttributionDoesNotAccuse reproduces the state every machine passes
// through right after the upgrade that introduced per-agent attribution: a
// window holding thousands of unattributed rows plus a handful of attributed
// ones. PreToolUse fires constantly and attributes itself within minutes;
// SessionStart fires once per session and has not run again yet. Reading its
// empty attributed bucket as silence accuses a working install of exactly the
// failure doctor exists to detect.
func TestMixedAttributionDoesNotAccuse(t *testing.T) {
	now := time.Now()
	mixed := hooks.EffectivenessSummary{
		Present:          true,
		Attributed:       true, // one attributed row is enough to flip this
		UnattributedRows: 17317,
		Events: map[string]hooks.EventActivity{
			"SessionStart": {Runs: 6, Emitted: 6}, "UserPromptSubmit": {Runs: 8, Emitted: 0},
			"PreToolUse": {Runs: 1700, Emitted: 400}, "PostToolUse": {Runs: 59, Emitted: 0},
		},
		// Only the constantly-firing hook has attributed rows yet.
		ByAgent: map[string]map[string]hooks.EventActivity{
			"codex": {"PreToolUse": {Runs: 15, Emitted: 3, DaemonKnown: 15, DaemonUp: 15}},
		},
		// Everything else in the window predates the field.
		Unattributed: map[string]hooks.EventActivity{
			"SessionStart": {Runs: 6, Emitted: 6}, "UserPromptSubmit": {Runs: 8},
			"PreToolUse": {Runs: 1685, Emitted: 397}, "PostToolUse": {Runs: 59},
		},
	}

	findings := Diagnose(codexAgent(), mixed, Adoption{FilesFound: 7, InWindow: 7, GortexCalls: 338, ShellCalls: 102}, now)

	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse"} {
		assertNoFindingWith(t, findings, event+" is configured but never ran")
	}
	if HasBlocker(findings) {
		t.Fatalf("mixed attribution is not evidence of failure: %+v", findings)
	}
}

// TestAttributedSilenceStillBlocks: once the window holds no unattributed
// runs of an event, an empty bucket really is silence and must still block —
// the guard above must not swallow the finding it exists to protect.
func TestAttributedSilenceStillBlocks(t *testing.T) {
	now := time.Now()
	clean := hooks.EffectivenessSummary{
		Present:    true,
		Attributed: true,
		Events: map[string]hooks.EventActivity{
			"PreToolUse": {Runs: 15, Emitted: 3},
		},
		ByAgent: map[string]map[string]hooks.EventActivity{
			"codex": {"PreToolUse": {Runs: 15, Emitted: 3, DaemonKnown: 15, DaemonUp: 15}},
		},
		Unattributed: map[string]hooks.EventActivity{},
	}

	findings := Diagnose(codexAgent(), clean, Adoption{FilesFound: 1, InWindow: 1, GortexCalls: 9, ShellCalls: 1}, now)
	got := findingWith(t, findings, "SessionStart is configured but never ran")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if !strings.Contains(got.Remedy, "/hooks") {
		t.Errorf("remedy=%q", got.Remedy)
	}
}

// TestWhollyAmbiguousWindowReportsUncertainty: no attributed runs at all for
// this agent, but the window has unattributed runs — doctor must say it
// cannot tell rather than either accuse or stay silent.
func TestWhollyAmbiguousWindowReportsUncertainty(t *testing.T) {
	now := time.Now()
	summary := hooks.EffectivenessSummary{
		Present:          true,
		Attributed:       true,
		UnattributedRows: 40,
		Events: map[string]hooks.EventActivity{
			"SessionStart": {Runs: 10}, "UserPromptSubmit": {Runs: 10},
			"PreToolUse": {Runs: 10}, "PostToolUse": {Runs: 10},
		},
		ByAgent: map[string]map[string]hooks.EventActivity{
			"claude-code": {"Stop": {Runs: 3}},
		},
		Unattributed: map[string]hooks.EventActivity{
			"SessionStart": {Runs: 10}, "UserPromptSubmit": {Runs: 10},
			"PreToolUse": {Runs: 10}, "PostToolUse": {Runs: 10},
		},
	}

	findings := Diagnose(codexAgent(), summary, Adoption{}, now)
	got := findingWith(t, findings, "predate per-agent attribution")
	if got.Severity != SeverityInfo {
		t.Errorf("severity=%s want INFO — uncertainty is not a failure", got.Severity)
	}
	if HasBlocker(findings) {
		t.Errorf("must not accuse on evidence that cannot decide: %+v", findings)
	}
}

// TestHooksTurnedOffPreemptsTheTrustRemedy — an agent configured not to run
// hooks explains an idle install completely. Sending the user to approve
// hooks, or to check PATH, would be advice about something that is not broken.
func TestHooksTurnedOffPreemptsTheTrustRemedy(t *testing.T) {
	agent := codexAgent()
	agent.HooksDisabled = true
	agent.HooksDisabledRemedy = "set `hooks = true` under `[features]`"

	findings := Diagnose(agent, activity(nil), Adoption{}, time.Now())

	got := findingWith(t, findings, "hook execution is turned off")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if got.Remedy != agent.HooksDisabledRemedy {
		t.Errorf("remedy=%q want %q", got.Remedy, agent.HooksDisabledRemedy)
	}
	assertNoFindingWith(t, findings, "none has run")
	assertNoFindingWith(t, findings, "has no Gortex lifecycle hooks configured")
}

// TestHooksTurnedOffWithNothingConfigured keeps the missing-install case
// ahead of the feature switch: there is nothing for the switch to stop.
func TestHooksTurnedOffWithNothingConfigured(t *testing.T) {
	agent := codexAgent()
	agent.Configured = map[string]int{}
	agent.HooksDisabled = true
	agent.HooksDisabledRemedy = "set `hooks = true` under `[features]`"

	findings := Diagnose(agent, activity(nil), Adoption{}, time.Now())

	// Both facts matter: there is nothing installed, AND installing would not
	// help until execution is switched back on.
	findingWith(t, findings, "has no Gortex lifecycle hooks configured")
	off := findingWith(t, findings, "hook execution turned off")
	if off.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", off.Severity)
	}
}

// TestDuplicateHookEventIsAWarning — an event declared in two files of one
// layer runs twice, because the agent merges the files. Nothing else on the
// machine names that except a startup warning inside the agent.
func TestDuplicateHookEventIsAWarning(t *testing.T) {
	agent := codexAgent()
	agent.DuplicateHookEvents = []string{"SessionStart"}
	agent.DuplicateHookSources = []string{"/home/u/.codex/hooks.json", "/home/u/.codex/config.toml"}
	agent.ManagedHookFile = "/home/u/.codex/config.toml"

	findings := Diagnose(agent, activity(map[string]hooks.EventActivity{
		"SessionStart": {Runs: 3, Emitted: 3},
	}), Adoption{}, time.Now())

	got := findingWith(t, findings, "declares SessionStart in both")
	if got.Severity != SeverityWarn {
		t.Errorf("severity=%s want WARN", got.Severity)
	}
	for _, want := range []string{"hooks.json", "config.toml", "once per declaration"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary should name %q, got %q", want, got.Summary)
		}
	}
	// Cleaning the managed file is undone by the next install, so the remedy
	// has to point at the other one.
	if !strings.Contains(got.Remedy, "remove SessionStart from /home/u/.codex/hooks.json") {
		t.Errorf("remedy should send the user to the unmanaged file, got %q", got.Remedy)
	}
	if strings.Contains(got.Remedy, "remove SessionStart from /home/u/.codex/config.toml") {
		t.Errorf("remedy points at the file `gortex install` rewrites: %q", got.Remedy)
	}
}

// TestDisjointHookSourcesAreNotADuplicate is the case that made the per-file
// version of this finding dangerous: two files, no shared event, nothing
// running twice — and a remedy phrased around the files would have deleted
// four working hooks to fix a duplicate that did not exist.
func TestDisjointHookSourcesAreNotADuplicate(t *testing.T) {
	agent := codexAgent()
	agent.DuplicateHookEvents = nil
	agent.DuplicateHookSources = []string{"/home/u/.codex/hooks.json", "/home/u/.codex/config.toml"}
	agent.ManagedHookFile = "/home/u/.codex/config.toml"

	findings := Diagnose(agent, activity(map[string]hooks.EventActivity{
		"SessionStart": {Runs: 3, Emitted: 3},
	}), Adoption{}, time.Now())

	assertNoFindingWith(t, findings, "in both")
}

// TestSingleHookSourceIsNotADuplicate — the ordinary install declares its
// hooks in exactly one file. One declaring file can never be a duplicate, no
// matter what events it carries.
func TestSingleHookSourceIsNotADuplicate(t *testing.T) {
	agent := codexAgent()
	agent.DuplicateHookEvents = []string{"SessionStart"}
	agent.DuplicateHookSources = []string{"/home/u/.codex/config.toml"}
	agent.ManagedHookFile = "/home/u/.codex/config.toml"

	findings := Diagnose(agent, activity(map[string]hooks.EventActivity{
		"SessionStart": {Runs: 3, Emitted: 3},
	}), Adoption{}, time.Now())

	assertNoFindingWith(t, findings, "in both")
}

// TestHooksTurnedOffSuppressesThePerEventTrustRemedy closes the gap the switch
// alone left open: `ran > 0` survives the switch being flipped, because runs
// earlier in the window still count, and the per-event rule then handed out
// the trust remedy event by event on a machine that simply has hooks off.
func TestHooksTurnedOffSuppressesThePerEventTrustRemedy(t *testing.T) {
	agent := codexAgent()
	agent.HooksDisabled = true
	agent.HooksDisabledRemedy = "set `hooks = true` under `[features]`"

	findings := Diagnose(agent, activity(map[string]hooks.EventActivity{
		"SessionStart": {Runs: 3, Emitted: 3},
	}), Adoption{}, time.Now())

	findingWith(t, findings, "hook execution is turned off")
	assertNoFindingWith(t, findings, "configured but never ran, while other hooks did")
	for _, f := range findings {
		if strings.Contains(f.Remedy, "/hooks") {
			t.Errorf("trust remedy offered while execution is turned off: %+v", f)
		}
	}
}

// TestPostToolUseSilenceUnderDenyPostureIsExpected is #630: the deny posture
// narrows the PostToolUse matcher to the Gortex MCP tools, so the enrichment
// path can never fire and zero injections is the designed steady state.
// Warning on it permanently accuses every healthy deny-mode install.
func TestPostToolUseSilenceUnderDenyPostureIsExpected(t *testing.T) {
	agent := codexAgent()
	agent.Agent = "claude-code"
	agent.PostToolUseDenyPosture = true

	findings := Diagnose(agent, activity(map[string]hooks.EventActivity{
		"SessionStart":     {Runs: 4, Emitted: 4, DaemonKnown: 4, DaemonUp: 4},
		"UserPromptSubmit": {Runs: 9, Emitted: 3, DaemonKnown: 9, DaemonUp: 9},
		"PreToolUse":       {Runs: 9, Emitted: 3, DaemonKnown: 9, DaemonUp: 9},
		"PostToolUse":      {Runs: 9, Emitted: 0, DaemonKnown: 9, DaemonUp: 9},
	}), Adoption{GortexCalls: 20, ShellCalls: 2}, time.Now())

	got := findingWith(t, findings, "PostToolUse ran 9 time(s) and injected context 0 times")
	if got.Severity != SeverityInfo {
		t.Errorf("severity=%s want INFO — deny-posture silence is expected, not a fault", got.Severity)
	}
	if !strings.Contains(got.Summary, "deny posture") {
		t.Errorf("summary should name the posture so the reader learns why: %q", got.Summary)
	}
	assertNoFindingWith(t, findings, "nothing to say")
}

// TestPostToolUseSilenceStillWarnsUnderEnrich is the guard the downgrade
// must not swallow: under enrich the matcher names the native read-shaped
// tools, so zero emissions genuinely means the enrichment path broke.
func TestPostToolUseSilenceStillWarnsUnderEnrich(t *testing.T) {
	agent := codexAgent()
	agent.Agent = "claude-code"
	agent.PostToolUseDenyPosture = false

	findings := Diagnose(agent, activity(map[string]hooks.EventActivity{
		"SessionStart":     {Runs: 4, Emitted: 4, DaemonKnown: 4, DaemonUp: 4},
		"UserPromptSubmit": {Runs: 9, Emitted: 3, DaemonKnown: 9, DaemonUp: 9},
		"PreToolUse":       {Runs: 9, Emitted: 3, DaemonKnown: 9, DaemonUp: 9},
		"PostToolUse":      {Runs: 9, Emitted: 0, DaemonKnown: 9, DaemonUp: 9},
	}), Adoption{GortexCalls: 20, ShellCalls: 2}, time.Now())

	got := findingWith(t, findings, "nothing to say")
	if got.Severity != SeverityWarn {
		t.Errorf("severity=%s want WARN — enrich-mode silence is real breakage", got.Severity)
	}
}
