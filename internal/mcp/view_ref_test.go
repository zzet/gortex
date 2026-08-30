package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

// The ref-view fixture: one real repository whose main branch is the indexed
// corpus, plus a branch, a tag and a remote-tracking ref that nobody has ever
// checked out — and a working tree deliberately left dirty, which is what the
// defining test needs to prove is invisible.
//
// Everything under it is real: a real git repository, a real index of its main
// branch, catalog rows written through the catalog's own API, and the
// production ref-view manager behind every selection.

const (
	refTestFamily   = "fam-ref"
	refTestCheckout = "co-ref"
	refTestSession  = "ref-session"
	refTestPrefix   = "repo"
)

type refStack struct {
	srv     *Server
	store   *store_sqlite.Store
	leases  *graphview.LeaseManager
	repo    string
	graphID string

	featureCommit string
	featureTree   string
}

func refIsolateGit(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "ref test")
	t.Setenv("GIT_AUTHOR_EMAIL", "ref@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "ref test")
	t.Setenv("GIT_COMMITTER_EMAIL", "ref@example.invalid")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

func refGit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func refWriteFiles(t testing.TB, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// newRefStack builds the repository, indexes main, writes the catalog identity
// the reconciler would have written, and wires a server that can serve views
// of committed state.
func newRefStack(t testing.TB) *refStack {
	t.Helper()
	refIsolateGit(t)

	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(repo)
	if err == nil {
		repo = resolved
	}

	refWriteFiles(t, repo, map[string]string{
		".gortex.yaml": "workspace: main-ws\n",
		"keep.go":      "package repo\n\nfunc Keeper() {}\n",
		"edit.go":      "package repo\n\nfunc Old() {}\n",
	})
	refGit(t, repo, "init", "--initial-branch=main")
	refGit(t, repo, "add", "-A")
	refGit(t, repo, "commit", "-m", "main")
	headTree := refGit(t, repo, "rev-parse", "HEAD^{tree}")

	refGit(t, repo, "switch", "--force-create", "feature", "main")
	refWriteFiles(t, repo, map[string]string{
		"edit.go":  "package repo\n\nfunc New() {}\n",
		"added.go": "package repo\n\nfunc Fresh() {}\n",
	})
	refGit(t, repo, "add", "-A")
	refGit(t, repo, "commit", "-m", "feature")
	featureCommit := refGit(t, repo, "rev-parse", "HEAD^{commit}")
	featureTree := refGit(t, repo, "rev-parse", "HEAD^{tree}")
	refGit(t, repo, "tag", "v1", featureCommit)
	refGit(t, repo, "update-ref", "refs/remotes/origin/feature", featureCommit)
	refGit(t, repo, "switch", "--force", "main")

	store, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfgPath := filepath.Join(base, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repo, Name: refTestPrefix}}}
	gc.SetConfigPath(cfgPath)
	if err := gc.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	cm, err := config.NewConfigManager(cfgPath)
	if err != nil {
		t.Fatalf("config manager: %v", err)
	}

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	bm := search.NewNull()
	mi := indexer.NewMultiIndexer(store, reg, bm, cm, zap.NewNop())
	if _, err := mi.IndexScoped("", ""); err != nil {
		t.Fatalf("index the fixture repository: %v", err)
	}

	graphID := indexer.GraphIDFor(refTestPrefix)
	seedRefCatalog(t, store, graphID, repo, headTree)

	ctx := context.Background()
	pipeline := indexer.DedicatedBasePipelineFor(cm.GetRepoConfig(refTestPrefix).Index)
	generationID, payload, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:         "dedicated_base",
		GraphID:           graphID,
		LayerID:           graphID + ":base",
		CheckoutID:        refTestCheckout,
		GenerationKind:    "dedicated_base",
		BaseGenerationID:  0,
		TreeOID:           headTree,
		ConfigHash:        pipeline.ConfigHash,
		ExtractorVersions: pipeline.ExtractorVersions,
		ResolverVersion:   pipeline.ResolverVersion,
		CreatedAt:         102,
	})
	if err != nil {
		t.Fatalf("begin primary payload generation: %v", err)
	}
	payloadMI := indexer.NewMultiIndexer(payload, reg, search.NewNull(), cm, zap.NewNop())
	if _, err := payloadMI.IndexScoped("", ""); err != nil {
		t.Fatalf("index primary payload generation: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 103); err != nil {
		t.Fatalf("publish primary payload generation: %v", err)
	}
	dedicated, found, err := store.Catalog().GetDedicatedGraph(ctx, graphID)
	if err != nil {
		t.Fatalf("get dedicated graph: %v", err)
	}
	if !found {
		t.Fatal("seeded dedicated graph is missing")
	}
	dedicated.ActiveGenerationID = generationID
	if err := store.Catalog().UpsertDedicatedGraph(ctx, dedicated); err != nil {
		t.Fatalf("activate primary payload generation: %v", err)
	}

	leases := graphview.NewLeaseManager()
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer:  mi,
		ConfigManager: cm,
		Graph:         store,
		Logger:        zap.NewNop(),
		ViewLeases:    leases,
	})
	if err != nil {
		t.Fatalf("build the lifecycle: %v", err)
	}

	eng := query.NewEngine(store)
	eng.SetSearch(bm)
	srv := NewServer(eng, store, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
		MultiIndexer:      mi,
		ConfigManager:     cm,
		CheckoutLifecycle: lifecycle,
	})
	srv.SetMaterializer(&graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases})

	return &refStack{
		srv: srv, store: store, leases: leases, repo: repo, graphID: graphID,
		featureCommit: featureCommit, featureTree: featureTree,
	}
}

