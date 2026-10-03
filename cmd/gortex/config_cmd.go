package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/excludes"
)

// Flags are shared by add/remove. Cobra parses them once per command,
// so declaring package-level vars is fine here.
var (
	excludeGlobalFlag bool
	excludeRepoFlag   string
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage Gortex configuration",
}

var configExcludeCmd = &cobra.Command{
	Use:   "exclude",
	Short: "Manage the effective ignore list (indexing + watching)",
	Long: `Add, list, or remove patterns from the effective ignore list used by both
indexing and watching. Patterns follow .gitignore semantics.

Targets (in precedence order; later layers override earlier):
  1. Builtin baseline (read-only)
  2. Global   - ~/.gortex/config.yaml              (--global)
  3. Repo    - GlobalConfig.repos[].exclude        (--repo <name>)
  4. Workspace - ./.gortex.yaml at the repo root   (default)

Use "!pattern" in a later layer to re-include something an earlier layer
excluded.`,
}

var configExcludeAddCmd = &cobra.Command{
	Use:   "add <path-or-pattern>",
	Short: "Append a pattern to the selected target",
	Long: `Appends a pattern. If the argument is an existing directory, it is
normalised to a repo-root-relative gitignore pattern (trailing slash).
Raw glob patterns (containing *, ?, [, leading / or !) are written
verbatim.`,
	Args: cobra.ExactArgs(1),
	RunE: runConfigExcludeAdd,
}

var configExcludeListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show the effective ignore list with source annotations",
	RunE:  runConfigExcludeList,
}

var configExcludeRemoveCmd = &cobra.Command{
	Use:   "remove <path-or-pattern>",
	Short: "Remove a pattern from the selected target",
	Args:  cobra.ExactArgs(1),
	RunE:  runConfigExcludeRemove,
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configExcludeCmd)
	configExcludeCmd.AddCommand(configExcludeAddCmd)
	configExcludeCmd.AddCommand(configExcludeListCmd)
	configExcludeCmd.AddCommand(configExcludeRemoveCmd)

	for _, c := range []*cobra.Command{configExcludeAddCmd, configExcludeRemoveCmd} {
		c.Flags().BoolVar(&excludeGlobalFlag, "global", false,
			"write to ~/.gortex/config.yaml (GlobalConfig.exclude)")
		c.Flags().StringVar(&excludeRepoFlag, "repo", "",
			"write to the named RepoEntry in the global config")
	}
}

// --- Target abstraction ---

// excludeTarget encapsulates load/save for one of the three writable
// exclude locations. repoRoot is set for workspace targets (used to
// anchor path normalisation); it's empty for global and repo-entry
// targets where patterns are not path-anchored.
type excludeTarget struct {
	label    string
	repoRoot string // "" for non-workspace targets

	load func() ([]string, error)
	save func(patterns []string) error
}

func resolveTarget() (*excludeTarget, error) {
	if excludeGlobalFlag && excludeRepoFlag != "" {
		return nil, fmt.Errorf("--global and --repo are mutually exclusive")
	}
	switch {
	case excludeGlobalFlag:
		return newGlobalTarget()
	case excludeRepoFlag != "":
		return newRepoEntryTarget(excludeRepoFlag)
	default:
		return newWorkspaceTarget()
	}
}

func newWorkspaceTarget() (*excludeTarget, error) {
	path, root, err := findWorkspaceConfigPath()
	if err != nil {
		return nil, err
	}
	return &excludeTarget{
		label:    "workspace " + path,
		repoRoot: root,
		load: func() ([]string, error) {
			cfg, err := readWorkspaceYAML(path)
			if err != nil {
				return nil, err
			}
			if cfg == nil {
				return nil, nil
			}
			// Fold legacy keys so `list` and `remove` see what the old
			// file actually contributed.
			patterns := append([]string{}, cfg.Exclude...)
			if len(cfg.Exclude) == 0 {
				patterns = append(patterns, cfg.Index.Exclude...)
				patterns = append(patterns, cfg.Watch.Exclude...)
			}
			return patterns, nil
		},
		save: func(patterns []string) error {
			cfg, err := readWorkspaceYAML(path)
			if err != nil {
				return err
			}
			if cfg == nil {
				cfg = &config.Config{}
			}
			cfg.Exclude = patterns
			// Scrub legacy keys once the user has written anything to
			// the new shape — they've been migrated.
			cfg.Index.Exclude = nil
			cfg.Watch.Exclude = nil
			return writeWorkspaceYAML(path, cfg)
		},
	}, nil
}

