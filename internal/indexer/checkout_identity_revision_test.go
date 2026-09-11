package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graphview"
	"go.uber.org/zap"
)

// The dependency revision is the cohort half of a checkout layer's identity:
// the digest of every resolver-visible input the payload is a function of but
// that no other identity column names — the repository roster, what the build
// can see of each member's bytes, the ownership evidence, the configuration
// domains outside config.IndexConfig, the producer policy and the capability
// vocabulary it is stated over.
//
// The tests here pin three things, in order of how much they matter:
//
//  1. the revision REACHES the stored generation row (the wiring claim);
//  2. every cohort dimension moves it, and nothing else does;
//  3. a cohort that cannot be described fails CLOSED — onto a revision nothing
//     can reuse, never onto the empty revision the reuse guards read as
//     "matches anything".

// cohortCoordinatorConfig is what a coordinator the lifecycle built looks like:
// a frozen index configuration plus the named configuration domains. A
// coordinator missing the sections is refused by the producer, which is the
// refusal TestRefusedCheckoutCohortIsNotReusable drives on purpose.
func cohortCoordinatorConfig() CheckoutCoordinatorConfig {
	return CheckoutCoordinatorConfig{
		ConfigSections: dedicatedBaseConfigSections(config.Default()),
	}
}

// TestCheckoutLayerIdentitiesCarryTheDependencyCohortRevision is the wiring
// claim: a real reconcile cycle, through the production entrypoint, stores both
// layers with the cohort revision the coordinator computed. Before the binding
// both rows carried an empty dependency_revision, which is the value the
// catalog's reuse guards treat as matching every candidate.
func TestCheckoutLayerIdentitiesCarryTheDependencyCohortRevision(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	worktreeWrite(t, f.worktree, "island.go", "package fixture\n\nfunc Island() {}\n")

	c := f.inertCoordinator(t, cohortCoordinatorConfig())
	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle did not route both layers: %+v", cycle)
	}

	revision := c.dependencyRevision()
	if !strings.HasPrefix(revision, DependencyRevisionEncodingVersion+":") {
		t.Fatalf("the coordinator's revision %q is not a computed cohort revision; "+
			"the production cohort was refused", revision)
	}
	for _, named := range []struct {
		what         string
		generationID int64
	}{
		{"commit layer", cycle.CommitGenerationID},
		{"working-tree layer", cycle.DirtyGenerationID},
	} {
		row, found := f.generation(named.generationID)
		if !found {
			t.Fatalf("%s generation %d is not in the catalog", named.what, named.generationID)
		}
		if row.DependencyRevision != revision {
			t.Errorf("%s stored dependency_revision %q, the coordinator's cohort is %q",
				named.what, row.DependencyRevision, revision)
		}
	}
}

