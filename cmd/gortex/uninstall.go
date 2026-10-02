package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/agents/copilotcli"
	"github.com/zzet/gortex/internal/agents/opencode"
	"github.com/zzet/gortex/internal/agents/pi"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
)

var (
	uninstallYes             bool
	uninstallGlobal          bool
	uninstallPurge           bool
	uninstallClaudeConfigDir string
)

// uninstallCmd is the canonical "remove Gortex from this project" command.
// `clean` is preserved as an alias because the original name predates this
// rename and is referenced in the wild — cobra resolves the alias before
// dispatching, so both verbs land in the same RunE.
var uninstallCmd = &cobra.Command{
	Use:     "uninstall",
	Aliases: []string{"clean"},
	Short:   "Uninstall Gortex from this repository (remove per-repo config + generated files)",
	Long: `Removes the per-repo Gortex footprint from the current directory:

  .mcp.json
  .opencode/plugin/gortex.js
  .claude/commands/
  .kiro/{steering,hooks,settings}/
  the gortex-* skills inside .agents/skills/, .opencode/skills/ and
  .github/skills/ (those trees are shared — your own skills stay)

Counterpart to ` + "`gortex init`" + `. For machine-wide setup (user MCP config,
rule blocks, user hooks) installed by ` + "`gortex install`" + `, pass --global
to also strip the user-level footprint of every host Gortex configures —
Claude Code, Codex, GitHub Copilot CLI, OpenCode and Pi: MCP entries,
permission allowlists, hooks, the rule blocks, and the gortex skills /
commands / sub-agents / bridge plugin / Pi sidecar. Merged files keep
everything that is not ours, and a gortex skill you have edited is kept
rather than deleted. Pi's own ` + "`packages`" + ` entry stays: that file is
yours, and ` + "`pi remove npm:pi-gortex`" + ` is what takes it back.
--global honors $CLAUDE_CONFIG_DIR; target a specific Claude Code profile
with --claude-config-dir.

Pass --purge to additionally tear down the machine-level runtime: stop the
daemon, remove its OS service unit (launchd / systemd / Task Scheduler),
and delete the unified ~/.gortex data/cache/config tree — plus the binary
when it was dropped by the installer script (package-manager installs are
left to the manager). --purge is orthogonal to per-repo cleanup and can be
combined with --global.

Destructive by design: the interactive run shows a confirm wizard listing
every file before any deletion. Non-TTY callers must pass --yes to bypass
the wizard — that gate is intentional, the safer default is to refuse and
tell the user instead of silently nuking config.

The legacy ` + "`gortex clean`" + ` invocation is kept as an alias so existing
docs / scripts / muscle memory still work.`,
	RunE: runUninstall,
}

func init() {
	uninstallCmd.Flags().BoolVarP(&uninstallYes, "yes", "y", false, "skip the confirmation prompt (required when stdin is not a TTY)")
	uninstallCmd.Flags().BoolVar(&uninstallGlobal, "global", false, "also remove the machine-level footprint of every configured host (Claude Code, Codex, Copilot CLI, OpenCode): user MCP entries, settings, rule blocks, hooks, skills/commands/agents/plugin, as installed by `gortex install`")
	uninstallCmd.Flags().BoolVar(&uninstallPurge, "purge", false, "also tear down the machine-level runtime: stop the daemon, remove its OS service unit, and delete the ~/.gortex data/cache/config tree (and the binary, for an installer-script install)")
	uninstallCmd.Flags().StringVar(&uninstallClaudeConfigDir, "claude-config-dir", "", "Claude Code config root to clean (implies --global); overrides $CLAUDE_CONFIG_DIR, defaults to ~/.claude")
	uninstallCmd.Flags().StringVar(&uninstallClaudeConfigDir, "config-root", "", "alias for --claude-config-dir")
	rootCmd.AddCommand(uninstallCmd)
}

