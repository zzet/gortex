package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestReadyGenerationCacheReusesAcrossOwnersAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := store.Catalog()
	key := readyCacheTestKey("graph-main", 0)
	first := createReadyCacheGeneration(t, catalog, key, "checkout", "checkout-a", "layer-a", "commit-a")
	second := createReadyCacheGeneration(t, catalog, key, "ref_view", "", "ref-alias", "commit-b")

	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key:                   key,
		CandidateGenerationID: second,
		LeaseToken:            "cross-owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found || claim.WinnerGenerationID != first || !claim.Reused || !claim.RetiredCandidate {
		t.Fatalf("cross-owner claim = %+v found=%v, want winner=%d reused retired", claim, found, first)
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claim, found, err = reopened.Catalog().ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key:        key,
		LeaseToken: "restart-hit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found || claim.WinnerGenerationID != first || !claim.Reused {
		t.Fatalf("restart claim = %+v found=%v, want persistent winner=%d", claim, found, first)
	}
}

func TestReadyGenerationCachePreservesGraphAndBaseIsolation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := store.Catalog()

	graphA := readyCacheTestKey("graph-a", 0)
	graphB := graphA
	graphB.GraphID = "graph-b"
	generationA := createReadyCacheGeneration(t, catalog, graphA, "checkout", "checkout-a", "layer-a", "commit-a")
	generationB := createReadyCacheGeneration(t, catalog, graphB, "checkout", "checkout-b", "layer-b", "commit-a")
	assertReadyCacheWinner(t, catalog, graphA, generationA, "graph-a-claim")
	assertReadyCacheWinner(t, catalog, graphB, generationB, "graph-b-claim")

	baseKey := readyCacheTestKey("graph-base", 0)
	baseKey.TreeOID = "base-tree-a"
	baseA := createReadyCacheGeneration(t, catalog, baseKey, "dedicated", "", "base-a", "base-a")
	baseKey.TreeOID = "base-tree-b"
	baseB := createReadyCacheGeneration(t, catalog, baseKey, "dedicated", "", "base-b", "base-b")
	layerA := readyCacheTestKey("graph-base", baseA)
	layerB := readyCacheTestKey("graph-base", baseB)
	generationOnA := createReadyCacheGeneration(t, catalog, layerA, "checkout", "checkout-a", "commit-a", "commit-a")
	generationOnB := createReadyCacheGeneration(t, catalog, layerB, "checkout", "checkout-b", "commit-b", "commit-a")
	assertReadyCacheWinner(t, catalog, layerA, generationOnA, "base-a-claim")
	assertReadyCacheWinner(t, catalog, layerB, generationOnB, "base-b-claim")
}

func TestReadyGenerationCacheRejectsStaleRootsAndDeadAncestry(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := store.Catalog()

	staleKey := readyCacheTestKey("graph-stale", 0)
	stale := createReadyCacheGeneration(t, catalog, staleKey, "checkout", "checkout-stale", "stale", "stale")
	if err := catalog.SetViewGenerationState(ctx, stale, ViewGenerationSuperseded, ViewGenerationReady); err != nil {
		t.Fatal(err)
	}
	if claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{Key: staleKey}); err != nil || found {
		t.Fatalf("stale root claim = %+v found=%v err=%v, want miss", claim, found, err)
	}

	baseKey := readyCacheTestKey("graph-dead-base", 0)
	baseKey.TreeOID = "base-tree"
	base := createReadyCacheGeneration(t, catalog, baseKey, "dedicated", "", "base", "base")
	childKey := readyCacheTestKey("graph-dead-base", base)
	child := createReadyCacheGeneration(t, catalog, childKey, "checkout", "checkout-child", "child", "child")
	if err := catalog.SetViewGenerationState(ctx, base, ViewGenerationRetiring, ViewGenerationReady); err != nil {
		t.Fatal(err)
	}
	if claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{Key: childKey}); err != nil || found {
		t.Fatalf("dead ancestry claim = %+v found=%v err=%v, want miss", claim, found, err)
	}
	if _, _, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key: childKey, CandidateGenerationID: child,
	}); err == nil {
		t.Fatal("candidate over retiring ancestry unexpectedly accepted")
	}
}

