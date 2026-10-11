package indexer

import (
	"context"
	"encoding/json"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestAsyncContractProducerRestagesLegacyGenerationIdentity(t *testing.T) {
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
	legacy := extractorVersionsFingerprint()
	encoded, err := json.Marshal(extractorVersionsSnapshot())
	if err != nil || legacy != string(encoded) {
		t.Fatalf("disabled identity changed: %q err=%v", legacy, err)
	}
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "1")
	builder := &SparseGenerationBuilder{}
	if builder.extractorVersionsFingerprint() != legacy {
		t.Fatal("environment enabled an uninstalled producer")
	}
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	current := builder.extractorVersionsFingerprint()
	if current == legacy || current != builder.extractorVersionsFingerprint() {
		t.Fatal("enabled producer identity must be distinct and stable")
	}
	identity := GenerationIdentity{ExtractorVersions: current}
	old := store_sqlite.ViewGeneration{ExtractorVersions: legacy}
	if sameCommitLayerInputs(old, identity) {
		t.Fatal("legacy commit layer remained reusable under async producer")
	}
	old.ExtractorVersions = current
	if !sameCommitLayerInputs(old, identity) {
		t.Fatal("rebuilt layer with identical producer inputs was rejected")
	}
	versions := extractorVersionsSnapshot()
	_ = contractGenerationProducerVersions(versions, true)
	if _, mutated := versions[contractAsyncGenerationProducerKey]; mutated {
		t.Fatal("producer policy mutated ordinary extractor version snapshot")
	}
}

func TestAsyncContractProducerIdentityIsCapturedByRealCoordinatorAndRefManager(t *testing.T) {
	t.Setenv("GORTEX_ASYNC_CONTRACTS", "1")
	fixture := newCoordinatorFixture(t)
	builder := builderNewBuilder(fixture.store)
	legacy := extractorVersionsFingerprint()
	uninstalled := fixture.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	require.Equal(t, legacy, uninstalled.extractors)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
	installed := fixture.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	want := builder.extractorVersionsFingerprint()
	require.NotEqual(t, legacy, want)
	require.Equal(t, legacy, uninstalled.builder.extractorVersionsFingerprint(), "installing the caller's builder must not retarget the old owner")
	require.Equal(t, want, installed.extractors)
	require.Equal(t, want, installed.cohort.ExtractorVersions)
	require.Equal(t, want, installed.dirtyIdentity(fixture.graphID, 0).ExtractorVersions)
	manager, err := NewRefViewManager(RefViewManagerConfig{Store: fixture.store, Builder: builder, Config: config.Default().Index})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	require.Equal(t, want, manager.extractors)
}

func TestAsyncContractProducerInstallationRefusesFrozenCoordinatorIdentity(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	coordinator := fixture.inertCoordinator(t, CheckoutCoordinatorConfig{})
	lifecycle := &CheckoutLifecycle{coordinators: map[string]*CheckoutCoordinator{fixture.checkoutID: coordinator}}
	before := coordinator.extractors
	require.Error(t, lifecycle.SetContractCoreRuntime(ContractCoreRuntimeHooks{}))
	require.Nil(t, lifecycle.contractCoreRuntime.Load())
	require.Nil(t, coordinator.builder.contractCoreRuntime)
	require.Equal(t, before, coordinator.extractors)
}

func TestAsyncContractProducerBuilderCopiesKeepInstalledOwnerState(t *testing.T) {
	fixture := newCoordinatorFixture(t)
	builder := builderNewBuilder(fixture.store)
	legacyCopy := *builder
	callbacks := make(chan string, 4)
	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: func(context.Context, string, string) { callbacks <- "old" }})
	installedCopy := *builder
	observation := dedicatedBaseObservation{Builder: installedCopy}
	oldOwner := fixture.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	manager, err := NewRefViewManager(RefViewManagerConfig{Store: fixture.store, Builder: builder, Config: config.Default().Index})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close()) })
	want := builder.extractorVersionsFingerprint()

	builder.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: func(context.Context, string, string) { callbacks <- "new" }})
	newOwner := fixture.inertCoordinator(t, CheckoutCoordinatorConfig{Builder: builder})
	require.Nil(t, legacyCopy.contractCoreRuntime, "a pre-install copy stays legacy")
	require.Equal(t, extractorVersionsFingerprint(), legacyCopy.extractorVersionsFingerprint())
	for _, owned := range []*SparseGenerationBuilder{&observation.Builder, oldOwner.builder, manager.builder} {
		require.Equal(t, want, owned.extractorVersionsFingerprint())
		owned.contractCoreRuntime.Published(t.Context(), "repo", "checkout")
		require.Equal(t, "old", <-callbacks, "copy and owner must retain the actual installed callback")
	}
	require.Equal(t, want, oldOwner.extractors)
	require.Equal(t, want, manager.extractors)
	require.Equal(t, want, newOwner.extractors)
	newOwner.builder.contractCoreRuntime.Published(t.Context(), "repo", "checkout")
	require.Equal(t, "new", <-callbacks)
}