func newGlobalTarget() (*excludeTarget, error) {
	return &excludeTarget{
		label: "global " + config.DefaultGlobalConfigPath(),
		load: func() ([]string, error) {
			gc, err := config.LoadGlobal()
			if err != nil {
				return nil, err
			}
			return append([]string{}, gc.Exclude...), nil
		},
		save: func(patterns []string) error {
			gc, err := config.LoadGlobal()
			if err != nil {
				return err
			}
			gc.Exclude = patterns
			return gc.Save()
		},
	}, nil
}

func newRepoEntryTarget(name string) (*excludeTarget, error) {
	return &excludeTarget{
		label: "repo entry " + name + " in " + config.DefaultGlobalConfigPath(),
		load: func() ([]string, error) {
			gc, err := config.LoadGlobal()
			if err != nil {
				return nil, err
			}
			entry := gc.FindRepoByPrefix(name)
			if entry == nil {
				return nil, fmt.Errorf("no repo entry named %q in global config", name)
			}
			return append([]string{}, entry.Exclude...), nil
		},
		save: func(patterns []string) error {
			gc, err := config.LoadGlobal()
			if err != nil {
				return err
			}
			entry := gc.FindRepoByPrefix(name)
			if entry == nil {
				return fmt.Errorf("no repo entry named %q in global config", name)
			}
			entry.Exclude = patterns
			return gc.Save()
		},
	}, nil
}

// --- Command implementations ---

func runConfigExcludeAdd(_ *cobra.Command, args []string) error {
	t, err := resolveTarget()
	if err != nil {
		return err
	}
	pattern, err := normalizePattern(args[0], t.repoRoot)
	if err != nil {
		return err
	}
	existing, err := t.load()
	if err != nil {
		return err
	}
	if slices.Contains(existing, pattern) {
		fmt.Fprintf(os.Stderr, "[gortex] already present in %s: %s\n", t.label, pattern)
		return nil
	}
	existing = append(existing, pattern)
	if err := t.save(existing); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex] added %q to %s\n", pattern, t.label)
	notifyDaemonReload()
	return nil
}

func runConfigExcludeRemove(_ *cobra.Command, args []string) error {
	t, err := resolveTarget()
	if err != nil {
		return err
	}
	existing, err := t.load()
	if err != nil {
		return err
	}

	// Accept both the raw arg and its normalised form so a user can
	// remove by path just as they added.
	candidates := map[string]struct{}{args[0]: {}}
	if p, err := normalizePattern(args[0], t.repoRoot); err == nil {
		candidates[p] = struct{}{}
	}

	filtered := existing[:0]
	removed := false
	for _, p := range existing {
		if _, drop := candidates[p]; drop {
			removed = true
			continue
		}
		filtered = append(filtered, p)
	}
	if !removed {
		return fmt.Errorf("pattern not found in %s", t.label)
	}
	if err := t.save(filtered); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[gortex] removed %q from %s\n", args[0], t.label)
	notifyDaemonReload()
	return nil
}

func runConfigExcludeList(cmd *cobra.Command, _ []string) error {
	rows := [][2]string{}
	for _, p := range excludes.Builtin {
		rows = append(rows, [2]string{"builtin", p})
	}

	gc, err := config.LoadGlobal()
	if err == nil {
		for _, p := range gc.Exclude {
			rows = append(rows, [2]string{"global", p})
		}
	}

	wsPath, wsRoot, wsErr := findWorkspaceConfigPath()
	if wsErr == nil && wsRoot != "" {
		wsPrefix := filepath.Base(wsRoot)
		if gc != nil {
			if entry := gc.FindRepoByPrefix(wsPrefix); entry != nil {
				for _, p := range entry.Exclude {
					rows = append(rows, [2]string{"repo:" + wsPrefix, p})
				}
			}
		}
		// Only read the workspace file if it actually exists; findWorkspaceConfigPath
		// returns a prospective path for the create case.
		if _, err := os.Stat(wsPath); err == nil {
			ws, err := readWorkspaceYAML(wsPath)
			if err != nil {
				// A malformed workspace file reduces the layer to zero
				// patterns — say so instead of just omitting it, so the
				// user sees why their excludes are not in effect.
				rows = append(rows, [2]string{"workspace", fmt.Sprintf("0 patterns loaded — file failed to parse: %v", err)})
			} else if ws != nil {
				for _, p := range ws.Exclude {
					rows = append(rows, [2]string{"workspace", p})
				}
				if len(ws.Exclude) == 0 {
					for _, p := range ws.Index.Exclude {
						rows = append(rows, [2]string{"workspace (legacy index.exclude)", p})
					}
					for _, p := range ws.Watch.Exclude {
						rows = append(rows, [2]string{"workspace (legacy watch.exclude)", p})
					}
				}
			}
			// Unknown keys at any depth are silently dropped by yaml.Unmarshal;
			// flag them here where a config typo is cheapest to spot.
			if unknown := config.UnknownWorkspaceKeys(wsPath); len(unknown) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"[gortex] warning: %s contains keys gortex does not recognize (they are ignored): %s\n",
					wsPath, strings.Join(unknown, ", "))
			}
		}
	}

	width := 0
	for _, r := range rows {
		if len(r[0]) > width {
			width = len(r[0])
		}
	}
	for _, r := range rows {
		fmt.Printf("[%-*s] %s\n", width, r[0], r[1])
	}
	return nil
}

