package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/localizationauth"
	"github.com/zzet/gortex/internal/telemetry"
)

var facadeDescriptions = map[string]string{
	"explore":         "Localize a task in indexed code.",
	"search":          "Search indexed code and artifacts by operation.",
	"read":            "Read files, symbols, or context by operation.",
	"relations":       "Query symbol relationships by operation.",
	"trace":           "Trace graph or data flow by operation.",
	"analyze":         "Run graph analysis by kind.",
	"ask":             "Ask the configured research agent.",
	"change":          "Assess a proposed or existing change.",
	"edit":            "Apply guarded source or file changes.",
	"refactor":        "Apply a semantic refactor.",
	"review":          "Build or critique a code review.",
	"publish_review":  "Publish a review to a forge.",
	"pr":              "Inspect pull requests.",
	"recall":          "Read notes, memories, or notebooks.",
	"remember":        "Persist notes, memories, or suppressions.",
	"workspace":       "Inspect workspace and index state.",
	"workspace_admin": "Change workspace or daemon state.",
	"session":         "Change volatile session state.",
	"overlay":         "Change speculative overlay state.",
	"response":        "Inspect a buffered response.",
	"capabilities":    "List operations or return an exact schema.",
}

func boolPointer(v bool) *bool { return &v }

func facadeAnnotation(name string) mcpgo.ToolAnnotation {
	readOnly := true
	destructive := false
	openWorld := false
	switch name {
	case "ask", "pr", "review":
		openWorld = true
	case "edit", "refactor", "remember", "workspace_admin":
		readOnly = false
		destructive = true
		if name == "workspace_admin" {
			openWorld = true
		}
	case "overlay", "session":
		readOnly = false
	case "publish_review":
		readOnly = false
		destructive = true
		openWorld = true
	}
	return mcpgo.ToolAnnotation{
		ReadOnlyHint:    boolPointer(readOnly),
		DestructiveHint: boolPointer(destructive),
		OpenWorldHint:   boolPointer(openWorld),
	}
}

func facadeTargetProperties() map[string]any {
	return map[string]any{
		"file":     map[string]any{"type": "string"},
		"symbol":   map[string]any{"type": "string"},
		"symbols":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"query":    map[string]any{"type": "string"},
		"artifact": map[string]any{"type": "string"},
		"repo":     map[string]any{"type": "string"},
	}
}

func facadeTargetProperty() mcpgo.PropertyOption {
	return mcpgo.Properties(facadeTargetProperties())
}

func facadeToolDefinition(name string) mcpgo.Tool {
	return facadeToolDefinitionWithOperations(name, facadeCanonicalOperationNames(name))
}

func facadeToolDefinitionWithOperations(name string, operations []string) mcpgo.Tool {
	desc := facadeDescriptions[name]
	annotation := mcpgo.WithToolAnnotation(facadeAnnotation(name))
	freeObject := func(field, _ string) mcpgo.ToolOption {
		return mcpgo.WithObject(field, mcpgo.AdditionalProperties(true))
	}
	operation := mcpgo.WithString("operation")
	options := freeObject("options", "")
	output := freeObject("output", "")
	target := mcpgo.WithObject("target", facadeTargetProperty(), mcpgo.AdditionalProperties(false))

	var opts []mcpgo.ToolOption
	switch name {
	case "explore":
		opts = []mcpgo.ToolOption{
			mcpgo.WithString("operation", mcpgo.Description("Use localize when the requested outcome is files or symbols; it returns terminal evidence. Use task only when diagnosis or implementation will continue.")),
			mcpgo.WithString("task", mcpgo.Description("Task, bug, or question to localize.")),
			mcpgo.WithString("path"),
			mcpgo.WithObject("options",
				mcpgo.Description("Set new_user_task=true only on the first explore call (task or localize) caused by a new user request. Never set it to retry, paraphrase, or continue the current request."),
				mcpgo.AdditionalProperties(true),
			),
			output,
		}
	case "search":
		opts = []mcpgo.ToolOption{operation, mcpgo.WithString("query"), options, output}
	case "read":
		opts = []mcpgo.ToolOption{
			operation, target, freeObject("context", "Read window or source-context controls."),
			mcpgo.WithObject("options",
				mcpgo.Description("Set new_user_task=true only on the first read.file call caused by a new user request. Never set it to retry, continue the current request, or bypass answer_ready."),
				mcpgo.AdditionalProperties(true),
			),
			output,
		}
	case "relations", "trace":
		opts = []mcpgo.ToolOption{operation, freeObject("target", "Primary file or symbol target."), freeObject("to", "Optional destination target."), options, output}
	case "analyze":
		opts = []mcpgo.ToolOption{
			mcpgo.WithString("kind", mcpgo.Description("Analysis kind or operation; omit to list supported kinds.")),
			freeObject("target", "Optional analysis target."), options, output,
		}
	case "ask":
		opts = []mcpgo.ToolOption{mcpgo.WithString("question", mcpgo.Required()), options, output}
	case "change":
		opts = []mcpgo.ToolOption{
			operation, target,
			freeObject("source", "Diff, working tree, ranges, symbols, or other change source."),
			options, output,
		}
	case "review":
		opts = []mcpgo.ToolOption{operation, freeObject("source", "Diff, working tree, ranges, symbols, or review source."), options, output}
	case "edit":
		opts = []mcpgo.ToolOption{
			operation, target, mcpgo.WithString("match"), mcpgo.WithString("replacement"),
			mcpgo.WithString("content"), freeObject("guard", "Stale-write and occurrence guards."),
			mcpgo.WithArray("changes", mcpgo.Description("Batch file or symbol edits."), mcpgo.Items(map[string]any{"type": "object", "additionalProperties": true})),
			mcpgo.WithBoolean("dry_run"), options, output,
		}
	case "refactor":
		opts = []mcpgo.ToolOption{
			operation, target, mcpgo.WithString("new_name"), mcpgo.WithString("destination"),
			mcpgo.WithBoolean("dry_run"), options, output,
		}
	case "publish_review", "pr", "recall", "remember", "workspace", "workspace_admin", "overlay", "response":
		// Cold domain facades keep only the stable discriminator plus a
		// runtime-validated payload. capabilities returns the exact operation
		// schema on demand without changing tools/list.
		opts = []mcpgo.ToolOption{operation, freeObject("arguments", "Operation arguments.")}
	case "session":
		opts = []mcpgo.ToolOption{
			mcpgo.WithString("operation", mcpgo.Description("Session operation; see capabilities. Use subscribe or unsubscribe with channel.")),
			mcpgo.WithString("channel", mcpgo.Description("daemon_health, diagnostics, graph_invalidated, stale_refs, or workspace_readiness")),
			freeObject("arguments", "Optional session arguments."),
		}
	case "capabilities":
		opts = []mcpgo.ToolOption{
			mcpgo.WithString("domain", mcpgo.Description("Public tool name; omit to list all tool domains.")),
			mcpgo.WithString("operation", mcpgo.Description("Operation name; omit to list the domain.")),
			mcpgo.WithString("detail", mcpgo.Description("summary or schema")),
		}
	default:
		opts = []mcpgo.ToolOption{operation, target, options, output}
	}
	// Response shaping is universal so the shell mirror can merge --format into
	// the same public request object for every compact tool. Common-domain cases
	// already include output above; reapplying the same property is idempotent.
	opts = append(opts, output)
	opts = append([]mcpgo.ToolOption{mcpgo.WithDescription(desc), annotation}, opts...)
	tool := mcpgo.NewTool(name, opts...)
	if name == "read" {
		if optionsSchema, ok := tool.InputSchema.Properties["options"].(map[string]any); ok {
			optionsSchema["properties"] = map[string]any{
				"new_user_task": map[string]any{
					"type":        "boolean",
					"description": "True only for the first read.file call caused by a new user request.",
				},
			}
		}
	}
	if targetSchema, ok := tool.InputSchema.Properties["target"].(map[string]any); ok {
		// The public facade validator already requires one selector. Encode that
		// contract in tools/list as well so a caller can construct its first read
		// without a capabilities round-trip or a rejected probe.
		targetSchema["minProperties"] = 1
		targetSchema["maxProperties"] = 1
		targetSchema["description"] = "Choose exactly one selector."
		if name == "read" {
			targetSchema["description"] = "Choose exactly one selector: symbol for one source symbol, symbols for a batch, or file for file content."
		}
	}
	discriminator := "operation"
	if name == "analyze" {
		discriminator = "kind"
	}
	if property, ok := tool.InputSchema.Properties[discriminator].(map[string]any); ok && len(operations) > 0 {
		property["enum"] = append([]string(nil), operations...)
	}
	return tool
}

func facadeCanonicalOperationNames(name string) []string {
	seen := make(map[string]bool)
	for _, spec := range facadeOperationSpecs() {
		if spec.Facade == name && !spec.Hidden {
			seen[spec.Operation] = true
		}
	}
	if name == "analyze" {
		for _, kind := range AnalyzeKinds() {
			if !analyzeKindRequiresAdmin(kind) {
				seen[kind] = true
			}
		}
	}
	if name == "session" {
		for operation := range seen {
			if strings.HasPrefix(operation, "subscribe_") || strings.HasPrefix(operation, "unsubscribe_") {
				delete(seen, operation)
			}
		}
		seen["subscribe"] = true
		seen["unsubscribe"] = true
	}
	operations := make([]string, 0, len(seen))
	for operation := range seen {
		operations = append(operations, operation)
	}
	sort.Strings(operations)
	return operations
}

func (s *Server) facadeToolDefinition(name string) mcpgo.Tool {
	specs := s.capabilityOperations(name)
	operations := make([]string, 0, len(specs))
	for _, spec := range specs {
		operations = append(operations, spec.Operation)
	}
	return facadeToolDefinitionWithOperations(name, operations)
}

// registerFacadeTools installs every facade name directly into the live MCP
// server. Session filtering keeps them out of legacy surfaces, while a
// facade-v1 session receives all names from its first tools/list and never
// depends on deferred promotion or tools/list_changed.
func (s *Server) registerFacadeTools() {
	for _, name := range facadeToolNames() {
		if _, alreadyLegacy := s.facades.legacy(name); alreadyLegacy {
			continue // explore/analyze/review (and ask when configured)
		}
		facade := name
		tool := s.facadeToolDefinition(facade)
		var handler server.ToolHandlerFunc
		if facade == "capabilities" {
			handler = s.handleCapabilities
		} else {
			handler = func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return s.handleFacade(ctx, facade, req)
			}
		}
		// Deliberately bypass addTool/lazy routing. The per-session surface
		// filter hides these from legacy clients; facade clients need every
		// dispatcher callable immediately.
		scrubToolText(&tool)
		s.mcpServer.AddTool(tool, s.wrapControlToolHandler(handler))
	}
}

// facadeSelectorKeys carry the subject of the question. Losing one silently
// changes the answer, so any value under these keys routes to the public
// dispatcher — a malformed selector then earns a structured error instead of
// a confident answer to a question the caller did not ask.
var facadeSelectorKeys = []string{"target", "to"}

// facadeContainerKeys are the remaining public envelope containers of the
// compact surface. They are vocabulary, not payload: no legacy handler has a
// field to read them from, so a call carrying one must be lowered through the
// public dispatcher or its knobs are silently discarded.
var facadeContainerKeys = []string{"arguments", "options", "source", "context", "guard", "output"}

// usesFacadeVocabulary reports whether a call to a reused (legacy-named)
// facade speaks the compact envelope. For the non-selector containers only an
// object counts: every legacy parameter of the reused names is a scalar, so an
// object under one of those keys is unambiguously the public shape and can
// never be a legacy argument that happens to share the name.
func usesFacadeVocabulary(args map[string]any) bool {
	for _, key := range facadeSelectorKeys {
		if value, present := args[key]; present && value != nil {
			return true
		}
	}
	for _, key := range facadeContainerKeys {
		if _, ok := args[key].(map[string]any); ok {
			return true
		}
	}
	return false
}

func (s *Server) wrapLegacyFacade(name string, raw server.ToolHandlerFunc) server.ToolHandlerFunc {
	if !isFacadeToolName(name) {
		return raw
	}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		args := req.GetArguments()
		_, explicitOperation := args["operation"]
		facadeSession := s.effectiveSessionPolicy(ctx).preset == FacadeSurfaceVersion
		// `operation` cannot be the only compact-call signal: analyze
		// discriminates on `kind`, so a well-formed compact analyze call
		// never carries one. Without the vocabulary check below, every
		// analyze(kind=…, target={…}) call outside a facade-v1 session went
		// straight to the legacy handler, which has no target to read — the
		// caller got a repo-wide ranking that looks like an answer.
		if !facadeSession && !explicitOperation && !usesFacadeVocabulary(args) {
			// A bare analyze(kind=…) call with no facade vocabulary still
			// needs the facade when the kind is an aliased operation
			// (processes, communities, contracts, …): the facade holds the
			// captured legacy handler directly, so the call works under the
			// core/defer surface without promoting the legacy tool into the
			// live registry. Native dispatcher kinds (hotspots, dead_code,
			// cycles, …) are not aliased and fall through to the dispatcher.
			if name == "analyze" && s.facadeAnalyzeKindAliased(ctx, req) {
				return s.handleFacade(ctx, name, req)
			}
			return raw(ctx, req)
		}
		if name == "analyze" {
			// Compact calls, including native dispatcher kinds, all pass through
			// the same effect split and capability lookup below.
			return s.handleFacade(ctx, name, req)
		}
		return s.handleFacade(ctx, name, req)
	}
}

