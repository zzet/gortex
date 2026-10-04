package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
)

var reposJSON bool

// reposBackendPath is the on-disk SQLite backend file `gortex repos`
// reads index-freshness provenance (the repo_index_state table) from.
// Empty defers to resolveReposBackendPath — the running daemon's recorded
// store, else the platform default. Set by --backend-path, and by tests
// pointing at an isolated store.
var reposBackendPath string

var reposCmd = &cobra.Command{
	Use:   "repos",
	Short: "List every tracked repository with its git head and index freshness",
	Long: `Lists the repositories registered in the global config
(~/.gortex/config.yaml).

For each repo the command reports the current git HEAD commit and an
index-freshness indicator: when the persisted index was last built and
whether that index still matches HEAD. A repo is "stale" when HEAD has
moved past the commit the cached index was built from, or when no index
has been persisted yet.

Freshness is read straight out of the graph store. The store is located
in this order: --backend-path if given, else the store a running daemon
recorded at startup (so a daemon started with --backend-path is followed
without repeating the flag here), else ~/.gortex/store/store.sqlite.

Scope: the freshness rows are the BASE corpus only. This command reads
repo_index_state directly, restricted to the base view generation, and
never opens the daemon's view catalog. A daemon serving routed worktree
views holds derived generations beside those rows; none of them is read
here, so a linked worktree's own freshness is NOT what this reports —
the row shown for it is its family's base. Ask "gortex repos
explain-view <path>" for the view that actually serves a working copy.

The default output is a table; --json emits the same data as a JSON
array suitable for scripting, with freshness_scope on every entry
naming the scope above.`,
	RunE: runRepos,
}

func init() {
	reposCmd.Flags().BoolVar(&reposJSON, "json", false, "emit machine-readable JSON instead of a table")
	reposCmd.Flags().StringVar(&reposBackendPath, "backend-path", "",
		"read index freshness from this store file (default: the running daemon's recorded store, else ~/.gortex/store/store.sqlite)")
	rootCmd.AddCommand(reposCmd)
}

// repoStatus is one repository's entry in the `gortex repos` output —
// identity plus the git head commit and index-freshness facts. It is
// the JSON shape emitted under --json; the table renderer projects the
// same struct into columns.
type repoStatus struct {
	// Name is the repo's configured name, falling back to the path
	// basename when the global config declares no explicit name.
	Name string `json:"name"`
	// Path is the absolute on-disk path of the repository.
	Path string `json:"path"`
	// Workspace is the workspace slug the repo's nodes are keyed on
	// (the global-config override; empty when none is declared).
	Workspace string `json:"workspace,omitempty"`
	// HeadCommit is the current git HEAD commit SHA, or empty when
	// the path is not a git repository / git is unavailable.
	HeadCommit string `json:"head_commit"`
	// Branch is the current git branch, empty for a detached HEAD.
	Branch string `json:"branch,omitempty"`
	// IndexedCommit is the commit SHA the persisted index was built
	// from. Empty when no index snapshot exists yet.
	IndexedCommit string `json:"indexed_commit,omitempty"`
	// LastIndexed is the timestamp the persisted index was built.
	// Nil (omitted from JSON) when the repo has never been indexed.
	LastIndexed *time.Time `json:"last_indexed,omitempty"`
	// Stale is true when the persisted index does not match the
	// current HEAD — either HEAD moved past IndexedCommit or no
	// index has been persisted at all.
	Stale bool `json:"stale"`
	// Indexed is true when a persisted index snapshot was found for
	// the repo's current branch slot.
	Indexed bool `json:"indexed"`
	// IndexedDirty is true when the recorded index was built from a
	// working tree with uncommitted changes (the repo_index_state.dirty
	// flag). Omitted from JSON when false / unknown. The index still
	// counts as fresh when its commit matches HEAD — this is provenance,
	// not a staleness signal.
	IndexedDirty bool `json:"indexed_dirty,omitempty"`
	// Missing is true when Path no longer names a directory on disk.
	// Such an entry outlives the checkout it points at and can never
	// go fresh again, so it is reported as its own state rather than
	// collapsing into "stale" behind an empty HEAD (#312).
	Missing bool `json:"missing,omitempty"`
	// FreshnessScope names which generation the Indexed* / Stale fields
	// describe. It is always reposFreshnessScope ("base"): the door this
	// command reads through is base-only by construction
	// (store_sqlite.RepoIndexStateBaseViewGen). It is emitted
	// unconditionally, and not omitted when it is the default, because a
	// scripted consumer must be able to tell "base, declared" from "an
	// older gortex that did not say" — an absent field is the second.
	FreshnessScope string `json:"freshness_scope"`
}