func seedRefCatalog(t testing.TB, store *store_sqlite.Store, graphID, repo, headTree string) {
	t.Helper()
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          refTestFamily,
		CommonDirIdentity: filepath.Join(repo, ".git"),
		State:             reconcile.FamilyStateReady,
		CreatedAt:         100,
		LastSeen:          100,
	}); err != nil {
		t.Fatalf("UpsertRepositoryFamily: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    refTestCheckout,
		Incarnation:   "inc-ref",
		FamilyID:      refTestFamily,
		RootPath:      repo,
		GitDir:        filepath.Join(repo, ".git"),
		AdminName:     "@main",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadRef:       "refs/heads/main",
		HeadTree:      headTree,
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("UpsertCheckout: %v", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         graphID,
		OwnerCheckoutID: refTestCheckout,
		RepoPrefix:      refTestPrefix,
		FamilyID:        refTestFamily,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
}

// dirty rewrites a file in the working tree without committing it. The bytes
// it leaves behind are what a working-copy read returns and what a view of
// committed state must never return.
func (r *refStack) dirty(t *testing.T, name, body string) {
	t.Helper()
	refWriteFiles(t, r.repo, map[string]string{name: body})
}

func refSelector(kind, value string) map[string]any {
	return map[string]any{"kind": kind, "value": value}
}

// call drives one request through the whole tool middleware into a real
// handler, exactly as a client would.
func (r *refStack) call(
	t *testing.T,
	tool string,
	view map[string]any,
	args map[string]any,
	handler func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error),
) (*mcplib.CallToolResult, error) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	if view != nil {
		merged := map[string]any{}
		for k, v := range view {
			merged[k] = v
		}
		if _, named := merged["graph_id"]; !named {
			merged["graph_id"] = r.graphID
		}
		args["view"] = merged
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	ctx := WithSessionCWD(WithSessionID(context.Background(), refTestSession), r.repo)
	return r.srv.wrapToolHandler(handler)(ctx, req)
}

func (r *refStack) readFile(t *testing.T, view map[string]any, path string) (*mcplib.CallToolResult, error) {
	t.Helper()
	return r.call(t, "read_file", view, map[string]any{"path": path}, r.srv.handleReadFile)
}

func refResultObject(t *testing.T, res *mcplib.CallToolResult) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(viewResultText(t, res)), &obj); err != nil {
		t.Fatalf("unmarshal result: %v\n%s", err, viewResultText(t, res))
	}
	return obj
}