// uninstallTargets is the canonical list of files / directories `gortex
// uninstall` removes from a project. Kept as a package-level slice so the
// confirm wizard can preview them before any deletion happens.
var (
	uninstallFiles = []string{
		".mcp.json",
		// The OpenCode bridge is user-level today, but an install from
		// before that split could have left one in the repo tree. The name
		// is Gortex's alone, so removing it here is safe.
		".opencode/plugin/gortex.js",
	}
	uninstallDirs = []string{
		".claude/commands",
		// Claude Code's per-community skills, unlike the three shared
		// trees below, sit under a `generated/` level that exists for
		// exactly this reason: nothing but Gortex writes there, so the
		// whole directory can go without inspecting its entries.
		".claude/skills/generated",
		".kiro/steering",
		".kiro/hooks",
		".kiro/settings",
	}
)

// uninstallOwnedDir is a target the slice-of-paths form above cannot
// express: "only the entries we own inside this directory".
//
// The three skill trees below are SHARED. `.agents/skills` is the
// vendor-neutral root Codex reads, `.opencode/skills` is OpenCode's and
// `.github/skills` is the Copilot CLI's, and in every one of them a team's
// own hand-written skills sit directly beside the ones `gortex init`
// generated. Adding any of these to uninstallDirs would delete the user's
// work, which is why they get a typed target rather than a bent slice.
//
// Ownership is the `gortex-` directory prefix — the same signal `gortex
// uninstall` already uses for `.kiro/steering` files, and the only one
// available here: generated community skills track the current graph, so
// unlike the curated user-level pack there is no stable shipped body to
// byte-compare against. The generators enforce that prefix on every
// DirName they emit (see each adapter's skills.go), so a `gortex-` skill
// directory in one of these trees is one we wrote.
type uninstallOwnedDir struct {
	// dir is the shared tree, repo-relative.
	dir string
	// prefix marks the entries inside it that are ours.
	prefix string
}

var uninstallOwnedDirs = []uninstallOwnedDir{
	{dir: ".agents/skills", prefix: "gortex-"},   // codex
	{dir: ".opencode/skills", prefix: "gortex-"}, // opencode
	{dir: ".github/skills", prefix: "gortex-"},   // copilot-cli
}

// ownedEntries lists the entries inside t.dir that are Gortex's, as
// repo-relative paths. Resolving the typed target down to concrete paths
// is what lets the confirm wizard preview exactly what will be deleted:
// preview and deletion read the same list rather than two descriptions of
// it that can drift.
func (t uninstallOwnedDir) ownedEntries() []string {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), t.prefix) {
			continue
		}
		out = append(out, filepath.Join(t.dir, e.Name()))
	}
	return out
}

