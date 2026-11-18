package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Inspect and edit per-repo workspace declarations",
	Long: `Manage the per-repo .gortex.yaml workspace and project slugs.

The hard graph boundary keys every node and contract on its
workspace. Two repos that should pair their
contracts as one logical service must declare the same 'workspace:'
in their respective .gortex.yaml files. Without that declaration,
each repo defaults to its own workspace (named after the repo) and
cross-repo contract matching stops at the boundary.

This command is the migration path: it lists what each tracked repo
currently declares and writes new declarations atomically without
disturbing other keys in the file.`,
}

var workspaceListJSON bool

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show the workspace and project declared in each tracked repo's .gortex.yaml",
	RunE:  runWorkspaceList,
}

var workspaceSetCmd = &cobra.Command{
	Use:   "set <repo> <workspace> [project]",
	Short: "Write workspace (and optional project) for a tracked repo",
	Long: `Updates one repo's workspace declaration.

The repo argument is matched against the global config: it can be
a name (e.g. 'tuck-api'), an absolute path, or a path relative to
the cwd. Project defaults to the workspace slug when omitted.

Without --global the value is written to the repo's .gortex.yaml.
With --global the value is written to
~/.gortex/config.yaml (your user-level config), which is
the right choice for OSS / read-only repos where you don't want
to leave any artifact in the repo. Global overrides win over
.gortex.yaml at resolution time, so you can also use --global to
override a workspace the repo author chose.`,
	Args: cobra.RangeArgs(2, 3),
	RunE: runWorkspaceSet,
}

var (
	workspaceSetAll    bool
	workspaceSetRoot   string
	workspaceSetGlobal bool
)

var workspaceSetAllCmd = &cobra.Command{
	Use:   "set-all <workspace>",
	Short: "Stamp the same workspace into every tracked repo (interactive confirmation)",
	Long: `Bulk migration helper. Walks every tracked repo in the global config
and writes the same 'workspace:' value to each.

By default the command prints the planned changes and asks for
confirmation. Pass --yes to skip the prompt (CI / scripted use).
--root <path> restricts the bulk update to repos under that prefix
(e.g. only your "work" repos). --global writes to
~/.gortex/config.yaml instead of touching each repo's
.gortex.yaml — the OSS-friendly path.`,
	Args: cobra.ExactArgs(1),
	RunE: runWorkspaceSetAll,
}

func init() {
	workspaceCmd.AddCommand(workspaceListCmd)
	workspaceCmd.AddCommand(workspaceSetCmd)
	workspaceCmd.AddCommand(workspaceSetAllCmd)
	workspaceListCmd.Flags().BoolVar(&workspaceListJSON, "json", false, "emit machine-readable JSON instead of a table")
	workspaceSetCmd.Flags().BoolVar(&workspaceSetGlobal, "global", false, "write to ~/.gortex/config.yaml instead of the repo's .gortex.yaml (OSS-friendly)")
	workspaceSetAllCmd.Flags().BoolVarP(&workspaceSetAll, "yes", "y", false, "skip interactive confirmation")
	workspaceSetAllCmd.Flags().StringVar(&workspaceSetRoot, "root", "", "only stamp repos whose path starts with this prefix")
	workspaceSetAllCmd.Flags().BoolVar(&workspaceSetGlobal, "global", false, "write to ~/.gortex/config.yaml instead of each repo's .gortex.yaml")
	rootCmd.AddCommand(workspaceCmd)
}

// loadGlobalRepos reads the global config (~/.gortex/config.yaml
// by default, or whatever --config points at) and returns the tracked
// repo entries. Failure to read the config returns an error rather
// than an empty list — silently doing nothing on a typo'd config
// path is a worse default than failing loudly.
func loadGlobalRepos() ([]config.RepoEntry, error) {
	path := config.DefaultGlobalConfigPath()
	if cfgFile != "" {
		path = cfgFile
	}
	gc, err := config.LoadGlobal(path)
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	return gc.Repos, nil
}

