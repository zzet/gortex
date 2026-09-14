package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/savings"
)

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		24 * time.Hour:      "24h",
		7 * 24 * time.Hour:  "7d",
		30 * 24 * time.Hour: "30d",
		2 * time.Hour:       "2h",
		90 * time.Minute:    "1h30m0s",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestBenchSourceAge_Bucketing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fresh file, user-provided: explicit timestamp marker.
	if got := benchSourceAge(path, false); !strings.Contains(got, "just generated") {
		t.Errorf("fresh user-provided source = %q, want '(just generated)'", got)
	}
	// Fresh file, auto-discovered: "seconds ago".
	if got := benchSourceAge(path, true); !strings.Contains(got, "seconds ago") {
		t.Errorf("fresh auto-discovered source = %q, want '(seconds ago)'", got)
	}

	// Backdate file → "min ago".
	old := time.Now().Add(-15 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if got := benchSourceAge(path, true); !strings.Contains(got, "min ago") {
		t.Errorf("15-min-old source = %q, want '(N min ago)'", got)
	}

	// Backdate to days.
	old = time.Now().Add(-3 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if got := benchSourceAge(path, true); !strings.Contains(got, "days ago") {
		t.Errorf("3-day-old source = %q, want '(N days ago)'", got)
	}

	// Missing file → empty string (no spurious age).
	if got := benchSourceAge(filepath.Join(dir, "missing.json"), false); got != "" {
		t.Errorf("missing file age = %q, want empty", got)
	}
}

func TestFindLatestBenchTokens_FindsMostRecent(t *testing.T) {
	dir := t.TempDir()
	// Build a minimal bench/results layout mirroring `bench all`.
	run1 := filepath.Join(dir, "run-20260101-000000")
	run2 := filepath.Join(dir, "run-20260518-000000")
	for _, r := range []string{run1, run2} {
		if err := os.MkdirAll(r, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r, "tokens.json"), []byte("[]"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Make run2 newer by explicit chtimes.
	now := time.Now()
	_ = os.Chtimes(filepath.Join(run1, "tokens.json"), now.Add(-72*time.Hour), now.Add(-72*time.Hour))
	_ = os.Chtimes(filepath.Join(run2, "tokens.json"), now.Add(-1*time.Hour), now.Add(-1*time.Hour))

	// chdir to the temp dir so the relative `bench/results` root the
	// function walks resolves under our fixture.
	tmpRoot := filepath.Join(dir, "workspace")
	resultsDir := filepath.Join(tmpRoot, "bench", "results")
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{run1, run2} {
		dst := filepath.Join(resultsDir, filepath.Base(r))
		if err := os.MkdirAll(dst, 0o755); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(r, "tokens.json")
		dstFile := filepath.Join(dst, "tokens.json")
		data, _ := os.ReadFile(src)
		_ = os.WriteFile(dstFile, data, 0o644)
		st, _ := os.Stat(src)
		_ = os.Chtimes(dstFile, st.ModTime(), st.ModTime())
	}
	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(tmpRoot); err != nil {
		t.Fatal(err)
	}

	got, ok := findLatestBenchTokens()
	if !ok {
		t.Fatal("expected to find a bench tokens artifact")
	}
	if !strings.Contains(got, filepath.Base(run2)) {
		t.Errorf("findLatestBenchTokens = %q, want a path under %s (newer)", got, filepath.Base(run2))
	}
}

func TestFindLatestBenchTokens_MissingDirReturnsFalse(t *testing.T) {
	wd, _ := os.Getwd()
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got, ok := findLatestBenchTokens(); ok {
		t.Errorf("expected no result in empty dir, got %q", got)
	}
}

func TestLoadHistory_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	h, err := loadHistory(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if h.Calls != 0 || h.Saved != 0 {
		t.Errorf("empty store should produce zero totals, got %+v", h)
	}
}

func TestLoadHistory_SinceZeroUsesCumulative(t *testing.T) {
	// Populate a ledger with a known total, then call loadHistory with
	// since=0. The result must come from the cumulative snapshot, not
	// an event scan.
	dir := t.TempDir()
	store, err := savings.Open(persistence.DefaultSidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddObservation(savings.Observation{Repo: "/r", Language: "go", Tool: "get_symbol_source", Returned: 50, Saved: 500})
	h, err := loadHistory(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.Calls != 1 || h.Saved != 500 {
		t.Errorf("since=0 should reflect cumulative ledger, got %+v", h)
	}
}

func TestLoadHistory_WindowFiltersEvents(t *testing.T) {
	dir := t.TempDir()
	store, err := savings.Open(persistence.DefaultSidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddObservation(savings.Observation{Repo: "/r", Language: "go", Tool: "read_file", Returned: 10, Saved: 90})

	h, err := loadHistory(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if h.Calls != 1 || h.Saved != 90 {
		t.Errorf("fresh event should fall inside a 24h window, got %+v", h)
	}
}

func TestGainHistory_JSON(t *testing.T) {
	h := &gainHistory{
		Path:     "/tmp/savings.json",
		Since:    24 * time.Hour,
		Calls:    42,
		Saved:    1000,
		Returned: 200,
		Costs:    map[string]float64{"claude-opus-4": 0.015},
	}
	out := h.toJSON()
	raw, _ := json.Marshal(out)
	for _, want := range []string{
		`"calls_counted":42`,
		`"tokens_saved":1000`,
		`"cost_avoided_usd":{`,
		`"claude-opus-4":0.015`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("history JSON missing %q\n%s", want, raw)
		}
	}
}

func TestRenderGainProjection_HeadlineMarker(t *testing.T) {
	var buf bytes.Buffer
	rows := []tokensMetric{
		{Case: "a", JSONTokens: 100, GCXTokens: 80},
	}
	renderGainProjection(&buf, rows, 1000, "claude-opus-4-8")
	out := buf.String()
	if !strings.Contains(out, "*claude-opus-4-8") {
		t.Errorf("headline model should be marked with *: %s", out)
	}
	if !strings.Contains(out, "*headlined: claude-opus-4-8") {
		t.Errorf("footer should name the headlined model: %s", out)
	}
}

func TestRenderGainProjection_NoHeadlineMarksNone(t *testing.T) {
	var buf bytes.Buffer
	rows := []tokensMetric{
		{Case: "a", JSONTokens: 100, GCXTokens: 80},
	}
	renderGainProjection(&buf, rows, 1000, "")
	out := buf.String()
	if strings.Contains(out, "*headlined") {
		t.Errorf("no headline → no footer; got: %s", out)
	}
	if strings.Contains(out, "*claude-opus-4") {
		t.Errorf("no headline → no row marker; got: %s", out)
	}
}

func TestGainCmd_Registered(t *testing.T) {
	subs := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		subs[c.Name()] = true
	}
	if !subs["gain"] {
		t.Errorf("rootCmd missing `gain`; have %v", subs)
	}
}

// --- F4b: the gain reader must see the live window ------------------------
//
// `gortex gain` opens a SECOND savings.Store on the ledger a writer in the
// same process is already buffering into. The two handles share one sidecar
// connection (persistence.OpenSidecar caches by path) but own separate
// buffers, so a reader that flushed only its own buffer reported a ledger
// that was still in the writer's memory — and its deferred Close then took
// the shared connection away, turning the writer's next flush into
// "database is closed" and DROPPING the window rather than delaying it.

// A whole window of buffered observations must be visible to loadHistory,
// not just the one the narrower regression tests book.
func TestLoadHistory_SeesEveryBufferedObservation(t *testing.T) {
	const observations = 12
	dir := t.TempDir()
	writer, err := savings.Open(persistence.DefaultSidecarPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	for i := range observations {
		writer.AddObservation(savings.Observation{
			Repo: "/r", Language: "go", Tool: "search_symbols",
			Returned: 10, Saved: int64(100 + i),
		})
	}
	if got := writer.Pending(); got != observations {
		t.Fatalf("precondition: want %d observations still buffered, got %d", observations, got)
	}

	var wantSaved int64
	for i := range observations {
		wantSaved += int64(100 + i)
	}

	h, err := loadHistory(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if h.Calls != observations || h.Saved != wantSaved {
		t.Errorf("cumulative read must drain the writer's buffer: want Calls=%d Saved=%d, got %+v",
			observations, wantSaved, h)
	}

	// The window reader (event scan) must agree with the cumulative one.
	hw, err := loadHistory(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if hw.Calls != observations || hw.Saved != wantSaved {
		t.Errorf("windowed read must see the same events: want Calls=%d Saved=%d, got %+v",
			observations, wantSaved, hw)
	}
}

// loadHistory's deferred Close must not strand the writer's buffer. The
// observation booked between the two reads has to survive a reader that
// releases the shared sidecar handle.
func TestLoadHistory_CloseDoesNotDropTheWritersWindow(t *testing.T) {
	dir := t.TempDir()
	path := persistence.DefaultSidecarPath(dir)
	writer, err := savings.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	writer.AddObservation(savings.Observation{Repo: "/r", Language: "go", Tool: "read_file", Saved: 7})
	if _, err := loadHistory(dir, 0); err != nil { // opens + closes its own handle
		t.Fatal(err)
	}

	// Re-read through a fresh handle: the first observation must be on disk
	// and nothing may be counted as dropped.
	reader, err := savings.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	snap, err := reader.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Totals.CallsCounted != 1 || snap.Totals.TokensSaved != 7 {
		t.Errorf("the writer's window must be committed, not dropped: got %+v", snap.Totals)
	}
	if snap.DroppedObservations != 0 {
		t.Errorf("no observation may be dropped, got %d", snap.DroppedObservations)
	}
}

// --- F4b: the one-shot stdio server flushes on every exit path ------------

// installOneshotSavingsFlush is the seam; runMCP is the production entry
// point that has to reach it. Parsing the source is the only way to assert
// the wiring without standing up a full stdio server: the call must be
// DEFERRED (so it runs on the errCh return, the SIGINT/SIGTERM return and
// every error return in between) and it must be registered after the stack's
// own `defer ss.Close()`, so LIFO commits the ledger before teardown.
func TestRunMCPDefersTheOneshotSavingsFlush(t *testing.T) {
	// Resolve mcp.go from THIS file's own location rather than the working
	// directory: other tests in this package chdir, and the assertion must
	// not depend on who ran last.
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(filepath.Dir(self), "mcp.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "runMCP" && fd.Recv == nil {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("runMCP not found in mcp.go")
	}

	closeAt, flushAt := -1, -1
	ast.Inspect(fn, func(n ast.Node) bool {
		def, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		pos := fset.Position(def.Pos()).Line
		switch inner := def.Call.Fun.(type) {
		case *ast.SelectorExpr: // defer ss.Close()
			if id, ok := inner.X.(*ast.Ident); ok && id.Name == "ss" && inner.Sel.Name == "Close" {
				closeAt = pos
			}
		case *ast.CallExpr: // defer installOneshotSavingsFlush(srv)()
			if id, ok := inner.Fun.(*ast.Ident); ok && id.Name == "installOneshotSavingsFlush" {
				flushAt = pos
			}
		}
		return true
	})
	if flushAt < 0 {
		t.Fatal("runMCP must defer installOneshotSavingsFlush(srv)() so every exit path commits the savings window")
	}
	if closeAt < 0 {
		t.Fatal("runMCP no longer defers ss.Close(); the ordering assertion below is meaningless")
	}
	if flushAt < closeAt {
		t.Errorf("the savings flush is deferred at line %d, before `defer ss.Close()` at line %d: "+
			"LIFO would then run it after the stack teardown", flushAt, closeAt)
	}
}

// The helper must tighten the window to the one-shot bound and hand back a
// closure that actually commits. Exercised against a real *gortexmcp.Server
// with a sidecar-backed ledger: a host that SIGKILLs its stdio server loses
// only what is still inside that bound.
func TestInstallOneshotSavingsFlush_BoundsAndCommits(t *testing.T) {
	srv := gortexmcp.NewServer(nil, nil, nil, nil, zap.NewNop(), nil)
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	store, err := savings.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv.InitSavings(store, "")

	if every, max := store.FlushBounds(); every != savings.DefaultFlushInterval || max != savings.DefaultFlushMax {
		t.Fatalf("precondition: a fresh store starts on the daemon default, got %v/%d", every, max)
	}

	flush := installOneshotSavingsFlush(srv)

	every, max := store.FlushBounds()
	if every != savings.OneshotFlushInterval || max != savings.OneshotFlushMax {
		t.Errorf("the one-shot entry point must tighten the window to %v/%d, got %v/%d",
			savings.OneshotFlushInterval, savings.OneshotFlushMax, every, max)
	}
	if every >= savings.DefaultFlushInterval {
		t.Errorf("the one-shot window (%v) must be shorter than the daemon default (%v)",
			every, savings.DefaultFlushInterval)
	}

	store.AddObservation(savings.Observation{Repo: "/r", Language: "go", Tool: "search_symbols", Saved: 42})
	if store.Pending() != 1 {
		t.Fatalf("precondition: the observation must still be buffered, got %d pending", store.Pending())
	}
	flush()
	if store.Pending() != 0 {
		t.Errorf("the deferred flush must drain the buffer, %d still pending", store.Pending())
	}
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Totals.CallsCounted != 1 || snap.Totals.TokensSaved != 42 {
		t.Errorf("the deferred flush must commit the window, got %+v", snap.Totals)
	}

	// A server with no ledger wired must still be deferrable blind.
	installOneshotSavingsFlush(gortexmcp.NewServer(nil, nil, nil, nil, zap.NewNop(), nil))()
	installOneshotSavingsFlush(nil)()
}
