package agents

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/profiles"
)

// TestCommandPRReviewAgent_ShellsTheReviewVerb asserts the agent-review skill
// retains the dedicated CLI path only for a harness with no MCP transport by
// design. A missing configured handle remains an integration failure.
func TestCommandPRReviewAgent_ShellsTheReviewVerb(t *testing.T) {
	for _, want := range []string{
		"Native Gortex MCP is mandatory",
		"review({operation: \"run\"",
		"configured callable tools are missing",
		"Gortex MCP integration failure",
		"do not start a daemon or use the Bash path below",
		"## Bash-only harness",
		"no MCP transport by design",
		"gortex review --audience agent",
		"--format json",
		"VERDICT:",
		"file:line",
	} {
		if !strings.Contains(commandPRReviewAgent, want) {
			t.Errorf("commandPRReviewAgent must reference %q so the agent shells the verb and parses its output", want)
		}
	}
	if strings.Contains(commandPRReviewAgent, "when native Gortex MCP is available") {
		t.Error("commandPRReviewAgent must not reinterpret a missing MCP bridge as transport unavailability")
	}
}

// TestCommandPRReviewAgent_Registered asserts the agent-review skill is wired
// into both the slash-command registry and the global-skill registry under
// matching names, so the plugin emitter and every adapter pick it up.
func TestCommandPRReviewAgent_Registered(t *testing.T) {
	if got := SlashCommands["gortex-pr-review-agent.md"]; got != commandPRReviewAgent {
		t.Error("gortex-pr-review-agent.md must map to commandPRReviewAgent in SlashCommands")
	}
	skill, ok := GlobalSkills["gortex-pr-review-agent"]
	if !ok {
		t.Fatal("gortex-pr-review-agent must be registered in GlobalSkills")
	}
	// The skill body is the frontmatter + the command body; the command body
	// must be present verbatim so a drift edit can't silently desync them.
	if !strings.Contains(skill, commandPRReviewAgent) {
		t.Error("gortex-pr-review-agent skill body must embed commandPRReviewAgent")
	}
	if !strings.HasPrefix(skill, "---\nname: gortex-pr-review-agent\n") {
		t.Error("gortex-pr-review-agent skill must carry matching frontmatter")
	}
}

// TestEverySkillCarriesCanonicalWorktreeBranchPolicy asserts every
// global skill body embeds the worktree branch routing policy exactly once.
func TestEverySkillCarriesCanonicalWorktreeBranchPolicy(t *testing.T) {
	for name, body := range GlobalSkills {
		if got := strings.Count(body, profiles.WorktreeBranchRoutingPolicy); got != 1 {
			t.Errorf("skill %q embeds canonical worktree policy %d times, want once", name, got)
		}
	}
}

// TestGeneratedAgentContent_UsesOnlyPublicToolDomains asserts every
// slash command and skill body restricts itself to the public MCP tool
// surface and stays within a lean byte budget.
func TestGeneratedAgentContent_UsesOnlyPublicToolDomains(t *testing.T) {
	public := map[string]bool{
		"analyze": true, "ask": true, "capabilities": true, "change": true,
		"edit": true, "explore": true, "overlay": true, "pr": true,
		"publish_review": true, "read": true, "recall": true, "refactor": true,
		"relations": true, "remember": true, "response": true, "review": true,
		"search": true, "session": true, "trace": true, "workspace": true,
		"workspace_admin": true,
	}
	legacy := []string{
		"smart_context", "search_symbols", "search_text", "read_file",
		"get_symbol_source", "get_editing_context", "find_usages", "get_callers",
		"get_call_chain", "detect_changes", "explain_change_impact", "verify_change",
		"check_guards", "get_test_targets", "preview_edit", "simulate_chain",
		"batch_edit", "edit_symbol", "rename_symbol", "get_code_actions",
		"tools_search", "tool_profile", "facade-v1",
	}
	callPattern := regexp.MustCompile(`\b([a-z][a-z0-9_]*)\(\{`)

	artifacts := make(map[string]string, len(SlashCommands)+len(GlobalSkills))
	for name, body := range SlashCommands {
		artifacts["command/"+name] = body
	}
	for name, body := range GlobalSkills {
		artifacts["skill/"+name] = body
	}

	for name, body := range artifacts {
		for _, hidden := range legacy {
			if strings.Contains(body, hidden) {
				t.Errorf("%s exposes hidden implementation tool %q", name, hidden)
			}
		}
		for _, match := range callPattern.FindAllStringSubmatch(body, -1) {
			if !public[match[1]] {
				t.Errorf("%s instructs the agent to call non-public tool %q", name, match[1])
			}
		}
		if len(body) > 6000 {
			t.Errorf("%s is not lean: %d bytes (limit 6000)", name, len(body))
		}
	}
}
