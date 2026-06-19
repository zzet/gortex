package ohmypi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

func TestOhMyPiCreatesAndMerges(t *testing.T) {
	env, _ := agentstest.NewEnv(t)

	// Create sentinel .omp directory so Detect returns true.
	if err := os.MkdirAll(filepath.Join(env.Root, ".omp"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := New()

	// Apply should write the MCP config to .omp/mcp.json.
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Detected {
		t.Fatal("expected Detected=true after creating .omp/")
	}
	if !res.Configured {
		t.Fatal("expected Configured=true after apply")
	}

	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 1})
	mcp := agentstest.ReadJSON(t, filepath.Join(env.Root, ".omp", "mcp.json"))
	servers := mcp["mcpServers"].(map[string]any)
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex server missing: %v", servers)
	}

	// Assert idempotence on re-run.
	agentstest.AssertIdempotent(t, a, env)
}

func TestOhMyPiGlobalIsNoOp(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal

	// Even if .omp directory exists, Detect should return false in ModeGlobal.
	if err := os.MkdirAll(filepath.Join(env.Root, ".omp"), 0o755); err != nil {
		t.Fatal(err)
	}

	a := New()

	detected, err := a.Detect(env)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if detected {
		t.Fatal("expected Detect to return false in ModeGlobal")
	}

	// Plan should be empty.
	plan, err := a.Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Files) > 0 {
		t.Fatalf("expected empty plan in ModeGlobal, got: %v", plan.Files)
	}

	// Apply should return early and not write files.
	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Configured {
		t.Fatal("expected Configured=false in ModeGlobal")
	}
	if len(res.Files) > 0 {
		t.Fatalf("expected no files touched in ModeGlobal, got: %v", res.Files)
	}
}
