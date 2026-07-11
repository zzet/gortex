package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/telemetry"
)

// Install-only flags. Repo-local `gortex init` has its own set —
// kept separate to avoid cross-pollination when wiring from the
// interactive wizard or the --json reporters.
var (
	installYes             bool
	installInteractive     bool
	installAgents          string
	installPrintConfig     string
	installAgentsSkip      string
	installJSON            bool
	installDryRun          bool
	installForce           bool
	installHooks           = true
	installNoHooks         bool
	installHookMode        string
	installCodexHookMode   string
	installClaudeMd        = true
	installNoClaudeMd      bool
	installClaudeConfigDir string
	installStartDaemon     bool
	installTrackRepo       bool
	installTrackPath       string
	installTelemetry       bool
	installNoTelemetry     bool
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install machine-wide Gortex integration for every detected AI assistant",
	Long: `Install Gortex at the user level: user-wide MCP config, user-level
skills / Knowledge Items, and (optionally) user-level hooks.

Run once per machine. Per-repo setup is done by ` + "`gortex init`" + ` inside
each repository.`,
	Args: cobra.NoArgs,
	RunE: runInstall,
}

func init() {
	installCmd.Flags().BoolVarP(&installYes, "yes", "y", false, "skip any interactive prompts (implied when stdin is not a TTY)")
	installCmd.Flags().BoolVarP(&installInteractive, "interactive", "i", false, "force the full-screen install wizard (banner + agent checklist + options panel + live dashboard)")
	installCmd.Flags().StringVar(&installAgents, "agents", "", "comma-separated list of agents to configure ('auto' means every registered adapter)")
	installCmd.Flags().StringVar(&installAgentsSkip, "agents-skip", "", "comma-separated list of agents to skip (composable with --agents)")
	installCmd.Flags().BoolVar(&installJSON, "json", false, "emit a structured JSON report on stdout (banner still goes to stderr)")
	installCmd.Flags().BoolVar(&installDryRun, "dry-run", false, "plan writes without modifying disk")
	installCmd.Flags().BoolVar(&installForce, "force", false, "overwrite keys we would otherwise preserve during a merge")
	installCmd.Flags().BoolVar(&installHooks, "hooks", true, "install user-level hooks for supported agents; use --no-hooks to skip")
	installCmd.Flags().BoolVar(&installNoHooks, "no-hooks", false, "skip user-level hooks for supported agents (inverse of --hooks)")
	installCmd.Flags().StringVar(&installHookMode, "hook-mode", "deny",
		"hook posture: 'deny' (PreToolUse redirects Grep/Glob/Read of indexed source), 'enrich' "+
			"(PreToolUse never denies; PostToolUse appends graph context after the tool runs — easier onboarding), "+
			"'consult-unlock' (deny fallback reads until the Gortex graph is queried once this session, then downgrade to soft context), "+
			"or 'nudge' (soft-deny once per burst of consecutive non-symbolic calls, then let the next call proceed)")
	installCmd.Flags().StringVar(&installCodexHookMode, "codex-hook-mode", "enrich",
		"Codex hook posture: 'enrich' (default, advisory only), 'deny', 'consult-unlock', or 'nudge'")
	installCmd.Flags().BoolVar(&installClaudeMd, "claude-md", true, "merge Gortex rule block into ~/.claude/CLAUDE.md; use --no-claude-md to skip")
	installCmd.Flags().BoolVar(&installNoClaudeMd, "no-claude-md", false, "skip the ~/.claude/CLAUDE.md rule block (inverse of --claude-md)")
	installCmd.Flags().StringVar(&installClaudeConfigDir, "claude-config-dir", "", "Claude Code config root to write into (skills/commands/agents/settings/CLAUDE.md/.claude.json); overrides $CLAUDE_CONFIG_DIR, defaults to ~/.claude. Useful for installing into a non-active profile or CI sandbox")
	installCmd.Flags().StringVar(&installClaudeConfigDir, "config-root", "", "alias for --claude-config-dir")
	installCmd.Flags().BoolVar(&installStartDaemon, "start", false, "start the daemon immediately after setup (detached)")
	installCmd.Flags().BoolVar(&installTrackRepo, "track", false, "track a repository with the daemon after setup")
	installCmd.Flags().StringVar(&installTrackPath, "track-path", ".", "repository to track when --track is set (default: current directory)")
	installCmd.Flags().BoolVar(&installTelemetry, "telemetry", false, "opt in to anonymous usage telemetry (tool/command counts only; off by default)")
	installCmd.Flags().BoolVar(&installNoTelemetry, "no-telemetry", false, "explicitly opt out of anonymous usage telemetry")
	installCmd.Flags().StringVar(&installPrintConfig, "print-config", "", "print the config a single agent would install (JSON, zero writes) and exit")

	rootCmd.AddCommand(installCmd)
}