// reposFreshnessScope names the generation `gortex repos` reports freshness
// for. It is derived from the constant the read door's predicate is built
// with, and it shares the probe surface's vocabulary for the same corpus
// (daemon.ProbeViewBase), so the two front doors call it the same thing.
//
// A non-base generation would need a different reader; this one cannot produce
// it, which is why the value is a constant rather than a field of the answer.
var reposFreshnessScope = reposScopeName(store_sqlite.RepoIndexStateBaseViewGen)

func reposScopeName(viewGen int) string {
	if viewGen == store_sqlite.RepoIndexStateBaseViewGen {
		return daemon.ProbeViewBase
	}
	// Unreachable while the read door is base-only. Spelled rather than
	// panicked so a future generation-aware reader names it instead of
	// silently keeping the base label.
	return fmt.Sprintf("view_gen:%d", viewGen)
}

func runRepos(cmd *cobra.Command, _ []string) error {
	repos, err := loadGlobalRepos()
	if err != nil {
		return err
	}

	// Freshness source: the daemon's on-disk SQLite backend records one
	// repo_index_state row per repo at the end of every (re)index. Read it
	// once, read-only, so a single open serves the whole list. A store that
	// does not exist yet yields an empty map and every repo reports as never
	// indexed; a store that exists but cannot be read fails the command.
	indexStates, err := loadRepoIndexStates()
	if err != nil {
		return err
	}

	entries := make([]repoStatus, 0, len(repos))
	for _, r := range repos {
		entries = append(entries, describeRepo(indexStates, len(repos), r))
	}
	// Stable order regardless of config-file ordering so scripted
	// diffs and the table stay deterministic.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Path < entries[j].Path
	})

	if reposJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	return renderReposTable(cmd, entries)
}

// loadRepoIndexStates reads the daemon's per-repo index-freshness rows
// (the SQLite repo_index_state table) keyed by repo prefix. It opens the
// backend store read-only so it is safe to run while a daemon holds the
// same store.
//
// "No store yet" is a legitimate empty answer and returns an empty map. A
// store that exists but cannot be read — corrupt bytes, wrong permissions, a
// schema the query cannot run against — is an error. Degrading those to an
// empty map printed a confident "never indexed" for every repo, which reads as
// a fact about the repos rather than a failure to look, and sends the user to
// re-index work that is already done.
func loadRepoIndexStates() (map[string]graph.RepoIndexState, error) {
	path := resolveReposBackendPath()
	states, err := store_sqlite.ReadRepoIndexStates(path)
	if err != nil {
		return nil, fmt.Errorf("read index freshness from %s: %w", path, err)
	}
	return states, nil
}

// resolveReposBackendPath picks the store to read freshness from:
//
//  1. --backend-path, when the user named one explicitly.
//  2. The store a running daemon recorded at startup. Without this step a
//     daemon started with --backend-path writes its rows somewhere this
//     command never looks, and every repo it has indexed reports as never
//     indexed — the flag would have to be repeated on every status call to
//     get a true answer.
//  3. The platform default, which is where a daemon started without the flag
//     put its store anyway.
func resolveReposBackendPath() string {
	if path := strings.TrimSpace(reposBackendPath); path != "" {
		return path
	}
	if st, ok := daemon.ReadRuntimeState(); ok && st.BackendPath != "" {
		return st.BackendPath
	}
	return filepath.Join(platform.StoreDir(), "store.sqlite")
}