// TestCheckoutCohortDimensionsMoveTheIdentity walks the production cohort — the
// one the coordinator assembles for a real build — and changes one dimension at
// a time. Each is a different set of inputs, so each must be a different
// revision; a dimension that does not move it is a dimension a stale payload
// can be reused across.
func TestCheckoutCohortDimensionsMoveTheIdentity(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, cohortCoordinatorConfig())

	lease, err := f.leases.AcquireRepositoryRoster()
	if err != nil {
		t.Fatalf("acquire the roster: %v", err)
	}
	defer lease.Release()
	base, err := c.cohort.inputs(context.Background(), lease)
	if err != nil {
		t.Fatalf("assemble the production cohort: %v", err)
	}
	baseline, err := ComputeDependencyRevision(base)
	if err != nil {
		t.Fatalf("digest the production cohort: %v", err)
	}
	if len(base.Repositories) == 0 || len(base.Ownership) == 0 ||
		len(base.ConfigSections) == 0 || len(base.Producers) == 0 ||
		len(base.Capabilities) == 0 || base.ExtractorVersions == "" {
		t.Fatalf("the production cohort left a dimension empty: %+v", base)
	}

	clone := func() DependencyRevisionInputs {
		out := base
		out.Repositories = append([]DependencyRevisionRepository(nil), base.Repositories...)
		out.Ownership = append([]DependencyRevisionOwnership(nil), base.Ownership...)
		out.ConfigSections = append([]DependencyRevisionConfigSection(nil), base.ConfigSections...)
		out.Producers = append([]DependencyRevisionProducer(nil), base.Producers...)
		out.Capabilities = append([]string(nil), base.Capabilities...)
		return out
	}

	for _, tc := range []struct {
		name   string
		mutate func(*DependencyRevisionInputs)
	}{
		{"a repository joins the roster", func(in *DependencyRevisionInputs) {
			in.Repositories = append(in.Repositories, DependencyRevisionRepository{
				RepoPrefix:     "sibling",
				Kind:           DependencyRevisionRepositoryDedicated,
				GraphID:        "graph-sibling",
				CheckoutID:     "checkout-sibling",
				Incarnation:    "incarnation-sibling",
				SourceIdentity: "tree:sibling",
			})
		}},
		{"a roster member's source moves", func(in *DependencyRevisionInputs) {
			in.Repositories[0].SourceIdentity += "-moved"
		}},
		{"a roster member is re-admitted", func(in *DependencyRevisionInputs) {
			in.Repositories[0].Incarnation += "-2"
		}},
		{"the ownership evidence gains a source", func(in *DependencyRevisionInputs) {
			in.Ownership = append(in.Ownership, DependencyRevisionOwnership{
				RepoPrefix: in.Repositories[0].RepoPrefix,
				Language:   "typescript",
				Owner:      "tsconfig-paths",
			})
		}},
		{"a named configuration domain changes", func(in *DependencyRevisionInputs) {
			in.ConfigSections[0].Digest += "-changed"
		}},
		{"the index configuration changes", func(in *DependencyRevisionInputs) {
			index := in.Config
			index.MaxFileSize = index.MaxFileSize + 1
			in.Config = index
		}},
		{"the producer policy changes", func(in *DependencyRevisionInputs) {
			in.Producers[0].State += "-changed"
		}},
		{"the producer policy's wording changes", func(in *DependencyRevisionInputs) {
			in.Producers[len(in.Producers)-1].Reason += " (reworded)"
		}},
		{"the capability vocabulary loses a member", func(in *DependencyRevisionInputs) {
			in.Capabilities = in.Capabilities[:len(in.Capabilities)-1]
		}},
		{"an extractor version is bumped", func(in *DependencyRevisionInputs) {
			in.ExtractorVersions += "+"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := clone()
			tc.mutate(&mutated)
			revision, err := ComputeDependencyRevision(mutated)
			if err != nil {
				t.Fatalf("digest the mutated cohort: %v", err)
			}
			if revision == baseline {
				t.Fatalf("%s left the revision at %q; that input is not in the cohort",
					tc.name, revision)
			}
			// The revision is a whole identity column, so a moved cohort has to
			// move the identity key the reuse guards compare.
			before := generationIdentityKey(GenerationIdentity{DependencyRevision: baseline})
			after := generationIdentityKey(GenerationIdentity{DependencyRevision: revision})
			if before == after {
				t.Fatalf("%s moved the revision but not the identity key", tc.name)
			}
		})
	}
}