func runInstall(cmd *cobra.Command, _ []string) (err error) {
	if installNoHooks {
		installHooks = false
	}
	if installNoClaudeMd {
		installClaudeMd = false
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	if home == "" {
		return fmt.Errorf("gortex install needs a home directory; $HOME is empty")
	}

	// An explicit --claude-config-dir / --config-root pins the Claude
	// Code config root for every adapter write below (and the banner),
	// overriding $CLAUDE_CONFIG_DIR. Resolve to an absolute path so a
	// relative flag doesn't depend on the daemon's later cwd.
	if installClaudeConfigDir != "" {
		abs, err := filepath.Abs(installClaudeConfigDir)
		if err != nil {
			return fmt.Errorf("resolve --claude-config-dir: %w", err)
		}
		claudecode.SetConfigDirOverride(abs)
	}

	// --print-config <agent>: a zero-write dry-run of one adapter's planned
	// config, as JSON, then exit. Useful for "what would `gortex install`
	// write for cursor?" without touching disk.
	if installPrintConfig != "" {
		root, _ := filepath.Abs(".")
		return agents.PrintConfig(cmd.OutOrStdout(), buildRegistry(), installPrintConfig,
			agents.Env{Root: root, Home: home, Mode: agents.ModeGlobal})
	}

	// Machine-global telemetry choice. An explicit --telemetry / --no-telemetry
	// flag wins; otherwise seed the wizard toggle from the PERSISTED choice
	// only (never the env-folded resolution), so a transient GORTEX_TELEMETRY /
	// DO_NOT_TRACK in this shell can't be laundered into a durable opt-in or
	// silently flip a saved one. Unset persisted choice → default off.
	telemetryFlagSet := cmd.Flags().Changed("telemetry") || cmd.Flags().Changed("no-telemetry")
	if installNoTelemetry {
		installTelemetry = false
	}
	if !telemetryFlagSet {
		if persisted := telemetry.LoadConsentConfig(platform.TelemetryDir()); persisted.Enabled != nil {
			installTelemetry = *persisted.Enabled
		}
	}

	// Interactive wizard — same shape as `gortex init -i`, just sourced from
	// the install* globals and writing under Home instead of Root. The
	// wizard mutates the install* globals (agents list, hooks, hook-mode) so
	// the orchestration below sees them as if they came from the flag set.
	wantWizard := installInteractive || (!installYes && progress.IsTTY(cmd.ErrOrStderr()) && isInteractive())
	if wantWizard {
		cancelled, werr := runInstallWizard(cmd, home)
		if werr != nil {
			return werr
		}
		if cancelled {
			fmt.Fprintln(cmd.ErrOrStderr(), "  cancelled — no changes made.")
			return nil
		}
	}

	// Persist the telemetry choice when the user expressed one — an explicit
	// flag, or the interactive wizard (whose toggle wrote back into
	// installTelemetry). A non-interactive run with no flag leaves the opt-in
	// default (off) and the first-run notice untouched.
	if (telemetryFlagSet || wantWizard) && !installDryRun {
		if serr := telemetry.SaveConsent(platform.TelemetryDir(), installTelemetry, "installer", time.Now); serr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "[gortex install] warning: could not save telemetry choice: %v\n", serr)
		}
	}

	realStderr := cmd.ErrOrStderr()
	prog := selectInstallProgress(realStderr)
	prog.Start("Installing gortex")
	if installClaudeMd && !installDryRun {
		prog.Sub(fmt.Sprintf("merging %s (use --no-claude-md to skip)",
			claudecode.UserClaudeMdPath(home)))
	}

	// Buffer chatty adapter logs while the animation is running. On success
	// we drop them entirely — the summary already conveys outcome. On
	// failure we replay them so the user can debug.
	var (
		chatter bytes.Buffer
		results []*agents.Result
		opts    agents.ApplyOpts
	)
	captured := prog.Enabled()
	if captured {
		cmd.SetErr(&chatter)
	}
	defer func() {
		if err != nil {
			prog.Fail(err)
		} else {
			prog.Done()
		}
		if captured {
			cmd.SetErr(realStderr)
			if err != nil {
				_, _ = io.Copy(realStderr, &chatter)
			}
		}
		if err == nil {
			emitInstallHuman(realStderr, results, opts)
		}
	}()

	env := agents.Env{
		// Root is still set so adapters that write to the daemon
		// config use the right cwd for "this is where I was run
		// from", but no per-repo files get written in ModeGlobal.
		Root:                      mustAbs(installTrackPath),
		Home:                      home,
		HookCommand:               claudecode.ResolveHookCommand(cmd.ErrOrStderr()),
		Mode:                      agents.ModeGlobal,
		InstallHooks:              installHooks,
		HookMode:                  installHookMode,
		CodexHookMode:             installCodexHookMode,
		InstallGlobalInstructions: installClaudeMd,
		Stderr:                    cmd.ErrOrStderr(),
	}

	registry := buildRegistry()
	selected, err := registry.Filter(installAgents, installAgentsSkip)
	if err != nil {
		return err
	}

	opts = agents.ApplyOpts{DryRun: installDryRun, Force: installForce}
	results = make([]*agents.Result, 0, len(selected))
	prog.Stage(stageAdapters, fmt.Sprintf("%d adapter(s)", len(selected)))
	for _, a := range selected {
		prog.Sub(a.Name())
		r, applyErr := a.Apply(env, opts)
		if applyErr != nil {
			// Claude Code is load-bearing — propagate. Other
			// adapters warn so one broken editor doesn't abort.
			if a.Name() == claudecode.Name {
				return fmt.Errorf("%s: %w", a.Name(), applyErr)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "[gortex install] warning: %s setup failed: %v\n", a.Name(), applyErr)
		}
		if r != nil {
			results = append(results, r)
		}
	}
	prog.StageDone(stageAdapters, fmt.Sprintf("%d adapter(s) configured", len(results)))

	// Ensure the daemon config file exists so --track doesn't
	// fail with "no config" on first use.
	if !installDryRun {
		if err := ensureGlobalConfigExists(); err != nil {
			return err
		}
		if err := runInstallFollowUps(cmd); err != nil {
			return err
		}
	}

	// Opt-in install telemetry: one bounded event per configured adapter. The
	// user's just-saved telemetry choice is re-resolved, so an install that
	// opted in is counted and an opt-out records nothing.
	if !installDryRun {
		recordInstallTelemetry(results)
	}

	if installJSON {
		if err := emitInstallJSON(cmd.OutOrStdout(), results, opts); err != nil {
			return err
		}
	}
	return nil
}