func refResultString(t *testing.T, obj map[string]any, key string) string {
	t.Helper()
	value, _ := obj[key].(string)
	return value
}

// TestRefViewServesTheCommittedTreeNotTheWorkingCopy is the defining
// semantic: the branch's committed bytes answer, and the uncommitted edit
// sitting at the same path in the checkout is invisible.
func TestRefViewServesTheCommittedTreeNotTheWorkingCopy(t *testing.T) {
	stack := newRefStack(t)
	stack.dirty(t, "edit.go", "package repo\n\nfunc Dirty() {}\n")

	res, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/edit.go")
	if err != nil {
		t.Fatalf("read through the ref view: %v", err)
	}
	obj := refResultObject(t, res)
	content := refResultString(t, obj, "content")
	if !strings.Contains(content, "func New()") {
		t.Errorf("the branch's committed content is missing:\n%s", content)
	}
	if strings.Contains(content, "Dirty") {
		t.Errorf("the uncommitted working-copy edit leaked into a committed view:\n%s", content)
	}
	if strings.Contains(content, "func Old()") {
		t.Errorf("the base corpus's content answered instead of the branch's:\n%s", content)
	}

	// The control: the same read with no view still reads the working copy,
	// so the difference above is the view and nothing else.
	plain, err := stack.readFile(t, nil, "repo/edit.go")
	if err != nil {
		t.Fatalf("read the working copy: %v", err)
	}
	if content := refResultString(t, refResultObject(t, plain), "content"); !strings.Contains(content, "Dirty") {
		t.Errorf("the working-copy read did not see the uncommitted edit:\n%s", content)
	}
}

// TestRefViewFileReadCarriesTheViewIdentity pins the location a file read
// reports under a view with no working copy: the view's own URI, and no path
// rooted in a checkout.
func TestRefViewFileReadCarriesTheViewIdentity(t *testing.T) {
	stack := newRefStack(t)
	res, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/added.go")
	if err != nil {
		t.Fatalf("read through the ref view: %v", err)
	}
	obj := refResultObject(t, res)

	uri := refResultString(t, obj, "view_uri")
	if !strings.HasPrefix(uri, graphview.ViewFileScheme+"://") {
		t.Fatalf("view_uri = %q, want a %s:// identity", uri, graphview.ViewFileScheme)
	}
	if !strings.HasSuffix(uri, "/repo/added.go") {
		t.Errorf("view_uri = %q, want it to end at the repo-relative path", uri)
	}
	if served := refResultString(t, obj, "served_from"); served != "view" {
		t.Errorf("served_from = %q, want \"view\"", served)
	}
	for _, key := range []string{"path", "resolved_path", "view_uri"} {
		if value := refResultString(t, obj, key); strings.Contains(value, stack.repo) {
			t.Errorf("%s = %q leaks the checkout root %q", key, value, stack.repo)
		}
	}

	rider := resultFreshness(t, res)
	if rider["exact"] != true {
		t.Errorf("an answer from the requested tree is not marked exact: %+v", rider)
	}
	if got := rider["resolved_commit"]; got != stack.featureCommit {
		t.Errorf("resolved_commit = %v, want %s", got, stack.featureCommit)
	}
	if got := rider["resolved_tree"]; got != stack.featureTree {
		t.Errorf("resolved_tree = %v, want %s", got, stack.featureTree)
	}
	if got := rider["resolved_ref"]; got != "refs/heads/feature" {
		t.Errorf("resolved_ref = %v, want refs/heads/feature", got)
	}
	if rider["view_fingerprint"] == nil {
		t.Error("the rider names no view fingerprint, so the file URI has no authority to resolve against")
	}
}

