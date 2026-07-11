package main

import (
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/hooks"
)

var (
	hookPort  int
	hookMode  string
	hookAgent string
)

var hookCmd = &cobra.Command{
	Use:    "hook",
	Short:  "Agent hook handler (Claude Code by default; --agent for Gemini / Antigravity / Hermes / Kimi)",
	Hidden: true, // Not for direct user invocation.
	Run: func(_ *cobra.Command, _ []string) {
		// --agent selects the hook wire protocol. Empty (the default) is the
		// Claude Code format; protocol-specific agents branch below, and the
		// remaining external agents share the hookSpecificOutput.additionalContext
		// wire shape.
		switch hookAgent {
		case "hermes":
			// Hermes (NousResearch hermes-agent) sends
			// snake_case events and expects an action/message decision shape, so
			// it gets its own dispatcher.
			hooks.RunHermes(hookPort, hooks.ParseMode(hookMode))
			return
		case "pi":
			// Pi (earendil-works/pi) has no MCP; its Gortex extension
			// shells `gortex hook --agent=pi`, sending a normalized event
			// envelope on stdin and applying the PiDecision read back.
			hooks.RunPi(hookPort, hooks.ParseMode(hookMode))
			return
		case "codex":
			// Codex defaults to advisory-only enrichment. An explicit
			// --codex-hook-mode=deny installation opts into enforcement.
			hooks.RunCodex(hookPort, hooks.ParseMode(hookMode))
			return
		case "kimi":
			// Kimi Code CLI: UserPromptSubmit / PreToolUse / Stop /
			// SubagentStart. Soft guidance rides Kimi's plain-stdout context
			// channel; an indexed whole-file read is blocked via the documented
			// hookSpecificOutput.permissionDecision shape.
			hooks.RunKimi(hookPort, hooks.ParseMode(hookMode))
			return
		case "", "claude":
			// Claude Code — handled below.
		default:
			hooks.RunExternalAgent()
			return
		}
		hooks.Run(hookPort, hooks.ParseMode(hookMode))
	},
}

func init() {
	hookCmd.Flags().IntVar(&hookPort, "port", 8765, "Gortex web server port")
	hookCmd.Flags().StringVar(&hookMode, "mode", "deny",
		"hook posture: 'deny' (redirect Grep/Glob/Read of indexed source), 'enrich' (never deny; PostToolUse appends graph context), 'consult-unlock' (deny fallback reads until the graph is queried once this session), or 'nudge' (soft-deny once per burst of non-symbolic calls)")
	hookCmd.Flags().StringVar(&hookAgent, "agent", "",
		"hook wire protocol: empty/'claude' (Claude Code PreToolUse/UserPromptSubmit), 'codex' (Codex Bash PreToolUse/PostToolUse + UserPromptSubmit; advisory by default, enforcement when installed with --codex-hook-mode=deny), 'kimi' (Kimi Code CLI UserPromptSubmit/PreToolUse/Stop/SubagentStart; plain-stdout context, permissionDecision deny for indexed reads), 'hermes' (NousResearch hermes-agent pre_tool_call/pre_llm_call), 'pi' (earendil-works/pi extension bridge — normalized PiEvent envelope in, PiDecision out), or 'gemini'/'antigravity' (emits hookSpecificOutput.additionalContext). Default (empty) is the Claude Code format.")
	rootCmd.AddCommand(hookCmd)
}
