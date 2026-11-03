package hooks

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/modelhint"
)

// antigravity.go implements the native Google Antigravity (agy CLI /
// Antigravity 2.0 / IDE) lifecycle hook protocol (~/.gemini/config/hooks.json).
//
// Key protocol differences from Claude Code and Copilot CLI:
//
//  1. Event Context on Stdin: Antigravity passes camelCase protojson with
//     `conversationId`, `workspacePaths` (slice of active workspace roots),
//     `transcriptPath`, `modelName`, and for tool events `toolCall: {name, args}`
//     plus `stepIdx`. Because hook commands execute with CWD set to the
//     directory containing `hooks.json` (~/.gemini/config), the active
//     repository root MUST be taken from `workspacePaths[0]`.
//  2. Lazy MCP Unwrapping (`call_mcp_tool`): Antigravity invokes lazy-loaded
//     MCP tools through a meta-tool `call_mcp_tool` with arguments
//     `{"ServerName": "gortex", "ToolName": "<op>", "Arguments": {...}}`.
//     Unwrapping `call_mcp_tool` into `mcp__gortex__<op>` allows Gortex's
//     consult-unlock marker, adaptive-nudge streak reset, full-file read
//     nudge, and localization terminal state machine to observe Gortex MCP
//     calls transparently.
//  3. Per-Event Stdout Schema:
//     - SessionStart:  `{"injectSteps": [{"systemMessage": {"systemMessage": "..."}}]}`
//     - PreInvocation: `{"injectSteps": [{"ephemeralMessage": "..."}]}`
//     - PreToolUse:    `{"decision": "allow"}` (with optional `"overwrite"`), or
//                      `{"decision": "deny", "reason": "..."}`
//                      (NOTE: never emit `{}` on PreToolUse — Antigravity's
//                      `CallPreToolHooks` treats a non-nil result with empty
//                      `decision` and `allowTool == false` as `"deny"`!)
//     - PostToolUse:   `{}`
//     - Stop:          `{"decision": "continue", "reason": "..."}` (or `{}`)

var antigravityToolNames = map[string]string{
	"read_file":                  "Read",
	"view_file":                  "Read",
	"grep_search":                "Grep",
	"find_by_name":               "Glob",
	"list_dir":                   "Glob",
	"run_command":                "Bash",
	"edit_file":                  "Edit",
	"replace_file_content":       "Edit",
	"multi_replace_file_content": "Edit",
	"write_file":                 "Write",
	"write_to_file":              "Write",
}

type antigravityToolCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type antigravityInput struct {
	HookEventName      string `json:"hookEventName"`
	HookEventNameSnake string `json:"hook_event_name"`
	EventName          string `json:"event"`

	ConversationID string   `json:"conversationId"`
	SessionID      string   `json:"sessionId"`
	SessionIDSnake string   `json:"session_id"`
	WorkspacePaths []string `json:"workspacePaths"`
	CWD            string   `json:"cwd"`
	TranscriptPath string   `json:"transcriptPath"`
	ModelName      string   `json:"modelName"`
	Model          string   `json:"model"`

	ToolCall      *antigravityToolCall `json:"toolCall"`
	ToolName      string               `json:"toolName"`
	ToolNameSnake string               `json:"tool_name"`
	ToolArgs      map[string]any       `json:"toolArgs"`
	ToolInput     map[string]any       `json:"tool_input"`
	StepIdx       *int                 `json:"stepIdx"`
	ToolUseID     string               `json:"tool_use_id"`
	Error         string               `json:"error"`
	Result        json.RawMessage      `json:"result"`
	ToolResponse  json.RawMessage      `json:"toolResponse"`
	ToolResult    json.RawMessage      `json:"tool_response"`

	InvocationNum      int    `json:"invocationNum"`
	InitialNumSteps    int    `json:"initialNumSteps"`
	LastUserInput      string `json:"lastUserInput"`
	LastUserInputSnake string `json:"last_user_input"`
	Prompt             string `json:"prompt"`

	ExecutionNum          int    `json:"executionNum"`
	TerminationReason     string `json:"terminationReason"`
	FullyIdle             bool   `json:"fullyIdle"`
	FinalModelOutput      string `json:"finalModelOutput"`
	FinalModelOutputSnake string `json:"final_model_output"`
	LastAssistantMessage  string `json:"last_assistant_message"`
	StopHookActive        bool   `json:"stop_hook_active"`
}

