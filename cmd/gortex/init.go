package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/aider"
	"github.com/zzet/gortex/internal/agents/antigravity"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/cline"
	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/agents/continuedev"
	"github.com/zzet/gortex/internal/agents/cursor"
	"github.com/zzet/gortex/internal/agents/gemini"
	"github.com/zzet/gortex/internal/agents/hermes"
	"github.com/zzet/gortex/internal/agents/kilocode"
	"github.com/zzet/gortex/internal/agents/ohmypi"
	"github.com/zzet/gortex/internal/agents/kiro"
	"github.com/zzet/gortex/internal/agents/openclaw"
	"github.com/zzet/gortex/internal/agents/opencode"
	"github.com/zzet/gortex/internal/agents/vscode"
	"github.com/zzet/gortex/internal/agents/windsurf"
	"github.com/zzet/gortex/internal/agents/zed"
	"github.com/zzet/gortex/internal/claudemd"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/query"
	genskills "github.com/zzet/gortex/internal/skills"
	"github.com/zzet/gortex/internal/telemetry"
	"github.com/zzet/gortex/internal/workspace"
)

// Per-flag globals. Behaviour flags stay separate from UX flags so
// the wizard and the orchestrator read them without cross-pollution.
var (
	// Core behaviour
	initAnalyze      bool
	initInstallHooks = true
	initNoHooks      bool
	initHooksOnly    bool
	initHookMode     string

	// Community skills generation (replaces the old `gortex skills`).
	initSkills          = true
	initNoSkills        bool
	initSkillsMinSize   int
	initSkillsMaxSkills int

	// Non-interactive / reporting knobs
	initYes         bool
	initInteractive bool
	initAgents      string
	initAgentsSkip  string
	initJSON        bool
	initDryRun      bool
	initForce       bool
)