func TestReadyGenerationCacheLeaseProtectsHandoff(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := store.Catalog()
	key := readyCacheTestKey("graph-lease", 0)
	generationID := createReadyCacheGeneration(t, catalog, key, "checkout", "checkout", "lease", "lease")
	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key: key, LeaseToken: "handoff",
	})
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	core := store.storeCore
	core.writeMu.Lock()
	_, err = core.writerDB.ExecContext(ctx, `
		UPDATE ready_generation_leases
		SET expires_at = unixepoch() + 1
		WHERE lease_token = ?
	`, claim.LeaseToken)
	core.writeMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	renewed, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key: key, LeaseToken: claim.LeaseToken,
	})
	if err != nil || !found {
		t.Fatalf("renew found=%v err=%v", found, err)
	}
	if renewed.WinnerGenerationID != generationID || renewed.LeaseToken != claim.LeaseToken || renewed.ExpiresAt <= claim.ExpiresAt-1 {
		t.Fatalf("renewed claim = %+v, original = %+v", renewed, claim)
	}
	var leaseRows int
	if err := core.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ready_generation_leases WHERE lease_token = ?
	`, claim.LeaseToken).Scan(&leaseRows); err != nil {
		t.Fatal(err)
	}
	if leaseRows != 1 {
		t.Fatalf("lease rows=%d, want one idempotent row", leaseRows)
	}
	if err := catalog.DeleteViewGeneration(ctx, generationID); err == nil {
		t.Fatal("leased generation was deleted before route handoff")
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := catalog.DeleteViewGeneration(ctx, generationID); err != nil {
		t.Fatalf("delete after lease release: %v", err)
	}
}

func TestReadyGenerationCacheExpiredLeaseDoesNotBlockDeletion(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := store.Catalog()
	key := readyCacheTestKey("graph-expired-lease", 0)
	generationID := createReadyCacheGeneration(t, catalog, key, "checkout", "checkout", "expired", "expired")
	claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
		Key: key, LeaseToken: "expired-handoff",
	})
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	core := store.storeCore
	core.writeMu.Lock()
	_, err = core.writerDB.ExecContext(ctx, `
		UPDATE ready_generation_leases
		SET expires_at = unixepoch() - 1
		WHERE lease_token = ?
	`, claim.LeaseToken)
	core.writeMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.DeleteViewGeneration(ctx, generationID); err != nil {
		t.Fatalf("delete with expired lease: %v", err)
	}
	var leaseRows int
	if err := core.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ready_generation_leases WHERE lease_token = ?
	`, claim.LeaseToken).Scan(&leaseRows); err != nil {
		t.Fatal(err)
	}
	if leaseRows != 0 {
		t.Fatalf("expired lease rows after generation delete=%d, want cascade cleanup", leaseRows)
	}
}

func TestReadyGenerationCacheConcurrentPublishersAdoptOneWinner(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := store.Catalog()
	key := readyCacheTestKey("graph-race", 0)
	first := createReadyCacheGeneration(t, catalog, key, "checkout", "checkout-a", "race-a", "race-a")
	second := createReadyCacheGeneration(t, catalog, key, "ref_view", "", "race-b", "race-b")

	const contenders = 16
	claims := make(chan ReadyGenerationClaim, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		candidate := first
		if i%2 == 1 {
			candidate = second
		}
		wg.Add(1)
		go func(i int, candidate int64) {
			defer wg.Done()
			claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
				Key: key, CandidateGenerationID: candidate,
				LeaseToken: fmt.Sprintf("racer-%d", i),
			})
			if err != nil {
				errs <- err
				return
			}
			if !found {
				errs <- fmt.Errorf("racer %d missed", i)
				return
			}
			claims <- claim
		}(i, candidate)
	}
	wg.Wait()
	close(claims)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for claim := range claims {
		if claim.WinnerGenerationID != first {
			t.Errorf("winner=%d, want %d", claim.WinnerGenerationID, first)
		}
		if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
			t.Error(err)
		}
	}
	row, ok, err := catalog.GetViewGeneration(ctx, second)
	if err != nil || !ok {
		t.Fatalf("read redundant generation ok=%v err=%v", ok, err)
	}
	if row.State != ViewGenerationSuperseded {
		t.Fatalf("redundant state=%q, want superseded", row.State)
	}
}

