package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The isolated-daemon fixture shared by every opt-in end-to-end measurement in
// this package. It was extracted verbatim from
// issue767_idle_io_integration_test.go so the idle harness, the worktree
// readiness harness and the W8 sustained-workload harness drive one private
// daemon recipe instead of three drifting copies. Behaviour is unchanged; the
// only additions are the corpus hook (so a harness can supply a generated
// corpus in place of the 32-file default), the path-scoped removal wait, the
// child-pid accessor, and the extra rusage fields the sustained harness needs.
//
// Every process launched here owns private XDG directories, SQLite storage, a
// Git fixture and a private gitconfig. None of it ever addresses the user's
// daemon, store or configuration.

// issue767Corpus writes the repository contents a fixture starts from. It runs
// after the private gitconfig exists and before `git init`, so everything it
// writes lands in the fixture's first commit.
type issue767Corpus func(f *issue767Fixture)

type issue767Fixture struct {
	t                                    *testing.T
	binary, root, primary, linked, store string
	env                                  []string

	// mu guards every field a concurrent reader may touch while the fixture
	// starts, stops or restarts the child. The sustained harness samples the
	// child's process counters and its log size at 1 Hz from its own
	// goroutine, which runs across start() and stop(); without this the
	// sampler's pid/log reads race the start that installs them.
	mu         sync.Mutex
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	done       chan error
	log        *os.File
	run        int
	lastOutput string
	// clientCalls counts every invocation of the measured binary this fixture
	// made — the harness's own traffic against the daemon. An "idle" window is
	// only idle if this number does not move inside it, and a floor attributed
	// to the daemon while a client was polling it twelve times is not a floor.
	// It is counted in tryCommand because that is the single door every CLI
	// call goes through, including the ones awaitSymbol makes on the harness's
	// behalf, which a caller-side counter cannot see.
	clientCalls int
}

// issue767FixtureOption adjusts how the private daemon is configured before it
// is started. Options exist for the knobs a measurement must be able to state
// and vary; everything else about the isolation is fixed.
type issue767FixtureOption func(*issue767FixtureOptions)

// issue767ProductReconcileInterval selects the product's own reconcile
// interval by leaving GORTEX_RECONCILE_INTERVAL unset, so a confirmatory arm
// can be run at the default the shipped daemon uses.
const issue767ProductReconcileInterval = "default"

// issue767DefaultTestReconcileInterval is what the measurement harnesses have
// always run with: a 5 s janitor, 720x faster than the product default, which
// accelerates reconciliation so a 60 s window sees it at all. It is a
// measurement choice and every artifact taken under it says so.
const issue767DefaultTestReconcileInterval = "5s"

type issue767FixtureOptions struct {
	reconcileInterval string
}

// issue767WithReconcileInterval sets GORTEX_RECONCILE_INTERVAL for the private
// daemon. The empty string keeps the harness default (5s);
// issue767ProductReconcileInterval omits the variable entirely so the product
// default applies.
func issue767WithReconcileInterval(interval string) issue767FixtureOption {
	return func(o *issue767FixtureOptions) { o.reconcileInterval = interval }
}

func newIssue767Fixture(t *testing.T, binary string) *issue767Fixture {
	t.Helper()
	return newIssue767FixtureWithCorpus(t, binary, issue767DefaultCorpus)
}

