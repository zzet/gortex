package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/agents/copilotcli"
	"github.com/zzet/gortex/internal/agents/hermes"
	"github.com/zzet/gortex/internal/agents/opencode"
	"github.com/zzet/gortex/internal/profiles"
)

// fileContains reports whether path exists and holds needle. Used to
// decide whether an agent's rule block is installed at all; an
// unreadable file is treated as "not installed" so a switch never
// creates a surface `gortex install` did not.
func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), needle)
}

// hostSkillSyncs is every host whose installed skill tree a profile
// switch has to reshape. Each entry reconciles that host's own root
// against the profile's subset, and each already knows how to enforce
// the one rule that matters: a skill the user edited is kept, never
// deleted to satisfy a profile.
//
// The table lives here, in cmd, deliberately. Routing the other hosts
// through claudecode would make one adapter depend on three others and
// invert exactly the direction internal/agents/skillpack was extracted
// to protect (adapters -> skillpack, never adapter -> adapter). The
// orchestrator is the layer allowed to know about all of them.
//
// Only Claude Code's entry installs missing skills on a machine that
// never ran `gortex install`; the three newer hosts no-op when their
// skills root is absent. That asymmetry is intentional — it preserves
// the behaviour `instructions switch` has always had for Claude Code
// while keeping a switch from conjuring a Codex / OpenCode / Copilot
// surface the user never asked for.
var hostSkillSyncs = []struct {
	name string
	sync func(w io.Writer, home string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error)
}{
	{"claude code", claudecode.SyncGlobalSkills},
	{"codex", codex.SyncSkills},
	{"opencode", opencode.SyncSkills},
	{"copilot cli", copilotcli.SyncSkills},
	{"hermes", hermes.SyncSkills},
}

// instructions.go is the `gortex instructions` command tree — the CLI
// front end for instruction profiles (internal/profiles): named
// bundles of instructions body + MCP tool preset + skills subset +
// hook-verbosity tier, generated from one table. `switch` atomically
// repoints the @-included active.md copy; agents pick the change up at
// their next session start.

var instructionsCmd = &cobra.Command{
	Use:   "instructions",
	Short: "Manage instruction profiles (guidance depth per machine)",
	Long: "Manage the machine's instruction profiles. A profile bundles the " +
		"agent-facing instructions body (@-included from the rules file), the " +
		"MCP tool preset sessions default to, the installed skills subset, and " +
		"the hook-verbosity tier — all generated from one table so they cannot " +
		"drift. Switching applies to NEW sessions only: instructions, " +
		"tools/list, and skills are all loaded at session start.",
}

var instructionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List profiles and show which one is active",
	Args:  cobra.NoArgs,
	RunE:  runInstructionsList,
}

var instructionsShowCmd = &cobra.Command{
	Use:   "show <profile>",
	Short: "Print one profile's instructions body",
	Args:  cobra.ExactArgs(1),
	RunE:  runInstructionsShow,
}

var instructionsSwitchCmd = &cobra.Command{
	Use:   "switch <profile>",
	Short: "Make a profile active (takes effect for new sessions)",
	Args:  cobra.ExactArgs(1),
	RunE:  runInstructionsSwitch,
}

var instructionsRegenCmd = &cobra.Command{
	Use:   "regen",
	Short: "Regenerate the profile files from this binary (keeps the active selection)",
	Args:  cobra.NoArgs,
	RunE:  runInstructionsRegen,
}

func init() {
	instructionsCmd.AddCommand(instructionsListCmd)
	instructionsCmd.AddCommand(instructionsShowCmd)
	instructionsCmd.AddCommand(instructionsSwitchCmd)
	instructionsCmd.AddCommand(instructionsRegenCmd)
	rootCmd.AddCommand(instructionsCmd)
}

