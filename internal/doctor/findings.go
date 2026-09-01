package doctor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/hooks"
)

// Severity orders findings by how much they cost the user.
type Severity string

const (
	// SeverityBlocker means the integration is not working. Doctor exits
	// non-zero when one is present so CI can gate on it.
	SeverityBlocker Severity = "BLOCKER"
	// SeverityWarn means it works but is degraded or half-wired.
	SeverityWarn Severity = "WARN"
	SeverityOK   Severity = "OK"
	SeverityInfo Severity = "INFO"
)

// Finding is one diagnosis. Remedy is the exact command or step that fixes
// it — a finding the reader cannot act on is a complaint, not a diagnosis.
type Finding struct {
	Severity Severity `json:"severity"`
	Summary  string   `json:"summary"`
	Remedy   string   `json:"remedy,omitempty"`
}

// agentActivity is one harness's slice of the invocation log, so the finding
// rules below cannot accidentally read another agent's rows.
type agentActivity struct {
	events  map[string]hooks.EventActivity
	summary hooks.EffectivenessSummary
}

// ambiguous reports that the window holds runs of this event that cannot be
// attributed either way, so silence cannot be concluded from an empty bucket.
//
// Zero attributed runs is not evidence on its own. A log is mixed for as long
// as the window still reaches back past the upgrade that introduced
// attribution, and during that overlap an event may have run with no agent
// recorded — most visibly for events that fire once per session, where a busy
// PreToolUse stream attributes itself within minutes while SessionStart waits
// for the next session. Concluding silence there accuses a working install.
func (a agentActivity) ambiguous(event string) bool {
	return a.events[event].Runs == 0 && a.summary.AmbiguousRuns(event) > 0
}

// AgentHooks is what an adapter's config declares, per lifecycle event.
type AgentHooks struct {
	// Agent is the adapter name ("codex").
	Agent string
	// Configured counts declared hook entries per event name.
	Configured map[string]int
	// RequiresTrust marks agents that gate hook execution behind an explicit
	// per-hook approval, where "declared but never ran" is the expected
	// signature of an unapproved hook rather than a broken install.
	RequiresTrust bool
	// TrustRemedy is how the user approves them.
	TrustRemedy string
	// InstructionsPath is the agent's user-level rules file, and
	// InstructionsWired reports whether Gortex's block is in it.
	InstructionsPath  string
	InstructionsWired bool
	// InstructionsShadowed is set when the agent will read a different file
	// instead, making the block above dead text.
	InstructionsShadowed string
	// PostToolUseDenyPosture is set when the installed PostToolUse hook runs
	// under the deny posture — its matcher names only the Gortex MCP tools,
	// never the native read-shaped tools the enrichment path keys on. There
	// the hook exists to watch localization receipts and is never expected
	// to inject context (PreToolUse already redirects native reads), so
	// "ran but never emitted" is the designed steady state, not a
	// degradation. Agents without a posture concept leave it false and keep
	// the original warning semantics.
	PostToolUseDenyPosture bool
	// HooksDisabled marks a harness whose configuration switches hook
	// execution off wholesale. It explains an idle install completely, so it
	// must pre-empt the trust and PATH remedies — both of which would send
	// the user to fix something that is not broken.
	HooksDisabled bool
	// HooksDisabledRemedy is how to switch execution back on.
	HooksDisabledRemedy string
	// DuplicateHookEvents names the lifecycle events declared in more than one
	// hook file of the same configuration layer, and DuplicateHookSources the
	// files involved.
	//
	// Two files both carrying hooks is not itself a defect — splitting
	// disjoint events across them is a valid configuration, and only an event
	// declared twice actually runs twice. Keying the finding on the files
	// alone would tell such a user to delete working hooks.
	DuplicateHookEvents  []string
	DuplicateHookSources []string
	// ManagedHookFile is the file the installer rewrites. The durable fix for
	// a duplicate is to drop the event from the *other* file, because this one
	// is restored on the next install.
	ManagedHookFile string
}