// newIssue767FixtureWithCorpus is newIssue767Fixture with the repository
// contents supplied by the caller. The private root, environment, gitconfig,
// first commit and tracking configuration are identical either way: a harness
// may change what the repository contains, never how it is isolated.
func newIssue767FixtureWithCorpus(t *testing.T, binary string, corpus issue767Corpus, opts ...issue767FixtureOption) *issue767Fixture {
	t.Helper()
	settings := issue767FixtureOptions{reconcileInterval: issue767DefaultTestReconcileInterval}
	for _, opt := range opts {
		opt(&settings)
	}
	if settings.reconcileInterval == "" {
		settings.reconcileInterval = issue767DefaultTestReconcileInterval
	}
	parent := os.Getenv("GORTEX_ISSUE767_ARTIFACT_DIR")
	preserve := parent != ""
	if !preserve {
		parent = "/tmp"
	}
	var err error
	parent, err = filepath.Abs(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(parent, "gx767-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if !preserve {
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	f := &issue767Fixture{t: t, binary: binary, root: root, primary: filepath.Join(root, "repo"), linked: filepath.Join(root, "linked"), store: filepath.Join(root, "store.sqlite")}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GORTEX_") || strings.HasPrefix(key, "XDG_") || strings.HasPrefix(key, "GIT_") {
			continue
		}
		f.env = append(f.env, entry)
	}
	f.env = append(f.env,
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_DATA_HOME="+filepath.Join(root, "data"), "XDG_CACHE_HOME="+filepath.Join(root, "cache"),
		"GORTEX_DAEMON_PPROF_ADDR=127.0.0.1:0",
		// Telemetry is off by default and dormant without an endpoint, but a
		// measured run states it: the consent/rollup files under the private
		// data dir are writes like any other.
		"GORTEX_TELEMETRY=0",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"), "GIT_TERMINAL_PROMPT=0", "GOWORK=off", "NO_COLOR=1", "CI=1")
	if settings.reconcileInterval != issue767ProductReconcileInterval {
		f.env = append(f.env, "GORTEX_RECONCILE_INTERVAL="+settings.reconcileInterval)
	}
	f.write(filepath.Join(root, "gitconfig"), "[user]\n\tname = Issue767 Test\n\temail = issue767@example.invalid\n[commit]\n\tgpgsign = false\n")
	corpus(f)
	f.git(f.primary, "init", "-b", "main")
	f.git(f.primary, "add", ".")
	f.git(f.primary, "commit", "-m", "isolated fixture")
	// Match configured cold startup; runtime track is a separate scenario.
	f.write(filepath.Join(root, "config", "gortex", "config.yaml"), "repos:\n  - path: "+strconv.Quote(f.primary)+"\n    name: issue767\n")
	t.Cleanup(f.stop)
	t.Logf("isolated fixture artifacts: %s", root)
	return f
}

// issue767DefaultCorpus is the original 32-file fixture: small enough that a
// cold index finishes in seconds, large enough that the resolver has real
// intra-package call edges to rebuild.
func issue767DefaultCorpus(f *issue767Fixture) {
	f.write(filepath.Join(f.primary, "go.mod"), "module example.invalid/issue767\n\ngo 1.24\n")
	f.write(filepath.Join(f.primary, "marker.go"), issue767MarkerSource("Issue767PrimaryMarker"))
	for i := 0; i < 32; i++ {
		f.write(filepath.Join(f.primary, fmt.Sprintf("file%02d.go", i)), fmt.Sprintf("package fixture\nfunc Issue767Target%02d() int { return %d }\nfunc Issue767Caller%02d() int { return Issue767Target00() }\n", i, i, i))
	}
}

func issue767MarkerSource(names ...string) string {
	source := "package fixture\n"
	for _, name := range names {
		source += "func " + name + "() int { return Issue767Target00() }\n"
	}
	return source
}

func (f *issue767Fixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *issue767Fixture) git(dir string, args ...string) {
	f.t.Helper()
	if output, err := f.tryGit(dir, args...); err != nil {
		f.t.Fatalf("fixture git %v: %v\n%s", args, err, output)
	}
}

// tryGit is git without the fatal. Teardown needs it: a cleanup that runs
// while a phase failure is already unwinding must report what it could not do,
// not raise a second failure over the first.
func (f *issue767Fixture) tryGit(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(f.t.Context()), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = dir, f.env
	return cmd.CombinedOutput()
}

func (f *issue767Fixture) command(timeout time.Duration, dir string, args ...string) []byte {
	f.t.Helper()
	output, err := f.tryCommand(timeout, dir, args...)
	if err != nil {
		f.t.Fatalf("isolated CLI %v: %v\n%s", args, err, output)
	}
	return output
}

func (f *issue767Fixture) tryCommand(timeout time.Duration, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(f.t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.binary, args...)
	cmd.Dir, cmd.Env = dir, f.env
	f.mu.Lock()
	f.clientCalls++
	f.mu.Unlock()
	output, err := cmd.CombinedOutput()
	tail := string(output)
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	f.mu.Lock()
	f.lastOutput = tail
	f.mu.Unlock()
	return output, err
}

// clientCallCount is the number of CLI invocations this fixture has made. A
// window's delta is the client traffic that window carried, which is what
// separates a daemon's idle floor from a poller's cost.
func (f *issue767Fixture) clientCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clientCalls
}

// lastResponse is the tail of the most recent CLI response, read under the
// lock so a timeout message assembled on one goroutine cannot tear a string
// another goroutine is writing.
func (f *issue767Fixture) lastResponse() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastOutput
}

func (f *issue767Fixture) start() {
	f.t.Helper()
	run := f.nextRun()
	log, err := os.Create(f.logPathFor(run))
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.t.Context())
	if deadline, ok := f.t.Deadline(); ok {
		cancel()
		ctx, cancel = context.WithDeadline(f.t.Context(), deadline.Add(-time.Minute))
	}
	cmd := exec.CommandContext(ctx, f.binary, "daemon", "start", "--embeddings=false", "--backend", "sqlite", "--backend-path", f.store, "--no-progress")
	// Global testing timeout bypasses Cleanup. This captured child receives
	// SIGINT and a bounded forced exit before the parent testing deadline.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = f.primary, f.env, log, log
	if err := cmd.Start(); err != nil {
		cancel()
		_ = log.Close()
		f.t.Fatal(err)
	}
	done := make(chan error, 1)
	// The child handle becomes visible to other goroutines only here, as one
	// guarded publication of an already-started process.
	f.setChild(cmd, done, cancel, log)
	go func() { done <- cmd.Wait() }()
	f.await("isolated daemon socket", time.Minute, func() bool {
		select {
		case err := <-done:
			// The child is gone: drop the handle, release its context and
			// close the log before failing, so nothing outlives this call.
			_, _, cancel, log := f.takeChild()
			if cancel != nil {
				cancel()
			}
			if log != nil {
				_ = log.Close()
			}
			f.t.Fatalf("isolated daemon exited during start: %v; log %s", err, f.logPathFor(run))
		default:
		}
		_, err := f.tryCommand(5*time.Second, f.primary, "daemon", "status", "--no-progress")
		return err == nil
	})
}

