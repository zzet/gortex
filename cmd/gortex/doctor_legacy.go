package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/respbudget"
)

// Legacy state: what older versions quietly left on disk.
//
// A user coming back from v0.64.x can be carrying two kinds of state they
// never asked for. Before the linked-worktree `track` warning (#935),
// `gortex track <worktree>` built a full, separate corpus for one branch, so a
// repository that was linked into many worktrees could end up with a corpus
// per worktree — each a complete index with its own watcher, coordinator and
// memory. And a repo the daemon auto-indexed on its own could be written to
// config.yaml by a later, unrelated config save; on the next boot seeding read
// it back and gave it a `manual_config` intent, after which it looked exactly
// like a repo the user configured.
//
// This section only LISTS. No repair happens here: removing state is a
// separate, confirmed operation, and doctor's contract is that it writes zero
// bytes. Every finding is a warning, so the exit status never moves.
//
// The one distinction the listing cannot make from the catalog alone is
// "configured" versus "written by the daemon for itself". There is no durable
// provenance table — deliberately, since adding one is out of scope — so the
// closest available signal is a timing gap between two UUIDv7 ids. A real
// entry gets its checkout row and its intent within seconds of each other; a
// leaked entry gets the checkout row on one boot and the intent on a later
// one. We call the latter "likely auto-indexed", and say "unattributed"
// rather than "leaked" everywhere: the wording has to be true of the false
// positives too.

// legacyImplicitGap is how far the intent may postdate its checkout before the
// entry is called likely auto-indexed. Boots are seconds apart; a gap this
// large means the intent came from a different run of the daemon.
const legacyImplicitGap = 10 * time.Minute

// DoctorLegacyState is the JSON `legacy_state` key.
type DoctorLegacyState struct {
	// Checked is false when the daemon was not running. The listing needs the
	// catalog, and doctor must never open the store offline to get it.
	Checked bool   `json:"checked"`
	Skipped string `json:"skipped,omitempty"`

	// DedicatedCorpora are linked worktrees that own a full corpus of their
	// own instead of composing over the family primary.
	DedicatedCorpora []DoctorLegacyCorpus `json:"dedicated_corpora,omitempty"`
	// Unattributed are configured repos whose only intent is `manual_config`:
	// a config entry with no record of the user asking for it.
	Unattributed []DoctorLegacyEntry `json:"unattributed,omitempty"`
	// Intentless are configured repos with no active tracking intent at all,
	// or no checkout row in the catalog.
	Intentless []DoctorLegacyEntry `json:"intentless,omitempty"`
}

// DoctorLegacyCorpus is one dedicated worktree corpus.
type DoctorLegacyCorpus struct {
	Family          string   `json:"family"`
	Root            string   `json:"root"`
	RepoPrefix      string   `json:"repo_prefix,omitempty"`
	GraphID         string   `json:"graph_id"`
	GraphState      string   `json:"graph_state,omitempty"`
	CoordinatorLive bool     `json:"coordinator_live"`
	Intents         []string `json:"intents,omitempty"`
	// FamilyPrimary marks a dedicated corpus that is also the family's base.
	// Removing one of these is not a simple cleanup, so it is called out
	// separately from the rest.
	FamilyPrimary bool `json:"family_primary,omitempty"`
}

// DoctorLegacyEntry is one configured repo that looks unattributed or has no
// intent.
type DoctorLegacyEntry struct {
	Path string `json:"path"`
	// SourceKinds are the active intents' kinds; empty for an intent-less
	// entry.
	SourceKinds []string `json:"source_kinds,omitempty"`
	// LikelyAutoIndexed is set when the manual_config intent postdates the
	// checkout row by more than legacyImplicitGap.
	LikelyAutoIndexed bool `json:"likely_auto_indexed,omitempty"`
	// Gap is the measured intent-minus-checkout interval, for the reader who
	// wants to judge the heuristic themselves.
	Gap string `json:"intent_checkout_gap,omitempty"`
}