func (in antigravityInput) sessionID() string {
	return firstNonEmpty(in.ConversationID, in.SessionID, in.SessionIDSnake)
}

func (in antigravityInput) cwd() string {
	for _, p := range in.WorkspacePaths {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			return trimmed
		}
	}
	return strings.TrimSpace(in.CWD)
}

func (in antigravityInput) model() string {
	m := firstNonEmpty(in.ModelName, in.Model)
	if strings.EqualFold(m, "auto") {
		return ""
	}
	return m
}

func (in antigravityInput) toolUseID() string {
	if strings.TrimSpace(in.ToolUseID) != "" {
		return strings.TrimSpace(in.ToolUseID)
	}
	if in.StepIdx != nil {
		return "step-" + strconv.Itoa(*in.StepIdx)
	}
	if name, _ := in.rawToolAndArgs(); name != "" {
		return "step-0"
	}
	return ""
}

func (in antigravityInput) rawToolAndArgs() (string, map[string]any) {
	if in.ToolCall != nil && strings.TrimSpace(in.ToolCall.Name) != "" {
		return strings.TrimSpace(in.ToolCall.Name), in.ToolCall.Args
	}
	name := firstNonEmpty(in.ToolName, in.ToolNameSnake)
	if len(in.ToolArgs) > 0 {
		return name, in.ToolArgs
	}
	return name, in.ToolInput
}

func (in antigravityInput) toolResponse() json.RawMessage {
	if len(in.Result) > 0 {
		return in.Result
	}
	if len(in.ToolResponse) > 0 {
		return in.ToolResponse
	}
	return in.ToolResult
}

func (in antigravityInput) userPrompt() string {
	if p := firstNonEmpty(in.LastUserInput, in.LastUserInputSnake, in.Prompt); p != "" {
		return stripAntigravityUserRequestTags(p)
	}
	if strings.TrimSpace(in.TranscriptPath) != "" {
		return readAntigravityTranscriptLastStep(in.TranscriptPath, "USER_INPUT")
	}
	return ""
}

func (in antigravityInput) finalModelOutput() string {
	if m := firstNonEmpty(in.FinalModelOutput, in.FinalModelOutputSnake, in.LastAssistantMessage); m != "" {
		return m
	}
	if strings.TrimSpace(in.TranscriptPath) != "" {
		return readAntigravityTranscriptLastStep(in.TranscriptPath, "PLANNER_RESPONSE")
	}
	return ""
}

type antigravitySystemMessage struct {
	SystemMessage string `json:"systemMessage"`
}

type antigravityInjectedStep struct {
	SystemMessage    *antigravitySystemMessage `json:"systemMessage,omitempty"`
	EphemeralMessage string                    `json:"ephemeralMessage,omitempty"`
}

type antigravityInjectResponse struct {
	InjectSteps []antigravityInjectedStep `json:"injectSteps,omitempty"`
}