func TestReadyGenerationClaimPrefersLiveLeasedWinner(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	key := readyCacheTestKey("graph-live-pinned", 0)
	older := createBuildingReadyCacheGeneration(t, catalog, key,
		"ref_view", "", "older-building", "older")
	pinned := createReadyCacheGeneration(t, catalog, key,
		"ref_view", "", "newer-ready", "newer")
	pin := claimReadyCacheSourceGeneration(t, catalog, key, pinned)

	publishReadyCacheGeneration(t, catalog, older)
	claim := claimReadyCacheSourceGeneration(t, catalog, key, older)
	if claim.WinnerGenerationID != pinned || !claim.Reused || !claim.RetiredCandidate {
		t.Fatalf("claim = %+v, want leased winner %d and retired candidate %d", claim, pinned, older)
	}
	assertReadyCacheGenerationState(t, catalog, older, ViewGenerationSuperseded)
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatalf("release adopted claim: %v", err)
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, pin.LeaseToken); err != nil {
		t.Fatalf("release pinned claim: %v", err)
	}
}

func TestReadyGenerationClaimPrefersDurablyBoundWinner(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	key := readyCacheTestKey("graph-durable-pinned", 0)
	view, build := seedReadyCacheRefView(t, catalog, key, "durable-pinned")
	older := createBuildingReadyCacheGeneration(t, catalog, key,
		"ref_view", "", "older-building", "older")
	pinned := createReadyCacheGeneration(t, catalog, key,
		"ref_view", "", "newer-ready", "newer")
	pin := claimReadyCacheSourceGeneration(t, catalog, key, pinned)
	if err := catalog.BindReadyGenerationLeaseToRefView(ctx,
		bindReadyCacheRefViewRequest(view, build, key, pin)); err != nil {
		t.Fatalf("bind pinned generation: %v", err)
	}

	publishReadyCacheGeneration(t, catalog, older)
	claim := claimReadyCacheSourceGeneration(t, catalog, key, older)
	if claim.WinnerGenerationID != pinned || !claim.Reused || !claim.RetiredCandidate {
		t.Fatalf("claim = %+v, want bound winner %d and retired candidate %d", claim, pinned, older)
	}
	assertReadyCacheGenerationState(t, catalog, older, ViewGenerationSuperseded)
	stored, found, err := catalog.GetRefView(ctx, view.RefViewID)
	if err != nil || !found {
		t.Fatalf("read pinned ref view: found=%v err=%v", found, err)
	}
	if stored.ActiveGenerationID != pinned {
		t.Fatalf("active generation=%d, want pinned winner %d", stored.ActiveGenerationID, pinned)
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatalf("release adopted claim: %v", err)
	}
}

func TestReadyGenerationClaimBypassesWithdrawnPinnedWinner(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	key := readyCacheTestKey("graph-withdrawn-pinned", 0)
	view, build := seedReadyCacheRefView(t, catalog, key, "withdrawn-pinned")
	older := createBuildingReadyCacheGeneration(t, catalog, key,
		"ref_view", "", "older-building", "older")
	pinned := createReadyCacheGeneration(t, catalog, key,
		"ref_view", "", "newer-ready", "newer")
	pin := claimReadyCacheSourceGeneration(t, catalog, key, pinned)
	if err := catalog.BindReadyGenerationLeaseToRefView(ctx,
		bindReadyCacheRefViewRequest(view, build, key, pin)); err != nil {
		t.Fatalf("bind pinned generation: %v", err)
	}
	if err := catalog.WithdrawProducer(ctx, pinned,
		readyGenerationSourceSnapshotCapability, "test withdrawal"); err != nil {
		t.Fatalf("withdraw pinned source snapshot: %v", err)
	}

	publishReadyCacheGeneration(t, catalog, older)
	claim := claimReadyCacheSourceGeneration(t, catalog, key, older)
	if claim.WinnerGenerationID != older || claim.Reused || claim.CapabilityMiss {
		t.Fatalf("claim = %+v, want compatible candidate %d", claim, older)
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatalf("release replacement claim: %v", err)
	}
}

func TestReadyGenerationClaimBypassesExpiredLease(t *testing.T) {
	ctx := context.Background()
	store := openCatalogStore(t)
	catalog := store.Catalog()
	key := readyCacheTestKey("graph-expired-pinned", 0)
	older := createBuildingReadyCacheGeneration(t, catalog, key,
		"ref_view", "", "older-building", "older")
	pinned := createReadyCacheGeneration(t, catalog, key,
		"ref_view", "", "newer-ready", "newer")
	pin := claimReadyCacheSourceGeneration(t, catalog, key, pinned)
	core := store.storeCore
	core.writeMu.Lock()
	_, err := core.writerDB.ExecContext(ctx, `
		UPDATE ready_generation_leases
		SET expires_at = unixepoch() - 1
		WHERE lease_token = ?
	`, pin.LeaseToken)
	core.writeMu.Unlock()
	if err != nil {
		t.Fatalf("expire pinned lease: %v", err)
	}

	publishReadyCacheGeneration(t, catalog, older)
	claim := claimReadyCacheSourceGeneration(t, catalog, key, older)
	if claim.WinnerGenerationID != older || claim.Reused {
		t.Fatalf("claim = %+v, want oldest candidate %d after lease expiry", claim, older)
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
		t.Fatalf("release replacement claim: %v", err)
	}
	if err := catalog.ReleaseReadyGenerationLease(ctx, pin.LeaseToken); err != nil {
		t.Fatalf("release expired claim: %v", err)
	}
}