// TestRefViewSelectorsAgreeOnOneTree pins that a branch, a tag on the same
// commit, a remote-tracking ref, and the commit id itself all serve the same
// content.
func TestRefViewSelectorsAgreeOnOneTree(t *testing.T) {
	stack := newRefStack(t)
	stack.dirty(t, "edit.go", "package repo\n\nfunc Dirty() {}\n")

	for _, tc := range []struct {
		name     string
		selector map[string]any
		wantRef  string
	}{
		{"branch", refSelector("git_ref", "refs/heads/feature"), "refs/heads/feature"},
		{"tag", refSelector("git_ref", "refs/tags/v1"), "refs/tags/v1"},
		{"remote branch", refSelector("git_ref", "refs/remotes/origin/feature"), "refs/remotes/origin/feature"},
		{"commit", refSelector("commit", stack.featureCommit), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := stack.readFile(t, tc.selector, "repo/edit.go")
			if err != nil {
				t.Fatalf("read through the %s view: %v", tc.name, err)
			}
			content := refResultString(t, refResultObject(t, res), "content")
			if !strings.Contains(content, "func New()") || strings.Contains(content, "Dirty") {
				t.Errorf("the %s selector did not serve the committed tree:\n%s", tc.name, content)
			}
			rider := resultFreshness(t, res)
			if got := rider["resolved_tree"]; got != stack.featureTree {
				t.Errorf("resolved_tree = %v, want %s", got, stack.featureTree)
			}
			if got, _ := rider["resolved_ref"].(string); got != tc.wantRef {
				t.Errorf("resolved_ref = %q, want %q", got, tc.wantRef)
			}
		})
	}
}

// TestRefViewGraphServesTheBranchSymbols pins the other half of the answer:
// the composed reader carries the branch's symbols, not main's.
func TestRefViewGraphServesTheBranchSymbols(t *testing.T) {
	stack := newRefStack(t)
	var (
		names      map[string]bool
		candidates int
		hasContent bool
	)
	_, err := stack.call(t, "get_symbol", refSelector("git_ref", "refs/heads/feature"), nil,
		func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			names = map[string]bool{}
			for _, file := range []string{"repo/edit.go", "repo/added.go", "repo/keep.go"} {
				if sg := stack.srv.engineFor(ctx).GetFileSymbols(file); sg != nil {
					for _, n := range sg.Nodes {
						if n != nil {
							names[n.Name] = true
						}
					}
				}
			}
			// Search cannot read through the composed reader — every corpus is
			// per generation — so the stack has to be bound as corpora too, or
			// search would answer about the base corpus alone.
			view := requestViewFromContext(ctx)
			candidates = len(view.candidateLayers())
			_, hasContent = stack.srv.contentSearcherFor(ctx)
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if candidates != 2 {
		t.Errorf("the search stack has %d layers, want the ref generation over its primary base", candidates)
	}
	if !hasContent {
		t.Error("content search found no corpus for the ref view")
	}
	for _, want := range []string{"New", "Fresh", "Keeper"} {
		if !names[want] {
			t.Errorf("the branch symbol %q is not visible through the view: %v", want, names)
		}
	}
	if names["Old"] {
		t.Errorf("the corpus symbol the branch replaced is still visible: %v", names)
	}
}

// TestRefViewRefusesWrites pins the read-only rule. A ref view has no working
// copy, so a write issued while reading one could only land somewhere else.
func TestRefViewRefusesWrites(t *testing.T) {
	stack := newRefStack(t)
	for _, tool := range []string{"edit_file", "write_file", "rename_symbol", "batch_edit"} {
		t.Run(tool, func(t *testing.T) {
			res, err := stack.call(t, tool, refSelector("git_ref", "refs/heads/feature"), nil,
				func(context.Context, mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
					return mcplib.NewToolResultText(`{"wrote":true}`), nil
				})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			text := viewResultText(t, res)
			if !strings.Contains(text, graphview.CodeViewReadOnly) {
				t.Fatalf("%s was not refused with %s: %s", tool, graphview.CodeViewReadOnly, text)
			}
			if strings.Contains(text, `"wrote"`) {
				t.Fatalf("%s reached its handler under a read-only view", tool)
			}
		})
	}
}

// TestRefViewResolutionFailuresSurfaceVerbatim pins the two resolution answers
// a caller can act on. Both codes travel to the client unchanged.
func TestRefViewResolutionFailuresSurfaceVerbatim(t *testing.T) {
	stack := newRefStack(t)
	treeOID := refGit(t, stack.repo, "rev-parse", "refs/heads/feature^{tree}")
	refGit(t, stack.repo, "update-ref", "refs/tags/treeish", treeOID)

	for _, tc := range []struct {
		name     string
		selector map[string]any
		want     string
	}{
		{"absent branch", refSelector("git_ref", "refs/heads/never"), graphview.CodeRefNotAvailableLocally},
		{"absent commit", refSelector("commit", strings.Repeat("a", 40)), graphview.CodeRefNotAvailableLocally},
		{"tag on a tree", refSelector("git_ref", "refs/tags/treeish"), graphview.CodeRefNotCommit},
		{"commit id naming a tree", refSelector("commit", treeOID), graphview.CodeRefNotCommit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := stack.readFile(t, tc.selector, "repo/edit.go")
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if text := viewResultText(t, res); !strings.Contains(text, tc.want) {
				t.Fatalf("want %s, got: %s", tc.want, text)
			}
		})
	}
}

