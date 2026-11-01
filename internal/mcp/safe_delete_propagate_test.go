package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

func TestClassifyCallSite(t *testing.T) {
	a, _ := classifyCallSite("\tTarget()", "Target")
	require.Equal(t, "remove_line", a)

	a, _ = classifyCallSite("\tobj.Target()", "Target")
	require.Equal(t, "remove_line", a)

	a, _ = classifyCallSite("\tx := Target()", "Target")
	require.Equal(t, "manual", a)

	a, _ = classifyCallSite("\treturn Target()", "Target")
	require.Equal(t, "manual", a)

	a, _ = classifyCallSite("\tn := a + Target()", "Target")
	require.Equal(t, "manual", a)

	a, _ = classifyCallSite("\tdefer Target()", "Target")
	require.Equal(t, "remove_line", a)
}

func setupDeleteServer(t *testing.T) (*Server, graph.Store, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "del.go"), []byte(`package p

func Target() {}
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "caller.go"), []byte(`package p

func Caller() {
	Target()
}
`), 0o644))

	g := graph.New()
	reg := testRegistry()
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv, g, dir
}

func TestPropagateDeletePlanAndApply(t *testing.T) {
	srv, g, dir := setupDeleteServer(t)

	var targetID string
	for _, n := range g.AllNodes() {
		if n.Name == "Target" && n.Kind == graph.KindFunction {
			targetID = n.ID
		}
	}
	require.NotEmpty(t, targetID)

	callRes := func(args map[string]any) map[string]any {
		req := mcplib.CallToolRequest{}
		req.Params.Arguments = args
		res, err := srv.handleSafeDeleteSymbol(context.Background(), req)
		require.NoError(t, err)
		require.False(t, res.IsError, "safe_delete errored: %s", toolResultText(res))
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &payload))
		return payload
	}

	// 1. Plan (dry-run): one removable caller patch.
	plan := callRes(map[string]any{"id": targetID, "propagate": true, "dry_run": true})
	require.Equal(t, "propagation_plan", plan["status"])
	require.Equal(t, float64(1), plan["removable"])
	require.Equal(t, float64(0), plan["manual"])

	// 2. Apply: patches the caller and deletes the target.
	applied := callRes(map[string]any{"id": targetID, "propagate": true, "dry_run": false})
	status, _ := applied["status"].(string)
	require.Contains(t, []string{"deleted", "deleted_by_propagation"}, status)

	// The caller's standalone Target() call was removed.
	callerSrc, err := os.ReadFile(filepath.Join(dir, "caller.go"))
	require.NoError(t, err)
	require.NotContains(t, string(callerSrc), "Target()", "the call site should be removed")
}

// A propagate plan is built from one read of each caller line, and the apply
// re-reads the file later. A line edited or shifted in between must be
// refused rather than deleted by number, every refusal must be reported once,
// and a file with nothing left to remove must not be rewritten.
func TestPropagateRefusesStaleLines(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644))
	}
	write("del.go", "package p\n\nfunc Target() {}\n")
	write("edited.go", "package p\n\nfunc Edited() {\n\tTarget()\n}\n")
	write("shifted.go", "package p\n\nfunc Shifted() {\n\tTarget()\n}\n")
	write("gate.go", "package p\n\nfunc Gate() {\n\tTarget(\n\t)\n}\n")

	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)

	var target *graph.Node
	for _, n := range g.AllNodes() {
		if n.Name == "Target" && n.Kind == graph.KindFunction {
			target = n
		}
	}
	require.NotNil(t, target)
	ctx := context.Background()
	plan := srv.buildPropagationPlan(ctx, g, target)
	removable := 0
	for _, p := range plan {
		if p.Action == "remove_line" {
			removable++
		}
	}
	require.Equal(t, 2, removable, "edited.go and shifted.go each hold one standalone call: %+v", plan)

	// gate.go: a planned line whose removal breaks the file, and a planned
	// line past the end of the same file.
	gateAbs := filepath.Join(dir, "gate.go")
	plan = append(plan,
		callerPatch{File: "gate.go", Line: 4, CurrentText: "Target(", Action: "remove_line", abs: gateAbs},
		callerPatch{File: "gate.go", Line: 40, CurrentText: "Target()", Action: "remove_line", abs: gateAbs},
	)

	// Concurrent edits after planning.
	write("edited.go", "package p\n\nfunc Edited() {\n\tKeep()\n}\n")
	write("shifted.go", "package p\n\nfunc Shifted() {\n\tprepare()\n\tTarget()\n}\n")

	before := map[string][]byte{}
	stats := map[string]os.FileInfo{}
	for _, name := range []string{"edited.go", "shifted.go", "gate.go"} {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		info, err := os.Stat(path)
		require.NoError(t, err)
		before[name], stats[name] = data, info
	}

	applied, failed, err := srv.applyRemoveLinePatches(ctx, plan)
	require.NoError(t, err)
	require.Empty(t, applied)

	got := map[string]string{}
	for _, p := range failed {
		key := fmt.Sprintf("%s:%d", p.File, p.Line)
		_, dup := got[key]
		require.False(t, dup, "patch %s reported twice in failed: %+v", key, failed)
		got[key] = p.Reason
	}
	require.Equal(t, map[string]string{
		"edited.go:4":  "line_changed",
		"shifted.go:4": "line_changed",
		"gate.go:4":    "parse_errors",
		"gate.go:40":   "line_out_of_range",
	}, got)

	for name, data := range before {
		path := filepath.Join(dir, name)
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, string(data), string(after), "%s must stay byte-identical", name)
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.True(t, os.SameFile(stats[name], info), "%s was rewritten although every patch was refused", name)
	}
}

// Every caller file is locked before any is written. A lock the apply cannot
// take, here one held past the request's deadline, must leave all of them as
// they were, not only the files the apply had not reached yet.
func TestPropagateWritesNothingWhenACallerLockFails(t *testing.T) {
	dir := t.TempDir()
	const src = "package p\n\nfunc C() {\n\tTarget()\n}\n"
	for i := range 8 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("c%d.go", i)), []byte(src), 0o644))
	}
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)

	var plan []callerPatch
	for i := range 8 {
		name := fmt.Sprintf("c%d.go", i)
		plan = append(plan, callerPatch{File: name, Line: 4, CurrentText: "Target()", Action: "remove_line", abs: filepath.Join(dir, name)})
	}
	release, err := acquireMutationPath(context.Background(), plan[len(plan)-1].abs)
	require.NoError(t, err)
	defer release()

	// Map order picks the file a file-by-file apply would lock first, so
	// repeat until such an apply would have written one.
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		applied, _, err := srv.applyRemoveLinePatches(ctx, plan)
		cancel()
		require.Error(t, err)
		require.Empty(t, applied)
		for _, p := range plan {
			data, err := os.ReadFile(p.abs)
			require.NoError(t, err)
			require.Equal(t, src, string(data), "%s was rewritten although the apply failed", p.File)
		}
	}
}
