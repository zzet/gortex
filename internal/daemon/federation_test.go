package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// recordingSearchRemote serves find_usages (empty SubGraph) and a
// search_symbols endpoint that records how many times it was hit, so a
// test can prove the name-keyed fallback did or did not fire.
func recordingSearchRemote(t *testing.T, searchJSON string) (*httptest.Server, *int32) {
	t.Helper()
	var searchCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "indexed": true, "schema_version": localSchemaMajor,
			"api_version": 1, "read_only": true,
		})
	})
	mux.HandleFunc("/v1/tools/search_symbols", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&searchCalls, 1)
		_, _ = w.Write(envelope(searchJSON))
	})
	mux.HandleFunc("/v1/tools/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &searchCalls
}

func nameKeyedFederator(on bool) *Federator {
	return NewFederator(FederationConfig{
		PerRemoteTimeout:  250 * time.Millisecond,
		Budget:            2 * time.Second,
		HealthTTL:         time.Millisecond,
		NameKeyedFallback: on,
	}, func(e ServerEntry) (*ServerClient, error) { return NewServerClient(e) }, nil)
}

// TestFederator_NameKeyedFallback_OffByDefault asserts that with the
// fallback off, no bare-name search is issued.
func TestFederator_NameKeyedFallback_OffByDefault(t *testing.T) {
	remote, searchCalls := recordingSearchRemote(t, `{"results":[{"id":"r/a::ComputeChecksum"}],"total":1}`)
	local := envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`)
	out := nameKeyedFederator(false).Augment(context.Background(), "find_usages",
		[]byte(`{"arguments":{"id":"l/a.go::ComputeChecksum"}}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	if atomic.LoadInt32(searchCalls) != 0 {
		t.Fatalf("name-keyed fallback must not fire when off (search hit %d times)", *searchCalls)
	}
	m := decodeFederated(t, out)
	if _, ok := m["name_hits"]; ok {
		t.Error("no name_hits section when the fallback is off")
	}
}

// TestFederator_NameKeyedFallback_OptInSeparateSection asserts that with
// the fallback on and an eligible name, remote hits land in a separate
// name_hits section tagged text_matched; a common/short name is skipped.
func TestFederator_NameKeyedFallback_OptInSeparateSection(t *testing.T) {
	remote, searchCalls := recordingSearchRemote(t, `{"results":[{"id":"r/a::ComputeChecksum","name":"ComputeChecksum"}],"total":1}`)
	fed := nameKeyedFederator(true)
	local := envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`)

	// Eligible name => name_hits present, tagged text_matched.
	out := fed.Augment(context.Background(), "find_usages",
		[]byte(`{"arguments":{"id":"l/a.go::ComputeChecksum"}}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	if atomic.LoadInt32(searchCalls) == 0 {
		t.Fatal("an eligible name must trigger the bare-name search")
	}
	m := decodeFederated(t, out)
	hits, ok := m["name_hits"].([]any)
	if !ok || len(hits) != 1 {
		t.Fatalf("name_hits should carry the remote hit, got %v", m["name_hits"])
	}
	if first, _ := hits[0].(map[string]any); first["confidence"] != "text_matched" || first["origin"] != "remote:r2" {
		t.Errorf("name hit must be tagged text_matched + remote origin, got %v", hits[0])
	}
	// The primary results are NOT polluted with name hits (SubGraph nodes stay empty).
	if nodes, _ := m["nodes"].([]any); len(nodes) != 0 {
		t.Error("name hits must stay in their own section, not the primary results")
	}

	// A short/common name is gated out even when on.
	atomic.StoreInt32(searchCalls, 0)
	out2 := fed.Augment(context.Background(), "find_usages",
		[]byte(`{"arguments":{"id":"l/a.go::len"}}`), local,
		[]ServerEntry{{Slug: "r2", URL: remote.URL}})
	if atomic.LoadInt32(searchCalls) != 0 {
		t.Error("a common/short name must be gated out of the fallback")
	}
	if _, ok := decodeFederated(t, out2)["name_hits"]; ok {
		t.Error("a gated name must produce no name_hits section")
	}
}

