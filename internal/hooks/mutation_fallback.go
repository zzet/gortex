package hooks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const mutationRefusalFallbackTTL = 10 * time.Minute

type fileScopeProbe func(cwd, path string) fileIndexStatus

// The config registry names family primaries, not every automatic linked
// checkout. Its outside-root gate must not hide an authoritative refusal for
// the exact file being opened. This branch can only add recovery context;
// ready/unknown/outside answers keep their previous silent behavior.
func outsideConfiguredReadOnlyRecovery(input HookInput, probe fileScopeProbe) enrichResult {
	path := ""
	writing := false
	switch input.ToolName {
	case "Read", "Edit", "Write":
		path, _ = input.ToolInput["file_path"].(string)
		writing = input.ToolName != "Read"
	case "Bash":
		command, _ := input.ToolInput["command"].(string)
		c := classifyBashCommand(command)
		if c.Action != BashActionReadSource {
			return enrichResult{}
		}
		path = c.Path
	default:
		return enrichResult{}
	}
	if !looksLikeSourceFile(path) || probe == nil {
		return enrichResult{}
	}
	abs, ok := hookAbsScopePath(path, input.CWD)
	// Local linked metadata only chooses a candidate. It never establishes
	// membership or permission: the existing exact path probe does that.
	if !ok || linkedWorktreeRoot(abs) == "" {
		return enrichResult{}
	}
	// Preserve the original pair so Codex's rewrite/advisory decision can
	// reuse its already-observed result instead of issuing a second probe.
	st := probe(input.CWD, path)
	if !st.ProbeOK || st.Unreached || !st.ReadOnly {
		return enrichResult{}
	}
	return readOnlyNativeAllowance(writing)
}

// One real coverage probe remains authoritative. An unreached probe cannot be
// turned into a fallback allowance; an explicit modern ready/read_only answer
// wins over old local state without a PreToolUse session-state write.
func sessionFileScopeProbe(sessionID string) fileScopeProbe {
	return func(cwd, path string) fileIndexStatus {
		st := queryFileIndexScope(cwd, path)
		if sessionID == "" || st.Unreached || st.MutationKnown || !st.ProbeOK {
			return st
		}
		local := loadSessionState(sessionID)
		if local.MutationRefusalSession != sessionID || len(local.MutationRefusals) == 0 {
			return st
		}
		root := mutationCheckoutRoot(cwd, path, false)
		if root != "" && mutationFallbackLive(local, root, time.Now()) {
			st.ReadOnly = true
			st.MutationReason = "checkout mutation admission was refused"
		}
		return st
	}
}

func mutationFallbackLive(st sessionState, root string, now time.Time) bool {
	stamp := st.MutationRefusals[root]
	if stamp <= 0 {
		return false
	}
	age := now.Sub(time.Unix(0, stamp))
	return age >= 0 && age < mutationRefusalFallbackTTL
}

// Resolve the nearest actual checkout boundary, not a family-primary fallback
// or the longest tracked prefix. A linked worktree/submodule gets its own root.
// Unknown/symlink-unresolvable identities simply supply no legacy allowance.
func mutationCheckoutRoot(cwd, path string, directory bool) string {
	return mutationCheckoutRootWithLstat(cwd, path, directory, os.Lstat)
}

func mutationCheckoutRootWithLstat(cwd, path string, directory bool, lstat func(string) (os.FileInfo, error)) string {
	if path == "" {
		path, directory = cwd, true
	}
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return ""
		}
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if !directory {
		path = filepath.Dir(path)
	}
	for {
		// A new Write target may not exist; resolve each existing ancestor so
		// a symlinked parent cannot borrow the wrong checkout's allowance.
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		} else if !errors.Is(err, os.ErrNotExist) {
			return ""
		}
		if info, err := lstat(filepath.Join(path, ".git")); err == nil {
			if !info.IsDir() && !info.Mode().IsRegular() {
				return ""
			}
			root, err := filepath.EvalSymlinks(path)
			if err != nil || len(root) > 32<<10 {
				return ""
			}
			return root
		} else if !errors.Is(err, os.ErrNotExist) {
			return ""
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
}

func gortexMutationHookName(tool string) bool {
	if !isGortexMCPToolName(tool) {
		return false
	}
	switch shortGortexToolName(tool) {
	case "edit", "edit_file", "write_file", "edit_symbol", "batch_edit":
		return true
	}
	return false
}