// recordInstallTelemetry records one install event per configured adapter,
// dimensioned by the adapter name. Consent-gated and fail-silent: a disabled
// recorder opens nothing.
func recordInstallTelemetry(results []*agents.Result) {
	consent := telemetry.ResolveConsent(telemetry.LoadConsentConfig(platform.TelemetryDir()), os.Getenv)
	if !consent.Enabled {
		return
	}
	rec := telemetry.NewRecorder(consent, telemetry.NewStore(platform.TelemetryDir()))
	for _, r := range results {
		if r == nil || !r.Configured {
			continue
		}
		telemetry.RecordInstall(rec, "install", r.Name)
	}
	rec.Flush()
}

// runInstallFollowUps runs the daemon-side post-setup operations
// that don't fit the Adapter interface: `--start` spawns the daemon
// detached, `--track` registers installTrackPath with the running
// daemon.
func runInstallFollowUps(cmd *cobra.Command) error {
	w := cmd.ErrOrStderr()

	if installStartDaemon {
		if daemon.IsRunning() {
			fmt.Fprintln(w, "[gortex install] daemon already running (skipped --start)")
		} else {
			if err := spawnDetachedDaemon(); err != nil {
				return fmt.Errorf("start daemon: %w", err)
			}
			fmt.Fprintln(w, "[gortex install] daemon started (detached)")
		}
	}

	if installTrackRepo {
		// Normalise the volume for this NEW tracked path so a Windows drive
		// letter converges with os.Getwd's upper-case convention before the
		// daemon stores it. No-op on POSIX; never touches the basename.
		abs := pathkey.NormalizeVolume(mustAbs(installTrackPath))
		if !daemon.IsRunning() {
			fmt.Fprintln(w, "[gortex install] ⚠ skipping --track: daemon is not running (try `gortex daemon start`)")
		} else {
			resp, err := trackViaDaemon(abs)
			if err != nil {
				return fmt.Errorf("track %s: %w", abs, err)
			}
			fmt.Fprintf(w, "[gortex install] tracked %s (%s)\n", abs, resp)
		}
	}

	if !installStartDaemon {
		fmt.Fprintln(w, "\nNext: start the daemon → `gortex daemon start --detach`")
	}
	if !installTrackRepo && installStartDaemon {
		fmt.Fprintln(w, "Then: track a repo → `cd <repo> && gortex init` (or `gortex track <path>`)")
	}
	return nil
}

func emitInstallJSON(w interface{ Write([]byte) (int, error) }, results []*agents.Result, opts agents.ApplyOpts) error {
	payload := map[string]any{
		"dry_run": opts.DryRun,
		"force":   opts.Force,
		"mode":    "global",
		"agents":  results,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func emitInstallHuman(w io.Writer, results []*agents.Result, opts agents.ApplyOpts) {
	emitAgentSummary(w, results, opts, []string{
		"cd into any repo and run `gortex init` to wire repo-local config for every detected assistant (MCP UIs differ by editor)",
	})
}

func mustAbs(p string) string {
	if p == "" {
		p = "."
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
