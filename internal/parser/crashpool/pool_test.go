package crashpool

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

const testWorkerEnv = "GORTEX_CRASHPOOL_TESTWORKER"

// TestMain lets the test binary re-execute itself as a worker
// subprocess: a Pool spawned with Argv={os.Args[0]} and the env var set
// runs one of the helper workers below instead of the test suite.
func TestMain(m *testing.M) {
	switch os.Getenv(testWorkerEnv) {
	case "real":
		_ = RunWorker(os.Stdin, os.Stdout)
		os.Exit(0)
	case "crash":
		testCrashWorker()
		os.Exit(0)
	case "slow":
		testSlowWorker()
		os.Exit(0)
	case "stderr":
		testStderrWorker()
		os.Exit(0)
	default:
		os.Exit(m.Run())
	}
}

// mockExtractor is a minimal parser.Extractor for worker tests.
type mockExtractor struct {
	lang   string
	exts   []string
	nodes  int
	panics bool
}

func (m *mockExtractor) Language() string     { return m.lang }
func (m *mockExtractor) Extensions() []string { return m.exts }
func (m *mockExtractor) Extract(filePath string, _ []byte) (*parser.ExtractionResult, error) {
	if m.panics {
		panic("mock extractor boom")
	}
	nodes := make([]*graph.Node, m.nodes)
	for i := range nodes {
		nodes[i] = &graph.Node{
			ID:   fmt.Sprintf("%s::n%d", filePath, i),
			Kind: graph.KindFunction,
			Name: fmt.Sprintf("n%d", i),
		}
	}
	return &parser.ExtractionResult{Nodes: nodes}, nil
}

func mockRegistry() *parser.Registry {
	reg := parser.NewRegistry()
	reg.Register(&mockExtractor{lang: "mock", exts: []string{".mock"}, nodes: 2})
	reg.Register(&mockExtractor{lang: "mockpanic", exts: []string{".mp"}, panics: true})
	return reg
}

// testCrashWorker serves mock extractions but hard-exits on any file
// whose path contains "BOOM" — simulating a SIGSEGV mid-parse.
func testCrashWorker() {
	reg := mockRegistry()
	dec := gob.NewDecoder(os.Stdin)
	enc := gob.NewEncoder(os.Stdout)
	for {
		var req extractRequest
		if dec.Decode(&req) != nil {
			return
		}
		if strings.Contains(req.RelPath, "BOOM") {
			os.Exit(99)
		}
		resp := serveOne(reg, req)
		if enc.Encode(&resp) != nil {
			return
		}
	}
}

// testSlowWorker sleeps before every response — used to exercise the
// per-request timeout.
func testSlowWorker() {
	dec := gob.NewDecoder(os.Stdin)
	enc := gob.NewEncoder(os.Stdout)
	for {
		var req extractRequest
		if dec.Decode(&req) != nil {
			return
		}
		time.Sleep(3 * time.Second)
		if enc.Encode(&extractResponse{Seq: req.Seq}) != nil {
			return
		}
	}
}

// testStderrWorker writes a fixed burst of lines to stderr before
// serving normally — used to exercise crashpool's stderr-routing path
// (a real worker prints panic/runtime diagnostics to stderr on the way
// down; this simulates that without actually crashing).
func testStderrWorker() {
	for i := 1; i <= 2; i++ {
		fmt.Fprintf(os.Stderr, "worker stderr line %d\n", i)
	}
	reg := mockRegistry()
	dec := gob.NewDecoder(os.Stdin)
	enc := gob.NewEncoder(os.Stdout)
	for {
		var req extractRequest
		if dec.Decode(&req) != nil {
			return
		}
		resp := serveOne(reg, req)
		if enc.Encode(&resp) != nil {
			return
		}
	}
}

func newTestPool(t *testing.T, mode string, cfg Config) *Pool {
	t.Helper()
	cfg.Argv = []string{os.Args[0]}
	cfg.Env = []string{testWorkerEnv + "=" + mode}
	if cfg.Workers == 0 {
		cfg.Workers = 2
	}
	p, err := NewPool(cfg)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return p
}

func TestServeOne_Normal(t *testing.T) {
	resp := serveOne(mockRegistry(), extractRequest{Seq: 1, RelPath: "x.mock", Language: "mock"})
	require.Equal(t, uint64(1), resp.Seq)
	require.False(t, resp.Panicked)
	require.Empty(t, resp.Err)
	require.Len(t, resp.Nodes, 2)
}

func TestServeOne_PanicRecovered(t *testing.T) {
	resp := serveOne(mockRegistry(), extractRequest{Seq: 7, RelPath: "x.mp", Language: "mockpanic"})
	require.Equal(t, uint64(7), resp.Seq)
	require.True(t, resp.Panicked)
	require.Contains(t, resp.Err, "boom")
	require.Nil(t, resp.Nodes)
}

func TestServeOne_NoExtractor(t *testing.T) {
	resp := serveOne(mockRegistry(), extractRequest{Seq: 1, Language: "nonesuch"})
	require.Contains(t, resp.Err, "no extractor")
}

func TestPool_NormalExtraction(t *testing.T) {
	p := newTestPool(t, "real", Config{Workers: 2})
	res := p.Submit("main.go", "go", []byte("package main\n\nfunc Hello() {}\n"))
	require.False(t, res.Bad(), "unexpected failure: %s", res.Err)
	require.NotEmpty(t, res.Nodes)
}