// TestRefViewSaturatedWriterAnswersViewBuilding pins what a selection says
// when the store's writer is busy with somebody else's build.
//
// A ref view is served by a build, and a build holds the store's mutation gate
// for as long as its transactions run. A selection that needed the writer for
// its own bookkeeping used to queue there and answer nothing until the tool
// deadline expired, which tells a caller neither what happened nor what to do.
// The typed answer is view_building with the retry interval: the same thing a
// build of this very view would have said, and the one a client can act on.
func TestRefViewSaturatedWriterAnswersViewBuilding(t *testing.T) {
	stack := newRefStack(t)

	release, err := stack.store.HoldWriteGate(context.Background())
	if err != nil {
		t.Fatalf("HoldWriteGate: %v", err)
	}
	defer release()

	res, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/edit.go")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	text := viewResultText(t, res)
	if !strings.Contains(text, graphview.CodeViewBuilding) {
		t.Fatalf("want %s while the writer is saturated, got: %s", graphview.CodeViewBuilding, text)
	}

	// The saturation was the whole of it: with the gate free the same request
	// builds the view and serves the committed bytes.
	release()
	if _, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/edit.go"); err != nil {
		t.Fatalf("call once the writer freed up: %v", err)
	}
}

// refProducerStates reads one generation's producer declarations by name.
func refProducerStates(t *testing.T, stack *refStack, generationID int64) map[string]store_sqlite.ProducerState {
	t.Helper()
	rows, err := stack.store.AtGeneration(generationID).ProducerStates()
	if err != nil {
		t.Fatalf("read producer states: %v", err)
	}
	out := make(map[string]store_sqlite.ProducerState, len(rows))
	for _, row := range rows {
		out[row.Producer] = row.State
	}
	return out
}