func gortexMutationHookTool(tool string, args map[string]any) bool {
	if !gortexMutationHookName(tool) {
		return false
	}
	if shortGortexToolName(tool) != "edit" {
		return true
	}
	switch mapString(args, "operation") {
	case "file", "write", "symbol", "batch":
		return true
	case "":
		return len(writeTargetPaths(tool, args)) > 0
	}
	return false
}

func mutationResponseCheckout(input postHookInput) string {
	root := mutationCheckoutRoot(input.CWD, "", true)
	if raw, present := input.ToolInput["view"]; present {
		view, ok := raw.(map[string]any)
		if !ok {
			return ""
		}
		switch mapString(view, "kind") {
		case "", "auto":
			if mapString(view, "checkout_id") != "" || mapString(view, "path") != "" || mapString(view, "graph_id") != "" {
				return ""
			}
		case "worktree":
			path := mapString(view, "path")
			if path == "" || !filepath.IsAbs(path) {
				return "" // an ID-only selector cannot be guessed from CWD
			}
			root = mutationCheckoutRoot("", path, true)
		default:
			return ""
		}
	}
	if root == "" {
		return ""
	}
	for _, target := range writeTargetPaths(input.ToolName, input.ToolInput) {
		path, _, _ := strings.Cut(target, "::")
		if filepath.IsAbs(path) && mutationCheckoutRoot("", path, false) != root {
			return "" // a cross-checkout batch is not one known checkout
		}
	}
	return root
}

// Only a structured MCP CallToolResult proves success or failure. Plain text
// (even text containing serialized JSON/error words) cannot establish an error
// outcome. MCP omits isError on success; a content array still identifies the
// result envelope. The text being read never supplies its own error flag.
func mutationMCPOutcome(response any) (text string, failed, known bool) {
	result, ok := response.(map[string]any)
	if !ok {
		return "", false, false
	}
	if wrapped, ok := result["result"].(map[string]any); ok && result["jsonrpc"] == "2.0" {
		result = wrapped
	}
	content, ok := result["content"].([]any)
	if !ok {
		return "", false, false
	}
	if raw, exists := result["isError"]; exists {
		failed, ok = raw.(bool)
		if !ok {
			return "", false, false
		}
	}
	var parts []string
	for _, item := range content {
		if row, ok := item.(map[string]any); ok && row["type"] == "text" {
			if body, ok := row["text"].(string); ok {
				parts = append(parts, body)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), failed, true
}

func staleMutationRefusalText(text string) bool {
	// Both checked entry points preserve these exact admission prefixes.
	admission := strings.HasPrefix(text, "view_read_only: checkout mutation was not admitted; no files were changed:") ||
		strings.HasPrefix(text, "view_read_only: byte mutation admission refused; no files were changed:")
	return admission && strings.Contains(text, "indexer: checkout mutation view is stale") &&
		!strings.Contains(text, "indexer: checkout mutation lane is busy") &&
		!strings.Contains(text, "indexer: checkout mutation refresh is pending")
}

// This reuses sessionState's existing best-effort persistence. It does not add
// atomic session updates, a tool-attempt ledger or another policy reconciler.
// Missing host outcome/checkout/session evidence supplies no new allowance.
func observeMutationFallback(input postHookInput, now time.Time) bool {
	if input.SessionID == "" || !gortexMutationHookTool(input.ToolName, input.ToolInput) {
		return false
	}
	text, failed, known := mutationMCPOutcome(input.ToolResponse)
	if !known || (failed && !staleMutationRefusalText(text)) {
		return true
	}
	root := mutationResponseCheckout(input)
	if root == "" {
		return true
	}
	st := loadSessionState(input.SessionID)
	if st.MutationRefusalSession != input.SessionID {
		st.MutationRefusals = nil
		st.MutationRefusalSession = strings.Clone(input.SessionID)
	}
	for key := range st.MutationRefusals {
		if !mutationFallbackLive(st, key, now) {
			delete(st.MutationRefusals, key)
		}
	}
	if failed {
		if st.MutationRefusals == nil {
			st.MutationRefusals = make(map[string]int64)
		}
		st.MutationRefusals[strings.Clone(root)] = now.UnixNano()
	} else {
		delete(st.MutationRefusals, root)
	}
	saveSessionState(input.SessionID, st)
	return true
}