// facadeAnalyzeKindAliased reports whether an analyze call's requested kind
// is a facade-aliased operation — one that routes to a captured legacy tool
// other than the analyze dispatcher (e.g. processes → get_processes,
// communities → get_communities). Aliased kinds are reachable through the
// facade without promoting the legacy tool into the live registry, so a
// plain analyze(kind=processes) call from a legacy or HTTP session must not
// fall through to the dispatcher's "unknown analyze kind" error.
func (s *Server) facadeAnalyzeKindAliased(ctx context.Context, req mcpgo.CallToolRequest) bool {
	if s == nil || s.facades == nil {
		return false
	}
	operation := requestedAnalyzeKind(req.GetArguments())
	if operation == "" {
		return false
	}
	spec, ok := s.capabilityOperation("analyze", operation)
	return ok && spec.Legacy != "analyze"
}

// decorateLocalizationReadResult makes a reserved localization read carry its
// next completion. JSON object results retain their public shape with one added
// completion field; text results receive the same compact JSON contract in one
// text block. Existing content blocks, structured payloads, error status,
// annotations, and metadata remain intact.
func decorateLocalizationReadResult(result *mcpgo.CallToolResult, completion localizationCompletion) *mcpgo.CallToolResult {
	if result == nil {
		return result
	}
	originalContent := append([]mcpgo.Content(nil), result.Content...)
	originalStructured := result.StructuredContent
	terminalContract := localizationContractFor(completion)
	contract, err := json.Marshal(terminalContract)
	if err != nil {
		return result
	}
	decorateBody := func(body string) string {
		trimmed := strings.TrimSpace(body)
		if len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid([]byte(trimmed)) {
			payload := make(map[string]json.RawMessage)
			if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
				encodedCompletion, marshalErr := json.Marshal(terminalContract.Completion)
				if marshalErr == nil {
					payload["completion"] = encodedCompletion
					payload["terminal"], _ = json.Marshal(terminalContract.Terminal)
					if merged, marshalErr := json.Marshal(payload); marshalErr == nil {
						return string(merged)
					}
				}
			}
		}
		return body + "\n\n" + string(contract)
	}
	decoratedText := false
	for index, content := range result.Content {
		text, ok := mcpgo.AsTextContent(content)
		if !ok {
			continue
		}
		text.Text = decorateBody(text.Text)
		result.Content[index] = *text
		decoratedText = true
		break
	}
	if !decoratedText {
		result.Content = append(result.Content, mcpgo.NewTextContent(string(contract)))
	}
	switch payload := originalStructured.(type) {
	case nil:
		result.StructuredContent = localizationReadStructuredPayload(originalContent, completion)
	case map[string]any:
		decorated := make(map[string]any, len(payload)+2)
		for key, value := range payload {
			decorated[key] = value
		}
		decorated["completion"] = terminalContract.Completion
		decorated["terminal"] = terminalContract.Terminal
		result.StructuredContent = decorated
	default:
		result.StructuredContent = map[string]any{
			"payload":    payload,
			"completion": terminalContract.Completion,
			"terminal":   terminalContract.Terminal,
		}
	}
	return attachLocalizationHostEnvelope(result, completion, completion.digest)
}

// localizationReadStructuredPayload mirrors a content-only legacy response
// into structuredContent before adding completion. Some MCP hosts prefer
// structuredContent whenever it is present, so a completion-only object would
// otherwise hide the source payload that remains in content.
func localizationReadStructuredPayload(content []mcpgo.Content, completion localizationCompletion) map[string]any {
	contract := localizationContractFor(completion)
	structured := map[string]any{"completion": contract.Completion, "terminal": contract.Terminal}
	if len(content) == 0 {
		return structured
	}
	if len(content) == 1 {
		if text, ok := mcpgo.AsTextContent(content[0]); ok {
			trimmed := strings.TrimSpace(text.Text)
			if len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid([]byte(trimmed)) {
				payload := make(map[string]any)
				if err := json.Unmarshal([]byte(trimmed), &payload); err == nil {
					payload["completion"] = contract.Completion
					payload["terminal"] = contract.Terminal
					return payload
				}
			}
			structured["text"] = text.Text
			return structured
		}
	}
	structured["content"] = content
	return structured
}

func parseLocalizationNewUserBoundary(facade, operation string, arguments map[string]any) (bool, *mcpgo.CallToolResult) {
	boundaryAllowed := (facade == "explore" && (operation == "task" || operation == "localize")) ||
		(facade == "read" && operation == "file")
	rawOptions, present := arguments["options"]
	if !present || rawOptions == nil {
		return false, nil
	}
	options, ok := rawOptions.(map[string]any)
	if !ok {
		if !boundaryAllowed {
			return false, nil
		}
		return false, NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   "options must be an object",
			Data:      map[string]any{"field": "options", "operation": operation},
		})
	}
	rawBoundary, present := options["new_user_task"]
	if !present {
		return false, nil
	}
	boundary, ok := rawBoundary.(bool)
	if !ok {
		return false, NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   "options.new_user_task must be a boolean",
			Data:      map[string]any{"field": "options.new_user_task", "operation": operation},
		})
	}
	if !boundaryAllowed {
		return false, NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   "options.new_user_task is valid only on the first explore.task, explore.localize, or read.file call of a new user request",
			Data:      map[string]any{"field": "options.new_user_task", "facade": facade, "operation": operation},
		})
	}
	return boundary, nil
}

func (s *Server) handleFacade(ctx context.Context, facade string, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	started := time.Now()
	input, _ := req.Params.Arguments.(map[string]any)
	localizationAuthToken := localizationauth.TakeArgument(input)
	operation := resolveFacadeOperationAlias(facade, normalizeFacadeOperation(req.GetString("operation", "")))
	if facade == "analyze" {
		operation = requestedAnalyzeKind(req.GetArguments())
		if operation == "" {
			operation = "help"
		}
	}
	if operation == "" {
		operation = inferFacadeOperation(facade, req.GetArguments())
	}
	if operation == "" {
		operation = defaultFacadeOperation(facade)
	}
	if facade == "read" {
		operation = normalizeFacadeReadOperation(operation, req.GetArguments())
	}
	terminal := s.localizationFor(ctx)
	freshLocalizeFlow := facade == "explore" && operation == "localize"
	newUserTask, invalidBoundary := parseLocalizationNewUserBoundary(facade, operation, req.GetArguments())
	if invalidBoundary != nil {
		s.recordFacadeTelemetry(facade, operation, facadeOutcomeInvalidArgument, time.Since(started))
		return invalidBoundary, nil
	}
	newUserExploreFlow := facade == "explore" && newUserTask && (operation == "task" || operation == "localize")
	newUserReadFlow := facade == "read" && operation == "file" && newUserTask
	newUserBoundaryFlow := newUserExploreFlow || newUserReadFlow
	// Parse enough of an explicit new-task boundary to validate it, then apply
	// the cheap terminal gate before operation lookup, shorthand resolution,
	// overlay construction, and legacy dispatch. Non-navigation facades never
	// enter this gate.
	recoveryGeneration := uint64(0)
	if !newUserBoundaryFlow {
		var blocked *mcpgo.CallToolResult
		blocked, recoveryGeneration = terminal.interceptAnswerReady(facade, operation, req.GetArguments())
		if blocked != nil {
			s.recordFacadeTelemetry(facade, operation, facadeOutcomeBlocked, time.Since(started))
			publishLocalizationAuthReceipt(localizationAuthToken, blocked)
			return blocked, nil
		}
	}
	if facade == "analyze" && analyzeKindRequiresAdmin(operation) {
		result := blockedAnalyzeKindResult(operation)
		s.recordFacadeTelemetry("analyze", operation, facadeOutcomeBlocked, time.Since(started))
		return result, nil
	}
	if facade == "session" && (operation == "subscribe" || operation == "unsubscribe") {
		channel := normalizeFacadeOperation(req.GetString("channel", ""))
		if !validFacadeSessionChannel(channel) {
			result := NewStructuredErrorResult(StructuredError{
				ErrorCode: ErrCodeInvalidArgument,
				Message:   fmt.Sprintf("unknown session channel %q", channel),
				Data: map[string]any{
					"operation": operation, "valid_channels": facadeSessionChannels,
				},
			})
			s.recordFacadeTelemetry("session", operation, facadeOutcomeInvalidOperation, time.Since(started))
			return result, nil
		}
		operation += "_" + channel
	}
	var spec facadeOperationSpec
	var ok bool
	if facade == "analyze" {
		spec, ok = s.capabilityOperation(facade, operation)
	} else {
		spec, ok = s.facades.operation(facade, operation)
	}
	if !ok {
		valid := make([]string, 0)
		for _, candidate := range s.capabilityOperations(facade) {
			valid = append(valid, candidate.Operation)
		}
		result := NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   fmt.Sprintf("unknown %s operation %q", facade, operation),
			Data:      map[string]any{"facade": facade, "operation": operation, "valid_operations": valid},
		})
		// Never put the caller-provided operation in telemetry. All unresolved
		// values collapse to the fixed sentinel "unknown".
		s.recordFacadeTelemetry(facade, "unknown", facadeOutcomeInvalidOperation, time.Since(started))
		return result, nil
	}
	if invalid := s.validateFacadeInput(spec, input); invalid != nil {
		if completion, consumed := terminal.consumeInvalidRecovery(facade, operation, recoveryGeneration); consumed {
			invalid, _ = decorateExhaustedLocalizationReadFailure(invalid, nil, completion, spec)
		}
		s.recordFacadeTelemetry(facade, operation, facadeOutcomeInvalidArgument, time.Since(started))
		publishLocalizationAuthReceipt(localizationAuthToken, invalid)
		return invalid, nil
	}
	// Every localize call and explicit new-user task boundary starts a
	// transactional reservation. Task text and directed-read arguments never
	// imply a boundary on their own. A diagnosis or read boundary stages inactive
	// navigation and replaces the prior contract only after the first call succeeds.
	transactionalBoundaryFlow := freshLocalizeFlow || newUserBoundaryFlow
	boundaryTask := req.GetString("task", "")
	if newUserReadFlow {
		target, _ := input["target"].(map[string]any)
		boundaryTask = "read.file " + strings.TrimSpace(fmt.Sprint(target["file"]))
	}
	taskBoundaryReservation := uint64(0)
	taskBoundaryFinished := false
	if transactionalBoundaryFlow {
		var blocked *mcpgo.CallToolResult
		taskBoundaryReservation, blocked = terminal.beginLocalize(boundaryTask, newUserTask)
		if blocked != nil {
			s.recordFacadeTelemetry(facade, operation, facadeOutcomeBlocked, time.Since(started))
			publishLocalizationAuthReceipt(localizationAuthToken, blocked)
			return blocked, nil
		}
		if !freshLocalizeFlow {
			terminal.keepOpenForTask(boundaryTask)
		}
	}
	localizationReadReservation := uint64(0)
	localizationReadSucceeded := false
	defer func() {
		if localizationReadReservation != 0 {
			terminal.finishReservedReadTokenWithDigest(localizationReadReservation, localizationReadSucceeded, nil, false)
		}
		if taskBoundaryReservation != 0 && !taskBoundaryFinished {
			// Errors and panics roll back to the previous completion contract.
			terminal.finishLocalize(taskBoundaryReservation, false)
		}
	}()
	if !transactionalBoundaryFlow {
		blocked, reservation := terminal.authorizeWithToken(facade, operation, req.GetArguments())
		if blocked != nil {
			s.recordFacadeTelemetry(facade, operation, facadeOutcomeBlocked, time.Since(started))
			publishLocalizationAuthReceipt(localizationAuthToken, blocked)
			return blocked, nil
		}
		localizationReadReservation = reservation
		if reservation != 0 {
			ctx = withLocalizationPermittedEvidenceCapture(ctx, reservation)
		}
	}
	result, err := s.invokeFacadeSpec(ctx, req, spec)
	succeeded := err == nil && result != nil && !result.IsError
	if localizationReadReservation != 0 {
		localizationReadSucceeded = succeeded
		var currentEvidence []localizationDigestRow
		evidenceRecorded := false
		if succeeded {
			currentEvidence, evidenceRecorded = localizationEvidenceForPermittedCall(ctx, facade, operation, localizationReadReservation)
		}
		// Finalize before decorating so a preclassified route or bounded recovery
		// can expose its next state in this same response. Typed identities are
		// merged under the terminal-state lock only when this call transitions to
		// answer_ready. Every failed allowance is converted to a tool error carrying
		// either its one restored retry or terminal answer_ready; a Go handler error
		// must not hide the contract.
		completion := terminal.finishReservedReadTokenWithDigest(localizationReadReservation, succeeded, currentEvidence, evidenceRecorded)
		localizationReadReservation = 0
		if succeeded {
			result = decorateLocalizationReadResult(result, completion)
		} else {
			result, err = decorateExhaustedLocalizationReadFailure(result, err, completion, spec)
		}
	}
	if transactionalBoundaryFlow {
		terminal.finishLocalize(taskBoundaryReservation, succeeded)
		taskBoundaryFinished = true
	}
	publishLocalizationAuthReceipt(localizationAuthToken, result)
	return result, err
}

func publishLocalizationAuthReceipt(token string, result *mcpgo.CallToolResult) {
	if token == "" || result == nil || result.Meta == nil || result.Meta.AdditionalFields == nil {
		return
	}
	host, ok := result.Meta.AdditionalFields[localizationHostMetaKey].(localizationHostEnvelope)
	if !ok || !host.Contract.Terminal || host.Contract.Completion.State != localizationStateAnswerReady {
		return
	}
	completion := host.Contract.Completion
	evidenceIDs := make([]string, 0, localizationReplayEvidenceLimit)
	if host.Evidence != nil {
		for _, row := range host.Evidence.Evidence {
			if id := strings.TrimSpace(row.ID); id != "" {
				evidenceIDs = append(evidenceIDs, id)
			}
		}
	}
	localizationauth.Publish(token, localizationauth.Receipt{
		FinalResponse:   completion.FinalResponse,
		PrimaryIDs:      append([]string(nil), host.PrimaryIDs...),
		EvidenceIDs:     evidenceIDs,
		ContractVersion: completion.ContractVersion,
		Enforceable:     completion.Enforceable,
	})
}