// awaitRefProducerState observes the asynchronous withdrawal worker's public
// contract. Object-missing reads schedule producer withdrawal without waiting
// behind SQLite's writer, so the catalog row is deliberately eventual rather
// than synchronized with the tool response.
func awaitRefProducerState(
	t *testing.T,
	stack *refStack,
	generationID int64,
	producer string,
	want store_sqlite.ProducerState,
) map[string]store_sqlite.ProducerState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		states := refProducerStates(t, stack, generationID)
		if states[producer] == want {
			return states
		}
		if time.Now().After(deadline) {
			t.Fatalf("producer %s did not reach %s: %+v", producer, want, states)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRefViewPrunedObjectWithdrawsTheSourceCapability pins the withdrawal: a
// read that finds the blob gone answers source_object_missing, the view stops
// claiming it can serve bytes, and everything the generation already holds
// keeps answering.
func TestRefViewPrunedObjectWithdrawsTheSourceCapability(t *testing.T) {
	stack := newRefStack(t)
	if _, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/edit.go"); err != nil {
		t.Fatalf("warm the view: %v", err)
	}
	generationID := stack.refViewGeneration(t)
	before := refProducerStates(t, stack, generationID)

	blob := refGit(t, stack.repo, "rev-parse", "refs/heads/feature:edit.go")
	loose := filepath.Join(stack.repo, ".git", "objects", blob[:2], blob[2:])
	if err := os.Remove(loose); err != nil {
		t.Skipf("the fixture blob is not a loose object: %v", err)
	}

	res, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/edit.go")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if text := viewResultText(t, res); !strings.Contains(text, graphview.CodeSourceObjectMissing) {
		t.Fatalf("want %s, got: %s", graphview.CodeSourceObjectMissing, text)
	}

	// The withdrawal moves exactly one producer. Comparing against what the
	// build declared is what makes that a claim about the withdrawal rather
	// than about which states a ref view happens to be born in.
	after := awaitRefProducerState(t, stack, generationID,
		string(graphview.CapSourceSnapshot), store_sqlite.ProducerStateUnavailable)
	for producer, state := range after {
		if producer == string(graphview.CapSourceSnapshot) {
			continue
		}
		if state != before[producer] {
			t.Errorf("the withdrawal disturbed %s: %s -> %s", producer, before[producer], state)
		}
	}

	// The graph half still answers out of rows the generation already holds.
	// Keep enough request evidence to distinguish selection refusal, a wrongly
	// bound view, and genuine graph-data loss when this invariant regresses.
	var (
		found          bool
		handlerReached bool
		boundLayers    string
		boundRider     string
	)
	graphResult, graphErr := stack.call(t, "get_symbol", refSelector("git_ref", "refs/heads/feature"), nil,
		func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			handlerReached = true
			view := requestViewFromContext(ctx)
			boundLayers = fmt.Sprintf("%+v", view.candidateLayers())
			boundRider = fmt.Sprintf("%+v", view.rider)
			sg := stack.srv.engineFor(ctx).GetFileSymbols("repo/added.go")
			found = sg != nil && len(sg.Nodes) > 0
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		})
	if graphErr != nil {
		t.Fatalf("post-withdrawal get_symbol failed: error=%v result=%+v rider=%s layers=%s",
			graphErr, graphResult, boundRider, boundLayers)
	}
	if !handlerReached {
		t.Fatalf("post-withdrawal get_symbol was refused before its handler: result=%+v rider=%s layers=%s",
			graphResult, boundRider, boundLayers)
	}
	responseRider := resultFreshness(t, graphResult)
	if responseRider["exact"] != true || responseRider["resolved_tree"] != stack.featureTree {
		t.Fatalf("post-withdrawal get_symbol bound the wrong view: rider=%+v request_rider=%s layers=%s result=%+v",
			responseRider, boundRider, boundLayers, graphResult)
	}
	if got := stack.refViewGeneration(t); got != generationID {
		t.Fatalf("post-withdrawal get_symbol replaced generation %d with %d: rider=%+v layers=%s result=%+v",
			generationID, got, responseRider, boundLayers, graphResult)
	}
	if !found {
		t.Fatalf("the exact ref view reached get_symbol but its graph lost repo/added.go: rider=%+v layers=%s result=%+v",
			responseRider, boundLayers, graphResult)
	}
}

