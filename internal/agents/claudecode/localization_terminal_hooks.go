package claudecode

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// Claude Code treats "*" as its native match-all sentinel. Using it keeps
	// the terminal gate host-wide without paying regex matching cost for ".*".
	localizationPreToolUseMatcher  = "*"
	localizationPostToolUseMatcher = userMCPToolPrefix + "explore|" +
		userMCPToolPrefix + "search|" +
		userMCPToolPrefix + "read|" +
		userMCPToolPrefix + "relations|" +
		userMCPToolPrefix + "trace|" +
		userMCPToolPrefix + "analyze|" +
		pluginMCPToolPrefix + "explore|" +
		pluginMCPToolPrefix + "search|" +
		pluginMCPToolPrefix + "read|" +
		pluginMCPToolPrefix + "relations|" +
		pluginMCPToolPrefix + "trace|" +
		pluginMCPToolPrefix + "analyze|" +
		// The same managed observer also records stale mutation refusals for
		// older daemons. Keep it installed in every access-policy mode.
		userMCPToolPrefix + "edit|" +
		userMCPToolPrefix + "edit_file|" +
		userMCPToolPrefix + "write_file|" +
		userMCPToolPrefix + "edit_symbol|" +
		userMCPToolPrefix + "batch_edit|" +
		pluginMCPToolPrefix + "edit|" +
		pluginMCPToolPrefix + "edit_file|" +
		pluginMCPToolPrefix + "write_file|" +
		pluginMCPToolPrefix + "edit_symbol|" +
		pluginMCPToolPrefix + "batch_edit"
)

func desiredPostToolUseMatcher(mode string) string {
	if mode != HookModeEnrich {
		return localizationPostToolUseMatcher
	}
	return joinHookMatchers(CurrentPostToolUseMatcher, localizationPostToolUseMatcher)
}

func desiredPostToolUseStatus(mode string) string {
	if mode == HookModeEnrich {
		return "Layering Gortex graph context and watching localization completion..."
	}
	return "Watching for Gortex localization completion..."
}

func joinHookMatchers(matchers ...string) string {
	seen := make(map[string]struct{}, len(matchers))
	parts := make([]string, 0, len(matchers))
	for _, matcher := range matchers {
		for _, part := range strings.Split(matcher, "|") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "|")
}

func rewriteGortexEventMatcher(hooks map[string]any, event, matcher string) int {
	groups, ok := hooks[event].([]any)
	if !ok {
		return 0
	}
	changed := 0
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok || !managedGortexHookGroup(group, event) {
			continue
		}
		if current, _ := group["matcher"].(string); current == matcher {
			continue
		}
		group["matcher"] = matcher
		changed++
	}
	return changed
}

func rewriteGortexPostToolUseStatus(hooks map[string]any, mode string) int {
	groups, ok := hooks["PostToolUse"].([]any)
	if !ok {
		return 0
	}
	desired := desiredPostToolUseStatus(mode)
	changed := 0
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok || !managedGortexHookGroup(group, "PostToolUse") {
			continue
		}
		entries, _ := group["hooks"].([]any)
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				continue
			}
			if current, _ := entry["statusMessage"].(string); current == desired {
				continue
			}
			entry["statusMessage"] = desired
			changed++
		}
	}
	return changed
}

func managedGortexHookGroup(group map[string]any, event string) bool {
	entries, ok := group["hooks"].([]any)
	if !ok || len(entries) == 0 {
		return false
	}
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok || !managedGortexHookHandler(entry, event) {
			return false
		}
	}
	return true
}

// managedGortexHookHandler identifies ownership on one handler, never by
// pairing a Gortex command from one handler with a generated status message
// from another. A group is mutable only when every handler is managed.
func managedGortexHookHandler(entry map[string]any, event string) bool {
	command, _ := entry["command"].(string)
	if !handlerInvokesGortex(command) {
		return false
	}
	status, _ := entry["statusMessage"].(string)
	return managedGortexHookStatus(event, status)
}

func managedGortexHookStatus(event, status string) bool {
	switch event {
	case "PreToolUse":
		return status == preToolUseStatusDeny ||
			status == preToolUseStatusEnrich ||
			status == preToolUseStatusConsultUnlock ||
			status == preToolUseStatusNudge
	case "PostToolUse":
		return status == "Layering Gortex graph context onto tool output..." ||
			status == desiredPostToolUseStatus(HookModeDeny) ||
			status == desiredPostToolUseStatus(HookModeEnrich)
	default:
		return false
	}
}

func hasManagedGortexHookEntry(hooks map[string]any, event string) bool {
	groups, ok := hooks[event].([]any)
	if !ok {
		return false
	}
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if ok && managedGortexHookGroup(group, event) {
			return true
		}
	}
	return false
}

func dedupManagedGortexEntries(hooks map[string]any, event string) int {
	groups, ok := hooks[event].([]any)
	if !ok {
		return 0
	}
	seen := false
	removed := 0
	kept := make([]any, 0, len(groups))
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok || !managedGortexHookGroup(group, event) {
			kept = append(kept, rawGroup)
			continue
		}
		if seen {
			removed++
			continue
		}
		seen = true
		kept = append(kept, rawGroup)
	}
	if removed > 0 {
		hooks[event] = kept
	}
	return removed
}

func hookGroupInvokesGortex(group map[string]any) bool {
	entries, ok := group["hooks"].([]any)
	if !ok {
		return false
	}
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		command, _ := entry["command"].(string)
		if handlerInvokesGortex(command) {
			return true
		}
	}
	return false
}

func handlerInvokesGortex(command string) bool {
	return commandInvokesGortexHook(command) || strings.Contains(command, "gortex-hook")
}

func pluginHooksWithLocalizationTerminality(data []byte) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse generated plugin hooks: %w", err)
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("generated plugin hooks: hooks object is missing")
	}

	preGroups, ok := hooks["PreToolUse"].([]any)
	if !ok {
		return nil, fmt.Errorf("generated plugin hooks: PreToolUse is missing")
	}
	var prototype map[string]any
	for _, rawGroup := range preGroups {
		group, ok := rawGroup.(map[string]any)
		if !ok || !hookGroupInvokesGortex(group) {
			continue
		}
		group["matcher"] = localizationPreToolUseMatcher
		if prototype == nil {
			prototype = group
		}
	}
	if prototype == nil {
		return nil, fmt.Errorf("generated plugin hooks: Gortex PreToolUse entry is missing")
	}

	postGroups, _ := hooks["PostToolUse"].([]any)
	postFound := false
	for _, rawGroup := range postGroups {
		group, ok := rawGroup.(map[string]any)
		if !ok || !hookGroupInvokesGortex(group) {
			continue
		}
		current, _ := group["matcher"].(string)
		group["matcher"] = joinHookMatchers(current, localizationPostToolUseMatcher)
		postFound = true
	}
	if !postFound {
		encoded, err := json.Marshal(prototype)
		if err != nil {
			return nil, err
		}
		var group map[string]any
		if err := json.Unmarshal(encoded, &group); err != nil {
			return nil, err
		}
		group["matcher"] = localizationPostToolUseMatcher
		for _, rawEntry := range group["hooks"].([]any) {
			if entry, ok := rawEntry.(map[string]any); ok {
				entry["statusMessage"] = desiredPostToolUseStatus(HookModeDeny)
			}
		}
		postGroups = append(postGroups, group)
		hooks["PostToolUse"] = postGroups
	}

	return json.MarshalIndent(root, "", "  ")
}