// Diagnose turns the collected evidence into ordered findings, most
// actionable first. It takes plain data so the whole decision table is
// testable without a machine that has Codex, hooks, or a daemon on it.
func Diagnose(agent AgentHooks, summary hooks.EffectivenessSummary, adoption Adoption, now time.Time) []Finding {
	var findings []Finding
	// Scope the evidence to this harness. On a machine running more than one
	// agent the union would let a busy Claude Code session vouch for hooks
	// Codex is skipping — exactly the failure doctor exists to catch.
	activity := agentActivity{events: summary.ForAgent(agent.Agent), summary: summary}
	add := func(sev Severity, remedy, format string, args ...any) {
		findings = append(findings, Finding{Severity: sev, Summary: fmt.Sprintf(format, args...), Remedy: remedy})
	}

	var configured int
	events := make([]string, 0, len(agent.Configured))
	for event, count := range agent.Configured {
		configured += count
		if count > 0 {
			events = append(events, event)
		}
	}
	sort.Strings(events)

	var ran, ambiguous int
	for _, event := range events {
		ran += activity.events[event].Runs
		ambiguous += summary.AmbiguousRuns(event)
	}

	// Reported whether or not anything is configured. A harness told not to
	// run hooks will not run the ones it has, and will not run the ones
	// `gortex install` would add either — so this stands beside the
	// nothing-configured warning rather than replacing it.
	if agent.HooksDisabled {
		if configured > 0 {
			add(SeverityBlocker, agent.HooksDisabledRemedy,
				"%d %s hook(s) are configured but hook execution is turned off in its configuration.",
				configured, agent.Agent)
		} else {
			add(SeverityBlocker, agent.HooksDisabledRemedy,
				"%s has hook execution turned off in its configuration.", agent.Agent)
		}
	}

	switch {
	case configured == 0:
		add(SeverityWarn, "gortex install",
			"%s has no Gortex lifecycle hooks configured.", agent.Agent)
	case agent.HooksDisabled:
		// Every rule below asks why configured hooks did not run. The finding
		// above already answered, and these remedies — trust them, check PATH
		// — would send the user to fix something that is not broken.
	case ran == 0 && ambiguous > 0:
		// The window straddles the upgrade that introduced attribution: runs
		// exist, they just cannot be assigned. Say so rather than accuse.
		add(SeverityInfo, "re-run after the window clears the upgrade",
			"%d hook run(s) in this window predate per-agent attribution, so %s's hooks can be neither confirmed nor ruled out.",
			ambiguous, agent.Agent)
	case ran == 0 && agent.RequiresTrust:
		// The headline case. Every declared hook is on disk and none has
		// run, which for a trust-gating agent means the hooks were never
		// approved — or were re-approved away by an upgrade that changed
		// their definitions, since trust is recorded against the hook hash.
		add(SeverityBlocker, agent.TrustRemedy,
			"%d %s hook(s) are configured but none has run in this window — %s skips new or changed hooks until they are trusted.",
			configured, agent.Agent, agent.Agent)
	case ran == 0:
		add(SeverityBlocker, "check the agent's hook configuration and that `gortex` is on its PATH",
			"%d %s hook(s) are configured but none has run in this window.", configured, agent.Agent)
	}

	// A per-event zero while other events ran is a narrower version of the
	// same problem: one hook's definition changed, so only its trust lapsed.
	//
	// Skipped when execution is turned off, for the same reason the switch
	// above skips its silence rules. `ran > 0` survives the switch being
	// flipped — runs earlier in the window still count — so without this the
	// trust remedy reappears event by event on a machine that simply has
	// hooks disabled.
	if ran > 0 && !agent.HooksDisabled {
		for _, event := range events {
			stats := activity.events[event]
			if stats.Runs > 0 || activity.ambiguous(event) {
				continue
			}
			remedy := "re-run `gortex install`"
			if agent.RequiresTrust {
				remedy = agent.TrustRemedy
			}
			add(SeverityBlocker, remedy,
				"%s is configured but never ran, while other hooks did%s.", event, lastSeenSuffix(stats.LastSeen, now))
		}
	}

	for _, event := range events {
		stats := activity.events[event]
		if stats.Runs == 0 {
			continue
		}
		if stats.DaemonKnown > 0 && stats.DaemonUp == 0 {
			add(SeverityBlocker, "gortex daemon start --detach",
				"%s ran %d time(s) and never reached the daemon.", event, stats.Runs)
			continue
		}
		if stats.Emitted == 0 {
			// Under the deny posture the PostToolUse hook only watches
			// Gortex tool calls for localization receipts — its matcher
			// never names the native read-shaped tools the enrichment
			// path keys on, so zero injections is the designed steady
			// state, and warning on it would fire permanently on every
			// healthy deny-mode install (#630). Enrich-mode installs
			// keep the warning: there, silence genuinely means the
			// enrichment path broke.
			if event == "PostToolUse" && agent.PostToolUseDenyPosture {
				add(SeverityInfo, "",
					"%s ran %d time(s) and injected context 0 times — expected under the deny posture: PreToolUse already redirects native reads, and this hook only watches Gortex tool calls.",
					event, stats.Runs)
				continue
			}
			add(SeverityWarn, "",
				"%s ran %d time(s) and injected context 0 times — it fires but has nothing to say.", event, stats.Runs)
		}
	}

	// One event declared in two files of the same layer is not a cosmetic
	// duplicate: harnesses that merge their hook files run each declaration,
	// so that event's work happens twice and the user pays for it twice.
	//
	// Named per event, and the remedy points away from the managed file. The
	// events that live in only one file are working configuration, and a
	// remedy phrased as "clean up one of these two files" would take them out
	// along with the duplicate.
	if len(agent.DuplicateHookEvents) > 0 && len(agent.DuplicateHookSources) > 1 {
		add(SeverityWarn, duplicateHookRemedy(agent),
			"%s declares %s in both %s — it merges hook files in the same layer, so each of those runs once per declaration.",
			agent.Agent,
			strings.Join(agent.DuplicateHookEvents, " and "),
			strings.Join(agent.DuplicateHookSources, " and "))
	}

	if agent.InstructionsShadowed != "" {
		add(SeverityWarn, fmt.Sprintf("move the Gortex block into %s, or remove it", agent.InstructionsShadowed),
			"%s reads %s instead of %s, so any rule block in the latter is ignored.",
			agent.Agent, agent.InstructionsShadowed, agent.InstructionsPath)
	} else if agent.InstructionsPath != "" && !agent.InstructionsWired {
		add(SeverityWarn, "gortex install",
			"No Gortex rule block in %s — the agent loads that file into every session, so without it a skipped or silent hook leaves the session with no standing rule.",
			agent.InstructionsPath)
	}

	switch share := adoption.GortexShare(); {
	case adoption.FilesFound == 0:
		add(SeverityInfo, "", "No %s session transcripts found — adoption could not be measured.", agent.Agent)
	case share < 0:
		add(SeverityInfo, "", "No tool calls in the window — adoption could not be measured.")
	case adoption.GortexCalls == 0:
		add(SeverityBlocker, "gortex install",
			"Across %d session(s) the model made %d shell call(s) and zero Gortex tool calls.",
			adoption.InWindow, adoption.ShellCalls)
	case share < 50:
		add(SeverityWarn, "",
			"Gortex tools are %.0f%% of tool calls (%d Gortex vs %d shell, %d read/search shaped).",
			share, adoption.GortexCalls, adoption.ShellCalls, adoption.ShellReads)
	default:
		add(SeverityOK, "",
			"Gortex tools are %.0f%% of tool calls (%d Gortex vs %d shell).",
			share, adoption.GortexCalls, adoption.ShellCalls)
	}

	sort.SliceStable(findings, func(i, j int) bool {
		return severityRank(findings[i].Severity) < severityRank(findings[j].Severity)
	})
	return findings
}

