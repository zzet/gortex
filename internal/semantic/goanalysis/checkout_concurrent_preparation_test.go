package goanalysis

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

func concurrentCheckoutGraph(t *testing.T, root string) *graph.Graph {
	t.Helper()
	g := graph.New()
	for _, node := range scopeHandleGraph().AllNodes() {
		node.ID = "r/" + node.ID
		node.FilePath = "r/" + node.FilePath
		node.RepoPrefix = "r"
		g.AddNode(node)
	}
	text := "package api\n\n"
	for i := 0; i < 16; i++ {
		name := fmt.Sprintf("Extra%d", i)
		text += fmt.Sprintf("func %s() int { return %d }\n", name, i)
		g.AddNode(&graph.Node{ID: "r/api/extra.go::" + name, FilePath: "r/api/extra.go", RepoPrefix: "r", Name: name, Kind: graph.KindFunction, Language: "go", StartLine: 3 + i, EndLine: 3 + i})
	}
	writeFile(t, root, "api/extra.go", text)
	return g
}

func TestCheckoutPreparationCapabilityUsesActualAdmissionAndScope(t *testing.T) {
	root := scopeFixture(t)
	p := newTestProvider(t)
	defer p.Close()
	scope := semantic.CheckoutCompilerScope{HandleRoots: true, TypecheckCache: true}
	gate, _ := p.admissionGates()
	require.Equal(t, cap(gate) >= 2, p.ConcurrentCheckoutPreparation(t.Context(), root, "r", scope, []string{"r/api/api.go"}))
	scope.HandleRoots = false
	require.False(t, p.ConcurrentCheckoutPreparation(t.Context(), root, "r", scope, []string{"r/api/api.go"}))
	scope.HandleRoots = true
	scope.ManifestChanged = true
	require.False(t, p.ConcurrentCheckoutPreparation(t.Context(), root, "r", scope, []string{"r/api/api.go"}))
	scope.ManifestChanged = false
	require.False(t, p.ConcurrentCheckoutPreparation(t.Context(), root, "r", scope, []string{"elsewhere/api.go"}))
	p.heavyGate = make(chan struct{}, 1)
	require.False(t, p.ConcurrentCheckoutPreparation(t.Context(), root, "r", scope, []string{"r/api/api.go"}))
}

func TestConcurrentCheckoutManagerKeepsOtherRootAdmittedAndSameRootWaitCancellable(t *testing.T) {
	p := newTestProvider(t)
	gate, _ := p.admissionGates()
	if cap(gate) < 2 {
		t.Skip("actual provider has one compiler slot; detached preparation is conservatively disabled")
	}
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	mgr.RegisterProvider(p)
	bgRoot, fgRoot := scopeFixture(t), scopeFixture(t)
	bgGraph, fgGraph := concurrentCheckoutGraph(t, bgRoot), concurrentCheckoutGraph(t, fgRoot)
	state := p.typecheckState(filepath.Clean(bgRoot), goManifestDigest(bgRoot))
	state.mu.Lock()
	var once sync.Once
	unlock := func() { once.Do(state.mu.Unlock) }
	t.Cleanup(func() { unlock(); require.NoError(t, mgr.Close()) })
	scope := semantic.CheckoutCompilerScope{HandleRoots: true, StripSiblingBodies: true, TypecheckCache: true}
	request := func(root string) semantic.CheckoutEnrichRequest {
		return semantic.CheckoutEnrichRequest{RepoPrefix: "r", CheckoutID: root, Root: root, Fingerprint: root, MinLanguageNodes: semantic.EnrichmentAdmissionFloor(), Compiler: scope}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type outcome struct {
		report semantic.CheckoutEnrichReport
		err    error
	}
	background := make(chan outcome, 1)
	go func() { r, e := mgr.EnrichCheckoutContext(ctx, bgGraph, request(bgRoot)); background <- outcome{r, e} }()
	for len(gate) != 1 {
		select {
		case <-ctx.Done():
			t.Fatal("background manager never entered actual compiler admission")
		case <-time.After(time.Millisecond):
		}
	}
	// The background holds one admitted program and waits on its own retained
	// state. A different root must execute its real provider pass in the other.
	foreground, err := mgr.EnrichCheckoutContext(ctx, fgGraph, request(fgRoot))
	require.NoError(t, err)
	require.Contains(t, foreground.Ran, "go")
	require.NotNil(t, foreground.Compiler)
	require.Equal(t, semantic.CompilerScopeHandleRoots, foreground.Compiler.Scope)
	known := false
	for _, edge := range fgGraph.AllEdges() {
		if edge.From == "r/api/api.go::Measure" && edge.To == "r/impl/impl.go::Double" && edge.Kind == graph.EdgeCalls && edge.Origin == graph.OriginLSPResolved {
			known = true
		}
	}
	require.True(t, known, "foreground real compiler produced no known resolved call")
	select {
	case out := <-background:
		t.Fatalf("background completed while its state was held: %+v", out)
	default:
	}
	sameCtx, sameCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	sameGraph := concurrentCheckoutGraph(t, bgRoot)
	_, err = mgr.EnrichCheckoutContext(sameCtx, sameGraph, request(bgRoot))
	sameCancel()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, len(gate), "cancelled same-root pass retained the spare compiler token")
	unlock()
	select {
	case out := <-background:
		require.NoError(t, out.err)
		require.Contains(t, out.report.Ran, "go")
	case <-ctx.Done():
		t.Fatal("background did not resume after its retained state was released")
	}
	require.Equal(t, 0, len(gate), "completed manager passes leaked compiler admission")
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal(ctx.Err())
	}
}