// TestPool_CrashIsolation is the headline guarantee: a worker that dies
// mid-parse is detected, the file is reported crashed, and the pool
// respawns so every other file still indexes.
func TestPool_CrashIsolation(t *testing.T) {
	p := newTestPool(t, "crash", Config{Workers: 2})

	ok := p.Submit("a.mock", "mock", []byte("x"))
	require.False(t, ok.Bad())
	require.Len(t, ok.Nodes, 2)

	bad := p.Submit("dir/BOOM.mock", "mock", []byte("x"))
	require.True(t, bad.Crashed)
	require.False(t, bad.ParseChecked)
	require.True(t, bad.Bad())
	require.Contains(t, bad.Err, "crashed")

	// The pool respawned — every later file still indexes.
	for i := 0; i < 8; i++ {
		r := p.Submit(fmt.Sprintf("after%d.mock", i), "mock", []byte("x"))
		require.False(t, r.Bad(), "submit %d after crash failed: %s", i, r.Err)
		require.Len(t, r.Nodes, 2)
	}

	spawns, crashes := p.Stats()
	require.GreaterOrEqual(t, crashes, int64(1))
	require.GreaterOrEqual(t, spawns, int64(3)) // 2 initial + >=1 respawn
}

func TestPool_PanicSurvivesWorker(t *testing.T) {
	p := newTestPool(t, "crash", Config{Workers: 1})

	bad := p.Submit("p.mp", "mockpanic", []byte("x"))
	require.True(t, bad.Panicked)
	require.False(t, bad.Crashed) // recovered panic — worker stays alive

	// Same worker keeps serving.
	ok := p.Submit("ok.mock", "mock", []byte("x"))
	require.False(t, ok.Bad())
	require.Len(t, ok.Nodes, 2)

	_, crashes := p.Stats()
	require.Equal(t, int64(0), crashes)
}

func TestPool_RequestTimeout(t *testing.T) {
	p := newTestPool(t, "slow", Config{Workers: 1, RequestTimeout: 250 * time.Millisecond})
	res := p.Submit("slow.mock", "mock", []byte("x"))
	require.True(t, res.Crashed)
	require.False(t, res.ParseChecked)
	require.Contains(t, res.Err, "timed out")
}

func TestPool_ClosedRejects(t *testing.T) {
	p := newTestPool(t, "crash", Config{Workers: 1})
	p.Close()
	res := p.Submit("x.mock", "mock", []byte("x"))
	require.True(t, res.Bad())
	require.Contains(t, res.Err, "closed")
}

func TestNewPool_EmptyArgv(t *testing.T) {
	_, err := NewPool(Config{Workers: 1})
	require.Error(t, err)
}

// TestPool_RoutesWorkerStderrThroughLogger verifies a worker's stderr
// is no longer inherited raw (which, in the daemon, would mean writing
// straight into daemon.log) but routed through cfg.Logger as
// structured, tagged Warn entries.
func TestPool_RoutesWorkerStderrThroughLogger(t *testing.T) {
	core, obs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	p := newTestPool(t, "stderr", Config{Workers: 1, Logger: logger})

	res := p.Submit("main.mock", "mock", []byte("x"))
	require.False(t, res.Bad())

	deadline := time.Now().Add(3 * time.Second)
	for obs.Len() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	var lines []string
	for _, e := range obs.All() {
		require.Equal(t, "subprocess stderr", e.Message)
		require.Equal(t, "crashpool worker", e.ContextMap()["tag"])
		line, _ := e.ContextMap()["line"].(string)
		lines = append(lines, line)
	}
	require.Contains(t, lines, "worker stderr line 1")
	require.Contains(t, lines, "worker stderr line 2")
}

// Old workers and plugin extractors have no full-parse proof; the additive
// wire field must remain unknown instead of certifying their zero default.
func TestParseVerdictOldWorkerAndPluginRemainUnknown(t *testing.T) {
	var wire bytes.Buffer
	old := struct {
		Seq         uint64
		ParseErrors int
		HasParseErr bool
	}{Seq: 9}
	require.NoError(t, gob.NewEncoder(&wire).Encode(old))
	var response extractResponse
	require.NoError(t, gob.NewDecoder(&wire).Decode(&response))
	require.False(t, response.ParseChecked)
	require.Zero(t, response.ParseErrors)
	require.False(t, serveOne(mockRegistry(), extractRequest{Language: "mock", RelPath: "x.mock"}).ParseChecked)
	require.False(t, serveOne(mockRegistry(), extractRequest{Language: "mockpanic", RelPath: "x.mp"}).ParseChecked)
	require.False(t, serveOne(mockRegistry(), extractRequest{Language: "unknown"}).ParseChecked)
}

func TestPoolNativeSyntaxVerdictRoundTrip(t *testing.T) {
	p := newTestPool(t, "real", Config{Workers: 1})
	for _, source := range []string{"package p\nfunc Good() {}\n", "@@@ <<<<<\n"} {
		result := p.Submit("main.go", "go", []byte(source))
		require.False(t, result.Bad(), result.Err)
		require.True(t, result.ParseChecked)
		if source[0] == '@' {
			require.Positive(t, result.ParseErrors)
			require.True(t, result.HasParseErr)
		} else {
			require.Zero(t, result.ParseErrors)
			require.False(t, result.HasParseErr)
		}
	}
}
