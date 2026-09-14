package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// TestDedicatedBaseAncestryBoundsAreOrdered pins the three numbers that make
// "bounded ancestry" a property rather than a coincidence.
//
// The allocation policy, the read-time bound and the catalog's hard limit live
// in three packages, and the invariant only holds while they stay ordered:
// whatever the publisher is willing to allocate, the materializer must be
// willing to compose, and both must stop well short of the limit the catalog
// refuses at — because the catalog's refusal arrives as a failed publication,
// not as a re-root.
//
// The hard limit is unexported in store_sqlite, so it is read out of that file
// rather than guessed. That is deliberate: a test that restated the number as
// its own literal would pass forever after someone changed the real one.
func TestDedicatedBaseAncestryBoundsAreOrdered(t *testing.T) {
	if maxDedicatedBaseDeltaAncestors != graphview.MaxDedicatedBaseChainDepth {
		t.Fatalf("allocation policy=%d, want the read-side bound graphview.MaxDedicatedBaseChainDepth=%d",
			maxDedicatedBaseDeltaAncestors, graphview.MaxDedicatedBaseChainDepth)
	}
	// One checkout commit generation legitimately stands on a dedicated head,
	// so the composed walk is one deeper than the chain the publisher builds.
	if graphview.MaxGenerationAncestryDepth < maxDedicatedBaseDeltaAncestors+1 {
		t.Fatalf("read bound=%d leaves no room for the commit layer over a maximal chain of %d",
			graphview.MaxGenerationAncestryDepth, maxDedicatedBaseDeltaAncestors)
	}

	source, err := os.ReadFile(filepath.Join("..", "graph", "store_sqlite", "catalog_dedicated_base.go"))
	if err != nil {
		t.Fatalf("read the catalog's ancestry limit: %v", err)
	}
	match := regexp.MustCompile(`(?m)^const maxDedicatedBaseAncestry = (\d+)$`).FindSubmatch(source)
	if match == nil {
		t.Fatal("catalog_dedicated_base.go no longer declares maxDedicatedBaseAncestry as a plain constant; update this guard")
	}
	hard, err := strconv.Atoi(string(match[1]))
	if err != nil {
		t.Fatalf("parse the catalog's ancestry limit %q: %v", match[1], err)
	}
	if maxDedicatedBaseActiveAncestryWalk != hard {
		t.Fatalf("planner walk limit=%d, want the catalog's hard limit %d", maxDedicatedBaseActiveAncestryWalk, hard)
	}
	if maxDedicatedBaseDeltaAncestors >= hard {
		t.Fatalf("allocation policy=%d is not below the catalog's hard limit %d", maxDedicatedBaseDeltaAncestors, hard)
	}
	if graphview.MaxGenerationAncestryDepth >= hard {
		t.Fatalf("read bound=%d is not below the catalog's hard limit %d", graphview.MaxGenerationAncestryDepth, hard)
	}
}

// extendDedicatedChain publishes one synthetic dedicated delta row over row and
// returns the new head. depth is used only for deterministic timestamps and
// failure messages.
//
// These are catalog rows only. Like the existing depth-policy test, this is a
// metadata-policy fixture: it says nothing about what a 32-layer runtime costs
// to parse, only about which parent the planner selects for a chain of a given
// length — which is a decision made entirely out of catalog metadata.
func extendDedicatedChain(t testing.TB, ctx context.Context, catalog *store_sqlite.Catalog, row store_sqlite.ViewGeneration, depth int) store_sqlite.ViewGeneration {
	t.Helper()
	id, err := catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: row.GraphID, CheckoutID: row.CheckoutID,
		GenerationKind: "dedicated", TreeOID: row.TreeOID, ConfigHash: row.ConfigHash,
		ExtractorVersions: row.ExtractorVersions, ResolverVersion: row.ResolverVersion,
		DependencyRevision:   row.DependencyRevision,
		BaseGenerationID:     row.GenerationID,
		LayerID:              fmt.Sprintf("dedicated-delta:%d", row.GenerationID),
		LowerViewFingerprint: fmt.Sprintf("dedicated:%s:%d", row.GraphID, row.GenerationID),
		CreatedAt:            int64(depth), State: store_sqlite.ViewGenerationBuilding,
	})
	if err != nil {
		t.Fatalf("create synthetic ancestor at depth %d: %v", depth, err)
	}
	if err := catalog.PublishViewGeneration(ctx, id, int64(depth+1)); err != nil {
		t.Fatalf("publish synthetic ancestor at depth %d: %v", depth, err)
	}
	next, found, err := catalog.GetViewGeneration(ctx, id)
	if err != nil || !found {
		t.Fatalf("read synthetic ancestor at depth %d: found=%v err=%v", depth, found, err)
	}
	return next
}

