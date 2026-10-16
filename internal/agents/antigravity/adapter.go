package antigravity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

// Name identifies the Antigravity adapter for --agents=<name>.
const Name = "antigravity"

// DocsURL is the current public docs entry point for MCP in
// Antigravity. As of 2026 Antigravity supports both MCP server
// registration (at ~/.gemini/antigravity/mcp_config.json for the IDE
// and ~/.gemini/config/mcp_config.json for the agy CLI / Antigravity 2.0)
// and the older Knowledge Item mechanism. We write both MCP paths and
// the KI today: MCP for runtime tool access, KI for the in-editor
// instructions panel.
const DocsURL = "https://antigravity.google/docs/mcp"

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// Detect returns true whenever a Home directory is resolvable.
// Both artifacts are cheap to install and harmless on machines
// without Antigravity installed — a user who installs later picks
// up the config automatically. The Step 4 audit will tighten this
// to a proper install check once Antigravity ships a PATH-visible
// CLI.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	return env.Home != "", nil
}

func mcpConfigPaths(home string) []string {
	return []string{
		filepath.Join(home, ".gemini", "antigravity", "mcp_config.json"),
		filepath.Join(home, ".gemini", "config", "mcp_config.json"),
	}
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	if env.Home == "" {
		return &agents.Plan{}, nil
	}
	kiDir := filepath.Join(env.Home, ".gemini", "antigravity", "knowledge", "gortex-workflow")
	files := make([]agents.FileAction, 0, 5)
	for _, mcpPath := range mcpConfigPaths(env.Home) {
		files = append(files, agents.FileAction{
			Path:   mcpPath,
			Action: agents.ActionWouldMerge,
			Keys:   []string{"mcpServers"},
		})
	}
	files = append(files,
		agents.FileAction{Path: filepath.Join(env.Home, ".gemini", "settings.json"), Action: agents.ActionWouldMerge, Keys: []string{"hooks"}},
		agents.FileAction{Path: filepath.Join(kiDir, "metadata.json"), Action: agents.ActionWouldCreate},
		agents.FileAction{Path: filepath.Join(kiDir, "artifacts", "gortex-instructions.md"), Action: agents.ActionWouldCreate},
	)
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
		mcpAction, err := agents.MergeJSON(env.Stderr, mcpPath, func(root map[string]any, _ bool) (bool, error) {
			return agents.UpsertMCPServer(root, "gortex", agents.DefaultGortexMCPEntry(), opts), nil
		}, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, mcpAction)
	}

	// 1b. Lifecycle hooks — Antigravity reads Gemini CLI's
	//     ~/.gemini/settings.json hooks, so install SessionStart +
	//     AfterTool there (the handler is agent-agnostic). If the Gemini
	//     adapter already added a gortex hook, UpsertGeminiHooks is a
	//     no-op, so the two adapters never double-register.
	hooksPath := filepath.Join(env.Home, ".gemini", "settings.json")
	hooksAction, err := agents.MergeJSON(env.Stderr, hooksPath, func(root map[string]any, _ bool) (bool, error) {
		return agents.UpsertGeminiHooks(root, Name, opts), nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, hooksAction)

	// 2. Knowledge Item — kept as a secondary artifact. It makes the native
	//    MCP public workflow mandatory and treats absent callable handles as an
	//    integration failure. Exact shipped v0.60.0 content is migrated in place;
	//    any customized KI is preserved.
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

	res.Configured = true
	return res, nil
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
