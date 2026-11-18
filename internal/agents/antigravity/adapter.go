package antigravity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

// Name identifies the Antigravity adapter for --agents=<name>.
const Name = "antigravity"

// DocsURL is the current public docs entry point for MCP in
// Antigravity. As of 2026 Antigravity supports MCP server registration
// (at ~/.gemini/antigravity/mcp_config.json for the IDE and
// ~/.gemini/config/mcp_config.json for the agy CLI / Antigravity 2.0),
// native lifecycle hooks (~/.gemini/config/hooks.json), Agent Skills
// (~/.gemini/config/skills/ and .agents/skills/), global plugin rules
// (~/.gemini/config/plugins/gortex/rules/AGENTS.md), and the older IDE
// Knowledge Item mechanism.
const DocsURL = "https://antigravity.google/docs/mcp"

const globalPluginManifestJSON = "{\n  \"name\": \"gortex\"\n}\n"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// WritesSkillFiles reports that this adapter installs generated
// community skills under <root>/.agents/skills.
func (a *Adapter) WritesSkillFiles() bool { return true }

// WritesCommunitiesRouting reports that this adapter merges the
// communities routing block into its instruction file (project mode).
func (a *Adapter) WritesCommunitiesRouting() bool { return true }

// CommunitiesRoutingPath reports the instruction file that carries the
// communities block in project mode. Antigravity walks up from CWD to
// the repo root loading AGENTS.md (and GEMINI.md fallback); targeting
// AGENTS.md converges with Claude Code, Codex, and OpenCode on a single block.
// An existing GEMINI.md is used only when AGENTS.md is absent, matching the
// fallback order — a block in GEMINI.md beside an AGENTS.md would go unread.
func (a *Adapter) CommunitiesRoutingPath(env agents.Env) string {
	agentsPath := filepath.Join(env.Root, "AGENTS.md")
	if env.Root != "" {
		if _, err := os.Stat(agentsPath); err != nil {
			geminiPath := filepath.Join(env.Root, "GEMINI.md")
			if _, err := os.Stat(geminiPath); err == nil {
				return geminiPath
			}
		}
	}
	return agentsPath
}

var lookAntigravityBinary = func() (string, error) {
	if p, err := exec.LookPath("agy"); err == nil && p != "" {
		return p, nil
	}
	return exec.LookPath("antigravity")
}

// Detect returns true when an Antigravity binary (agy / antigravity) is on
// PATH, the project root has a .antigravity directory, or the home directory
// holds state that Antigravity itself writes.
//
// The existence of ~/.gemini/antigravity or ~/.gemini/config proves nothing:
// earlier Gortex releases configured this adapter on every machine and
// created both (the Knowledge Item and the MCP stanzas). Treating them as an
// install would hand every upgrader hooks no host ever runs, which doctor
// then reports as a blocker.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := lookAntigravityBinary(); err == nil && p != "" {
		return true, nil
	}
	if env.Root != "" {
		if _, err := os.Stat(filepath.Join(env.Root, ".antigravity")); err == nil {
			return true, nil
		}
	}
	return env.Home != "" && hostStatePresent(env.Home), nil
}

// hostStatePresent reports whether home carries state written by Antigravity
// rather than by Gortex: the agy CLI home, agy's own config.json, or anything
// in the IDE's ~/.gemini/antigravity beyond the files Gortex writes there.
func hostStatePresent(home string) bool {
	for _, p := range []string{
		filepath.Join(home, ".gemini", "antigravity-cli"),
		filepath.Join(home, ".gemini", "config", "config.json"),
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	ideHome := filepath.Join(home, ".gemini", "antigravity")
	if hasEntriesBesides(ideHome, "knowledge", "mcp_config.json") {
		return true
	}
	return hasEntriesBesides(filepath.Join(ideHome, "knowledge"), "gortex-workflow")
}

// hasEntriesBesides reports whether dir exists and lists an entry not named
// in owned.
func hasEntriesBesides(dir string, owned ...string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !slices.Contains(owned, e.Name()) {
			return true
		}
	}
	return false
}