// readRepoYAML loads <repoPath>/.gortex.yaml and returns the parsed
// generic map. Missing file is not an error — returns an empty map
// so callers can write a fresh file.
func readRepoYAML(repoPath string) (map[string]any, error) {
	p := filepath.Join(repoPath, ".gortex.yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	out := make(map[string]any)
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if out == nil {
		out = make(map[string]any)
	}
	return out, nil
}

// writeRepoYAML serialises the map back to <repoPath>/.gortex.yaml.
// Uses an atomic tmp+rename so a partial write doesn't leave the
// file truncated. The yaml.v3 encoder preserves key order from the
// input map's iteration which is non-deterministic; we sort keys to
// keep diffs readable.
func writeRepoYAML(repoPath string, payload map[string]any) error {
	p := filepath.Join(repoPath, ".gortex.yaml")
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Hand-roll the encoder so we can write keys in deterministic
	// order. yaml.v3 doesn't expose a sorted-encode helper.
	var buf strings.Builder
	for _, k := range keys {
		v, err := yaml.Marshal(map[string]any{k: payload[k]})
		if err != nil {
			return fmt.Errorf("marshal %s: %w", k, err)
		}
		buf.Write(v)
	}

	// Write atomically with a random-named temp file so a predictable ".tmp"
	// name cannot be pre-created (or symlinked) by a co-tenant in the
	// directory to redirect this write.
	tmpFile, err := os.CreateTemp(filepath.Dir(p), filepath.Base(p)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", p, err)
	}
	tmp := tmpFile.Name()
	if _, err := tmpFile.Write([]byte(buf.String())); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, p, err)
	}
	return nil
}

// matchRepo finds the RepoEntry whose name OR absolute path matches
// `target`. Returns (-1, error) when no entry matches.
func matchRepo(repos []config.RepoEntry, target string) (int, error) {
	abs, _ := filepath.Abs(target)
	for i, r := range repos {
		if r.Name == target {
			return i, nil
		}
		ra, _ := filepath.Abs(r.Path)
		if pathkey.EqualPaths(ra, abs) {
			return i, nil
		}
		// Allow short suffix matches like "tuck-api" against
		// "/Users/x/code/work/tuck-api".
		if hasTrailingComponent(r.Path, target) {
			return i, nil
		}
	}
	return -1, fmt.Errorf("no tracked repo matches %q (try `gortex daemon status` for the list)", target)
}

// hasTrailingComponent reports whether name is the last path component of p.
// Both separators are accepted because a configured repo path is a string a
// human (or a Git-for-Windows tool) wrote: on Windows the same entry turns up
// as `C:\code\tuck-api` and `C:/code/tuck-api`, and a short-name lookup must
// find either. On POSIX the two branches are the same test.
func hasTrailingComponent(p, name string) bool {
	if name == "" {
		return false
	}
	return strings.HasSuffix(p, "/"+name) ||
		strings.HasSuffix(p, string(filepath.Separator)+name)
}

// workspaceListEntry is one repo's resolved workspace/project view —
// the row of `gortex workspace list`. It is also the JSON shape emitted
// under --json; the table renderer projects the same struct.
type workspaceListEntry struct {
	// Repo is the repo's configured name, falling back to the path
	// basename when the global config declares no explicit name.
	Repo string `json:"repo"`
	// Path is the absolute on-disk path of the repository.
	Path string `json:"path"`
	// Workspace is the effective workspace slug after applying the
	// precedence rules (global config > .gortex.yaml > default).
	Workspace string `json:"workspace"`
	// Project is the effective project slug, same precedence.
	Project string `json:"project"`
	// Source records where the effective Workspace came from:
	// "global", ".gortex.yaml", "default", or a "global (overrides
	// .gortex.yaml=...)" note when an override is shadowing a repo
	// declaration.
	Source string `json:"source"`
	// Error carries a non-empty message when the repo's .gortex.yaml
	// could not be read or parsed; the other fields are unset then.
	Error string `json:"error,omitempty"`
}

// collectWorkspaceList resolves every tracked repo into a
// workspaceListEntry. The precedence rules (global RepoEntry override >
// .gortex.yaml declaration > default repo prefix) are applied here so
// the table and JSON renderers stay byte-identical in semantics.
func collectWorkspaceList(repos []config.RepoEntry) []workspaceListEntry {
	entries := make([]workspaceListEntry, 0, len(repos))
	for _, r := range repos {
		// Precedence: global config (RepoEntry) > .gortex.yaml > default.
		ws, proj, src := "", "", ""
		if r.Workspace != "" || r.Project != "" {
			ws, proj, src = r.Workspace, r.Project, "global"
		}
		yamlWS, yamlProj := "", ""
		payload, yamlErr := readRepoYAML(r.Path)
		if yamlErr != nil {
			entries = append(entries, workspaceListEntry{
				Repo:  repoLabel(r),
				Path:  r.Path,
				Error: yamlErr.Error(),
			})
			continue
		}
		yamlWS, _ = payload["workspace"].(string)
		yamlProj, _ = payload["project"].(string)
		if ws == "" {
			ws = yamlWS
			if ws != "" {
				src = ".gortex.yaml"
			}
		}
		if proj == "" {
			proj = yamlProj
		}
		// Note when global override is shadowing a repo declaration.
		if r.Workspace != "" && yamlWS != "" && r.Workspace != yamlWS {
			src = "global (overrides .gortex.yaml=" + yamlWS + ")"
		}
		if ws == "" {
			ws = "(default: " + repoLabel(r) + ")"
			if src == "" {
				src = "default"
			}
		}
		if proj == "" {
			proj = "(default: " + repoLabel(r) + ")"
		}
		entries = append(entries, workspaceListEntry{
			Repo:      repoLabel(r),
			Path:      r.Path,
			Workspace: ws,
			Project:   proj,
			Source:    src,
		})
	}
	return entries
}

