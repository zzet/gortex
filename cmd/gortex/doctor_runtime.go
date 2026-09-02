package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/agents/copilotcli"
	"github.com/zzet/gortex/internal/agents/opencode"
	"github.com/zzet/gortex/internal/doctor"
	"github.com/zzet/gortex/internal/hooks"
	"github.com/zzet/gortex/internal/savings"
)

// doctor_runtime.go is the half of `gortex doctor` that reads evidence rather
// than configuration: did the hooks run, is the agent calling Gortex tools,
// and what did the savings ledger record. Everything here is read-only and
// degrades to "no data" — doctor has to work on exactly the broken machines
// that make people run it.

// errDoctorBlocker makes the command exit non-zero when a blocker is found.
// SilenceUsage/SilenceErrors on the command keep cobra from printing it: the
// findings block is the message.
var errDoctorBlocker = errors.New("doctor found a blocking problem")

type doctorRuntime struct {
	Window   string                     `json:"window"`
	Since    time.Time                  `json:"since"`
	Redacted bool                       `json:"redacted"`
	Hooks    hooks.EffectivenessSummary `json:"hooks"`
	Agents   []doctorAgentRuntime       `json:"agents"`
	Savings  doctorSavings              `json:"savings"`
}

// doctorAgentRuntime is one agent's runtime slice. Reporting a list rather
// than a single agent is the difference between "here is your machine" and
// "here is the one agent doctor happens to know about" — the invocation log
// has always been partitioned per agent, and only the rendering singled Codex
// out.
type doctorAgentRuntime struct {
	Agent      string                         `json:"agent"`
	Install    doctorAgentInstall             `json:"install"`
	HookEvents []string                       `json:"hook_events"`
	Activity   map[string]hooks.EventActivity `json:"activity"`
	Adoption   doctor.Adoption                `json:"adoption"`
	Findings   []doctor.Finding               `json:"findings"`
}

// doctorAgentInstall is the shape both adapters' Inspect calls lower into, so
// the renderer does not branch per agent.
type doctorAgentInstall struct {
	ConfigPath    string         `json:"config_path"`
	ConfigPresent bool           `json:"config_present"`
	MCPServer     bool           `json:"mcp_server"`
	Hooks         map[string]int `json:"hooks"`
	// HookSources is the per-file breakdown behind Hooks. An agent that reads
	// hooks from more than one file needs it: the merged count alone cannot
	// say which file a hook came from, nor that two of them declare the same
	// hook twice.
	HookSources []doctorHookSource `json:"hook_sources,omitempty"`
	// HooksDisabled marks a config that switches hook execution off.
	HooksDisabled     bool   `json:"hooks_disabled,omitempty"`
	InstructionsPath  string `json:"instructions_path,omitempty"`
	InstructionsWired bool   `json:"instructions_wired"`
	ShadowedBy        string `json:"shadowed_by,omitempty"`
}

// doctorHookSource is one file an agent reads hook declarations from.
type doctorHookSource struct {
	Path  string         `json:"path"`
	Hooks map[string]int `json:"hooks,omitempty"`
	Total int            `json:"total"`
}

type doctorSavings struct {
	Available bool             `json:"available"`
	Error     string           `json:"error,omitempty"`
	Events    int              `json:"events"`
	Saved     int64            `json:"saved"`
	Returned  int64            `json:"returned"`
	NoClient  int              `json:"events_without_client"`
	NoModel   int              `json:"events_without_model"`
	ByTool    []doctorDimTotal `json:"by_tool,omitempty"`
	ByClient  []doctorDimTotal `json:"by_client,omitempty"`
	ByModel   []doctorDimTotal `json:"by_model,omitempty"`
}

type doctorDimTotal struct {
	Name  string `json:"name"`
	Calls int64  `json:"calls"`
	Saved int64  `json:"saved"`
}