func mcpConfigPaths(home string) []string {
	return []string{
		filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		filepath.Join(home, ".gemini", "config", "mcp_config.json"),
	}
}

// GlobalPluginManifestPath returns the plugin.json path that registers
// Gortex's global rules bundle in ~/.gemini/config/plugins/gortex/.
func GlobalPluginManifestPath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "config", "plugins", "gortex", "plugin.json")
}

// GlobalInstructionsPath returns the user-level rules file Antigravity
// loads from Gortex's global plugin (~/.gemini/config/plugins/gortex/rules/AGENTS.md).
func GlobalInstructionsPath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "config", "plugins", "gortex", "rules", "AGENTS.md")
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	if env.Home == "" {
		return &agents.Plan{}, nil
	}
	kiDir := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	files := make([]agents.FileAction, 0, 8)
	for _, mcpPath := range mcpConfigPaths(env.Home) {
		files = append(files, agents.FileAction{
			Path:   mcpPath,
			Action: agents.ActionWouldMerge,
			Keys:   []string{"mcpServers"},
		})
	}
	files = append(files,
		agents.FileAction{Path: filepath.Join(kiDir, "metadata.json"), Action: agents.ActionWouldCreate},
		agents.FileAction{Path: filepath.Join(kiDir, "artifacts", "gortex-instructions.md"), Action: agents.ActionWouldCreate},
	)
	if env.Mode == agents.ModeGlobal {
		if env.InstallHooks {
			files = append(files, agents.FileAction{
				Path:   HookConfigPath(env.Home),
				Action: agents.ActionWouldMerge,
				Keys:   []string{"hooks"},
			})
		}
		if env.InstallGlobalInstructions {
			files = append(files,
				agents.FileAction{Path: GlobalPluginManifestPath(env.Home), Action: agents.ActionWouldCreate},
				agents.FileAction{Path: GlobalInstructionsPath(env.Home), Action: agents.ActionWouldMerge, Keys: []string{"gortex-rules-block"}},
			)
		}
		for _, id := range CuratedSkillNames() {
			if path := curatedSkillPath(env.Home, id); path != "" {
				files = append(files, agents.FileAction{
					Path: path, Action: agents.ActionWouldCreate, Keys: []string{"skill"},
				})
			}
		}
	} else {
		if env.SkillsRouting != "" && env.Root != "" {
			files = append(files, agents.FileAction{
				Path:   a.CommunitiesRoutingPath(env),
				Action: agents.ActionWouldMerge,
				Keys:   []string{"communities-block"},
			})
		}
		if env.Root != "" {
			for _, s := range env.GeneratedSkills {
				if !skillpack.ValidID(s.DirName) {
					continue
				}
				files = append(files, agents.FileAction{
					Path: generatedSkillPath(env.Root, s.DirName), Action: agents.ActionWouldCreate,
					Keys: []string{"skill"},
				})
			}
		}
	}
	return &agents.Plan{Files: files}, nil
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		return res, nil
	}
	if env.Home == "" {
		return res, fmt.Errorf("antigravity: requires a resolved home directory")
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up Antigravity integration...")

	// 1. Native MCP registration — Antigravity IDE reads
	//    ~/.gemini/antigravity/mcp_config.json while Antigravity CLI (agy) /
	//    Antigravity 2.0 reads ~/.gemini/config/mcp_config.json.
	for _, mcpPath := range mcpConfigPaths(env.Home) {
		mcpAction, err := agents.MergeJSONC(env.Stderr, mcpPath, func(root map[string]any, _ bool) (bool, error) {
			return agents.UpsertMCPServerWithMigration(root, "gortex", agents.DefaultGortexMCPEntry(), opts), nil
		}, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, mcpAction)
	}

	// 1b. Lifecycle hooks — Antigravity reads ~/.gemini/config/hooks.json
	//     (where top-level keys are hook names mapping to SessionStart,
	//     PreInvocation, PreToolUse, PostToolUse, and Stop handlers).
	//     Only installed in ModeGlobal (never in project mode / gortex init).
	if env.Mode == agents.ModeGlobal && env.InstallHooks {
		hooksPath := HookConfigPath(env.Home)
		hooksAction, err := agents.MergeJSONC(env.Stderr, hooksPath, func(root map[string]any, _ bool) (bool, error) {
			return upsertAntigravityHooks(root, env, opts), nil
		}, opts)
		if err != nil {
			return res, err
		}
		if hooksAction.Action != agents.ActionSkip {
			hooksAction.Keys = []string{"hooks"}
		}
		res.Files = append(res.Files, hooksAction)
	}

	// 2. Knowledge Item — kept as a secondary artifact for the IDE's
	//    in-editor knowledge panel. Exact shipped v0.60.0 content is
	//    migrated in place; any customized KI is preserved.
	kiDir := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")

	metaAction, err := writeKnowledgeItem(env.Stderr, filepath.Join(kiDir, "metadata.json"), Metadata, v060Metadata, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, metaAction)

	instrAction, err := writeKnowledgeItem(env.Stderr, filepath.Join(kiDir, "artifacts", "gortex-instructions.md"), Instructions, v060Instructions, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, instrAction)

	if env.Mode == agents.ModeGlobal {
		if env.InstallGlobalInstructions {
			manifestAction, err := agents.WriteOwnedFile(env.Stderr, GlobalPluginManifestPath(env.Home), globalPluginManifestJSON, opts)
			if err != nil {
				return res, fmt.Errorf("antigravity plugin manifest: %w", err)
			}
			res.Files = append(res.Files, manifestAction)

			insAction, err := upsertGlobalInstructions(env, opts)
			if err != nil {
				return res, fmt.Errorf("antigravity global instructions: %w", err)
			}
			res.Files = append(res.Files, insAction)
		}
		res.Files = append(res.Files, installCuratedSkills(env, opts)...)
	} else {
		if env.SkillsRouting != "" && env.Root != "" {
			routingPath := a.CommunitiesRoutingPath(env)
			routingAction, err := agents.UpsertMarkedBlock(env.Stderr, routingPath, env.SkillsRouting,
				agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
			if err != nil {
				return res, err
			}
			res.Files = append(res.Files, routingAction)
		}
		if env.Root != "" {
			res.Files = append(res.Files, installGeneratedSkills(env, opts)...)
		}
	}

	res.Configured = true
	return res, nil
}

func upsertGlobalInstructions(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	path := GlobalInstructionsPath(env.Home)
	action, err := agents.UpsertMarkedBlock(nil, path, agents.GlobalInlineBody(agents.InstructionsDir(env)),
		agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
	if err != nil {
		return agents.FileAction{}, err
	}
	if action.Keys != nil {
		action.Keys = []string{"gortex-rules-block"}
	}
	if !opts.DryRun && action.Action != agents.ActionSkip {
		internalutil.Logf(env.Stderr, "[gortex install] wrote rule block to %s", path)
	}
	return action, nil
}

// writeKnowledgeItem creates a missing artifact or replaces it only when its
// bytes match a Gortex-shipped migration fingerprint. A different existing file is
// user-authored policy and is never overwritten, including under --force.
func writeKnowledgeItem(w io.Writer, path, current, migration string, opts agents.ApplyOpts) (agents.FileAction, error) {
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return agents.WriteIfNotExists(w, path, current, opts)
	}
	if err != nil {
		return agents.FileAction{}, fmt.Errorf("read %s: %w", path, err)
	}

	switch string(existing) {
	case current:
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "unchanged"}, nil
	case migration:
		return agents.WriteOwnedFile(w, path, current, opts)
	default:
		internalutil.Logf(w, "[gortex init] skip %s (customized Knowledge Item)", path)
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "customized"}, nil
	}
}
