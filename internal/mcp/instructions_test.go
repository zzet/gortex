package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/toolref"
)

// TestInitialize_ReturnsInstructions drives a real `initialize`
// JSON-RPC frame through HandleMessage and asserts the server-level
// `instructions` field is populated — the field MCP clients surface
// to the agent as "how to drive this server".
func TestInitialize_ReturnsInstructions(t *testing.T) {
	srv := newFullTestServer(t)
	frame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)

	reply := srv.MCPServer().HandleMessage(context.Background(), frame)
	if reply == nil {
		t.Fatal("HandleMessage returned nil for initialize")
	}
	out, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	var parsed struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if strings.TrimSpace(parsed.Result.Instructions) == "" {
		t.Fatalf("initialize response carried no instructions field; got: %s", out)
	}
	require.Equal(t, codingAgentInstructions, parsed.Result.Instructions)
	require.Equal(t, 1, strings.Count(parsed.Result.Instructions, profiles.WorktreeBranchRoutingPolicy))
}

func TestServerInstructions_NonEmpty(t *testing.T) {
	if strings.TrimSpace(serverInstructions) == "" {
		t.Fatal("serverInstructions constant is empty")
	}
	if scrubControlChars(serverInstructions) != serverInstructions {
		t.Error("serverInstructions carries control characters")
	}
	if got := strings.Count(serverInstructions, profiles.WorktreeBranchRoutingPolicy); got != 1 {
		t.Fatalf("serverInstructions embeds canonical worktree policy %d times, want once", got)
	}
	if got := strings.Count(codingAgentInstructions, profiles.WorktreeBranchRoutingPolicy); got != 1 {
		t.Fatalf("codingAgentInstructions embeds canonical worktree policy %d times, want once", got)
	}
}