func collectRuntime(home string, days int) doctorRuntime {
	if days < 1 {
		days = 1
	}
	now := time.Now()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)

	activity, err := hooks.ReadEffectiveness(since)
	if err != nil {
		// A log we cannot read is not fatal; the other sections still carry
		// signal, and the empty summary renders as "no hook has ever run".
		activity.Path = hooks.EffectivenessLogPath()
	}

	out := doctorRuntime{
		Window:   fmt.Sprintf("last %dd", days),
		Since:    since,
		Redacted: doctorRedact,
		Hooks:    activity,
		Savings:  collectSavings(since),
	}
	for _, probe := range doctorAgentProbes(home) {
		out.Agents = append(out.Agents, probe.run(since, activity, now))
	}
	if doctorRedact {
		out.Hooks.Path = doctorPath(out.Hooks.Path)
	}
	return out
}

// agentProbe collects one agent's runtime slice. The agent name is carried
// beside the closure rather than only inside it so the coverage guard in
// doctor_runtime_test.go can enumerate what doctor speaks about without
// running a probe — a probe reads the real home and scans session
// transcripts, which is not something a unit test should be doing.
type agentProbe struct {
	agent string
	run   func(since time.Time, activity hooks.EffectivenessSummary, now time.Time) doctorAgentRuntime
}

// doctorAgentProbes returns a probe per agent doctor can speak about. Adding
// an agent here is the whole cost of covering it — the finding rules, the
// per-agent log partition, and the renderer are all already generic.
//
// The `agent` string is not cosmetic: it is the key the invocation log is
// partitioned by (hooks.EffectivenessSummary.ForAgent), and the writer side
// stamps whatever `gortex hook --agent=<x>` was invoked with, normalised
// through hooks.SetAgent. A probe naming an agent the hook handler does not
// report reads zero runs forever and turns a healthy install into a
// permanent "configured but none has run" blocker, so each entry below uses
// the adapter's own Name constant — the same value the adapters bake into
// their hook command lines.
func doctorAgentProbes(home string) []agentProbe {
	return []agentProbe{
		{agent: codex.Name, run: func(since time.Time, activity hooks.EffectivenessSummary, now time.Time) doctorAgentRuntime {
			state := codex.Inspect(home)
			return buildAgentRuntime(agentRuntimeInput{
				agent:      codex.Name,
				hookEvents: codex.HookEvents,
				install: doctorAgentInstall{
					ConfigPath: state.ConfigPath, ConfigPresent: state.ConfigPresent,
					MCPServer: state.MCPServer, Hooks: state.Hooks,
					HookSources: codexHookSources(state), HooksDisabled: state.HooksDisabled,
					InstructionsPath: state.InstructionsPath, InstructionsWired: state.InstructionsWired,
					ShadowedBy: state.ShadowedBy,
				},
				// Codex is the one harness that gates hook execution on an
				// explicit per-hook approval.
				requiresTrust:       true,
				trustRemedy:         codex.TrustRemedy,
				hooksDisabledRemedy: codex.HooksDisabledRemedy,
				adoption:            doctor.ScanCodexSessions(doctor.CodexHome(), since, 10),
			}, activity, now)
		}},
		{agent: hooks.AgentClaudeCode, run: func(since time.Time, activity hooks.EffectivenessSummary, now time.Time) doctorAgentRuntime {
			state := claudecode.Inspect(home)
			return buildAgentRuntime(agentRuntimeInput{
				agent:      hooks.AgentClaudeCode,
				hookEvents: claudecode.HookEvents,
				install: doctorAgentInstall{
					ConfigPath: state.ConfigPath, ConfigPresent: state.ConfigPresent,
					MCPServer: state.MCPServer, Hooks: state.Hooks,
					InstructionsPath: state.InstructionsPath, InstructionsWired: state.InstructionsWired,
				},
				// Deny-posture installs never expect PostToolUse context
				// injections (its matcher only watches Gortex tools); the
				// flag keeps Diagnose from warning on that steady state.
				postToolUseDeny: state.PostToolUseDenyPosture,
				adoption:        doctor.ScanClaudeSessions(doctor.ClaudeHome(), since, 10),
			}, activity, now)
		}},
		{agent: copilotcli.Name, run: func(since time.Time, activity hooks.EffectivenessSummary, now time.Time) doctorAgentRuntime {
			state := copilotcli.Inspect(home)
			return buildAgentRuntime(agentRuntimeInput{
				agent:      copilotcli.Name,
				hookEvents: copilotcli.HookEvents,
				install: doctorAgentInstall{
					ConfigPath: state.ConfigPath, ConfigPresent: state.ConfigPresent,
					MCPServer: state.MCPServer, Hooks: state.Hooks,
					InstructionsPath: state.InstructionsPath, InstructionsWired: state.InstructionsWired,
				},
				// No per-hook trust gate: the Copilot CLI runs whatever is in
				// hooks/*.json, so "configured but never ran" here means the
				// hook is broken rather than unapproved, and pointing the user
				// at an approval flow that does not exist would be worse than
				// the generic remedy.
				//
				// STOP-LINE: no adoption scan. Copilot CLI keeps its session
				// history in ~/.copilot/session-store.db — SQLite with an
				// undocumented, unversioned schema and no export path.
				// Reverse-engineering private tables buys a number that
				// silently turns into zeros after any patch release, so the
				// Adoption slice stays at its zero value on purpose and
				// doctor reports "adoption could not be measured".
			}, activity, now)
		}},
		{agent: opencode.Name, run: func(since time.Time, activity hooks.EffectivenessSummary, now time.Time) doctorAgentRuntime {
			state := opencode.Inspect(home)
			return buildAgentRuntime(agentRuntimeInput{
				agent: opencode.Name,
				// Three events, not four: opencode.HookEvents deliberately
				// omits PostToolUse because the plugin's after-hook never
				// shells the bridge and so can never log a run.
				hookEvents: opencode.HookEvents,
				install: doctorAgentInstall{
					ConfigPath: state.ConfigPath, ConfigPresent: state.ConfigPresent,
					MCPServer: state.MCPServer, Hooks: state.Hooks,
					// No InstructionsPath: OpenCode has no user-level rules
					// file Gortex writes (its routing block is per-repo in
					// AGENTS.md), and naming one would produce a permanent
					// "no Gortex rule block" warning about a file nothing
					// ever writes.
				},
				// No per-hook trust gate — the plugin runs as soon as
				// OpenCode loads it.
				//
				// STOP-LINE: no adoption scan. OpenCode keeps no session log
				// Gortex can read, so there is no transcript to count tool
				// calls from. Passing no adoption data is the honest answer;
				// inventing one would be a fabricated metric.
			}, activity, now)
		}},
	}
}