type localizationPermittedEvidenceCaptureKey struct{}

type localizationPermittedEvidenceCapture struct {
	mu       sync.Mutex
	token    uint64
	recorded bool
	rows     []localizationDigestRow
}

func withLocalizationPermittedEvidenceCapture(ctx context.Context, token uint64) context.Context {
	if token == 0 {
		return ctx
	}
	return context.WithValue(ctx, localizationPermittedEvidenceCaptureKey{}, &localizationPermittedEvidenceCapture{token: token})
}

func captureLocalizationRows(ctx context.Context, rows []localizationDigestRow) {
	capture, _ := ctx.Value(localizationPermittedEvidenceCaptureKey{}).(*localizationPermittedEvidenceCapture)
	if capture == nil {
		return
	}
	capture.mu.Lock()
	capture.rows = cloneLocalizationDigestRows(rows)
	capture.recorded = true
	capture.mu.Unlock()
}

// captureLocalizationSearchSymbols records the exact typed, scoped page that
// search_symbols is about to render. The request-private context value keeps a
// concurrent or stale search from supplying identities to another reservation.
func captureLocalizationSearchSymbols(ctx context.Context, nodes []*graph.Node) {
	rows := make([]localizationDigestRow, 0, len(nodes))
	for _, node := range nodes {
		if row, ok := localizationDigestRowFromNode(node, "permitted_search_symbols"); ok {
			rows = append(rows, row)
		}
	}
	captureLocalizationRows(ctx, rows)
}

// captureLocalizationSearchText promotes only graph-backed identities from the
// typed search.text page. It reuses the exact bounded file indexes built for
// enrichment, so a permitted-evidence capture never repeats a file scan.
func (s *Server) captureLocalizationSearchText(
	ctx context.Context,
	matches []enrichedTextMatch,
	indexes map[string]*fileSymbolIndex,
) {
	capture, _ := ctx.Value(localizationPermittedEvidenceCaptureKey{}).(*localizationPermittedEvidenceCapture)
	if capture == nil {
		return
	}
	rows := make([]localizationDigestRow, 0, len(matches))
	for _, match := range matches {
		node, provenance := s.localizationTextMatchNode(ctx, match, indexes)
		if row, ok := localizationDigestRowFromNode(node, provenance); ok {
			if match.Line > 0 {
				row.Line = match.Line
			}
			rows = append(rows, row)
		}
	}
	captureLocalizationRows(ctx, rows)
}

func (s *Server) localizationTextMatchNode(
	ctx context.Context,
	match enrichedTextMatch,
	indexes map[string]*fileSymbolIndex,
) (*graph.Node, string) {
	if s == nil {
		return nil, ""
	}
	reader := s.readerFor(ctx)
	if reader == nil {
		return nil, ""
	}
	// Preserve the typed SymbolID path exactly: an already-enriched hit needs
	// one identity lookup, not another file projection.
	if id := strings.TrimSpace(match.SymbolID); id != "" {
		if node := reader.GetNode(id); node != nil && s.nodeInSessionScope(ctx, node) {
			return node, "permitted_search_text"
		}
		return nil, ""
	}
	path := strings.TrimSpace(match.Path)
	if path == "" {
		return nil, ""
	}
	index := fileSymbolIndexForPath(indexes, path)
	if index == nil || index.saturated {
		return nil, ""
	}
	if owner := index.smallestEnclosing(match.Line); owner != nil && s.nodeInSessionScope(ctx, owner) {
		return owner, "permitted_search_text_owner"
	}
	if index.fileNode != nil && s.nodeInSessionScope(ctx, index.fileNode) {
		return index.fileNode, "permitted_search_text_file"
	}
	return nil, ""
}

// captureLocalizationReadSource records the validated graph node whose source
// was successfully read. It never reconstructs identity from serialized output.
func captureLocalizationReadSource(ctx context.Context, node *graph.Node) {
	rows := make([]localizationDigestRow, 0, 1)
	if row, ok := localizationDigestRowFromNode(node, localizationProvenancePermittedReadSource); ok {
		rows = append(rows, row)
	}
	captureLocalizationRows(ctx, rows)
}

func localizationCapturedEvidence(ctx context.Context, token uint64) ([]localizationDigestRow, bool) {
	capture, _ := ctx.Value(localizationPermittedEvidenceCaptureKey{}).(*localizationPermittedEvidenceCapture)
	if capture == nil || token == 0 || capture.token != token {
		return nil, false
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if !capture.recorded {
		return nil, false
	}
	return cloneLocalizationDigestRows(capture.rows), true
}

func localizationDigestRowFromNode(node *graph.Node, provenance string) (localizationDigestRow, bool) {
	if node == nil {
		return localizationDigestRow{}, false
	}
	id := strings.TrimSpace(node.ID)
	file := strings.TrimSpace(nodeDisplayPath(node))
	if id == "" || file == "" {
		return localizationDigestRow{}, false
	}
	signature := ""
	if node.Meta != nil {
		signature, _ = node.Meta["signature"].(string)
	}
	return localizationDigestRow{
		ID:         id,
		Name:       node.Name,
		QualName:   node.QualName,
		Kind:       string(node.Kind),
		File:       file,
		Line:       node.StartLine,
		Signature:  signature,
		Provenance: provenance,
	}, true
}

func localizationEvidenceForPermittedCall(ctx context.Context, facade, operation string, token uint64) ([]localizationDigestRow, bool) {
	if token == 0 {
		return nil, false
	}
	switch facade + "." + operation {
	case "search.symbols", "search.text", "read.source":
		return localizationCapturedEvidence(ctx, token)
	default:
		return nil, false
	}
}

func decorateExhaustedLocalizationReadFailure(
	result *mcpgo.CallToolResult,
	err error,
	completion localizationCompletion,
	spec facadeOperationSpec,
) (*mcpgo.CallToolResult, error) {
	if result == nil || (err != nil && !result.IsError) {
		message := "localization read failed"
		if err != nil {
			message = err.Error()
		}
		result = mcpgo.NewToolResultError(message)
	}
	result = decorateFacadeResultIdentity(result, spec)
	return decorateLocalizationReadResult(result, completion), nil
}

func inferFacadeOperation(facade string, input map[string]any) string {
	target, _ := input["target"].(map[string]any)
	switch facade {
	case "read":
		switch {
		case facadeSelectorPresent(target["file"]):
			return "file"
		case facadeSelectorPresent(target["symbol"]):
			return "source"
		case facadeSelectorPresent(target["symbols"]):
			return "symbols"
		case facadeSelectorPresent(target["artifact"]):
			return "artifact"
		}
	case "edit":
		switch {
		case facadeSelectorPresent(input["changes"]):
			return "batch"
		case facadeSelectorPresent(target["symbol"]):
			return "symbol"
		case facadeSelectorPresent(target["file"]):
			if facadeSelectorPresent(input["content"]) && !facadeSelectorPresent(input["match"]) {
				return "write"
			}
			return "file"
		}
	}
	return ""
}

// normalizeFacadeReadOperation makes the selector cardinality authoritative.
// This accepts harmless migration aliases without forwarding an impossible
// request to a single-symbol or batch legacy handler.
func normalizeFacadeReadOperation(operation string, input map[string]any) string {
	target, _ := input["target"].(map[string]any)
	hasFile := facadeSelectorPresent(target["file"])
	hasSymbol := facadeSelectorPresent(target["symbol"])
	hasSymbols := facadeSelectorPresent(target["symbols"])
	switch operation {
	case "source":
		if hasSymbols {
			return "symbols"
		}
		if hasFile && !hasSymbol {
			return "file"
		}
	case "symbols":
		if hasSymbol && !hasSymbols {
			return "source"
		}
		if hasFile && !hasSymbols {
			return "file"
		}
	}
	return operation
}

var facadeSessionChannels = []string{
	"daemon_health", "diagnostics", "graph_invalidated", "stale_refs", "workspace_readiness",
}

func validFacadeSessionChannel(channel string) bool {
	return slices.Contains(facadeSessionChannels, channel)
}

func defaultFacadeOperation(facade string) string {
	switch facade {
	case "explore":
		return "task"
	case "search":
		return "symbols"
	case "read":
		return "source"
	case "relations":
		return "usages"
	case "trace":
		return "call_chain"
	case "analyze":
		return "help"
	case "ask":
		return "research"
	case "change":
		return "contract"
	case "edit":
		return "file"
	case "refactor":
		return "rename"
	case "review":
		return "run"
	case "publish_review":
		return "post"
	case "pr":
		return "list"
	case "recall":
		return "surface"
	case "remember":
		return "memory"
	case "workspace":
		return "info"
	case "response":
		return "stats"
	default:
		return ""
	}
}

func (s *Server) invokeFacadeSpec(ctx context.Context, req mcpgo.CallToolRequest, spec facadeOperationSpec) (result *mcpgo.CallToolResult, err error) {
	started := time.Now()
	outcome := ""
	defer func() {
		if outcome == "" {
			outcome = classifyFacadeOutcome(result, err)
		}
		s.recordFacadeTelemetry(spec.Facade, spec.Operation, outcome, time.Since(started))
	}()
	legacy, ok := s.facades.legacy(spec.Legacy)
	if !ok {
		outcome = facadeOutcomeUnavailable
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   fmt.Sprintf("%s.%s is unavailable in this server configuration", spec.Facade, spec.Operation),
			Data:      map[string]any{"facade": spec.Facade, "operation": spec.Operation, "legacy_tool": spec.Legacy},
		}), nil
	}
	normalized := normalizeFacadeArguments(spec, req.GetArguments())
	if targetErr := normalizeFacadeChangeTargets(spec, req.GetArguments(), normalized); targetErr != nil {
		outcome = facadeOutcomeInvalidArgument
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   targetErr.Error(),
			Data:      map[string]any{"facade": spec.Facade, "operation": spec.Operation},
		}), nil
	}
	if spec.Facade == "read" && (spec.Operation == "source" || spec.Operation == "symbols") {
		ids := []string{strings.TrimSpace(fmt.Sprint(normalized["id"]))}
		field := "id"
		if spec.Operation == "symbols" {
			var valid bool
			ids, valid = parseBatchSymbolIDs(normalized["ids"])
			if !valid {
				outcome = facadeOutcomeInvalidArgument
				return NewStructuredErrorResult(StructuredError{
					ErrorCode: ErrCodeInvalidArgument,
					Message:   "read.symbols requires a non-empty symbol ID array or scalar shorthand",
					Data:      map[string]any{"field": "target.symbols"},
				}), nil
			}
			field = "ids"
		}
		resolved := make([]string, 0, len(ids))
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			canonical, ambiguous := s.resolveFacadeSymbolShorthand(ctx, id)
			if len(ambiguous) > 0 {
				outcome = facadeOutcomeInvalidArgument
				return NewStructuredErrorResult(StructuredError{
					ErrorCode: ErrCodeInvalidArgument,
					Message:   fmt.Sprintf("symbol shorthand %q is ambiguous", id),
					Data:      map[string]any{"symbol": id, "candidates": ambiguous},
				}), nil
			}
			resolved = append(resolved, canonical)
		}
		if len(resolved) > 0 {
			if field == "ids" {
				encoded, _ := json.Marshal(resolved)
				normalized[field] = string(encoded)
			} else {
				normalized[field] = resolved[0]
			}
		}
	}
	if spec.Facade == "read" && spec.Operation == "editing_context" {
		if rawID, exists := normalized["id"]; exists {
			if id := strings.TrimSpace(fmt.Sprint(rawID)); id != "" {
				canonical, ambiguous := s.resolveFacadeSymbolShorthand(ctx, id)
				if len(ambiguous) > 0 {
					outcome = facadeOutcomeInvalidArgument
					return NewStructuredErrorResult(StructuredError{
						ErrorCode: ErrCodeInvalidArgument,
						Message:   fmt.Sprintf("symbol shorthand %q is ambiguous", id),
						Data:      map[string]any{"symbol": id, "candidates": ambiguous},
					}), nil
				}
				var node *graph.Node
				if s.graph != nil {
					node = s.graph.GetNode(canonical)
				}
				if node == nil || node.FilePath == "" || !s.nodeInSessionScope(ctx, node) {
					outcome = facadeOutcomeInvalidArgument
					return NewStructuredErrorResult(StructuredError{
						ErrorCode: ErrCodeSymbolNotFound,
						Message:   fmt.Sprintf("symbol %q is not indexed in this session scope", id),
						Data:      map[string]any{"symbol": id},
					}), nil
				}
				normalized["path"] = node.FilePath
				delete(normalized, "id")
			}
		}
	}
	if spec.Facade == "change" && spec.Operation == "impact" {
		if rawPath, exists := normalized["path"]; exists {
			if path := strings.TrimSpace(fmt.Sprint(rawPath)); path != "" {
				path = s.graphRelPath(path)
				eng := s.engineFor(ctx)
				ids := make([]string, 0)
				if eng != nil {
					if symbols := eng.GetFileSymbols(path); symbols != nil {
						for _, node := range symbols.Nodes {
							if node == nil || node.Kind == graph.KindFile || !exploreLocalizableKind(node.Kind) || !s.nodeInSessionScope(ctx, node) {
								continue
							}
							ids = append(ids, node.ID)
						}
					}
				}
				if len(ids) == 0 {
					outcome = facadeOutcomeInvalidArgument
					return NewStructuredErrorResult(StructuredError{
						ErrorCode: ErrCodeFileNotIndexed,
						Message:   fmt.Sprintf("no indexed symbols found for file %q", path),
						Data:      map[string]any{"file": path},
					}), nil
				}
				sort.Strings(ids)
				normalized["ids"] = strings.Join(ids, ",")
				delete(normalized, "path")
			}
		}
	}
	if spec.Facade == "analyze" && analyzeKindRequiresAdmin(normalizeFacadeOperation(fmt.Sprint(normalized["kind"]))) {
		kind := normalizeFacadeOperation(fmt.Sprint(normalized["kind"]))
		outcome = facadeOutcomeBlocked
		return blockedAnalyzeKindResult(kind), nil
	}
	if invalid := s.rejectInertFacadeSelector(spec, req.GetArguments()); invalid != nil {
		outcome = facadeOutcomeInvalidArgument
		return invalid, nil
	}
	if !facadeLegacyManagesOwnOverlay(spec.Legacy) {
		var viewErr error
		ctx, _, viewErr = s.prepareOverlayRequest(ctx)
		if viewErr != nil {
			if ctxErr := requestContextError(ctx, viewErr); ctxErr != nil {
				return nil, ctxErr
			}
			outcome = facadeOutcomeToolError
			return mcpgo.NewToolResultError(viewErr.Error()), nil
		}
	}
	forwarded := req
	forwarded.Params.Name = spec.Legacy
	forwarded.Params.Arguments = normalized
	forwarded.Params.RawArguments = nil
	result, err = legacy.handler(ctx, forwarded)
	if err == nil {
		// Book the retrieval half of the savings ledger under the LEGACY tool
		// name, so a facade call and a direct legacy call land in the same
		// per-tool bucket. Runs before decoration: the baseline is what the
		// handler actually retrieved, not the riders bolted on afterwards.
		s.recordRetrievalSavings(ctx, spec.Legacy, result)
		result = s.decorateFacadeFreshness(spec.Legacy, forwarded, result)
	}
	result = decorateFacadeResultIdentity(result, spec)
	return result, err
}

