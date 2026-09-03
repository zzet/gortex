package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/modelhint"
)

// CodexMode is deliberately separate from the cross-agent hook Mode: Codex's
// compatibility default must remain advisory even though the generic hook
// command defaults to deny for Claude Code.
type CodexMode int

const (
	CodexModeEnrich CodexMode = iota
	CodexModeDeny
	CodexModeRewrite
	CodexModeSuppress
)

func ParseCodexMode(value string) CodexMode {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "deny", "hard-deny":
		return CodexModeDeny
	case "rewrite", "input-rewrite":
		return CodexModeRewrite
	case "suppress", "replace-output", "output-suppression":
		return CodexModeSuppress
	default:
		return CodexModeEnrich
	}
}

func (m CodexMode) String() string {
	switch m {
	case CodexModeDeny:
		return "deny"
	case CodexModeRewrite:
		return "rewrite"
	case CodexModeSuppress:
		return "suppress"
	default:
		return "enrich"
	}
}

// RunCodex handles the Codex hook wire shape. Advisory enrich remains the
// default. Operators may opt into hard deny, conservative input rewrite, or
// supported PostToolUse result replacement (the current Codex release rejects
// the nominal suppressOutput field, so suppress mode never emits it).
func RunCodex(port int, selected ...CodexMode) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	runCodex(data, port, selected...)
}

func runCodex(data []byte, port int, selected ...CodexMode) {
	var peek struct {
		HookEventName string `json:"hook_event_name"`
		ToolName      string `json:"tool_name"`
		SessionID     string `json:"session_id"`
		PromptID      string `json:"prompt_id"`
		AgentID       string `json:"agent_id"`
		CWD           string `json:"cwd"`
		// Codex sends the active model slug on every hook event. Claude Code
		// does not, which is why its hint has to be recovered from the
		// transcript; here it is simply handed to us.
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return
	}
	captureCodexModelHint(peek.HookEventName, peek.CWD, peek.Model)
	mode := CodexModeEnrich
	if len(selected) > 0 {
		mode = selected[0]
	}
	setHookCWD(peek.CWD)
	defer setHookCWD("")

	// Codex has specialized Bash and Gortex-read handlers, so enforce the
	// shared terminal contract before dispatch. Decode only the identity and
	// tool name here: Codex permits tool_input to be any JSON value, and a
	// scalar or array must not bypass an enforceable terminal marker merely
	// because later tool-specific handlers expect an arguments object.
	if peek.HookEventName == "PreToolUse" && enforceLocalizationTerminalPreToolUse(HookInput{
		HookEventName: peek.HookEventName,
		ToolName:      peek.ToolName,
		SessionID:     peek.SessionID,
		PromptID:      peek.PromptID,
		AgentID:       peek.AgentID,
		CWD:           peek.CWD,
	}, time.Now()) {
		return
	}

	switch {
	case peek.HookEventName == "Stop":
		// Codex uses the same stop_hook_active and last_assistant_message
		// fields as the shared Stop handler. Unsupported/older hosts omit the
		// message and therefore fail open in runPostTask.
		runPostTask(data, port)
	case peek.HookEventName == "SessionStart":
		runSessionStart(data, port)
	case peek.HookEventName == "PreToolUse" && peek.ToolName == "Bash":
		switch mode {
		case CodexModeDeny:
			runCodexBashHardDeny(data, port)
		case CodexModeRewrite:
			runCodexBashRewrite(data, port)
		default:
			runPreToolUseForHost(data, port, ModeEnrich, preToolUseCodex)
		}
	case peek.HookEventName == "PreToolUse" && codexLocalizationPreToolUseTool(peek.ToolName):
		runCodexLocalizationPreToolUse(data, mode)
	case peek.HookEventName == "PostToolUse" && (peek.ToolName == "Bash" || peek.ToolName == "apply_patch" || localizationNavigationTool(peek.ToolName)):
		runCodexPostToolUse(data, port, mode)
	case peek.HookEventName == "UserPromptSubmit":
		// Re-surface graph symbols relevant to the prompt on every turn.
		// Codex forgets MCP tools as context grows, so a SessionStart
		// orientation alone fades; this lands a fresh, prompt-specific
		// nudge at the top of each turn (the wire shape is shared with
		// Claude Code — hookSpecificOutput.additionalContext).
		runUserPromptSubmit(data)
	}
}