// deepDedicatedChain extends root until the active chain holds depth
// generations, counting root itself as depth one, and returns the head row.
func deepDedicatedChain(t testing.TB, ctx context.Context, catalog *store_sqlite.Catalog, root store_sqlite.ViewGeneration, depth int) store_sqlite.ViewGeneration {
	t.Helper()
	row := root
	for current := 2; current <= depth; current++ {
		row = extendDedicatedChain(t, ctx, catalog, row, current)
	}
	return row
}

// TestDedicatedBaseAdvanceRootsOnceTheChainHoldsTheBound walks the allocation
// policy across its boundary one generation at a time.
//
// The statement being pinned is the whole of W6.9's write half: the planner
// extends a chain only while doing so keeps it inside the bound, and the
// generation that would pass the bound is published as a new full root instead
// — never as a silently longer chain, and never as a refusal, since a full root
// is always available and refusing would wedge publication for good.
func TestDedicatedBaseAdvanceRootsOnceTheChainHoldsTheBound(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	catalog := f.builder.Store.Catalog()
	root, found, err := catalog.GetViewGeneration(ctx, initial.Claim.GenerationID)
	if err != nil || !found {
		t.Fatalf("initial row: found=%v err=%v", found, err)
	}
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	observation, err := f.observe(t, ctx)
	if err != nil {
		t.Fatal(err)
	}

	firstRoot := 0
	head := root
	for depth := 1; depth <= maxDedicatedBaseDeltaAncestors+1; depth++ {
		if depth > 1 {
			head = extendDedicatedChain(t, ctx, catalog, head, depth)
		}
		selected, err := dedicatedBaseParentForAdvance(ctx, catalog, f.publisher.authority, head, observation.Identity)
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		switch selected {
		case head.GenerationID:
			if firstRoot != 0 {
				t.Fatalf("depth %d extended the chain again after rooting at depth %d", depth, firstRoot)
			}
		case 0:
			if firstRoot == 0 {
				firstRoot = depth
			}
		default:
			t.Fatalf("depth %d selected %d, want the active head %d or a full root", depth, selected, head.GenerationID)
		}
	}
	// A chain already holding maxDedicatedBaseDeltaAncestors generations is the
	// first one the planner refuses to extend, because the link it would add
	// would be the bound plus one. So firstRoot is also the deepest chain the
	// planner ever produces.
	if firstRoot != maxDedicatedBaseDeltaAncestors {
		t.Fatalf("first full root forced at active depth %d, want %d", firstRoot, maxDedicatedBaseDeltaAncestors)
	}
	// And that deepest chain is one the materializer will compose, with room
	// left for the checkout commit layer that stands on the dedicated head.
	if firstRoot >= graphview.MaxGenerationAncestryDepth {
		t.Fatalf("the planner produces chains of %d, leaving no composable room under the %d-generation read bound",
			firstRoot, graphview.MaxGenerationAncestryDepth)
	}
}