// --- Helpers ---

// normalizePattern converts an arg to a canonical pattern.
// If the arg looks like a glob (contains *, ?, [, or leading ! or /),
// it's returned verbatim. If it's an existing filesystem path under
// repoRoot, it's normalised to a repo-relative gitignore pattern
// (trailing "/" for directories). Otherwise returned as-is — users
// may intentionally write literal patterns we can't stat.
func normalizePattern(arg, repoRoot string) (string, error) {
	if looksLikeGlob(arg) {
		return arg, nil
	}
	if repoRoot == "" {
		return arg, nil
	}
	absArg, err := filepath.Abs(arg)
	if err != nil {
		return arg, nil
	}
	info, err := os.Stat(absArg)
	if err != nil {
		return arg, nil
	}
	rel, err := filepath.Rel(repoRoot, absArg)
	if err != nil {
		return arg, nil
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "", fmt.Errorf("refusing to ignore the repo root")
	}
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return "", fmt.Errorf("path %s is outside the repo root %s", arg, repoRoot)
	}
	if info.IsDir() {
		return rel + "/", nil
	}
	return rel, nil
}

// looksLikeGlob reports whether s has pattern syntax that should be
// preserved verbatim rather than treated as a filesystem path. A leading
// "/" is intentionally NOT treated as a glob marker — users more often
// pass an absolute filesystem path than a root-anchored gitignore
// pattern; the latter is better expressed by running the command from
// the repo root with a relative arg.
func looksLikeGlob(s string) bool {
	if strings.HasPrefix(s, "!") {
		return true
	}
	return strings.ContainsAny(s, "*?[")
}

// findWorkspaceConfigPath walks up from the cwd looking for an existing
// .gortex.yaml. When one is found, its path and containing dir are
// returned. Otherwise it returns a prospective path at the nearest
// git root (or cwd as a last resort) so `add` can create the file.
func findWorkspaceConfigPath() (path, repoRoot string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	dir := cwd
	for {
		candidate := filepath.Join(dir, ".gortex.yaml")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if root := gitToplevel(cwd); root != "" {
		return filepath.Join(root, ".gortex.yaml"), root, nil
	}
	return filepath.Join(cwd, ".gortex.yaml"), cwd, nil
}

func gitToplevel(start string) string {
	dir := start
	for {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// readWorkspaceYAML loads .gortex.yaml into a zero-value Config: callers
// mutate and write the result back, and seeding it with Default() would
// persist computed defaults into the file on save. Parse acceptance is
// still the daemon's — it goes through the shared
// config.ParseWorkspaceFileInto parser, so this surface agrees with the
// daemon and with `gortex init` on which files are honored. Returns nil
// with no error when the file is absent — the workspace target creates it
// on save.
func readWorkspaceYAML(path string) (*config.Config, error) {
	cfg := &config.Config{}
	if err := config.ParseWorkspaceFileInto(path, cfg); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return cfg, nil
}

// writeWorkspaceYAML writes .gortex.yaml. The round-trip through the
// Config struct loses comments, which is an accepted trade-off until
// someone needs preservation.
func writeWorkspaceYAML(path string, cfg *config.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// notifyDaemonReload sends a Reload RPC when a daemon is running so
// exclude changes take effect without a restart. Non-fatal on error —
// the written config will be picked up on the daemon's next start.
func notifyDaemonReload() {
	if !daemon.IsRunning() {
		return
	}
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli-config"})
	if err != nil {
		return
	}
	defer c.Close()
	resp, err := c.Control(daemon.ControlReload, nil)
	if err != nil || !resp.OK {
		return
	}
	fmt.Fprintln(os.Stderr, "[gortex] daemon reloaded")
}
