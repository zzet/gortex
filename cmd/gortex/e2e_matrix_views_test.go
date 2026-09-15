package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// End-to-end matrix 4: the view lifecycle, over BOTH front doors.
//
// This file and e2e_matrix_advance_test.go are one item. They share the opt-in
// gate, the outcome table, the JSON-rider readers, the private HTTP client and
// the read-only store census declared here; matrix 5 (committed advancement
// with ten dependents) lives in the sibling file because it is a different
// workload, not a different harness.
//
// Isolation is the shared fixture's (issue767_fixture_shared_test.go): a child
// daemon on a supplied binary, private XDG dirs, a private store, a private Git
// fixture and a private gitconfig under a short /tmp root. Nothing here can
// reach the user's daemon, store or configuration, and nothing here starts a
// daemon the fixture does not also stop.
//
// Every case names the acceptance gate it serves:
//
//	gate 1  snapshot correctness — a composed view matches the snapshot it claims
//	gate 4  same-branch reuse — compatible switches reuse rather than rebuild
//	gate 5  advancing main — dependents stay coherent while their base moves
//	gate 7  lifetime and cleanup — discovery, mode change, removal
//	gate 10 reproducible evidence — the run records what it did and did not do
//
// The matrix is OPT-IN: it needs a daemon binary, a real filesystem, minutes of
// wall clock and a free loopback port, so it never runs as part of an ordinary
// `go test ./cmd/gortex`. The skip reason names the ledger row.

const (
	// e2eViewsBinaryEnv supplies the candidate daemon binary both matrices run
	// against. One binary serves both files: the matrices measure one
	// candidate, not two.
	e2eViewsBinaryEnv = "GX_E2E_MATRIX_BINARY"
	// e2eViewsSkipReason is the named skip reason. A case that is not exercised
	// says so by name — never a silent pass.
	e2eViewsSkipReason = "set " + e2eViewsBinaryEnv + " to a candidate daemon binary to opt into the isolated view-lifecycle / advancement matrix (ledger row: E2E matrix 4+5)"
)

// ------------------------------------------------------------ opt-in gate ---

// e2eViewsBinary resolves the candidate binary, or skips with the named reason.
func e2eViewsBinary(t *testing.T) string {
	t.Helper()
	binary := strings.TrimSpace(os.Getenv(e2eViewsBinaryEnv))
	if binary == "" {
		t.Skip(e2eViewsSkipReason)
	}
	absolute, err := filepath.Abs(binary)
	if err != nil {
		t.Fatalf("%s=%q: %v", e2eViewsBinaryEnv, binary, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		t.Fatalf("%s=%q: %v", e2eViewsBinaryEnv, absolute, err)
	}
	if info.IsDir() || info.Mode()&0111 == 0 {
		t.Fatalf("%s=%q is not an executable file", e2eViewsBinaryEnv, absolute)
	}
	return absolute
}

// e2eViewsBudget refuses to start a matrix that cannot finish. A run that exhausts
// the test deadline half way through reports a timeout rather than an outcome,
// and an outcome table missing half its rows is worse than no run at all.
func e2eViewsBudget(t *testing.T, need time.Duration) {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return
	}
	if remaining := time.Until(deadline); remaining < need {
		t.Fatalf("this matrix needs at least %s of test deadline (have %s); run it with -timeout %s or longer",
			need, remaining.Truncate(time.Second), (need + 10*time.Minute).Round(time.Minute))
	}
}

// ---------------------------------------------------------- outcome table ---

// The outcome classes. A case is exercised and passes, is exercised and the
// observation is recorded without a verdict, or is not reachable here and says
// why. None of the three is ever rendered as another.
const (
	e2eViewsPass        = "PASS"
	e2eViewsUnsupported = "UNSUPPORTED"
	e2eViewsObserved    = "OBSERVED"
)