var initCmd = &cobra.Command{
	Use:   "init [path]",
	Short: "Wire Gortex into the current repository for every detected AI coding assistant",
	Long: `Configure Gortex for this repository: per-repo MCP and instruction files for each
detected assistant, optional Claude Code hooks, community-derived routing, and (with --analyze)
a richer CLAUDE.md overview.

For one-time machine-wide setup (user MCP config, user skills /
Knowledge Items, user hooks), run ` + "`gortex install`" + ` once.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInit,
}

func init() {
	initCmd.Flags().BoolVar(&initAnalyze, "analyze", false, "index the repo to generate a richer CLAUDE.md with codebase overview")
	initCmd.Flags().BoolVar(&initInstallHooks, "hooks", true, "install Claude Code hooks (PreToolUse + PreCompact + Stop); use --no-hooks to skip")
	initCmd.Flags().BoolVar(&initNoHooks, "no-hooks", false, "skip installing Claude Code hooks (inverse of --hooks)")
	initCmd.Flags().BoolVar(&initHooksOnly, "hooks-only", false, "only install/update Claude Code hooks in .claude/settings.local.json, skip everything else")
	initCmd.Flags().StringVar(&initHookMode, "hook-mode", "deny",
		"hook posture: 'deny' (PreToolUse redirects Grep/Glob/Read of indexed source) or 'enrich' "+
			"(PreToolUse never denies; PostToolUse appends graph context after the tool runs)")

	initCmd.Flags().BoolVar(&initSkills, "skills", true, "generate per-community routing + SKILL.md files; use --no-skills to skip")
	initCmd.Flags().BoolVar(&initNoSkills, "no-skills", false, "skip community-skill generation (inverse of --skills)")
	initCmd.Flags().IntVar(&initSkillsMinSize, "skills-min-size", 3, "minimum community size to generate a skill")
	initCmd.Flags().IntVar(&initSkillsMaxSkills, "skills-max", 20, "maximum number of skills to generate")

	initCmd.Flags().BoolVarP(&initYes, "yes", "y", false, "skip the interactive wizard (implied when stdin is not a TTY)")
	initCmd.Flags().BoolVarP(&initInteractive, "interactive", "i", false, "force the full-screen wizard (banner + agent checklist + options panel + live dashboard) even when stdin would normally fall through to defaults")
	initCmd.Flags().StringVar(&initAgents, "agents", "", "comma-separated list of agents to configure ('auto' means every registered adapter)")
	initCmd.Flags().StringVar(&initAgentsSkip, "agents-skip", "", "comma-separated list of agents to skip (composable with --agents)")
	initCmd.Flags().BoolVar(&initJSON, "json", false, "emit a structured JSON report on stdout")
	initCmd.Flags().BoolVar(&initDryRun, "dry-run", false, "plan writes without modifying disk")
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite keys we would otherwise preserve during a merge")

	rootCmd.AddCommand(initCmd)
}

// buildRegistry wires up every registered adapter. Registration
// order is also execution order — Claude Code first (hooks-heavy),
// then Cursor (common MCP + project rules), then alphabetical for
// stable --json.
func buildRegistry() *agents.Registry {
	r := agents.NewRegistry()
	r.Register(claudecode.New())
	r.Register(cursor.New())
	r.Register(aider.New())
	r.Register(antigravity.New())
	r.Register(cline.New())
	r.Register(codex.New())
	r.Register(continuedev.New())
	r.Register(gemini.New())
	r.Register(hermes.New())
	r.Register(kilocode.New())
	r.Register(kiro.New())
	r.Register(ohmypi.New())
	r.Register(opencode.New())
	r.Register(openclaw.New())
	r.Register(vscode.New())
	r.Register(windsurf.New())
	r.Register(zed.New())
	return r
}

func runInit(cmd *cobra.Command, args []string) (err error) {
	root := "."
	if len(args) > 0 {
		root = args[0]
	}

	if initNoHooks {
		initInstallHooks = false
	}
	if initNoSkills {
		initSkills = false
	}

	// Interactive wizard. Two flavours:
	//   * --interactive (-i) or a TTY without --yes: launch the full
	//     bubbletea wizard (banner + agent checklist + options panel +
	//     confirm). The wizard owns every decision; if the user cancels
	//     we return without touching disk.
	//   * Non-TTY or --yes: fall through to the legacy bufio prompt for
	//     the single hooks question so CI scripts that piped a yes/no
	//     line into `gortex init` keep working.
	wantWizard := initInteractive || (!initYes && !initHooksOnly && progress.IsTTY(cmd.ErrOrStderr()) && isInteractive())
	if wantWizard {
		cancelled, err := runInitWizard(cmd, &cobraInitState{
			rootPath: root,
		})
		if err != nil {
			return err
		}
		if cancelled {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "  cancelled — no changes made.")
			return nil
		}
	} else if !initYes && !initHooksOnly && isInteractive() {
		hooksPreset := cmd.Flags().Changed("hooks") || cmd.Flags().Changed("no-hooks")
		if choice, ran := runInteractiveInit(os.Stdin, cmd.ErrOrStderr(), hooksPreset); ran {
			if !hooksPreset {
				initInstallHooks = choice.Hooks
			}
		}
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	// Bind this directory as a single-project entry point so the MCP
	// server can resolve it without --hooks-only setups, daemon-less
	// clients, or future runs needing manual setup. The marker is the
	// `.gortex/` directory itself (see internal/workspace.IndexDir);
	// nothing else needs to live inside it.
	if !initDryRun && !initHooksOnly {
		if err := ensureProjectMarker(absRoot, cmd.ErrOrStderr()); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init] warning: could not create %s: %v\n", workspace.IndexDir, err)
		}
	}

	// --hooks-only short-circuit: install/heal Claude Code hooks
	// and exit. Everything else is a no-op.
	if initHooksOnly {
		settingsPath := filepath.Join(absRoot, ".claude", "settings.local.json")
		if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
			return err
		}
		action, err := claudecode.InstallHookWithMode(cmd.ErrOrStderr(), settingsPath, initHookMode, agents.ApplyOpts{DryRun: initDryRun, Force: initForce})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init --hooks-only] %s %s\n", action.Action, action.Path)
		return nil
	}

	realStderr := cmd.ErrOrStderr()
	prog := selectInitProgress(realStderr)
	prog.Start("Initializing gortex")

	// Buffer chatty adapter logs while the animation is running. On success
	// drop them — the summary already conveys outcome. On failure replay
	// them so the user can debug. env.Stderr is captured AFTER the swap so
	// adapters write into the buffer too.
	var (
		chatter bytes.Buffer
		results []*agents.Result
		opts    agents.ApplyOpts
	)
	captured := prog.Enabled()
	if captured {
		cmd.SetErr(&chatter)
	}

	home, _ := os.UserHomeDir()
	env := agents.Env{
		Root:         absRoot,
		Home:         home,
		HookCommand:  claudecode.ResolveHookCommand(cmd.ErrOrStderr()),
		Mode:         agents.ModeProject,
		InstallHooks: initInstallHooks,
		HookMode:     initHookMode,
		AnalyzeRepo:  initAnalyze,
		Stderr:       cmd.ErrOrStderr(),
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
			emitHumanSummary(realStderr, results, opts)
		}
	}()

	// Indexing powers both --analyze (codebase overview in
	// CLAUDE.md) and --skills (community routing in every per-repo
	// instructions surface). Index once, feed both.
	needIndex := initAnalyze || initSkills
	if needIndex {
		prog.Stage(stageIndex, absRoot)
		ctx := progress.WithReporter(context.Background(), prog.Reporter())
		// Silence zap info logs from the indexer when the surface is live;
		// the dashboard / spinner already shows the same stage transitions.
		var idxLogger *zap.Logger
		if prog.Enabled() {
			idxLogger = zap.NewNop()
		}
		g, idxErr := indexRepoForInit(ctx, absRoot, idxLogger)
		if idxErr != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init] indexing failed: %v — proceeding without analysis/skills\n", idxErr)
		} else {
			prog.StageDone(stageIndex, "")
			if initAnalyze {
				prog.Stage(stageAnalyze, "")
				eng := query.NewEngine(g)
				env.AnalyzedOverview = claudemd.Generate(eng, 180)
				prog.StageDone(stageAnalyze, "")
			}
			if initSkills {
				prog.Stage(stageSkills, "")
				generated, routing := genskills.Build(g, genskills.BuildOpts{
					MinSize:   initSkillsMinSize,
					MaxSkills: initSkillsMaxSkills,
				})
				if len(generated) > 0 {
					env.GeneratedSkills = toEnvSkills(generated)
					env.SkillsRouting = routing
					prog.StageDone(stageSkills, fmt.Sprintf("%d community skill(s)", len(generated)))
				} else {
					prog.StageDone(stageSkills, fmt.Sprintf("no communities large enough (min-size: %d)", initSkillsMinSize))
				}
			}
		}
	}

	prog.Stage(stageAdapters, "")
	registry := buildRegistry()
	selected, err := registry.Filter(initAgents, initAgentsSkip)
	if err != nil {
		return err
	}

	opts = agents.ApplyOpts{DryRun: initDryRun, Force: initForce}
	results = make([]*agents.Result, 0, len(selected))
	for _, a := range selected {
		prog.Sub(a.Name())
		r, applyErr := a.Apply(env, opts)
		if applyErr != nil {
			if a.Name() == claudecode.Name {
				return fmt.Errorf("%s: %w", a.Name(), applyErr)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init] warning: %s setup failed: %v\n", a.Name(), applyErr)
		}
		if r != nil {
			results = append(results, r)
		}
	}
	prog.StageDone(stageAdapters, fmt.Sprintf("%d adapter(s) configured", len(results)))

	// Always update Gortex's own global config so the daemon picks
	// up this repo next time it starts (harmless when no daemon).
	if !initDryRun {
		if err := ensureGlobalConfig(absRoot); err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[gortex init] warning: could not update global config: %v\n", err)
		}
	}

	// One-time opt-in telemetry notice. Telemetry is off by default; this
	// only informs and records the default choice so it shows at most once.
	// Goes to stderr, so it never pollutes --json output. Skipped on dry run.
	if !initDryRun {
		telemetry.MaybeFirstRunNotice(platform.TelemetryDir(), cmd.ErrOrStderr())
	}

	if initJSON {
		if err := emitJSONReport(cmd.OutOrStdout(), results, opts); err != nil {
			return err
		}
	}
	return nil
}

// toEnvSkills converts the skills generator's output into the
// agents.GeneratedSkill payload carried on Env. The two shapes are
// identical today; the mirror keeps the agents package free of the
// internal/skills dependency.
func toEnvSkills(src []genskills.GeneratedSkill) []agents.GeneratedSkill {
	out := make([]agents.GeneratedSkill, len(src))
	for i, s := range src {
		out[i] = agents.GeneratedSkill{
			CommunityID: s.CommunityID,
			Label:       s.Label,
			DirName:     s.DirName,
			Content:     s.Content,
		}
	}
	return out
}

// indexRepoForInit runs a one-shot index of the repo. Kept inside
// cmd/gortex (not an adapter) because the indexer pulls in many
// gortex-internal packages we'd rather not leak into internal/agents.
// The ctx carries a progress.Reporter so the caller's spinner picks up
// stage transitions ("walking files", "parsing", …) as sub-status.
// Pass a Nop logger when running under an animated spinner so structured
// info logs don't duplicate the mesh frame.
func indexRepoForInit(ctx context.Context, root string, logger *zap.Logger) (*graph.Graph, error) {
	if logger == nil {
		logger = newLogger()
	}
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load("")
	if err != nil {
		cfg = &config.Config{}
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	idx := indexer.New(g, reg, cfg.Index, logger)
	if _, err := idx.IndexCtx(ctx, root); err != nil {
		return nil, err
	}
	return g, nil
}

// emitJSONReport writes a single JSON object to w. Shape kept
// compatible with earlier releases (agents array, dry_run, force)
// plus a mode discriminator so the install/init outputs are
// distinguishable.
func emitJSONReport(w io.Writer, results []*agents.Result, opts agents.ApplyOpts) error {
	payload := map[string]any{
		"dry_run": opts.DryRun,
		"force":   opts.Force,
		"mode":    "project",
		"agents":  results,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// emitHumanSummary prints the per-agent file counts to stderr.
func emitHumanSummary(w io.Writer, results []*agents.Result, opts agents.ApplyOpts) {
	emitAgentSummary(w, results, opts, []string{
		"if your editor uses MCP, enable the gortex server there (and reload the window) when tools do not appear after the first init",
		"commit the generated files your team relies on (.mcp.json, .claude/, .cursor/, CLAUDE.md, and other adapter outputs)",
		"run `gortex install` once per machine to wire user-level integration",
	})
}

// ensureProjectMarker creates `.gortex/` at the repo root to hold the
// project's Gortex config (`.gortex.yaml` and friends). Idempotent: a
// no-op if the directory already exists. Reports first-time creation to
// stderr.
func ensureProjectMarker(root string, w io.Writer) error {
	dir := filepath.Join(root, workspace.IndexDir)
	existed := true
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		existed = false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Keep the project's local index state out of git — write (or heal) a
	// self-scoping .gitignore inside the marker dir so a user never commits it.
	if _, err := workspace.EnsureIndexDirGitignore(dir); err != nil {
		return err
	}
	if !existed {
		_, _ = fmt.Fprintf(w, "[gortex init] created %s/ to hold this project's Gortex config\n", workspace.IndexDir)
	}
	return nil
}

// ensureGlobalConfig adds this repo to ~/.gortex/config.yaml
// so the daemon picks it up on its next restart. Skipped in --dry-run.
func ensureGlobalConfig(root string) error {
	gc, err := config.LoadGlobal()
	if err != nil {
		return err
	}
	if err := gc.AddRepo(config.RepoEntry{Path: root}); err != nil {
		return err
	}
	if err := gc.Save(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stderr, "[gortex init] updated global config at %s\n", gc.ConfigPath())
	return nil
}