// codexHookSources lowers the adapter's per-file hook breakdown into the
// renderer's shape.
func codexHookSources(state codex.InstallState) []doctorHookSource {
	out := make([]doctorHookSource, 0, len(state.HookSources))
	for _, src := range state.HookSources {
		out = append(out, doctorHookSource{Path: src.Path, Hooks: src.Hooks, Total: src.Total})
	}
	return out
}

// duplicateHookDeclarations reports the lifecycle events declared in more than
// one hook file, and the files that declare them.
//
// The unit is the event, not the file. Two files each carrying Gortex hooks is
// a valid split as long as they declare different events — only an event
// present in both is merged into two runs. Answering this per file instead
// would flag that split and send the user to delete working hooks.
func duplicateHookDeclarations(sources []doctorHookSource) (events, files []string) {
	perEvent := map[string]int{}
	for _, src := range sources {
		for event, n := range src.Hooks {
			if n > 0 {
				perEvent[event]++
			}
		}
	}
	for event, declaringFiles := range perEvent {
		if declaringFiles > 1 {
			events = append(events, event)
		}
	}
	if len(events) == 0 {
		return nil, nil
	}
	sort.Strings(events)
	for _, src := range sources {
		if src.Total > 0 {
			files = append(files, src.Path)
		}
	}
	return events, files
}

type agentRuntimeInput struct {
	agent               string
	hookEvents          []string
	install             doctorAgentInstall
	postToolUseDeny     bool
	requiresTrust       bool
	trustRemedy         string
	hooksDisabledRemedy string
	adoption            doctor.Adoption
}