type e2eViewsRow struct {
	Case    string `json:"case"`
	Gate    string `json:"gate"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

// e2eViewsTable is the run's outcome table: one row per matrix case, each naming
// the gate it serves. It is rendered into the test log and written beside the
// fixture artifacts so the Suite stage can lift it into the ledger verbatim.
type e2eViewsTable struct {
	t    *testing.T
	name string
	rows []e2eViewsRow
}

func e2eViewsNewTable(t *testing.T, name string) *e2eViewsTable {
	table := &e2eViewsTable{t: t, name: name}
	t.Cleanup(table.render)
	return table
}

func (tb *e2eViewsTable) record(kase, gate, outcome, detail string, args ...any) {
	if len(args) > 0 {
		detail = fmt.Sprintf(detail, args...)
	}
	tb.rows = append(tb.rows, e2eViewsRow{Case: kase, Gate: gate, Outcome: outcome, Detail: detail})
	tb.t.Logf("[%s] %-5s %-11s %-7s %s", tb.name, kase, gate, outcome, detail)
}

func (tb *e2eViewsTable) pass(kase, gate, detail string, args ...any) {
	tb.record(kase, gate, e2eViewsPass, detail, args...)
}

func (tb *e2eViewsTable) observed(kase, gate, detail string, args ...any) {
	tb.record(kase, gate, e2eViewsObserved, detail, args...)
}

func (tb *e2eViewsTable) unsupported(kase, gate, detail string, args ...any) {
	tb.record(kase, gate, e2eViewsUnsupported, detail, args...)
}

// render prints the table. It runs from t.Cleanup, so a matrix that fails half
// way still reports every case it reached — and, by their absence, the ones it
// never attempted.
func (tb *e2eViewsTable) render() {
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s outcome table (%d cases)\n", tb.name, len(tb.rows))
	fmt.Fprintf(&b, "| case | gate | outcome | detail |\n|---|---|---|---|\n")
	for _, row := range tb.rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", row.Case, row.Gate, row.Outcome, strings.ReplaceAll(row.Detail, "|", "\\|"))
	}
	tb.t.Log(b.String())
}

// writeTo persists the table beside the fixture's artifacts.
func (tb *e2eViewsTable) writeTo(dir string) {
	raw, err := json.MarshalIndent(tb.rows, "", "  ")
	if err != nil {
		tb.t.Logf("outcome table not encoded: %v", err)
		return
	}
	path := filepath.Join(dir, tb.name+"-outcomes.json")
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		tb.t.Logf("outcome table not written: %v", err)
		return
	}
	tb.t.Logf("%s outcome table: %s", tb.name, path)
}

// ------------------------------------------------------------ JSON riders ---

// e2eViewsFreshness digs the freshness rider out of a response.
//
// The rider is attached under the "freshness" key — internal/mcp/view_request.go:2253
// merges it into the result _meta and :2272 into a JSON object payload — and
// the CLI's --format json wraps a tool payload that is itself a JSON string, so
// the search walks maps, arrays and JSON-in-string alike. Map keys are visited
// in sorted order so two calls over the same response return the same rider.
// nil means the response carried no rider at all, which is itself a finding.
func e2eViewsFreshness(value any) map[string]any {
	switch value := value.(type) {
	case map[string]any:
		if rider, ok := value["freshness"].(map[string]any); ok {
			return rider
		}
		for _, key := range e2eViewsSortedKeys(value) {
			if rider := e2eViewsFreshness(value[key]); rider != nil {
				return rider
			}
		}
	case []any:
		for _, child := range value {
			if rider := e2eViewsFreshness(child); rider != nil {
				return rider
			}
		}
	case string:
		if child, ok := e2eViewsEmbeddedJSON(value); ok {
			return e2eViewsFreshness(child)
		}
	}
	return nil
}

func e2eViewsSortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// e2eViewsEmbeddedJSON decodes a string that is itself a JSON document. Tool
// payloads ride inside content[].text, so without this the rider and the
// evidence would be invisible to every reader here.
func e2eViewsEmbeddedJSON(value string) (any, bool) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return nil, false
	}
	var child any
	if json.Unmarshal([]byte(trimmed), &child) != nil {
		return nil, false
	}
	return child, true
}

// e2eViewsIdentity is the part of a rider that says WHICH view answered. It is
// the comparison the two front doors have to agree on: a door that resolves a
// different checkout, a different graph or a different freshness for the same
// request is a front door outside the rule, whatever else its answer contains.
//
// Only identity fields are lifted. Timing (waited_ms) and capability
// annotations legitimately differ between two calls made seconds apart, and
// comparing those would make the parity assertion flaky rather than strict.
func e2eViewsIdentity(rider map[string]any) map[string]any {
	identity := map[string]any{}
	for _, field := range []string{"requested_view", "actual_view", "exact", "graph_id", "checkout_id", "view_fingerprint", "fallback_reason"} {
		if value, ok := rider[field]; ok {
			identity[field] = value
		}
	}
	return identity
}

// e2eViewsSameIdentity returns the fields two rider identities disagree on, so a
// failure names the divergence instead of dumping two maps.
func e2eViewsSameIdentity(left, right map[string]any) []string {
	seen := map[string]bool{}
	var diverged []string
	for _, side := range []map[string]any{left, right} {
		for field := range side {
			if seen[field] {
				continue
			}
			seen[field] = true
			if fmt.Sprint(left[field]) != fmt.Sprint(right[field]) {
				diverged = append(diverged, fmt.Sprintf("%s: %v != %v", field, left[field], right[field]))
			}
		}
	}
	sort.Strings(diverged)
	return diverged
}

func e2eViewsTail(raw []byte) string {
	if len(raw) > 3000 {
		return "…" + string(raw[len(raw)-3000:])
	}
	return string(raw)
}

func e2eViewsJSONTail(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return e2eViewsTail(raw)
}

// e2eViewsViewSelector renders one view selector for a request body. Empty fields
// are dropped: graphview.ParseSelectorWithPath refuses a field the kind does
// not use (internal/graphview/selector.go:69-114), so an empty string is a
// rejected request rather than an omitted field.
func e2eViewsViewSelector(kind string, fields map[string]string) map[string]any {
	view := map[string]any{"kind": kind}
	for key, value := range fields {
		if value != "" {
			view[key] = value
		}
	}
	return view
}

// e2eViewsSearchArgs is the search request both doors send, so a parity failure
// can never be an argument difference.
func e2eViewsSearchArgs(name string, view map[string]any) map[string]any {
	args := map[string]any{
		"operation": "symbols",
		"query":     name,
		"options":   map[string]any{"limit": 10, "query_class": "symbol", "expand": "off"},
	}
	if view != nil {
		args["view"] = view
	}
	return args
}

// --------------------------------------------------------- the CLI door -----

// e2eViewsCLI calls one tool over the unix-socket front door and returns the
// decoded response. Transport failures are returned, never retried: a matrix
// that silently re-asks has measured retries, not behaviour.
func e2eViewsCLI(t *testing.T, f *issue767Fixture, dir, tool string, args map[string]any, timeout time.Duration) (any, []byte, error) {
	t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.tryCommand(timeout, dir, "call", tool, "--index", dir, "--json", string(body), "--format", "json")
	if err != nil {
		return nil, raw, fmt.Errorf("cli %s: %w: %s", tool, err, e2eViewsTail(raw))
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, raw, fmt.Errorf("cli %s response is not JSON: %w: %s", tool, err, e2eViewsTail(raw))
	}
	return value, raw, nil
}

// ------------------------------------------------------- symbol assertions ---

// e2eViewsPresent asserts that one name answers out of one file, through one
// spelling. It uses askSymbol rather than the verdict helper because a matrix
// case needs to say WHICH part of the evidence failed.
func e2eViewsPresent(t *testing.T, f *issue767Fixture, kase, root, name, file string, spelling issue767Spelling) issue767Answer {
	t.Helper()
	answer, err := f.askSymbol(root, name, file, spelling)
	if err != nil {
		t.Fatalf("case %s: search %s in %s via %s: %v", kase, name, root, spelling, err)
	}
	if !answer.Found || !answer.FromExpectedFile {
		t.Fatalf("case %s: %s did not answer out of %s via %s: %+v", kase, name, file, spelling, answer)
	}
	if answer.Fallback {
		t.Fatalf("case %s: %s answered from a fallback view via %s: %+v", kase, name, spelling, answer)
	}
	if spelling == issue767AsAutomaticWorktree && !answer.Exact {
		t.Fatalf("case %s: %s answered without the exact freshness label: %+v", kase, name, answer)
	}
	return answer
}

// e2eViewsAbsentEverywhere asserts that a name answers from nowhere in the view a
// root addresses — the leak probe. It is deliberately stronger than "not from
// this file": a symbol that only exists in a sibling checkout must not surface
// at all, whichever file the answer claims.
func e2eViewsAbsentEverywhere(t *testing.T, f *issue767Fixture, kase, root, name, file string, spelling issue767Spelling) {
	t.Helper()
	answer, err := f.askSymbol(root, name, file, spelling)
	if err != nil {
		t.Fatalf("case %s: leak probe for %s in %s: %v", kase, name, root, err)
	}
	if answer.Found {
		t.Fatalf("case %s: %s leaked into the view addressed by %s: %+v", kase, name, root, answer)
	}
}

// e2eViewsAbsentFromFile asserts that a name no longer answers out of one
// particular file. It is the weaker probe, for the case where a same-named
// symbol legitimately survives elsewhere in the composed stack.
func e2eViewsAbsentFromFile(t *testing.T, f *issue767Fixture, kase, root, name, file string, spelling issue767Spelling) {
	t.Helper()
	answer, err := f.askSymbol(root, name, file, spelling)
	if err != nil {
		t.Fatalf("case %s: stale probe for %s in %s: %v", kase, name, file, err)
	}
	if answer.FromExpectedFile {
		t.Fatalf("case %s: %s still answers out of %s: %+v", kase, name, file, answer)
	}
}

// -------------------------------------------------------- the HTTP door -----

// e2eViewsFreeAddr reserves a loopback address by binding :0 and letting go. The
// window between release and the daemon's own bind is unavoidable for a child
// that takes its address on the command line; a collision surfaces as a daemon
// that fails to start, which the fixture reports with its log path.
func e2eViewsFreeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved port %s: %v", addr, err)
	}
	return addr
}

// e2eViewsHTTPWrapper returns a launcher that is the candidate binary for every
// subcommand except `daemon start`, which additionally gets --http-addr.
//
// The fixture's start() owns the daemon argv, and --http-addr has no
// environment fallback: cmd/gortex/daemon.go:151 binds it to the flag alone and
// :418 gates the whole HTTP surface (/mcp + /v1) on it being non-empty. Rather
// than fork the shared fixture — which every other matrix also drives — this
// item supplies a launcher that injects the flag. The wrapper execs, so the
// child keeps one pid: the fixture's SIGINT, its WaitDelay kill and its
// process-counter sampling all address the daemon itself.
func e2eViewsHTTPWrapper(t *testing.T, dir, binary, addr string) string {
	t.Helper()
	path := filepath.Join(dir, "gortex-http-launcher")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"daemon\" ] && [ \"$2\" = \"start\" ]; then\n" +
		"  shift 2\n" +
		"  exec " + e2eViewsShellQuote(binary) + " daemon start --http-addr " + e2eViewsShellQuote(addr) + " \"$@\"\n" +
		"fi\n" +
		"exec " + e2eViewsShellQuote(binary) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatalf("write http launcher: %v", err)
	}
	return path
}

func e2eViewsShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// e2eViewsHTTP is one MCP Streamable HTTP session against the private daemon.
//
// It is a session, not a connection: the Mcp-Session-Id minted by initialize
// rides every later request, which is what makes the buffer-overlay case
// expressible at all (an overlay belongs to the calling MCP session —
// internal/mcp/tools_overlay.go:38-65 — and the CLI mints a fresh session per
// invocation). X-Gortex-Cwd is the header the transport reads a caller's cwd
// from (internal/mcp/streamable/transport.go:491,548,971), so binding a session
// to a checkout is one header, exactly as an editor does it.
type e2eViewsHTTP struct {
	t       *testing.T
	base    string
	client  *http.Client
	session string
	cwd     string
}

func e2eViewsNewHTTP(t *testing.T, addr, cwd string) *e2eViewsHTTP {
	return &e2eViewsHTTP{
		t:      t,
		base:   "http://" + addr,
		client: &http.Client{Timeout: 90 * time.Second},
		cwd:    cwd,
	}
}

func (h *e2eViewsHTTP) post(ctx context.Context, body []byte) (map[string]any, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+"/mcp", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if h.session != "" {
		request.Header.Set("Mcp-Session-Id", h.session)
	}
	if h.cwd != "" {
		request.Header.Set("X-Gortex-Cwd", h.cwd)
	}
	response, err := h.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, response.Header, err
	}
	if response.StatusCode == http.StatusAccepted {
		return nil, response.Header, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, response.Header, fmt.Errorf("POST /mcp: %s: %s", response.Status, e2eViewsTail(raw))
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, response.Header, fmt.Errorf("POST /mcp response is not JSON: %w: %s", err, e2eViewsTail(raw))
	}
	return envelope, response.Header, nil
}

func e2eViewsFrame(id any, method string, params any) []byte {
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		frame["id"] = id
	}
	if params != nil {
		frame["params"] = params
	}
	raw, _ := json.Marshal(frame)
	return raw
}

// initialize opens the session and completes the handshake, retrying only the
// connection: the daemon's unix socket answers before its TCP listener exists,
// and a refused connection is not an answer.
func (h *e2eViewsHTTP) initialize(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	body := e2eViewsFrame(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "e2e-matrix", "version": "1"},
	})
	var last error
	for time.Now().Before(deadline) {
		envelope, header, err := h.post(ctx, body)
		if err == nil && envelope != nil {
			if id := header.Get("Mcp-Session-Id"); id != "" {
				h.session = id
			}
			if _, _, err := h.post(ctx, e2eViewsFrame(nil, "notifications/initialized", nil)); err != nil {
				return err
			}
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("http front door never answered initialize: %w", last)
}

// call sends one request and returns its `result`, or the JSON-RPC error.
func (h *e2eViewsHTTP) call(ctx context.Context, method string, params any) (any, error) {
	envelope, _, err := h.post(ctx, e2eViewsFrame(2, method, params))
	if err != nil {
		return nil, err
	}
	if envelope == nil {
		return nil, fmt.Errorf("%s: no response body", method)
	}
	if rpcErr, ok := envelope["error"]; ok {
		return nil, fmt.Errorf("%s: json-rpc error: %v", method, rpcErr)
	}
	return envelope["result"], nil
}

func (h *e2eViewsHTTP) tool(ctx context.Context, name string, args map[string]any) (any, error) {
	return h.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
}

func (h *e2eViewsHTTP) resource(ctx context.Context, uri string) (any, error) {
	return h.call(ctx, "resources/read", map[string]any{"uri": uri})
}

// ------------------------------------------------------------ store census ---

// e2eViewsRoute is one checkout's routed pair, read read-only from the private
// store. The pair is the physical evidence a rebuild happened: generation ids
// come from an AUTOINCREMENT sequence (store_sqlite/schema.go:671-672), so a
// route whose pair is unchanged is a route nothing rebuilt.
type e2eViewsRoute struct {
	CheckoutID string
	RootPath   string
	State      string
	HeadTree   string
	Mode       string
	CommitGen  int64
	DirtyGen   int64
}

func (r e2eViewsRoute) pair() string { return fmt.Sprintf("(%d,%d)", r.CommitGen, r.DirtyGen) }

// e2eViewsRoutes reads every checkout and its route, keyed by cleaned root path.
func e2eViewsRoutes(t *testing.T, ctx context.Context, db *sql.DB) map[string]e2eViewsRoute {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT c.checkout_id, c.root_path, c.state, c.head_tree, c.effective_mode,
		       COALESCE(r.commit_generation_id, 0), COALESCE(r.dirty_generation_id, 0)
		  FROM checkouts c LEFT JOIN checkout_routes r ON r.checkout_id = c.checkout_id`)
	if err != nil {
		t.Fatalf("read checkout routes: %v", err)
	}
	defer rows.Close()
	out := map[string]e2eViewsRoute{}
	for rows.Next() {
		var route e2eViewsRoute
		if err := rows.Scan(&route.CheckoutID, &route.RootPath, &route.State, &route.HeadTree, &route.Mode, &route.CommitGen, &route.DirtyGen); err != nil {
			t.Fatalf("scan checkout route: %v", err)
		}
		out[filepath.Clean(route.RootPath)] = route
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate checkout routes: %v", err)
	}
	return out
}