// TestRefViewEditingContextCompressesTheCommittedFile pins the whole-file read
// the compress_bodies path makes under a view with no working copy: the bytes
// come out of the pinned tree and are labelled with the view they came from,
// and a pruned blob answers source_object_missing like every other read
// surface rather than degrading to the structural sections in silence.
func TestRefViewEditingContextCompressesTheCommittedFile(t *testing.T) {
	stack := newRefStack(t)
	stack.dirty(t, "edit.go", "package repo\n\nfunc Dirty() {}\n")

	args := func() map[string]any {
		return map[string]any{"path": "repo/edit.go", "compress_bodies": true}
	}
	res, err := stack.call(t, "get_editing_context", refSelector("git_ref", "refs/heads/feature"),
		args(), stack.srv.handleGetEditingContext)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	obj := refResultObject(t, res)
	compressed := refResultString(t, obj, "source_compressed")
	if compressed == "" {
		t.Fatalf("no compressed source came back: %v", obj)
	}
	if !strings.Contains(compressed, "func New()") {
		t.Errorf("the branch's committed content is missing:\n%s", compressed)
	}
	if strings.Contains(compressed, "Dirty") {
		t.Errorf("the uncommitted working-copy edit leaked into a committed view:\n%s", compressed)
	}
	if served := refResultString(t, obj, "served_from"); served != "view" {
		t.Errorf("served_from = %q, want the view", served)
	}
	if uri := refResultString(t, obj, "view_uri"); !strings.HasPrefix(uri, graphview.ViewFileScheme+"://") {
		t.Errorf("view_uri = %q, want the view's own identity", uri)
	}

	blob := refGit(t, stack.repo, "rev-parse", "refs/heads/feature:edit.go")
	loose := filepath.Join(stack.repo, ".git", "objects", blob[:2], blob[2:])
	if err := os.Remove(loose); err != nil {
		t.Skipf("the fixture blob is not a loose object: %v", err)
	}

	pruned, err := stack.call(t, "get_editing_context", refSelector("git_ref", "refs/heads/feature"),
		args(), stack.srv.handleGetEditingContext)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	text := viewResultText(t, pruned)
	if !strings.Contains(text, graphview.CodeSourceObjectMissing) {
		t.Fatalf("want %s, got: %s", graphview.CodeSourceObjectMissing, text)
	}
	// A degraded payload carrying the structural sections would contain the
	// same words in an omission note, so the refusal itself is the assertion.
	if !pruned.IsError {
		t.Fatalf("the pruned blob answered with a payload rather than a refusal: %s", text)
	}
}

// refViewGeneration reads the one ref-view generation the fixture published.
func (r *refStack) refViewGeneration(t *testing.T) int64 {
	t.Helper()
	rows, err := r.store.Catalog().ListViewGenerations(context.Background(), store_sqlite.ViewGenerationFilter{
		GraphID: r.graphID,
	})
	if err != nil {
		t.Fatalf("list generations: %v", err)
	}
	for _, row := range rows {
		if row.OwnerKind == "ref_view" && row.State == store_sqlite.ViewGenerationReady {
			return row.GenerationID
		}
	}
	t.Fatalf("no ref view generation was published: %+v", rows)
	return 0
}

// TestRefViewBuildingServesTheOlderGenerationAsAFallback pins the rider shape
// of a selection whose payload is still being produced: the older generation
// may answer, but only labelled inexact and naming the build to wait on.
func TestRefViewBuildingServesTheOlderGenerationAsAFallback(t *testing.T) {
	stack := newRefStack(t)
	if _, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"), "repo/edit.go"); err != nil {
		t.Fatalf("publish the first generation: %v", err)
	}

	ctx := context.Background()
	dedicated, found, err := stack.store.Catalog().GetDedicatedGraph(ctx, stack.graphID)
	if err != nil || !found {
		t.Fatalf("read the dedicated graph: found=%v err=%v", found, err)
	}
	views, err := stack.store.Catalog().ListRefViews(ctx, stack.graphID)
	if err != nil || len(views) != 1 {
		t.Fatalf("list ref views: %d rows, err=%v", len(views), err)
	}

	selector, err := graphview.ParseSelector("git_ref", stack.graphID, "", "refs/heads/feature")
	if err != nil {
		t.Fatalf("ParseSelector: %v", err)
	}
	rider := graphview.NewViewRider(selector)
	view, err := stack.srv.refViewBuilding(ctx, dedicated, indexer.RefViewResult{
		RefViewID:  views[0].RefViewID,
		State:      store_sqlite.RefViewBuilding,
		BuildToken: "build-token-1",
	}, rider)
	if err != nil {
		t.Fatalf("refViewBuilding: %v", err)
	}
	defer view.close()

	if view.rider.Exact {
		t.Error("a generation built for a tree the selector has left is marked exact")
	}
	if !strings.Contains(view.rider.FallbackReason, graphview.CodeViewBuilding) ||
		!strings.Contains(view.rider.FallbackReason, "build-token-1") {
		t.Errorf("fallback_reason = %q, want it to name the build behind %s",
			view.rider.FallbackReason, graphview.CodeViewBuilding)
	}
	if view.rider.BuildToken != "build-token-1" {
		t.Errorf("build_token = %q, want the build to poll", view.rider.BuildToken)
	}
	if view.rider.RetryAfter <= 0 {
		t.Error("a building answer carries no retry hint")
	}
	if view.reader == nil {
		t.Error("the labelled fallback served nothing at all")
	}
}

