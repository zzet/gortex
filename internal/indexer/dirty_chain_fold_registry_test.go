package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// renderContracts is a registry's records, one JSON line each, sorted.
func renderContracts(list []contracts.Contract) string {
	lines := make([]string, 0, len(list))
	for _, c := range list {
		data, _ := json.Marshal(c)
		lines = append(lines, string(data))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// registryOfReader loads the repository's contract registry from r.
func registryOfReader(r graph.Reader) []contracts.Contract {
	reg := contracts.LoadRegistryFromGraphWithScope(graph.NewDeltaWriter(r, nil), builderRepoPrefix, builderRepoPrefix, builderRepoPrefix)
	if reg == nil {
		return nil
	}
	return reg.ByRepo(builderRepoPrefix)
}

// The first edit over a fold takes the contract registry the chain's edits
// kept, handed over to the fold's stack (handOverFoldRegistryStack), and
// answers the contracts question as a clean index does. The chain is keyed
// by a commit stack, as the daemon's is over a dedicated base (a coordinator
// fixture's base corpus is the mutable generation zero, which keys nothing);
// the fold is the store's ChainFold, stepped to its end and published.
func TestFirstEditOverAFoldTakesTheHandedOverRegistry(t *testing.T) {
	resetChainOverlayCaches()
	t.Cleanup(resetChainOverlayCaches)
	builderIsolateGit(t)
	store := builderOpenStore(t, "registry-over-fold")
	repoDir := builderTempDir(t, "checkout-registry-over-fold")
	builderGit(t, repoDir, "init", "--initial-branch=main")
	builderWriteTree(t, repoDir, chainOverlayTree())
	builderGit(t, repoDir, "add", "-A")
	builderGit(t, repoDir, "commit", "-q", "-m", "base")
	builderIndex(t, store, repoDir)
	builder := builderNewBuilder(store)
	h := newDirtyChainBuilder(t, builder, store, repoDir, true)
	h.compact = false
	ctx := context.Background()
	const commit = int64(1) << 40
	build := func(label string, stack []int64) *EditDeltaReport {
		t.Helper()
		req := h.request()
		parent := stack[len(stack)-1]
		manifest, why := loadDirtyChainManifest(ctx, store, stack[1:])
		if why != "" {
			t.Fatalf("%s: the chain's manifest: %s", label, why)
		}
		req.Base = commitLayerBase{Reader: dirtyChainComposed(t, store, stack[1:]), corpus: store, stack: stack}
		req.Identity.BaseGenerationID = parent
		req.parent, req.parentManifest, req.parentDepth = parent, manifest, len(stack)-1
		recordLastEditDelta(nil)
		id, report, err := builder.BuildDirtyLayer(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if report.ParentGenerationID != parent {
			t.Fatalf("%s: built direct (%q)", label, report.ChainFallbackReason)
		}
		h.chain = append(h.chain, id)
		return LastEditDeltaReport()
	}
	route := func(k int) {
		builderWriteFile(t, repoDir, "api/routes.go", fmt.Sprintf(
			"package api\n\nimport \"github.com/gin-gonic/gin\"\n\nfunc setupRoutes(r *gin.Engine) {\n\tr.GET(\"/api/users\", listUsers)\n\tr.POST(\"/api/items%d\", createItem)\n}\n\nfunc listUsers()  {}\nfunc createItem() {}\n", k))
	}

	// c1 stands on the commit alone; c2, c3 chain on it, each carrying the
	// registry forward to the next stack.
	route(1)
	req := h.request()
	req.Base = commitLayerBase{Reader: store, corpus: store, stack: []int64{commit}}
	recordLastEditDelta(nil)
	c1, _, err := builder.BuildDirtyLayer(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	h.chain = append(h.chain, c1)
	for k := 2; k <= 3; k++ {
		route(k)
		build(fmt.Sprintf("c%d", k), append([]int64{commit}, h.chain...))
	}
	lower := slices.Clone(h.chain)

	// The fold of c1..c3.
	top, _, err := store.Catalog().GetViewGeneration(ctx, lower[len(lower)-1])
	if err != nil {
		t.Fatal(err)
	}
	to, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: top.OwnerKind, GraphID: top.GraphID, LayerID: top.LayerID, CheckoutID: top.CheckoutID,
		GenerationKind: top.GenerationKind, TreeOID: top.TreeOID, LowerViewFingerprint: top.LowerViewFingerprint,
		ConfigHash: top.ConfigHash, ExtractorVersions: top.ExtractorVersions, ResolverVersion: top.ResolverVersion,
		DependencyRevision: top.DependencyRevision, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := storeChainFoldBackend{store: store}
	fold, err := backend.BeginChainFold(ctx, lower, to, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runChainFoldSteps(ctx, fold, backend.StepRetryable, nil); err != nil {
		t.Fatalf("the fold's steps: %v", err)
	}
	manifest, why := loadDirtyChainManifest(ctx, store, lower)
	if why != "" {
		t.Fatalf("the lower chain's manifest: %s", why)
	}
	entries := make([]store_sqlite.InputManifestEntry, 0, len(manifest.entries))
	for _, e := range manifest.entries {
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b store_sqlite.InputManifestEntry) int { return strings.Compare(a.FilePath, b.FilePath) })
	if err := handle.WriteInputManifest(ctx, store_sqlite.InputManifestMeta{
		ManifestVersion: store_sqlite.InputManifestVersion, IsFull: true, EntryCount: len(entries), PolicyDigest: manifest.policy,
	}, entries); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishPayloadGeneration(ctx, to, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := fold.Release(ctx); err != nil {
		t.Fatal(err)
	}

	// The landing hands the registry over; the first edit over the fold
	// takes it and answers as a clean index does.
	if !handOverFoldRegistryStack(store, builderRepoPrefix, builderRepoPrefix, builderRepoPrefix, []int64{commit}, nil, lower, to, nil) {
		t.Fatal("no registry was kept for the folded chain")
	}
	// The handed-over registry is what a load of the fold's view gives.
	key, ok := editDeltaRegistryKey(commitLayerBase{stack: []int64{commit, to}}, store, builderRepoPrefix, builderRepoPrefix, builderRepoPrefix)
	if !ok {
		t.Fatal("no registry key for the fold's stack")
	}
	carried, ok := keptEditDeltaContractRegistry(key)
	if !ok {
		t.Fatal("nothing was filed under the fold's stack")
	}
	if got, exp := renderContracts(carried), renderContracts(registryOfReader(dirtyChainComposed(t, store, []int64{to}))); got != exp {
		t.Fatalf("the handed-over registry differs from a load of the fold's view\nhanded over:\n%s\nloaded:\n%s", got, exp)
	}
	h.chain = []int64{to}
	route(4)
	over := build("over the fold", []int64{commit, to})
	if over == nil || !over.ContractRegistryCached {
		t.Fatal("the first edit over the fold reloaded the contract registry")
	}
	clean := builderOpenStore(t, "registry-over-fold-clean")
	builderIndex(t, clean, repoDir)
	// By record identity, as the carry's own test compares with a clean
	// index: a whole index and the per-file path stamp different contract
	// metadata, and the symbol of a shared ID is a load's pick.
	got := renderIdentity(registryOfReader(dirtyChainComposed(t, store, h.chain)))
	want := renderIdentity(registryOfReader(clean))
	if want == "" {
		t.Fatal("the fixture has no contracts")
	}
	if got != want {
		t.Fatalf("contracts over the fold:\n%s\nclean index:\n%s", got, want)
	}
}