func buildAgentRuntime(in agentRuntimeInput, activity hooks.EffectivenessSummary, now time.Time) doctorAgentRuntime {
	if doctorRedact {
		in.install.ConfigPath = doctorPath(in.install.ConfigPath)
		in.install.InstructionsPath = doctorPath(in.install.InstructionsPath)
		in.install.ShadowedBy = doctorPath(in.install.ShadowedBy)
		for i := range in.install.HookSources {
			in.install.HookSources[i].Path = doctorPath(in.install.HookSources[i].Path)
		}
	}
	out := doctorAgentRuntime{
		Agent:      in.agent,
		Install:    in.install,
		HookEvents: in.hookEvents,
		Activity:   activity.ForAgent(in.agent),
		Adoption:   in.adoption,
	}
	duplicateEvents, duplicateFiles := duplicateHookDeclarations(in.install.HookSources)
	out.Findings = doctor.Diagnose(doctor.AgentHooks{
		Agent:                  in.agent,
		Configured:             in.install.Hooks,
		PostToolUseDenyPosture: in.postToolUseDeny,
		RequiresTrust:          in.requiresTrust,
		TrustRemedy:            in.trustRemedy,
		HooksDisabled:          in.install.HooksDisabled,
		HooksDisabledRemedy:    in.hooksDisabledRemedy,
		DuplicateHookEvents:    duplicateEvents,
		// ConfigPath is the file the adapter's installer rewrites, so it is
		// the one a duplicate must NOT be cleaned out of.
		DuplicateHookSources: duplicateFiles,
		ManagedHookFile:      in.install.ConfigPath,
		InstructionsPath:     in.install.InstructionsPath,
		InstructionsWired:    in.install.InstructionsWired,
		InstructionsShadowed: in.install.ShadowedBy,
	}, activity, in.adoption, now)
	if doctorRedact {
		out.Adoption = redactAdoption(out.Adoption)
	}
	return out
}

func collectSavings(since time.Time) doctorSavings {
	out := doctorSavings{}
	store, err := savings.Open(savings.DefaultDBPath())
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer store.Close()
	out.Available = true

	events, err := store.EventsSince(since)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Events = len(events)
	for _, e := range events {
		out.Saved += e.Saved
		out.Returned += e.Returned
		if e.Client == "" {
			out.NoClient++
		}
		if e.Model == "" {
			out.NoModel++
		}
	}
	if tools, err := store.ToolTotals(since); err == nil {
		for _, t := range tools {
			out.ByTool = append(out.ByTool, doctorDimTotal{Name: t.Tool, Calls: t.CallsCounted, Saved: t.TokensSaved})
		}
	}
	if clients, err := store.ClientTotals(since); err == nil {
		for _, c := range clients {
			out.ByClient = append(out.ByClient, doctorDimTotal{Name: c.Name, Calls: c.CallsCounted, Saved: c.TokensSaved})
		}
	}
	if models, err := store.ModelTotals(since); err == nil {
		for _, m := range models {
			out.ByModel = append(out.ByModel, doctorDimTotal{Name: m.Name, Calls: m.CallsCounted, Saved: m.TokensSaved})
		}
	}
	return out
}

// redactAdoption hashes the workspace identifiers so the report can be pasted
// into an issue. Tool names, counts, and timings are never sensitive and are
// always left intact — redacting them would defeat the point of sharing it.
func redactAdoption(a doctor.Adoption) doctor.Adoption {
	a.Root = doctorPath(a.Root)
	for i := range a.Sessions {
		a.Sessions[i].CWD = doctorPath(a.Sessions[i].CWD)
		a.Sessions[i].Branch = redactName(a.Sessions[i].Branch)
	}
	return a
}

// redactName hashes a value that is not a path — a branch name can carry a
// ticket id or a customer name just as readily as a repo path can.
func redactName(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return "<" + hex.EncodeToString(sum[:])[:8] + ">"
}

