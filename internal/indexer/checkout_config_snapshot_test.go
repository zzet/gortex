package indexer

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

// A generation's configuration identity used to be indexConfigHash over
// config.IndexConfig alone, computed from the ConfigManager's SHALLOW
// GetRepoConfig result. Two things were wrong with that and both are pinned
// here: the configuration was shared with whoever could still change it, and
// the digest named only part of the configuration a payload is a function of.

// TestCoordinatorFreezesItsIndexConfiguration pins the ownership half. The
// caller's nested values must not be reachable from the coordinator after
// construction: GetRepoConfig hands back a struct copy whose
// FrameworkSynthesizers is a POINTER to the ConfigManager's own slice, so a
// coordinator that kept it would be building under a configuration a later
// reload can move underneath it.
func TestCoordinatorFreezesItsIndexConfiguration(t *testing.T) {
	f := newCoordinatorFixture(t)

	shared := []string{"gin", "echo"}
	index := config.Default().Index
	index.FrameworkSynthesizers = &shared

	// Constructed directly rather than through the fixture helper: that helper
	// overwrites Config with the default, and the caller's own nested values are
	// exactly what this test is about.
	c, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
		CheckoutID:     f.checkoutID,
		CheckoutRoot:   f.worktree,
		FamilyID:       f.familyID,
		RepoPrefix:     builderRepoPrefix,
		WorkspaceID:    builderRepoPrefix,
		ProjectID:      builderRepoPrefix,
		Store:          f.store,
		Builder:        builderNewBuilder(f.store),
		Leases:         f.leases,
		Config:         index,
		ConfigSections: dedicatedBaseConfigSections(config.Default()),
		Logger:         zap.NewNop(),
		PollInterval:   -1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	frozen := c.config.FrameworkSynthesizers
	require.NotNil(t, frozen, "the coordinator dropped an explicit synthesizer allow-list")
	require.Equal(t, []string{"gin", "echo"}, *frozen)

	// The live value moves, the way a configuration reload moves it.
	shared[0] = "chi"
	shared = append(shared, "fiber")
	index.FrameworkSynthesizers = &shared

	assert.Equal(t, []string{"gin", "echo"}, *c.config.FrameworkSynthesizers,
		"the coordinator's configuration followed the caller's slice; it is not frozen")
	assert.NotSame(t, &shared, c.config.FrameworkSynthesizers,
		"the coordinator kept the caller's pointer")
}

// TestBuildCoordinatorFreezesTheBuildersConfiguration is the production
// entrypoint for the same claim. buildCoordinator is what the lifecycle calls;
// the configuration it hands the SparseGenerationBuilder must be the
// coordinator's own, not values the ConfigManager still owns.
//
// EffectiveExclude is the observable: the manager memoizes it and returns the
// SAME backing array to every caller, and GetRepoConfig writes that shared slice
// straight into Index.Exclude. A snapshot owns a copy; a shallow copy does not.
func TestBuildCoordinatorFreezesTheBuildersConfiguration(t *testing.T) {
	f := newFamilyFixture(t, "frozen-config")
	defer f.close()
	ctx := context.Background()

	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)

	f.lc.dropCoordinator(f.automatic.CheckoutID)
	coordinator, err := f.lc.buildCoordinator(ctx, f.primaryGraph, checkout)
	require.NoError(t, err)
	require.NotNil(t, coordinator)
	defer func() { require.NoError(t, coordinator.Close()) }()

	shared := f.cm.EffectiveExclude(f.mainPrefix)
	require.NotEmpty(t, shared, "the fixture's repository has no effective exclude layer to observe")

	for _, owned := range []struct {
		what string
		list []string
	}{
		{"the builder's", coordinator.builder.Config.Exclude},
		{"the coordinator's", coordinator.config.Exclude},
	} {
		require.NotEmpty(t, owned.list, "%s configuration lost the exclude layer", owned.what)
		assert.Equal(t, shared, owned.list, "%s configuration changed the exclude layer", owned.what)
		if &shared[0] == &owned.list[0] {
			t.Errorf("%s configuration shares the ConfigManager's memoized exclude slice; "+
				"it is a shallow copy, not a snapshot", owned.what)
		}
	}

	// The same call is what supplies the named configuration domains, without
	// which the cohort is refused.
	require.NotEmpty(t, coordinator.configSections,
		"buildCoordinator supplied no configuration sections; every cohort it builds is refused")
	for _, domain := range dependencyRevisionRequiredConfigDomains {
		if !slices.ContainsFunc(coordinator.configSections,
			func(s DependencyRevisionConfigSection) bool { return s.Name == domain }) {
			t.Errorf("buildCoordinator did not declare the %q configuration domain", domain)
		}
	}
}