// captureCodexModelHint records which model is driving this Codex session so
// the daemon can attribute savings to it. Without a writer of its own, a Codex
// session either recorded no model or — worse, before the reader learned to
// check the hint's agent — inherited whichever model Claude Code last
// announced for the same working directory.
//
// Only the once-per-session and once-per-turn events write. PreToolUse fires
// hundreds of times per session and would turn a 12-hour hint into a
// per-tool-call disk write for no extra freshness.
func captureCodexModelHint(event, cwd, model string) {
	switch event {
	case "SessionStart", "UserPromptSubmit":
	default:
		return
	}
	if strings.TrimSpace(model) == "" {
		return
	}
	// Tagged with the agent name; modelhint.SameClient bridges it to the
	// "codex-mcp-client" the MCP session reports.
	modelhint.Write(cwd, model, "codex")
}

func runCodexBashHardDeny(data []byte, port int) {
	started := time.Now()
	var input HookInput
	if err := json.Unmarshal(data, &input); err != nil || input.HookEventName != "PreToolUse" || input.ToolName != "Bash" {
		return
	}
	daemonUp := daemonReachableFn()
	emitted := false
	defer func() {
		logHookEffectiveness("PreToolUse", emitted, daemonUp, hookAlternationSegmentCount(input), time.Since(started))
	}()

	// Daemon outage: the deny posture stands down entirely (#486) — both the
	// deny escalation and its advisory fallback mandate MCP operations the
	// daemon cannot currently serve.
	if !daemonUp {
		return
	}

	result := enrich(input, port)
	classification := classifyBashCommand(fmt.Sprint(input.ToolInput["command"]))
	searchShape := classification.Action == BashActionGrepLike || classification.Action == BashActionFindName
	workspaceScoped := !searchShape || bashSearchTargetsWorkspace(fmt.Sprint(input.ToolInput["command"]), input.CWD, classification.Action)
	if result.deny && searchShape && !workspaceScoped {
		// A graph hit does not prove an explicitly external grep/find target is
		// indexed. Keep the reminder but never block that command.
		result = enrichResult{context: defaultGrepGuidance()}
	}
	if !result.deny && workspaceScoped &&
		classification.Action == BashActionGrepLike && result.context != "" &&
		codexTextSearchHitFn(port, classification.Pattern) {
		result.deny = true
		result.reason = "[Gortex] BLOCKED by opt-in Codex deny posture. Use the public MCP search/relations operations instead of a raw source search.\n" + result.context
		result.context = ""
	}
	if result.context == "" && !result.deny {
		return
	}
	hso := &HookSpecificOutput{HookEventName: "PreToolUse"}
	if result.deny {
		hso.PermissionDecision = "deny"
		hso.PermissionDecisionReason = result.reason
	} else {
		hso.AdditionalContext = result.context
	}
	emitted = true
	emitPreToolUse(HookOutput{HookSpecificOutput: hso})
}

// codexTextSearchHitFn confirms that a raw regex/literal search actually has
// an indexed-code match before the opt-in deny posture blocks it. Keeping the
// check behind a seam makes the hard-deny boundary testable without a daemon.
var codexTextSearchHitFn = codexTextSearchHasHit

func codexTextSearchHasHit(port int, pattern string) bool {
	if strings.TrimSpace(pattern) == "" {
		return false
	}
	raw := callServerTool(port, "search_text", map[string]any{
		"query": pattern, "regexp": true, "limit": 1,
	})
	var result struct {
		Count int `json:"count"`
	}
	return json.Unmarshal([]byte(raw), &result) == nil && result.Count > 0
}

// bashSearchTargetsWorkspace accepts only a single grep/find command whose
// explicit search roots stay under cwd. Compound commands remain advisory;
// an absolute or ../ scope outside the workspace can never be hard-denied just
// because the same pattern happens to exist in the graph.
func bashSearchTargetsWorkspace(command, cwd string, action BashAction) bool {
	if !simpleBashCommand(command) {
		return false
	}
	tokens := tokenize(command)
	for len(tokens) > 0 && (tokens[0] == "sudo" || tokens[0] == "time" ||
		(strings.Contains(tokens[0], "=") && !strings.HasPrefix(tokens[0], "-"))) {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return false
	}
	var scopes []string
	switch action {
	case BashActionGrepLike:
		_, patternAt, ok := extractGrepPatternAt(tokens)
		if !ok {
			return false
		}
		for i := patternAt + 1; i < len(tokens); i++ {
			token := tokens[i]
			if token == "--" {
				scopes = append(scopes, tokens[i+1:]...)
				break
			}
			if strings.HasPrefix(token, "--") && strings.Contains(token, "=") {
				continue
			}
			if grepFlagsTakingArg[token] {
				i++
				continue
			}
			if strings.HasPrefix(token, "-") {
				continue
			}
			scopes = append(scopes, token)
		}
	case BashActionFindName:
		for _, token := range tokens[1:] {
			if strings.HasPrefix(token, "-") || token == "!" || token == "(" {
				break
			}
			scopes = append(scopes, token)
		}
	default:
		return false
	}
	if len(scopes) == 0 {
		scopes = []string{"."}
	}
	for _, scope := range scopes {
		if !pathWithinWorkspace(cwd, scope) {
			return false
		}
	}
	return true
}