// TestAudit_RemoteRoutedCallLogged asserts a remote-routed call emits a
// structured audit line carrying {session_id, cwd, tool, target_slug}.
func TestAudit_RemoteRoutedCallLogged(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"nodes":[],"edges":[]}`})
	cfg := &ServersConfig{Server: []ServerEntry{{Slug: "r2", URL: remote.URL, Default: true}}}
	router := NewRouter(RouterConfig{
		Servers:     cfg,
		Rosters:     NewWorkspaceRosterCache(time.Minute),
		CwdResolver: func(string) (string, bool) { return "", false },
		LocalSlug:   LocalServerSentinel,
		LocalExecute: func(context.Context, string, []byte) ([]byte, int, error) {
			return []byte(`{}`), 200, nil
		},
		Logger: zap.New(core),
	})
	_, _, err := router.RouteToolCall(context.Background(), "find_usages", []byte(`{}`), RouteContext{
		Cwd:            "/repo",
		SessionID:      "sess-1",
		EnabledRemotes: []ServerEntry{{Slug: "r2", URL: remote.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := logs.FilterMessage("federation: remote-routed call").All()
	if len(entries) != 1 {
		t.Fatalf("want exactly one audit line, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	for k, want := range map[string]string{"tool": "find_usages", "target_slug": "r2", "cwd": "/repo", "session_id": "sess-1"} {
		if fields[k] != want {
			t.Errorf("audit field %q = %v, want %q", k, fields[k], want)
		}
	}
}

// envelope wraps a tool JSON payload in the MCP result envelope the local
// executor and /v1/tools both emit.
func envelope(toolJSON string) []byte {
	b, _ := json.Marshal(map[string]any{
		"content": []map[string]any{{"type": "text", "text": toolJSON}},
	})
	return b
}

type fakeRemoteOpts struct {
	indexed   bool
	schema    int
	toolJSON  string
	toolSleep time.Duration
	healthErr bool
}

func fakeRemote(t *testing.T, o fakeRemoteOpts) *httptest.Server {
	t.Helper()
	// Closed as the test ends, before srv.Close (cleanups run last-in
	// first-out): Close waits for every handler, and a sleeping one would hold
	// the test for its whole toolSleep after the caller gave up on it.
	stop := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		if o.healthErr {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		schema := o.schema
		if schema == 0 {
			schema = localSchemaMajor
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "indexed": o.indexed, "nodes": 1, "edges": 0,
			"version": "test", "schema_version": schema, "api_version": 1,
			"read_only": true, "capabilities": []string{"events"},
		})
	})
	mux.HandleFunc("/v1/tools/", func(w http.ResponseWriter, r *http.Request) {
		if o.toolSleep > 0 {
			select {
			case <-time.After(o.toolSleep):
			case <-stop:
				return
			}
		}
		_, _ = w.Write(envelope(o.toolJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })
	return srv
}

func testFederator() *Federator {
	return NewFederator(FederationConfig{
		PerRemoteTimeout: 250 * time.Millisecond,
		Budget:           2 * time.Second,
		BreakerThreshold: 2,
		BreakerCooldown:  500 * time.Millisecond,
		HealthTTL:        time.Millisecond,
	}, func(e ServerEntry) (*ServerClient, error) { return NewServerClient(e) }, nil)
}

func decodeFederated(t *testing.T, out []byte) map[string]any {
	t.Helper()
	tool, _ := unwrapToolJSON(out)
	var m map[string]any
	if err := json.Unmarshal(tool, &m); err != nil {
		t.Fatalf("decode merged tool json: %v (%s)", err, tool)
	}
	return m
}

// TestFederator_MergeSubGraphOrigins merges a local + remote SubGraph and
// asserts the origins map tags each node, local wins on collision, and
// edges dedupe.
func TestFederator_MergeSubGraphOrigins(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"nodes":[{"id":"r/x.go::Caller"},{"id":"shared::Sym"}],"edges":[{"from":"r/x.go::Caller","to":"shared::Sym","kind":"calls"}],"total_nodes":2,"total_edges":1}`})
	local := envelope(`{"nodes":[{"id":"shared::Sym"}],"edges":[],"total_nodes":1,"total_edges":0}`)

	out := testFederator().Augment(context.Background(), "find_usages", []byte(`{}`),
		local, []ServerEntry{{Slug: "r2", URL: remote.URL}})

	m := decodeFederated(t, out)
	origins, _ := m["origins"].(map[string]any)
	if origins["shared::Sym"] != "local" {
		t.Errorf("collision node must stay local-origin, got %v", origins["shared::Sym"])
	}
	if origins["r/x.go::Caller"] != "remote:r2" {
		t.Errorf("remote-only node must be tagged remote:r2, got %v", origins["r/x.go::Caller"])
	}
	nodes, _ := m["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("merged nodes: want 2 (local wins on the shared id), got %d", len(nodes))
	}
	fed, _ := m["federation"].(map[string]any)
	q, _ := fed["remotes_queried"].([]any)
	if len(q) != 1 {
		t.Errorf("remotes_queried should list r2, got %v", fed["remotes_queried"])
	}
	if fed["degraded"] != false {
		t.Errorf("a healthy remote must not be degraded")
	}
}

