package hermes

import (
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	yaml "gopkg.in/yaml.v3"
)

// Hermes shell-hook wiring. The Claude Code adapter installs PreToolUse
// (+ UserPromptSubmit / SessionStart) hooks that redirect Grep / Glob /
// Read of indexed source to graph tools; Hermes exposes the equivalent
// primitives under the `hooks:` block of ~/.hermes/config.yaml, so this
// file gives Hermes the same graph-first enforcement.
//
// Two events are wired (the only two that can affect a turn — Hermes'
// on_session_start is observer-only and post_tool_call is
// fire-and-forget, so neither can inject context or block):
//
//   - pre_tool_call (matcher "read_file|terminal"): the block/redirect
//     hook. read_file covers whole-file reads of indexed source;
//     terminal covers shell grep / find / cat (Hermes has no separate
//     Grep / Glob tool — every shell command rides the one terminal
//     tool). Honours the posture flag the same way Claude Code does.
//   - pre_llm_call (no matcher): context injection. It does double duty
//     — the Gortex orientation briefing on the first turn (Claude's
//     SessionStart equivalent, which Hermes lacks) and relevant-symbol
//     surfacing on every later turn (Claude's UserPromptSubmit).
//
// Both events are written only to the GLOBAL ~/.hermes/config.yaml.
// Unlike `mcp_servers` (which a profile can re-declare, so we upsert
// per-profile too), Hermes documents shell hooks only at global scope.
const (
	// hermesPreToolEvent / hermesPreLLMEvent are the snake_case event
	// keys under the config's `hooks:` map.
	hermesPreToolEvent = "pre_tool_call"
	hermesPreLLMEvent  = "pre_llm_call"

	// hermesToolMatcher is the regex Hermes matches against tool_name
	// for the pre_tool_call hook. It must agree with the tool names the
	// hooks.RunHermes dispatcher inspects (read_file + terminal).
	hermesToolMatcher = "read_file|terminal"

	// hermesHookTimeoutSecs bounds each hook invocation. Hermes hook
	// timeouts are in seconds (default 60, max 300). 5s is generous for a
	// localhost daemon probe yet well under the ceiling, so a wedged
	// daemon can't stall a turn for long.
	hermesHookTimeoutSecs = 5
)

// upsertGortexHooks merges the gortex pre_tool_call + pre_llm_call
// entries into the `hooks:` block of an already-open config root,
// preserving comments and unrelated hooks. It is called from inside the
// global-config MergeYAML mutate so the whole config.yaml (mcp_servers +
// hooks) is written in a single comment-preserving pass. Returns whether
// it changed anything.
func upsertGortexHooks(root *yaml.Node, mode string, force bool) (bool, error) {
	hooksNode, err := ensureHooksMapping(root)
	if err != nil {
		return false, err
	}

	binary := hermesHookBinary()
	preToolCmd := binary + " hook --agent hermes" + hermesModeSuffix(mode)
	preLLMCmd := binary + " hook --agent hermes"

	// pre_tool_call carries the matcher + posture; pre_llm_call is the
	// matcher-less injection hook (its posture is irrelevant — it never
	// blocks).
	changedTool := upsertHookEvent(hooksNode, hermesPreToolEvent, hermesToolMatcher, preToolCmd, force)
	changedLLM := upsertHookEvent(hooksNode, hermesPreLLMEvent, "", preLLMCmd, force)
	return changedTool || changedLLM, nil
}

// ensureHooksMapping returns the `hooks:` mapping node, creating it when
// absent or replacing an explicit-null value (`hooks:` with nothing, or
// `hooks: {}` decoded to null) in place so its leading comment survives.
// A non-mapping `hooks:` value is refused rather than clobbered.
func ensureHooksMapping(root *yaml.Node) (*yaml.Node, error) {
	node := agents.YAMLMapValue(root, "hooks")
	switch {
	case node == nil, yamlIsNull(node):
		node = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		agents.YAMLSetMapValue(root, "hooks", node)
	case node.Kind == yaml.MappingNode:
		// Reuse in place.
	default:
		return nil, &hooksShapeError{}
	}
	return node, nil
}

// upsertHookEvent ensures hooks[event] is a sequence holding exactly one
// gortex entry with the desired matcher + command + timeout. An existing
// gortex entry is re-stamped in place when it drifts (binary path moved,
// posture switched) so user-added fields and comments survive; a missing
// one is appended; an up-to-date one is left untouched unless force.
// Non-gortex entries in the same sequence are never touched.
func upsertHookEvent(hooksNode *yaml.Node, event, matcher, command string, force bool) bool {
	seq := agents.YAMLMapValue(hooksNode, event)
	if seq == nil || yamlIsNull(seq) {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		agents.YAMLSetMapValue(hooksNode, event, seq)
	} else if seq.Kind != yaml.SequenceNode {
		// `event:` holds a scalar/mapping rather than a list — not a
		// shape we can splice an entry into. Leave it alone.
		return false
	}

	existing := findGortexHookEntry(seq)
	if existing == nil {
		seq.Content = append(seq.Content, hermesHookEntryNode(matcher, command))
		return true
	}
	if hookEntryMatches(existing, matcher, command) && !force {
		return false
	}
	// Re-stamp the existing entry's contents in place (keeps node
	// identity, so any HeadComment the user attached to it survives).
	desired := hermesHookEntryNode(matcher, command)
	existing.Content = desired.Content
	return true
}