// collectLegacyState builds the section. It asks the daemon for the checkout
// catalog exactly the way `gortex repos families` does, then reads config.yaml
// directly for the configured repo list. It never opens the store.
func collectLegacyState(home string) DoctorLegacyState {
	state := DoctorLegacyState{Checked: false}
	if !doctorLegacyEnv().DaemonRunning {
		state.Skipped = "daemon not running; legacy state not checked"
		return state
	}

	// The census must be daemon-wide, and it must not be structurally trimmed.
	//
	// Two defects made this request lie about the inventory it had read:
	//
	//  1. It relayed through the current repository, so `list_checkouts`
	//     answered "families visible to THIS session's workspace". A doctor run
	//     inside one tracked repository never saw the healthy repositories
	//     tracked in other workspaces, and classifyConfiguredRepos then read
	//     their absence as "no active intent". An untracked cwd fails the
	//     relay's preflight outright. The census below is the administrative,
	//     unbound read the tool documents as "the daemon-wide inventory" -- the
	//     only read whose absence means what it appears to mean.
	//  2. It passed an empty argument map, so `list_checkouts` applied its
	//     default 40,000-byte response budget. The budgeter drops nested
	//     checkout/graph rows and whole families when a large worktree
	//     inventory overflows, and unmarshalling into FamiliesOverview ignores
	//     the truncation metadata. A trimmed census is partial evidence read as
	//     complete evidence.
	//
	// `max_bytes: 0` opts out of the byte cap. The response is then validated
	// for completeness before anything is classified.
	raw, err := collectDaemonWideCensus("list_checkouts", map[string]any{"max_bytes": 0})
	if err != nil {
		// A daemon that answered the handshake but refuses the listing is not
		// a legacy-state finding; say the check did not happen and move on.
		state.Skipped = "daemon did not answer list_checkouts; legacy state not checked"
		return state
	}

	// Fail closed on a trimmed census. Declaring the check complete while
	// holding a partial listing is how a missing row becomes a false
	// "intentless" warning.
	if bytes.Contains(raw, []byte(respbudget.TruncatedKey)) {
		state.Skipped = "daemon returned a truncated checkout listing; legacy state not checked " +
			"(a partial inventory would report tracked repositories as intentless)"
		return state
	}

	var overview indexer.FamiliesOverview
	if err := json.Unmarshal(raw, &overview); err != nil {
		state.Skipped = "daemon returned an unreadable checkout listing; legacy state not checked"
		return state
	}
	state.Checked = true

	state.DedicatedCorpora = dedicatedWorktreeCorpora(overview)
	state.Unattributed, state.Intentless = classifyConfiguredRepos(overview, doctorConfiguredPaths(home))
	return state
}

// doctorLegacyEnv is the environment probe the collector reads. Stubbed in
// tests: doctorEnvironment() performs a live control handshake, so a collector
// test on a host with no daemon would take the "not checked" arm and assert
// nothing at all.
var doctorLegacyEnv = doctorEnvironment

// collectDaemonWideCensus reads a tool result from an administrative session
// that is NOT bound to a working directory.
//
// Every other CLI path goes through resolveExecutorWithToolSurface, which
// preflights the cwd and refuses to dial when no tracked repo covers it. That
// refusal is right for a graph query -- the answer would come from the wrong
// corpus -- but it is exactly wrong for an inventory: doctor is most useful
// when it runs from a directory gortex does not track, and a workspace-bound
// session answers only about its own workspace. An empty CWD yields an unbound
// session, which the daemon serves from the whole catalog (see
// sessionWorkspaceRepoSet: an unbound session is never clamped).
//
// When no daemon is running we say so rather than inventing an empty inventory.
func collectDaemonWideCensus(tool string, args map[string]any) (json.RawMessage, error) {
	return doctorCensusTool(tool, args)
}

// doctorCensusTool is the seam the collector tests stub. A census that needs a
// live daemon cannot be tested for the properties that matter here -- an
// unbound read, an explicit no-truncation cap, and a fail-closed arm when the
// daemon trims anyway.
var doctorCensusTool = func(tool string, args map[string]any) (json.RawMessage, error) {
	return dialUnboundCensus(tool, args)
}

func dialUnboundCensus(tool string, args map[string]any) (json.RawMessage, error) {
	if !daemon.IsRunning() {
		return nil, ErrNoExecutor
	}
	c, err := daemon.Dial(daemon.Handshake{
		Mode:       daemon.ModeMCP,
		ClientName: "doctor-legacy-census",
		Tools:      cliLegacyToolSurface,
		ToolsMode:  cliLegacyToolMode,
		// CWD deliberately empty: an unbound administrative session.
	})
	if err != nil {
		return nil, ErrNoExecutor
	}
	defer func() { _ = c.Close() }()
	exec := &daemonExecutor{client: c, pinJSONDefault: cliLegacyToolSurface != gortexmcp.FacadeSurfaceVersion}
	return exec.CallTool(context.Background(), tool, args)
}

// doctorConfiguredPaths is the seam the collector tests stub. The real reader
// resolves the platform config path, which a t.TempDir() home does not control,
// so a fixture that wrote config.yaml into its own temp dir was reading the
// developer's actual config instead.
var doctorConfiguredPaths = configuredRepoPaths

