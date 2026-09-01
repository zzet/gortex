package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

func TestNormalizeSweepMode(t *testing.T) {
	cases := map[string]string{
		"demand":   sweepModeDemand,
		"DEMAND":   sweepModeDemand,
		"  full  ": sweepModeFull,
		"Full":     sweepModeFull,
		"off":      sweepModeOff,
		"none":     sweepModeOff, // alias
		"NONE":     sweepModeOff,
		"":         "", // empty → no opinion, caller falls through
		"bogus":    "", // unrecognised → no opinion
	}
	for in, want := range cases {
		assert.Equalf(t, want, normalizeSweepMode(in), "normalizeSweepMode(%q)", in)
	}
}

func TestResolveSweepMode_EnvWinsOverConfig(t *testing.T) {
	// No env, no config, no spec default → demand default.
	t.Setenv(SweepEnv, "")
	assert.Equal(t, sweepModeDemand, resolveSweepMode("", ""), "empty env + empty config + empty spec default resolves to the demand default")

	// Config alone is honoured when the env is unset.
	assert.Equal(t, sweepModeFull, resolveSweepMode("full", ""), "configured value is used when env is unset")
	assert.Equal(t, sweepModeOff, resolveSweepMode("off", ""))

	// An unrecognised config value falls back to the default rather than failing.
	assert.Equal(t, sweepModeDemand, resolveSweepMode("bogus", ""))

	// Env wins over config in both directions.
	t.Setenv(SweepEnv, "full")
	assert.Equal(t, sweepModeFull, resolveSweepMode("off", ""), "env override must win over a configured off")
	t.Setenv(SweepEnv, "off")
	assert.Equal(t, sweepModeOff, resolveSweepMode("full", ""), "env override must win over a configured full")

	// An unrecognised env value is ignored — config (then default) takes over.
	t.Setenv(SweepEnv, "garbage")
	assert.Equal(t, sweepModeFull, resolveSweepMode("full", ""), "an unrecognised env value falls through to config")
	assert.Equal(t, sweepModeDemand, resolveSweepMode("", ""), "an unrecognised env value with no config falls through to the default")
}

func TestResolveSweepMode_SpecDefault(t *testing.T) {
	t.Setenv(SweepEnv, "")

	// A spec default is used when neither env nor config is set.
	assert.Equal(t, sweepModeFull, resolveSweepMode("", sweepModeFull), "spec default is used when env + config are unset")

	// Operator config outranks the spec default in both directions.
	assert.Equal(t, sweepModeDemand, resolveSweepMode("demand", sweepModeFull), "configured value wins over a full spec default")
	assert.Equal(t, sweepModeOff, resolveSweepMode("off", sweepModeFull), "configured off wins over a full spec default")

	// The env override outranks both config and the spec default.
	t.Setenv(SweepEnv, "demand")
	assert.Equal(t, sweepModeDemand, resolveSweepMode("", sweepModeFull), "env wins over a full spec default")

	// An unrecognised spec default is ignored — falls through to the demand default.
	t.Setenv(SweepEnv, "")
	assert.Equal(t, sweepModeDemand, resolveSweepMode("", "bogus"), "an unrecognised spec default falls through to the demand default")
}

func TestSweepFile(t *testing.T) {
	// off never sweeps, even a demand-bearing or dispatch-relevant file.
	assert.False(t, sweepFile(sweepModeOff, 5, false))
	assert.False(t, sweepFile(sweepModeOff, 0, true))
	// full always sweeps regardless of demand / dispatch.
	assert.True(t, sweepFile(sweepModeFull, 0, false))
	assert.True(t, sweepFile(sweepModeFull, 3, false))
	// demand default: swept on demand > 0 OR a dispatch-relevant declaration.
	assert.True(t, sweepFile(sweepModeDemand, 1, false))
	assert.False(t, sweepFile(sweepModeDemand, 0, false))
	assert.True(t, sweepFile(sweepModeDemand, 0, true), "a dispatch-relevant file is swept even with zero demand")
	assert.True(t, sweepFile(sweepModeDemand, 2, true))
	// An unrecognised residue behaves like the demand default.
	assert.True(t, sweepFile("bogus", 2, false))
	assert.True(t, sweepFile("bogus", 0, true))
	assert.False(t, sweepFile("bogus", 0, false))
}