type antigravityDecisionResponse struct {
	Decision  string         `json:"decision,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	Overwrite map[string]any `json:"overwrite,omitempty"`
}

// RunAntigravity reads one Antigravity hook payload from stdin, writes the
// JSON response on stdout, and exits 0.
func RunAntigravity(port int, mode Mode, eventFlag string) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	out, code := handleAntigravity(data, port, mode, eventFlag)
	if out != "" {
		fmt.Print(out)
	}
	if code != 0 {
		os.Exit(code)
	}
}

func handleAntigravity(data []byte, port int, mode Mode, eventFlag string) (string, int) {
	// No --event: a legacy `gortex hook --agent antigravity` entry in
	// ~/.gemini/settings.json, written before the hooks moved to
	// ~/.gemini/config/hooks.json (whose entries always pass --event). Only
	// Gemini CLI reads that file, so answer exactly as before.
	if strings.TrimSpace(eventFlag) == "" {
		return externalAgentResponse(data), 0
	}

	var in antigravityInput
	if err := json.Unmarshal(data, &in); err != nil {
		if strings.EqualFold(strings.TrimSpace(eventFlag), "PreToolUse") {
			return marshalAntigravity(antigravityDecisionResponse{Decision: "allow"}), 0
		}
		return "{}", 0
	}

	rawEvent := firstNonEmpty(eventFlag, in.HookEventName, in.HookEventNameSnake, in.EventName)
	event := normalizeAntigravityEvent(rawEvent, in)
	if event == "AfterTool" {
		// Legacy Gemini-style AfterTool fallback.
		out := ""
		if ctx := buildStaleIndexHint(in.cwd()); strings.TrimSpace(ctx) != "" {
			if b, err := json.Marshal(HookOutput{HookSpecificOutput: &HookSpecificOutput{
				HookEventName:     "AfterTool",
				AdditionalContext: ctx,
			}}); err == nil {
				out = string(b)
			}
		}
		return out, 0
	}

	cwd := in.cwd()
	setHookCWD(cwd)
	defer setHookCWD("")

	captureAntigravityModelHint(event, cwd, in.model())

	switch event {
	case "SessionStart":
		return antigravitySessionStart(in, cwd)
	case "PreInvocation":
		return antigravityPreInvocation(in, cwd)
	case "PreToolUse":
		return antigravityPreToolUse(in, cwd, port, mode)
	case "PostToolUse":
		return antigravityPostToolUse(in, cwd)
	case "Stop":
		return antigravityStop(in, cwd)
	default:
		return "{}", 0
	}
}

func normalizeAntigravityEvent(raw string, in antigravityInput) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "sessionstart":
		return "SessionStart"
	case "preinvocation", "userpromptsubmit", "userpromptsubmitted":
		return "PreInvocation"
	case "pretooluse":
		return "PreToolUse"
	case "posttooluse":
		return "PostToolUse"
	case "aftertool":
		return "AfterTool"
	case "stop", "agentstop":
		return "Stop"
	}
	if toolName, _ := in.rawToolAndArgs(); toolName != "" {
		if len(in.toolResponse()) > 0 || in.Error != "" {
			return "PostToolUse"
		}
		return "PreToolUse"
	}
	if in.TerminationReason != "" || in.ExecutionNum > 0 || in.FullyIdle ||
		in.FinalModelOutput != "" || in.FinalModelOutputSnake != "" || in.LastAssistantMessage != "" {
		return "Stop"
	}
	if in.InvocationNum > 0 || in.InitialNumSteps > 0 || in.Prompt != "" ||
		in.LastUserInput != "" || in.LastUserInputSnake != "" {
		return "PreInvocation"
	}
	return "SessionStart"
}

func captureAntigravityModelHint(event, cwd, model string) {
	switch event {
	case "SessionStart", "PreInvocation":
	default:
		return
	}
	if strings.TrimSpace(model) == "" {
		return
	}
	modelhint.Write(cwd, model, "antigravity")
}

func antigravitySessionStart(in antigravityInput, cwd string) (string, int) {
	started := time.Now()
	if clearPayload, err := json.Marshal(localizationTerminalHookInput{
		HookEventName: "SessionStart",
		SessionID:     in.sessionID(),
		CWD:           cwd,
	}); err == nil {
		if clearLocalizationTerminalFromHook(clearPayload) {
			localizationTerminalTelemetry("cleared_session", false, started)
		}
	}

	ctx := buildSessionStartBriefing(cwd)
	logHookEffectiveness("SessionStart", ctx != "", daemonReachableFn(), 0, time.Since(started))
	if strings.TrimSpace(ctx) == "" {
		return "{}", 0
	}
	return marshalAntigravity(antigravityInjectResponse{
		InjectSteps: []antigravityInjectedStep{
			{SystemMessage: &antigravitySystemMessage{SystemMessage: ctx}},
		},
	}), 0
}

func antigravityPreInvocation(in antigravityInput, cwd string) (string, int) {
	started := time.Now()

	// agy fires PreInvocation before every model invocation, not once per
	// user prompt: invocationNum is 0 for a prompt's first invocation and
	// counts up as the model takes tool steps. A later invocation belongs to
	// the turn already in progress, so it must not discard that turn's
	// localization state or repeat the prompt briefing. With no turn on
	// record (a missed first invocation), fall through and start one. A
	// continuation is not logged as a run: the effectiveness log counts
	// UserPromptSubmit once per prompt, as on the other hosts.
	if in.InvocationNum > 0 {
		if _, inTurn := currentLocalizationTurnState(in.sessionID(), "", "", cwd); inTurn {
			return "{}", 0
		}
	}

	prompt := in.userPrompt()

	if clearPayload, err := json.Marshal(localizationTerminalHookInput{
		HookEventName: "UserPromptSubmit",
		SessionID:     in.sessionID(),
		CWD:           cwd,
		Prompt:        prompt,
	}); err == nil {
		if clearLocalizationTerminalFromHook(clearPayload) {
			localizationTerminalTelemetry("cleared_prompt", false, started)
		}
	}

	daemonUp := daemonReachableFn()
	ctx := ""
	if daemonUp && prompt != "" {
		ctx = buildUserPromptSubmitContext("UserPromptSubmit", prompt)
	}
	logHookEffectiveness("UserPromptSubmit", ctx != "", daemonUp, 0, time.Since(started))
	if strings.TrimSpace(ctx) == "" {
		return "{}", 0
	}
	return marshalAntigravity(antigravityInjectResponse{
		InjectSteps: []antigravityInjectedStep{
			{EphemeralMessage: ctx},
		},
	}), 0
}

func antigravityPreToolUse(in antigravityInput, cwd string, port int, mode Mode) (string, int) {
	started := time.Now()
	name, args := in.rawToolAndArgs()
	rawTool, mappedTool, mappedArgs := unwrapAntigravityTool(name, args)

	input := HookInput{
		HookEventName: "PreToolUse",
		ToolName:      mappedTool,
		ToolInput:     mappedArgs,
		ToolUseID:     in.toolUseID(),
		CWD:           cwd,
		SessionID:     in.sessionID(),
	}

	if reason, denied := antigravityLocalizationTerminalDeny(input, started); denied {
		return marshalAntigravity(antigravityDecisionResponse{
			Decision: "deny",
			Reason:   reason,
		}), 0
	}

	terminalTurn, terminalTurnReady := currentLocalizationTurnState(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	authToken := ""
	if terminalTurnReady {
		if tok, ok := snapshotLocalizationToolUseWithAuth(input, terminalTurn.Identity); ok {
			authToken = tok
		}
	}
	updatedInput := localizationPreToolUpdatedInput(input, authToken, terminalTurn.ProblemStatement)
	overwrite := antigravityWrapOverwrite(name, args, updatedInput)

	recordSessionWriteTargets(input)

	isGortexTool := isGortexMCPToolName(rawTool)
	if isGortexTool && mode == ModeConsultUnlock {
		markGraphConsulted(input.SessionID)
	}

	allowOut := marshalAntigravity(antigravityDecisionResponse{
		Decision:  "allow",
		Overwrite: overwrite,
	})

	if !preToolUsePolicyTool(mappedTool) || cwd == "" {
		return allowOut, 0
	}

	daemonUp := daemonReachableFn()
	out := allowOut
	emitted := false
	// Same pipeline as Claude and Copilot: Gortex tools go through applyMode
	// too, so nudge mode resets its streak on them, and the compress-bodies
	// read advisory only denies under GORTEX_HOOK_FORCE_COMPRESS. PreToolUse
	// has no soft-context channel, so advisory context is dropped.
	if daemonUp {
		if result := applyMode(input, isGortexTool, mode, enrich(input, port)); result.deny {
			emitted = true
			out = marshalAntigravity(antigravityDecisionResponse{
				Decision: "deny",
				Reason:   result.reason,
			})
		}
	}

	logHookEffectiveness("PreToolUse", emitted, daemonUp, hookAlternationSegmentCount(input), time.Since(started))
	return out, 0
}

func antigravityWrapOverwrite(rawToolName string, rawArgs, updatedInput map[string]any) map[string]any {
	if len(updatedInput) == 0 {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(rawToolName), "call_mcp_tool") {
		argKey := "Arguments"
		if _, ok := rawArgs["arguments"]; ok {
			if _, hasUpper := rawArgs["Arguments"]; !hasUpper {
				argKey = "arguments"
			}
		}
		return map[string]any{argKey: updatedInput}
	}
	return updatedInput
}

func antigravityLocalizationTerminalDeny(input HookInput, started time.Time) (string, bool) {
	terminalTurn, ready := currentLocalizationTurnState(input.SessionID, input.PromptID, input.AgentID, input.CWD)
	if !ready {
		return "", false
	}
	marker, marked := localizationTerminalMarkerFor(terminalTurn.Identity)
	if !marked {
		return "", false
	}

	reason := ""
	switch {
	case !marker.Advisory:
		reason = localizationTerminalDenyReason
	case localizationNavigationTool(input.ToolName):
		reason = localizationAdvisoryDenyReason
	case localizationRedirectedHostTool(input.ToolName):
		reason = localizationAdvisoryDenyReason
	}
	if reason == "" {
		return "", false
	}
	if answer := strings.TrimSpace(marker.FinalResponse); answer != "" {
		reason += "\n\n" + answer
	}
	localizationTerminalTelemetry("denied", true, started)
	return reason, true
}

func antigravityPostToolUse(in antigravityInput, cwd string) (string, int) {
	started := time.Now()
	name, args := in.rawToolAndArgs()
	_, mappedTool, mappedArgs := unwrapAntigravityTool(name, args)

	recorded := false
	defer func() {
		if !recorded {
			logHookEffectivenessUnknown("PostToolUse", false, 0, time.Since(started))
		}
	}()

	if localizationNavigationTool(mappedTool) {
		normBytes, err := json.Marshal(localizationTerminalHookInput{
			HookEventName: "PostToolUse",
			ToolName:      mappedTool,
			ToolUseID:     in.toolUseID(),
			ToolInput:     mappedArgs,
			SessionID:     in.sessionID(),
			CWD:           cwd,
			ToolResponse:  in.toolResponse(),
		})
		if err == nil {
			if _, observed := observeLocalizationTerminal(normBytes); observed {
				recorded = true
				localizationTerminalTelemetry("observed", false, started)
			}
		}
	}
	return "{}", 0
}

func antigravityStop(in antigravityInput, cwd string) (string, int) {
	started := time.Now()
	_ = clearLocalizationProblemStatementForTurn(in.sessionID(), "", "", cwd)

	emitted := false
	defer func() {
		logHookEffectiveness("Stop", emitted, daemonReachableFn(), 0, time.Since(started))
	}()

	if in.StopHookActive {
		return "{}", 0
	}
	if reason := strings.TrimSpace(in.TerminationReason); reason != "" && reason != "NO_TOOL_CALL" {
		return "{}", 0
	}

	lastMsg := in.finalModelOutput()

	if claimCheck := localizationClaimCheck(PostTaskInput{
		HookEventName:        "Stop",
		SessionID:            in.sessionID(),
		CWD:                  cwd,
		TranscriptPath:       in.TranscriptPath,
		LastAssistantMessage: lastMsg,
	}); claimCheck != "" {
		emitted = true
		return marshalAntigravity(antigravityDecisionResponse{
			Decision: "continue",
			Reason:   claimCheck,
		}), 0
	}
	return "{}", 0
}

// unwrapAntigravityTool normalizes an Antigravity tool call into Gortex's
// canonical tool vocabulary, unwrapping lazy `call_mcp_tool` invocations
// targeting the "gortex" server into `mcp__gortex__<ToolName>`.
func unwrapAntigravityTool(name string, args map[string]any) (rawTool, mappedTool string, mappedArgs map[string]any) {
	trimmed := strings.TrimSpace(name)
	lower := strings.ToLower(trimmed)

	if lower == "call_mcp_tool" {
		server := antigravityStringArg(args, "ServerName", "server_name", "serverName")
		tool := antigravityStringArg(args, "ToolName", "tool_name", "toolName")
		inner := antigravityMapArg(args, "Arguments", "arguments")
		if strings.EqualFold(server, "gortex") && tool != "" {
			canonical := gortexMCPToolPrefix + tool
			return canonical, canonical, inner
		}
		if server != "" && tool != "" {
			other := "mcp__" + server + "__" + tool
			return other, other, inner
		}
	}

	if strings.HasPrefix(lower, "mcp_gortex_") {
		op := strings.TrimPrefix(trimmed, trimmed[:len("mcp_gortex_")])
		canonical := gortexMCPToolPrefix + op
		return canonical, canonical, cloneStringAnyMap(args)
	}

	if mapped, ok := antigravityToolNames[lower]; ok {
		return trimmed, mapped, antigravityToolInput(mapped, args)
	}
	return trimmed, trimmed, cloneStringAnyMap(args)
}

func antigravityUnquoteArg(v string) string {
	trimmed := strings.TrimSpace(v)
	if strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) && len(trimmed) >= 2 {
		var unquoted string
		if err := json.Unmarshal([]byte(trimmed), &unquoted); err == nil {
			return unquoted
		}
		return strings.Trim(trimmed, `"`)
	}
	return trimmed
}

func antigravityStringArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok {
			s := antigravityUnquoteArg(v)
			if s != "" {
				return s
			}
		}
	}
	return ""
}

func antigravityMapArg(args map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		switch v := args[k].(type) {
		case map[string]any:
			return cloneStringAnyMap(v)
		case string:
			var parsed map[string]any
			if json.Unmarshal([]byte(v), &parsed) == nil && parsed != nil {
				return parsed
			}
		}
	}
	return map[string]any{}
}

func antigravityToolInput(mappedTool string, args map[string]any) map[string]any {
	if len(args) == 0 {
		return args
	}
	out := cloneStringAnyMap(args)
	switch mappedTool {
	case "Read":
		antigravityAliasStringKey(out, "file_path", "Path", "AbsolutePath", "TargetFile", "path", "filePath", "file")
		antigravityAliasAnyKey(out, "offset", "StartLine", "start_line", "startLine")
		antigravityAliasAnyKey(out, "limit", "EndLine", "end_line", "endLine")
	case "Write", "Edit":
		antigravityAliasStringKey(out, "file_path", "Path", "TargetFile", "AbsolutePath", "path", "filePath", "file")
	case "Grep":
		antigravityAliasStringKey(out, "pattern", "Query", "query", "regex", "search")
		antigravityAliasStringKey(out, "path", "SearchPath", "SearchDirectory", "DirectoryPath")
	case "Glob":
		antigravityAliasStringKey(out, "pattern", "Pattern", "Query", "glob", "query")
		antigravityAliasStringKey(out, "path", "SearchDirectory", "DirectoryPath", "SearchPath")
	case "Bash":
		antigravityAliasStringKey(out, "command", "CommandLine", "command_line", "cmd", "script")
		antigravityAliasStringKey(out, "cwd", "Cwd")
	}
	return out
}