func runWorkspaceList(cmd *cobra.Command, _ []string) error {
	repos, err := loadGlobalRepos()
	if err != nil {
		return err
	}

	entries := collectWorkspaceList(repos)

	if workspaceListJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	tty := progress.IsTTY(cmd.ErrOrStderr()) && !noProgress

	if len(entries) == 0 {
		if tty {
			emitWorkspaceBanner(cmd.ErrOrStderr(), "list", "Workspace declarations across every tracked repo.")
			fmt.Fprintln(cmd.ErrOrStderr(), "  "+progress.StyleHint.Render("◌  no tracked repos — run `gortex track <path>` to add one"))
			fmt.Fprintln(cmd.ErrOrStderr())
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "(no tracked repos)")
		}
		return nil
	}

	if tty {
		emitWorkspaceBanner(cmd.ErrOrStderr(), "list", "Workspace declarations across every tracked repo.")
	}

	t := table.NewWriter()
	t.SetOutputMirror(cmd.OutOrStdout())
	t.SetStyle(table.StyleLight)
	t.AppendHeader(table.Row{"repo", "workspace", "project", "source", "path"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignLeft},
		{Number: 4, Align: text.AlignLeft},
		{Number: 5, Align: text.AlignLeft},
	})

	for _, e := range entries {
		if e.Error != "" {
			t.AppendRow(table.Row{e.Repo, "(error)", e.Error, "", e.Path})
			continue
		}
		t.AppendRow(table.Row{e.Repo, e.Workspace, e.Project, e.Source, e.Path})
	}
	t.Render()
	return nil
}

func repoLabel(r config.RepoEntry) string {
	if r.Name != "" {
		return r.Name
	}
	return filepath.Base(r.Path)
}

func runWorkspaceSet(cmd *cobra.Command, args []string) error {
	repos, err := loadGlobalRepos()
	if err != nil {
		return err
	}
	idx, err := matchRepo(repos, args[0])
	if err != nil {
		return err
	}
	workspace := args[1]
	project := workspace
	if len(args) >= 3 {
		project = args[2]
	}
	r := repos[idx]

	if workspaceSetGlobal {
		path, err := stampWorkspaceGlobal(r, workspace, project)
		if err != nil {
			return err
		}
		emitWorkspaceSetSummary(cmd, path, repoLabel(r), workspace, project, true)
		return nil
	}

	if err := stampWorkspace(r.Path, workspace, project); err != nil {
		return err
	}
	emitWorkspaceSetSummary(cmd, r.Path+"/.gortex.yaml", repoLabel(r), workspace, project, false)
	return nil
}

// emitWorkspaceBanner prints the workspace-subcommand banner on stderr. We
// keep banners on stderr so the table / JSON / one-liner on stdout stays
// pipe-clean for scripts.
func emitWorkspaceBanner(w io.Writer, mode, subtitle string) {
	if !progress.IsTTY(w) || noProgress {
		return
	}
	banner := tui.Banner{
		Title:    "gortex workspace — " + mode,
		Subtitle: subtitle,
	}.Render()
	fmt.Fprintln(w)
	fmt.Fprintln(w, banner)
	fmt.Fprintln(w)
}