// TestCheckoutCohortIsStableAcrossTwoConstructions is the no-op half: unchanged
// inputs must produce a byte-identical identity. A revision that carried a
// timestamp, a map iteration order or a process-local value would invalidate
// every cached layer on every daemon restart, which is the write amplification
// the cohort exists to bound.
func TestCheckoutCohortIsStableAcrossTwoConstructions(t *testing.T) {
	f := newCoordinatorFixture(t)

	first := f.inertCoordinator(t, cohortCoordinatorConfig())
	second := f.inertCoordinator(t, cohortCoordinatorConfig())

	if got := first.dependencyRevision(); !strings.HasPrefix(got, DependencyRevisionEncodingVersion+":") {
		t.Fatalf("the first coordinator's cohort was refused: %q", got)
	}
	if first.dependencyRevision() != second.dependencyRevision() {
		t.Fatalf("two coordinators over identical inputs disagree:\n first  %q\n second %q",
			first.dependencyRevision(), second.dependencyRevision())
	}
	if first.configHash != second.configHash {
		t.Fatalf("two coordinators over identical configuration disagree: %q vs %q",
			first.configHash, second.configHash)
	}

	base := primaryBase{graphID: f.graphID, treeOID: f.treeA}
	if a, b := generationIdentityKey(first.commitIdentity(base, f.treeA)),
		generationIdentityKey(second.commitIdentity(base, f.treeA)); a != b {
		t.Fatalf("identical inputs produced two commit identities:\n %q\n %q", a, b)
	}
	if a, b := generationIdentityKey(first.dirtyIdentity(f.graphID, 7)),
		generationIdentityKey(second.dirtyIdentity(f.graphID, 7)); a != b {
		t.Fatalf("identical inputs produced two dirty identities:\n %q\n %q", a, b)
	}
}

