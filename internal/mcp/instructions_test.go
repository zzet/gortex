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
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
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
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	main := filepath.Join(t.TempDir(), "main")
	gitInitWorktreeRepo(t, main)
	linked := filepath.Join(t.TempDir(), "linked")
	gitInit(t, main, "worktree", "add", "-q", "-b", "feature", linked)

	srv, _ := newWorktreeMCPServer(t, config.RepoEntry{Path: main, Name: "main"})
	got := srv.stateAwareInstructions(linked)
	require.Contains(t, got, "awaiting automatic discovery")
	require.Contains(t, got, "Do not run `gortex track`")
	require.NotContains(t, got, "`gortex track "+linked+"`")
	require.Equal(t, 1, strings.Count(got, profiles.WorktreeBranchRoutingPolicy))

	reverseSrv, _ := newWorktreeMCPServer(t, config.RepoEntry{Path: linked, Name: "linked"})
	reverse := reverseSrv.stateAwareInstructions(main)
	require.Contains(t, reverse, "awaiting automatic discovery")
	require.Contains(t, reverse, "Do not run `gortex track`")
	require.NotContains(t, reverse, "`gortex track "+main+"`")

	unrelated := filepath.Join(t.TempDir(), "unrelated")
	require.NoError(t, os.MkdirAll(unrelated, 0o755))
	unrelatedInstructions := srv.stateAwareInstructions(unrelated)
	require.Contains(t, unrelatedInstructions, "gortex track "+unrelated)
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
	_, err = mi.IndexAll()
	require.NoError(t, err)

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