func printDoctorRuntime(w io.Writer, r doctorRuntime) {
	fmt.Fprintf(w, "Gortex doctor — runtime evidence (%s):\n", r.Window)

	for _, agent := range r.Agents {
		printAgentRuntime(w, agent, r.Hooks)
	}
	printSavings(w, r.Savings)

	fmt.Fprintln(w, "  findings")
	for _, agent := range r.Agents {
		for _, f := range agent.Findings {
			glyph := "·"
			switch f.Severity {
			case doctor.SeverityBlocker:
				glyph = glyphCross
			case doctor.SeverityWarn:
				glyph = glyphWarn
			case doctor.SeverityOK:
				glyph = glyphCheck
			}
			fmt.Fprintf(w, "    %s %-7s [%s] %s\n", glyph, f.Severity, agent.Agent, f.Summary)
			if f.Remedy != "" {
				fmt.Fprintf(w, "              → %s\n", f.Remedy)
			}
		}
	}
	if !r.Redacted {
		fmt.Fprintln(w, "\n  sharing this? re-run with --redact to hash repo paths and branch names.")
	}
	fmt.Fprintln(w)
}

func printAgentRuntime(w io.Writer, a doctorAgentRuntime, summary hooks.EffectivenessSummary) {
	fmt.Fprintf(w, "\n  %s — install\n", a.Agent)
	if a.Install.ConfigPresent {
		fmt.Fprintf(w, "    %s mcp server registered\n", mark(a.Install.MCPServer))
	} else {
		fmt.Fprintf(w, "    %s %s missing\n", glyphAbsent, a.Install.ConfigPath)
	}
	// Hook lines print whenever the config carries them OR hooks are wired
	// at all. The second half is not redundant: OpenCode has no hook
	// configuration to read back — its hook surface is the bridge plugin,
	// which lives nowhere near ConfigPath — so gating on ConfigPresent
	// would render a fully wired OpenCode install as one line about a
	// missing opencode.json and nothing else.
	if a.Install.ConfigPresent || anyHookConfigured(a.Install.Hooks) {
		for _, event := range a.HookEvents {
			fmt.Fprintf(w, "    %s hook %-17s %d configured\n", mark(a.Install.Hooks[event] > 0), event, a.Install.Hooks[event])
		}
	}
	// Naming the files the counts came from is what bounds them. Without it a
	// zero reads as "nothing is configured anywhere" when it can only ever
	// mean "nothing in the files listed here".
	if len(a.Install.HookSources) > 0 {
		parts := make([]string, 0, len(a.Install.HookSources))
		for _, src := range a.Install.HookSources {
			parts = append(parts, fmt.Sprintf("%s (%d)", src.Path, src.Total))
		}
		fmt.Fprintf(w, "      counted in %s\n", joinComma(parts))
	}
	if a.Install.HooksDisabled {
		fmt.Fprintf(w, "    %s hook execution is turned off in %s\n", glyphWarn, a.Install.ConfigPath)
	}
	if a.Install.InstructionsPath != "" {
		fmt.Fprintf(w, "    %s rule block in %s\n", mark(a.Install.InstructionsWired), a.Install.InstructionsPath)
	}
	if a.Install.ShadowedBy != "" {
		fmt.Fprintf(w, "    %s shadowed by %s\n", glyphWarn, a.Install.ShadowedBy)
	}

	fmt.Fprintf(w, "\n  %s — hook activity\n", a.Agent)
	if !summary.Present {
		fmt.Fprintf(w, "    no %s — no hook has ever run on this machine\n", summary.Path)
	} else {
		fmt.Fprintf(w, "    %-18s %7s %9s %10s %8s  %s\n", "event", "runs", "injected", "daemon-up", "unattrib", "last seen")
		for _, event := range a.HookEvents {
			stats := a.Activity[event]
			daemon := "n/a"
			if stats.DaemonKnown > 0 {
				daemon = fmt.Sprintf("%d/%d", stats.DaemonUp, stats.DaemonKnown)
			}
			// A zero in `runs` is only alarming when `unattrib` is zero too;
			// showing them side by side keeps the reader from reaching the
			// conclusion the findings deliberately withheld.
			ambiguous := summary.AmbiguousRuns(event)
			seen := stats.LastSeen
			if seen.IsZero() {
				seen = summary.Unattributed[event].LastSeen
			}
			last := "never"
			if !seen.IsZero() {
				last = seen.Format(time.RFC3339)[:19]
			}
			fmt.Fprintf(w, "    %-18s %7d %9d %10s %8d  %s\n", event, stats.Runs, stats.Emitted, daemon, ambiguous, last)
		}
	}

	fmt.Fprintf(w, "\n  %s — adoption (what the model actually called)\n", a.Agent)
	if a.Adoption.FilesFound == 0 {
		fmt.Fprintf(w, "    no session transcripts under %s\n", a.Adoption.Root)
	} else {
		fmt.Fprintf(w, "    sessions with tool calls   %d of %d in window\n", a.Adoption.InWindow, a.Adoption.FilesFound)
		fmt.Fprintf(w, "    gortex MCP calls           %d\n", a.Adoption.GortexCalls)
		fmt.Fprintf(w, "    other MCP calls            %d\n", a.Adoption.OtherMCP)
		fmt.Fprintf(w, "    native read/shell calls    %d (%d read/search shaped)\n", a.Adoption.ShellCalls, a.Adoption.ShellReads)
		if len(a.Adoption.Tools) > 0 {
			fmt.Fprintf(w, "    gortex tools used          %s\n", topCounts(a.Adoption.Tools, 8))
		}
		if len(a.Adoption.Models) > 0 {
			fmt.Fprintf(w, "    models                     %s\n", topCounts(a.Adoption.Models, 4))
		}
	}
	fmt.Fprintln(w)
}