// e2eViewsGeneration is one payload generation row.
type e2eViewsGeneration struct {
	ID         int64
	OwnerKind  string
	Kind       string
	CheckoutID string
	BaseID     int64
	TreeOID    string
	State      string
}

func e2eViewsGenerations(t *testing.T, ctx context.Context, db *sql.DB) map[int64]e2eViewsGeneration {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
		SELECT generation_id, owner_kind, generation_kind, COALESCE(checkout_id,''),
		       COALESCE(base_generation_id,0), tree_oid, state
		  FROM view_generations`)
	if err != nil {
		t.Fatalf("read view generations: %v", err)
	}
	defer rows.Close()
	out := map[int64]e2eViewsGeneration{}
	for rows.Next() {
		var g e2eViewsGeneration
		if err := rows.Scan(&g.ID, &g.OwnerKind, &g.Kind, &g.CheckoutID, &g.BaseID, &g.TreeOID, &g.State); err != nil {
			t.Fatalf("scan view generation: %v", err)
		}
		out[g.ID] = g
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate view generations: %v", err)
	}
	return out
}

// ------------------------------------------------------------- matrix 4 -----

// TestE2EMatrix4ViewLifecycle is E2E matrix 4.
//
// One private daemon, one fixture repository, both front doors. The cases run
// in order against the same daemon because the lifecycle IS the ordering: a
// discovered checkout becomes dedicated, is demoted again, switches branches,
// is read through an inactive ref and is finally removed. Asserting each in its
// own daemon would test eleven cold starts instead of one lifecycle.
func TestE2EMatrix4ViewLifecycle(t *testing.T) {
	binary := e2eViewsBinary(t)
	e2eViewsBudget(t, 20*time.Minute)
	table := e2eViewsNewTable(t, "matrix4")

	addr := e2eViewsFreeAddr(t)
	f := newIssue767Fixture(t, binary)
	// The launcher lives beside the fixture artifacts and is removed with them.
	f.binary = e2eViewsHTTPWrapper(t, f.root, binary, addr)
	t.Cleanup(func() { table.writeTo(f.root) })
	f.start()
	f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
	t.Logf("matrix4 candidate %s, http front door %s, artifacts %s", binary, addr, f.root)

	ctx := t.Context()
	db := f.openReadOnly()
	primaryMarker := filepath.Join(f.primary, "marker.go")

	// --- case 4.1 (gate 7): a new linked checkout is DISCOVERED, not tracked.
	alpha := filepath.Join(f.root, "m4-alpha")
	alphaMarker := filepath.Join(alpha, "marker.go")
	started := time.Now()
	f.git(f.primary, "worktree", "add", "-b", "m4-alpha", alpha)
	f.awaitSymbolAs(alpha, "Issue767PrimaryMarker", alphaMarker, 3*time.Minute, issue767AsAutomaticWorktree)
	table.pass("4.1", "gate 7", "implicit worktree discovered and served exactly in %s, with no track call and no config edit",
		time.Since(started).Truncate(time.Millisecond))

	// --- case 4.2 (gate 1): search → edit → fresh read, and no leak either way.
	f.write(alphaMarker, issue767MarkerSource("GxViewsAlphaEdited"))
	f.awaitSymbolAs(alpha, "GxViewsAlphaEdited", alphaMarker, 3*time.Minute, issue767AsAutomaticWorktree)
	e2eViewsPresent(t, f, "4.2", alpha, "GxViewsAlphaEdited", alphaMarker, issue767AsAutomaticWorktree)
	e2eViewsAbsentFromFile(t, f, "4.2", alpha, "Issue767PrimaryMarker", alphaMarker, issue767AsAutomaticWorktree)
	e2eViewsAbsentEverywhere(t, f, "4.2", f.primary, "GxViewsAlphaEdited", primaryMarker, issue767AsPrimary)
	e2eViewsPresent(t, f, "4.2", f.primary, "Issue767PrimaryMarker", primaryMarker, issue767AsPrimary)
	table.pass("4.2", "gate 1", "edit → fresh read exact from the checkout's own file; the replaced declaration stops answering there; nothing leaks into the primary")

	// --- case 4.3 (gate 1): sibling checkouts do not see each other.
	beta := filepath.Join(f.root, "m4-beta")
	betaMarker := filepath.Join(beta, "marker.go")
	f.git(f.primary, "worktree", "add", "-b", "m4-beta", beta)
	f.write(betaMarker, issue767MarkerSource("GxViewsBetaOnly"))
	f.awaitSymbolAs(beta, "GxViewsBetaOnly", betaMarker, 3*time.Minute, issue767AsAutomaticWorktree)
	e2eViewsAbsentEverywhere(t, f, "4.3", alpha, "GxViewsBetaOnly", alphaMarker, issue767AsAutomaticWorktree)
	e2eViewsAbsentEverywhere(t, f, "4.3", beta, "GxViewsAlphaEdited", betaMarker, issue767AsAutomaticWorktree)
	table.pass("4.3", "gate 1", "two sibling automatic checkouts each answer their own dirty marker and neither answers the other's")

	// --- case 4.4 (gates 1+7): both front doors resolve the SAME view.
	//
	// Two spellings, each asked on both doors: an explicit worktree selector,
	// and a bare cwd binding. The CLI sends the selector in the request body
	// and its cwd as the process cwd; the HTTP door sends the same body and its
	// cwd as X-Gortex-Cwd. A door that resolves a different checkout, graph or
	// freshness for the same question is a front door outside the rule.
	httpSession := e2eViewsNewHTTP(t, addr, alpha)
	if err := httpSession.initialize(ctx, 2*time.Minute); err != nil {
		t.Fatalf("case 4.4: %v", err)
	}
	selector := e2eViewsViewSelector("worktree", map[string]string{"path": alpha})
	var familyGraphID string
	for _, spelling := range []struct {
		name string
		view map[string]any
	}{
		{"explicit worktree selector", selector},
		{"cwd binding", nil},
	} {
		cliValue, cliRaw, err := e2eViewsCLI(t, f, alpha, "search", e2eViewsSearchArgs("GxViewsAlphaEdited", spelling.view), 60*time.Second)
		if err != nil {
			t.Fatalf("case 4.4 (%s): cli door: %v", spelling.name, err)
		}
		cliRider := e2eViewsFreshness(cliValue)
		if cliRider == nil {
			t.Fatalf("case 4.4 (%s): the cli answer carries no freshness rider: %s", spelling.name, e2eViewsTail(cliRaw))
		}
		httpValue, err := httpSession.tool(ctx, "search", e2eViewsSearchArgs("GxViewsAlphaEdited", spelling.view))
		if err != nil {
			t.Fatalf("case 4.4 (%s): http door: %v", spelling.name, err)
		}
		httpRider := e2eViewsFreshness(httpValue)
		if httpRider == nil {
			t.Fatalf("case 4.4 (%s): the http answer carries no freshness rider: %s", spelling.name, e2eViewsJSONTail(httpValue))
		}
		cliIdentity, httpIdentity := e2eViewsIdentity(cliRider), e2eViewsIdentity(httpRider)
		if diverged := e2eViewsSameIdentity(cliIdentity, httpIdentity); len(diverged) > 0 {
			t.Fatalf("case 4.4 (%s): the two front doors resolved different views: %s\ncli=%v\nhttp=%v",
				spelling.name, strings.Join(diverged, "; "), cliIdentity, httpIdentity)
		}
		if !issue767JSONSource(cliValue, "GxViewsAlphaEdited", alphaMarker) {
			t.Fatalf("case 4.4 (%s): the cli door answered from somewhere other than %s: %s", spelling.name, alphaMarker, e2eViewsTail(cliRaw))
		}
		if !issue767JSONSource(httpValue, "GxViewsAlphaEdited", alphaMarker) {
			t.Fatalf("case 4.4 (%s): the http door answered from somewhere other than %s: %s", spelling.name, alphaMarker, e2eViewsJSONTail(httpValue))
		}
		if id, _ := cliIdentity["graph_id"].(string); id != "" {
			familyGraphID = id
		}
		table.pass("4.4", "gate 1+7", "%s: both front doors resolved the same view identity %v", spelling.name, cliIdentity)
	}

	// --- case 4.5 (gate 1): resources/read gortex://stats ≡ the graph_stats tool.
	//
	// This is the criterion an earlier item left unmet by deferral: the
	// resource and the tool are documented byte-for-byte equal — one
	// buildGraphStatsPayload, internal/mcp/tools_core.go:3513-3515 — and that
	// payload is context-scoped (engineFor(ctx) / readerFor(ctx), :3517-3531),
	// so for a session bound to a checkout the two surfaces must answer the
	// same view. The second session is what keeps the equality from being
	// vacuous: if the pair is view-scoped at all, a primary-bound session and a
	// checkout-bound session cannot both be right with the same numbers.
	primarySession := e2eViewsNewHTTP(t, addr, f.primary)
	if err := primarySession.initialize(ctx, 2*time.Minute); err != nil {
		t.Fatalf("case 4.5: %v", err)
	}
	boundResource, _ := e2eViewsStatsPayload(t, ctx, httpSession, true)
	boundTool, toolSpelling := e2eViewsStatsPayload(t, ctx, httpSession, false)
	if diff := e2eViewsStatsDiff(boundResource, boundTool); diff != "" {
		t.Fatalf("case 4.5: resources/read gortex://stats and %s disagree for a checkout-bound session: %s", toolSpelling, diff)
	}
	primaryResource, _ := e2eViewsStatsPayload(t, ctx, primarySession, true)
	primaryTool, _ := e2eViewsStatsPayload(t, ctx, primarySession, false)
	if diff := e2eViewsStatsDiff(primaryResource, primaryTool); diff != "" {
		t.Fatalf("case 4.5: resources/read gortex://stats and %s disagree for a primary-bound session: %s", toolSpelling, diff)
	}
	if diff := e2eViewsStatsDiff(boundTool, primaryTool); diff == "" {
		table.observed("4.5", "gate 1", "resources/read gortex://stats ≡ %s on both sessions, but the checkout-bound and primary-bound payloads are identical (%s), so the equality alone does not prove the pair is view-scoped",
			toolSpelling, e2eViewsStatsSummary(boundTool))
	} else {
		table.pass("4.5", "gate 1", "resources/read gortex://stats ≡ %s on a checkout-bound AND a primary-bound session, and the two sessions differ (%s)", toolSpelling, diff)
	}

	// --- case 4.6 (gate 1): a buffer overlay is session-scoped and writes nothing.
	overlaySource := issue767MarkerSource("GxViewsOverlayOnly")
	_, pushSpelling, pushErr := e2eViewsToolOrFacade(ctx, httpSession, "overlay_push", "overlay", "push",
		map[string]any{"path": alphaMarker, "content": overlaySource})
	if pushErr != nil {
		table.unsupported("4.6", "gate 1", "overlay_push refused on this daemon through both the legacy and the facade spelling: %v", pushErr)
	} else {
		if !e2eViewsAwaitHTTPSymbol(t, ctx, httpSession, "GxViewsOverlayOnly", alphaMarker, selector, 90*time.Second) {
			t.Fatal("case 4.6: the pushed buffer never became visible to the session that pushed it")
		}
		onDisk, err := os.ReadFile(alphaMarker)
		if err != nil {
			t.Fatal(err)
		}
		if string(onDisk) == overlaySource {
			t.Fatal("case 4.6: the overlay wrote its buffer to disk; an unsaved buffer must not become a file")
		}
		other := e2eViewsNewHTTP(t, addr, alpha)
		if err := other.initialize(ctx, time.Minute); err != nil {
			t.Fatalf("case 4.6: second session: %v", err)
		}
		if e2eViewsHTTPHasSymbol(t, ctx, other, "GxViewsOverlayOnly", alphaMarker, selector) {
			t.Fatal("case 4.6: a second MCP session saw another session's unsaved buffer")
		}
		if _, _, err := e2eViewsToolOrFacade(ctx, httpSession, "overlay_drop", "overlay", "drop", map[string]any{}); err != nil {
			t.Fatalf("case 4.6: overlay_drop: %v", err)
		}
		if e2eViewsHTTPHasSymbol(t, ctx, httpSession, "GxViewsOverlayOnly", alphaMarker, selector) {
			t.Fatal("case 4.6: the dropped overlay still answers")
		}
		table.pass("4.6", "gate 1", "buffer overlay (%s) visible only to its own MCP session, never written to disk, gone after overlay_drop", pushSpelling)
	}

	// --- case 4.7 (gate 7): automatic → dedicated → automatic.
	//
	// The request spelling follows the mode, and that is the point: a dedicated
	// checkout owns no checkout_routes row — "Only an automatic checkout is
	// routed here" (internal/mcp/view_request.go:1038-1041) — so it is asked as
	// its own repository, and what pins the answer is physical: the answering
	// symbol's absolute path has to be the checkout's own file.
	f.git(beta, "add", "-A")
	f.git(beta, "commit", "-m", "beta own commit")
	f.awaitSymbolAs(beta, "GxViewsBetaOnly", betaMarker, 3*time.Minute, issue767AsAutomaticWorktree)
	output, err := f.tryCommand(8*time.Minute, beta, "track", beta, "--as-worktree", "--wait", "--wait-timeout", "5m", "--no-progress")
	if err != nil {
		t.Fatalf("case 4.7: track --as-worktree %s: %v\n%s", beta, err, e2eViewsTail(output))
	}
	f.awaitSymbolAs(beta, "GxViewsBetaOnly", betaMarker, 5*time.Minute, issue767AsOwnCorpus)
	dedicated := e2eViewsPresent(t, f, "4.7", beta, "GxViewsBetaOnly", betaMarker, issue767AsOwnCorpus)
	dedicatedRoute, hadRoute := e2eViewsRoutes(t, ctx, db)[filepath.Clean(beta)]
	output, err = f.tryCommand(8*time.Minute, f.primary, "untrack", beta, "--no-progress")
	if err != nil {
		t.Fatalf("case 4.7: untrack %s: %v\n%s", beta, err, e2eViewsTail(output))
	}
	f.awaitSymbolAs(beta, "GxViewsBetaOnly", betaMarker, 5*time.Minute, issue767AsAutomaticWorktree)
	e2eViewsPresent(t, f, "4.7", f.primary, "Issue767PrimaryMarker", primaryMarker, issue767AsPrimary)
	table.pass("4.7", "gate 7", "automatic → dedicated (answered_by=%q, catalog=%s) → automatic again; the primary answered exactly throughout",
		dedicated.Prefix, e2eViewsRouteMode(dedicatedRoute, hadRoute))

	// --- case 4.8 (gate 4): a compatible branch switch reuses the routed pair.
	f.git(alpha, "add", "-A")
	f.git(alpha, "commit", "-m", "alpha own commit")
	f.awaitSymbolAs(alpha, "GxViewsAlphaEdited", alphaMarker, 3*time.Minute, issue767AsAutomaticWorktree)
	f.settle()
	settled := e2eViewsRoutes(t, ctx, db)[filepath.Clean(alpha)]
	f.git(alpha, "checkout", "-b", "m4-alpha-compat")
	// Exactness is not instantaneous across a switch, and settle() is not a
	// barrier for it: settle waits for three identical GENERATION samples, and
	// a checkout answering from a fallback while its route is re-keyed mints
	// no generation at all. So the wait is explicit, and how long it lasts is
	// part of what this case measures.
	switchStarted := time.Now()
	f.awaitSymbolAs(alpha, "GxViewsAlphaEdited", alphaMarker, 3*time.Minute, issue767AsAutomaticWorktree)
	switchWait := time.Since(switchStarted)
	f.settle()
	compat := e2eViewsRoutes(t, ctx, db)[filepath.Clean(alpha)]
	e2eViewsPresent(t, f, "4.8", alpha, "GxViewsAlphaEdited", alphaMarker, issue767AsAutomaticWorktree)
	if compat.HeadTree != settled.HeadTree {
		t.Fatalf("case 4.8: the switch was meant to keep the tree: %s -> %s", settled.HeadTree, compat.HeadTree)
	}
	if compat.pair() != settled.pair() {
		table.observed("4.8", "gate 4", "same-tree branch switch re-keyed the routed pair %s -> %s over an unchanged tree, and took %s to answer exactly again",
			settled.pair(), compat.pair(), switchWait.Truncate(time.Millisecond))
	} else {
		table.pass("4.8", "gate 4", "same-tree branch switch reused the routed pair %s unchanged and answered exactly again after %s",
			settled.pair(), switchWait.Truncate(time.Millisecond))
	}

	// --- case 4.9 (gate 1): an incompatible switch serves the new tree only.
	f.git(alpha, "checkout", "-b", "m4-alpha-divergent")
	f.write(alphaMarker, issue767MarkerSource("GxViewsDivergentOnly"))
	f.git(alpha, "add", "-A")
	f.git(alpha, "commit", "-m", "divergent content")
	f.awaitSymbolAs(alpha, "GxViewsDivergentOnly", alphaMarker, 3*time.Minute, issue767AsAutomaticWorktree)
	f.git(alpha, "checkout", "m4-alpha-compat")
	f.awaitSymbolAs(alpha, "GxViewsAlphaEdited", alphaMarker, 3*time.Minute, issue767AsAutomaticWorktree)
	e2eViewsAbsentEverywhere(t, f, "4.9", alpha, "GxViewsDivergentOnly", alphaMarker, issue767AsAutomaticWorktree)
	table.pass("4.9", "gate 1", "incompatible branch switch serves the new tree exactly and stops serving the abandoned one")

	// --- case 4.10 (gate 1): an inactive ref view reads committed state only.
	//
	// refs/heads/m4-alpha-divergent is no longer checked out anywhere. A
	// git_ref selector over it must serve that commit's own snapshot, and must
	// never serve the tree that IS checked out. The one outcome that is never
	// acceptable is an exact label over content from neither.
	const divergentRef = "refs/heads/m4-alpha-divergent"
	wantCommit := e2eViewsGit(t, f, alpha, "rev-parse", divergentRef)
	wantTree := e2eViewsGit(t, f, alpha, "rev-parse", divergentRef+"^{tree}")
	refView := e2eViewsViewSelector("git_ref", map[string]string{"value": divergentRef, "graph_id": familyGraphID})
	refValue, refRaw, refErr := e2eViewsCLI(t, f, f.primary, "search", e2eViewsSearchArgs("GxViewsDivergentOnly", refView), 3*time.Minute)
	if refErr != nil {
		table.unsupported("4.10", "gate 1", "the inactive ref view refused on this host: %v", refErr)
	} else {
		rider := e2eViewsFreshness(refValue)
		found, fallback := issue767JSONEvidence(refValue, "GxViewsDivergentOnly")
		exact := issue767JSONExact(refValue)
		switch {
		case found && !fallback:
			// A ref view answers from committed state, so its nodes carry the
			// repo-relative path and no absolute working-copy path: the commit
			// is checked out nowhere. What pins the answer instead is the
			// rider's own resolution — the ref, the commit and the tree it
			// says it read — plus the repo-relative file the symbol came from.
			file, prefix, ok := e2eViewsNamedResult(refValue, "GxViewsDivergentOnly")
			if !ok || !strings.HasSuffix(filepath.ToSlash(file), "marker.go") || !issue767PrefixMatches(prefix) {
				t.Fatalf("case 4.10: the ref view answered out of %q in %q: %s", file, prefix, e2eViewsTail(refRaw))
			}
			if got, _ := rider["resolved_ref"].(string); got != divergentRef {
				t.Fatalf("case 4.10: the ref view resolved %q, not the ref that was asked for: %s", got, e2eViewsTail(refRaw))
			}
			if got, _ := rider["resolved_commit"].(string); got != wantCommit {
				t.Fatalf("case 4.10: the ref view resolved commit %q, want %q", got, wantCommit)
			}
			if got, _ := rider["resolved_tree"].(string); got != wantTree {
				t.Fatalf("case 4.10: the ref view resolved tree %q, want %q", got, wantTree)
			}
			// The symbol that exists only on the branch that IS checked out
			// must not be reachable through a view of a commit that never had it.
			liveValue, _, liveErr := e2eViewsCLI(t, f, f.primary, "search", e2eViewsSearchArgs("GxViewsAlphaEdited", refView), 2*time.Minute)
			if liveErr == nil {
				if leaked, _ := issue767JSONEvidence(liveValue, "GxViewsAlphaEdited"); leaked {
					t.Fatalf("case 4.10: the inactive ref view answered for a symbol that exists only on the checked-out branch: %s", e2eViewsJSONTail(liveValue))
				}
			}
			table.pass("4.10", "gate 1", "inactive ref %s served its own committed snapshot (exact=%v, commit=%s tree=%s) out of %s, and nothing from the checked-out tree",
				divergentRef, exact, wantCommit[:8], wantTree[:8], file)
		case exact:
			t.Fatalf("case 4.10: the ref view labelled an answer exact while carrying neither the symbol nor a refusal: %s", e2eViewsTail(refRaw))
		default:
			table.unsupported("4.10", "gate 1", "the inactive ref view did not materialise within the budget on this host; rider=%v", rider)
		}
	}

	// --- case 4.11 (gate 7): removal cleans the catalog; the family survives.
	f.git(f.primary, "worktree", "remove", "--force", beta)
	f.awaitRemovedPath(beta)
	e2eViewsPresent(t, f, "4.11", f.primary, "Issue767PrimaryMarker", primaryMarker, issue767AsPrimary)
	e2eViewsPresent(t, f, "4.11", alpha, "GxViewsAlphaEdited", alphaMarker, issue767AsAutomaticWorktree)
	table.pass("4.11", "gate 7", "worktree remove dropped the catalog row; the primary and the surviving sibling both still answer exactly")
}

// e2eViewsRouteMode renders a checkout's catalog mode and routed pair, or says the
// row was absent — itself the documented shape for a checkout served from its
// own corpus rather than through its family.
func e2eViewsRouteMode(route e2eViewsRoute, present bool) string {
	if !present {
		return "no checkouts row"
	}
	return route.Mode + " route" + route.pair()
}

// e2eViewsNamedResult finds the result node for one symbol name and returns the
// file it came from and the repository prefix that served it.
//
// It reads file_path rather than absolute_file_path on purpose: a ref view of a
// commit that is checked out nowhere has no working-copy path to report, and a
// reader that insisted on one would mistake a correct committed answer for a
// wrong one.
func e2eViewsNamedResult(value any, name string) (file, prefix string, ok bool) {
	switch value := value.(type) {
	case map[string]any:
		if value["name"] == name {
			if path, has := value["file_path"].(string); has {
				repo, _ := value["repo_prefix"].(string)
				return path, repo, true
			}
		}
		for _, key := range e2eViewsSortedKeys(value) {
			if file, prefix, ok := e2eViewsNamedResult(value[key], name); ok {
				return file, prefix, true
			}
		}
	case []any:
		for _, child := range value {
			if file, prefix, ok := e2eViewsNamedResult(child, name); ok {
				return file, prefix, true
			}
		}
	case string:
		if child, has := e2eViewsEmbeddedJSON(value); has {
			return e2eViewsNamedResult(child, name)
		}
	}
	return "", "", false
}

// e2eViewsGit runs one read-only git command in a checkout and returns its
// trimmed output.
func e2eViewsGit(t *testing.T, f *issue767Fixture, dir string, args ...string) string {
	t.Helper()
	output, err := f.tryGit(dir, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, output)
	}
	return strings.TrimSpace(string(output))
}

// --------------------------------------------------- stats-surface helpers ---

// e2eViewsStatsPayload reads the graph-stats payload from one of the two surfaces
// of the same session: the resource, or the tool. It returns the payload and
// the spelling that answered, because the tool has two on this surface and
// which one a client can reach is part of the finding.
func e2eViewsStatsPayload(t *testing.T, ctx context.Context, session *e2eViewsHTTP, asResource bool) (map[string]any, string) {
	t.Helper()
	var (
		value any
		what  string
		err   error
	)
	if asResource {
		what = "resources/read gortex://stats"
		value, err = session.resource(ctx, "gortex://stats")
	} else {
		value, what, err = e2eViewsStatsTool(ctx, session)
	}
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	payload := e2eViewsFindStats(value)
	if payload == nil {
		t.Fatalf("%s carried no graph-stats payload: %s", what, e2eViewsJSONTail(value))
	}
	return payload, what
}

// e2eViewsToolOrFacade calls a tool by whichever spelling this daemon publishes,
// and reports which one answered.
//
// A legacy tool name is eagerly registered only under the full tool surface.
// The default preset publishes the curated core and defers the rest
// (internal/mcp/tool_presets.go), and the streamable transport's dispatcher
// answers `tool '<name>' not found` for a deferred name rather than promoting
// it — so a matrix that only knew the legacy spelling would record UNSUPPORTED
// for behaviour a default daemon has. Every deferred tool this matrix needs has
// a facade route that a default daemon always publishes
// (internal/mcp/facade_registry.go), and both spellings reach the same handler.
func e2eViewsToolOrFacade(ctx context.Context, session *e2eViewsHTTP, legacy, facade, operation string, args map[string]any) (any, string, error) {
	value, legacyErr := session.tool(ctx, legacy, args)
	if legacyErr == nil {
		return value, "tools/call " + legacy, nil
	}
	routed := map[string]any{"operation": operation}
	for key, value := range args {
		routed[key] = value
	}
	value, facadeErr := session.tool(ctx, facade, routed)
	if facadeErr == nil {
		return value, fmt.Sprintf("tools/call %s{operation:%s}", facade, operation), nil
	}
	return nil, "tools/call " + legacy, errors.Join(legacyErr, facadeErr)
}

// e2eViewsStatsTool is the graph-stats half of that: the `workspace` facade's
// `graph` operation IS graph_stats (internal/mcp/facade_registry.go:422-423),
// so either spelling reaches the one buildGraphStatsPayload the parity gate is
// about.
func e2eViewsStatsTool(ctx context.Context, session *e2eViewsHTTP) (any, string, error) {
	return e2eViewsToolOrFacade(ctx, session, "graph_stats", "workspace", "graph", map[string]any{})
}

// e2eViewsFindStats digs out the object carrying total_nodes and total_edges. The
// two surfaces wrap it differently (a resource contents list, a tool content
// list, either possibly as a JSON string) and both unwrap the same way.
func e2eViewsFindStats(value any) map[string]any {
	switch value := value.(type) {
	case map[string]any:
		_, hasNodes := value["total_nodes"]
		_, hasEdges := value["total_edges"]
		if hasNodes && hasEdges {
			return value
		}
		for _, key := range e2eViewsSortedKeys(value) {
			if found := e2eViewsFindStats(value[key]); found != nil {
				return found
			}
		}
	case []any:
		for _, child := range value {
			if found := e2eViewsFindStats(child); found != nil {
				return found
			}
		}
	case string:
		if child, ok := e2eViewsEmbeddedJSON(value); ok {
			return e2eViewsFindStats(child)
		}
	}
	return nil
}

// e2eViewsStatsDiff compares two graph-stats payloads on the fields the documented
// equality is about, and returns "" when they agree.
func e2eViewsStatsDiff(left, right map[string]any) string {
	var diverged []string
	for _, field := range []string{"total_nodes", "total_edges", "edge_identity_revisions"} {
		if fmt.Sprint(left[field]) != fmt.Sprint(right[field]) {
			diverged = append(diverged, fmt.Sprintf("%s: %v != %v", field, left[field], right[field]))
		}
	}
	return strings.Join(diverged, "; ")
}

func e2eViewsStatsSummary(payload map[string]any) string {
	return fmt.Sprintf("total_nodes=%v total_edges=%v", payload["total_nodes"], payload["total_edges"])
}

// ------------------------------------------------------------ HTTP probes ---

// e2eViewsHTTPHasSymbol asks one HTTP session for a symbol and reports whether it
// answered out of the expected file.
func e2eViewsHTTPHasSymbol(t *testing.T, ctx context.Context, session *e2eViewsHTTP, name, file string, view map[string]any) bool {
	t.Helper()
	value, err := session.tool(ctx, "search", e2eViewsSearchArgs(name, view))
	if err != nil {
		t.Logf("http search %s: %v", name, err)
		return false
	}
	return issue767JSONSource(value, name, file)
}

// e2eViewsAwaitHTTPSymbol polls one HTTP session until the symbol answers.
func e2eViewsAwaitHTTPSymbol(t *testing.T, ctx context.Context, session *e2eViewsHTTP, name, file string, view map[string]any, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e2eViewsHTTPHasSymbol(t, ctx, session, name, file, view) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

// ---------------------------------------------------- readers under test ----
//
// The matrices above are opt-in and need a daemon. The readers they judge with
// are not: a rider reader that silently returns nil, an identity comparison
// that reports agreement between two different views, or a census whose SQL
// does not match the product's schema would turn every matrix row into a
// vacuous pass. These run in the ordinary suite.

func TestE2EMatrixViewsFreshnessReaderFindsTheRiderWhereverItRides(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		want          string
	}{
		{
			name:    "tool result meta",
			payload: `{"result":{"_meta":{"freshness":{"exact":true,"actual_view":"worktree:path:/w"}}}}`,
			want:    "worktree:path:/w",
		},
		{
			name:    "json payload inside a content text block",
			payload: `{"result":{"content":[{"type":"text","text":"{\"freshness\":{\"exact\":false,\"actual_view\":\"base:g1\",\"fallback_reason\":\"view_building\"}}"}]}}`,
			want:    "base:g1",
		},
		{
			name:    "no rider at all",
			payload: `{"result":{"content":[{"type":"text","text":"{\"hits\":[]}"}]}}`,
			want:    "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(tc.payload), &value); err != nil {
				t.Fatal(err)
			}
			rider := e2eViewsFreshness(value)
			if tc.want == "" {
				if rider != nil {
					t.Fatalf("rider = %v, want none", rider)
				}
				return
			}
			if rider == nil {
				t.Fatal("no rider found where one rides")
			}
			if got, _ := rider["actual_view"].(string); got != tc.want {
				t.Fatalf("actual_view = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestE2EMatrixViewsIdentityComparisonNamesTheDivergingField(t *testing.T) {
	left := map[string]any{"requested_view": "worktree:path:/w", "actual_view": "worktree:path:/w", "exact": true, "checkout_id": "c1"}
	if diverged := e2eViewsSameIdentity(left, left); len(diverged) != 0 {
		t.Fatalf("identical identities diverged: %v", diverged)
	}
	right := map[string]any{"requested_view": "worktree:path:/w", "actual_view": "base:g1", "exact": true, "checkout_id": "c1"}
	diverged := e2eViewsSameIdentity(left, right)
	if len(diverged) != 1 || !strings.HasPrefix(diverged[0], "actual_view:") {
		t.Fatalf("diverged = %v, want exactly the actual_view field", diverged)
	}
	// A field only one side carries is a divergence, not a match: a door that
	// omits the checkout id has not resolved the same view as one that names
	// it. Both directions are pinned, because a comparison that only walked
	// the left-hand map would still catch the first and silently pass the
	// second — the door that answers with MORE identity than the other.
	thin := map[string]any{"requested_view": "worktree:path:/w", "actual_view": "worktree:path:/w", "exact": true}
	missing := e2eViewsSameIdentity(left, thin)
	if len(missing) != 1 || !strings.HasPrefix(missing[0], "checkout_id:") {
		t.Fatalf("diverged = %v, want exactly the checkout_id field", missing)
	}
	extra := e2eViewsSameIdentity(thin, left)
	if len(extra) != 1 || !strings.HasPrefix(extra[0], "checkout_id:") {
		t.Fatalf("diverged = %v, want exactly the checkout_id field when only the RIGHT side carries it", extra)
	}
}

func TestE2EMatrixViewsIdentityLiftsOnlyIdentityFields(t *testing.T) {
	identity := e2eViewsIdentity(map[string]any{
		"requested_view": "worktree:path:/w", "actual_view": "worktree:path:/w", "exact": true,
		"waited_ms": 1200, "base_scoped": []any{"graph_stats"},
	})
	if _, carried := identity["waited_ms"]; carried {
		t.Fatal("timing rode into the identity comparison; two calls seconds apart would diverge for no reason")
	}
	if len(identity) != 3 {
		t.Fatalf("identity = %v, want exactly the three fields the rider carried", identity)
	}
}

func TestE2EMatrixViewsStatsReaderAndDiff(t *testing.T) {
	var resource any
	if err := json.Unmarshal([]byte(`{"contents":[{"uri":"gortex://stats","text":"{\"total_nodes\":10,\"total_edges\":20,\"edge_identity_revisions\":0}"}]}`), &resource); err != nil {
		t.Fatal(err)
	}
	left := e2eViewsFindStats(resource)
	if left == nil {
		t.Fatal("no stats payload found in a resource response")
	}
	if diff := e2eViewsStatsDiff(left, map[string]any{"total_nodes": float64(10), "total_edges": float64(20), "edge_identity_revisions": float64(0)}); diff != "" {
		t.Fatalf("equal payloads diffed: %s", diff)
	}
	diff := e2eViewsStatsDiff(left, map[string]any{"total_nodes": float64(11), "total_edges": float64(20), "edge_identity_revisions": float64(0)})
	if !strings.Contains(diff, "total_nodes") {
		t.Fatalf("diff = %q, want the total_nodes divergence", diff)
	}
}

// TestE2EMatrixViewsLauncherInjectsTheFlagOnlyForDaemonStart runs the real launcher
// against a stub that prints its argv. Without this the HTTP half of matrix 4
// could be measuring a daemon that never got --http-addr and a CLI that got one
// it does not accept.
func TestE2EMatrixViewsLauncherInjectsTheFlagOnlyForDaemonStart(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	launcher := e2eViewsHTTPWrapper(t, dir, stub, "127.0.0.1:65001")
	for _, tc := range []struct {
		name string
		argv []string
		want []string
	}{
		{"daemon start", []string{"daemon", "start", "--backend", "sqlite"},
			[]string{"daemon", "start", "--http-addr", "127.0.0.1:65001", "--backend", "sqlite"}},
		{"daemon status", []string{"daemon", "status", "--no-progress"},
			[]string{"daemon", "status", "--no-progress"}},
		{"a tool call", []string{"call", "search", "--json", "{}"},
			[]string{"call", "search", "--json", "{}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			output, err := execCommandForE2EViews(ctx, launcher, tc.argv)
			if err != nil {
				t.Fatalf("launcher %v: %v\n%s", tc.argv, err, output)
			}
			got := strings.Split(strings.TrimSpace(string(output)), "\n")
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("argv = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestE2EMatrixViewsShellQuoteSurvivesAQuote(t *testing.T) {
	if got := e2eViewsShellQuote("/tmp/it's here/gortex"); got != `'/tmp/it'\''s here/gortex'` {
		t.Fatalf("quote = %s", got)
	}
}