// duplicateHookRemedy names the file to take the duplicated events out of, and
// only those events.
//
// It points at the file the installer does NOT rewrite. Cleaning the managed
// one looks like it worked and is undone by the next `gortex install`, which
// is worse than not advising at all — the user pays for the edit twice and
// still has the duplicate.
func duplicateHookRemedy(agent AgentHooks) string {
	events := strings.Join(agent.DuplicateHookEvents, " and ")
	for _, path := range agent.DuplicateHookSources {
		if path != agent.ManagedHookFile {
			return fmt.Sprintf("remove %s from %s — %s is rewritten by `gortex install`",
				events, path, agent.ManagedHookFile)
		}
	}
	return fmt.Sprintf("keep one declaration of %s", events)
}

// HasBlocker reports whether any finding is a blocker — the command's exit
// status.
func HasBlocker(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityBlocker {
			return true
		}
	}
	return false
}

func severityRank(s Severity) int {
	switch s {
	case SeverityBlocker:
		return 0
	case SeverityWarn:
		return 1
	case SeverityInfo:
		return 2
	default:
		return 3
	}
}

// lastSeenSuffix turns a never/stale timestamp into a clause, so "never ran"
// can distinguish "not in this window" from "not once, ever".
func lastSeenSuffix(last, now time.Time) string {
	if last.IsZero() {
		return " (not once, ever)"
	}
	days := int(now.Sub(last).Hours() / 24)
	if days <= 0 {
		return ""
	}
	return fmt.Sprintf(" (last ran %dd ago)", days)
}
