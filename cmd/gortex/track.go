package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
)

var (
	trackName        string
	trackAsWorktree  bool
	trackWait        bool
	trackWaitTimeout time.Duration
	untrackConfirm   bool
	untrackFormat    string
)

// untrackDaemonTool is the daemon-tool relay seam. Untrack goes through the
// tool rather than the control socket because what an untrack does depends on
// the family: a checkout the family can still serve is demoted outright, and a
// plan that removes rows is previewed and needs --confirm. Both decisions are
// the tool's, so the CLI and an agent see the same one.
var untrackDaemonTool = requireDaemonTool

// Injectable seams keep the --wait orchestration testable without a live
// daemon. The real notification receives the absolute deadline so dialing and
// the control round trip consume one shared budget.
var (
	trackStatusFn            = fetchDaemonStatusForCLI
	trackEnsureDaemonReadyFn = ensureDaemonReady
	trackNotifyDaemonTrackFn = notifyDaemonTrack
)

// trackPollInterval is how often --wait re-queries the daemon. A package var so
// tests can drop it to a sub-millisecond tick instead of waiting whole seconds.
var trackPollInterval = time.Second

var trackCmd = &cobra.Command{
	Use:   "track <path>",
	Short: "Add a repository to the tracked workspace",
	Long:  "Resolves the path to absolute, validates it exists, and adds it to the global config.",
	Args:  cobra.ExactArgs(1),
	RunE:  runTrack,
}