// emitWorkspaceSetSummary prints the post-set summary card showing the file
// modified, the repo affected, and the new workspace/project. Non-TTY callers
// see the legacy one-liner so script parsers keep matching on "updated …".
func emitWorkspaceSetSummary(cmd *cobra.Command, target, repo, workspace, project string, global bool) {
	w := cmd.ErrOrStderr()
	if !progress.IsTTY(w) || noProgress {
		out := cmd.OutOrStdout()
		if global {
			fmt.Fprintf(out, "updated %s: %s → workspace=%s project=%s\n",
				target, repo, workspace, project)
		} else {
			fmt.Fprintf(out, "updated %s: workspace=%s project=%s\n",
				target, workspace, project)
		}
		fmt.Fprintln(out, "\nNote: a running daemon needs `gortex daemon reload` (or restart) to pick up the change.")
		return
	}

	mode := "set"
	if global {
		mode = "set --global"
	}
	emitWorkspaceBanner(w, mode, "Updated workspace declaration for "+repo+".")

	fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render(repo))
	stats := []string{
		progress.Stat(workspace, "workspace", progress.StatGood),
	}
	if project != "" && project != workspace {
		stats = append(stats, progress.Stat(project, "project", progress.StatNeutral))
	}
	fmt.Fprintln(w, "     "+progress.StatStrip(stats...))
	fmt.Fprintln(w, "     "+progress.Row("file", target, 8))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "     "+progress.Caption("a running daemon needs `gortex daemon reload` (or restart) to pick up the change."))
	fmt.Fprintln(w)
}

// stampWorkspace writes workspace+project into a repo's .gortex.yaml
// without disturbing other keys (guards, exclude rules, semantic
// providers, etc.). Idempotent — re-running with the same values
// is a no-op rewrite.
func stampWorkspace(repoPath, workspace, project string) error {
	payload, err := readRepoYAML(repoPath)
	if err != nil {
		return err
	}
	payload["workspace"] = workspace
	payload["project"] = project
	return writeRepoYAML(repoPath, payload)
}

// stampWorkspaceGlobal writes the workspace/project override onto
// the matching RepoEntry in `~/.gortex/config.yaml`. Returns
// the path of the file modified for the user-facing message. Used
// when the user passes --global — the OSS-friendly path that
// leaves no trace in the repo itself.
func stampWorkspaceGlobal(target config.RepoEntry, workspace, project string) (string, error) {
	path := config.DefaultGlobalConfigPath()
	if cfgFile != "" {
		path = cfgFile
	}
	gc, err := config.LoadGlobal(path)
	if err != nil {
		return "", fmt.Errorf("load global config: %w", err)
	}
	updated := false
	for i := range gc.Repos {
		// Match on absolute path — the most stable identity. Name and
		// prefix can collide (two repos can share a basename) but a
		// path is unique to the on-disk location.
		ra, _ := filepath.Abs(gc.Repos[i].Path)
		ta, _ := filepath.Abs(target.Path)
		if ra != ta {
			continue
		}
		gc.Repos[i].Workspace = workspace
		gc.Repos[i].Project = project
		updated = true
		break
	}
	if !updated {
		return "", fmt.Errorf("repo %s not found in %s", target.Path, path)
	}
	if err := gc.Save(); err != nil {
		return "", fmt.Errorf("save global config: %w", err)
	}
	return path, nil
}

func runWorkspaceSetAll(cmd *cobra.Command, args []string) error {
	workspace := args[0]
	repos, err := loadGlobalRepos()
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no tracked repos to update)")
		return nil
	}

	// Filter to --root prefix when set. Match on absolute path
	// prefix so 'gortex workspace set-all work --root ~/code/work'
	// only stamps repos under the work tree.
	var rootAbs string
	if workspaceSetRoot != "" {
		rootAbs, err = filepath.Abs(workspaceSetRoot)
		if err != nil {
			return fmt.Errorf("resolve --root: %w", err)
		}
	}
	planned := make([]config.RepoEntry, 0, len(repos))
	for _, r := range repos {
		if rootAbs != "" {
			ra, _ := filepath.Abs(r.Path)
			if !strings.HasPrefix(ra, rootAbs) {
				continue
			}
		}
		planned = append(planned, r)
	}
	if len(planned) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "(no tracked repos match --root)")
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Plan: stamp workspace=%q (project=workspace) into %d repos:\n", workspace, len(planned))
	for _, r := range planned {
		fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", r.Path)
	}
	if !workspaceSetAll {
		fmt.Fprint(cmd.OutOrStdout(), "\nProceed? [y/N] ")
		var answer string
		_, _ = fmt.Fscanln(os.Stdin, &answer)
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(cmd.OutOrStdout(), "aborted")
			return nil
		}
	}

	updated := 0
	var failures []string
	for _, r := range planned {
		var stampErr error
		if workspaceSetGlobal {
			_, stampErr = stampWorkspaceGlobal(r, workspace, workspace)
		} else {
			stampErr = stampWorkspace(r.Path, workspace, workspace)
		}
		if stampErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.Path, stampErr))
			continue
		}
		updated++
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nupdated %d/%d repos\n", updated, len(planned))
	if len(failures) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "\nfailures:")
		for _, f := range failures {
			fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", f)
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), "\nNote: a running daemon needs `gortex daemon reload` (or restart) to pick up the change.")
	return nil
}
