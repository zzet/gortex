package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDiscoveryAttemptsRetainSessionCWDAndRefusalTarget(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "pending", "subdir")
	root := filepath.Dir(cwd)
	details := map[string]any{"error_code": "view_building", "path": root,
		"family": "family-a", "reason": "family_capacity", "state": "discovery_pending", "retryable": true}
	socket := startStub(t, func(_ stubFrame) (any, *rpcError) {
		return nil, &rpcError{Code: -32000, Message: "checkout discovery unavailable", Data: details}
	})
	client, err := openMCP(t.Context(), socket, cwd)
	require.NoError(t, err)
	defer client.Close()
	_, attempts, err := callTool(t.Context(), client, request{Tool: "search", Operation: "symbols"},
		map[string]any{"operation": "symbols", "query": "Target"}, "workload", 1, 0, 0, time.Second,
		func(context.Context, time.Duration) error { return nil })
	require.Error(t, err)
	require.Len(t, attempts, 1)
	encoded, err := json.Marshal(attempts[0])
	require.NoError(t, err)
	var observation map[string]any
	require.NoError(t, json.Unmarshal(encoded, &observation))
	require.Equal(t, cwd, observation["cwd"], "request cwd must remain distinct from its pending checkout root")
	require.Equal(t, details, observation["error_data"], "refusal scope must survive report serialization")
}

func TestDiscoveryRefusalAccountingUsesCWDAndPendingWindow(t *testing.T) {
	start := time.Unix(100, 0)
	root := filepath.Join(t.TempDir(), "pending")
	entries := []checkoutDiscoveryObservation{{Root: root, Added: start, ExactAt: start.Add(time.Second)}}
	rows := []Attempt{
		{CWD: filepath.Join(root, "subdir"), Started: start.Add(time.Millisecond), Code: -32000, Outcome: "error", DurationMS: 2},
		{CWD: root + "-sibling", Started: start.Add(time.Millisecond), Code: -32000, Outcome: "error", DurationMS: 3},
		{CWD: root, Started: start.Add(2 * time.Second), Code: -32000, Outcome: "error", DurationMS: 4},
		{CWD: filepath.Dir(root), Started: start.Add(time.Millisecond), Outcome: "success", DurationMS: 1},
	}
	report := summarizeCheckoutDiscovery(rows, entries, filepath.Dir(root), root+"-sibling")
	require.Equal(t, 2, report.OutsidePendingRefusals, "a sibling and an already-published checkout are outside the pending scope")
	require.Equal(t, 2, report.OutsidePendingErrors)
	require.Equal(t, 1, report.ByCWD[root].Refusals)
	require.Equal(t, 1, report.ByCWD[filepath.Dir(root)].Latency.Count)
}

func TestDiscoveryScenarioOptionsHaveAnExplicitBaseline(t *testing.T) {
	args := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir(), "--scenario", checkoutDiscoveryScenario, "--agents", "2", "--edit-agents", "0", "--worktrees", "0"}
	options, err := parseRun(args)
	require.NoError(t, err)
	require.Equal(t, 0, options.Worktrees)
	require.Equal(t, checkoutDiscoveryScenario, options.Scenario)
	_, err = parseRun(append(args, "--agents", "1"))
	require.Error(t, err, "both initial repositories must retain a reading session")
}