func TestReadyGenerationCacheKeyUsesEveryCompatibilityField(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog := store.Catalog()
	key := readyCacheTestKey("graph-fields", 0)
	createReadyCacheGeneration(t, catalog, key, "checkout", "checkout", "fields", "fields")

	variants := map[string]ReadyGenerationCacheKey{}
	graph := key
	graph.GraphID = "graph-other"
	variants["graph"] = graph
	base := key
	base.BaseGenerationID = 999999
	variants["base"] = base
	tree := key
	tree.TreeOID = "tree-other"
	variants["tree"] = tree
	config := key
	config.IndexConfigHash = "config-other"
	variants["config"] = config
	extractor := key
	extractor.ExtractorFingerprint = "extractor-other"
	variants["extractor"] = extractor
	epoch := key
	epoch.SchemaPipelineEpoch = "epoch-other"
	variants["epoch"] = epoch

	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{Key: variant})
			if err != nil {
				t.Fatal(err)
			}
			if found {
				t.Fatalf("incompatible key reused %+v", claim)
			}
		})
	}
}

func BenchmarkReadyGenerationCache(b *testing.B) {
	ctx := context.Background()
	b.Run("ReadyHitWithLease", func(b *testing.B) {
		store, err := Open(filepath.Join(b.TempDir(), "store.db"))
		if err != nil {
			b.Fatal(err)
		}
		defer store.Close()
		catalog := store.Catalog()
		key := readyCacheTestKey("graph-bench", 0)
		createReadyCacheGeneration(b, catalog, key, "checkout", "checkout", "bench", "bench")
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			token := fmt.Sprintf("bench-%d", i)
			claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
				Key: key, LeaseToken: token,
			})
			if err != nil || !found {
				b.Fatalf("claim found=%v err=%v", found, err)
			}
			b.StopTimer()
			if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
	})
	b.Run("PinnedWinner", func(b *testing.B) {
		store, err := Open(filepath.Join(b.TempDir(), "store.db"))
		if err != nil {
			b.Fatal(err)
		}
		defer store.Close()
		catalog := store.Catalog()
		key := readyCacheTestKey("graph-pinned-bench", 0)
		older := make([]int64, 8)
		for i := range older {
			older[i] = createBuildingReadyCacheGeneration(b, catalog, key,
				"ref_view", "", fmt.Sprintf("older-%d", i), "older")
		}
		pinned := createReadyCacheGeneration(b, catalog, key,
			"ref_view", "", "pinned", "pinned")
		pin, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
			Key:                  key,
			LeaseToken:           "benchmark-pin",
			RequiredCapabilities: []string{readyGenerationSourceSnapshotCapability},
		})
		if err != nil || !found || pin.WinnerGenerationID != pinned {
			b.Fatalf("pin claim = %+v found=%v err=%v, want %d", pin, found, err, pinned)
		}
		defer func() {
			if err := catalog.ReleaseReadyGenerationLease(ctx, pin.LeaseToken); err != nil {
				b.Errorf("release pin: %v", err)
			}
		}()
		for _, generationID := range older {
			publishReadyCacheGeneration(b, catalog, generationID)
		}

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			token := fmt.Sprintf("pinned-bench-%d", i)
			claim, found, err := catalog.ClaimReadyGeneration(ctx, ClaimReadyGenerationRequest{
				Key:                  key,
				LeaseToken:           token,
				RequiredCapabilities: []string{readyGenerationSourceSnapshotCapability},
			})
			if err != nil || !found || claim.WinnerGenerationID != pinned {
				b.Fatalf("claim = %+v found=%v err=%v, want pinned winner %d", claim, found, err, pinned)
			}
			b.StopTimer()
			if err := catalog.ReleaseReadyGenerationLease(ctx, claim.LeaseToken); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
	})
	b.Run("ColdPayload100Nodes", func(b *testing.B) {
		store, err := Open(filepath.Join(b.TempDir(), "store.db"))
		if err != nil {
			b.Fatal(err)
		}
		defer store.Close()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			generationID, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
				OwnerKind:         "checkout",
				GraphID:           "graph-bench",
				LayerID:           fmt.Sprintf("cold-%d", i),
				CheckoutID:        "checkout",
				GenerationKind:    "commit",
				TreeOID:           fmt.Sprintf("tree-%d", i),
				ConfigHash:        "config-hash",
				ExtractorVersions: "extractor-fingerprint",
				ResolverVersion:   "pipeline-epoch",
				CreatedAt:         10,
			})
			if err != nil {
				b.Fatal(err)
			}
			nodes := make([]*graph.Node, 100)
			for j := range nodes {
				nodes[j] = &graph.Node{
					ID:         fmt.Sprintf("node-%d-%d", i, j),
					Kind:       graph.KindFunction,
					Name:       fmt.Sprintf("Function%d", j),
					QualName:   fmt.Sprintf("bench.Function%d", j),
					FilePath:   fmt.Sprintf("file-%d.go", j/10),
					StartLine:  j + 1,
					Language:   "go",
					RepoPrefix: "bench",
				}
			}
			handle.AddBatch(nodes, nil)
			if err := store.PublishPayloadGeneration(ctx, generationID, 11); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func readyCacheTestKey(graphID string, baseGenerationID int64) ReadyGenerationCacheKey {
	return ReadyGenerationCacheKey{
		GraphID:              graphID,
		BaseGenerationID:     baseGenerationID,
		TreeOID:              "tree-oid",
		IndexConfigHash:      "config-hash",
		ExtractorFingerprint: "extractor-fingerprint",
		SchemaPipelineEpoch:  "pipeline-epoch",
	}
}