func runInstructionsList(cmd *cobra.Command, _ []string) error {
	dir := profiles.DefaultDir()
	active := profiles.ActiveName(dir)
	cmd.Printf("Instruction profiles (dir: %s)\n\n", dir)
	for _, p := range profiles.Table() {
		marker := " "
		if p.Name == active {
			marker = "*"
		}
		preset := p.ToolPreset
		if preset == "" {
			preset = "(client default)"
		}
		skills := "all"
		if p.Skills != nil {
			skills = fmt.Sprintf("%d", len(p.Skills))
		}
		cmd.Printf("%s %-13s %s\n", marker, p.Name, p.Summary)
		cmd.Printf("    body: %4d bytes · tool preset: %s · skills: %s · hook tier: %s\n",
			len(p.Body()), preset, skills, p.HookTier)
	}
	cmd.Printf("\n* = active. Switch with `gortex instructions switch <name>` — applies to NEW sessions only.\n")
	return nil
}

func runInstructionsShow(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	p, ok := profiles.ByName(name)
	if !ok {
		return fmt.Errorf("unknown instruction profile %q (known: %s)", name, strings.Join(profiles.Names(), ", "))
	}
	cmd.Print(p.Body())
	return nil
}

func runInstructionsSwitch(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	dir := profiles.DefaultDir()
	p, err := profiles.Switch(dir, name, canonicalVersion())
	if err != nil {
		return err
	}
	cmd.Printf("Switched to the %q instruction profile (active copy: %s/%s).\n", p.Name, dir, profiles.ActiveFileName)

	// Reconcile the installed skills with the profile's subset, on every
	// skill-aware host — a profile that keeps three playbooks is a lie
	// if the other eighteen are still sitting in Codex's or OpenCode's
	// picker. Best effort: a missing home or an unwritable skills dir
	// downgrades to a warning, the profile switch itself has already
	// landed.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		installed, removed := 0, 0
		for _, host := range hostSkillSyncs {
			actions, err := host.sync(cmd.ErrOrStderr(), home, p.Skills, agents.ApplyOpts{})
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s skills sync incomplete: %v\n", host.name, err)
			}
			// Counted even on error: the actions returned before the
			// failure describe files that really did change.
			for _, a := range actions {
				switch a.Action {
				case agents.ActionCreate:
					installed++
				case agents.ActionDelete:
					removed++
				}
			}
		}
		if installed+removed > 0 {
			cmd.Printf("Skills reconciled: %d installed, %d removed.\n", installed, removed)
		}
		// Nudge when the pointer block was never installed — the
		// profile only reaches agents through the @-include.
		if md, err := os.ReadFile(claudecode.UserClaudeMdPath(home)); err != nil || !strings.Contains(string(md), agents.GlobalRulesStartMarker) {
			cmd.Printf("Note: no Gortex rule block found in ~/.claude/CLAUDE.md — run `gortex install` to wire the @-include pointer.\n")
		}

		// Codex has no @-include: its rule block is a copy of the active
		// profile body, so the switch has to rewrite it. Refresh only
		// when a block is already installed — creation stays with
		// `gortex install`, which is what decides Codex is set up at all.
		if path := codex.GlobalInstructionsPath(home); fileContains(path, agents.GlobalRulesStartMarker) {
			if _, err := agents.UpsertMarkedBlock(nil, path, agents.GlobalInlineBody(dir),
				agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, agents.ApplyOpts{}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not refresh %s: %v\n", path, err)
			} else {
				cmd.Printf("Refreshed the Gortex rule block in %s.\n", path)
			}
		}
	}

	cmd.Printf("\nTakes effect for NEW sessions only: the instructions @-include, the MCP tools/list, and skills are all loaded at session start — running sessions keep their current surface.\n")
	return nil
}

func runInstructionsRegen(cmd *cobra.Command, _ []string) error {
	dir := profiles.DefaultDir()
	if err := profiles.Generate(dir, canonicalVersion()); err != nil {
		return err
	}
	cmd.Printf("Regenerated %d profile(s) in %s (active: %s — unchanged).\n",
		len(profiles.Table()), dir, profiles.ActiveName(dir))
	return nil
}