// runUninstall is the per-repo removal entry point. Used by both
// `gortex uninstall` and `gortex clean` (the legacy alias) — same flow,
// same wizard, same summary card.
func runUninstall(cmd *cobra.Command, _ []string) error {
	w := cmd.ErrOrStderr()

	// An explicit --claude-config-dir / --config-root pins the Claude
	// Code config root to clean and implies --global (you only name a
	// profile when you mean to scrub it).
	if uninstallClaudeConfigDir != "" {
		abs, err := filepath.Abs(uninstallClaudeConfigDir)
		if err != nil {
			return fmt.Errorf("resolve --claude-config-dir: %w", err)
		}
		claudecode.SetConfigDirOverride(abs)
		uninstallGlobal = true
	}

	// Build the actual list of present targets so the wizard previews only
	// what would really be removed — listing every potential path even
	// when nothing exists feels noisy.
	presentFiles, presentDirs := filterPresentUninstallTargets()

	// --global also strips the user-level footprint that `gortex install`
	// wrote, for every host that has one. Each adapter's GlobalArtifacts
	// lists only paths that actually carry a Gortex footprint — and applies
	// the same ownership tests its RemoveGlobal does, so a skill the user
	// customised never shows up in a preview that would then keep it.
	// claudecode honors any config-dir override resolved above.
	var (
		home        string
		globalItems []string
	)
	if uninstallGlobal {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return fmt.Errorf("--global needs a home directory: %w", err)
		}
		home = h
		globalItems = globalUninstallArtifacts(home)
	}

	// --purge extends the removal to the machine-level runtime that `gortex
	// install` / the daemon created: the OS service unit, the unified
	// ~/.gortex data/cache/config tree, and (installer-script method) the
	// binary. Computed up front so the wizard previews the full blast radius.
	var (
		purge      purgePlan
		purgeItems []string
	)
	if uninstallPurge {
		purge = computePurgePlan()
		purgeItems = purgeWizardItems(purge, daemon.IsRunning())
	}

	totalPresent := len(presentFiles) + len(presentDirs) + len(globalItems) + len(purgeItems)
	if totalPresent == 0 {
		emitUninstallNothingTodo(w)
		return nil
	}

	// Gate the destructive op behind a confirm wizard when running on a TTY
	// and --yes / -y wasn't passed. Non-TTY callers without --yes get a
	// hard error so a misconfigured CI script can't silently nuke files.
	if !uninstallYes {
		if !progress.IsTTY(w) || noProgress {
			return fmt.Errorf("`gortex uninstall` is destructive; pass --yes to confirm (or run with a TTY for the interactive prompt)")
		}
		accepted, err := runUninstallConfirmWizard(w, presentFiles, presentDirs, globalItems, purgeItems)
		if err != nil {
			return err
		}
		if !accepted {
			fmt.Fprintln(w, "  cancelled — no files removed.")
			return nil
		}
	}

	removed, failures := executeUninstall(presentFiles, presentDirs)

	// User-level cleanup. RemoveGlobal strips only the Gortex portion
	// of merged files (other MCP servers / hooks / CLAUDE.md prose
	// survive) and deletes the owned skills/commands/agents. A nil
	// Stderr keeps its per-file chatter out of the styled summary.
	globalCleaned := false
	if uninstallGlobal && len(globalItems) > 0 {
		env := agents.Env{Home: home, Mode: agents.ModeGlobal}
		for _, host := range globalHosts() {
			gRemoved, gFailures := host.remove(env, agents.ApplyOpts{})
			removed += gRemoved
			failures = append(failures, gFailures...)
		}
		globalCleaned = true
	}

	// Machine-level teardown last, so a failure here still reports the
	// per-repo / global files already removed above.
	if uninstallPurge {
		pRemoved, pFailures := executePurge(cmd, purge)
		removed += pRemoved
		failures = append(failures, pFailures...)
		if note := purgeBinaryNote(purge); note != "" {
			fmt.Fprintln(w, note)
		}
	}

	emitUninstallSummary(w, removed, failures, totalPresent, globalCleaned)
	return nil
}

// filterPresentUninstallTargets resolves every target — the flat file and
// directory lists, plus the owned-entries-inside-a-shared-tree targets —
// down to the concrete paths that exist on disk right now.
//
// Lowering uninstallOwnedDirs here rather than at deletion time is what
// keeps the wizard honest: it previews the same list executeUninstall
// deletes, so it can neither promise to remove a user's own skill nor stay
// silent about a Gortex one it is about to delete.
func filterPresentUninstallTargets() ([]string, []string) {
	var pf, pd []string
	// The literal targets are written with '/' for readability, while the
	// owned-entry targets come out of filepath.Join. Normalising the
	// literals to the native separator keeps one spelling in the confirm
	// wizard: on Windows a list that mixes `.opencode/plugin/gortex.js`
	// with `.agents\skills\gortex-stub` reads as two different things.
	for _, f := range uninstallFiles {
		f = filepath.FromSlash(f)
		if _, err := os.Stat(f); err == nil {
			pf = append(pf, f)
		}
	}
	for _, d := range uninstallDirs {
		d = filepath.FromSlash(d)
		if _, err := os.Stat(d); err == nil {
			pd = append(pd, d)
		}
	}
	for _, owned := range uninstallOwnedDirs {
		pd = append(pd, owned.ownedEntries()...)
	}
	return pf, pd
}

// globalHost pairs one host's user-level footprint reporter with its
// remover.
//
// They are one row rather than two lists on purpose: that is the shape
// that makes the two halves impossible to get out of step. A host present
// only in the preview promises a deletion nothing performs; a host present
// only in the remover deletes files the confirm wizard never showed the
// user. Both are the same bug, and neither is representable here.
type globalHost struct {
	artifacts func(home string) []string
	remove    func(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string)
}