func decorateFacadeResultIdentity(result *mcpgo.CallToolResult, spec facadeOperationSpec) *mcpgo.CallToolResult {
	if result == nil {
		return nil
	}
	if result.Meta == nil {
		result.Meta = &mcpgo.Meta{}
	}
	if result.Meta.AdditionalFields == nil {
		result.Meta.AdditionalFields = make(map[string]any)
	}
	result.Meta.AdditionalFields["gortex_facade"] = map[string]any{
		"surface_version": FacadeSurfaceVersion,
		"facade":          spec.Facade,
		"operation":       spec.Operation,
		"canonical_tool":  spec.Legacy,
	}
	return result
}

func (s *Server) resolveFacadeSymbolShorthand(ctx context.Context, id string) (string, []string) {
	resolved := s.resolveSymbolID(ctx, id)
	if s.graph == nil || s.graph.GetNode(resolved) != nil || strings.Contains(id, "::") {
		return resolved, nil
	}
	eng := s.engineFor(ctx)
	if eng == nil {
		return resolved, nil
	}
	seen := make(map[string]bool)
	candidates := make([]string, 0, 2)
	for _, node := range eng.FindSymbols(id) {
		if node == nil || seen[node.ID] || !s.nodeInSessionScope(ctx, node) {
			continue
		}
		storedName := node.Name
		if parts := strings.SplitN(node.ID, "::", 2); len(parts) == 2 && parts[1] == id {
			storedName = id
		}
		if storedName != id {
			continue
		}
		seen[node.ID] = true
		candidates = append(candidates, node.ID)
	}
	sort.Strings(candidates)
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) > 1 {
		return id, candidates
	}
	return resolved, nil
}

// requestedAnalyzeKind applies the same argument-container precedence as the
// public dispatcher before choosing an operation. This closes nested bypasses
// such as options.kind=coverage while keeping the wire shape compact.
func requestedAnalyzeKind(input map[string]any) string {
	normalized := normalizeFacadeArguments(facadeOperationSpec{
		Facade: "analyze", Legacy: "analyze", Effect: facadeEffectRead,
	}, input)
	raw, ok := normalized["kind"]
	if !ok || raw == nil {
		return ""
	}
	return normalizeFacadeOperation(fmt.Sprint(raw))
}

// facadeBespokeSelectorOperations are the operations whose selectors are
// consumed by an operation-specific rule (normalizeFacadeAliases or one of the
// lowering blocks in invokeFacadeSpec) rather than by applyFacadeTarget's
// default field mapping. They opt out of the "is this selector inert?" check
// below because the field it lands in is not the one facadeTargetField names.
var facadeBespokeSelectorOperations = map[string]bool{
	"change.impact":        true, // target.symbol/target.file both become ids
	"change.edit_plan":     true, // normalizeFacadeChangeTargets lowers the set
	"change.guards":        true,
	"change.tests":         true,
	"change.contract":      true,
	"change.pattern":       true,
	"read.editing_context": true, // the symbol is resolved to its file
	"trace.flow":           true, // target/to become source_id/sink_id
	"trace.path":           true,
	"trace.taint":          true, // target/to become source_pattern/sink_pattern
}

// facadeToSelectorOperations are the only operations that read the `to`
// destination container. Everywhere else normalizeFacadeArguments lowers it to
// to_<selector> keys that no handler declares, so it is inert.
var facadeToSelectorOperations = map[string]bool{
	"trace.flow": true, "trace.path": true, "trace.taint": true,
}

// analyzeAliasedSymbolKinds are the analyze kinds that dispatch to a tool other
// than the analyze dispatcher AND whose id-shaped field really holds a symbol.
// Declaring the field is not evidence enough for these: get_communities and
// get_processes both take an `id`, but it names a community or a process. A
// symbol lowered into one resolves against the wrong entity and comes back as
// "community not found: pkg/foo.go::Bar" — a loud failure, but one that blames
// the id channel for what is really a selector-semantics mistake, and one
// capabilities never advertised as available in the first place. Refusing it
// keeps the published shape and the accepted shape the same set.
var analyzeAliasedSymbolKinds = map[string]bool{
	"co_change": true,
	"why":       true,
}

// legacyDeclaresField reports whether the named legacy tool advertises a field.
func (s *Server) legacyDeclaresField(legacy, field string) bool {
	if field == "" {
		return false
	}
	captured, available := s.facades.legacy(legacy)
	if !available {
		return false
	}
	_, declared := captured.tool.InputSchema.Properties[field]
	return declared
}

// facadeSelectorReaches reports whether a public selector actually arrives at
// the selected legacy handler as a field that handler declares and reads.
func (s *Server) facadeSelectorReaches(spec facadeOperationSpec, selector string) bool {
	if facadeBespokeSelectorOperations[spec.Facade+"."+spec.Operation] {
		return true
	}
	if spec.Facade == "analyze" && spec.Legacy != "analyze" &&
		(selector == "symbol" || selector == "symbols") && !analyzeAliasedSymbolKinds[spec.Operation] {
		return false
	}
	field := facadeTargetField(spec.Legacy, selector)
	if field == "" {
		return false
	}
	legacy, available := s.facades.legacy(spec.Legacy)
	if !available {
		return true // availability is reported separately; do not guess here
	}
	if len(legacy.tool.InputSchema.Properties) == 0 {
		// A tool that advertises no parameters is not evidence that it ignores
		// this one. Refuse only where the schema actually says so.
		return true
	}
	property, declared := legacy.tool.InputSchema.Properties[field]
	if !declared {
		return false
	}
	if spec.Legacy != "analyze" {
		return true
	}
	// The analyze dispatcher is many handlers behind one schema, so declaring
	// the field is not enough — the requested kind has to be one that reads
	// it. That per-kind answer comes from the dispatcher's own annotated
	// descriptions, the same source capabilities publishes, so the selectors
	// accepted and the selectors advertised cannot drift apart.
	return analyzeFieldApplies(spec.Operation, field, property)
}

// facadeOperationsAcceptingSelector lists the operations of a facade that do
// consume the selector, so a refusal can point at the calls that would work.
func (s *Server) facadeOperationsAcceptingSelector(facade, selector string) []string {
	operations := make([]string, 0, 4)
	for _, spec := range s.capabilityOperations(facade) {
		if spec.Operation == "help" {
			continue
		}
		if s.facadeSelectorReaches(spec, selector) {
			operations = append(operations, spec.Operation)
		}
	}
	sort.Strings(operations)
	return operations
}

// rejectInertFacadeSelector fails closed when a caller aims an operation at a
// selector that operation cannot consume. Silently dropping it is the failure
// the whole lowering path exists to prevent: a repo-wide ranking returned for
// "what is the blast radius of X" is indistinguishable from a correct answer,
// and the caller has no way to notice their target was ignored.
func (s *Server) rejectInertFacadeSelector(spec facadeOperationSpec, input map[string]any) *mcpgo.CallToolResult {
	refuse := func(container, selector string, accepted []string) *mcpgo.CallToolResult {
		data := map[string]any{
			"field":  container + "." + selector,
			"reason": "unsupported_target",
			"domain": spec.Facade, "operation": spec.Operation,
		}
		subject := fmt.Sprintf("%s.%s", spec.Facade, spec.Operation)
		if spec.Facade == "analyze" {
			data["kind"] = spec.Operation
			subject = fmt.Sprintf("analyze(kind=%s)", spec.Operation)
		}
		if len(accepted) > 0 {
			data["accepted_by"] = accepted
		}
		// Two different mistakes reach here and they deserve different words.
		// Usually the handler has no field for the selector, so it would be
		// dropped. Sometimes the field exists but names another kind of
		// entity, and lowering into it would resolve the caller's symbol
		// against a community or a process instead. Only a kind dispatching
		// to its own legacy tool can be in the second case: the analyze
		// dispatcher declares id once for every kind behind it, so a field it
		// declares says nothing about whether this kind reads it.
		field := facadeTargetField(spec.Legacy, selector)
		message := fmt.Sprintf("%s has no %s.%s to act on — it would be ignored, so the call is refused rather than answered as if it had not been sent",
			subject, container, selector)
		if spec.Legacy != "analyze" && s.legacyDeclaresField(spec.Legacy, field) {
			data["entity_mismatch"] = field
			message = fmt.Sprintf("%s selects by %s, which does not name a symbol — %s.%s would be resolved against the wrong entity, so the call is refused",
				subject, field, container, selector)
		}
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument,
			Message:   message,
			Data:      data,
		})
	}
	if to, ok := input["to"].(map[string]any); ok && len(to) > 0 {
		if !facadeToSelectorOperations[spec.Facade+"."+spec.Operation] {
			for _, selector := range sortedSelectorKeys(to) {
				return refuse("to", selector, nil)
			}
		}
	}
	target, ok := input["target"].(map[string]any)
	if !ok || len(target) == 0 {
		return nil
	}
	for _, selector := range sortedSelectorKeys(target) {
		if s.facadeSelectorReaches(spec, selector) {
			continue
		}
		return refuse("target", selector, s.facadeOperationsAcceptingSelector(spec.Facade, selector))
	}
	return nil
}

func sortedSelectorKeys(selectors map[string]any) []string {
	keys := make([]string, 0, len(selectors))
	for key, value := range selectors {
		if value == nil {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func blockedAnalyzeKindResult(kind string) *mcpgo.CallToolResult {
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeToolBlockedByMode,
		Message:   fmt.Sprintf("analyze(kind=%s) changes durable state; use workspace_admin(operation=%s)", kind, kind),
		Data:      map[string]any{"domain": "workspace_admin", "operation": kind},
	})
}

// decorateFacadeFreshness runs the existing legacy freshness policy after a
// facade operation has resolved to its canonical tool and normalized request.
// The outer facade middleware only sees compact names/targets (read,
// relations, target.file, ...), so applying the policy there would miss the
// legacy path/id fields the rider is deliberately keyed to.
func (s *Server) decorateFacadeFreshness(legacy string, req mcpgo.CallToolRequest, result *mcpgo.CallToolResult) *mcpgo.CallToolResult {
	if rider := s.freshnessRiderFor(legacy, req); rider != nil {
		return decorateResultWithFreshness(result, rider)
	}
	if isFreshnessListTool(legacy) {
		return s.decorateListResultWithFreshness(result)
	}
	return result
}

func facadeLegacyManagesOwnOverlay(name string) bool {
	if strings.HasPrefix(name, "overlay_") || strings.HasPrefix(name, "subscribe_") ||
		strings.HasPrefix(name, "unsubscribe_") || strings.HasPrefix(name, "proxy_") {
		return true
	}
	switch name {
	case "preview_edit", "simulate_chain", "compare_with_overlay", "compare_branches", "agent_registry", "set_planning_mode", "workflow":
		return true
	default:
		return false
	}
}

func facadeJSONType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return "number"
	case []any, []string, []map[string]any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unsupported value"
	}
}