// TestFederator_LocalOnlyUnchanged asserts no enabled remotes leaves the
// local response byte-for-byte unchanged in the pure-local path.
func TestFederator_LocalOnlyUnchanged(t *testing.T) {
	local := envelope(`{"nodes":[{"id":"shared::Sym"}],"edges":[],"total_nodes":1,"total_edges":0}`)
	out := testFederator().Augment(context.Background(), "find_usages", []byte(`{}`), local, nil)
	if string(out) != string(local) {
		t.Fatalf("local-only response must be unchanged:\n got %s\nwant %s", out, local)
	}
}

// TestFederator_DeadRemoteDegrades asserts a never-answering remote
// degrades the response within the deadline and is bucketed in
// remotes_failed, with the local result still present.
func TestFederator_DeadRemoteDegrades(t *testing.T) {
	dead := fakeRemote(t, fakeRemoteOpts{indexed: true, toolSleep: 5 * time.Second, toolJSON: `{}`})
	local := envelope(`{"nodes":[{"id":"shared::Sym"}],"edges":[],"total_nodes":1,"total_edges":0}`)

	start := time.Now()
	out := testFederator().Augment(context.Background(), "find_usages", []byte(`{}`),
		local, []ServerEntry{{Slug: "slow", URL: dead.URL}})
	if time.Since(start) > 2*time.Second {
		t.Fatalf("a dead remote must not block past the budget")
	}
	m := decodeFederated(t, out)
	fed, _ := m["federation"].(map[string]any)
	if fed["degraded"] != true {
		t.Errorf("a dead remote must set degraded:true")
	}
	failed, _ := fed["remotes_failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("the dead remote must be in remotes_failed, got %v", fed["remotes_failed"])
	}
	// Local node still present.
	if nodes, _ := m["nodes"].([]any); len(nodes) != 1 {
		t.Errorf("the local result must survive a remote failure, got %d nodes", len(nodes))
	}
}

// TestFederator_QueriedVsFailed asserts both lists are present with one OK
// and one dead remote.
func TestFederator_QueriedVsFailed(t *testing.T) {
	ok := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"nodes":[{"id":"r/a::N"}],"edges":[],"total_nodes":1,"total_edges":0}`})
	dead := fakeRemote(t, fakeRemoteOpts{indexed: true, toolSleep: 5 * time.Second, toolJSON: `{}`})
	local := envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`)

	out := testFederator().Augment(context.Background(), "find_usages", []byte(`{}`), local,
		[]ServerEntry{{Slug: "ok", URL: ok.URL}, {Slug: "dead", URL: dead.URL}})
	m := decodeFederated(t, out)
	fed, _ := m["federation"].(map[string]any)
	if q, _ := fed["remotes_queried"].([]any); len(q) != 2 {
		t.Errorf("remotes_queried should list both, got %v", fed["remotes_queried"])
	}
	if f, _ := fed["remotes_failed"].([]any); len(f) != 1 {
		t.Errorf("remotes_failed should list the dead one, got %v", fed["remotes_failed"])
	}
	if fed["note"] == nil {
		t.Error("a human-readable note must be emitted when a remote fails")
	}
}

// TestFederator_WarmingBucketed asserts a reachable-but-warming remote is
// bucketed, not counted as an empty success.
func TestFederator_WarmingBucketed(t *testing.T) {
	warming := fakeRemote(t, fakeRemoteOpts{indexed: false, toolJSON: `{}`})
	local := envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`)
	out := testFederator().Augment(context.Background(), "find_usages", []byte(`{}`),
		local, []ServerEntry{{Slug: "warm", URL: warming.URL}})
	m := decodeFederated(t, out)
	fed, _ := m["federation"].(map[string]any)
	failed, _ := fed["remotes_failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("a warming remote must be in remotes_failed, got %v", fed["remotes_failed"])
	}
	if first, _ := failed[0].(map[string]any); first["reason"] != "warming" {
		t.Errorf("reason should be warming, got %v", failed[0])
	}
}

// TestFederator_IncompatibleSchemaRefused asserts a remote on an
// incompatible major schema is refused.
func TestFederator_IncompatibleSchemaRefused(t *testing.T) {
	incompatible := fakeRemote(t, fakeRemoteOpts{indexed: true, schema: 99, toolJSON: `{"nodes":[{"id":"r/a::N"}]}`})
	local := envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`)
	out := testFederator().Augment(context.Background(), "find_usages", []byte(`{}`),
		local, []ServerEntry{{Slug: "newer", URL: incompatible.URL}})
	m := decodeFederated(t, out)
	fed, _ := m["federation"].(map[string]any)
	failed, _ := fed["remotes_failed"].([]any)
	if len(failed) != 1 {
		t.Fatalf("incompatible schema must be refused, got %v", fed["remotes_failed"])
	}
	if first, _ := failed[0].(map[string]any); first["reason"] != "incompatible_schema" {
		t.Errorf("reason should be incompatible_schema, got %v", failed[0])
	}
}

