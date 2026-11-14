package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// These are real clean Git worktrees and a normally indexed/published SQLite
// store, not synthetic ready routes. Reopening preserves checkout identity.
type cleanLinkedSourceFixture struct {
	roots     []string
	symbol    string
	store     *store_sqlite.Store
	srv       *Server
	extractor *pendingBudgetExtractor
	ids       []store_sqlite.Checkout
	close     func()
	observed  map[string]any
}

func newCleanLinkedSourceFixture(t *testing.T) *cleanLinkedSourceFixture {
	t.Helper()
	return newCleanLinkedSourceFixtureWithSource(t, []byte("package sample\n\nfunc Target() string { return \"clean\" }\n"), "Target")
}

func newCleanLinkedSourceFixtureWithSource(t *testing.T, source []byte, symbol string) *cleanLinkedSourceFixture {
	t.Helper()
	_, err := exec.LookPath("git")
	require.NoError(t, err)
	base := t.TempDir()
	main := writeViewRepo(t, filepath.Join(base, "repo"), "clean-source", map[string]string{
		"main.go": string(source),
	})
	gitInit(t, main, "init", "-q", "-b", "main")
	gitInit(t, main, "config", "user.name", "Test")
	gitInit(t, main, "config", "user.email", "test@example.com")
	gitInit(t, main, "config", "commit.gpgsign", "false")
	gitInit(t, main, "add", ".")
	gitInit(t, main, "commit", "-q", "-m", "clean source")
	roots := []string{main, filepath.Join(base, "linked-a"), filepath.Join(base, "linked-b")}
	for _, root := range roots[1:] {
		gitInit(t, main, "worktree", "add", "-q", "--detach", root)
	}
	cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: main, Name: "repo"}}}
	cfgPath := filepath.Join(base, "config.yaml")
	cfg.SetConfigPath(cfgPath)
	require.NoError(t, cfg.Save())
	f := &cleanLinkedSourceFixture{roots: roots, symbol: symbol}
	open := func() {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		store, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
		require.NoError(t, err)
		cm, err := config.NewConfigManager(cfgPath)
		require.NoError(t, err)
		e := &pendingBudgetExtractor{base: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
		registry := parser.NewRegistry()
		registry.Register(e)
		mi := indexer.NewMultiIndexer(store, registry, search.NewNull(), cm, zap.NewNop())
		registerMCPFixtureRepos(t, mi, cm)
		eng := query.NewEngine(store)
		eng.SetSearch(search.NewNull())
		srv := NewServer(eng, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{MultiIndexer: mi, ConfigManager: cm})
		require.NotNil(t, srv.lifecycle)
		srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: srv.lifecycle.ViewLeases()})
		f.store, f.srv, f.extractor = store, srv, e
		f.close = sync.OnceFunc(func() { require.NoError(t, srv.lifecycle.Close()); require.NoError(t, store.Close()) })
		t.Cleanup(f.close)
		require.NoError(t, srv.lifecycle.Seed(ctx))
		f.ids = nil
		for _, root := range roots {
			checkout, present, err := srv.lifecycle.ObserveCheckoutPath(ctx, root)
			require.NoError(t, err)
			require.True(t, present)
			f.ids = append(f.ids, checkout)
			if root == main {
				continue
			}
			srv.lifecycle.ActivateCheckout(checkout.CheckoutID, "clean persisted source test")
			for {
				route, found, err := store.Catalog().GetCheckoutRoute(ctx, checkout.CheckoutID)
				require.NoError(t, err)
				if found && graphview.RouteReady(route) {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("normal clean linked route did not publish: %v", ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		idx := mi.GetIndexer("repo")
		require.NotNil(t, idx)
		raw, err := os.ReadFile(filepath.Join(main, "main.go"))
		require.NoError(t, err)
		matched, err := idx.IndexedContentMatchesContext(ctx, "main.go", raw)
		require.NoError(t, err)
		require.True(t, matched, "canonical content identity must be established before comparison")
	}
	open()
	before := append([]store_sqlite.Checkout(nil), f.ids...)
	f.close()
	open()
	for i := range before {
		require.Equal(t, before[i].CheckoutID, f.ids[i].CheckoutID)
		require.Equal(t, before[i].Incarnation, f.ids[i].Incarnation)
	}
	return f
}

func (f *cleanLinkedSourceFixture) call(t *testing.T, root string, requestID int, withoutSelectedProof ...bool) *mcplib.CallToolResult {
	t.Helper()
	leaf, ok := f.srv.facades.legacy("get_symbol_source")
	require.True(t, ok)
	f.srv.facades.capture(leaf.tool, func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if len(withoutSelectedProof) > 0 && withoutSelectedProof[0] {
			// Exercise the existing unsupported-reader fallback, not a false
			// hash or fabricated generation/owner. No production state changes.
			view := *requestViewFromContext(ctx)
			view.reader = struct{ graph.Reader }{view.reader}
			ctx = withRequestView(ctx, &view)
		}
		result, callErr := leaf.handler(ctx, req)
		deadline, hasDeadline := ctx.Value(projectedSourceDeadlineKey{}).(time.Time)
		observation := map[string]any{"private_deadline_present": hasDeadline, "private_deadline_expired": hasDeadline && !time.Now().Before(deadline), "caller_error": fmt.Sprint(ctx.Err()), "deferred_files": requestDeferredProjectionFiles(ctx), "selected_root": requestViewPathRoot(ctx).root}
		// Observe only after the real leaf has already returned its answer.
		// This cannot spend the measured source read's allowance or supply data
		// to it. The caller's unchanged three-second context bounds observation.
		view := requestViewFromContext(ctx)
		if view != nil && view.materialized != nil {
			handoff := view.materialized.Handoff()
			if handoff != nil {
				defer handoff.Close()
				if checked, ok := handoff.Reader.(graph.ConstantValueProjectionReader); ok {
					key := graph.ConstantFileKey{RepoPrefix: "repo", FilePath: "repo/main.go"}
					proof, proofErr := checked.ReadConstantValueProjectionContext(ctx, []string{"repo/main.go::" + f.symbol}, []graph.ConstantFileKey{key})
					observation["selected_file_meta"] = proof.Files[key]
					observation["selected_node_file"] = proof.Nodes["repo/main.go::"+f.symbol]
					observation["selected_proof_error"] = fmt.Sprint(proofErr)
				}
			}
		}
		owners := []map[string]any{}
		pendingFileProjections.mu.Lock()
		for _, entry := range pendingFileProjections.snap.Load().entries {
			owners = append(owners, map[string]any{"path": entry.absPath, "graph_path": entry.graphPath, "commit_seq": entry.commitSeq, "source_commit_seq": entry.sourceCommitSeq, "source_events": entry.sourceEvents})
		}
		pendingFileProjections.mu.Unlock()
		observation["projection_owners"] = owners
		f.observed = observation
		return result, callErr
	})
	defer f.srv.facades.capture(leaf.tool, leaf.handler)
	session := root
	require.NoError(t, f.srv.ConnectSession(session, func([]byte) error { return nil }))
	t.Cleanup(func() { f.srv.DisconnectSession(session) })
	ctx, cancel := context.WithTimeout(WithSessionCWD(WithSessionID(t.Context(), session), root), 3*time.Second)
	defer cancel()
	ctx = f.srv.WithClientSession(ctx, session)
	f.srv.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
	require.NotNil(t, f.srv.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load","version":"1"}}}`)))
	f.srv.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	args := map[string]any{"operation": "source", "target": map[string]any{"symbol": "repo/main.go::" + f.symbol}, "output": map[string]any{"format": "json", "max_bytes": 0}}
	result, err := externalVersionRegisteredCall(f.srv, ctx, requestID, "read", args)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

// This is the exact 118501-byte source target from the retained acceptance
// fixture (bdbcdf6), copied as inert test data; no historical checkout is read.
func TestCleanLinkedPersistedSourceRepresentativeFile(t *testing.T) {
	selectedProjectionRegistry(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "linked-source-store.go.txt"))
	require.NoError(t, err)
	require.Len(t, raw, 118501)
	f := newCleanLinkedSourceFixtureWithSource(t, raw, "Open")
	for i, root := range f.roots {
		path := filepath.Join(root, "main.go")
		beforeStamp, err := os.Stat(path)
		require.NoError(t, err)
		result := f.call(t, root, 400+i)
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, sha256.Sum256(raw), sha256.Sum256(after))
		afterStamp, err := os.Stat(path)
		require.NoError(t, err)
		require.True(t, os.SameFile(beforeStamp, afterStamp))
		require.Equal(t, beforeStamp.ModTime(), afterStamp.ModTime())
		if i > 0 {
			meta, ok := f.observed["selected_file_meta"].(graph.FileMetaRow)
			require.True(t, ok)
			sum := blake3.Sum256(raw)
			require.Equal(t, hex.EncodeToString(sum[:]), meta.ContentHash)
			require.Equal(t, len(raw), meta.Size)
			require.Equal(t, "repo/main.go", f.observed["selected_node_file"])
			require.Equal(t, f.ids[i].CheckoutID, resultFreshness(t, result)["checkout_id"])
			require.Equal(t, f.ids[i].Incarnation, resultFreshness(t, result)["checkout_incarnation"])
		}
		encoded, err := json.Marshal(f.observed)
		require.NoError(t, err)
		t.Logf("ACTUAL_CLEAN_SOURCE_READ root=%s bytes=%d sha256=%x observation=%s response=%s", root, len(raw), sha256.Sum256(after), encoded, resultText(result))
		require.False(t, result.IsError, resultText(result))
		body := extractTextResult(t, result)
		require.Contains(t, body["source"], "func Open(", "clean persisted source did not return actual target: %s", resultText(result))
	}
}

func TestCleanLinkedPersistedSourceRegisteredReads(t *testing.T) {
	selectedProjectionRegistry(t)
	f := newCleanLinkedSourceFixture(t)
	for i, root := range f.roots {
		before, err := os.ReadFile(filepath.Join(root, "main.go"))
		require.NoError(t, err)
		result := f.call(t, root, 100+i)
		require.False(t, result.IsError, resultText(result))
		body := extractTextResult(t, result)
		require.Contains(t, body["source"], `return "clean"`, "clean persisted checkout returned no actual source: %s", resultText(result))
		if i > 0 {
			require.Equal(t, f.ids[i].CheckoutID, resultFreshness(t, result)["checkout_id"])
		}
		after, err := os.ReadFile(filepath.Join(root, "main.go"))
		require.NoError(t, err)
		require.Equal(t, sha256.Sum256(before), sha256.Sum256(after))
	}
}

func TestCleanLinkedPersistedSourceBudgetIsNotPhysicalWrite(t *testing.T) {
	selectedProjectionRegistry(t)
	f := newCleanLinkedSourceFixture(t)
	f.extractor.hold.Store(true)
	release := sync.OnceFunc(func() { close(f.extractor.release) })
	t.Cleanup(release)
	primary := f.call(t, f.roots[0], 201)
	require.Contains(t, extractTextResult(t, primary)["source"], `return "clean"`)
	require.True(t, f.extractor.hold.Load(), "canonical matching read unnecessarily extracted")
	path := filepath.Join(f.roots[1], "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	stamp, err := os.Stat(path)
	require.NoError(t, err)
	// Unknown selected proof retains the existing bounded physical projection.
	// Its budget expiry reports pending without inventing a physical write.
	result := f.call(t, f.roots[1], 202, true)
	select {
	case <-f.extractor.entered:
	default:
		t.Fatal("routed source did not enter actual held extractor")
	}
	require.NotContains(t, resultText(result), `return \"clean\"`, "expired projection must not claim source")
	require.Contains(t, resultText(result), "pending")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	afterStamp, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(stamp, afterStamp))
	require.Equal(t, stamp.ModTime(), afterStamp.ModTime())
	require.Equal(t, sha256.Sum256(before), sha256.Sum256(after))
	release()
	select {
	case <-f.extractor.finished:
	case <-time.After(time.Second):
		t.Fatal("held extraction did not join")
	}
	t.Log("ACTUAL_UNSUPPORTED_SELECTED_PROOF_FALLBACK: held_extractor_entered=true pending_no_source=true file_hash_inode_mtime_unchanged=true extraction_joined=true")
}

func TestCleanLinkedPersistedSourceMatchedReceiptAvoidsHeldExtractor(t *testing.T) {
	selectedProjectionRegistry(t)
	f := newCleanLinkedSourceFixture(t)
	f.extractor.hold.Store(true)
	release := sync.OnceFunc(func() { close(f.extractor.release) })
	t.Cleanup(release)
	for i, root := range f.roots {
		result := f.call(t, root, 500+i)
		require.Contains(t, extractTextResult(t, result)["source"], `return "clean"`)
		require.True(t, f.extractor.hold.Load(), "matching indexed bytes were parsed again")
	}
}

func TestCleanLinkedPersistedSourceRealWriteNeverServesOldBytes(t *testing.T) {
	selectedProjectionRegistry(t)
	f := newCleanLinkedSourceFixture(t)
	path := filepath.Join(f.roots[1], "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n\nfunc Target() string { return \"changed\" }\n"), 0o644))
	result := f.call(t, f.roots[1], 301)
	body := extractTextResult(t, result)
	if source, ok := body["source"].(string); ok {
		require.Contains(t, source, `return "changed"`)
		require.NotContains(t, source, `return "clean"`)
	} else {
		require.Contains(t, resultText(result), "pending", "a true write may refuse but never certify old bytes")
	}
}