func pathWithinWorkspace(cwd, scope string) bool {
	if strings.TrimSpace(scope) == "" || scope == "-" {
		return false
	}
	root, err := filepath.Abs(cwd)
	if err != nil || strings.TrimSpace(cwd) == "" {
		root = ""
	}
	candidate := filepath.Clean(scope)
	if root == "" {
		return !filepath.IsAbs(candidate) && candidate != ".." && !strings.HasPrefix(candidate, ".."+string(filepath.Separator))
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func codexMCPReadPreToolUseTool(toolName string) bool {
	switch toolName {
	case gortexCompactReadTool, gortexReadFileTool, gortexEditingContextTool:
		return true
	default:
		return false
	}
}

func codexLocalizationPreToolUseTool(toolName string) bool {
	return codexMCPReadPreToolUseTool(toolName) || localizationNavigationTool(toolName)
}

func runCodexLocalizationPreToolUse(data []byte, mode CodexMode) {
	started := time.Now()
	var input HookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "PreToolUse" || !codexLocalizationPreToolUseTool(input.ToolName) {
		return
	}

	updatedInput := map[string]any(nil)
	if turn, ok := currentLocalizationTurnState(input.SessionID, input.PromptID, input.AgentID, input.CWD); ok {
		if authToken, ready := snapshotLocalizationToolUseWithAuth(input, turn.Identity); ready {
			updatedInput = localizationPreToolUpdatedInput(input, authToken, turn.ProblemStatement)
		}
	}

	daemonUp := daemonReachableFn()
	emitted := false
	defer func() {
		logHookEffectiveness("PreToolUse", emitted, daemonUp, 0, time.Since(started))
	}()

	ctx := ""
	if daemonUp && codexMCPReadPreToolUseTool(input.ToolName) {
		ctx = gortexReadNudge(input.ToolName, input.ToolInput)
	}
	if ctx == "" && updatedInput == nil {
		return
	}
	hso := &HookSpecificOutput{
		HookEventName:     "PreToolUse",
		AdditionalContext: ctx,
		UpdatedInput:      updatedInput,
	}
	if updatedInput != nil {
		// Codex requires every input rewrite to carry an explicit allow.
		// Unlike Claude Code, it does not accept ask with updatedInput.
		hso.PermissionDecision = "allow"
	}
	if ctx != "" {
		switch mode {
		case CodexModeDeny:
			hso.AdditionalContext = ""
			hso.UpdatedInput = nil
			hso.PermissionDecision = "deny"
			hso.PermissionDecisionReason = ctx
		case CodexModeRewrite:
			hso.PermissionDecision = "allow"
			rewritten := rewrittenGortexReadInput(input.ToolName, input.ToolInput)
			for key, value := range updatedInput {
				rewritten[key] = value
			}
			hso.UpdatedInput = rewritten
		}
	}
	emitted = true
	emitPreToolUse(HookOutput{HookSpecificOutput: hso})
}

// Kept for existing callers that exercise the read-only Codex path directly.
func runCodexMCPReadPreToolUse(data []byte, mode CodexMode) {
	runCodexLocalizationPreToolUse(data, mode)
}

func rewrittenGortexReadInput(toolName string, input map[string]any) map[string]any {
	out := cloneStringAnyMap(input)
	switch strings.TrimSpace(toolName) {
	case gortexCompactReadTool, "read":
		options, _ := input["options"].(map[string]any)
		options = cloneStringAnyMap(options)
		options["compress_bodies"] = true
		out["options"] = options
	default:
		out["compress_bodies"] = true
	}
	return out
}

func cloneStringAnyMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input)+1)
	for key, value := range input {
		out[key] = value
	}
	return out
}