func antigravityAliasStringKey(args map[string]any, canonical string, aliases ...string) {
	if value, ok := args[canonical].(string); ok && strings.TrimSpace(value) != "" {
		args[canonical] = antigravityUnquoteArg(value)
		return
	}
	for _, alias := range aliases {
		if value, ok := args[alias].(string); ok {
			unquoted := antigravityUnquoteArg(value)
			if unquoted != "" {
				args[canonical] = unquoted
				return
			}
		}
	}
}

func antigravityAliasAnyKey(args map[string]any, canonical string, aliases ...string) {
	if _, exists := args[canonical]; exists {
		return
	}
	for _, alias := range aliases {
		if v, exists := args[alias]; exists && v != nil {
			args[canonical] = v
			return
		}
	}
}

func stripAntigravityUserRequestTags(s string) string {
	s = strings.TrimSpace(s)
	if start := strings.Index(s, "<USER_REQUEST>"); start >= 0 {
		inner := s[start+len("<USER_REQUEST>"):]
		if end := strings.LastIndex(inner, "</USER_REQUEST>"); end >= 0 {
			inner = inner[:end]
		}
		return strings.TrimSpace(inner)
	}
	return s
}

func readAntigravityTranscriptLastStep(path, stepType string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 2<<20)
	last := ""
	for scanner.Scan() {
		var step struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		}
		if json.Unmarshal(scanner.Bytes(), &step) != nil {
			continue
		}
		if step.Type == stepType && strings.TrimSpace(step.Content) != "" {
			last = strings.TrimSpace(step.Content)
		}
	}
	if stepType == "USER_INPUT" {
		return stripAntigravityUserRequestTags(last)
	}
	return last
}

func marshalAntigravity(response any) string {
	out, err := json.Marshal(response)
	if err != nil {
		return "{}"
	}
	return string(out)
}