func TestNodeHasSemanticType(t *testing.T) {
	assert.False(t, nodeHasSemanticType(nil))
	assert.False(t, nodeHasSemanticType(&graph.Node{}))
	assert.False(t, nodeHasSemanticType(&graph.Node{Meta: map[string]any{"semantic_type": ""}}))
	assert.False(t, nodeHasSemanticType(&graph.Node{Meta: map[string]any{"other": "x"}}))
	assert.False(t, nodeHasSemanticType(&graph.Node{Meta: map[string]any{"semantic_type": 42}}))
	assert.True(t, nodeHasSemanticType(&graph.Node{Meta: map[string]any{"semantic_type": "func()"}}))
}

func TestEffectiveSweepMode(t *testing.T) {
	p := &Provider{sweepMode: "off"}
	t.Setenv(SweepEnv, "")
	assert.Equal(t, sweepModeOff, p.effectiveSweepMode(), "the router-configured field is honoured when env is unset")
	t.Setenv(SweepEnv, "full")
	assert.Equal(t, sweepModeFull, p.effectiveSweepMode(), "the env override wins over the configured field")

	empty := &Provider{}
	t.Setenv(SweepEnv, "")
	assert.Equal(t, sweepModeDemand, empty.effectiveSweepMode(), "an unset field and unset env resolve to the demand default")

	// A server spec's DefaultSweepMode is honoured when neither env nor the
	// router field is set — and both still override it.
	specFull := &Provider{spec: &ServerSpec{DefaultSweepMode: sweepModeFull}}
	assert.Equal(t, sweepModeFull, specFull.effectiveSweepMode(), "the spec default is used when env + field are unset")
	specWithField := &Provider{sweepMode: "demand", spec: &ServerSpec{DefaultSweepMode: sweepModeFull}}
	assert.Equal(t, sweepModeDemand, specWithField.effectiveSweepMode(), "the router-configured field wins over the spec default")
	t.Setenv(SweepEnv, "off")
	assert.Equal(t, sweepModeOff, specFull.effectiveSweepMode(), "the env override wins over the spec default")

	// The rust-analyzer registry spec ships a full-sweep default.
	t.Setenv(SweepEnv, "")
	ra := &Provider{spec: SpecByName("rust-analyzer")}
	require.NotNil(t, ra.spec)
	assert.Equal(t, sweepModeFull, ra.effectiveSweepMode(), "rust-analyzer defaults to the full sweep out of the box")
}

// enrichCapturingResult runs a full enrichment pass and returns its result,
// guarding against a hang with a hard timeout.
func enrichCapturingResult(t *testing.T, p *Provider, g graph.Store, repoRoot string) *semantic.EnrichResult {
	t.Helper()
	type out struct {
		res *semantic.EnrichResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := p.Enrich(g, repoRoot)
		done <- out{res, err}
	}()
	select {
	case o := <-done:
		require.NoError(t, o.err)
		require.NotNil(t, o.res)
		return o.res
	case <-time.After(10 * time.Second):
		t.Fatal("Enrich timed out")
		return nil
	}
}

// hoverURIRecorder registers a hover handler that records the base name of
// each file the server is asked to hover, so a test can assert exactly which
// files the per-file sweep visited. The returned map is final once Enrich
// returns (every hover request is issued and awaited before the pass ends).
func hoverURIRecorder(server *instrumentedServer) (*sync.Mutex, map[string]bool) {
	var mu sync.Mutex
	hovered := map[string]bool{}
	server.handle("textDocument/hover", func(params json.RawMessage) (any, *jsonRPCError) {
		var req struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		_ = json.Unmarshal(params, &req)
		mu.Lock()
		hovered[filepath.Base(uriToAbsPath(req.TextDocument.URI))] = true
		mu.Unlock()
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func X()"}}, nil
	})
	return &mu, hovered
}

// seedDemand gives declName enrichment demand by minting the unresolved
// same-name call stub the demand check looks for. Confidence 1.0 keeps the
// stub edge out of the ambiguous-edge confirm target set, so the ONLY pass
// that can touch the file is the per-file sweep under test.
func seedDemand(g graph.Store, callerID, declName string) {
	g.AddEdge(&graph.Edge{
		From: callerID, To: graph.UnresolvedMarker + "*." + declName,
		Kind: graph.EdgeCalls, Confidence: 1.0,
	})
}

