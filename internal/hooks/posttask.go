package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// postTaskDiffScope is the detect_changes scope both Stop-hook briefings
// measure. "all" == `git diff HEAD`: staged and unstaged changes to tracked
// files. The previous "unstaged" hid staged work entirely, so a session that
// staged its edits got a briefing about only what it had *not* staged.
// Untracked files are invisible to every scope — MapGitDiff parses git diff
// output, which never lists them.
const postTaskDiffScope = "all"

// postTaskScopePhrase describes postTaskDiffScope in prose. Kept beside the
// constant so the scope we request and the scope we claim cannot drift.
const postTaskScopePhrase = "uncommitted (staged + unstaged) changes to tracked files"

// PostTaskInput is the JSON structure Claude Code sends to Stop hooks.
// stop_hook_active is true when the hook is already being rerun (another
// Stop hook asked the agent to continue) — we must skip in that case to
// avoid recursion.
type PostTaskInput struct {
	HookEventName        string `json:"hook_event_name"`
	SessionID            string `json:"session_id"`
	PromptID             string `json:"prompt_id"`
	AgentID              string `json:"agent_id"`
	TranscriptPath       string `json:"transcript_path"`
	CWD                  string `json:"cwd"`
	LastAssistantMessage string `json:"last_assistant_message"`
	StopHookActive       bool   `json:"stop_hook_active"`
}

// runPostTask handles a Stop hook invocation with the raw stdin bytes.
// It runs diagnostics on any changed symbols (dead-code, test targets,
// guards, contract violations) and injects the findings as
// additionalContext so the agent self-corrects before handing off.
//
// Degrades silently when: the bridge is unreachable, there are no
// changes, or stop_hook_active is true.
func runPostTask(data []byte, port int) {
	runPostTaskWith(data, port, claudeStopBriefing)
}

// claudeStopBriefing wraps the briefing in Claude Code's
// hookSpecificOutput.additionalContext envelope.
func claudeStopBriefing(briefing string) HookOutput {
	return HookOutput{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     "Stop",
			AdditionalContext: briefing,
		},
	}
}

// codexStopBriefing carries the briefing as a top-level systemMessage.
// Codex's Stop output schema is closed (additionalProperties: false) and
// has no hookSpecificOutput, so the Claude envelope fails validation with
// "invalid stop hook JSON output" (#779). decision:block would validate
// too, but it forces another turn on every dirty working tree.
func codexStopBriefing(briefing string) HookOutput {
	return HookOutput{SystemMessage: briefing}
}

// runPostTaskWith is runPostTask with a host-specific envelope for the
// diagnostics briefing. The claim-check block (decision/reason) is valid
// on every host, so it does not go through wrap.
func runPostTaskWith(data []byte, port int, wrap func(briefing string) HookOutput) {
	var input PostTaskInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "Stop" {
		return
	}
	claimCheck, briefing := postTaskFindings(input, port)
	var output HookOutput
	switch {
	case claimCheck != "":
		output = HookOutput{Decision: "block", Reason: claimCheck}
	case briefing != "":
		output = wrap(briefing)
	default:
		return
	}
	out, err := json.Marshal(output)
	if err != nil {
		return
	}
	fmt.Print(string(out))
}

// postTaskFindings runs the Stop checks every host shares: the claim check,
// which asks the agent to keep working, and otherwise the diagnostics
// briefing. At most one is non-empty, and both are empty on a rerun Stop.
func postTaskFindings(input PostTaskInput, port int) (claimCheck, briefing string) {
	started := time.Now()
	_ = clearLocalizationProblemStatementForTurn(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	defer func() {
		logHookEffectiveness("Stop", claimCheck != "" || briefing != "", daemonReachableFn(), 0, time.Since(started))
	}()
	// Prevent recursion — if we're already rerunning a Stop hook, don't fire again.
	if input.StopHookActive {
		return "", ""
	}
	if claimCheck = localizationClaimCheck(input); claimCheck != "" {
		return claimCheck, ""
	}
	return "", buildPostTaskBriefing(port, sessionScope{
		SessionID: input.SessionID,
		CWD:       firstNonEmpty(input.CWD, loadHookCWD()),
	})
}

// changedSymbol is one entry of detect_changes' changed_symbols list.
// FilePath carries the graph node's file, which is repo-prefixed in
// multi-repo mode while changed_files is not — see attributeChanges.
type changedSymbol struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	FilePath string `json:"file_path"`
}