func (s *Server) facadeAcceptedTopLevelFields(spec facadeOperationSpec) []string {
	capability := s.facadeCapability(spec, true)
	schema, _ := capability["input_schema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	fields := sortedFacadeMapKeys(properties)
	fieldRank := func(field string) int {
		switch field {
		case "operation":
			return 0
		case "options":
			return 2
		default:
			return 1
		}
	}
	sort.SliceStable(fields, func(i, j int) bool {
		leftRank := fieldRank(fields[i])
		rightRank := fieldRank(fields[j])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return fields[i] < fields[j]
	})
	return fields
}

func (s *Server) validateFacadeInput(spec facadeOperationSpec, input map[string]any) *mcpgo.CallToolResult {
	for _, field := range facadeContainerKeys {
		value, present := input[field]
		if !present || value == nil {
			continue
		}
		if _, ok := value.(map[string]any); !ok {
			if field == "arguments" {
				_, declared := facadeToolDefinition(spec.Facade).InputSchema.Properties[field]
				if !declared {
					receivedType := facadeJSONType(value)
					acceptedFields := s.facadeAcceptedTopLevelFields(spec)
					return NewStructuredErrorResult(StructuredError{
						ErrorCode: ErrCodeInvalidArgument,
						Message: fmt.Sprintf(
							"arguments is an unexpected top-level key for %s.%s (received %s); pass %s at the top level; arguments is the JSON-RPC envelope, not a parameter",
							spec.Facade, spec.Operation, receivedType, strings.Join(acceptedFields, "/"),
						),
						Data: map[string]any{
							"field": "arguments", "received_type": receivedType,
							"accepted_fields": acceptedFields,
							"accepted_shape":  "pass request_shape fields directly as tool arguments",
						},
					})
				}
			}
			return NewStructuredErrorResult(StructuredError{
				ErrorCode: ErrCodeInvalidArgument,
				Message:   fmt.Sprintf("%s must be an object", field),
				Data:      map[string]any{"field": field},
			})
		}
	}
	if invalid := s.validateFacadeRequestFields(spec, input); invalid != nil {
		return invalid
	}
	for _, field := range []string{"target", "to"} {
		if raw, present := input[field]; present && raw != nil {
			if invalid := validateFacadeSelector(field, raw); invalid != nil {
				return invalid
			}
		}
	}
	if spec.Facade == "search" {
		switch spec.Operation {
		case "symbols", "text", "completion":
			query := strings.TrimSpace(fmt.Sprint(input["query"]))
			if query == "" || query == "<nil>" {
				return NewStructuredErrorResult(StructuredError{
					ErrorCode: ErrCodeInvalidArgument,
					Message:   fmt.Sprintf("search.%s requires query", spec.Operation),
					Data:      map[string]any{"field": "query", "operation": spec.Operation},
				})
			}
		}
	}
	if spec.Facade == "explore" && spec.Operation == "task" {
		normalized := normalizeFacadeArguments(spec, input)
		if localize, _ := normalized["localize"].(bool); localize {
			return NewStructuredErrorResult(StructuredError{
				ErrorCode: ErrCodeInvalidArgument, Message: "explore.task does not accept localize=true",
				Data: map[string]any{"field": "localize", "operation": spec.Operation},
			})
		}
	}
	task, _ := input["task"].(string)
	if spec.Facade == "explore" && (spec.Operation == "task" || spec.Operation == "localize") && strings.TrimSpace(task) == "" {
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument, Message: fmt.Sprintf("explore.%s requires task", spec.Operation),
			Data: map[string]any{"field": "task", "operation": spec.Operation},
		})
	}
	return nil
}

// validateFacadeRequestFields refuses any top-level or container field the
// selected operation can neither advertise nor consume.
//
// The published per-operation schema closes every container, so a field outside
// it reaches no handler: normalization copies it into the legacy arguments and
// nothing reads it. Dropping it silently is not neutral — the caller stated an
// intent the server then ignored, and for a field that names a target (a
// repository, a workspace, a path) the operation proceeds against the active
// one instead. On a write that is a write to the wrong place, with a success
// result. Refusing costs a retry; accepting-and-ignoring costs correctness the
// caller cannot audit.
//
// A field the published schema omits is still accepted when it actually reaches
// the handler, which keeps every working compatibility alias alive.
func (s *Server) validateFacadeRequestFields(spec facadeOperationSpec, input map[string]any) *mcpgo.CallToolResult {
	canonicalPath := s.facadePublicRepositoryField(spec)
	return forEachFacadeRequestField(input, func(containerName, field, path string, value any) *mcpgo.CallToolResult {
		if !facadeRepositorySelectorLike(field) {
			return nil
		}
		if path == canonicalPath {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return NewStructuredErrorResult(StructuredError{
					ErrorCode: ErrCodeInvalidArgument,
					Message:   fmt.Sprintf("%s must be a non-empty string", path),
					Data: map[string]any{
						"field": path, "expected_type": "non-empty string",
					},
				})
			}
		}
		if s.facadeFieldConsumed(spec, containerName, field, value) {
			return nil
		}
		return facadeUnknownFieldResult(spec, canonicalPath, containerName, path, field)
	})
}

// forEachFacadeRequestField walks the top level and every container in a stable
// order, skipping the envelope keys the dispatcher consumes itself, and returns
// the first refusal a check produces.
func forEachFacadeRequestField(
	input map[string]any,
	check func(containerName, field, path string, value any) *mcpgo.CallToolResult,
) *mcpgo.CallToolResult {
	locations := append([]string{""}, facadeContainerKeys...)
	for _, containerName := range locations {
		fields := input
		if containerName != "" {
			var ok bool
			fields, ok = input[containerName].(map[string]any)
			if !ok {
				continue
			}
		}
		for _, field := range sortedFacadeMapKeys(fields) {
			if containerName == "" && isFacadeEnvelopeKey(field) {
				// The dispatcher consumes these itself and they never reach the
				// handler as fields, so no operation schema enumerates them.
				// Container bodies are walked by this same loop; target and to
				// are checked by validateFacadeSelector.
				continue
			}
			path := field
			if containerName != "" {
				path = containerName + "." + field
			}
			if refusal := check(containerName, field, path, fields[field]); refusal != nil {
				return refusal
			}
		}
	}
	return nil
}

// facadeUnknownFieldResult explains a refusal in the terms the caller can act
// on: where the operation does take a repository, or that it takes none, or
// which fields the container advertises.
func facadeUnknownFieldResult(
	spec facadeOperationSpec,
	canonicalPath, containerName, path, field string,
) *mcpgo.CallToolResult {
	data := map[string]any{"field": path}
	if containerName != "" {
		data["container"] = containerName
	}
	message := fmt.Sprintf("unknown field %q", path)
	switch {
	case canonicalPath != "":
		data["suggested_field"] = canonicalPath
		message += fmt.Sprintf("; use %s to select a repository", canonicalPath)
	default:
		// Refusing without saying why leaves the caller retrying spellings of a
		// selector this operation will never accept.
		data["reason"] = "no_repository_selector"
		message += fmt.Sprintf("; %s.%s accepts no repository selector and runs against the active project"+
			" — switch it with workspace_admin.set_active_project", spec.Facade, spec.Operation)
	}
	return NewStructuredErrorResult(StructuredError{
		ErrorCode: ErrCodeInvalidArgument,
		Message:   message,
		Data:      data,
	})
}

// facadeTranslatesEditAliases reports whether the facade publishes the friendly
// match/replacement pair that normalizeFacadeArguments lowers into the legacy
// old_string/new_string vocabulary. TestFacadeEditAliasTranslationMatchesPublishedVocabulary
// pins this to the facade definitions so the two cannot drift.
func facadeTranslatesEditAliases(facade string) bool {
	return facade == "edit"
}

// isFacadeEnvelopeKey reports whether a top-level key belongs to the request
// envelope rather than to the operation's arguments. normalizeFacadeArguments
// drops exactly these before merging, so the two must agree.
func isFacadeEnvelopeKey(key string) bool {
	switch key {
	case "operation", "arguments", "options", "source", "context", "guard", "output", "target", "to":
		return true
	default:
		return false
	}
}

// facadeFieldConsumed reports whether the field, lowered through the same
// normalization the dispatcher applies, reaches the selected handler as a field
// that handler declares. The probe is diffed against an operation-only baseline
// so a key the normalizer injects unconditionally cannot make every field look
// consumed, and fixed arguments do not count: the caller's value is discarded.
func (s *Server) facadeFieldConsumed(spec facadeOperationSpec, containerName, field string, value any) bool {
	baseline := normalizeFacadeArguments(spec, map[string]any{"operation": spec.Operation})
	probe := map[string]any{"operation": spec.Operation}
	if containerName == "" {
		probe[field] = value
	} else {
		probe[containerName] = map[string]any{field: value}
	}
	for lowered, candidateValue := range normalizeFacadeArguments(spec, probe) {
		if _, fixed := spec.Fixed[lowered]; fixed || !s.legacyDeclaresField(spec.Legacy, lowered) {
			continue
		}
		baselineValue, existed := baseline[lowered]
		if !existed || !reflect.DeepEqual(baselineValue, candidateValue) {
			return true
		}
	}
	return false
}

// facadePublicRepositoryField reports where the operation's published schema
// exposes its repository selector, as a dotted path, or "" when it has none.
func (s *Server) facadePublicRepositoryField(spec facadeOperationSpec) string {
	key := spec.Facade + "." + spec.Operation
	if cached, ok := s.facades.cachedRepositoryField(key); ok {
		return cached
	}
	field := s.resolveFacadePublicRepositoryField(spec)
	s.facades.cacheRepositoryField(key, field)
	return field
}

func (s *Server) resolveFacadePublicRepositoryField(spec facadeOperationSpec) string {
	capability := s.facadeCapability(spec, true)
	schema, _ := capability["input_schema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["repo"]; ok {
		return "repo"
	}
	for _, container := range facadeContainerKeys {
		containerSchema, _ := properties[container].(map[string]any)
		containerProperties, _ := containerSchema["properties"].(map[string]any)
		if _, ok := containerProperties["repo"]; ok {
			return container + ".repo"
		}
	}
	return ""
}

// facadeRepositorySelectorLike reports whether a field name states WHERE an
// operation should act — the repository, workspace, project, or path it should
// target.
//
// This is the class where dropping a field silently is not merely wasteful but
// wrong: the operation proceeds against the active target while the caller
// believes it addressed the one they named, and on a write that is a write to
// the wrong place reported as success. Every name here is checked against the
// consumption probe, so an operation that genuinely reads one still gets it;
// only the spellings that reach no reader are refused.
//
// The list is deliberately wider than the vocabulary the surface publishes.
// A caller who invents `repo_root` or `cwd` has stated the same intent as one
// who writes `repo`, and answering about a different target is equally wrong.
//
// It stops at names that unambiguously address a repository or workspace.
// `path` and `scope` are excluded on purpose: across this surface they far more
// often mean a file path or a working-tree scope than a repository, so treating
// them as target selectors would refuse working calls to buy nothing.
func facadeRepositorySelectorLike(field string) bool {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "repo", "repo_path", "repository", "repository_path",
		"repo_root", "repository_root", "repopath", "repo-path", "repo_dir",
		"root", "cwd", "dir", "worktree", "work_tree", "base_repo",
		"workspace", "project":
		return true
	default:
		return false
	}
}

func validateFacadeSelector(field string, raw any) *mcpgo.CallToolResult {
	target, ok := raw.(map[string]any)
	if !ok {
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument, Message: field + " must be an object",
			Data: map[string]any{"field": field},
		})
	}
	allowed := map[string]bool{"file": true, "symbol": true, "symbols": true, "query": true, "artifact": true, "repo": true}
	selectors := make([]string, 0, len(target))
	for key, value := range target {
		if !allowed[key] {
			return NewStructuredErrorResult(StructuredError{
				ErrorCode: ErrCodeInvalidArgument, Message: fmt.Sprintf("unknown %s selector %q", field, key),
				Data: map[string]any{"field": field, "valid_selectors": []string{"file", "symbol", "symbols", "query", "artifact", "repo"}},
			})
		}
		if facadeSelectorPresent(value) {
			selectors = append(selectors, key)
		}
	}
	if len(selectors) != 1 {
		sort.Strings(selectors)
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument, Message: field + " must contain exactly one selector",
			Data: map[string]any{"field": field, "selectors": selectors},
		})
	}
	return nil
}

func facadeSelectorPresent(value any) bool {
	if value == nil {
		return false
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case []string:
		return len(typed) > 0
	default:
		return fmt.Sprint(value) != ""
	}
}

const (
	facadeOutcomeSuccess          = "success"
	facadeOutcomeInvalidOperation = "invalid_operation"
	facadeOutcomeInvalidArgument  = "invalid_argument"
	facadeOutcomeBlocked          = "blocked"
	facadeOutcomeUnavailable      = "unavailable"
	facadeOutcomeToolError        = "tool_error"
	facadeOutcomeHandlerError     = "handler_error"
	facadeOutcomeEmptyResult      = "empty_result"
)

// boundedFacadeTelemetryDimension joins fixed, low-cardinality tokens and
// deterministically folds long combinations under telemetry's 32-byte guard.
// Callers must pass registry values or fixed sentinels, never request values.
func boundedFacadeTelemetryDimension(parts ...string) string {
	dim := strings.Join(parts, ".")
	if len(dim) <= 32 {
		return dim
	}
	sum := sha256.Sum256([]byte(dim))
	return dim[:23] + "." + hex.EncodeToString(sum[:4])
}

func classifyFacadeOutcome(result *mcpgo.CallToolResult, err error) string {
	if err != nil {
		return facadeOutcomeHandlerError
	}
	if result == nil {
		return facadeOutcomeEmptyResult
	}
	if !result.IsError {
		return facadeOutcomeSuccess
	}
	body, ok := singleTextContent(result)
	if !ok {
		return facadeOutcomeToolError
	}
	var structured struct {
		ErrorCode ErrorCode `json:"error_code"`
	}
	if json.Unmarshal([]byte(body), &structured) != nil {
		return facadeOutcomeToolError
	}
	switch structured.ErrorCode {
	case ErrCodeInvalidArgument:
		return facadeOutcomeInvalidArgument
	case ErrCodeToolBlockedByMode, ErrCodeToolOutOfPhase:
		return facadeOutcomeBlocked
	case ErrCodeWorkspaceUnknown, ErrCodeProjectUnknown, ErrCodeRepoNotTracked, ErrCodeRouteUnresolved:
		return facadeOutcomeUnavailable
	default:
		return facadeOutcomeToolError
	}
}