// Default mode is demand-gated: the per-file hover sweep visits only files
// whose declarations still carry unresolved same-name candidates. A
// fully-resolved file is skipped, so a warm restart pays no sweep.
func TestLSP_Enrich_SweepDemandGatedByDefault(t *testing.T) {
	t.Setenv(SweepEnv, "") // hermetic: no external override, no config → demand default

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "plain.go"),
		[]byte("package p\n\nfunc Plain() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "hot.go"),
		[]byte("package p\n\nfunc Hot() {}\nfunc caller() {}\n"), 0o644))

	server := newInstrumentedServer()
	mu, hovered := hoverURIRecorder(server)

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{ID: "plain.go::Plain", Kind: graph.KindFunction, Name: "Plain",
		FilePath: "plain.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "hot.go::Hot", Kind: graph.KindFunction, Name: "Hot",
		FilePath: "hot.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "hot.go::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "hot.go", StartLine: 4, EndLine: 4, Language: "go"})
	seedDemand(g, "hot.go::caller", "Hot") // hot.go now carries demand; plain.go does not

	res := enrichCapturingResult(t, p, g, repoRoot)

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, hovered["hot.go"], "the demand-bearing file must be swept")
	assert.False(t, hovered["plain.go"], "the fully-resolved file must be skipped under the demand default")
	assert.Positive(t, res.NodesEnriched, "the swept file's hovers must land stamps")

	require.Eventually(t, func() bool { return server.wasOpened(pathToURI(filepath.Join(repoRoot, "hot.go"))) },
		2*time.Second, 5*time.Millisecond, "hot.go should be opened by the sweep")
	assert.False(t, server.wasOpened(pathToURI(filepath.Join(repoRoot, "plain.go"))),
		"plain.go must never be opened under the demand default")
}

// Under the demand default, a file whose only enrichable work is a type
// hierarchy (a class with an extends clause — zero call demand) must still be
// swept, or the extends / supertype edges only this sweep recovers are
// silently dropped. Regression guard: enrichNodeHasUnresolvedDemand counts
// callables only, so such a file scores demand == 0; the dispatch-relevant
// disjunct is what keeps it in the sweep.
func TestLSP_Enrich_SweepDispatchRelevantRecoversHierarchyByDefault(t *testing.T) {
	t.Setenv(SweepEnv, "") // hermetic: exercise the demand default, not full

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(repoRoot, "h.ts"),
		[]byte("class Animal {}\nclass Dog extends Animal {}\n"),
		0o644,
	))

	server := newFakeLSPServer()
	server.handle("textDocument/hover", func(_ json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/implementation", func(_ json.RawMessage) (any, *jsonRPCError) { return nil, nil })
	server.handle("textDocument/prepareTypeHierarchy", func(_ json.RawMessage) (any, *jsonRPCError) {
		return []TypeHierarchyItem{{
			Name:           "Dog",
			URI:            pathToURI(filepath.Join(repoRoot, "h.ts")),
			SelectionRange: Range{Start: Position{Line: 1, Character: 6}, End: Position{Line: 1, Character: 9}},
		}}, nil
	})
	server.handle("typeHierarchy/supertypes", func(_ json.RawMessage) (any, *jsonRPCError) {
		return []TypeHierarchyItem{{
			Name:           "Animal",
			URI:            pathToURI(filepath.Join(repoRoot, "h.ts")),
			SelectionRange: Range{Start: Position{Line: 0, Character: 6}, End: Position{Line: 0, Character: 12}},
		}}, nil
	})
	server.handle("typeHierarchy/subtypes", func(_ json.RawMessage) (any, *jsonRPCError) { return nil, nil })

	p, cleanup := providerWithFakeServer(t, server, []string{"typescript"})
	defer cleanup()

	g := graph.New()
	// Only type declarations — no functions, so demand == 0 for this file.
	// The extends clause is syntax, so the extractor emitted its edge even
	// though it could not resolve the target — the unresolvable-base shape
	// that keeps the file dispatch-relevant (see typeIsDispatchRelevant).
	g.AddNode(&graph.Node{ID: "h.ts::Animal", Kind: graph.KindType, Name: "Animal",
		FilePath: "h.ts", StartLine: 1, EndLine: 1, Language: "typescript"})
	g.AddNode(&graph.Node{ID: "h.ts::Dog", Kind: graph.KindType, Name: "Dog",
		FilePath: "h.ts", StartLine: 2, EndLine: 2, Language: "typescript"})
	g.AddEdge(&graph.Edge{From: "h.ts::Dog", To: graph.UnresolvedMarker + "Animal",
		Kind: graph.EdgeExtends, FilePath: "h.ts", Line: 2})

	done := make(chan error, 1)
	go func() {
		_, err := p.Enrich(g, repoRoot)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Enrich timed out")
	}

	var ext *graph.Edge
	for _, e := range g.GetOutEdges("h.ts::Dog") {
		if e.Kind == graph.EdgeExtends && e.To == "h.ts::Animal" {
			ext = e
			break
		}
	}
	require.NotNil(t, ext, "a type-only (zero-demand) file must still be swept under the demand default so its extends edge is recovered")
}

// A node that already carries a semantic_type stamp (e.g. reloaded on a warm
// restart) must not be re-hovered by the sweep, while an unstamped node in the
// same pass still is. The file is force-swept so the skip — not the gate — is
// the only reason the stamped node is untouched.
func TestLSP_Enrich_SweepSkipsHoverForAlreadyStampedNode(t *testing.T) {
	t.Setenv(SweepEnv, "full") // force the sweep; isolate the hover-skip from the gate

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "stamped.go"),
		[]byte("package p\n\nfunc Stamped() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "fresh.go"),
		[]byte("package p\n\nfunc Fresh() {}\n"), 0o644))

	server := newInstrumentedServer()
	mu, hovered := hoverURIRecorder(server)

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{ID: "stamped.go::Stamped", Kind: graph.KindFunction, Name: "Stamped",
		FilePath: "stamped.go", StartLine: 3, EndLine: 3, Language: "go",
		Meta: map[string]any{"semantic_type": "func() original"}}) // already enriched
	g.AddNode(&graph.Node{ID: "fresh.go::Fresh", Kind: graph.KindFunction, Name: "Fresh",
		FilePath: "fresh.go", StartLine: 3, EndLine: 3, Language: "go"})

	enrichCapturingResult(t, p, g, repoRoot)

	mu.Lock()
	assert.True(t, hovered["fresh.go"], "an unstamped node must be hovered under a full sweep")
	assert.False(t, hovered["stamped.go"], "a node already carrying a semantic_type must not be re-hovered")
	mu.Unlock()

	assert.Equal(t, "func() original", g.GetNode("stamped.go::Stamped").Meta["semantic_type"],
		"the skipped node keeps its prior stamp untouched")
}