func runCodexBashRewrite(data []byte, port int) {
	started := time.Now()
	var input HookInput
	if err := json.Unmarshal(data, &input); err != nil || input.HookEventName != "PreToolUse" || input.ToolName != "Bash" {
		return
	}
	daemonUp := daemonReachableFn()
	emitted := false
	defer func() {
		logHookEffectiveness("PreToolUse", emitted, daemonUp, hookAlternationSegmentCount(input), time.Since(started))
	}()

	// Daemon outage: stand down (#486). Rewriting a working `cat` into a
	// `gortex call read` that cannot be served would break the command
	// outright, and the advisory fallback mandates the same unusable tools.
	if !daemonUp {
		return
	}

	if updated, message, ok := rewrittenCodexBashInput(input); ok {
		emitted = true
		emitPreToolUse(HookOutput{HookSpecificOutput: &HookSpecificOutput{
			HookEventName:      "PreToolUse",
			AdditionalContext:  message,
			PermissionDecision: "allow",
			UpdatedInput:       updated,
		}})
		return
	}

	result := applyMode(input, false, ModeEnrich, enrich(input, port))
	if result.context == "" {
		return
	}
	emitted = true
	emitPreToolUse(HookOutput{HookSpecificOutput: &HookSpecificOutput{
		HookEventName:     "PreToolUse",
		AdditionalContext: result.context,
	}})
}

// rewrittenCodexBashInput rewrites only a single, unpiped `cat <source>` for
// a file the daemon confirms is indexed. Head/tail, compound commands,
// redirects, and search/list shapes retain advisory behavior because changing
// their output contract could alter caller semantics.
func rewrittenCodexBashInput(input HookInput) (map[string]any, string, bool) {
	command, _ := input.ToolInput["command"].(string)
	classification := classifyBashCommand(command)
	if classification.Action != BashActionReadSource || classification.Primary != "cat" || !simpleBashCommand(command) {
		return nil, "", false
	}
	indexed, _ := queryFileIndexed(input.CWD, classification.Path)
	if !indexed {
		return nil, "", false
	}
	args, err := json.Marshal(map[string]any{"target": map[string]any{"file": classification.Path}})
	if err != nil {
		return nil, "", false
	}
	updated := cloneStringAnyMap(input.ToolInput)
	updated["command"] = "gortex call read --json " + shellSingleQuote(string(args))
	return updated, fmt.Sprintf("[Gortex] Rewrote indexed source read %s to the exact public read mirror.", classification.Path), true
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func runCodexPostToolUse(data []byte, port int, mode CodexMode) {
	started := time.Now()
	var input postHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	if input.HookEventName != "PostToolUse" {
		return
	}
	if localizationNavigationTool(input.ToolName) {
		if terminal, observed := observeLocalizationTerminal(data); observed {
			_ = emitLocalizationTerminalContext(terminal)
		}
		return
	}
	if input.ToolName == "apply_patch" {
		daemonUp := daemonReachableFn()
		ctx := ""
		// Daemon outage: the mutation briefing is built from daemon answers
		// only, so skip its round-trips instead of paying them just to
		// render nothing (#486).
		if daemonUp {
			ctx = buildMutationBriefing(port, sessionScope{
				SessionID: input.SessionID,
				CWD:       firstNonEmpty(input.CWD, loadHookCWD()),
			})
		}
		emitted := ctx != ""
		logHookEffectiveness("PostToolUse", emitted, daemonUp, 0, time.Since(started))
		if !emitted {
			return
		}
		emitPostToolContext(ctx, mode == CodexModeSuppress)
		return
	}
	if input.ToolName != "Bash" {
		return
	}

	cmd, _ := input.ToolInput["command"].(string)
	classification := classifyBashCommand(cmd)
	switch classification.Action {
	case BashActionGrepLike:
		// Codex wraps grep/rg/ag in Bash. Re-label that narrow shape as Grep so
		// the existing PostToolUse enrichment can parse path:line output and do
		// the graph lookup without changing Claude Code behavior.
		input.ToolName = "Grep"
	case BashActionFindName, BashActionFileList:
		input.ToolName = "Glob"
	case BashActionReadSource, BashActionReadRange:
		if classification.Path == "" {
			return
		}
		if input.ToolInput == nil {
			input.ToolInput = make(map[string]any)
		}
		input.ToolName = "Read"
		input.ToolInput["file_path"] = classification.Path
	default:
		return
	}

	normalized, err := json.Marshal(input)
	if err != nil {
		return
	}
	if mode != CodexModeSuppress {
		runPostToolUse(normalized)
		return
	}
	daemonUp := daemonReachableFn()
	ctx := ""
	// Same stand-down as runPostToolUse's own gate (#486): the follow-ups
	// need daemon answers and point at tools an outage cannot serve.
	if daemonUp {
		ctx = postToolContext(input)
	}
	emitted := ctx != ""
	logHookEffectiveness("PostToolUse", emitted, daemonUp, 0, time.Since(started))
	if emitted {
		emitPostToolContext(ctx, true)
	}
}