// globalHosts is every adapter with a user-level footprint `gortex
// install` writes and `gortex uninstall --global` must take back.
func globalHosts() []globalHost {
	return []globalHost{
		{artifacts: claudecode.GlobalArtifacts, remove: claudecode.New().RemoveGlobal},
		{artifacts: codex.GlobalArtifacts, remove: codex.New().RemoveGlobal},
		{artifacts: copilotcli.GlobalArtifacts, remove: copilotcli.New().RemoveGlobal},
		{artifacts: opencode.GlobalArtifacts, remove: opencode.New().RemoveGlobal},
		{artifacts: pi.GlobalArtifacts, remove: pi.New().RemoveGlobal},
	}
}

// globalUninstallArtifacts is the union of every host's user-level Gortex
// footprint, sorted so the wizard's list is stable across runs.
func globalUninstallArtifacts(home string) []string {
	var out []string
	for _, host := range globalHosts() {
		out = append(out, host.artifacts(home)...)
	}
	sort.Strings(out)
	return out
}

// runUninstallConfirmWizard renders a confirm wizard listing every target
// that would be removed, then waits for the user to accept or cancel.
// Returns accepted=true only on an explicit yes.
func runUninstallConfirmWizard(w io.Writer, files, dirs, global, purge []string) (bool, error) {
	items := make([]string, 0, len(files)+len(dirs)+len(global)+len(purge))
	for _, f := range files {
		items = append(items, f+"  "+progress.StyleHint.Render("(file)"))
	}
	for _, d := range dirs {
		items = append(items, d+"/  "+progress.StyleHint.Render("(directory + contents)"))
	}
	for _, g := range global {
		items = append(items, g+"  "+progress.StyleHint.Render("(user-level)"))
	}
	for _, p := range purge {
		items = append(items, p+"  "+progress.StyleHint.Render("(machine-level)"))
	}

	subtitle := "Remove all Gortex-generated config from this repo."
	switch {
	case len(purge) > 0:
		subtitle = "Remove Gortex config and tear down the machine-level runtime (daemon, service, ~/.gortex)."
	case len(global) > 0:
		subtitle = "Remove Gortex config from this repo and the user-level Claude Code profile."
	}
	m := tui.NewConfirmModel(
		"gortex uninstall",
		subtitle,
	)
	m.Warning = "This cannot be undone — re-run `gortex init` to restore."
	if len(purge) > 0 {
		m.Warning = "This cannot be undone — --purge deletes the daemon's data/cache/config tree."
	}
	m.Items = items
	m.YesLabel = "yes, remove " + strconv.Itoa(len(items)) + " item(s)"
	m.NoLabel = "no, keep them"

	prog := tea.NewProgram(m,
		tea.WithOutput(w),
		tea.WithAltScreen(),
		tea.WithoutSignalHandler(),
	)
	final, err := prog.Run()
	if err != nil {
		return false, fmt.Errorf("confirm: %w", err)
	}
	out, ok := final.(*tui.ConfirmModel)
	if !ok {
		return false, nil
	}
	return out.Accepted(), nil
}

// executeUninstall removes each present target and accumulates removed-count
// + per-target failures. Failures are warnings, not hard errors — partial
// success still emits a useful summary. (No writer dependency — caller emits
// the styled summary; this helper just does the disk work.)
func executeUninstall(files, dirs []string) (int, []string) {
	removed := 0
	var failures []string
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		removed++
	}
	for _, d := range dirs {
		if err := os.RemoveAll(d); err != nil {
			failures = append(failures, fmt.Sprintf("%s/: %v", d, err))
			continue
		}
		removed++
	}
	// A shared skill tree we emptied is ours to tidy; one that still holds
	// the user's own skills is not, and os.Remove refusing a non-empty
	// directory is exactly that distinction. Only trees we actually took an
	// entry out of are considered: an empty `.github/skills` the user made
	// and we never wrote to was not in the wizard's preview, so removing it
	// would be a deletion nobody agreed to.
	for _, owned := range uninstallOwnedDirs {
		cleaned := filepath.Clean(owned.dir)
		for _, d := range dirs {
			if filepath.Dir(d) == cleaned {
				_ = os.Remove(owned.dir)
				break
			}
		}
	}
	return removed, failures
}