func (f *issue767Fixture) stop() {
	cmd, done, cancel, log := f.takeChild()
	if cancel != nil {
		defer cancel()
	}
	if cmd == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			f.t.Errorf("isolated child %d did not exit after kill", cmd.Process.Pid)
		}
		f.t.Errorf("isolated child %d required force kill", cmd.Process.Pid)
	}
	if log != nil {
		_ = log.Close()
	}
}

// setChild publishes a started child; takeChild removes it and hands the
// caller everything needed to shut it down. Both are guarded because a
// sampler goroutine reads the same fields between them.
func (f *issue767Fixture) setChild(cmd *exec.Cmd, done chan error, cancel context.CancelFunc, log *os.File) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmd, f.done, f.cancel, f.log = cmd, done, cancel, log
}

func (f *issue767Fixture) takeChild() (*exec.Cmd, chan error, context.CancelFunc, *os.File) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd, done, cancel, log := f.cmd, f.done, f.cancel, f.log
	f.cmd, f.cancel, f.log = nil, nil, nil
	return cmd, done, cancel, log
}

// nextRun claims the next daemon-run index; runIndex reports the current one.
func (f *issue767Fixture) nextRun() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.run++
	return f.run
}

func (f *issue767Fixture) runIndex() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.run
}

// logPath is the current child's log file. It is safe to call from a sampling
// goroutine while the fixture is starting or restarting the daemon.
func (f *issue767Fixture) logPath() string { return f.logPathFor(max(f.runIndex(), 1)) }

func (f *issue767Fixture) logPathFor(run int) string {
	return filepath.Join(f.root, fmt.Sprintf("daemon-%d.log", run))
}

// pid is the running private child's process id, or 0 when no child is
// running. A caller sampling process I/O must re-read it after every restart:
// the counters belong to the process, not to the fixture.
func (f *issue767Fixture) pid() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cmd == nil || f.cmd.Process == nil {
		return 0
	}
	return f.cmd.Process.Pid
}