// changeSet is the slice of a detect_changes response both Stop-hook
// briefings read. Scope/Repo/RepoRoot are echoed by the handler so the
// briefing can name the tree it actually diffed instead of guessing.
type changeSet struct {
	ChangedFiles   []string        `json:"changed_files"`
	ChangedSymbols []changedSymbol `json:"changed_symbols"`
	Risk           string          `json:"risk"`
	Summary        string          `json:"summary"`
	Scope          string          `json:"scope"`
	Repo           string          `json:"repo"`
	RepoRoot       string          `json:"repo_root"`
}

// detectChangeSet asks the bridge for the current diff and decodes it.
// Returns nil when the bridge is unreachable or the payload is unusable.
func detectChangeSet(port int) *changeSet {
	raw := callServerTool(port, "detect_changes", map[string]any{
		"scope": postTaskDiffScope,
		// The briefings never read by_depth; summary_only drops the
		// largest part of the response.
		"summary_only": true,
	})
	if raw == "" {
		return nil
	}
	var changes changeSet
	if err := json.Unmarshal([]byte(raw), &changes); err != nil {
		return nil
	}
	return &changes
}

// briefingScope names the working tree detect_changes actually diffed.
// The daemon echoes repo / repo_root; when an older daemon omits both we
// say so rather than guessing from the hook cwd — diffRepoScope resolves
// the tree as explicit-selector → lone-tracked-repo → session cwd, so with
// several repos tracked a cwd-derived name can label a repo the diff never
// touched, replacing one wrong claim with a subtler one.
func briefingScope(repo, repoRoot string) string {
	switch {
	case strings.TrimSpace(repoRoot) != "":
		return "`" + strings.TrimSpace(repoRoot) + "`"
	case strings.TrimSpace(repo) != "":
		return "repo `" + strings.TrimSpace(repo) + "`"
	default:
		return "the working tree Gortex resolved for this session"
	}
}

