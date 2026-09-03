package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/pathkey"
)

// daemonRoutingProbeTimeout bounds the "does the daemon own this repo?" probe
// that precedes every CLI graph query. Short on purpose: the answer only picks
// an execution backend, and waiting on a busy daemon to learn it is strictly
// worse than getting on with the query.
const daemonRoutingProbeTimeout = 3 * time.Second

// daemonProbeIndeterminate reports whether a control probe failed in a way
// that means "the daemon is busy", as opposed to giving a real answer. Covers
// both ends: the client's own deadline, and the daemon answering that it could
// not finish in time.
func daemonProbeIndeterminate(err error, resp daemon.ControlResponse) bool {
	if errors.Is(err, daemon.ErrDaemonUnresponsive) {
		return true
	}
	return err == nil && !resp.OK && resp.ErrorCode == daemon.ErrTimeout
}

// ErrNoExecutor signals that no warm daemon owns the repo and no explicit
// daemonless path (--oneshot) was selected — the caller decides whether to
// fall back (Stage 1) or refuse (Stage 3).
var ErrNoExecutor = errors.New("no warm daemon and --oneshot not set")

// ErrRepoNotTracked is the typed form of the daemon's repo_not_tracked
// refusal, distinguished so a CLI command can fall back rather than treat
// it as a hard error.
var ErrRepoNotTracked = errors.New("repository not tracked by the daemon")

const (
	cliLegacyToolSurface = "core"
	cliLegacyToolMode    = "defer"
)

// cliExecutor runs a registered MCP tool by name and returns its raw
// result JSON (the same payload the MCP server returns).
type cliExecutor interface {
	CallTool(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error)
	Close() error
}

// daemonExecutor relays a one-shot tools/call over the daemon's AF_UNIX
// ModeMCP channel — the same warm graph the editor proxies hit, no cold
// index. It pins the JSON wire format so per-tool decoding is stable.
type daemonExecutor struct {
	client         *daemon.Client
	nextID         int
	pinJSONDefault bool
}

func (d *daemonExecutor) CallTool(_ context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	d.nextID++
	frame, err := buildToolCallFrameWithDefault(d.nextID, tool, args, d.pinJSONDefault)
	if err != nil {
		return nil, err
	}
	if err := d.client.WriteMCPFrame(frame); err != nil {
		return nil, err
	}
	resp, err := d.client.ReadMCPFrame()
	if err != nil {
		return nil, err
	}
	return extractToolResult(resp)
}

// buildToolCallFrame constructs the JSON-RPC tools/call frame, pinning the
// JSON wire format so the daemon's per-client GCX/TOON auto-selection does
// not defeat the per-tool decode.
func buildToolCallFrame(id int, tool string, args map[string]any) ([]byte, error) {
	return buildToolCallFrameWithDefault(id, tool, args, true)
}

func buildToolCallFrameWithDefault(id int, tool string, args map[string]any, pinJSON bool) ([]byte, error) {
	if args == nil {
		args = map[string]any{}
	}
	// Default to JSON, but honour a caller-provided format (e.g.
	// mermaid / dot for diagram output) so the CLI can request the
	// daemon's other renderers.
	if _, ok := args["format"]; pinJSON && !ok {
		args["format"] = "json"
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	})
}

func (d *daemonExecutor) Close() error { return d.client.Close() }

