package main

import (
	"bytes"
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

The workspace target is the nearest .gortex.yaml between the current
directory and the enclosing git root; the search never climbs past the
git root (or past the current directory outside a git repo). When none
exists there, "add" creates one at the git root (or current directory).

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
			migrated, err := writeWorkspaceExcludes(path, patterns)
			if err != nil {
				return err
			}
			if migrated {
				fmt.Fprintf(os.Stderr, "[gortex] migrated legacy index.exclude / watch.exclude in %s to top-level exclude:\n", path)
			}
			return nil
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
			if entry := gc.FindRepoByPath(wsRoot); entry != nil {
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
// .gortex.yaml, stopping at the enclosing git root: the workspace config
// is the one the daemon reads at the repo root, so a file above the repo
// boundary belongs to some other project and must never be adopted.
// Outside a git repo the search is bounded to the cwd itself. When no
// file is found within that scope, it returns a prospective path at the
// git root (or the cwd) so `add` can create the file.
func findWorkspaceConfigPath() (path, repoRoot string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	floor := gitToplevel(cwd)
	if floor == "" {
		floor = cwd
	}
	dir := cwd
	for {
		candidate := filepath.Join(dir, ".gortex.yaml")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, dir, nil
		}
		parent := filepath.Dir(dir)
		if dir == floor || parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(floor, ".gortex.yaml"), floor, nil
}

// gitToplevel returns the nearest ancestor of start (inclusive) holding a
// .git entry — a directory for a regular checkout, a file for a linked
// worktree or submodule — or "" when start is not inside a git repo.
func gitToplevel(start string) string {
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// readWorkspaceYAML loads .gortex.yaml into a zero-value Config so callers
// see only what the file itself sets — seeding it with Default() would mix
// computed defaults into the layer report. Writes go through
// writeWorkspaceExcludes, never a Config round trip. Parse acceptance is
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

// writeWorkspaceExcludes sets the exclude list of the .gortex.yaml at path
// to patterns, creating the file when absent. It edits the YAML node tree
// rather than round-tripping through config.Config, so comments, key
// order, scalar quoting and keys the Config struct does not model all
// survive; only the exclude keys are touched.
//
// The patterns are written to the top-level `exclude:` key. When that key
// is absent or empty, the target's load() reported the legacy
// index.exclude / watch.exclude lists instead, so those (now folded into
// patterns) are removed from their blocks — siblings such as
// watch.enabled stay — and migrated reports that this happened. An empty
// patterns list removes the top-level key.
func writeWorkspaceExcludes(path string, patterns []string) (migrated bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	var buf bytes.Buffer
	if len(doc.Content) == 0 {
		// A blank or comment-only file parses to no nodes, so its text
		// would be lost on encode; carry it over verbatim instead.
		if len(bytes.TrimSpace(data)) > 0 {
			buf.Write(data)
			if !bytes.HasSuffix(data, []byte("\n")) {
				buf.WriteByte('\n')
			}
		}
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return false, fmt.Errorf("%s: top level is not a mapping", path)
	}

	// Existing scalar nodes are reused for unchanged patterns so their
	// quoting and line comments are kept.
	var pool []*yaml.Node
	seq := yamlMappingValue(root, "exclude")
	if seq != nil && seq.Kind != yaml.SequenceNode && !isYAMLNull(seq) {
		return false, fmt.Errorf("%s: exclude is not a list", path)
	}
	useLegacy := seq == nil || len(seq.Content) == 0
	if useLegacy {
		for _, block := range []string{"index", "watch"} {
			if legacy := yamlMappingValue(yamlMappingValue(root, block), "exclude"); legacy != nil && legacy.Kind == yaml.SequenceNode {
				pool = append(pool, legacy.Content...)
			}
		}
	} else {
		pool = append(pool, seq.Content...)
	}

	if len(patterns) == 0 {
		yamlMappingDelete(root, "exclude")
	} else {
		// New patterns copy the quoting of the existing entries so they
		// match their neighbours.
		var style yaml.Style
		for _, n := range pool {
			if n.Kind == yaml.ScalarNode {
				style = n.Style & (yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle)
			}
		}
		items := make([]*yaml.Node, 0, len(patterns))
		for _, p := range patterns {
			items = append(items, takeYAMLScalar(&pool, p, style))
		}
		switch {
		case seq != nil && seq.Kind == yaml.SequenceNode:
			seq.Content = items
		case seq != nil: // `exclude:` with a null value
			*seq = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items,
				HeadComment: seq.HeadComment, LineComment: seq.LineComment, FootComment: seq.FootComment}
		default:
			root.Content = append(root.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "exclude"},
				&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: items})
		}
	}

	if useLegacy {
		for _, block := range []string{"index", "watch"} {
			m := yamlMappingValue(root, block)
			if m == nil || m.Kind != yaml.MappingNode || !yamlMappingDelete(m, "exclude") {
				continue
			}
			migrated = true
			if len(m.Content) == 0 {
				yamlMappingDelete(root, block)
			}
		}
	}

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(yamlIndent(data))
	if err := enc.Encode(&doc); err != nil {
		return false, err
	}
	if err := enc.Close(); err != nil {
		return false, err
	}
	mode := os.FileMode(0644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	return migrated, os.WriteFile(path, buf.Bytes(), mode)
}

// yamlMappingValue returns the value node for key in mapping m, or nil
// when m is not a mapping or has no such key.
func yamlMappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlMappingDelete removes key (and its value) from mapping m and
// reports whether it was present.
func yamlMappingDelete(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

func isYAMLNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// takeYAMLScalar returns the first node in pool whose value is v, removing
// it from the pool, or a fresh scalar with the given style.
func takeYAMLScalar(pool *[]*yaml.Node, v string, style yaml.Style) *yaml.Node {
	for i, n := range *pool {
		if n.Kind == yaml.ScalarNode && n.Value == v {
			*pool = append((*pool)[:i], (*pool)[i+1:]...)
			return n
		}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: style}
}

// yamlIndent guesses the indentation width of an existing YAML document
// from its shallowest indented content line, defaulting to 2.
func yamlIndent(data []byte) int {
	indent := 0
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		n := len(line) - len(trimmed)
		if n == 0 || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indent == 0 || n < indent {
			indent = n
		}
	}
	if indent < 2 || indent > 8 {
		return 2
	}
	return indent
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