func validFacadeOutcome(outcome string) string {
	switch outcome {
	case facadeOutcomeSuccess, facadeOutcomeInvalidOperation, facadeOutcomeInvalidArgument,
		facadeOutcomeBlocked, facadeOutcomeUnavailable, facadeOutcomeToolError,
		facadeOutcomeHandlerError, facadeOutcomeEmptyResult:
		return outcome
	default:
		return facadeOutcomeToolError
	}
}

// facadeTelemetryIdentity admits only registry-backed operations and four
// fixed capabilities buckets. This is the privacy boundary that prevents a
// caller-provided operation/domain from becoming even a hashed dimension.
func (s *Server) facadeTelemetryIdentity(facade, operation string) (string, string) {
	if !isFacadeToolName(facade) {
		return "unknown", "unknown"
	}
	if facade == "capabilities" {
		switch operation {
		case "list", "domain", "operation", "unknown":
			return facade, operation
		default:
			return facade, "unknown"
		}
	}
	if operation == "unknown" {
		return facade, operation
	}
	if _, ok := s.facades.operation(facade, operation); ok {
		return facade, operation
	}
	if facade == "analyze" && AnalyzeKindDescription(operation) != "" {
		return facade, operation
	}
	if facade == "session" && (operation == "subscribe" || operation == "unsubscribe") {
		return facade, operation
	}
	// Admin-only analyze kinds are rejected before capability dispatch, but
	// remain a fixed low-cardinality vocabulary worth measuring directly.
	if facade == "analyze" && analyzeKindRequiresAdmin(operation) {
		return facade, operation
	}
	return facade, "unknown"
}

func (s *Server) recordFacadeTelemetry(facade, operation, outcome string, elapsed time.Duration) {
	facade, operation = s.facadeTelemetryIdentity(facade, operation)
	outcome = validFacadeOutcome(outcome)
	status := "error"
	if outcome == facadeOutcomeSuccess {
		status = "ok"
	}
	s.recorder.Record("mcp_facade_call", boundedFacadeTelemetryDimension(facade, operation))
	s.recorder.Record("mcp_facade_status", boundedFacadeTelemetryDimension(facade, operation, status))
	s.recorder.Record("mcp_facade_outcome", boundedFacadeTelemetryDimension(facade, operation, outcome))
	s.recorder.Record("mcp_facade_latency", boundedFacadeTelemetryDimension(facade, operation, telemetry.BucketDuration(elapsed)))
	if outcome == facadeOutcomeInvalidOperation || outcome == facadeOutcomeInvalidArgument {
		s.recorder.Record("mcp_facade_invalid", boundedFacadeTelemetryDimension(facade, operation, string(ErrCodeInvalidArgument)))
	}
}

func normalizeFacadeArguments(spec facadeOperationSpec, input map[string]any) map[string]any {
	out := make(map[string]any)
	mergeFacadeObject(out, input["arguments"])
	mergeFacadeObject(out, input["options"])
	mergeFacadeObject(out, input["source"])
	mergeFacadeObject(out, input["context"])
	mergeFacadeObject(out, input["guard"])
	mergeFacadeObject(out, input["output"])
	for key, value := range input {
		if isFacadeEnvelopeKey(key) {
			continue
		}
		out[key] = value
	}
	if target, ok := input["target"].(map[string]any); ok {
		applyFacadeTarget(spec.Legacy, out, target)
	}
	if to, ok := input["to"].(map[string]any); ok {
		for key, value := range to {
			out["to_"+key] = value
		}
	}
	// Friendly edit aliases become the exact legacy vocabulary. Only the facade
	// that publishes them may translate: elsewhere `match` and `replacement` are
	// a handler's own vocabulary, and rewriting them drops the caller's value
	// into a field nobody reads.
	if facadeTranslatesEditAliases(spec.Facade) {
		if match, ok := out["match"]; ok {
			if spec.Legacy == "edit_symbol" {
				out["old_source"] = match
			} else {
				out["old_string"] = match
			}
			delete(out, "match")
		}
		if replacement, ok := out["replacement"]; ok {
			if spec.Legacy == "edit_symbol" {
				out["new_source"] = replacement
			} else {
				out["new_string"] = replacement
			}
			delete(out, "replacement")
		}
	}
	normalizeFacadeAliases(spec, input, out)
	for key, value := range spec.Fixed {
		out[key] = value
	}
	normalizeFacadeReadFileRange(spec, out)
	return out
}

func normalizeFacadeReadFileRange(spec facadeOperationSpec, out map[string]any) {
	if spec.Legacy != "read_file" {
		return
	}
	line := func(primary, alias string) (int, bool) {
		raw, ok := out[primary]
		if !ok && alias != "" {
			raw, ok = out[alias]
		}
		if !ok {
			return 0, false
		}
		switch value := raw.(type) {
		case int:
			return value, true
		case int32:
			return int(value), true
		case int64:
			return int(value), true
		case float32:
			return int(value), true
		case float64:
			return int(value), true
		default:
			return 0, false
		}
	}

	start, hasStart := line("start_line", "start")
	end, hasEnd := line("end_line", "end")
	if !hasStart && !hasEnd {
		return
	}
	if !hasStart || start < 1 {
		start = 1
	}
	out["offset"] = start
	if hasEnd {
		if end < start {
			end = start
		}
		out["limit"] = end - start + 1
	}
	delete(out, "start_line")
	delete(out, "end_line")
	delete(out, "start")
	delete(out, "end")
}

func normalizeFacadeAliases(spec facadeOperationSpec, input, out map[string]any) {
	alias := func(from, to string) {
		if value, ok := out[from]; ok {
			out[to] = value
			if from != to {
				delete(out, from)
			}
		}
	}
	jsonString := func(key string) {
		value, ok := out[key]
		if !ok {
			return
		}
		if _, already := value.(string); already {
			return
		}
		if raw, err := json.Marshal(value); err == nil {
			out[key] = string(raw)
		}
	}
	commaString := func(from, to string) {
		value, ok := out[from]
		if !ok {
			return
		}
		switch values := value.(type) {
		case []any:
			parts := make([]string, 0, len(values))
			for _, item := range values {
				parts = append(parts, fmt.Sprint(item))
			}
			out[to] = strings.Join(parts, ",")
		case []string:
			out[to] = strings.Join(values, ",")
		default:
			out[to] = value
		}
		if from != to {
			delete(out, from)
		}
	}
	flattenRange := func() {
		raw, ok := out["range"]
		if !ok {
			return
		}
		if fields, ok := raw.(map[string]any); ok {
			for _, key := range []string{"start_line", "start_char", "end_line", "end_char"} {
				if value, exists := fields[key]; exists {
					out[key] = value
				}
			}
		}
		delete(out, "range")
	}
	// Explore's public path is a repository-selection anchor, not a legacy
	// retrieval field. Lower it to repo so a caller working outside the active
	// repository is either scoped to the containing tracked repo or receives an
	// explicit scope error. A non-empty path wins over options.repo: silently
	// ignoring an explicit filesystem anchor would be less safe than rejecting
	// an untracked path.
	if spec.Facade == "explore" {
		if path, exists := out["path"]; exists {
			if strings.TrimSpace(fmt.Sprint(path)) != "" {
				out["repo"] = path
			}
			delete(out, "path")
		}
	}
	switch spec.Facade + "." + spec.Operation {
	case "read.file":
		normalizeFacadeReadWindow(out)
	case "read.symbols":
		if _, explicit := out["include_source"]; !explicit {
			out["include_source"] = true
		}
	case "search.ast":
		alias("query", "pattern")
	case "search.winnow":
		alias("query", "text_match")
	case "relations.declaration":
		alias("query", "use_site")
	case "edit.batch":
		alias("changes", "edits")
	case "refactor.move":
		alias("destination", "target_file")
	case "change.impact":
		// Compatibility source fields are lowered first. An explicit target
		// is the canonical selector and therefore wins deterministically when
		// a caller supplies both forms during migration.
		commaString("symbols", "ids")
		if symbol := facadeSelector(input["target"], "symbol"); symbol != nil {
			out["ids"] = symbol
		}
		if symbols := facadeSelector(input["target"], "symbols"); symbols != nil {
			out["symbols"] = symbols
			commaString("symbols", "ids")
		}
		delete(out, "id")
	case "change.edit_plan", "change.guards", "change.tests":
		commaString("symbols", "ids")
	case "change.pattern":
		// suggest_pattern accepts one anchor. Preserve an explicit id; when the
		// public source carries a one-element symbols list, lower its first item.
		if _, exists := out["id"]; !exists {
			switch values := out["symbols"].(type) {
			case []any:
				if len(values) > 0 {
					out["id"] = fmt.Sprint(values[0])
				}
			case []string:
				if len(values) > 0 {
					out["id"] = values[0]
				}
			case string:
				out["id"] = values
			}
		}
		delete(out, "symbols")
	case "change.verify":
		jsonString("changes")
	case "change.diagnostics", "change.code_actions":
		alias("file", "path")
		flattenRange()
	case "change.ranges":
		alias("file", "path")
		flattenRange()
		jsonString("ranges")
	case "change.preview":
		jsonString("workspace_edit")
	case "change.simulate":
		jsonString("steps")
	case "change.contract":
		commaString("symbols", "symbols")
		jsonString("ranges")
		jsonString("workspace_edit")
	case "trace.flow", "trace.path":
		if source := facadeSelector(input["target"], "symbol", "query"); source != nil {
			out["source_id"] = source
		}
		if sink := facadeSelector(input["to"], "symbol", "query"); sink != nil {
			out["sink_id"] = sink
		}
		delete(out, "id")
	case "trace.taint":
		if source := facadeSelector(input["target"], "query", "symbol"); source != nil {
			out["source_pattern"] = source
		}
		if sink := facadeSelector(input["to"], "query", "symbol"); sink != nil {
			out["sink_pattern"] = sink
		}
		delete(out, "id")
	}
	// Capability/schema probes use this same lowering path as live dispatch.
	// Invalid selector combinations are rejected by invokeFacadeSpec; probes
	// deliberately ignore the error so they can still discover captured fields.
	_ = normalizeFacadeChangeTargets(spec, input, out)
}

// normalizeFacadeChangeTargets lowers every supported symbol-selector shape to
// the one legacy field consumed by the selected change operation. The same
// function is used during capability probing and live dispatch so schemas cannot
// advertise a selector that handlers interpret differently.
func normalizeFacadeChangeTargets(spec facadeOperationSpec, input, out map[string]any) error {
	if spec.Facade != "change" {
		return nil
	}
	switch spec.Operation {
	case "edit_plan", "guards", "tests", "contract":
	default:
		return nil
	}

	type selection struct {
		label string
		ids   []string
	}
	selections := make([]selection, 0, 4)
	collect := func(container string, raw any) error {
		fields, ok := raw.(map[string]any)
		if !ok || fields == nil {
			return nil
		}
		for _, field := range []string{"symbol", "symbols", "id", "ids"} {
			value, present := fields[field]
			if !present {
				continue
			}
			ids, err := facadeChangeTargetIDs(value, field == "symbol" || field == "id")
			if err != nil {
				return fmt.Errorf("change.%s %s.%s: %w", spec.Operation, container, field, err)
			}
			selections = append(selections, selection{label: container + "." + field, ids: ids})
		}
		return nil
	}

	// Canonical target selectors lead so equivalent compatibility forms retain
	// target order. Different selectors must name the same set or the request is
	// ambiguous; silently choosing one is unsafe for change analysis.
	if err := collect("target", input["target"]); err != nil {
		return err
	}
	for _, container := range []string{"source", "options", "arguments"} {
		if err := collect(container, input[container]); err != nil {
			return err
		}
	}
	top := make(map[string]any, 4)
	for _, field := range []string{"symbol", "symbols", "id", "ids"} {
		if value, present := input[field]; present {
			top[field] = value
		}
	}
	if err := collect("request", top); err != nil {
		return err
	}

	var ids []string
	if len(selections) > 0 {
		ids = selections[0].ids
		for _, candidate := range selections[1:] {
			if !sameFacadeChangeTargetSet(ids, candidate.ids) {
				return fmt.Errorf("change.%s received conflicting symbol selectors %s and %s",
					spec.Operation, selections[0].label, candidate.label)
			}
		}
	}

	if spec.Operation == "contract" {
		source := strings.ToLower(strings.TrimSpace(fmt.Sprint(out["source"])))
		if source == "<nil>" {
			source = ""
		}
		if len(ids) > 0 {
			if source != "" && source != "auto" && source != "symbols" {
				return fmt.Errorf("change.contract symbol targets conflict with source=%s", source)
			}
			out["source"] = "symbols"
			out["symbols"] = strings.Join(ids, ",")
			delete(out, "id")
			delete(out, "ids")
			return nil
		}
		if source == "" || source == "auto" || source == "symbols" {
			return fmt.Errorf("change.contract requires target.symbol/target.symbols or an explicit non-symbol source")
		}
		return nil
	}

	if len(ids) == 0 {
		return fmt.Errorf("change.%s requires target.symbol, target.symbols, or ids", spec.Operation)
	}
	out["ids"] = strings.Join(ids, ",")
	delete(out, "id")
	delete(out, "symbols")
	return nil
}