func (f *issue767Fixture) await(label string, timeout time.Duration, ready func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		select {
		case <-f.t.Context().Done():
			f.t.Fatal(f.t.Context().Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	f.t.Fatalf("timed out waiting for %s; artifacts %s; last CLI response: %s", label, f.root, f.lastResponse())
}

func (f *issue767Fixture) searchHasSymbol(root, name string) bool {
	found, err := f.trySearchSymbol(root, name)
	return err == nil && found
}

func (f *issue767Fixture) trySearchSymbol(root, name string) (bool, error) {
	return f.trySearchSymbolIn(root, name, filepath.Join(root, "marker.go"))
}

// trySearchSymbolIn is trySearchSymbol with the file the symbol must come from
// named explicitly. The marker-file spelling is the common case (every checkout
// carries one marker.go probe); a harness that edits a corpus file and waits for
// the edited declaration needs to name that file instead, otherwise a hit from
// a stale sibling checkout would pass for the edit landing.
//
// The request spelling follows the checkout's mode (see spellingFor).
func (f *issue767Fixture) trySearchSymbolIn(root, name, file string) (bool, error) {
	return f.trySearchSymbolAs(root, name, file, f.spellingFor(root))
}

// issue767Spelling names HOW a checkout is addressed in a request. It is not a
// harness preference: the product serves the checkout modes through different
// doors, and asking through the wrong one is refused rather than answered.
//
//   - An automatic (discovered) checkout is routed to its composed view. Both
//     the CWD binding and an explicit worktree selector reach it, and the
//     answer carries an exact freshness label.
//   - A dedicated (explicitly tracked) checkout is served from its own indexed
//     corpus — "Only an automatic checkout is routed here. A dedicated checkout
//     and the family's primary are served from the indexed corpus"
//     (internal/mcp/view_request.go:1038-1041; graphview.ServesAutomaticView,
//     internal/graphview/binding.go:79-82, requires CheckoutModeAutomatic). It
//     owns no checkout_routes row, so an explicit worktree selector — which
//     goes through materializeRequestView with strict=true and refuses on
//     "!found || !RouteReady(route)" (internal/mcp/view_request.go:1995-2001) —
//     answers CodeViewBuilding ("is not fully routed yet") indefinitely.
type issue767Spelling int

const (
	// issue767AsPrimary addresses the configured repository root: no view
	// selector, and no freshness label is demanded (the primary corpus is the
	// answer, not a view over one).
	issue767AsPrimary issue767Spelling = iota
	// issue767AsAutomaticWorktree addresses a discovered linked checkout
	// through an explicit worktree view selector and demands the exact label.
	issue767AsAutomaticWorktree
	// issue767AsOwnCorpus addresses a checkout as its own repository: no view
	// selector. It is the dedicated lane's spelling. The answer carries no
	// freshness label at all, so this spelling must never be reported as an
	// exact route; what pins it instead is physical — the answering symbol's
	// absolute file path has to be inside the checkout that was asked.
	issue767AsOwnCorpus
)

func (s issue767Spelling) String() string {
	switch s {
	case issue767AsPrimary:
		return "primary"
	case issue767AsAutomaticWorktree:
		return "worktree_view_selector"
	case issue767AsOwnCorpus:
		return "own_corpus_no_view_selector"
	}
	return "unknown"
}

// spellingFor is the default spelling for a root: the configured primary, or a
// discovered automatic checkout. A harness that explicitly tracks a checkout
// has to name issue767AsOwnCorpus itself — the fixture cannot know that a
// `track --as-worktree` happened.
func (f *issue767Fixture) spellingFor(root string) issue767Spelling {
	if root == f.primary {
		return issue767AsPrimary
	}
	return issue767AsAutomaticWorktree
}

// issue767Answer is one search answer's evidence, separated from the verdict so
// a caller can record what an answer does and does not carry instead of only
// whether it satisfied a rule.
type issue767Answer struct {
	Spelling         string `json:"spelling"`
	Found            bool   `json:"found"`
	Exact            bool   `json:"exact_label"`
	Fallback         bool   `json:"fallback"`
	FromExpectedFile bool   `json:"from_expected_file"`
	// Prefix is the repository prefix of the node that answered — which
	// corpus served the request. The family base answers as "issue767"; an
	// explicitly tracked checkout answers as "issue767@<worktree>". It is
	// recorded, never asserted: it is the measurement that says which corpus
	// a spelling reached.
	Prefix string `json:"repo_prefix,omitempty"`
	Error  string `json:"error,omitempty"`
}

// issue767SearchRequest is the request body one spelling sends. Only the
// automatic spelling carries a view selector: the primary and a dedicated
// checkout are both served from an indexed corpus, and an explicit worktree
// selector over a checkout with no route is refused rather than answered.
func issue767SearchRequest(root, name string, spelling issue767Spelling) map[string]any {
	request := map[string]any{"operation": "symbols", "query": name, "options": map[string]any{"limit": 10, "query_class": "symbol", "expand": "off"}}
	if spelling == issue767AsAutomaticWorktree {
		request["view"] = map[string]any{"kind": "worktree", "path": root}
	}
	return request
}

// issue767Verdict is the evidence rule one spelling has to satisfy.
//
// The automatic spelling demands the exact freshness label, as it always has.
// The own-corpus spelling cannot: that answer carries no freshness label at
// all, so this rule must never be reported as an exact route. What it pins
// instead is physical: the answering symbol's absolute file path has to be the
// file the caller named inside the checkout it asked, so a hit from a sibling
// checkout, from the primary's own copy of the same file, or from a stale
// generation fails the rule. Which corpus served the answer is recorded
// separately (issue767Answer.Prefix) rather than asserted — a measurement, not
// a contract the harness invents.
// errIssue767Inexact marks the half of that rule that means "ask again", not
// "this is wrong".
//
// A fallback rider (`base_changed`, `route_moved`) and a missing exact label
// are the product telling the truth about a view that is still moving:
// view_request.go labels the answer instead of blocking, and the label clears
// on its own in well under a second once the base settles. A caller that
// polls — awaitSymbol and every bounded await built on it — must retry those;
// a caller that judges an answer once must know it has not been given a
// judgeable one. Distinguishing them is not a relaxation: the rule that an
// automatic checkout must prove exact freshness is unchanged, and an answer
// that comes out of the wrong file is still a flat failure.
var errIssue767Inexact = errors.New("view answered inexactly; the answer is not judgeable yet")

// issue767Inexact reports whether an error from issue767Verdict (or from a
// helper built on it) is the retryable kind.
func issue767Inexact(err error) bool { return errors.Is(err, errIssue767Inexact) }

func issue767Verdict(answer issue767Answer, spelling issue767Spelling) (bool, error) {
	if answer.Fallback {
		return false, fmt.Errorf("search returned fallback or tool error: %w", errIssue767Inexact)
	}
	if spelling == issue767AsAutomaticWorktree && !answer.Exact {
		return false, fmt.Errorf("automatic checkout search did not prove exact freshness: %w", errIssue767Inexact)
	}
	if answer.Found && !answer.FromExpectedFile {
		return false, errors.New("symbol did not belong to selected source file and repository")
	}
	return answer.Found, nil
}

// askSymbol performs one search and reports what came back. Only transport and
// decode failures are errors here; every judgement is left to the caller.
func (f *issue767Fixture) askSymbol(root, name, file string, spelling issue767Spelling) (issue767Answer, error) {
	answer := issue767Answer{Spelling: spelling.String()}
	request := issue767SearchRequest(root, name, spelling)
	payload, err := json.Marshal(request)
	if err != nil {
		f.t.Fatal(err)
	}
	output, err := f.tryCommand(10*time.Second, root, "call", "search", "--index", root, "--json", string(payload), "--format", "json")
	if err != nil {
		answer.Error = fmt.Sprintf("search command: %v: %s", err, output)
		return answer, fmt.Errorf("search command: %w: %s", err, output)
	}
	var value any
	if err := json.Unmarshal(output, &value); err != nil {
		answer.Error = fmt.Sprintf("search response: %v: %s", err, output)
		return answer, fmt.Errorf("search response: %w: %s", err, output)
	}
	answer.Found, answer.Fallback = issue767JSONEvidence(value, name)
	answer.Exact = issue767JSONExact(value)
	answer.FromExpectedFile = issue767JSONSource(value, name, file)
	answer.Prefix = issue767JSONPrefix(value, name, file)
	return answer, nil
}

// issue767JSONPrefix is the repository prefix of the node that answered for
// this name out of this file, or "" when no node did. The prefix is what
// distinguishes the family's base corpus from a checkout's own: a linked
// checkout served automatically answers with the family prefix and the
// checkout's absolute path, while the same checkout tracked as an independent
// instance answers with its own "<family>@<worktree>" prefix.
func issue767JSONPrefix(value any, name, file string) string {
	switch value := value.(type) {
	case map[string]any:
		if value["name"] == name {
			if path, ok := value["absolute_file_path"].(string); ok && filepath.Clean(path) == filepath.Clean(file) {
				if prefix, ok := value["repo_prefix"].(string); ok {
					return prefix
				}
			}
		}
		for _, child := range value {
			if prefix := issue767JSONPrefix(child, name, file); prefix != "" {
				return prefix
			}
		}
	case []any:
		for _, child := range value {
			if prefix := issue767JSONPrefix(child, name, file); prefix != "" {
				return prefix
			}
		}
	case string:
		var child any
		if (strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")) && json.Unmarshal([]byte(value), &child) == nil {
			return issue767JSONPrefix(child, name, file)
		}
	}
	return ""
}

// issue767Probe is one bounded question and everything the harness needs to
// say what happened: the last answer, whether the rule could be applied to it
// at all, how many times it had to ask, and how long it waited.
type issue767Probe struct {
	Answer    issue767Answer `json:"answer"`
	Found     bool           `json:"found"`
	Judgeable bool           `json:"judgeable"`
	Polls     int            `json:"polls"`
	Seconds   float64        `json:"wait_s"`
	Err       string         `json:"error,omitempty"`
}

// awaitJudgeable asks one symbol question until the spelling's evidence rule
// can actually be applied to the answer, or until the timeout.
//
// It is the bounded sibling of awaitSymbolAs for the callers that judge an
// answer once rather than wait for a particular one: an isolation probe asks
// "is this symbol visible here", and an inexact answer is not a "no" — it is
// the product saying the view moved under the question. Judging it either way
// invents a fact. The wait is returned so the caller can account for it
// exactly as it accounts for an exactness wait.
//
// Judgeable=false at the deadline is an unavailability with a name, never a
// pass: the caller is told the rule could not be applied, and the last answer
// travels with it.
func (f *issue767Fixture) awaitJudgeable(root, name, file string, spelling issue767Spelling, timeout time.Duration) issue767Probe {
	started := time.Now()
	deadline := started.Add(timeout)
	probe := issue767Probe{}
	for {
		probe.Polls++
		answer, err := f.askSymbol(root, name, file, spelling)
		probe.Answer = answer
		if err == nil {
			found, verdictErr := issue767Verdict(answer, spelling)
			if !issue767Inexact(verdictErr) {
				probe.Found, probe.Judgeable = found, true
				if verdictErr != nil {
					probe.Err = verdictErr.Error()
				} else {
					probe.Err = ""
				}
				probe.Seconds = time.Since(started).Seconds()
				return probe
			}
			probe.Err = verdictErr.Error()
		} else {
			probe.Err = err.Error()
		}
		if !time.Now().Before(deadline) {
			probe.Seconds = time.Since(started).Seconds()
			return probe
		}
		select {
		case <-f.t.Context().Done():
			probe.Err = f.t.Context().Err().Error()
			probe.Seconds = time.Since(started).Seconds()
			return probe
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// trySearchSymbolAs applies the spelling's evidence rule to one answer.
func (f *issue767Fixture) trySearchSymbolAs(root, name, file string, spelling issue767Spelling) (bool, error) {
	answer, err := f.askSymbol(root, name, file, spelling)
	if err != nil {
		return false, err
	}
	return issue767Verdict(answer, spelling)
}

func issue767JSONEvidence(value any, name string) (found, fallback bool) {
	merge := func(child any) {
		childFound, childFallback := issue767JSONEvidence(child, name)
		found, fallback = found || childFound, fallback || childFallback
	}
	switch value := value.(type) {
	case map[string]any:
		exact, hasExact := value["exact"].(bool)
		fallback = hasExact && !exact
		if code, ok := value["error_code"].(string); ok && code != "" {
			fallback = true
		}
		if isError, ok := value["isError"].(bool); ok && isError {
			fallback = true
		}
		found = value["name"] == name
		for _, child := range value {
			merge(child)
		}
	case []any:
		for _, child := range value {
			merge(child)
		}
	case string:
		var child any
		if strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[") {
			if json.Unmarshal([]byte(value), &child) == nil {
				merge(child)
			}
		}
	}
	return found, fallback
}

func issue767JSONExact(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if exact, ok := value["exact"].(bool); ok && exact {
			return true
		}
		for _, child := range value {
			if issue767JSONExact(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if issue767JSONExact(child) {
				return true
			}
		}
	case string:
		var child any
		if (strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")) && json.Unmarshal([]byte(value), &child) == nil {
			return issue767JSONExact(child)
		}
	}
	return false
}

// issue767FixturePrefix is the repo prefix the fixture configures.
const issue767FixturePrefix = "issue767"

// issue767PrefixMatches accepts the fixture's own prefix and any worktree
// instance derived from it.
//
// A linked checkout served through its family answers as "issue767"; one that
// was explicitly tracked as an independent instance answers as
// "issue767@<worktree>" (the daemon's own spelling, observed in
// `daemon status` tracked_repos as `issue767@wt01`). Both are this fixture, and
// the absolute file path already pins WHICH checkout answered, so a harness
// that drives an explicit track must accept the family, not one literal.
func issue767PrefixMatches(prefix string) bool {
	return prefix == issue767FixturePrefix || strings.HasPrefix(prefix, issue767FixturePrefix+"@")
}

// issue767JSONSource reports whether the named symbol in the response belongs to
// the fixture repository and to the exact file the caller expects. file is an
// absolute path.
func issue767JSONSource(value any, name, file string) bool {
	switch value := value.(type) {
	case map[string]any:
		if prefix, _ := value["repo_prefix"].(string); value["name"] == name && issue767PrefixMatches(prefix) {
			path, ok := value["absolute_file_path"].(string)
			if ok && filepath.Clean(path) == filepath.Clean(file) {
				return true
			}
		}
		for _, child := range value {
			if issue767JSONSource(child, name, file) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if issue767JSONSource(child, name, file) {
				return true
			}
		}
	case string:
		var child any
		if (strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")) && json.Unmarshal([]byte(value), &child) == nil {
			return issue767JSONSource(child, name, file)
		}
	}
	return false
}

func (f *issue767Fixture) awaitSymbol(root, name string) {
	f.t.Helper()
	f.await("selected symbol "+name, 2*time.Minute, func() bool { return f.searchHasSymbol(root, name) })
}

// awaitSymbolIn waits for a symbol to answer from one named file, under the
// default spelling for that root.
func (f *issue767Fixture) awaitSymbolIn(root, name, file string, timeout time.Duration) {
	f.t.Helper()
	f.awaitSymbolAs(root, name, file, timeout, f.spellingFor(root))
}

// awaitSymbolAs is awaitSymbolIn with the request spelling named by the caller.
func (f *issue767Fixture) awaitSymbolAs(root, name, file string, timeout time.Duration, spelling issue767Spelling) {
	f.t.Helper()
	f.await("selected symbol "+name+" in "+file+" via "+spelling.String(), timeout, func() bool {
		found, err := f.trySearchSymbolAs(root, name, file, spelling)
		return err == nil && found
	})
}

func (f *issue767Fixture) openReadOnly() *sql.DB {
	f.t.Helper()
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(f.store), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		f.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.t.Cleanup(func() { _ = db.Close() })
	return db
}

func (f *issue767Fixture) awaitRemoved() {
	f.awaitRemovedPath(f.linked)
}

// awaitRemovedPath waits for the daemon's own cleanup to drop a removed
// checkout's catalog row. A harness that creates more than one linked checkout
// needs the path parameter; awaitRemoved keeps the single-checkout spelling.
func (f *issue767Fixture) awaitRemovedPath(path string) {
	db := f.openReadOnly()
	defer db.Close()
	f.await("removed checkout cleanup "+path, 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(f.t.Context(), time.Second)
		defer cancel()
		var count int
		return db.QueryRowContext(ctx, "SELECT COUNT(*) FROM checkouts WHERE root_path=?", path).Scan(&count) == nil && count == 0
	})
}

type issue767GenerationSnapshot struct {
	Count, Max, Sequence                    int64
	Nodes, Edges, RefFacts, PrimaryRefFacts int64
}

func issue767ReadGenerations(ctx context.Context, db *sql.DB) (issue767GenerationSnapshot, error) {
	result := issue767GenerationSnapshot{Sequence: -1}
	err := db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(generation_id),0) FROM view_generations").Scan(&result.Count, &result.Max)
	if err != nil {
		return result, err
	}
	err = db.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM nodes), (SELECT COUNT(*) FROM edges), (SELECT COUNT(*) FROM ref_facts), (SELECT COUNT(*) FROM ref_facts WHERE view_gen=0 AND repo_prefix='issue767')").Scan(&result.Nodes, &result.Edges, &result.RefFacts, &result.PrimaryRefFacts)
	if err != nil {
		return result, err
	}
	// sqlite_sequence carries a row for an AUTOINCREMENT table only once that
	// table has taken a rowid. "No generation has ever been allocated in this
	// store" is therefore a legitimate reading of 0, not a failed read — and it
	// is the NORMAL reading on a baseline arm whose binary predates generation
	// allocation entirely (main 56a1c29d indexes 404 nodes into a store whose
	// view_generations table stays empty).
	//
	// Reporting ErrNoRows here made the whole snapshot unreadable on that arm,
	// so settle() never saw three stable samples and every baseline phase died
	// on its two-minute wait: the paired measurement had no baseline at all.
	// Only the absence of the row is absorbed; a missing table, a closed
	// database or any other scan failure is still an error, so a store that
	// genuinely cannot be read is never reported as a store with no
	// generations.
	err = db.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name='view_generations'").Scan(&result.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		result.Sequence = 0
		return result, nil
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

func (f *issue767Fixture) settle() {
	db := f.openReadOnly()
	defer db.Close()
	var previous issue767GenerationSnapshot
	stable := 0
	f.await("three stable generation samples", 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(f.t.Context(), 3*time.Second)
		defer cancel()
		current, err := issue767ReadGenerations(ctx, db)
		if err != nil {
			return false
		}
		if current == previous {
			stable++
		} else {
			stable = 0
		}
		previous = current
		if stable < 3 {
			time.Sleep(5 * time.Second)
		}
		return stable >= 3
	})
}

// issue767ProcessIO is one RUSAGE_INFO_V4 / /proc read of a child process.
//
// BytesWritten, LogicalBytesWritten and StartTicks are the original idle-harness
// fields. BytesRead, PhysFootprint, UserTimeNS and SystemTimeNS were added for
// the sustained harness, which has to report read amplification, peak memory and
// CPU next to the write series; they are zero where the platform does not
// supply them (every one of them is Darwin-only today) and no caller may treat
// a zero as a measurement.
type issue767ProcessIO struct {
	BytesWritten        uint64
	LogicalBytesWritten *uint64
	StartTicks          uint64
	BytesRead           uint64 `json:",omitempty"`
	PhysFootprint       uint64 `json:",omitempty"`
	UserTimeNS          uint64 `json:",omitempty"`
	SystemTimeNS        uint64 `json:",omitempty"`
}

const issue767DarwinIOScript = "import ctypes,json,sys\nfields='user_time system_time pkg_idle_wkups interrupt_wkups pageins wired_size resident_size phys_footprint proc_start_abstime proc_exit_abstime child_user_time child_system_time child_pkg_idle_wkups child_interrupt_wkups child_pageins child_elapsed_abstime diskio_bytesread diskio_byteswritten cpu_time_qos_default cpu_time_qos_maintenance cpu_time_qos_background cpu_time_qos_utility cpu_time_qos_legacy cpu_time_qos_user_initiated cpu_time_qos_user_interactive billed_system_time serviced_system_time logical_writes lifetime_max_phys_footprint instructions cycles billed_energy serviced_energy interval_max_phys_footprint runnable_time'.split()\nclass R(ctypes.Structure):\n _fields_=[('uuid',ctypes.c_uint8*16)]+[(n,ctypes.c_uint64) for n in fields]\nr=R();lib=ctypes.CDLL('/usr/lib/libproc.dylib',use_errno=True);fn=lib.proc_pid_rusage;fn.argtypes=[ctypes.c_int,ctypes.c_int,ctypes.c_void_p];fn.restype=ctypes.c_int\nif fn(int(sys.argv[1]),4,ctypes.byref(r))!=0: raise OSError(ctypes.get_errno(),'proc_pid_rusage')\nprint(json.dumps({'BytesWritten':r.diskio_byteswritten,'LogicalBytesWritten':r.logical_writes,'StartTicks':r.proc_start_abstime,'BytesRead':r.diskio_bytesread,'PhysFootprint':r.phys_footprint,'UserTimeNS':r.user_time,'SystemTimeNS':r.system_time}))\n"

func issue767ReadProcessIO(ctx context.Context, pid int) (issue767ProcessIO, error) {
	var result issue767ProcessIO
	if runtime.GOOS == "darwin" {
		output, err := exec.CommandContext(ctx, "python3", "-c", issue767DarwinIOScript, strconv.Itoa(pid)).Output()
		if err != nil {
			return result, err
		}
		err = json.Unmarshal(output, &result)
		return result, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return result, err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return result, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return result, errors.New("process starttime missing")
	}
	result.StartTicks, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return result, err
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return result, err
	}
	written := false
	for _, line := range strings.Split(string(raw), "\n") {
		if value, found := strings.CutPrefix(line, "read_bytes:"); found {
			if parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64); err == nil {
				result.BytesRead = parsed
			}
			continue
		}
		if value, found := strings.CutPrefix(line, "write_bytes:"); found {
			result.BytesWritten, err = strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return result, err
			}
			written = true
		}
	}
	if !written {
		return result, errors.New("process write_bytes counter not found")
	}
	return result, nil
}