// describeRepo resolves one RepoEntry into a repoStatus by reading the
// repo's current git HEAD and looking up its recorded index freshness.
//
// Freshness comes from the daemon's repo_index_state row, keyed by the
// repo's resolved prefix (config.ResolvePrefix — the entry Name, else the
// path basename); this is what the daemon writes when it tracks or warms up
// a repo. When there is exactly one tracked repo, a lone-repo index keyed
// under the empty prefix counts too. A repo is fresh when the recorded
// commit matches HEAD.
func describeRepo(indexStates map[string]graph.RepoIndexState, repoCount int, r config.RepoEntry) repoStatus {
	head := gitCommitHash(r.Path)
	branch := gitBranch(r.Path)

	entry := repoStatus{
		Name:           repoLabel(r),
		Path:           r.Path,
		Workspace:      r.Workspace,
		HeadCommit:     head,
		Branch:         branch,
		FreshnessScope: reposFreshnessScope,
		// Default to stale; cleared below only when a recorded
		// index is found whose commit matches HEAD.
		Stale: true,
		// A deleted checkout is why HEAD came back empty above. Record
		// the cause so the table can say so instead of leaving the user
		// to infer it from "(none)" — the ghost in #312 sat unflagged
		// for eight days on exactly that inference.
		Missing: config.RepoPathMissing(r.Path),
	}

	// The daemon's freshness row for this repo's prefix.
	prefix := config.ResolvePrefix(r)
	st, ok := indexStates[prefix]
	if !ok && repoCount == 1 {
		// A single-repo (lone) index is keyed under the empty prefix.
		st, ok = indexStates[""]
	}
	if ok {
		entry.Indexed = true
		entry.IndexedCommit = st.IndexedSHA
		entry.IndexedDirty = st.Dirty
		if st.IndexedAt > 0 {
			ts := time.Unix(st.IndexedAt, 0)
			entry.LastIndexed = &ts
		}
		// Fresh only when the recorded index was built from the exact
		// commit HEAD currently points at. An empty HeadCommit (not a
		// git repo) or an empty recorded SHA can never be fresh.
		entry.Stale = head == "" || st.IndexedSHA == "" || st.IndexedSHA != head
	}
	return entry
}

// renderReposTable prints the repo list as an ASCII table — the default
// human-readable form. Columns mirror the repoStatus JSON fields. On a TTY
// we wrap the table in a styled banner + bottom stat strip; on a non-TTY
// (script piping `gortex repos | grep …`) we keep only the bare table so
// parser-shaped scripts don't break.
func renderReposTable(cmd *cobra.Command, entries []repoStatus) error {
	out := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()
	tty := progress.IsTTY(stderr) && !noProgress

	if len(entries) == 0 {
		if tty {
			emitReposBanner(stderr)
			fmt.Fprintln(stderr, "  "+progress.StyleHint.Render("◌  no tracked repos — run `gortex track <path>` to add one"))
			fmt.Fprintln(stderr)
		} else {
			fmt.Fprintln(out, "(no tracked repos)")
		}
		return nil
	}

	if tty {
		emitReposBanner(stderr)
	}
	// Declared before the rows it qualifies, so the scope is read with them
	// rather than discovered under them.
	emitReposScopeNote(stderr)

	t := table.NewWriter()
	t.SetOutputMirror(out)
	t.SetStyle(table.StyleLight)
	t.AppendHeader(table.Row{"repo", "head", "indexed", "last indexed", "freshness", "path"})
	t.SetColumnConfigs([]table.ColumnConfig{
		{Number: 1, Align: text.AlignLeft},
		{Number: 2, Align: text.AlignLeft},
		{Number: 3, Align: text.AlignLeft},
		{Number: 4, Align: text.AlignLeft},
		{Number: 5, Align: text.AlignLeft},
		{Number: 6, Align: text.AlignLeft},
	})

	for _, e := range entries {
		t.AppendRow(table.Row{
			e.Name,
			shortSHA(e.HeadCommit),
			shortSHA(e.IndexedCommit),
			lastIndexedCell(e),
			freshnessCell(e, tty),
			e.Path,
		})
	}
	t.Render()

	emitReposMissingHint(stderr, entries)
	if tty {
		emitReposSummary(stderr, entries)
	}
	return nil
}

// emitReposMissingHint names every tracked repo whose directory is gone
// and the command that removes it.
//
// On stderr, unconditionally — not gated on a TTY like the banner and
// stat strip. Those are decoration; this is the one thing in the output
// a user has to act on, so a scripted `gortex repos | grep …` must still
// see it, and putting it on stdout would inject `gortex untrack <path>`
// lines into the table that same pipeline parses. Scripted callers read
// the `missing` field from --json instead.
func emitReposMissingHint(w interface{ Write([]byte) (int, error) }, entries []repoStatus) {
	var gone []repoStatus
	for _, e := range entries {
		if e.Missing {
			gone = append(gone, e)
		}
	}
	if len(gone) == 0 {
		return
	}
	subject := "repo no longer exists"
	if len(gone) > 1 {
		subject = "repos no longer exist"
	}
	fmt.Fprintf(w, "\n!! %d tracked %s on disk — the path was deleted, renamed, or unmounted.\n",
		len(gone), subject)
	fmt.Fprintln(w, "   They can never be re-indexed. Drop each from the inventory with:")
	for _, e := range gone {
		fmt.Fprintf(w, "     gortex untrack %s\n", e.Path)
	}
}