// configuredRepoPaths reads the repo list out of config.yaml. Reading the file
// is not writing it, and it is the only place a `manual_config` entry can be
// said to come from.
func configuredRepoPaths(home string) []string {
	path := filepath.Join(home, ".gortex", "config.yaml")
	if p := config.DefaultGlobalConfigPath(); p != "" {
		path = p
	}
	gc, err := config.LoadGlobal(path)
	if err != nil || gc == nil {
		return nil
	}
	out := make([]string, 0, len(gc.Repos))
	for _, r := range gc.Repos {
		if strings.TrimSpace(r.Path) != "" {
			out = append(out, r.Path)
		}
	}
	return out
}

// dedicatedWorktreeCorpora finds every checkout that is a linked worktree
// (never the family's main checkout) and owns a dedicated corpus of its own.
// A checkout served through the family's automatic lane has no GraphID and is
// exactly what we want to see at scale, so it is skipped.
func dedicatedWorktreeCorpora(overview indexer.FamiliesOverview) []DoctorLegacyCorpus {
	var out []DoctorLegacyCorpus
	for _, family := range overview.Families {
		graphByID := make(map[string]indexer.GraphOverview, len(family.Graphs))
		for _, g := range family.Graphs {
			graphByID[g.GraphID] = g
		}
		for _, co := range family.Checkouts {
			if co.GraphID == "" || co.EffectiveMode != "dedicated" {
				continue
			}
			// The family's main checkout is the base corpus itself; a linked
			// worktree is everything else. AdminName is the git worktree
			// administrative name, and only the main one is empty.
			if co.AdminName == "" {
				continue
			}
			graph := graphByID[co.GraphID]
			out = append(out, DoctorLegacyCorpus{
				Family: doctorLabel(family.DisplayRemote),
				Root:   doctorPath(co.RootPath),
				// A prefix names the repository as surely as the remote does.
				RepoPrefix:      doctorLabel(graph.RepoPrefix),
				GraphID:         co.GraphID,
				GraphState:      graph.State,
				CoordinatorLive: co.CoordinatorLive,
				Intents:         append([]string(nil), co.Intents...),
				FamilyPrimary:   graph.IsPrimary || family.PrimaryGraphID == co.GraphID,
			})
		}
	}
	return out
}

// classifyConfiguredRepos splits configured repos into the two findings: ones
// whose only intent is manual_config (unattributed, maybe auto-indexed) and
// ones with no active intent at all.
func classifyConfiguredRepos(overview indexer.FamiliesOverview, configured []string) (unattributed, intentless []DoctorLegacyEntry) {
	byPath := checkoutIndexByRoot(overview)
	for _, cfgPath := range configured {
		co, ok := byPath[canonicalRoot(cfgPath)]
		if !ok {
			intentless = append(intentless, DoctorLegacyEntry{Path: doctorPath(cfgPath)})
			continue
		}
		if len(co.Intents) == 0 {
			intentless = append(intentless, DoctorLegacyEntry{Path: doctorPath(cfgPath)})
			continue
		}
		if hasRecordedIntent(co.Intents) {
			continue
		}
		entry := DoctorLegacyEntry{Path: doctorPath(cfgPath), SourceKinds: append([]string(nil), co.Intents...)}
		if gap, measured := intentCheckoutGap(co); measured {
			entry.Gap = gap.Round(time.Second).String()
			entry.LikelyAutoIndexed = gap > legacyImplicitGap
		}
		unattributed = append(unattributed, entry)
	}
	return unattributed, intentless
}

// hasRecordedIntent reports whether the user actually asked for this checkout.
// A manual_config intent is what config seeding writes, so it does not count.
func hasRecordedIntent(kinds []string) bool {
	for _, k := range kinds {
		if k == "cli_track" || k == "mcp_track" {
			return true
		}
	}
	return false
}

// intentCheckoutGap is how long after its checkout an entry's manual_config
// intent was created, decoded from the two UUIDv7 ids. ok is false when either
// id is not a decodable v7 (an old row, a hand-written id).
func intentCheckoutGap(co indexer.CheckoutOverview) (time.Duration, bool) {
	checkoutAt, ok := uuidV7Time(co.CheckoutID)
	if !ok {
		return 0, false
	}
	var intentAt time.Time
	for _, d := range co.IntentDetails {
		if d.SourceKind == "manual_config" {
			at, ok := uuidV7Time(d.IntentID)
			if !ok {
				return 0, false
			}
			intentAt = at
			break
		}
	}
	if intentAt.IsZero() {
		return 0, false
	}
	return intentAt.Sub(checkoutAt), true
}