// TestE2EMatrixViewsStoreCensusReadsTheColumnsTheCatalogDeclares builds the two catalog
// tables the census reads and proves the query, the join and the keying.
func TestE2EMatrixViewsStoreCensusReadsTheColumnsTheCatalogDeclares(t *testing.T) {
	db := e2eViewsMemoryStore(t, `
		CREATE TABLE checkouts (
			checkout_id TEXT PRIMARY KEY, root_path TEXT NOT NULL, state TEXT NOT NULL,
			head_tree TEXT NOT NULL DEFAULT '', effective_mode TEXT NOT NULL);
		CREATE TABLE checkout_routes (
			checkout_id TEXT PRIMARY KEY, graph_id TEXT NOT NULL,
			commit_generation_id INTEGER, dirty_generation_id INTEGER,
			route_epoch INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL);
		CREATE TABLE view_generations (
			generation_id INTEGER PRIMARY KEY AUTOINCREMENT, owner_kind TEXT NOT NULL,
			graph_id TEXT NOT NULL DEFAULT '', layer_id TEXT, checkout_id TEXT,
			generation_kind TEXT NOT NULL, base_generation_id INTEGER,
			tree_oid TEXT NOT NULL DEFAULT '', state TEXT NOT NULL);
		INSERT INTO checkouts VALUES ('c1','/w/one','ready','treeA','automatic');
		INSERT INTO checkouts VALUES ('c2','/w/two','ready','treeB','automatic');
		INSERT INTO checkout_routes VALUES ('c1','g1',11,12,3,'ready');
		INSERT INTO view_generations VALUES (7,'dedicated_graph','g1',NULL,NULL,'dedicated',NULL,'treeBase','active');
		INSERT INTO view_generations VALUES (11,'dedicated_graph','g1','commit:c1','c1','commit',7,'treeA','active');
		INSERT INTO view_generations VALUES (12,'dedicated_graph','g1','dirty:c1','c1','dirty',11,'','active');
	`)
	ctx := t.Context()
	routes := e2eViewsRoutes(t, ctx, db)
	if len(routes) != 2 {
		t.Fatalf("routes = %v, want both checkouts (a checkout with no route must still be seen)", routes)
	}
	one := routes["/w/one"]
	if one.CheckoutID != "c1" || one.HeadTree != "treeA" || one.Mode != "automatic" || one.pair() != "(11,12)" {
		t.Fatalf("route = %+v", one)
	}
	if two := routes["/w/two"]; two.pair() != "(0,0)" {
		t.Fatalf("an unrouted checkout must read as pair (0,0), got %s", two.pair())
	}
	generations := e2eViewsGenerations(t, ctx, db)
	if len(generations) != 3 {
		t.Fatalf("generations = %v", generations)
	}
	if commit := generations[11]; commit.Kind != "commit" || commit.BaseID != 7 || commit.TreeOID != "treeA" || commit.CheckoutID != "c1" {
		t.Fatalf("commit layer = %+v", commit)
	}
	if dirty := generations[12]; dirty.BaseID != 11 {
		t.Fatalf("dirty layer must name its commit layer as base, got %+v", dirty)
	}
	if base := generations[7]; base.Kind != "dedicated" || base.CheckoutID != "" {
		t.Fatalf("committed base = %+v", base)
	}
}