// findGortexHookEntry returns the first sequence item that is a gortex
// hook entry (its `command` invokes `gortex hook --agent hermes`), or
// nil. Identifies our entry by the command string so a re-install
// updates it in place rather than appending a duplicate.
func findGortexHookEntry(seq *yaml.Node) *yaml.Node {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		cmd := agents.YAMLMapValue(item, "command")
		if cmd != nil && cmd.Kind == yaml.ScalarNode && commandIsGortexHermesHook(cmd.Value) {
			return item
		}
	}
	return nil
}

// commandIsGortexHermesHook reports whether a hook command string is a
// gortex Hermes hook invocation: it runs the `hook` subcommand of a
// gortex binary with the hermes agent protocol selected. Robust to the
// binary being an absolute path and to `--agent hermes` vs
// `--agent=hermes`.
func commandIsGortexHermesHook(cmd string) bool {
	lower := strings.ToLower(cmd)
	if !strings.Contains(lower, "gortex") || !strings.Contains(lower, "hook") {
		return false
	}
	return strings.Contains(cmd, "--agent hermes") || strings.Contains(cmd, "--agent=hermes")
}

// hookEntryMatches reports whether an existing hook entry already has
// the desired matcher + command + timeout, so an idempotent re-run is a
// no-op. A missing matcher (pre_llm_call) must mean no matcher key.
func hookEntryMatches(entry *yaml.Node, matcher, command string) bool {
	if scalarValue(agents.YAMLMapValue(entry, "command")) != command {
		return false
	}
	if scalarValue(agents.YAMLMapValue(entry, "matcher")) != matcher {
		return false
	}
	return scalarValue(agents.YAMLMapValue(entry, "timeout")) == strconv.Itoa(hermesHookTimeoutSecs)
}

// hermesHookEntryNode builds one `hooks:` list entry: an optional
// matcher (omitted when empty — pre_llm_call takes no matcher), the
// command, and the timeout. matcher + command are double-quoted to
// match the documented Hermes example shape and to survive any future
// special characters in a binary path.
func hermesHookEntryNode(matcher, command string) *yaml.Node {
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if matcher != "" {
		agents.YAMLSetMapValue(entry, "matcher", yamlQuoted(matcher))
	}
	agents.YAMLSetMapValue(entry, "command", yamlQuoted(command))
	agents.YAMLSetMapValue(entry, "timeout", yamlInt(hermesHookTimeoutSecs))
	return entry
}

// hermesHookBinary resolves the gortex binary path for the hook command
// and normalises backslashes to forward slashes. Hermes runs hook
// commands through a shell; on Windows that shell is Git Bash, which
// mangles backslash path separators — forward slashes survive there and
// are correct everywhere else. Same treatment the Claude Code adapter
// applies to its hook command.
func hermesHookBinary() string {
	return strings.ReplaceAll(resolveGortexCommand(), "\\", "/")
}

// hermesModeSuffix renders the `--mode=<mode>` suffix for the
// pre_tool_call command. deny is the historical default and emitted bare
// so a deny install carries no suffix; every other posture is explicit.
// Reuses the Claude Code adapter's canonical mode strings so the two
// adapters can't drift on what a posture is called.
func hermesModeSuffix(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case claudecode.HookModeEnrich:
		return " --mode=enrich"
	case claudecode.HookModeConsultUnlock:
		return " --mode=consult-unlock"
	case claudecode.HookModeAdaptiveNudge, "adaptive-nudge":
		return " --mode=nudge"
	default:
		return ""
	}
}

// yamlIsNull reports whether n is a YAML null — an explicit null scalar
// (`~`, `null`) or an empty value (`key:` with nothing after it). A
// local copy of the agents-package predicate, which is unexported.
func yamlIsNull(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && (n.Tag == "!!null" || n.Value == "")
}

// yamlQuoted builds a double-quoted string scalar node.
func yamlQuoted(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.DoubleQuotedStyle, Value: s}
}

// scalarValue returns a scalar node's value, or "" for a nil / non-scalar.
func scalarValue(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// hooksShapeError is returned when `hooks:` holds a non-mapping value we
// refuse to overwrite.
type hooksShapeError struct{}

func (*hooksShapeError) Error() string {
	return "hermes: `hooks` is not a mapping; refusing to overwrite"
}