// TestDedicatedBaseWalkReportsDepthSeparatelyFromCycles covers the last line of
// defence. A stored chain past the catalog's own hard limit is not a shape this
// planner can have produced, so it is reported rather than re-rooted around —
// and it is reported as a depth problem, not folded into the cycle message it
// used to share, because the two have nothing to do with each other.
func TestDedicatedBaseWalkReportsDepthSeparatelyFromCycles(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	catalog := f.builder.Store.Catalog()
	root, found, err := catalog.GetViewGeneration(ctx, initial.Claim.GenerationID)
	if err != nil || !found {
		t.Fatalf("initial row: found=%v err=%v", found, err)
	}
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	observation, err := f.observe(t, ctx)
	if err != nil {
		t.Fatal(err)
	}
	head := deepDedicatedChain(t, ctx, catalog, root, maxDedicatedBaseActiveAncestryWalk+1)
	_, err = dedicatedBaseParentForAdvance(ctx, catalog, f.publisher.authority, head, observation.Identity)
	if !errors.Is(err, store_sqlite.ErrDedicatedBaseCandidate) {
		t.Fatalf("a chain past the catalog limit was admitted: %v", err)
	}
	if strings.Contains(err.Error(), "cyclic") {
		t.Errorf("an over-deep chain is reported as a cycle: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxDedicatedBaseActiveAncestryWalk)) {
		t.Errorf("refusal does not name the limit it enforced: %v", err)
	}
}

// TestDedicatedDeltaClaimRefusedAtTheAncestryBound covers the parent the
// planner did not choose.
//
// buildObservedClaim's own contract says the catalog may hand back a claim
// naming a different valid parent than the caller proposed — a cached or
// coalesced reservation — and the catalog admits any chain under its hard
// 64-generation limit. So the bound has to be re-checked against the parent
// actually claimed, before any payload is written: a delta built over a chain
// already at the bound would publish a head no reader can compose.
func TestDedicatedDeltaClaimRefusedAtTheAncestryBound(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	catalog := f.builder.Store.Catalog()
	root, found, err := catalog.GetViewGeneration(ctx, initial.Claim.GenerationID)
	if err != nil || !found {
		t.Fatalf("initial row: found=%v err=%v", found, err)
	}
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	observation, err := f.observe(t, ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The measurement the gate reads. One generation short of the bound leaves
	// room for the delta; one more does not. The admissible side is asserted on
	// the depth primitive rather than by driving a build over a synthetic chain:
	// buildObservedClaim's success path physically builds and publishes, and the
	// existing end-to-end advance tests in this package already prove ordinary
	// deltas pass this gate.
	admissible := deepDedicatedChain(t, ctx, catalog, root, maxDedicatedBaseDeltaAncestors-1)
	if depth, err := dedicatedBaseAncestryDepth(ctx, catalog, admissible, maxDedicatedBaseDeltaAncestors); err != nil || depth != maxDedicatedBaseDeltaAncestors-1 {
		t.Fatalf("admissible fixture depth=%d err=%v, want %d", depth, err, maxDedicatedBaseDeltaAncestors-1)
	}
	refused := extendDedicatedChain(t, ctx, catalog, admissible, maxDedicatedBaseDeltaAncestors)
	if depth, err := dedicatedBaseAncestryDepth(ctx, catalog, refused, maxDedicatedBaseDeltaAncestors); err != nil || depth != maxDedicatedBaseDeltaAncestors {
		t.Fatalf("refused fixture depth=%d err=%v, want %d", depth, err, maxDedicatedBaseDeltaAncestors)
	}

	// The production entrypoint, on a claim whose parent the planner would
	// never have proposed.
	claim := initial.Claim
	claim.Status = "allocated"
	claim.AlreadyAdopted = false
	claim.BaseGenerationID = refused.GenerationID
	if _, _, err = f.publisher.buildObservedClaim(ctx, observation, claim, f.leases); !errors.Is(err, store_sqlite.ErrDedicatedBaseCandidate) {
		t.Fatalf("a delta over a maximal chain was admitted: %v", err)
	}
	if !strings.Contains(err.Error(), "ancestry bound") {
		t.Fatalf("refusal is not the ancestry bound's: %v", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxDedicatedBaseDeltaAncestors)) {
		t.Errorf("refusal does not name the bound it enforced: %v", err)
	}
	// The refusal lands before any physical work: the claimed generation is
	// still the row the fixture published, unchanged by this attempt.
	after, found, err := catalog.GetViewGeneration(ctx, claim.GenerationID)
	if err != nil || !found {
		t.Fatalf("claimed generation after the refusal: found=%v err=%v", found, err)
	}
	if after.BaseGenerationID != 0 {
		t.Fatalf("the refused attempt rewrote the claimed generation's lower to %d", after.BaseGenerationID)
	}
}