// emitUninstallNothingTodo prints the empty-state message. One-liner on
// non-TTY, styled hint card on TTY.
func emitUninstallNothingTodo(w io.Writer) {
	if !progress.IsTTY(w) || noProgress {
		fmt.Fprintln(w, "[gortex uninstall] nothing to uninstall")
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+progress.StyleHint.Render("◌  nothing to uninstall — no Gortex artefacts found in this directory"))
	fmt.Fprintln(w)
}

// emitUninstallSummary prints the post-uninstall summary. Signals:
// removed count, failures (if any), whether the user-level Claude Code
// footprint was cleaned (globalCleaned, via --global), and the standing
// notes about the artifacts still left at user scope (Kiro / Antigravity
// / Hermes) that --global does not touch.
func emitUninstallSummary(w io.Writer, removed int, failures []string, totalPresent int, globalCleaned bool) {
	if !progress.IsTTY(w) || noProgress {
		if removed == 0 && len(failures) == 0 {
			fmt.Fprintln(w, "[gortex uninstall] nothing to uninstall")
			return
		}
		for _, f := range failures {
			fmt.Fprintf(w, "[gortex uninstall] failed: %s\n", f)
		}
		fmt.Fprintf(w, "[gortex uninstall] done (%d/%d items removed)\n", removed, totalPresent)
		if globalCleaned {
			fmt.Fprintln(w, "Note: the user-level footprint was removed for Claude Code, Codex, Copilot CLI, OpenCode and Pi (rule blocks, MCP entries, hooks, skills/commands/agents/plugin, Pi sidecar). Other content in those files was preserved, and any gortex skill you had edited was kept. Pi's `packages` entry stays: run `pi remove npm:pi-gortex` to drop it.")
		} else {
			fmt.Fprintln(w, "Note: CLAUDE.md was not modified — remove the Gortex block manually if needed (or re-run with --global).")
		}
		fmt.Fprintln(w, "Note: .kiro/steering/ files with 'gortex-' prefix were removed. Other .kiro/ files were preserved.")
		fmt.Fprintln(w, "Note: only 'gortex-' skills were removed from .agents/skills/, .opencode/skills/ and .github/skills/ — those trees are shared, so your own skills were preserved.")
		fmt.Fprintln(w, "Note: Antigravity KIs are global and were not removed. Manually delete ~/.gemini/antigravity/knowledge/gortex-workflow if desired.")
		fmt.Fprintln(w, "Note: Hermes config is global and was not removed. Manually delete the gortex entry in ~/.hermes/config.yaml (+ profiles), the gortex pre_tool_call / pre_llm_call entries under its `hooks:` block, and the gortex / gortex-* skill directories under ~/.hermes/skills/ if desired.")
		return
	}

	fmt.Fprintln(w)
	stats := []string{progress.Stat(strconv.Itoa(removed), "removed", progress.StatGood)}
	if len(failures) > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(len(failures)), "failed", progress.StatBad))
	}
	fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render("uninstall complete"))
	fmt.Fprintln(w, "     "+progress.StatStrip(stats...))

	if len(failures) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "     "+progress.Heading("failures"))
		for _, f := range failures {
			fmt.Fprintln(w, "       "+progress.StyleErr.Render("✗")+"  "+progress.StyleVal.Render(f))
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "     "+progress.Heading("preserved"))
	if globalCleaned {
		fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render("non-Gortex content in every merged config (Gortex entries removed)"))
		fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render("gortex skills you had edited — a customised copy is never deleted"))
	} else {
		fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render("CLAUDE.md — remove the Gortex block manually if needed (or re-run with --global)"))
	}
	fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render(".kiro/ files without the 'gortex-' prefix"))
	fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render("your own skills in .agents/skills/, .opencode/skills/ and .github/skills/"))
	fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render("~/.gemini/antigravity/knowledge/gortex-workflow (global)"))
	fmt.Fprintln(w, "       "+progress.StyleHint.Render("·")+"  "+progress.StyleVal.Render("~/.hermes/config.yaml + skills (global)"))
	fmt.Fprintln(w)
}