// TestRefusedCheckoutCohortIsNotReusable pins the fail-closed direction.
//
// A coordinator whose cohort cannot be described — here, a lease manager with
// no registered roster — must not fall back to the empty revision. Empty is the
// LEGACY identity: generationIdentityKey renders it byte-for-byte as a
// pre-cohort key, so every stored generation from before this change would
// match it.
//
// What it falls back to is the STABLE degraded revision: outside the certified
// vocabulary, so no certified layer is ever reused for an undescribable cohort
// or the other way round, and identical for two producers over identical
// describable inputs. Stability is the half W2.4 changed: the first
// implementation minted `cohort-refused:<nanoseconds>` per coordinator, and a
// value nothing can ever match again means every layer stamped with one is
// rebuilt on every following cycle for as long as the transient lasts — the
// write amplification the revision exists to bound. The cost of stability is
// declared in the degraded revision's own doc comment: two degraded builds
// whose describable inputs match are treated as one.
func TestRefusedCheckoutCohortIsNotReusable(t *testing.T) {
	f := newCoordinatorFixture(t)

	// A lease manager with no registered repository: the roster enumerator has
	// nothing to describe, which is the shape every incomplete cohort takes.
	refused := func() *CheckoutCoordinator {
		t.Helper()
		c, err := NewCheckoutCoordinator(CheckoutCoordinatorConfig{
			CheckoutID:     f.checkoutID,
			CheckoutRoot:   f.worktree,
			FamilyID:       f.familyID,
			RepoPrefix:     builderRepoPrefix,
			WorkspaceID:    builderRepoPrefix,
			ProjectID:      builderRepoPrefix,
			Store:          f.store,
			Builder:        builderNewBuilder(f.store),
			Leases:         graphview.NewLeaseManager(),
			Config:         config.Default().Index,
			ConfigSections: dedicatedBaseConfigSections(config.Default()),
			Logger:         zap.NewNop(),
			PollInterval:   -1,
		})
		if err != nil {
			t.Fatalf("NewCheckoutCoordinator: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	first, second := refused(), refused()

	revision := first.dependencyRevision()
	switch {
	case revision == "":
		t.Fatal("a refused cohort fell back to the empty (legacy) revision, " +
			"which every stored generation matches")
	case strings.HasPrefix(revision, DependencyRevisionEncodingVersion+":"):
		t.Fatalf("a refused cohort produced a certificate: %q", revision)
	case !strings.HasPrefix(revision, DependencyRevisionDegradedPrefix+":"):
		t.Fatalf("a refused cohort produced %q, which names neither vocabulary", revision)
	}
	if second.dependencyRevision() != revision {
		t.Fatalf("two refused coordinators over identical inputs disagree:\n first  %q\n second %q\n"+
			"a per-coordinator value can never be matched again, so every layer stamped with "+
			"one is rebuilt on the next cycle", revision, second.dependencyRevision())
	}

	// Stable within the coordinator: the refusal must not re-mint on every read,
	// or the coordinator's own reuse cache never hits and every cycle rebuilds.
	if again := first.dependencyRevision(); again != revision {
		t.Fatalf("the refusal re-minted itself: %q then %q", revision, again)
	}
	first.describeDependencyCohort(context.Background())
	if again := first.dependencyRevision(); again != revision {
		t.Fatalf("a refreshed refusal re-minted itself: %q then %q", revision, again)
	}
	if first.dependencyCohortDescribable() {
		t.Fatal("a coordinator with no roster reported a describable cohort")
	}

	// And it is a different identity from the legacy one, at both layers.
	base := primaryBase{graphID: f.graphID, treeOID: f.treeA}
	legacy := first.commitIdentity(base, f.treeA)
	legacy.DependencyRevision = ""
	if generationIdentityKey(first.commitIdentity(base, f.treeA)) == generationIdentityKey(legacy) {
		t.Fatal("a refused cohort's commit identity equals the legacy identity")
	}
	dirtyLegacy := first.dirtyIdentity(f.graphID, 3)
	dirtyLegacy.DependencyRevision = ""
	if generationIdentityKey(first.dirtyIdentity(f.graphID, 3)) == generationIdentityKey(dirtyLegacy) {
		t.Fatal("a refused cohort's working-tree identity equals the legacy identity")
	}
}

// TestCheckoutReuseRefusesALayerBuiltUnderAnotherCohort is the guard that makes
// the binding worth anything: the coordinator's own reuse comparison —
// generationRowKey against generationIdentityKey, the same comparison the
// catalog's ready-reuse lookup makes over the identity columns — must refuse a
// stored generation whose dependency revision is not this build's.
//
// It is driven through settledWithoutBuild, the polling path that decides
// whether a routed layer still describes the checkout. The poll settles while
// the cohort is what the build froze, and stops settling once a repository in
// this checkout's WORKSPACE joins the roster and the cohort is re-described:
// the routed payload was resolved against a corpus that no longer describes the
// inputs.
//
// The re-description is an explicit event rather than something the poll does
// for itself — see TestAnIdlePollDescribesNoCohort for why an idle poll must
// describe nothing — and the sibling is a real indexed repository rather than a
// name with no catalog row, so what moves the revision is a roster gaining a
// member and not a refusal.
func TestCheckoutReuseRefusesALayerBuiltUnderAnotherCohort(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()

	cfg := cohortCoordinatorConfig()
	workspace := newMutableWorkspace(builderRepoPrefix)
	cfg.WorkspaceMembers = workspace.seam()
	c := f.inertCoordinator(t, cfg)
	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle did not route both layers: %+v", cycle)
	}
	built := c.dependencyRevision()

	if _, settled := c.settledWithoutBuild(context.Background()); !settled {
		t.Fatal("the poll did not settle on the layers the cycle had just routed")
	}

	// A second repository joins this workspace's resolver-visible roster.
	// Nothing about this checkout's tree changed; what changed is what its
	// resolution can see.
	// Nothing tells the coordinator: the poll re-reads the workspace topology
	// itself, which is the production source for this staleness.
	registerCohortSibling(t, f, "workspace-sibling", "tree-workspace-sibling")
	workspace.add("workspace-sibling")

	if _, settled := c.settledWithoutBuild(context.Background()); settled {
		t.Fatal("the poll settled on a layer built under a different input cohort")
	}
	if c.dependencyRevision() == built {
		t.Fatal("a roster change left the cohort revision where it was")
	}

	// The refusal is the identity comparison, not a side effect: the stored row
	// and the wanted identity differ in exactly the revision.
	row, found := f.generation(cycle.CommitGenerationID)
	if !found {
		t.Fatalf("commit generation %d vanished", cycle.CommitGenerationID)
	}
	wanted := row
	wanted.DependencyRevision = c.dependencyRevision()
	if generationRowKey(row) == generationRowKey(wanted) {
		t.Fatal("two rows differing only in dependency_revision compare equal; " +
			"the revision is not part of the reuse key")
	}
}