// "full" (via the env override, winning over a configured "off") sweeps even
// a file with no unresolved demand — the pre-knob behaviour for a cold index
// that wants maximal hover coverage.
func TestLSP_Enrich_SweepFullEnvSweepsNoDemandFile(t *testing.T) {
	t.Setenv(SweepEnv, "full")

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "plain.go"),
		[]byte("package p\n\nfunc Plain() {}\n"), 0o644))

	server := newInstrumentedServer()
	mu, hovered := hoverURIRecorder(server)

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()
	p.sweepMode = "off" // env "full" must win over this configured mode

	g := graph.New()
	g.AddNode(&graph.Node{ID: "plain.go::Plain", Kind: graph.KindFunction, Name: "Plain",
		FilePath: "plain.go", StartLine: 3, EndLine: 3, Language: "go"})

	res := enrichCapturingResult(t, p, g, repoRoot)

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, hovered["plain.go"], "full mode (env wins over configured off) must sweep a no-demand file")
	assert.Positive(t, res.NodesEnriched)
}

// "off" skips the per-file sweep entirely — even a demand-bearing file is not
// hovered. The tier-deciding passes still run, but nothing is stamped here.
func TestLSP_Enrich_SweepOffSkipsEvenDemandFile(t *testing.T) {
	t.Setenv(SweepEnv, "off")

	repoRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "hot.go"),
		[]byte("package p\n\nfunc Hot() {}\nfunc caller() {}\n"), 0o644))

	server := newInstrumentedServer()
	var hoverCalls int
	var hmu sync.Mutex
	server.handle("textDocument/hover", func(_ json.RawMessage) (any, *jsonRPCError) {
		hmu.Lock()
		hoverCalls++
		hmu.Unlock()
		return map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func X()"}}, nil
	})

	p, cleanup := providerWithInstrumentedServer(t, server, []string{"go"}, 4)
	defer cleanup()

	g := graph.New()
	g.AddNode(&graph.Node{ID: "hot.go::Hot", Kind: graph.KindFunction, Name: "Hot",
		FilePath: "hot.go", StartLine: 3, EndLine: 3, Language: "go"})
	g.AddNode(&graph.Node{ID: "hot.go::caller", Kind: graph.KindFunction, Name: "caller",
		FilePath: "hot.go", StartLine: 4, EndLine: 4, Language: "go"})
	seedDemand(g, "hot.go::caller", "Hot")

	res := enrichCapturingResult(t, p, g, repoRoot)

	// Enrich returns only after every sweep goroutine finishes, so once we
	// are here no further hover requests can be issued: the count is final.
	hmu.Lock()
	assert.Zero(t, hoverCalls, "off mode must issue no hover requests, even for a demand-bearing file")
	hmu.Unlock()
	assert.Zero(t, res.NodesEnriched, "off mode stamps nothing")
	assert.False(t, server.wasOpened(pathToURI(filepath.Join(repoRoot, "hot.go"))),
		"off mode must not open the file for the sweep")
}