// changedSymbolIDs projects a changed-symbol list to its IDs, dropping blanks.
func changedSymbolIDs(symbols []changedSymbol) []string {
	ids := make([]string, 0, len(symbols))
	for _, cs := range symbols {
		if id := strings.TrimSpace(cs.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// ownedRisk returns the blast-radius tier for just the session's own symbols.
// When the session owns the entire change set there is nothing to recompute,
// so detect_changes' own risk is reused and the round-trip skipped — the
// common single-session case. Never falls back to the tree-wide tier on a
// partition: that number belongs to another session's work.
func ownedRisk(port int, ownedIDs []string, treeRisk string, ownsAll bool) string {
	if ownsAll {
		return treeRisk
	}
	if len(ownedIDs) == 0 {
		return "unknown"
	}
	raw := callServerTool(port, "explain_change_impact", map[string]any{
		"ids":          strings.Join(ownedIDs, ","),
		"summary_only": true,
	})
	if raw == "" {
		return "unknown"
	}
	var impact struct {
		Risk string `json:"risk"`
	}
	if json.Unmarshal([]byte(raw), &impact) != nil || strings.TrimSpace(impact.Risk) == "" {
		return "unknown"
	}
	return impact.Risk
}

// renderUnattributedFiles names the dirty files this session did not write, so
// an attribution miss degrades to a mention rather than a disappearance.
func renderUnattributedFiles(files []string, capped bool) string {
	if len(files) == 0 {
		return ""
	}
	const show = 5
	shown := files
	suffix := ""
	if len(shown) > show {
		shown = shown[:show]
		suffix = fmt.Sprintf(" (+%d more)", len(files)-show)
	}
	quoted := make([]string, len(shown))
	for i, f := range shown {
		quoted[i] = "`" + f + "`"
	}
	out := "_Also dirty, not attributed to this session:_ " + strings.Join(quoted, ", ") + suffix + "\n"
	if capped {
		out += "_This session's tracked-edit set hit its cap, so some of your own edits may be listed above._\n"
	}
	return out + "\n"
}

// buildPostTaskBriefing runs diagnostics on the changes this session made and
// returns a compact markdown summary. Returns empty string when there's
// nothing to report or the bridge is unreachable.
//
// detect_changes answers about the whole working tree, so on a shared checkout
// it also carries a sibling session's in-flight edits. attributeChanges splits
// the result using the write targets PreToolUse recorded for this session; the
// diagnostics below run over this session's symbols only, and the remainder is
// named rather than dropped. When the session cannot be identified at all the
// briefing falls back to the whole tree and labels itself as such.
func buildPostTaskBriefing(port int, scope sessionScope) string {
	changes := detectChangeSet(port)
	if changes == nil {
		return ""
	}

	if len(changes.ChangedSymbols) == 0 {
		// No indexed symbols touched — skip silently. The agent doesn't need
		// a post-task briefing when nothing meaningful changed in the graph.
		return ""
	}

	attr := attributeChanges(scope, changes)
	symbols := changes.ChangedSymbols
	risk := changes.Risk
	if attr.Attributed {
		if len(attr.OwnedSymbols) == 0 {
			// Every dirty file belongs to somebody else. Say nothing: Stop
			// fires at the end of every turn, so a session reading code beside
			// an editing sibling would otherwise get the same notice each time.
			return ""
		}
		symbols = attr.OwnedSymbols
		risk = ownedRisk(port, changedSymbolIDs(symbols), changes.Risk,
			len(attr.OwnedSymbols) == len(changes.ChangedSymbols))
	}

	ids := changedSymbolIDs(symbols)
	idsCSV := strings.Join(ids, ",")

	var sb strings.Builder
	if attr.Attributed {
		// Only now is "post-task" a true description of the content.
		sb.WriteString("## Gortex Post-Task Diagnostics\n\n")
		fmt.Fprintf(&sb, "**Your edits this session:** %d symbol(s) across %d file(s) in %s — risk `%s`.\n\n",
			len(symbols), len(attr.OwnedFiles), briefingScope(changes.Repo, changes.RepoRoot), risk)
		sb.WriteString(renderUnattributedFiles(attr.Unattributed, attr.Capped))
	} else {
		sb.WriteString("## Gortex Working-Tree Diagnostics\n\n")
		fmt.Fprintf(&sb, "**Measured:** %d symbol(s) across %d file(s) with %s in %s — aggregate risk `%s`.\n\n",
			len(symbols), len(changes.ChangedFiles), postTaskScopePhrase,
			briefingScope(changes.Repo, changes.RepoRoot), risk)
		sb.WriteString("**Scope:** whole working tree. Gortex does not attribute these changes to a session — " +
			"this is a diff of everything uncommitted, not a record of your work. Anything left by another agent, " +
			"a parallel session on this same checkout, or an earlier turn is included below. " +
			"Untracked (never `git add`ed) files are invisible to this check entirely.\n\n")
	}

	// Test targets — what to run.
	if tests := renderTestTargets(port, idsCSV); tests != "" {
		sb.WriteString("### Tests Covering These Symbols\n\n")
		sb.WriteString(tests)
		sb.WriteString("\n")
	}

	// Guard rule violations.
	if guards := renderGuardViolations(port, idsCSV); guards != "" {
		sb.WriteString("### Guard Violations\n\n")
		sb.WriteString(guards)
		sb.WriteString("\n")
	}

	// Dead code — specifically whether any of the changed symbols are now orphaned.
	if dead := renderDeadCodeHits(port, ids); dead != "" {
		sb.WriteString("### Potential Dead Code (among uncommitted symbols)\n\n")
		sb.WriteString(dead)
		sb.WriteString("\n")
	}

	// Coverage gaps among changed symbols. Silent when the graph carries
	// no coverage_pct meta (the analyzer returns "no coverage gaps
	// matched" or just an empty body) — we don't want to nag when the
	// repo simply doesn't run `gortex enrich coverage`.
	if gaps := renderCoverageGapsHits(port, ids); gaps != "" {
		sb.WriteString("### Coverage Gaps (among uncommitted symbols)\n\n")
		sb.WriteString(gaps)
		sb.WriteString("\n")
	}

	// Stale-flag regression check. Silent unless one of the changed
	// symbols is itself a flag site or toggles one — pointing the agent
	// at flag rollouts they may have just touched.
	if flags := renderStaleFlagHits(port, ids); flags != "" {
		sb.WriteString("### Stale Flag Sites Among Uncommitted Symbols\n\n")
		sb.WriteString(flags)
		sb.WriteString("\n")
	}

	// Contract mismatches. `contracts check` has no id filter, so this
	// section stays repo-wide even when everything above is session-scoped —
	// label it rather than drop a real signal or imply it is yours.
	if contracts := renderContractMismatches(port); contracts != "" {
		if attr.Attributed {
			sb.WriteString("### API Contract Issues (repo-wide, not attributed to this session)\n\n")
		} else {
			sb.WriteString("### API Contract Issues\n\n")
		}
		sb.WriteString(contracts)
		sb.WriteString("\n")
	}

	if attr.Attributed {
		sb.WriteString("_Run the tests above and resolve any flagged item in your own changes before handoff._\n")
	} else {
		sb.WriteString("_Cross-check each finding against the edits you actually made this turn: act on the ones " +
			"that match, ignore the rest. Do not investigate or \"fix\" changes you did not make._\n")
	}
	return sb.String()
}

// buildMutationBriefing is the focused PostToolUse pipeline for mutations that
// happen outside Gortex, notably Codex apply_patch. It detects affected graph
// symbols first, then asks for tests, guards, and contracts using that observed
// change set. The hook is advisory: an unavailable daemon or an edit touching
// no indexed symbol yields no output and never changes the completed mutation.
//
// Threaded with the same sessionScope as the Stop briefing so the two share one
// attribution path. Note that Codex's PreToolUse payload carries no session_id,
// so nothing is ever recorded for it and it always resolves to the labeled
// whole-tree fallback — this does not give Codex attribution coverage.
func buildMutationBriefing(port int, scope sessionScope) string {
	changes := detectChangeSet(port)
	if changes == nil || len(changes.ChangedSymbols) == 0 {
		return ""
	}

	attr := attributeChanges(scope, changes)
	symbols := changes.ChangedSymbols
	risk := changes.Risk
	if attr.Attributed {
		if len(attr.OwnedSymbols) == 0 {
			return ""
		}
		symbols = attr.OwnedSymbols
		risk = ownedRisk(port, changedSymbolIDs(symbols), changes.Risk,
			len(attr.OwnedSymbols) == len(changes.ChangedSymbols))
	}

	ids := changedSymbolIDs(symbols)
	if len(ids) == 0 {
		return ""
	}
	idsCSV := strings.Join(ids, ",")

	var out strings.Builder
	out.WriteString("## Gortex mutation follow-up\n\n")
	if attr.Attributed {
		fmt.Fprintf(&out, "After this mutation, your session's uncommitted work is %d symbol(s) across %d file(s) "+
			"in %s; risk `%s`. The set spans everything you have edited this session, not only the patch just "+
			"applied.\n\n",
			len(ids), len(attr.OwnedFiles), briefingScope(changes.Repo, changes.RepoRoot), risk)
		out.WriteString(renderUnattributedFiles(attr.Unattributed, attr.Capped))
	} else {
		fmt.Fprintf(&out, "Working-tree scan after this mutation: %d symbol(s) across %d file(s) with %s in %s; "+
			"aggregate risk `%s`. The scan covers every uncommitted change in the tree, not only the patch just "+
			"applied — entries that predate this mutation, or that belong to another agent on this checkout, are "+
			"included.\n\n",
			len(ids), len(changes.ChangedFiles), postTaskScopePhrase,
			briefingScope(changes.Repo, changes.RepoRoot), risk)
	}
	out.WriteString("Uncommitted symbols in scope:\n")
	for _, id := range ids {
		fmt.Fprintf(&out, "- `%s`\n", id)
	}
	out.WriteString("\n")
	if tests := renderTestTargets(port, idsCSV); tests != "" {
		out.WriteString("### Tests\n\n")
		out.WriteString(tests)
		out.WriteString("\n")
	}
	if guards := renderGuardViolations(port, idsCSV); guards != "" {
		out.WriteString("### Guards\n\n")
		out.WriteString(guards)
		out.WriteString("\n")
	}
	if contracts := renderContractMismatches(port); contracts != "" {
		out.WriteString("### Contracts\n\n")
		out.WriteString(contracts)
		out.WriteString("\n")
	}
	out.WriteString("Run the tests covering the symbols this patch actually changed, and resolve any guard or " +
		"contract finding the patch introduced. Ignore entries you did not touch.\n")
	return out.String()
}

// renderTestTargets asks the bridge for test files that exercise the changed symbols.
func renderTestTargets(port int, idsCSV string) string {
	raw := callServerTool(port, "get_test_targets", map[string]any{
		"ids":     idsCSV,
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "no test targets found" {
		return ""
	}
	return cappedText(raw, 15, 2000)
}

// renderGuardViolations asks the bridge for .gortex.yaml guard rule violations.
func renderGuardViolations(port int, idsCSV string) string {
	raw := callServerTool(port, "check_guards", map[string]any{
		"ids":     idsCSV,
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	// Two distinct clean sentinels: no violations found, and no rules to
	// evaluate in the first place.
	if raw == "" ||
		strings.HasPrefix(raw, "no guard rule violations") ||
		strings.HasPrefix(raw, "no guard rules configured") {
		return ""
	}
	return cappedText(raw, 10, 1500)
}

// renderDeadCodeHits filters analyze:dead_code results to the intersection
// with the currently-changed symbols (i.e. "did this task leave anything
// orphaned"). Emits nothing when the intersection is empty.
func renderDeadCodeHits(port int, ids []string) string {
	raw := callServerTool(port, "analyze", map[string]any{
		"kind":    "dead_code",
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}

	var hits []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		// Each compact line starts with the symbol descriptor including the
		// file path — we substring-match any of the changed IDs.
		for id := range idSet {
			if strings.Contains(line, id) {
				hits = append(hits, line)
				break
			}
		}
	}
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > 8 {
		hits = hits[:8]
	}
	return strings.Join(hits, "\n") + "\n"
}

// renderCoverageGapsHits filters analyze:coverage_gaps results to
// the intersection with currently-changed symbols. Silent when no
// changed symbol carries coverage_pct (i.e. coverage hasn't been
// hydrated for this graph) or none of the changed symbols are
// undertested. The agent gets a nudge precisely when a coverage
// gap was just touched — not a noisy package-wide rollup.
func renderCoverageGapsHits(port int, ids []string) string {
	raw := callServerTool(port, "analyze", map[string]any{
		"kind":    "coverage_gaps",
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "no coverage gaps") {
		return ""
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	var hits []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		for id := range idSet {
			if strings.Contains(line, id) {
				hits = append(hits, line)
				break
			}
		}
	}
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > 8 {
		hits = hits[:8]
	}
	return strings.Join(hits, "\n") + "\n"
}

// renderStaleFlagHits filters analyze:stale_flags results to the
// intersection with currently-changed symbols. Silent unless one
// of the changed symbols is a flag site (its id appears in the
// stale_flags output). Useful for catching a refactor that
// inadvertently touched a flag rollout that's been quiet for ages.
func renderStaleFlagHits(port int, ids []string) string {
	raw := callServerTool(port, "analyze", map[string]any{
		"kind":    "stale_flags",
		"compact": true,
	})
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "no stale flags") {
		return ""
	}

	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	var hits []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		for id := range idSet {
			if strings.Contains(line, id) {
				hits = append(hits, line)
				break
			}
		}
	}
	if len(hits) == 0 {
		return ""
	}
	if len(hits) > 8 {
		hits = hits[:8]
	}
	return strings.Join(hits, "\n") + "\n"
}

// renderContractMismatches runs the contracts check and returns only the
// orphan providers/consumers. Empty when all contracts are matched.
//
// The compact summary always opens with `matched: N pairs` followed by one
// row per matched pair, and puts the orphan counts and rows at the END. So a
// leading "is it clean?" prefix test can never fire, and a leading line cap
// truncates the orphan lists — the only actionable part — off the bottom in
// any repo with enough matched pairs. Locate the orphan block and render from
// there instead.
func renderContractMismatches(port int) string {
	raw := strings.TrimSpace(callServerTool(port, "contracts", map[string]any{
		"action":  "check",
		"compact": true,
	}))
	if raw == "" {
		return ""
	}

	lines := strings.Split(raw, "\n")
	orphanStart := -1
	providers, consumers := 0, 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if n, _ := fmt.Sscanf(trimmed, "orphan providers: %d", &providers); n == 1 {
			if orphanStart < 0 {
				orphanStart = i
			}
			continue
		}
		if n, _ := fmt.Sscanf(trimmed, "orphan consumers: %d", &consumers); n == 1 && orphanStart < 0 {
			orphanStart = i
		}
	}
	// No recognizable orphan block (older or changed compact shape) — suppress
	// rather than paste an unparsed blob under an "issues" heading.
	if orphanStart < 0 || (providers == 0 && consumers == 0) {
		return ""
	}
	return cappedText(strings.TrimSpace(strings.Join(lines[orphanStart:], "\n")), 10, 1500)
}