func TestDiscoveryAddsWorktreesWhileOriginalSessionsKeepReading(t *testing.T) {
	first, second := gitFixture(t), gitFixture(t)
	second.Label = "other"
	p, err := newPrivateRun(t.Context(), t.TempDir(), "unused", []Corpus{first, second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(p.SocketDir) })
	p.Repos[0].Prefix, p.Repos[1].Prefix = "sample", "other"
	var mu sync.Mutex
	starts, ends := make(map[int]time.Time), make(map[int]time.Time)
	p.Socket = startStub(t, func(frame stubFrame) (any, *rpcError) {
		if frame.Kind != "" {
			return map[string]any{"view": map[string]any{"exact": true, "kind": "worktree", "repo_prefix": "sample"}}, nil
		}
		mu.Lock()
		if starts[frame.Connection].IsZero() {
			starts[frame.Connection] = time.Now()
		}
		ends[frame.Connection] = time.Now()
		mu.Unlock()
		return stubContent(map[string]any{"results": []any{map[string]any{"id": "sample/sample.go::Example", "name": "Example"}}}), nil
	})
	var clients []*socketClient
	for _, repo := range p.Repos {
		c, err := openMCP(t.Context(), p.Socket, repo.Path)
		require.NoError(t, err)
		clients = append(clients, c)
	}
	defer closeClients(clients)
	phase := Phase{Gate: make(map[string]json.RawMessage)}
	err = measureCheckoutDiscovery(t.Context(), runOptions{Agents: 2, Worktrees: 1, Duration: 1200 * time.Millisecond, Deadline: time.Second, Profile: "saturation", Seed: 1}, p, clients, &phase)
	failures := make(map[string]int)
	for _, a := range phase.Attempts {
		if a.Outcome != "success" && a.Outcome != "phase_censored" {
			failures[a.Error]++
		}
	}
	require.NoError(t, err, "attempt failures: %v", failures)
	var report checkoutDiscoveryReport
	require.NoError(t, json.Unmarshal(phase.Gate["checkout_discovery"], &report))
	require.Len(t, report.Checkouts, 1)
	added := report.Checkouts[0].Added
	require.NotNil(t, report.Checkouts[0].ExactViewLag.Value)
	mu.Lock()
	defer mu.Unlock()
	for _, conn := range []int{1, 2} {
		require.True(t, starts[conn].Before(added), "original session must run before worktree add")
		require.True(t, ends[conn].After(added), "original session must remain active during discovery")
	}
	root := report.Checkouts[0].Root
	require.Positive(t, report.ByCWD[root].Calls)
	require.Positive(t, report.ByCWD[filepath.Join(root, ".discovery-probe")].Calls)
	require.Zero(t, report.OutsidePendingRefusals)
}

func TestDiscoveryForeignCapacityIsNotExcusedAsPending(t *testing.T) {
	start := time.Unix(100, 0)
	root := filepath.Join(t.TempDir(), "foreign-new")
	entries := []checkoutDiscoveryObservation{{Root: root, FamilyRole: "foreign", Added: start}}
	rows := []Attempt{
		{CWD: root, Started: start.Add(time.Millisecond), Code: -32000, Outcome: "error", ErrorData: map[string]any{"reason": "in_progress"}},
		{CWD: root, Started: start.Add(time.Millisecond), Code: -32000, Outcome: "error", ErrorData: map[string]any{"reason": "family_capacity"}},
		{CWD: filepath.Join(root, "subdir"), Started: start.Add(time.Millisecond), Code: -32000, Outcome: "error", ErrorData: map[string]any{"reason": "stuck_capacity"}},
	}
	report := summarizeCheckoutDiscovery(rows, entries, root+"-primary", root+"-other")
	require.Equal(t, 3, report.PendingRefusals)
	require.Equal(t, 2, report.ForeignCapacityRefusals, "foreign family capacity cannot be hidden by the new-root pending window")
	require.Equal(t, "foreign_new_worktree", report.ByCWD[root].Class)
}

func TestDiscoveryNewRootsRetainEachUnrelatedFamily(t *testing.T) {
	first, second := gitFixture(t), gitFixture(t)
	second.Label = "other"
	second.Query = "ForeignOnly"
	p, err := newPrivateRun(t.Context(), t.TempDir(), "unused", []Corpus{first, second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(p.SocketDir) })
	p.Repos[0].Prefix, p.Repos[1].Prefix = "primary", "foreign"
	for _, family := range []int{0, 1, 0, 1} {
		require.NoError(t, p.addDiscoveryWorktree(t.Context(), family))
		linked := p.Repos[len(p.Repos)-1]
		primary := p.Repos[family]
		expected, err := p.git(t.Context(), primary.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		require.NoError(t, err)
		actual, err := p.git(t.Context(), linked.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		require.NoError(t, err)
		require.Equal(t, string(expected), string(actual), "linked checkout must share only its selected family")
		require.Equal(t, primary.Prefix, linked.Prefix)
		require.Equal(t, primary.Corpus.Query, linked.Corpus.Query)
	}
	require.Error(t, p.addDiscoveryWorktree(t.Context(), 2))
}