func facadeChangeTargetIDs(raw any, singular bool) ([]string, error) {
	var values []string
	switch value := raw.(type) {
	case string:
		if singular && strings.Contains(value, ",") {
			return nil, fmt.Errorf("singular selector contains multiple IDs")
		}
		values = strings.Split(value, ",")
	case []string:
		values = append(values, value...)
	case []any:
		values = make([]string, 0, len(value))
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("IDs must be strings")
			}
			values = append(values, text)
		}
	default:
		return nil, fmt.Errorf("expected a string or string array")
	}
	if singular && len(values) != 1 {
		return nil, fmt.Errorf("singular selector requires exactly one ID")
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("selector must not be empty")
	}

	ids := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := strings.TrimSpace(value)
		if id == "" {
			return nil, fmt.Errorf("selector contains an empty ID")
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("selector must not be empty")
	}
	return ids, nil
}

func sameFacadeChangeTargetSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	return slices.Equal(left, right)
}

func normalizeFacadeReadWindow(out map[string]any) {
	if window, ok := out["window"].(map[string]any); ok {
		for _, key := range []string{"offset", "limit", "line"} {
			if _, exists := out[key]; !exists {
				if value, present := window[key]; present {
					out[key] = value
				}
			}
		}
	}
	delete(out, "window")
	if line, ok := facadePositiveInt(out["line"]); ok {
		if _, exists := out["offset"]; !exists {
			out["offset"] = line
		}
		if _, exists := out["limit"]; !exists {
			out["limit"] = 1
		}
	}
	delete(out, "line")
}

func facadePositiveInt(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, value > 0
	case int32:
		return int(value), value > 0
	case int64:
		return int(value), value > 0
	case float32:
		integer := int(value)
		return integer, value > 0 && float32(integer) == value
	case float64:
		integer := int(value)
		return integer, value > 0 && float64(integer) == value
	case json.Number:
		integer, err := value.Int64()
		return int(integer), err == nil && integer > 0
	default:
		return 0, false
	}
}

func facadeSelector(raw any, keys ...string) any {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range keys {
		if value, exists := obj[key]; exists && value != nil && fmt.Sprint(value) != "" {
			return value
		}
	}
	return nil
}

func mergeFacadeObject(dst map[string]any, raw any) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return
	}
	for key, value := range obj {
		dst[key] = value
	}
}

// facadeTargetField is the single decision of which legacy field a public
// target selector lowers into. Both the lowering below and the "would this
// selector reach the handler at all?" check read it, so a selector can never
// be accepted by one and dropped by the other. An empty result means the
// selector has no legacy field on this tool.
func facadeTargetField(legacy, selector string) string {
	switch selector {
	case "file":
		if legacy == "find_co_changing_symbols" {
			return "file_path"
		}
		return "path"
	case "symbol":
		switch legacy {
		case "check_references", "find_co_changing_symbols":
			return "symbol_id"
		case "find_import_path":
			return "name"
		}
		return "id"
	case "symbols":
		return "ids"
	case "query":
		return "query"
	case "artifact":
		return "id"
	case "repo":
		return "repo"
	default:
		return ""
	}
}

func applyFacadeTarget(legacy string, out, target map[string]any) {
	set := func(key string, value any) {
		if value != nil {
			out[key] = value
		}
	}
	if file := target["file"]; file != nil {
		set(facadeTargetField(legacy, "file"), file)
	}
	if symbol := target["symbol"]; symbol != nil {
		set(facadeTargetField(legacy, "symbol"), symbol)
	}
	if symbols := target["symbols"]; symbols != nil {
		if values, ok := symbols.([]any); ok {
			parts := make([]string, 0, len(values))
			for _, value := range values {
				parts = append(parts, fmt.Sprint(value))
			}
			if encoded, err := json.Marshal(parts); err == nil {
				set(facadeTargetField(legacy, "symbols"), string(encoded))
			}
		} else if values, ok := symbols.([]string); ok {
			if encoded, err := json.Marshal(values); err == nil {
				set(facadeTargetField(legacy, "symbols"), string(encoded))
			}
		} else {
			set(facadeTargetField(legacy, "symbols"), symbols)
		}
	}
	if query := target["query"]; query != nil {
		set(facadeTargetField(legacy, "query"), query)
	}
	if artifact := target["artifact"]; artifact != nil {
		set(facadeTargetField(legacy, "artifact"), artifact)
	}
	if repo := target["repo"]; repo != nil {
		set(facadeTargetField(legacy, "repo"), repo)
	}
}

func (s *Server) handleCapabilities(_ context.Context, req mcpgo.CallToolRequest) (result *mcpgo.CallToolResult, err error) {
	started := time.Now()
	telemetryOperation := "list"
	outcome := ""
	defer func() {
		if outcome == "" {
			outcome = classifyFacadeOutcome(result, err)
		}
		s.recordFacadeTelemetry("capabilities", telemetryOperation, outcome, time.Since(started))
	}()
	domain := normalizeFacadeOperation(req.GetString("domain", ""))
	operation := normalizeFacadeOperation(req.GetString("operation", ""))
	detail := normalizeFacadeOperation(req.GetString("detail", "summary"))
	if domain == "" {
		domains := make([]map[string]any, 0, len(facadeToolNames()))
		for _, name := range facadeToolNames() {
			domains = append(domains, map[string]any{
				"name": name, "description": facadeDescriptions[name], "operations": len(s.capabilityOperations(name)),
			})
		}
		return mcpgo.NewToolResultJSON(map[string]any{
			"surface_version": FacadeSurfaceVersion, "domains": domains,
		})
	}
	telemetryOperation = "domain"
	if !isFacadeToolName(domain) {
		telemetryOperation = "unknown"
		outcome = facadeOutcomeInvalidOperation
		return NewStructuredErrorResult(StructuredError{
			ErrorCode: ErrCodeInvalidArgument, Message: fmt.Sprintf("unknown tool domain %q", domain),
			Data: map[string]any{"valid_domains": facadeToolNames()},
		}), nil
	}
	if operation != "" {
		telemetryOperation = "operation"
		spec, ok := s.capabilityOperation(domain, operation)
		if !ok {
			telemetryOperation = "unknown"
			outcome = facadeOutcomeInvalidOperation
			return NewStructuredErrorResult(StructuredError{
				ErrorCode: ErrCodeInvalidArgument, Message: fmt.Sprintf("unknown %s operation %q", domain, operation),
			}), nil
		}
		return mcpgo.NewToolResultJSON(s.facadeCapability(spec, detail == "schema"))
	}
	ops := make([]map[string]any, 0)
	for _, spec := range s.capabilityOperations(domain) {
		ops = append(ops, s.facadeCapability(spec, detail == "schema"))
	}
	return mcpgo.NewToolResultJSON(map[string]any{
		"surface_version": FacadeSurfaceVersion, "domain": domain, "operations": ops,
	})
}

// capabilityOperation includes the native analyze(kind=...) catalogue without
// duplicating every kind in the legacy-to-public migration registry. Mutating
// dispatcher kinds are available only through workspace_admin.
func (s *Server) capabilityOperation(domain, operation string) (facadeOperationSpec, bool) {
	if domain == "session" {
		switch operation {
		case "subscribe":
			_, available := s.facades.legacy("subscribe_diagnostics")
			return facadeOperationSpec{Facade: "session", Operation: operation, Legacy: "subscribe_diagnostics", Effect: facadeEffectSessionWrite}, available
		case "unsubscribe":
			_, available := s.facades.legacy("unsubscribe_diagnostics")
			return facadeOperationSpec{Facade: "session", Operation: operation, Legacy: "unsubscribe_diagnostics", Effect: facadeEffectSessionWrite}, available
		}
		if strings.HasPrefix(operation, "subscribe_") || strings.HasPrefix(operation, "unsubscribe_") {
			return facadeOperationSpec{}, false
		}
	}
	if spec, ok := s.facades.operation(domain, operation); ok {
		if _, available := s.facades.legacy(spec.Legacy); available {
			return spec, true
		}
	}
	if domain == "analyze" && !analyzeKindRequiresAdmin(operation) && AnalyzeKindDescription(operation) != "" {
		if _, available := s.facades.legacy("analyze"); available {
			return facadeOperationSpec{
				Facade: "analyze", Operation: operation, Legacy: "analyze", Effect: facadeEffectRead,
				Fixed: publicAnalyzeFixedArguments(operation),
			}, true
		}
	}
	return facadeOperationSpec{}, false
}

func (s *Server) capabilityOperations(domain string) []facadeOperationSpec {
	ops := s.facades.availableOperations(domain)
	if domain == "session" {
		public := make([]facadeOperationSpec, 0, len(ops)+2)
		for _, spec := range ops {
			if strings.HasPrefix(spec.Operation, "subscribe_") || strings.HasPrefix(spec.Operation, "unsubscribe_") {
				continue
			}
			public = append(public, spec)
		}
		public = append(public,
			facadeOperationSpec{Facade: "session", Operation: "subscribe", Legacy: "subscribe_diagnostics", Effect: facadeEffectSessionWrite},
			facadeOperationSpec{Facade: "session", Operation: "unsubscribe", Legacy: "unsubscribe_diagnostics", Effect: facadeEffectSessionWrite},
		)
		sort.Slice(public, func(i, j int) bool { return public[i].Operation < public[j].Operation })
		return public
	}
	if domain != "analyze" {
		return ops
	}
	seen := make(map[string]bool, len(ops))
	for _, spec := range ops {
		seen[spec.Operation] = true
	}
	for _, kind := range AnalyzeKinds() {
		if analyzeKindRequiresAdmin(kind) || seen[kind] {
			continue
		}
		ops = append(ops, facadeOperationSpec{
			Facade: "analyze", Operation: kind, Legacy: "analyze", Effect: facadeEffectRead,
			Fixed: publicAnalyzeFixedArguments(kind),
		})
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].Operation < ops[j].Operation })
	return ops
}

// publicAnalyzeFixedArguments keeps the read-only analyze boundary free of
// optional external effects. Explicit legacy calls retain their historical
// behavior.
func publicAnalyzeFixedArguments(kind string) map[string]any {
	fixed := map[string]any{"kind": kind}
	switch kind {
	case "concepts":
		fixed["use_llm"] = false
	case "impact":
		fixed["refresh_cochange"] = false
	case "sql_call_sites":
		fixed["materialize"] = false
	}
	return fixed
}

func (s *Server) facadeCapability(spec facadeOperationSpec, includeSchema bool) map[string]any {
	legacy, available := s.facades.legacy(spec.Legacy)
	out := map[string]any{
		"surface_version": FacadeSurfaceVersion, "domain": spec.Facade, "operation": spec.Operation,
		"effect": spec.Effect, "available": available,
	}
	if len(spec.Fixed) > 0 {
		out["fixed_arguments"] = spec.Fixed
	}
	if available {
		if spec.Facade == "explore" && spec.Operation == "localize" {
			out["summary"] = "Locate files and symbols, then stop navigation and answer from the returned evidence. Set options.new_user_task=true only on the first explore call (task or localize) caused by a new user request; never use it to retry or continue the current request."
		} else if spec.Facade == "explore" && spec.Operation == "task" {
			out["summary"] = "Gather a nonterminal neighborhood for diagnosis or implementation that will continue."
		} else if spec.Facade == "analyze" && spec.Operation == "help" {
			out["summary"] = "List supported analysis kinds."
		} else if summary := AnalyzeKindDescription(spec.Operation); spec.Legacy == "analyze" && summary != "" {
			out["summary"] = summary
		} else {
			out["summary"] = firstSentence(legacy.tool.Description)
		}
		if includeSchema {
			inputSchema := any(legacy.tool.InputSchema)
			properties := legacy.tool.InputSchema.Properties
			if spec.Facade == "read" && spec.Operation == "symbols" {
				properties = cloneFacadeSchemaMap(properties)
				if includeSource, ok := properties["include_source"].(map[string]any); ok {
					includeSource["default"] = true
					includeSource["description"] = "Include source code for each symbol (default: true; pass false for metadata only)."
				}
			}
			required := legacy.tool.InputSchema.Required
			if spec.Facade == "analyze" || (spec.Facade == "workspace_admin" && spec.Legacy == "analyze") {
				inputSchema, properties, required = analyzeFacadeCapabilitySchema(spec, properties, required)
			} else if spec.Facade == "session" && (spec.Operation == "subscribe" || spec.Operation == "unsubscribe") {
				inputSchema = map[string]any{
					"type": "object",
					"properties": map[string]any{
						"channel":   map[string]any{"type": "string", "enum": facadeSessionChannels},
						"arguments": map[string]any{"type": "object", "additionalProperties": true},
					},
					"required": []string{"channel"},
				}
				properties = map[string]any{"channel": map[string]any{"type": "string"}}
				required = []string{"channel"}
			}
			requestShape := facadeRequestShape(spec, properties, required)
			if spec.Facade != "analyze" && spec.Facade != "session" && (spec.Facade != "workspace_admin" || spec.Legacy != "analyze") {
				inputSchema = facadePublicCapabilitySchema(spec, properties, required, requestShape)
			}
			if spec.Facade == "read" && spec.Operation == "symbols" {
				if schema, ok := inputSchema.(map[string]any); ok {
					schemaProperties, _ := schema["properties"].(map[string]any)
					options, _ := schemaProperties["options"].(map[string]any)
					optionProperties, _ := options["properties"].(map[string]any)
					if optionProperties == nil {
						optionProperties = make(map[string]any)
						options["properties"] = optionProperties
					}
					includeSource, _ := optionProperties["include_source"].(map[string]any)
					if includeSource == nil {
						includeSource = map[string]any{"type": "boolean"}
						optionProperties["include_source"] = includeSource
					}
					includeSource["default"] = true
					includeSource["description"] = "Include source code for each symbol (default: true; pass false for metadata only)."
				}
			}
			out["input_schema"] = inputSchema
			out["request_shape"] = requestShape
			out["request_shape_note"] = fmt.Sprintf("Pass each request_shape field directly in the %s tool call; do not nest the object under arguments or params.", spec.Facade)
			if raw, err := json.Marshal(inputSchema); err == nil {
				sum := sha256.Sum256(raw)
				out["schema_hash"] = hex.EncodeToString(sum[:])
			}
		}
	}
	return out
}

