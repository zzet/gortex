package main

import (
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/hooks"
)

var (
	hookPort  int
	hookMode  string
	hookAgent string
	hookEvent string
)

var hookCmd = &cobra.Command{
	Use:    "hook",
	Short:  "Agent hook handler (Claude Code by default; --agent for Codex / Copilot CLI / Kimi / Hermes / Pi / OpenCode / Gemini / Antigravity)",
	Hidden: true, // Not for direct user invocation.
	Run: func(_ *cobra.Command, _ []string) {
		// --agent selects the hook wire protocol. Empty (the default) is the
		// Claude Code format; protocol-specific agents branch below, and the
		// remaining external agents share the hookSpecificOutput.additionalContext
		// wire shape.
		// Attribute every telemetry row this process writes to the harness that
		// invoked it, before any handler runs.
		hooks.SetAgent(hookAgent)

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
		case "opencode":
			// OpenCode has no hook configuration at all — its Gortex plugin
			// (JS) shells `gortex hook --agent=opencode` from
			// tool.execute.before / permission.ask / chat.message and applies
			// the BridgeDecision read back, so it shares Pi's bridge core.
			hooks.RunOpenCode(hookPort, hooks.ParseMode(hookMode))
			return
		case "copilot-cli":
			// GitHub Copilot CLI: camelCase lifecycle events and a flat,
			// per-event stdout shape (permissionDecision / modifiedPrompt /
			// additionalContext) rather than Claude's hookSpecificOutput
			// envelope, over a lowercase tool vocabulary of its own.
			hooks.RunCopilot(hookPort, hooks.ParseMode(hookMode))
			return
		case "codex":
			// Codex defaults to advisory context. Explicit postures add hard
			// deny, conservative input rewrite, or PostToolUse result replacement.
			hooks.RunCodex(hookPort, hooks.ParseCodexMode(hookMode))
			return
		case "kimi":
			// Kimi Code CLI: UserPromptSubmit / PreToolUse / Stop /
			// SubagentStart. Soft guidance rides Kimi's plain-stdout context
			// channel; an indexed whole-file read is blocked via the documented
			// hookSpecificOutput.permissionDecision shape.
			hooks.RunKimi(hookPort, hooks.ParseMode(hookMode))
			return
		case "antigravity":
			// Google Antigravity (agy CLI / Antigravity 2.0 / IDE):
			// protojson stdin with workspacePaths + toolCall, call_mcp_tool
			// unwrapping, and per-event injectSteps / decision stdout.
			hooks.RunAntigravity(hookPort, hooks.ParseMode(hookMode), hookEvent)
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
	hookCmd.Flags().StringVar(&hookMode, "mode", "",
		"hook posture: Claude defaults to deny and accepts deny|enrich|consult-unlock|nudge; Codex defaults to enrich and accepts enrich|deny|rewrite|suppress")
	hookCmd.Flags().StringVar(&hookAgent, "agent", "",
		"hook wire protocol: empty/'claude' (Claude Code lifecycle hooks), 'codex' (Codex Bash/MCP/apply_patch hooks), 'copilot-cli' (GitHub Copilot CLI hooks), 'kimi' (Kimi Code CLI hooks), 'hermes' (NousResearch hermes-agent hooks), 'pi' (Pi extension bridge), 'opencode' (OpenCode plugin bridge), 'antigravity' (Google Antigravity hooks), or 'gemini'. Default (empty) is Claude Code.")
	hookCmd.Flags().StringVar(&hookEvent, "event", "",
		"lifecycle event name when the host omits it from stdin (e.g. Antigravity SessionStart|PreInvocation|PreToolUse|PostToolUse|Stop)")
	rootCmd.AddCommand(hookCmd)
}