// emitReposScopeNote declares what generation the freshness columns describe.
//
// On stderr, unconditionally — the same placement and for the same reason as
// emitReposMissingHint: stdout carries the parseable table, and a scripted
// `gortex repos | grep stale` must not gain a line. But this is not decoration
// either, so it is not gated on a TTY: a user reading "fresh" beside a linked
// worktree's repository is reading a fact about the family's BASE corpus, and
// the command that produced it never opened the view catalog that would know
// the worktree's own generation. Saying nothing is what let that read as the
// checkout's own freshness.
//
// Scripted callers read `freshness_scope` from --json instead.
func emitReposScopeNote(w interface{ Write([]byte) (int, error) }) {
	fmt.Fprintf(w, "note: freshness below is the %s corpus only (view generation %d) — "+
		"a routed worktree's own generation is not read here; ask `gortex repos explain-view <path>`\n",
		reposFreshnessScope, store_sqlite.RepoIndexStateBaseViewGen)
}

// emitReposBanner prints the gortex mesh banner on stderr above the table.
// Keeping the banner on stderr (not stdout) means `gortex repos | grep foo`
// still sees only the table on stdout — the JSON / table is parseable, the
// decoration is purely visual.
func emitReposBanner(w interface{ Write([]byte) (int, error) }) {
	banner := tui.Banner{
		Title:    "gortex repos",
		Subtitle: "Every tracked repository with its git head and index freshness.",
	}.Render()
	fmt.Fprintln(w)
	fmt.Fprintln(w, banner)
	fmt.Fprintln(w)
}

// emitReposSummary appends a stat strip below the table: total / fresh /
// stale / never-indexed counts so the eye gets the headline at a glance.
func emitReposSummary(w interface{ Write([]byte) (int, error) }, entries []repoStatus) {
	fresh, stale, never, missing := 0, 0, 0, 0
	for _, e := range entries {
		switch {
		// Missing wins over every freshness bucket: the entry has no
		// checkout left to be fresh or stale about.
		case e.Missing:
			missing++
		case !e.Indexed:
			never++
		case e.Stale:
			stale++
		default:
			fresh++
		}
	}
	stats := []string{
		progress.Stat(strconv.Itoa(len(entries)), "tracked", progress.StatNeutral),
		progress.Stat(strconv.Itoa(fresh), "fresh", progress.StatGood),
	}
	if stale > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(stale), "stale", progress.StatWarn))
	}
	if never > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(never), "never indexed", progress.StatBad))
	}
	if missing > 0 {
		stats = append(stats, progress.Stat(strconv.Itoa(missing), "missing", progress.StatBad))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+progress.StatStrip(stats...))
	fmt.Fprintln(w)
}

// shortSHA abbreviates a 40-char git SHA to its 12-char prefix for the
// table — the full hash stays in the JSON output. Empty in, empty out.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}

// lastIndexedCell renders the last-indexed timestamp for the table, or
// a placeholder when the repo has never been indexed.
func lastIndexedCell(e repoStatus) string {
	if e.LastIndexed == nil {
		return "(never)"
	}
	return e.LastIndexed.Local().Format("2006-01-02 15:04:05")
}

// freshnessCell renders the staleness indicator for the table. On a TTY
// the label is colour-tiered (green/yellow/red) so the eye picks up risk
// from a long list at a glance; non-TTY keeps the plain text so scripts
// that grep for "stale" / "fresh" still match.
func freshnessCell(e repoStatus, tty bool) string {
	label := "fresh"
	style := progress.StyleOK
	switch {
	// Checked first: a deleted checkout is why HEAD is empty, and
	// "stale" would read as "re-index me" for a repo that cannot be
	// re-indexed at all.
	case e.Missing:
		label = "MISSING"
		style = progress.StyleErr
	case !e.Indexed:
		label = "not indexed"
		style = progress.StyleErr
	case e.Stale:
		label = "stale"
		style = progress.StyleHint
	}
	if !tty {
		return label
	}
	return style.Render(label)
}