// TestFederator_PerToolShapes asserts the ranked-list (search_symbols)
// and impl-list (find_implementations) adapters merge by their own key.
func TestFederator_PerToolShapes(t *testing.T) {
	t.Run("search_symbols", func(t *testing.T) {
		remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"results":[{"id":"r/a::N","name":"N"}],"total":1}`})
		local := envelope(`{"results":[{"id":"l/a::M","name":"M"}],"total":1}`)
		out := testFederator().Augment(context.Background(), "search_symbols", []byte(`{}`),
			local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
		m := decodeFederated(t, out)
		results, _ := m["results"].([]any)
		if len(results) != 2 {
			t.Fatalf("ranked list should concat to 2, got %d", len(results))
		}
		if toInt(m["total"]) != 2 {
			t.Errorf("totals should sum to 2, got %v", m["total"])
		}
		origins, _ := m["origins"].(map[string]any)
		if origins["r/a::N"] != "remote:r2" || origins["l/a::M"] != "local" {
			t.Errorf("origins wrong: %v", origins)
		}
	})
	t.Run("find_implementations", func(t *testing.T) {
		remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{"implementations":[{"id":"r/a::Impl"}],"total":1}`})
		local := envelope(`{"implementations":[{"id":"l/a::Impl"}],"total":1}`)
		out := testFederator().Augment(context.Background(), "find_implementations", []byte(`{}`),
			local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
		m := decodeFederated(t, out)
		if impls, _ := m["implementations"].([]any); len(impls) != 2 {
			t.Fatalf("impl list should union to 2, got %d", len(impls))
		}
	})
}

// TestFederation_MultiRemoteConcurrent asserts the fan-out queries two
// remotes concurrently (both handlers entered before either returns) and
// never bulk-ingests /v1/graph.
func TestFederation_MultiRemoteConcurrent(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	mk := func(slug string) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "indexed": true, "schema_version": localSchemaMajor, "api_version": 1})
		})
		mux.HandleFunc("/v1/graph", func(w http.ResponseWriter, r *http.Request) {
			t.Error("federation must never bulk-ingest /v1/graph")
		})
		mux.HandleFunc("/v1/tools/", func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
			_, _ = w.Write(envelope(`{"nodes":[{"id":"` + slug + `::N"}],"edges":[],"total_nodes":1,"total_edges":0}`))
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}
	r1, r2 := mk("r1"), mk("r2")
	fed := NewFederator(FederationConfig{PerRemoteTimeout: 5 * time.Second, Budget: 10 * time.Second, HealthTTL: time.Millisecond},
		func(e ServerEntry) (*ServerClient, error) { return NewServerClient(e) }, nil)

	local := envelope(`{"nodes":[],"edges":[],"total_nodes":0,"total_edges":0}`)
	done := make(chan []byte, 1)
	go func() {
		done <- fed.Augment(context.Background(), "find_usages", []byte(`{}`), local,
			[]ServerEntry{{Slug: "r1", URL: r1.URL}, {Slug: "r2", URL: r2.URL}})
	}()

	// If the fan-out were serial, only one handler would enter while the
	// other blocks — this would deadlock. Reading both proves concurrency.
	timeout := time.After(8 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-timeout:
			t.Fatal("remotes were queried serially, not concurrently")
		}
	}
	close(release)

	m := decodeFederated(t, <-done)
	origins, _ := m["origins"].(map[string]any)
	if origins["r1::N"] != "remote:r1" || origins["r2::N"] != "remote:r2" {
		t.Errorf("both remotes' nodes should be tagged by origin, got %v", origins)
	}
}

// TestFederator_WriteToolNeverFederated asserts a mutating tool is never
// fanned out even if (somehow) it reaches Augment.
func TestFederator_WriteToolNeverFederated(t *testing.T) {
	remote := fakeRemote(t, fakeRemoteOpts{indexed: true, toolJSON: `{}`})
	local := envelope(`{"ok":true}`)
	for _, tool := range []string{"edit_file", "batch_edit"} {
		out := testFederator().Augment(context.Background(), tool, []byte(`{}`),
			local, []ServerEntry{{Slug: "r2", URL: remote.URL}})
		if string(out) != string(local) {
			t.Fatalf("%s must never be federated; response changed", tool)
		}
	}
}

// TestFederationReadAllowlistIsEffectFree makes the fan-out allowlist and the
// canonical effect registry a two-key gate. A future session/control writer
// must not become remotely fan-out eligible merely because it is intentionally
// excluded from the planning-mode IsMutating set.
func TestFederationReadAllowlistIsEffectFree(t *testing.T) {
	for tool := range federationReadTools {
		if effect := EffectOf(tool); effect != EffectNone {
			t.Errorf("federation read allowlist contains effectful tool %q (effect=%d)", tool, effect)
		}
	}
}