// analyzeFacadeCapabilitySchema turns the legacy unified dispatcher schema
// into the public operation-specific contract. Agents see only fields relevant
// to the selected kind, fixed safety arguments disappear, and conditional
// requirements become ordinary JSON Schema requirements.
func analyzeFacadeCapabilitySchema(spec facadeOperationSpec, legacyProperties map[string]any, legacyRequired []string) (map[string]any, map[string]any, []string) {
	options := make(map[string]any)
	output := make(map[string]any)
	for field, property := range legacyProperties {
		// kind is the discriminator and target is a top-level envelope
		// container the switch below publishes explicitly; neither belongs
		// in the per-operation options object.
		if field == "kind" || field == "target" {
			continue
		}
		if _, fixed := spec.Fixed[field]; fixed {
			continue
		}
		if !analyzeFieldApplies(spec.Operation, field, property) {
			continue
		}
		switch field {
		case "format", "max_bytes", "cursor", "fields", "compact", "limit":
			output[field] = property
		default:
			options[field] = property
		}
	}

	requiredFields := append([]string(nil), analyzeRequiredFields(spec.Operation)...)
	for _, field := range legacyRequired {
		if field == "kind" {
			continue
		}
		if _, fixed := spec.Fixed[field]; fixed {
			continue
		}
		if _, available := options[field]; available && !slices.Contains(requiredFields, field) {
			requiredFields = append(requiredFields, field)
		}
	}
	if spec.Facade == "workspace_admin" {
		arguments := map[string]any{
			"type":                 "object",
			"properties":           options,
			"additionalProperties": false,
		}
		if len(requiredFields) > 0 {
			arguments["required"] = requiredFields
		}
		properties := map[string]any{
			"operation": map[string]any{"type": "string", "const": spec.Operation},
			"arguments": arguments,
		}
		if len(output) > 0 {
			properties["output"] = map[string]any{"type": "object", "properties": output, "additionalProperties": false}
		}
		return map[string]any{
			"type": "object", "properties": properties,
			"required": []string{"operation", "arguments"}, "additionalProperties": false,
		}, mergeAnalyzeSchemaProperties(options, output), requiredFields
	}

	properties := map[string]any{
		"kind": map[string]any{"type": "string", "const": spec.Operation},
		"options": map[string]any{
			"type":                 "object",
			"properties":           options,
			"additionalProperties": false,
		},
	}
	topRequired := []string{"kind"}
	if len(requiredFields) > 0 {
		properties["options"].(map[string]any)["required"] = requiredFields
		topRequired = append(topRequired, "options")
	}
	if len(output) > 0 {
		properties["output"] = map[string]any{"type": "object", "properties": output, "additionalProperties": false}
	}
	// def_use, co_change and why are target-only. impact's target is optional:
	// with one it ranks that symbol's blast radius, without one it keeps
	// its repo-wide ranking.
	switch spec.Operation {
	case "def_use", "co_change", "impact", "why":
		targetProperties := map[string]any{"symbol": map[string]any{"type": "string"}}
		if spec.Operation != "def_use" && spec.Operation != "why" {
			targetProperties["file"] = map[string]any{"type": "string"}
		}
		properties["target"] = map[string]any{
			"type": "object", "properties": targetProperties,
			"minProperties": 1, "maxProperties": 1, "additionalProperties": false,
		}
		if spec.Operation != "impact" {
			topRequired = append(topRequired, "target")
		}
	}
	return map[string]any{
		"type": "object", "properties": properties,
		"required": topRequired, "additionalProperties": false,
	}, mergeAnalyzeSchemaProperties(options, output), requiredFields
}

func mergeAnalyzeSchemaProperties(options, output map[string]any) map[string]any {
	merged := make(map[string]any, len(options)+len(output))
	for key, value := range options {
		merged[key] = value
	}
	for key, value := range output {
		merged[key] = value
	}
	return merged
}

func analyzeRequiredFields(kind string) []string {
	switch kind {
	case "coverage":
		return []string{"profile"}
	case "would_create_cycle":
		return []string{"from_id", "to_id"}
	default:
		return nil
	}
}

// analyzeFieldApplies filters the legacy dispatcher's annotated field list.
// Kind-specific descriptions start with one or more parenthesized kind groups;
// unannotated fields are shared. A few handlers predate complete annotations
// and are covered by the explicit additions below.
func analyzeFieldApplies(kind, field string, raw any) bool {
	if kind == "help" {
		return false
	}
	property, _ := raw.(map[string]any)
	description, _ := property["description"].(string)
	description = strings.TrimSpace(description)
	if strings.HasPrefix(description, "(") {
		matched := false
		remaining := description
		for {
			start := strings.IndexByte(remaining, '(')
			if start < 0 {
				break
			}
			remaining = remaining[start+1:]
			end := strings.IndexByte(remaining, ')')
			if end < 0 {
				break
			}
			for _, candidate := range strings.Split(remaining[:end], ",") {
				if normalizeFacadeOperation(candidate) == kind {
					matched = true
				}
			}
			remaining = strings.TrimSpace(remaining[end+1:])
		}
		if !matched {
			switch kind + "." + field {
			case "impact.ids", "impact.path_prefix", "impact.kinds", "impact.min_score", "impact.max_score", "impact.limit",
				"def_use.id", "def_use.ids":
				return true
			default:
				return false
			}
		}
	}
	return true
}

// facadeRequestShape makes capabilities actionable without teaching callers
// canonical handler names. input_schema describes the operation-specific
// fields; request_shape shows the directly callable top-level fields and the
// target selector to use.
func facadeRequestShape(spec facadeOperationSpec, properties map[string]any, required []string) map[string]any {
	args := map[string]any{"operation": spec.Operation}
	placeholder := func(key string) map[string]any { return map[string]any{key: "<" + key + ">"} }
	hasLegacyField := func(key string) bool {
		_, ok := properties[key]
		return ok
	}

	switch spec.Facade {
	case "explore":
		switch spec.Operation {
		case "task", "context":
			args["task"] = "<task>"
		case "closure":
			args["options"] = map[string]any{"files": "<file>"}
		default:
			args["options"] = map[string]any{}
		}
	case "search":
		args["query"] = "<query>"
		args["options"] = map[string]any{}
	case "read":
		switch spec.Operation {
		case "file", "editing_context", "summary":
			args["target"] = placeholder("file")
		case "symbols":
			args["target"] = map[string]any{"symbols": []string{"<symbol>"}}
		case "artifact":
			args["target"] = placeholder("artifact")
		default:
			args["target"] = placeholder("symbol")
		}
		args["options"] = map[string]any{}
	case "relations":
		if spec.Operation == "declaration" {
			args["target"] = placeholder("query")
		} else {
			args["target"] = placeholder("symbol")
		}
		args["options"] = map[string]any{}
	case "trace":
		switch spec.Operation {
		case "flow", "path":
			args["target"] = placeholder("symbol")
			args["to"] = placeholder("symbol")
		case "taint":
			args["target"] = placeholder("query")
			args["to"] = placeholder("query")
		case "graph":
			args["options"] = map[string]any{"query": "<graph query>"}
		default:
			args["target"] = placeholder("symbol")
		}
		if _, ok := args["options"]; !ok {
			args["options"] = map[string]any{}
		}
	case "analyze":
		delete(args, "operation")
		args["kind"] = spec.Operation
		args["options"] = map[string]any{}
		switch spec.Operation {
		case "citation":
			args["options"] = map[string]any{"span": "<verbatim code>", "file_path": "<file>"}
		case "co_change", "def_use", "impact", "why":
			args["target"] = placeholder("symbol")
		case "would_create_cycle":
			args["options"] = map[string]any{"from_id": "<source symbol>", "to_id": "<target symbol>"}
		}
	case "ask":
		delete(args, "operation")
		args["question"] = "<question>"
	case "change":
		source := map[string]any{}
		switch spec.Operation {
		case "api_impact":
			source["file"] = "<file>"
		case "impact":
			args["target"] = placeholder("symbol")
		case "edit_plan", "guards", "tests":
			args["target"] = map[string]any{"symbols": []string{"<symbol>"}}
		case "pattern":
			source["symbols"] = []string{"<symbol>"}
		case "verify":
			source["changes"] = []map[string]any{{"symbol_id": "<symbol>", "new_signature": "<signature>"}}
		case "diagnostics", "code_actions", "ranges":
			source["file"] = "<file>"
			if spec.Operation == "ranges" {
				source["ranges"] = []map[string]any{{"file": "<file>", "start_line": 1, "end_line": 1}}
			}
		case "detect":
			source["scope"] = "unstaged"
		case "preview":
			source["workspace_edit"] = "<WorkspaceEdit JSON>"
		case "simulate":
			source["steps"] = "<WorkspaceEdit JSON array>"
		case "contract":
			args["target"] = map[string]any{"symbols": []string{"<symbol>"}}
		}
		if len(source) > 0 {
			args["source"] = source
		}
	case "review":
		args["source"] = map[string]any{}
	case "edit":
		switch spec.Operation {
		case "file":
			args["target"] = placeholder("file")
			args["match"] = "<existing text>"
			args["replacement"] = "<replacement text>"
		case "write":
			args["target"] = placeholder("file")
			args["content"] = "<file content>"
		case "symbol":
			args["target"] = placeholder("symbol")
			args["match"] = "<existing source>"
			args["replacement"] = "<replacement source>"
		case "batch":
			args["changes"] = []map[string]any{{
				"op": "edit_file", "path": "<file>",
				"old_string": "<existing text>", "new_string": "<replacement text>",
			}}
		default:
			args["options"] = map[string]any{}
		}
		if spec.Operation == "skill" {
			args["options"] = map[string]any{"directory": "<directory>"}
		}
		if hasLegacyField("dry_run") {
			args["dry_run"] = true
		}
	case "refactor":
		switch spec.Operation {
		case "fix_all", "apply_code_action":
			args["target"] = placeholder("file")
		case "rename":
			args["target"] = placeholder("symbol")
			args["new_name"] = "<new name>"
		case "move":
			args["target"] = placeholder("symbol")
			args["destination"] = "<destination file>"
		default:
			args["target"] = placeholder("symbol")
		}
		args["options"] = map[string]any{}
		if hasLegacyField("dry_run") {
			args["dry_run"] = true
		}
	case "session":
		args["arguments"] = map[string]any{}
		if spec.Operation == "subscribe" || spec.Operation == "unsubscribe" {
			args["channel"] = "<channel>"
		}
	case "capabilities":
		delete(args, "operation")
		args["domain"] = "<tool>"
	case "remember":
		args["arguments"] = map[string]any{}
		if spec.Operation == "risk_ack" {
			args["arguments"] = map[string]any{"source": "symbols", "symbols": "<symbol>"}
		}
	default:
		args["arguments"] = map[string]any{}
	}
	if spec.Facade == "workspace_admin" && spec.Operation == "coverage" {
		args["arguments"] = map[string]any{"profile": "<cover profile>"}
	}
	if spec.Facade != "analyze" && spec.Facade != "session" {
		facadeCompleteRequiredSelectors(spec, args, required)
	}

	// Manual aliases above cover common intent-oriented and conditional fields;
	// remaining schema-required legacy fields stay operation-specific under
	// options/arguments. Handler data preconditions may still apply.
	lowered := normalizeFacadeArguments(spec, args)
	var extras map[string]any
	for _, field := range required {
		if _, fixedOrLowered := lowered[field]; fixedOrLowered {
			continue
		}
		if extras == nil {
			container := "options"
			switch spec.Facade {
			case "publish_review", "pr", "recall", "remember", "workspace", "workspace_admin", "overlay", "response", "session":
				container = "arguments"
			}
			if existing, ok := args[container].(map[string]any); ok {
				extras = existing
			} else {
				extras = map[string]any{}
				args[container] = extras
			}
		}
		extras[field] = facadeSchemaPlaceholder(field, properties[field])
	}
	return args
}

// applyFacadeSurface provides session-level surface negotiation. Legacy
// clients never see the new dedicated facade names. facade-v1 clients see
// exactly the 21 compact definitions, including reused names whose global
// registration still carries a legacy schema.
func (s *Server) applyFacadeSurface(ctx context.Context, tools []mcpgo.Tool) []mcpgo.Tool {
	p := s.effectiveSessionPolicy(ctx)
	if p == nil || p.preset != FacadeSurfaceVersion {
		out := tools[:0]
		for _, tool := range tools {
			if isDedicatedFacadeTool(tool.Name) {
				continue
			}
			if tool.Name == "ask" {
				if _, available := s.facades.legacy("ask"); !available {
					continue
				}
			}
			out = append(out, tool)
		}
		return out
	}
	byName := make(map[string]mcpgo.Tool, len(facadeToolNames()))
	for _, tool := range tools {
		if isFacadeToolName(tool.Name) {
			byName[tool.Name] = s.facadeToolDefinition(tool.Name)
		}
	}
	out := make([]mcpgo.Tool, 0, len(facadeToolNames()))
	for _, name := range facadeToolNames() {
		if tool, ok := byName[name]; ok {
			out = append(out, tool)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