func printSavings(w io.Writer, sv doctorSavings) {
	fmt.Fprintln(w, "  savings ledger")
	switch {
	case !sv.Available:
		fmt.Fprintf(w, "    unavailable: %s\n", sv.Error)
	case sv.Events == 0:
		fmt.Fprintln(w, "    no recorded events in window")
	default:
		baseline := sv.Saved + sv.Returned
		pct := 0.0
		if baseline > 0 {
			pct = 100 * float64(sv.Saved) / float64(baseline)
		}
		fmt.Fprintf(w, "    events                     %d\n", sv.Events)
		fmt.Fprintf(w, "    saved / baseline           %d / %d (%.1f%%)\n", sv.Saved, baseline, pct)
		fmt.Fprintf(w, "    without client / model     %d / %d\n", sv.NoClient, sv.NoModel)
		for _, group := range []struct {
			label string
			rows  []doctorDimTotal
		}{{"by tool", sv.ByTool}, {"by client", sv.ByClient}, {"by model", sv.ByModel}} {
			label := group.label
			for i, row := range group.rows {
				if i >= 6 {
					break
				}
				fmt.Fprintf(w, "    %-10s %-24s %6d calls  %10d saved\n", label, doctorTruncate(row.Name, 24), row.Calls, row.Saved)
				label = ""
			}
		}
	}
	fmt.Fprintln(w, "    note: saved = whole-file tokens − returned. The read-family tools book the")
	fmt.Fprintln(w, "          one file they stand in for; explore / search / relations / trace book")
	fmt.Fprintln(w, "          the files their page cites, each credited once per session. Reading a")
	fmt.Fprintln(w, "          whole file still records 0 — it displaces nothing.")
	fmt.Fprintln(w)
}

func topCounts(counts map[string]int, limit int) string {
	type kv struct {
		name string
		n    int
	}
	rows := make([]kv, 0, len(counts))
	for name, n := range counts {
		rows = append(rows, kv{name, n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf("%s×%d", row.name, row.n))
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func doctorTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// anyHookConfigured reports whether the agent declares at least one Gortex
// hook, regardless of which file (if any) that declaration lives in.
func anyHookConfigured(hooks map[string]int) bool {
	for _, n := range hooks {
		if n > 0 {
			return true
		}
	}
	return false
}

func mark(ok bool) string {
	if ok {
		return glyphCheck
	}
	return glyphCross
}

// silenceDoctorErrors keeps the blocker exit status from printing a second
// error line under the findings block.
func silenceDoctorErrors(cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.SilenceUsage = true
		c.SilenceErrors = true
	}
}
