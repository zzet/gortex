package indexer

import (
	"context"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// The real-repository harness in the production shape: the corpus is a
// published dedicated base generation (a positive id, as a daemon's is), and
// every working-tree build stands on a view the materializer composes and
// ancestryLayerBase opens — so the base carries the immutable stack the
// per-stack caches key on. Over generation zero (the harness's older shape)
// the stack is nil and every per-stack cache is off.

// editDeltaRealDedicatedGraph is the dedicated graph the harness's base
// generation is published under.
const editDeltaRealDedicatedGraph = "graph-fixture"

// editDeltaRealDedicatedBase returns the published dedicated base generation
// of the store at storePath, building it from tree on first use. The id is
// kept beside the store (storePath + ".generation") so later runs reuse it.
func editDeltaRealDedicatedBase(t *testing.T, tree, storePath string, cfg config.IndexConfig, logger *zap.Logger) int64 {
	t.Helper()
	idPath := storePath + ".generation"
	if raw, err := os.ReadFile(idPath); err == nil {
		if id, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil && id > 0 {
			return id
		}
	}
	started := time.Now()
	ctx := context.Background()
	store := builderOpenStoreAt(t, storePath)
	defer func() { _ = store.Close() }()
	builder := &SparseGenerationBuilder{Store: store, Registry: builderRegistry(), Config: cfg, Logger: logger}
	treeOID := builderGit(t, tree, "rev-parse", "HEAD^{tree}")
	commit := builderGit(t, tree, "rev-parse", "HEAD")
	catalog := store.Catalog()
	const ownerID, incarnation = "checkout-fixture-owner", "incarnation-owner"
	family := store_sqlite.RepositoryFamily{FamilyID: "family-fixture", CommonDirIdentity: filepath.Join(tree, ".git"), State: "active"}
	if err := catalog.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID: ownerID, Incarnation: incarnation, FamilyID: family.FamilyID,
		RootPath: tree, GitDir: family.CommonDirIdentity, AdminName: "main", State: store_sqlite.CheckoutStateReady,
		DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadTree: treeOID, HeadCommit: commit,
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID: editDeltaRealDedicatedGraph, OwnerCheckoutID: ownerID, RepoPrefix: builderRepoPrefix,
		FamilyID: family.FamilyID, IsPrimaryBase: true, State: store_sqlite.DedicatedGraphReady,
	}); err != nil {
		t.Fatal(err)
	}
	authority, err := catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: editDeltaRealDedicatedGraph,
		Owner:   store_sqlite.DedicatedBaseOwner{CheckoutID: ownerID, Incarnation: incarnation},
		Token:   "authority-real-repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: authority, Identity: store_sqlite.DedicatedBaseIdentity{
			TreeOID: treeOID, ConfigHash: "config-real-repo",
			ExtractorVersions: extractorVersionsFingerprint(), ResolverVersion: "resolver-real-repo",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "attempt-real-repo", ProvenanceCommitOID: commit, CreatedAt: time.Now().Unix(),
	})
	if err != nil || claim.GenerationID <= 0 {
		t.Fatalf("claim the dedicated base: %+v %v", claim, err)
	}
	id, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: tree, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
	})
	if err != nil {
		t.Fatalf("build the dedicated base: %v", err)
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		t.Fatalf("adopt the dedicated base: %v", err)
	}
	if err := os.WriteFile(idPath, []byte(strconv.FormatInt(id, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("dedicated base %d of %s: %s", id, tree, time.Since(started))
	return id
}

// useDedicatedBase makes every build of h stand on a materialized view of
// the dedicated base generation (a direct build) or of the chain's top (a
// chained one), opened the way the coordinator opens it (ancestryLayerBase).
func (h *dirtyChainBuilder) useDedicatedBase(base int64) {
	materializer := &graphview.Materializer{Store: h.store, Catalog: h.store.Catalog(), Leases: graphview.NewLeaseManager(), Logger: zap.NewNop()}
	opener := &CheckoutCoordinator{store: h.store}
	h.rootGeneration = base
	h.open = func(ctx context.Context, generationID int64) (LayerBase, func(), error) {
		view, err := materializer.MaterializeRefView(ctx, editDeltaRealDedicatedGraph, generationID)
		if err != nil {
			return nil, nil, err
		}
		return opener.ancestryLayerBase(view), view.Close, nil
	}
}

// editDeltaRealCheckStackKey confirms, for one edit in the dedicated-base
// shape, that the per-stack caches were on and the chain was applied over
// them: the delta has a key, a chained edit keeps the key of the edit before
// it, and it composed every layer of its parent chain. It logs the key (as a
// short digest), the layers overlaid and the contract registry's load, which
// stays keyed by the whole stack and so reloads on every chained edit. It
// returns the key for the next edit.
func editDeltaRealCheckStackKey(t *testing.T, rel string, edit int, delta *EditDeltaReport, chain []int64, prevKey string) string {
	t.Helper()
	if delta == nil {
		t.Errorf("%s edit %d: no edit delta", rel, edit)
		return prevKey
	}
	var registry time.Duration
	for _, phase := range delta.Phases {
		if phase.Name == "contract_registry" {
			registry += phase.Duration
		}
	}
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(delta.StackCacheKey))
	t.Logf("%s edit %d: stack key %016x (%d bytes) overlaid=%d chain=%d contract_registry=%.1fms cached=%t",
		rel, edit, digest.Sum64(), len(delta.StackCacheKey), delta.ChainLayersOverlaid, len(chain),
		float64(registry.Microseconds())/1000, delta.ContractRegistryCached)
	if delta.StackCacheKey == "" {
		t.Errorf("%s edit %d: the delta had no per-stack key: the caches were off", rel, edit)
	}
	if want := len(chain) - 1; delta.ChainLayersOverlaid != want {
		t.Errorf("%s edit %d: overlaid %d chain layers, want %d", rel, edit, delta.ChainLayersOverlaid, want)
	}
	if len(chain) > 1 && prevKey != "" && delta.StackCacheKey != prevKey {
		t.Errorf("%s edit %d: a chained edit moved the per-stack key", rel, edit)
	}
	return delta.StackCacheKey
}