func issue767FileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// TestIssue767SpellingForIsTheExactnessDemandingDefault pins the dispatcher
// every default wait in both harnesses routes through.
//
// awaitSymbol, awaitSymbolIn, trySearchSymbolIn, w8Run.awaitProbe and therefore
// every exactness wait in P0/P1/P2/P5/P6/P8 call spellingFor(root) rather than
// naming a spelling. issue767AsOwnCorpus is strictly weaker than
// issue767AsAutomaticWorktree — it carries no freshness label and its verdict
// cannot demand one — so a one-word change here would retire the harness's
// central guarantee ("every checkout answer in a measured run is proven exact")
// across the whole measured run with no test going red, and every number the
// run produced would silently be a number about a possibly-stale view.
//
// The identity assertions alone would not catch that: the pin is the
// consequence. For a non-primary root the default route must (a) carry the
// worktree view selector and (b) refuse an answer that does not prove exact
// freshness. Both are asserted through the same public shapes the waits use.
func TestIssue767SpellingForIsTheExactnessDemandingDefault(t *testing.T) {
	root := t.TempDir()
	f := &issue767Fixture{t: t, root: root, primary: filepath.Join(root, "repo"), linked: filepath.Join(root, "linked")}

	if got := f.spellingFor(f.primary); got != issue767AsPrimary {
		t.Fatalf("spellingFor(primary) = %v, want %v", got, issue767AsPrimary)
	}
	for _, root := range []string{f.linked, filepath.Join(f.root, "wt01"), f.primary + "-sibling"} {
		got := f.spellingFor(root)
		if got != issue767AsAutomaticWorktree {
			t.Fatalf("spellingFor(%q) = %v, want %v: a default wait over a checkout must take the automatic lane", root, got, issue767AsAutomaticWorktree)
		}
		// (a) the default route addresses the checkout as a view.
		request := issue767SearchRequest(root, "Marker", got)
		view, ok := request["view"].(map[string]any)
		if !ok {
			t.Fatalf("the default spelling for %q sends no view selector: %v", root, request)
		}
		if view["kind"] != "worktree" || view["path"] != root {
			t.Fatalf("the default spelling for %q selects %v, want the worktree itself", root, view)
		}
		// (b) the default route refuses an answer that proves no freshness.
		stale := issue767Answer{Found: true, FromExpectedFile: true, Exact: false}
		if ok, err := issue767Verdict(stale, got); ok || err == nil {
			t.Fatalf("the default spelling for %q accepted an answer with no exactness label (ok=%v err=%v); "+
				"every default wait in the harness would stop demanding exactness", root, ok, err)
		}
		if ok, err := issue767Verdict(issue767Answer{Found: true, FromExpectedFile: true, Exact: true}, got); !ok || err != nil {
			t.Fatalf("the default spelling for %q rejected an exact answer: ok=%v err=%v", root, ok, err)
		}
	}
}