var untrackCmd = &cobra.Command{
	Use:   "untrack <path>",
	Short: "Remove a repository from the tracked workspace",
	Long: `Resolves the path and removes the matching entry from the global config.

Against a running daemon what happens depends on what the checkout's family can
still serve it from. A checkout another corpus can serve is demoted into the
family's automatic lane and the call runs outright. A plan that removes rows —
a primary corpus with everything composed over it, or a checkout with nowhere
to be demoted to — is previewed instead, and runs only with --confirm.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runUntrack,
}

func init() {
	trackCmd.Flags().StringVar(&trackName, "name", "",
		"Explicit repo prefix override (default: directory basename)")
	trackCmd.Flags().BoolVar(&trackAsWorktree, "as-worktree", false,
		"Track a linked git worktree as an independent instance even when its repo is already tracked elsewhere")
	trackCmd.Flags().BoolVar(&trackWait, "wait", false,
		"Block until the daemon has indexed the repo and the graph is queryable (useful for CI / one-shot scripts)")
	trackCmd.Flags().DurationVar(&trackWaitTimeout, "wait-timeout", 10*time.Minute,
		"With --wait, fail if indexing has not settled within this duration (0 = wait forever)")
	untrackCmd.Flags().BoolVar(&untrackConfirm, "confirm", false,
		"Run a plan that removes rows. Without it such a plan is only previewed.")
	untrackCmd.Flags().StringVar(&untrackFormat, "format", "text", "output format: text|json")
	rootCmd.AddCommand(trackCmd)
	rootCmd.AddCommand(untrackCmd)
}

// declaredWorkspace reads the `workspace:` slug from a repo's own
// `.gortex.yaml`, or "" when the file is absent or declares none. Used by
// the daemon-less track path to derive a stable worktree-instance prefix
// the same way the daemon would.
func declaredWorkspace(repoPath string) string {
	cfgFile := filepath.Join(repoPath, ".gortex.yaml")
	if _, err := os.Stat(cfgFile); err != nil {
		return ""
	}
	cfg, err := config.Load(cfgFile)
	if err != nil || cfg == nil {
		return ""
	}
	return cfg.Workspace
}

func runTrack(cmd *cobra.Command, args []string) error {
	rawPath := args[0]
	w := cmd.ErrOrStderr()

	// Resolve to absolute path. Normalise the volume (upper-case a Windows
	// drive letter) for this NEW entry so it converges with os.Getwd's
	// convention — the volume is never part of a repo basename, so this is
	// cosmetic and cannot rotate a repo prefix. No-op on POSIX.
	absPath, err := filepath.Abs(rawPath)
	if err != nil {
		return fmt.Errorf("resolving path %s: %w", rawPath, err)
	}
	absPath = pathkey.NormalizeVolume(absPath)
	absPath = pathkey.CanonicalExistingRoot(absPath)

	// Validate path exists and is a directory.
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("path does not exist: %s", absPath)
	}
	if !info.IsDir() {
		return fmt.Errorf("path is not a directory: %s", absPath)
	}

	emitTrackBanner(w, absPath, daemon.IsRunning())

	// 1. Config write FIRST. Registering a repo is a config operation
	//    that always succeeds with no daemon, so `gortex track` is
	//    offline-safe — the durable source of truth is written before any
	//    daemon contact.
	gc, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}
	// Heal any pre-existing duplicate case-variant entries so the track
	// path (and the persisted config) sees a clean list (#270).
	healDuplicateRepos(gc, nil)
	already := false
	for _, existing := range gc.Repos {
		if existingAbs, _ := filepath.Abs(existing.Path); pathkey.SamePathIdentity(existingAbs, absPath) {
			already = true
			break
		}
	}
	var prefix string
	if already {
		prefix = config.ResolvePrefix(config.RepoEntry{Path: absPath, Name: trackName})
	} else {
		entry := config.RepoEntry{Path: absPath}
		switch {
		case trackName != "":
			entry.Name = trackName
		case trackAsWorktree:
			// The AsWorktree flag is not persisted, so pin the derived
			// instance prefix as the entry Name now. The daemon reproduces
			// it intrinsically for a declared-workspace worktree, but a
			// forced (branch-tagged) instance must be recorded here.
			base := config.ResolvePrefix(entry)
			if name, sep := indexer.WorktreeInstanceName(absPath, base, declaredWorkspace(absPath), true); sep {
				entry.Name = name
			}
		}
		if err := gc.AddRepo(entry); err != nil {
			return err
		}
		if err := gc.Save(); err != nil {
			return fmt.Errorf("saving global config: %w", err)
		}
		prefix = config.ResolvePrefix(entry)
	}

	// 2. Best-effort daemon: bring it up (single-flight) and hand it the
	//    repo so indexing starts now. For --wait, one absolute deadline spans
	//    readiness, the track control RPC, and the settle poll. The config write
	//    above remains durable even when that wait budget expires.
	waitDeadline := time.Time{}
	if trackWait && trackWaitTimeout > 0 {
		waitDeadline = time.Now().Add(trackWaitTimeout)
	}
	ensureFn := trackEnsureDaemonReadyFn
	decision, timedOut := beforeTrackDeadline(waitDeadline, func() daemonDecision {
		return ensureFn(daemon.ParseAutostart())
	})
	if timedOut || trackDeadlineExpired(waitDeadline) {
		return trackWaitTimeoutError(absPath, trackWaitTimeout, "waiting for daemon readiness")
	}
	if decision != daemonUnavailable {
		notifyFn := trackNotifyDaemonTrackFn
		notifyErr, notifyTimedOut := beforeTrackDeadline(waitDeadline, func() error {
			return notifyFn(absPath, waitDeadline)
		})
		if notifyTimedOut || trackDeadlineExpired(waitDeadline) {
			return trackWaitTimeoutError(absPath, trackWaitTimeout, "waiting for the daemon to accept the repository")
		}
		if notifyErr == nil {
			// --wait blocks until the daemon has actually indexed the repo so
			// a following `gortex analyze` / query sees a complete graph.
			if trackWait {
				if werr := waitForRepoIndexedUntil(w, absPath, waitDeadline, trackWaitTimeout); werr != nil {
					return werr
				}
			}
			emitTrackSummary(w, absPath, trackResult{viaDaemon: true, prefix: prefix, alreadyTracked: already})
			return nil
		} else if trackWait {
			// The repo is persisted, but --wait promised a queryable graph we
			// can no longer deliver — surface that rather than a soft success.
			return fmt.Errorf("--wait: daemon did not accept the repo (it remains tracked in config): %w", notifyErr)
		}
	} else if trackWait {
		return fmt.Errorf("--wait requires a running daemon, but none is available; the repository remains tracked in config — start it with `gortex daemon start --detach`")
	}

	// 3. Daemon unavailable (autostart off, spawn failed/timed out, or
	//    the control hop failed). The repo is tracked on disk; tell the
	//    user the daemon will pick it up later — success, not error.
	emitTrackSummary(w, absPath, trackResult{configOnly: true, repoCount: len(gc.Repos), prefix: prefix, alreadyTracked: already})
	return nil
}

// repoNodeCount returns the indexed node count for the repo at absPath in the
// daemon status, or -1 if the daemon has not registered the repo yet.
func repoNodeCount(st daemon.StatusResponse, absPath string) int {
	for _, r := range st.TrackedRepos {
		if ra, err := filepath.Abs(r.Path); err == nil && pathkey.EqualPaths(ra, absPath) {
			return r.Nodes
		}
	}
	return -1
}

// beforeTrackDeadline runs one potentially blocking wait stage against the
// command's shared absolute deadline. The result channel is buffered so a
// bounded caller can return while an uncancellable readiness probe winds down.
func beforeTrackDeadline[T any](deadline time.Time, fn func() T) (T, bool) {
	if deadline.IsZero() {
		return fn(), false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		var zero T
		return zero, true
	}
	result := make(chan T, 1)
	go func() { result <- fn() }()
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case value := <-result:
		return value, false
	case <-timer.C:
		var zero T
		return zero, true
	}
}

func trackDeadlineExpired(deadline time.Time) bool {
	return !deadline.IsZero() && time.Until(deadline) <= 0
}

func trackWaitTimeoutError(absPath string, timeout time.Duration, phase string) error {
	return fmt.Errorf("--wait: timed out after %s %s for %s; repository is tracked in config and daemon work may continue", timeout, phase, absPath)
}

// indexSettled reports whether the repo at absPath looks fully indexed: the
// daemon has registered it, its non-negative node count has stopped moving
// (equal to prevNodes), and the graph is resolved (Ready). Zero is a valid
// settled count for an empty repository; -1 exclusively means not registered.
// Requiring a stable count across two polls is a per-repo heuristic that holds
// even on a warm multi-repo daemon where the global Ready flag is insufficient.
func indexSettled(st daemon.StatusResponse, absPath string, prevNodes int) (settled bool, nodes int) {
	nodes = repoNodeCount(st, absPath)
	if nodes < 0 || !st.Ready {
		return false, nodes
	}
	return nodes == prevNodes, nodes
}

// waitForRepoIndexed polls the daemon until the repo at absPath has settled
// (see indexSettled) or timeout elapses. timeout <= 0 waits forever.
func waitForRepoIndexed(w io.Writer, absPath string, timeout time.Duration) error {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	return waitForRepoIndexedUntil(w, absPath, deadline, timeout)
}

type trackStatusResult struct {
	status daemon.StatusResponse
	err    error
}

// waitForRepoIndexedUntil is the runTrack form: deadline was created before
// daemon readiness and registration, so status RPCs and poll sleeps only get
// the budget left over from those earlier stages.
func waitForRepoIndexedUntil(w io.Writer, absPath string, deadline time.Time, timeout time.Duration) error {
	tr := progress.NewTracker(w)
	if noProgress {
		tr = progress.NewTracker(w, progress.WithoutAnimation())
	}
	tr.Start("waiting for indexing to settle (--wait)")
	step := tr.StartStep("indexing " + filepath.Base(absPath))
	step.SetUnit("nodes")

	failTimeout := func() error {
		err := trackWaitTimeoutError(absPath, timeout, "waiting for indexing to settle")
		tr.Fail(err)
		return err
	}
	prevNodes := -1
	statusFn := trackStatusFn
	for {
		statusResult, timedOut := beforeTrackDeadline(deadline, func() trackStatusResult {
			st, err := statusFn()
			return trackStatusResult{status: st, err: err}
		})
		if timedOut || trackDeadlineExpired(deadline) {
			return failTimeout()
		}
		if statusResult.err == nil {
			settled, nodes := indexSettled(statusResult.status, absPath, prevNodes)
			if nodes > 0 {
				step.Progress(int64(nodes), 0)
			}
			if settled {
				step.DoneAs("index settled")
				tr.Done("indexed", humanizeInt(nodes)+" nodes")
				return nil
			}
			prevNodes = nodes
		}
		if trackDeadlineExpired(deadline) {
			return failTimeout()
		}
		delay := trackPollInterval
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return failTimeout()
			}
			if delay > remaining {
				delay = remaining
			}
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			<-timer.C
		}
	}
}

// trackResult bundles the outcome of runTrack so emitTrackSummary can pick the
// right summary card variant without re-deriving facts from the call sites.
type trackResult struct {
	viaDaemon      bool
	configOnly     bool
	alreadyTracked bool
	repoCount      int    // tracked repo count *after* this call (configOnly path)
	prefix         string // the prefix the repo was registered under (may differ from basename for worktree instances)
}

// emitTrackBanner prints the gortex mesh banner + subtitle indicating which
// path will be tracked and whether a daemon will pick it up immediately. Only
// emitted when stderr is a TTY — non-TTY runs (CI scripts) stay quiet so
// existing piped output still parses.
// notifyDaemonTrack hands the repo to a running daemon via the control
// socket. It returns an error when the daemon can't be reached or rejects
// the request; the caller treats that as non-fatal because the config
// write already persisted the repo.
func notifyDaemonTrack(absPath string, deadline time.Time) error {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return err
	}
	defer c.Close()

	controlTimeout := time.Duration(0)
	if !deadline.IsZero() {
		controlTimeout = time.Until(deadline)
		if controlTimeout <= 0 {
			return fmt.Errorf("track wait deadline expired before the control request")
		}
	}
	resp, err := c.ControlWithTimeout(daemon.ControlTrack, daemon.TrackParams{
		Path:       absPath,
		Name:       trackName,
		AsWorktree: trackAsWorktree,
	}, controlTimeout)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("track rejected: %s %s", resp.ErrorCode, resp.ErrorMsg)
	}
	return nil
}

func emitTrackBanner(w io.Writer, absPath string, daemonUp bool) {
	if !progress.IsTTY(w) {
		return
	}
	sub := "Adding repository to the workspace."
	if daemonUp {
		sub = "Adding repository — daemon is up, indexing will start immediately."
	}
	banner := tui.Banner{
		Title:    "gortex track",
		Subtitle: sub,
	}.Render()
	fmt.Fprintln(w)
	fmt.Fprintln(w, banner)
	fmt.Fprintln(w, "  "+progress.Row("path", absPath, 6))
	fmt.Fprintln(w)
}

// emitTrackSummary prints the post-track summary card. Three variants: via
// daemon (indexing is live), config-only (daemon will pick it up later), or
// already tracked (idempotent no-op).
func emitTrackSummary(w io.Writer, absPath string, r trackResult) {
	if !progress.IsTTY(w) {
		// Preserve the legacy one-line output for non-TTY callers so
		// scripts that grep this line keep working.
		suffix := ""
		if r.prefix != "" && r.prefix != filepath.Base(absPath) {
			suffix = fmt.Sprintf(" as %q", r.prefix)
		}
		switch {
		case r.viaDaemon:
			fmt.Fprintf(w, "[gortex] tracked %s%s (via daemon)\n", absPath, suffix)
		case r.alreadyTracked:
			fmt.Fprintf(w, "[gortex] already tracked: %s\n", absPath)
		case r.configOnly:
			fmt.Fprintf(w, "[gortex] tracked %s%s (config only — start daemon to index)\n", absPath, suffix)
		}
		return
	}

	var stats []string
	if r.prefix != "" && r.prefix != filepath.Base(absPath) {
		stats = append(stats, progress.Stat("prefix", r.prefix, progress.StatGood))
	}
	switch {
	case r.viaDaemon:
		stats = append(stats, progress.Stat("via daemon", "", progress.StatGood))
		stats = append(stats, progress.Stat("indexing", "live", progress.StatGood))
	case r.alreadyTracked:
		stats = append(stats, progress.Stat("already", "tracked", progress.StatNeutral))
		if r.repoCount > 0 {
			stats = append(stats, progress.Stat(strconv.Itoa(r.repoCount), "tracked repos", progress.StatNeutral))
		}
	case r.configOnly:
		stats = append(stats, progress.Stat("written to", "global config", progress.StatGood))
		stats = append(stats, progress.Stat("daemon", "offline — start to index", progress.StatWarn))
	}

	fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render(absPath))
	fmt.Fprintln(w, "     "+progress.StatStrip(stats...))

	switch {
	case r.viaDaemon:
		fmt.Fprintln(w, "\n     "+progress.Caption("watch progress: `gortex daemon status --watch`"))
	case r.configOnly:
		fmt.Fprintln(w, "\n     "+progress.Caption("next: `gortex daemon start --detach` to index this repo"))
	}
	fmt.Fprintln(w)
}

func runUntrack(cmd *cobra.Command, args []string) error {
	rawPath := args[0]
	w := cmd.ErrOrStderr()

	// Argument can be either a path or a repo prefix; the daemon accepts
	// both. Resolve to absolute only when it looks like a path (starts
	// with / or . or has a path separator); otherwise treat as a prefix.
	target := rawPath
	if filepath.IsAbs(rawPath) || rawPath == "." || rawPath == ".." {
		abs, err := filepath.Abs(rawPath)
		if err != nil {
			return fmt.Errorf("resolving path %s: %w", rawPath, err)
		}
		target = abs
	} else if info, statErr := os.Stat(rawPath); statErr == nil && info.IsDir() {
		// A relative arg that names an existing directory (e.g. `foo/bar`
		// from cwd) is a path, not a prefix — absolutise it so it resolves
		// against the tracked roots. A bare prefix that names no directory
		// keeps its as-is behaviour.
		if abs, err := filepath.Abs(rawPath); err == nil {
			target = abs
		}
	}

	emitUntrackBanner(w, target, daemon.IsRunning())

	if index, reachable := untrackDaemonIndex(target); reachable {
		return untrackViaDaemon(cmd, w, index, target)
	}

	// Standalone fallback.
	gc, err := config.LoadGlobal()
	if err != nil {
		return fmt.Errorf("loading global config: %w", err)
	}
	if err := gc.RemoveRepo(target); err != nil {
		return err
	}
	if err := gc.Save(); err != nil {
		return fmt.Errorf("saving global config: %w", err)
	}
	emitUntrackSummary(w, target, untrackResult{configOnly: true, repoCount: len(gc.Repos)})
	return nil
}

// untrackDaemonIndex picks the repository the untrack tool call is routed
// through, and reports false when no running daemon owns it.
//
// False is what keeps `gortex untrack` offline-safe: a repo the daemon never
// loaded is still a line in the user's config, and removing that line is
// exactly what the standalone fallback is for.
func untrackDaemonIndex(target string) (string, bool) {
	if !daemon.IsRunning() {
		return "", false
	}
	// A bare repo prefix names no directory, so the call is routed through the
	// caller's own working directory instead; the prefix rides as the tool's
	// argument either way.
	index := target
	if !filepath.IsAbs(target) {
		index = "."
	}
	abs, err := filepath.Abs(index)
	if err != nil {
		return "", false
	}
	if !daemonOwnsRepo(abs) {
		return "", false
	}
	return index, true
}

// untrackViaDaemon runs the untrack tool and renders what it decided: a
// preview the caller has to confirm, or the plan it carried out.
func untrackViaDaemon(cmd *cobra.Command, w io.Writer, index, target string) error {
	toolArgs := map[string]any{"path": target}
	if untrackConfirm {
		toolArgs["confirm"] = true
	}
	raw, err := untrackDaemonTool(index, "untrack_repository", toolArgs)
	if err != nil {
		return err
	}
	if untrackFormat == "json" {
		return emitDaemonJSON(cmd, raw)
	}
	var payload checkoutOutcome
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Status == "" {
		// Anything that is not one of this tool's own answers — the
		// not-tracked guidance, a shape a newer daemon added — is printed as
		// it came rather than squeezed into a summary card.
		return emitDaemonJSON(cmd, raw)
	}
	if payload.Status == "preview" {
		renderCheckoutOutcome(cmd.OutOrStdout(), target, payload,
			"gortex untrack "+target+" --confirm")
		return nil
	}
	emitUntrackSummary(w, target, untrackResult{viaDaemon: true, demoted: payload.Demoted})
	return nil
}

// untrackResult mirrors trackResult — kept distinct so the two summaries can
// drift apart later (e.g. untrack might want to show whether the repo had
// pending edits before removal) without one breaking the other.
type untrackResult struct {
	viaDaemon  bool
	configOnly bool
	// demoted reports that the checkout kept its identity and moved to the
	// family's automatic lane instead of being removed.
	demoted   bool
	repoCount int // tracked repo count *after* removal (configOnly path)
}

// emitUntrackBanner prints the gortex mesh banner + subtitle indicating which
// path is being untracked and where the change will land (daemon vs config).
func emitUntrackBanner(w io.Writer, target string, daemonUp bool) {
	if !progress.IsTTY(w) {
		return
	}
	sub := "Removing repository from the workspace."
	if daemonUp {
		sub = "Removing repository — daemon will drop the index immediately."
	}
	banner := tui.Banner{
		Title:    "gortex untrack",
		Subtitle: sub,
	}.Render()
	fmt.Fprintln(w)
	fmt.Fprintln(w, banner)
	fmt.Fprintln(w, "  "+progress.Row("target", target, 8))
	fmt.Fprintln(w)
}

// emitUntrackSummary prints the post-untrack summary card. Same TTY vs.
// non-TTY split as the track sibling so script parsers keep working.
func emitUntrackSummary(w io.Writer, target string, r untrackResult) {
	if !progress.IsTTY(w) {
		switch {
		case r.demoted:
			fmt.Fprintf(w, "[gortex] demoted %s to its family's automatic lane (via daemon)\n", target)
		case r.viaDaemon:
			fmt.Fprintf(w, "[gortex] untracked %s (via daemon)\n", target)
		case r.configOnly:
			fmt.Fprintf(w, "[gortex] untracked %s (config only)\n", target)
		}
		return
	}

	var stats []string
	switch {
	case r.demoted:
		stats = append(stats, progress.Stat("via daemon", "", progress.StatGood))
		stats = append(stats, progress.Stat("served from", "the family primary", progress.StatGood))
	case r.viaDaemon:
		stats = append(stats, progress.Stat("via daemon", "", progress.StatGood))
		stats = append(stats, progress.Stat("index", "dropped", progress.StatGood))
	case r.configOnly:
		stats = append(stats, progress.Stat("removed from", "global config", progress.StatGood))
		if r.repoCount >= 0 {
			stats = append(stats, progress.Stat(strconv.Itoa(r.repoCount), "repos remain", progress.StatNeutral))
		}
	}

	fmt.Fprintln(w, "  "+progress.StyleOK.Render("✓")+"  "+progress.StyleStrong.Render(target))
	fmt.Fprintln(w, "     "+progress.StatStrip(stats...))
	fmt.Fprintln(w)
}