// uuidV7Time decodes the millisecond timestamp a UUIDv7 carries in its first
// 48 bits. Both checkout ids and intent ids are minted with uuid.NewV7(), so
// this reads a creation time the catalog does not otherwise store — and it is
// the reason no schema step is needed to tell a leaked entry from a real one.
func uuidV7Time(id string) (time.Time, bool) {
	u, err := uuid.Parse(id)
	if err != nil || u.Version() != 7 {
		return time.Time{}, false
	}
	ms := int64(u[0])<<40 | int64(u[1])<<32 | int64(u[2])<<24 |
		int64(u[3])<<16 | int64(u[4])<<8 | int64(u[5])
	if ms == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

// checkoutIndexByRoot keys every checkout by a canonical root path so a
// config.yaml entry can be matched to the catalog row for the same directory.
func checkoutIndexByRoot(overview indexer.FamiliesOverview) map[string]indexer.CheckoutOverview {
	out := make(map[string]indexer.CheckoutOverview)
	for _, family := range overview.Families {
		for _, co := range family.Checkouts {
			if co.RootPath == "" {
				continue
			}
			out[canonicalRoot(co.RootPath)] = co
		}
	}
	return out
}

// canonicalRoot is the comparison key for a checkout root: absolute and
// symlink-resolved where possible, so `/home/me/repo` and `/home/me/repo/`
// and a symlinked home all find the same row.
func canonicalRoot(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// printDoctorLegacy renders the human section. Every finding is a warning and
// names the command that resolves it, because a listing whose next step is
// undocumented reads as a problem the user cannot act on.
func printDoctorLegacy(w io.Writer, state DoctorLegacyState) {
	fmt.Fprintln(w, "Gortex doctor — legacy state:")
	if !state.Checked {
		fmt.Fprintf(w, "  %s %s\n\n", glyphAbsent, state.Skipped)
		return
	}

	if len(state.DedicatedCorpora) == 0 && len(state.Unattributed) == 0 && len(state.Intentless) == 0 {
		fmt.Fprintf(w, "  %s no legacy state found\n\n", glyphCheck)
		return
	}

	if len(state.DedicatedCorpora) > 0 {
		fmt.Fprintf(w, "  %s %d dedicated worktree corpora (each a full index with its own watcher)\n",
			glyphWarn, len(state.DedicatedCorpora))
		for _, c := range state.DedicatedCorpora {
			// RepoPrefix is already redactor-applied by the collector, so the
			// human rendering no longer prints a raw identifier here.
			name := c.RepoPrefix
			if name == "" {
				name = c.GraphID
			}
			flags := make([]string, 0, 3)
			if c.GraphState != "" {
				flags = append(flags, "state="+c.GraphState)
			}
			if c.CoordinatorLive {
				flags = append(flags, "coordinator live")
			}
			if len(c.Intents) > 0 {
				flags = append(flags, "intents="+strings.Join(c.Intents, ","))
			}
			suffix := ""
			if len(flags) > 0 {
				suffix = " (" + strings.Join(flags, ", ") + ")"
			}
			fmt.Fprintf(w, "    %s %s%s\n", name, doctorPath(c.Root), suffix)
			if c.FamilyPrimary {
				fmt.Fprintf(w, "      %s this corpus is also its family's primary corpus — removing it is not a simple cleanup\n", glyphWarn)
			}
		}
		fmt.Fprintln(w, "    fix: `gortex untrack <path>` removes one; `gortex track <path>` confirms it as intentional")
		fmt.Fprintln(w)
	}

	if len(state.Unattributed) > 0 {
		fmt.Fprintf(w, "  %s %d unattributed config entries (no cli_track or mcp_track intent)\n",
			glyphWarn, len(state.Unattributed))
		for _, e := range state.Unattributed {
			note := ""
			if e.LikelyAutoIndexed {
				note = fmt.Sprintf("  likely auto-indexed (intent created %s after the checkout)", e.Gap)
			} else if e.Gap != "" {
				note = fmt.Sprintf("  intent created %s after the checkout", e.Gap)
			}
			fmt.Fprintf(w, "    %s%s\n", e.Path, note)
		}
		fmt.Fprintln(w, "    fix: `gortex track <path>` records a cli_track intent and clears the warning;")
		fmt.Fprintln(w, "         `gortex untrack <path>` removes the entry")
		fmt.Fprintln(w)
	}

	if len(state.Intentless) > 0 {
		fmt.Fprintf(w, "  %s %d configured entries with no active intent\n", glyphWarn, len(state.Intentless))
		for _, e := range state.Intentless {
			fmt.Fprintf(w, "    %s\n", e.Path)
		}
		fmt.Fprintln(w, "    fix: `gortex track <path>` to confirm it, or `gortex untrack <path>` to remove it")
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "  Known false positives: `gortex track` run while the daemon was down,")
	fmt.Fprintln(w, "  `gortex init`, a hand edit to config.yaml, and a repo tracked before the")
	fmt.Fprintln(w, "  checkout catalog existed. These are warnings; the exit status is unchanged.")
	fmt.Fprintln(w)
}