func createReadyCacheGeneration(
	t testing.TB,
	catalog *Catalog,
	key ReadyGenerationCacheKey,
	ownerKind, checkoutID, layerID, provenance string,
) int64 {
	t.Helper()
	generationID := createBuildingReadyCacheGeneration(t, catalog, key,
		ownerKind, checkoutID, layerID, provenance)
	publishReadyCacheGeneration(t, catalog, generationID)
	return generationID
}

func createBuildingReadyCacheGeneration(
	t testing.TB,
	catalog *Catalog,
	key ReadyGenerationCacheKey,
	ownerKind, checkoutID, layerID, provenance string,
) int64 {
	t.Helper()
	ctx := context.Background()
	generationID, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind:           ownerKind,
		GraphID:             key.GraphID,
		LayerID:             layerID,
		CheckoutID:          checkoutID,
		GenerationKind:      "commit",
		BaseGenerationID:    key.BaseGenerationID,
		TreeOID:             key.TreeOID,
		ProvenanceCommitOID: provenance,
		ConfigHash:          key.IndexConfigHash,
		ExtractorVersions:   key.ExtractorFingerprint,
		ResolverVersion:     key.SchemaPipelineEpoch,
		State:               ViewGenerationBuilding,
		CreatedAt:           10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.store.AtGeneration(generationID).SetProducerState(ProducerCompleteness{
		Producer: readyGenerationSourceSnapshotCapability,
		State:    ProducerStateComplete,
	}); err != nil {
		t.Fatal(err)
	}
	return generationID
}

func publishReadyCacheGeneration(t testing.TB, catalog *Catalog, generationID int64) {
	t.Helper()
	if err := catalog.PublishViewGeneration(context.Background(), generationID, 11); err != nil {
		t.Fatal(err)
	}
}

func assertReadyCacheGenerationState(t testing.TB, catalog *Catalog, generationID int64, want ViewGenerationState) {
	t.Helper()
	row, found, err := catalog.GetViewGeneration(context.Background(), generationID)
	if err != nil || !found {
		t.Fatalf("read generation %d: found=%v err=%v", generationID, found, err)
	}
	if row.State != want {
		t.Fatalf("generation %d state=%q, want %q", generationID, row.State, want)
	}
}

func assertReadyCacheWinner(t testing.TB, catalog *Catalog, key ReadyGenerationCacheKey, want int64, token string) {
	t.Helper()
	claim, found, err := catalog.ClaimReadyGeneration(context.Background(), ClaimReadyGenerationRequest{
		Key: key, LeaseToken: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found || claim.WinnerGenerationID != want {
		t.Fatalf("claim=%+v found=%v, want winner=%d", claim, found, want)
	}
	if err := catalog.ReleaseReadyGenerationLease(context.Background(), claim.LeaseToken); err != nil {
		t.Fatal(err)
	}
}