// TestRefViewNeedsAnUnambiguousGraph pins that the server never picks a
// repository for a caller that reaches several: the same branch name in two
// repositories is two different answers.
func TestRefViewNeedsAnUnambiguousGraph(t *testing.T) {
	stack := newRefStack(t)
	res, err := stack.call(t, "read_file",
		map[string]any{"kind": "git_ref", "value": "refs/heads/feature", "graph_id": "graph-nonexistent"},
		map[string]any{"path": "repo/edit.go"}, stack.srv.handleReadFile)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if text := viewResultText(t, res); !strings.Contains(text, graphview.CodeInvalidViewSelector) {
		t.Fatalf("want %s, got: %s", graphview.CodeInvalidViewSelector, text)
	}
}

// TestRefViewRefusesAnAbsolutePath pins that a path naming a location in a
// working copy is refused rather than resolved against one this view is not.
func TestRefViewRefusesAnAbsolutePath(t *testing.T) {
	stack := newRefStack(t)
	res, err := stack.readFile(t, refSelector("git_ref", "refs/heads/feature"),
		filepath.Join(stack.repo, "edit.go"))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if text := viewResultText(t, res); !strings.Contains(text, "absolute path") {
		t.Fatalf("an absolute path was not refused: %s", text)
	}
}

// TestRefViewSymbolSourceReadsTheCommittedTree pins the second file-content
// surface: a symbol's lines come out of the view's tree, not out of whatever
// the checkout holds at that path.
func TestRefViewSymbolSourceReadsTheCommittedTree(t *testing.T) {
	stack := newRefStack(t)
	stack.dirty(t, "added.go", "package repo\n\nfunc Dirty() {}\n")

	var symbolID string
	if _, err := stack.call(t, "get_symbol_source", refSelector("git_ref", "refs/heads/feature"), nil,
		func(ctx context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			if sg := stack.srv.engineFor(ctx).GetFileSymbols("repo/added.go"); sg != nil {
				for _, n := range sg.Nodes {
					if n != nil && n.Name == "Fresh" {
						symbolID = n.ID
					}
				}
			}
			return mcplib.NewToolResultText(`{"ok":true}`), nil
		}); err != nil {
		t.Fatalf("locate the branch symbol: %v", err)
	}
	if symbolID == "" {
		t.Fatal("the branch symbol is not in the composed graph")
	}

	res, err := stack.call(t, "get_symbol_source", refSelector("git_ref", "refs/heads/feature"),
		map[string]any{"id": symbolID}, stack.srv.handleGetSymbolSource)
	if err != nil {
		t.Fatalf("get_symbol_source: %v", err)
	}
	obj := refResultObject(t, res)
	if source := refResultString(t, obj, "source"); !strings.Contains(source, "func Fresh()") ||
		strings.Contains(source, "Dirty") {
		t.Errorf("the symbol source did not come from the committed tree:\n%s", source)
	}
	if uri := refResultString(t, obj, "view_uri"); !strings.HasPrefix(uri, graphview.ViewFileScheme+"://") {
		t.Errorf("view_uri = %q, want a %s:// identity", uri, graphview.ViewFileScheme)
	}
}