// TestE2EMatrixViewsCensusColumnsExistInTheCatalogSchema guards the transcription above
// against a column rename: every column name the census SQL uses has to appear
// in the catalog DDL the store actually creates.
func TestE2EMatrixViewsCensusColumnsExistInTheCatalogSchema(t *testing.T) {
	path := filepath.Join("..", "..", "internal", "graph", "store_sqlite", "schema.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("catalog schema not readable from this working directory (%s): %v", path, err)
	}
	ddl := string(raw)
	for _, column := range []string{
		"checkout_id", "root_path", "head_tree", "effective_mode",
		"commit_generation_id", "dirty_generation_id",
		"generation_id", "owner_kind", "generation_kind", "base_generation_id", "tree_oid",
	} {
		if !strings.Contains(ddl, column) {
			t.Fatalf("the census reads %q, which the catalog schema no longer declares", column)
		}
	}
}

func TestE2EMatrixViewsBudgetRefusesAShortDeadline(t *testing.T) {
	// A deadline is only observable through a real *testing.T, so the guard is
	// exercised for its no-deadline arm here and by the matrices themselves for
	// the other. What is pinned is that it does not fail when there is room.
	e2eViewsBudget(t, time.Nanosecond)
}

func TestE2EMatrixViewsTableRendersEveryOutcomeClassDistinctly(t *testing.T) {
	table := &e2eViewsTable{t: t, name: "unit"}
	table.pass("1", "gate 1", "ok")
	table.observed("2", "gate 5", "seen %d", 3)
	table.unsupported("3", "gate 7", "no")
	if len(table.rows) != 3 {
		t.Fatalf("rows = %v", table.rows)
	}
	for i, want := range []string{e2eViewsPass, e2eViewsObserved, e2eViewsUnsupported} {
		if table.rows[i].Outcome != want {
			t.Fatalf("row %d outcome = %q, want %q", i, table.rows[i].Outcome, want)
		}
	}
	if table.rows[1].Detail != "seen 3" {
		t.Fatalf("detail = %q", table.rows[1].Detail)
	}
}

// execCommandForE2EViews runs one command and returns its combined output. It is
// used only by the launcher test above, against a stub in a temp directory.
func execCommandForE2EViews(ctx context.Context, binary string, argv []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, argv...)
	return cmd.CombinedOutput()
}

// e2eViewsMemoryStore opens a private in-memory SQLite database and applies one
// DDL script. Single connection: modernc.org/sqlite's :memory: database is
// per-connection, so a pool would hand the test an empty schema.
func e2eViewsMemoryStore(t *testing.T, ddl string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), ddl); err != nil {
		t.Fatalf("apply fixture schema: %v", err)
	}
	return db
}