func TestStateAwareInstructionsWaitForAutomaticFamilyCheckout(t *testing.T) {
	for _, routed := range []bool{true, false} {
		name := "routed"
		if !routed {
			name = "ready-without-route"
		}
		t.Run(name, func(t *testing.T) {
			v := newViewStack(t)
			if !routed {
				require.NoError(t, v.store.Catalog().DeleteCheckoutRoute(t.Context(), viewTestWorktree))
			}
			serves, err := v.srv.CheckoutServesCWDChecked(t.Context(), v.worktreeRoot)
			require.NoError(t, err)
			require.True(t, serves)
			got := v.srv.stateAwareInstructions(v.worktreeRoot)
			require.NotContains(t, got, "awaiting automatic discovery")
			require.NotContains(t, got, "checkout discovery is pending")
			require.Contains(t, got, serverInstructions)
		})
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	main := filepath.Join(t.TempDir(), "main")
	gitInitWorktreeRepo(t, main)
	linked := filepath.Join(t.TempDir(), "linked")
	gitInit(t, main, "worktree", "add", "-q", "-b", "feature", linked)

	srv, _ := newWorktreeMCPServer(t, config.RepoEntry{Path: main, Name: "main"})
	got := srv.stateAwareInstructions(linked)
	require.Contains(t, got, "checkout discovery is pending")
	require.Contains(t, got, "Do not run `gortex track`")
	require.NotContains(t, got, "`gortex track "+linked+"`")
	require.Equal(t, 1, strings.Count(got, profiles.WorktreeBranchRoutingPolicy))

	reverseSrv, _ := newWorktreeMCPServer(t, config.RepoEntry{Path: linked, Name: "linked"})
	reverse := reverseSrv.stateAwareInstructions(main)
	require.Contains(t, reverse, "checkout discovery is pending")
	require.Contains(t, reverse, "Do not run `gortex track`")
	require.NotContains(t, reverse, "`gortex track "+main+"`")

	unrelated := filepath.Join(t.TempDir(), "unrelated")
	require.NoError(t, os.MkdirAll(unrelated, 0o755))
	unrelatedInstructions := srv.stateAwareInstructions(unrelated)
	require.Contains(t, unrelatedInstructions, "gortex track "+unrelated)
}

func TestPendingTextNoReconnect(t *testing.T) {
	for _, text := range []string{ServerInstructionsPendingAutomaticCheckout("/checkout"), ServerInstructionsUntracked("/other")} {
		withoutException := strings.ReplaceAll(text, "no reconnect is needed", "")
		require.NotContains(t, strings.ToLower(withoutException), "reconnect")
		require.NotContains(t, text, "may return repo_not_tracked")
	}
}

func TestStateAwareInstructionsChecksAdmissionErrors(t *testing.T) {
	v := newViewStack(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := v.srv.CheckoutServesCWDChecked(ctx, v.worktreeRoot)
	require.Error(t, err)
	got := v.srv.stateAwareInstructionsContext(ctx, v.worktreeRoot, serverInstructions)
	require.Equal(t, CheckoutAdmissionInstructions(v.worktreeRoot, err), got)
	require.NotContains(t, got, "gortex track")
}

func TestStateAwareInstructionsGraceAndTransitionDoNotRecommendTrack(t *testing.T) {
	for _, state := range []store_sqlite.CheckoutState{store_sqlite.CheckoutStateAvailabilityGrace, store_sqlite.CheckoutStateReconciling} {
		t.Run(string(state), func(t *testing.T) {
			v := newViewStack(t)
			gitInitWorktreeRepo(t, v.repoRoot)
			gitInit(t, v.repoRoot, "worktree", "add", "-q", "-b", "feature", v.worktreeRoot)
			v.setWorktreeState(t, state)
			got := v.srv.stateAwareInstructions(v.worktreeRoot)
			require.Contains(t, got, "checkout discovery is pending")
			require.Contains(t, got, "view_building")
			require.NotContains(t, got, "gortex track "+v.worktreeRoot)
		})
	}
}

// TestStateAwareInstructionsVariants proves the F5 contract: the initialize
// `instructions` field adapts to THIS connection's cwd. An uncovered cwd gets
// the terse activation affordance; a covered cwd gets the base guidance plus a
// live-facts block (tracked-repo names + count, active project, daemon warmup
// state). The handshake subtests prove the after-initialize hook delivers
// those live facts to ANY MCP client — including one that never runs a
// SessionStart hook — which codegraph's static instructions cannot.
func TestStateAwareInstructionsVariants(t *testing.T) {
	repoA := setupMiniRepo(t, "repo-a")
	repoB := setupMiniRepo(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	for _, entry := range cm.Global().ReposSnapshot() {
		_, err := mi.TrackRepoCtx(context.Background(), entry)
		require.NoError(t, err)
	}

	srv := NewServer(query.NewEngine(g), g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		ConfigManager: cm,
		MultiIndexer:  mi,
	})

	// A live readiness snapshot so the covered variant carries the warmup
	// fact a SessionStart-hook-less client could otherwise never learn.
	srv.PublishReadiness("ready", true, nil)

	instructionsFromHandshake := func(t *testing.T, cwd string, id int) string {
		t.Helper()
		ctx := WithSessionCWD(context.Background(), cwd)
		frame := []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) +
			`,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
		reply := srv.MCPServer().HandleMessage(ctx, frame)
		require.NotNil(t, reply)
		out, err := json.Marshal(reply)
		require.NoError(t, err)
		var parsed struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		return parsed.Result.Instructions
	}

	t.Run("uncovered_cwd_gets_terse_activation_variant", func(t *testing.T) {
		stranger := t.TempDir() // not tracked, under no repo
		got := srv.stateAwareInstructions(stranger)
		require.Contains(t, got, "INACTIVE")
		require.Contains(t, got, "gortex track "+stranger)
		require.NotContains(t, got, "Tracked repositories",
			"the inactive variant must not leak the live-facts block")
	})

	t.Run("covered_cwd_appends_live_facts", func(t *testing.T) {
		got := srv.stateAwareInstructions(repoA)
		require.Contains(t, got, "explore", "base guidance must still lead")
		require.Contains(t, got, "Tracked repositories", "live workspace facts must be appended")
		require.Contains(t, got, "repo-a")
		require.Contains(t, got, "Index status: ready", "warmup readiness must be surfaced")
	})

	t.Run("covered_handshake_delivers_live_facts", func(t *testing.T) {
		// The whole point: a non-Claude-Code client learns the workspace
		// shape + warmup state from initialize alone.
		instr := instructionsFromHandshake(t, repoA, 1)
		require.Contains(t, instr, "Tracked repositories")
		require.Contains(t, instr, "repo-a")
		require.Contains(t, instr, "Index status: ready")
	})

	t.Run("uncovered_handshake_is_inactive", func(t *testing.T) {
		stranger := t.TempDir()
		instr := instructionsFromHandshake(t, stranger, 2)
		require.Contains(t, instr, "INACTIVE")
		require.Contains(t, instr, "gortex track "+stranger)
	})
}

func TestInitializeSharedTransientGuidanceAcrossStates(t *testing.T) {
	require.True(t, strings.HasPrefix(codingAgentInstructions, "When Gortex can serve the step: "), "compact workflow must not negate the degraded exception")
	loading, _, repo := newLoadingServer(t)
	main := filepath.Join(t.TempDir(), "main")
	gitInitWorktreeRepo(t, main)
	linked := filepath.Join(t.TempDir(), "linked")
	gitInit(t, main, "worktree", "add", "-q", "-b", "feature", linked)
	routed, _ := newWorktreeMCPServer(t, config.RepoEntry{Path: main, Name: "main"})
	for _, base := range []string{serverInstructions, codingAgentInstructions} {
		for name, instructions := range map[string]string{
			"ready":             routed.stateAwareInstructionsWithBase(main, base),
			"repo-loading":      loading.stateAwareInstructionsWithBase(repo, base),
			"pending-discovery": routed.stateAwareInstructionsWithBase(linked, base),
		} {
			for _, sentence := range []string{toolref.TransportClause(), toolref.DegradedModeLine()} {
				require.Contains(t, instructions, sentence, name)
			}
			for _, stop := range []string{"integration failure and stop", "integration failure, stop", "Wait for reconciliation and reconnect", "To activate: run"} {
				require.NotContains(t, instructions, stop, name)
			}
		}
	}
}