// TestIssue767ReadGenerationsAcceptsAStoreThatNeverAllocatedAGeneration is the
// baseline arm's regression.
//
// The paired I/O protocol runs the same harness against two binaries whose
// stores differ by four schema versions. A binary that predates generation
// allocation leaves view_generations empty, so sqlite_sequence has no row for
// it — and a snapshot read that calls that an error takes settle() down with
// it, which takes every phase of the baseline arm down with it, which leaves
// the measurement with nothing to compare the candidate against.
//
// The absent row is absorbed as 0 and nothing else is: a store whose
// view_generations table is missing altogether is still a failed read, because
// "this store has no generations" and "this store cannot be read" are different
// facts and only one of them belongs in a census.
func TestIssue767ReadGenerationsAcceptsAStoreThatNeverAllocatedAGeneration(t *testing.T) {
	open := func(name string, schema ...string) *sql.DB {
		path := filepath.Join(t.TempDir(), name)
		db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		db.SetMaxOpenConns(1)
		for _, statement := range schema {
			if _, err := db.Exec(statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		return db
	}
	const generations = "CREATE TABLE view_generations (generation_id INTEGER PRIMARY KEY AUTOINCREMENT, state TEXT, storage_bytes INTEGER, covered_files INTEGER, affected_files INTEGER)"
	rest := []string{
		"CREATE TABLE nodes (id TEXT)",
		"CREATE TABLE edges (id INTEGER PRIMARY KEY AUTOINCREMENT)",
		"CREATE TABLE ref_facts (view_gen INTEGER, repo_prefix TEXT)",
	}

	// A pre-generation store: the tables exist, nothing was ever allocated.
	db := open("baseline.sqlite", append([]string{generations}, rest...)...)
	if _, err := db.Exec("INSERT INTO edges DEFAULT VALUES"); err != nil {
		t.Fatal(err) // so sqlite_sequence exists, but with no view_generations row
	}
	snapshot, err := issue767ReadGenerations(t.Context(), db)
	if err != nil {
		t.Fatalf("a store that never allocated a generation must read, not fail: %v", err)
	}
	if snapshot.Sequence != 0 {
		t.Fatalf("sequence = %d, want 0 for a store with no allocated generation", snapshot.Sequence)
	}
	if snapshot.Count != 0 || snapshot.Max != 0 {
		t.Fatalf("snapshot invented generations: %+v", snapshot)
	}
	// Two identical reads must compare equal, which is the only thing settle()
	// asks of the snapshot: an arm that cannot produce a stable sample has no
	// phase boundaries and therefore no measurement.
	again, err := issue767ReadGenerations(t.Context(), db)
	if err != nil || again != snapshot {
		t.Fatalf("repeated read is not stable: %+v vs %+v (err %v)", again, snapshot, err)
	}

	// An allocated generation is still reported as itself.
	if _, err := db.Exec("INSERT INTO view_generations(state) VALUES ('committed')"); err != nil {
		t.Fatal(err)
	}
	allocated, err := issue767ReadGenerations(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	if allocated.Sequence != 1 || allocated.Count != 1 || allocated.Max != 1 {
		t.Fatalf("an allocated generation read back as %+v", allocated)
	}

	// A store without the table at all is still an error.
	broken := open("broken.sqlite", rest...)
	if _, err := issue767ReadGenerations(t.Context(), broken); err == nil {
		t.Fatal("a store with no view_generations table must fail the read, not report zero generations")
	}
}