// TestDedicatedBaseConfigSectionsCoverEveryOutputAffectingDomain pins the
// widening itself: each domain outside config.IndexConfig that decides what a
// payload contains has to move its own digest when it moves.
func TestDedicatedBaseConfigSectionsCoverEveryOutputAffectingDomain(t *testing.T) {
	digestOf := func(sections []DependencyRevisionConfigSection, name string) string {
		for _, section := range sections {
			if section.Name == name {
				return section.Digest
			}
		}
		t.Fatalf("no %q section was declared", name)
		return ""
	}

	base := dedicatedBaseConfigSections(config.Default())
	for _, domain := range dependencyRevisionRequiredConfigDomains {
		require.NotEmpty(t, digestOf(base, domain), "domain %q has no digest", domain)
	}
	require.NotEmpty(t, digestOf(base, dedicatedBaseSourceSelectionSection))

	for _, tc := range []struct {
		domain string
		mutate func(*config.Config)
	}{
		{DependencyRevisionConfigArtifacts, func(c *config.Config) {
			c.Artifacts = append(c.Artifacts, config.ArtifactEntry{Path: "schema.sql"})
		}},
		{DependencyRevisionConfigLSP, func(c *config.Config) {
			c.Semantic.LSPMaxParallel = c.Semantic.LSPMaxParallel + 3
		}},
		{DependencyRevisionConfigProject, func(c *config.Config) {
			c.Project = "another-project"
		}},
		{DependencyRevisionConfigSemantic, func(c *config.Config) {
			c.Semantic.Enabled = !c.Semantic.Enabled
		}},
		{DependencyRevisionConfigWorkspace, func(c *config.Config) {
			c.Workspace = "another-workspace"
		}},
		{dedicatedBaseSourceSelectionSection, func(c *config.Config) {
			c.RuleFiles = append(c.RuleFiles, "rules/routes.toml")
		}},
	} {
		t.Run(tc.domain, func(t *testing.T) {
			moved := config.Default()
			tc.mutate(moved)
			got := dedicatedBaseConfigSections(moved)
			if digestOf(got, tc.domain) == digestOf(base, tc.domain) {
				t.Fatalf("moving the %q configuration left its digest unchanged", tc.domain)
			}
		})
	}
}

// TestCheckoutConfigHashCoversTheNamedSections pins the digest that carries
// them into the identity. A section that changes must move the hash; the
// caller's enumeration order must not.
func TestCheckoutConfigHashCoversTheNamedSections(t *testing.T) {
	sections := dedicatedBaseConfigSections(config.Default())
	baseline := checkoutConfigHash("fingerprint", sections)

	if checkoutConfigHash("fingerprint", nil) == baseline {
		t.Fatal("the named configuration domains do not reach the config digest")
	}
	if checkoutConfigHash("other-fingerprint", sections) == baseline {
		t.Fatal("the frozen index-configuration fingerprint does not reach the config digest")
	}

	shuffled := slices.Clone(sections)
	slices.Reverse(shuffled)
	if got := checkoutConfigHash("fingerprint", shuffled); got != baseline {
		t.Fatalf("the enumeration order entered the digest: %q vs %q", got, baseline)
	}

	moved := slices.Clone(sections)
	moved[0] = DependencyRevisionConfigSection{Name: moved[0].Name, Digest: moved[0].Digest + "!"}
	if checkoutConfigHash("fingerprint", moved) == baseline {
		t.Fatal("a changed section digest left the config digest unchanged")
	}

	renamed := slices.Clone(sections)
	renamed[0] = DependencyRevisionConfigSection{Name: renamed[0].Name + "-x", Digest: renamed[0].Digest}
	if checkoutConfigHash("fingerprint", renamed) == baseline {
		t.Fatal("a renamed section left the config digest unchanged; " +
			"name and digest are not separated in the encoding")
	}

	// The length-delimited encoding is what keeps "ab"/"c" apart from "a"/"bc".
	split := []DependencyRevisionConfigSection{{Name: "ab", Digest: "c"}}
	other := []DependencyRevisionConfigSection{{Name: "a", Digest: "bc"}}
	if checkoutConfigHash("fingerprint", split) == checkoutConfigHash("fingerprint", other) {
		t.Fatal("two distinct section sets collided; the encoding is not length-delimited")
	}
}

// TestCoordinatorConfigDigestReachesTheIdentity closes the loop: a coordinator
// whose named configuration differs must not reuse the other's layers.
func TestCoordinatorConfigDigestReachesTheIdentity(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.registerRosterOwner(t)

	plain := f.inertCoordinator(t, cohortCoordinatorConfig())

	withArtifacts := config.Default()
	withArtifacts.Artifacts = append(withArtifacts.Artifacts, config.ArtifactEntry{Path: "schema.sql"})
	other := f.inertCoordinator(t, CheckoutCoordinatorConfig{
		ConfigSections: dedicatedBaseConfigSections(withArtifacts),
	})

	if plain.configHash == other.configHash {
		t.Fatal("a changed artifacts configuration left the generation's config hash where it was")
	}
	base := primaryBase{graphID: f.graphID, treeOID: f.treeA}
	if generationIdentityKey(plain.commitIdentity(base, f.treeA)) ==
		generationIdentityKey(other.commitIdentity(base, f.treeA)) {
		t.Fatal("two configurations produced one commit identity")
	}
	if plain.dependencyRevision() == other.dependencyRevision() {
		t.Fatal("a changed configuration domain left the dependency cohort where it was")
	}
}

// TestUnencodableConfigurationStaysItsOwnIdentity pins the fail-safe
// indexConfigHash always had and this widening preserves: when the frozen
// snapshot cannot be fingerprinted the constructor hands checkoutConfigHash a
// unique token, and a unique token must produce a unique digest rather than one
// some stored generation shares.
func TestUnencodableConfigurationStaysItsOwnIdentity(t *testing.T) {
	sections := dedicatedBaseConfigSections(config.Default())
	first := checkoutConfigHash("unhashable-aaaaaaaa", sections)
	second := checkoutConfigHash("unhashable-bbbbbbbb", sections)
	if first == second {
		t.Fatal("two unencodable configurations share one digest; such a build reuses another's payload")
	}
	if first == checkoutConfigHash("", sections) {
		t.Fatal("an unencodable configuration digests as an empty fingerprint would")
	}
}