// extractToolResult unwraps a JSON-RPC tools/call response: a
// repo_not_tracked error maps to the typed sentinel, any other error is
// surfaced verbatim, and a success returns the tool's JSON payload (the
// text of the first content block).
func extractToolResult(resp []byte) (json.RawMessage, error) {
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				ErrorCode string `json:"error_code"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &rpc); err != nil {
		return nil, fmt.Errorf("decode daemon response: %w", err)
	}
	if rpc.Error != nil {
		if rpc.Error.Data.ErrorCode == "repo_not_tracked" {
			return nil, ErrRepoNotTracked
		}
		return nil, errors.New(rpc.Error.Message)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(rpc.Result, &res); err != nil || len(res.Content) == 0 {
		return rpc.Result, nil
	}
	payload := json.RawMessage(res.Content[0].Text)
	if res.IsError {
		return nil, errors.New(res.Content[0].Text)
	}
	return payload, nil
}

// resolveExecutor decides where a CLI graph query runs. This Stage-1 slice
// covers the daemon-first case (a warm daemon that owns the repo) and the
// no-executor case; --oneshot and autostart land with the shared
// constructor and the autostart primitive.
func resolveExecutor(repoPath string) (cliExecutor, error) {
	return resolveExecutorWithToolSurface(repoPath, cliLegacyToolSurface, cliLegacyToolMode)
}

// resolveExecutorWithToolSurface is the daemon-first executor with an
// optional per-connection MCP surface. Ordinary CLI verbs explicitly request
// core/defer so legacy tool names keep their historical semantics regardless
// of daemon/client defaults; compact calls request facade-v1/hide. Neither path
// changes the shared daemon or any other session.
func resolveExecutorWithToolSurface(repoPath, tools, toolsMode string) (cliExecutor, error) {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		abs = repoPath
	}
	if !daemon.IsRunning() {
		return nil, ErrNoExecutor
	}
	switch verdict := probeCWDReach(abs); verdict.reach {
	case reachDaemon:
	case reachUnboundWorktree:
		// The daemon knows the family and would serve the worktree once it is
		// bound; refusing with ErrNoExecutor here would hand the caller the
		// `gortex track <worktree>` remedy, which is the one thing that must
		// not happen for a worktree.
		return nil, worktreeCWDErr(abs, verdict.family, verdict.familyRepo)
	default:
		return nil, ErrNoExecutor
	}
	c, err := daemon.Dial(daemon.Handshake{
		Mode:       daemon.ModeMCP,
		ClientName: "cli",
		CWD:        abs,
		Tools:      tools,
		ToolsMode:  toolsMode,
	})
	if err != nil {
		return nil, ErrNoExecutor
	}
	return &daemonExecutor{
		client:         c,
		pinJSONDefault: tools != gortexmcp.FacadeSurfaceVersion,
	}, nil
}

// cwdReach is how the running daemon reaches a working directory.
type cwdReach int

const (
	// reachNone — no tracked repo and no registered checkout covers the path.
	reachNone cwdReach = iota
	// reachDaemon — the daemon can answer for the path.
	reachDaemon
	// reachUnboundWorktree — the path is a linked git worktree of a tracked
	// repository that the daemon has not bound to a checkout view.
	reachUnboundWorktree
)

// cwdVerdict is what the routing probe decided about one working directory.
type cwdVerdict struct {
	reach cwdReach
	// family is the linked-worktree family the path belongs to. Set only for
	// reachUnboundWorktree.
	family worktreeFamily
	// familyRepo is the tracked working copy that proves the daemon knows the
	// family. It is the repository the remedy names and the one the checkout
	// verbs relay through — never the worktree itself, which must not be
	// tracked as a repository of its own.
	familyRepo string
}

// daemonOwnsRepo reports whether the running daemon can answer for abs.
func daemonOwnsRepo(abs string) bool {
	return probeCWDReach(abs).reach == reachDaemon
}

// probeCWDReach asks the running daemon how it reaches abs.
//
// This runs ahead of every CLI graph query, which made it the single point
// where a busy daemon stalled the whole CLI: Status serialises behind the
// controller mutex that track / reload / enrichment hold for minutes, and the
// call had no bound on either end.
//
// It gets a short budget, and when that budget expires it FAILS OPEN — the
// answer is unknown, not "no". Treating an indeterminate probe as "not ours"
// would be a lie with an actively harmful remedy: the caller would be told
// "the daemon does not track <path> — add it with `gortex track <path>`",
// when the daemon does track it and `track` is itself the unbounded operation
// holding the lock. Proceeding to the MCP path costs nothing to be wrong
// about: that path does not need the controller mutex, and a genuinely
// untracked repo comes back as repo_not_tracked, which surfaces the same
// message this probe would have produced.
func probeCWDReach(abs string) cwdVerdict {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return cwdVerdict{reach: reachNone}
	}
	defer c.Close()
	resp, err := c.ControlWithTimeout(daemon.ControlStatus, nil, daemonRoutingProbeTimeout)
	if daemonProbeIndeterminate(err, resp) {
		fmt.Fprintf(os.Stderr,
			"[gortex] daemon did not answer within %s (a track / reload / enrichment may be holding it) — asking it anyway\n",
			daemonRoutingProbeTimeout)
		return cwdVerdict{reach: reachDaemon}
	}
	if err != nil || !resp.OK {
		return cwdVerdict{reach: reachNone}
	}
	var st daemon.StatusResponse
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		return cwdVerdict{reach: reachNone}
	}
	if trackedReposReach(st, abs) {
		return cwdVerdict{reach: reachDaemon}
	}
	// abs lies in no tracked repo root, which is the ordinary shape of a
	// linked worktree: the family is tracked through its main checkout and the
	// worktree is a directory the repository registry never covers. The
	// catalog binds such a path to its family's view, exactly as an MCP
	// session cwd is bound, so the daemon is still the one that answers.
	if checkoutBindsCWD(c, abs) {
		return cwdVerdict{reach: reachDaemon}
	}
	// A worktree of a tracked family that no checkout is bound to yet. The
	// query cannot be served, but the caller must not be told to track it.
	if fam, ok := linkedWorktreeAt(abs); ok {
		if repo := familyRepoIn(st, fam); repo != "" {
			return cwdVerdict{reach: reachUnboundWorktree, family: fam, familyRepo: repo}
		}
	}
	return cwdVerdict{reach: reachNone}
}

// trackedRootContains reports whether p lies inside a tracked repo root.
func trackedRootContains(st daemon.StatusResponse, p string) bool {
	for _, repo := range st.TrackedRepos {
		if repo.Path != "" && pathkey.CanonicalHasPathPrefix(p, repo.Path) {
			return true
		}
	}
	return false
}

// trackedReposReach adds reverse containment to trackedRootContains: p is a
// root ABOVE tracked repos. The MCP dispatcher serves this shape
// (cmd/gortex/daemon_mcp.go cwdReachable), and the two gates answering
// differently for the same directory is a difference the user experiences as
// "the agent can query this folder but the CLI cannot".
func trackedReposReach(st daemon.StatusResponse, p string) bool {
	if trackedRootContains(st, p) {
		return true
	}
	for _, repo := range st.TrackedRepos {
		if repo.Path != "" && pathkey.CanonicalHasPathPrefix(repo.Path, p) {
			return true
		}
	}
	return false
}

// checkoutBindsCWD asks the daemon whether abs sits inside a registered
// checkout — a working copy the catalog binds to its family's view.
//
// file_coverage is the control-surface answer to "which graph serves this
// path", and its view block names the checkout that owns the path. It has to
// be the control surface: the tool surface is what this pre-flight guards, so
// asking it here would be circular.
//
// A daemon too old to know the verb reports no checkout, leaving the caller
// with exactly the verdict it reached before this arm existed. A daemon too
// BUSY to answer inside the routing budget reports a binding instead: that is
// the fail-open the status probe above takes, for the same reason. Silence is
// not evidence that the worktree is unbound, and answering it with the
// reconcile remedy would be the same lie in a new place.
func checkoutBindsCWD(c *daemon.Client, abs string) bool {
	resp, err := c.ControlWithTimeout(daemon.ControlFileCoverage,
		daemon.FileCoverageParams{Path: abs}, daemonRoutingProbeTimeout)
	if daemonProbeIndeterminate(err, resp) {
		return true
	}
	if err != nil || !resp.OK {
		return false
	}
	var out daemon.FileCoverageResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return false
	}
	return out.View != nil && out.View.CheckoutID != ""
}

// worktreeFamily identifies the set of working copies a linked git worktree
// shares a git directory with.
type worktreeFamily struct {
	// mainRepo is the family's main checkout — the directory holding the
	// shared git directory. Empty when the family has none: a bare hub
	// (`git clone --bare`) owns worktrees but is nobody's working copy, so
	// there is no main checkout to name, let alone to track.
	mainRepo string
	// commonDir is the shared git directory every worktree of the family
	// resolves through. It is the family's identity, and the only handle the
	// error message has when mainRepo is empty.
	commonDir string
}

// linkedWorktreeAt describes the linked git worktree abs sits in, reporting
// false when it sits in none. A cwd is not necessarily a worktree root, so
// the nearest enclosing `.git` is what gets classified.
func linkedWorktreeAt(abs string) (worktreeFamily, bool) {
	for dir := filepath.Clean(abs); ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			info := indexer.ResolveWorktree(dir)
			if !info.IsWorktree {
				return worktreeFamily{}, false
			}
			fam := worktreeFamily{commonDir: info.GitCommonDir}
			// ResolveWorktree names a main checkout only when the shared git
			// directory is itself called `.git`; a bare hub's is not, and it
			// leaves MainRepoPath as the queried directory. A worktree is
			// never its own main checkout, so that answer is dropped.
			if info.MainRepoPath != dir {
				fam.mainRepo = info.MainRepoPath
			}
			return fam, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return worktreeFamily{}, false
		}
		dir = parent
	}
}

// familyRepoIn returns the tracked repository that belongs to fam, or "" when
// the daemon tracks none of the family's working copies.
//
// Membership is the shared git directory, not root containment: a family's
// tracked working copy can be its main checkout OR any sibling worktree, and
// a bare hub has no main checkout at all — a worktree is the only working
// copy such a family can ever offer.
func familyRepoIn(st daemon.StatusResponse, fam worktreeFamily) string {
	if fam.mainRepo != "" && trackedRootContains(st, fam.mainRepo) {
		return fam.mainRepo
	}
	if fam.commonDir == "" {
		return ""
	}
	for _, repo := range st.TrackedRepos {
		if repo.Path == "" {
			continue
		}
		if indexer.ResolveWorktree(repo.Path).GitCommonDir == fam.commonDir {
			return repo.Path
		}
	}
	return ""
}

// trackedFamilyRepo asks the running daemon which of fam's working copies it
// tracks. Used on the arms that have no probe verdict to hand.
func trackedFamilyRepo(fam worktreeFamily) string {
	if !daemon.IsRunning() {
		return ""
	}
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return ""
	}
	defer c.Close()
	resp, err := c.ControlWithTimeout(daemon.ControlStatus, nil, daemonRoutingProbeTimeout)
	if err != nil || !resp.OK {
		return ""
	}
	var st daemon.StatusResponse
	if err := json.Unmarshal(resp.Result, &st); err != nil {
		return ""
	}
	return familyRepoIn(st, fam)
}

// worktreeCWDErr explains a linked git worktree the daemon cannot answer for.
// familyRepo is the family's tracked working copy, or "" when it has none.
//
// The remedy is never `gortex track <worktree>`. A linked worktree registered
// as a repository of its own is indexed a second time and stops being served
// through its family, which is what the family model exists to prevent — and
// for a bare hub, whose worktrees have no main checkout above them, naming the
// worktree would also be naming it as its own main.
func worktreeCWDErr(worktree string, fam worktreeFamily, familyRepo string) error {
	if familyRepo != "" {
		return fmt.Errorf(
			"the gortex daemon tracks %s but has not bound the worktree %s to a view yet — run `gortex repos reconcile %s` and retry",
			familyRepo, worktree, familyRepo)
	}
	remedy := fmt.Sprintf("track its main checkout with `gortex track %s`", fam.mainRepo)
	if fam.mainRepo == "" {
		remedy = fmt.Sprintf(
			"track one of the family's own worktrees with `gortex track <checkout>` (its shared git directory %s is bare, so the family has no main checkout)",
			fam.commonDir)
	}
	if !daemon.IsRunning() {
		return fmt.Errorf(
			"no gortex daemon is running — start it with `gortex daemon start --detach`, then %s; %s is served through the family, not as a repository of its own",
			remedy, worktree)
	}
	return fmt.Errorf(
		"the gortex daemon does not track the family %s is a linked worktree of — %s; the worktree is then served through it",
		worktree, remedy)
}

// checkoutsRelayPath resolves the repository path the checkout verbs relay
// through.
//
// Those verbs exist to inspect and repair the binding between a working copy
// and its family, so the cwd whose binding is what's broken must not be the
// path that decides whether they may run: the routing pre-flight would refuse
// them with the very error that recommends them, and take the diagnostic verbs
// for that state down with it. An unbound worktree relays through the tracked
// working copy of its own family instead. Nothing but the daemon route depends
// on this path — every one of these verbs names its subject explicitly or asks
// about the whole catalog.
func checkoutsRelayPath(repoPath string) string {
	abs, err := filepath.Abs(repoPath)
	if err != nil || !daemon.IsRunning() {
		return repoPath
	}
	// Only a linked worktree can take the unbound shape. Classifying that on
	// the filesystem first keeps an ordinary cwd from paying for a second
	// routing probe, and keeps a worktree the daemon DOES serve on its own
	// cwd — relaying that one away would answer it in another view.
	if _, ok := linkedWorktreeAt(abs); !ok {
		return repoPath
	}
	if v := probeCWDReach(abs); v.reach == reachUnboundWorktree && v.familyRepo != "" {
		return v.familyRepo
	}
	return repoPath
}
