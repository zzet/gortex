package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

type dedicatedAdvanceFixture struct {
	builder   *SparseGenerationBuilder
	request   dedicatedBuilderFixtureRequest
	publisher *dedicatedBasePublisher
	leases    *graphview.LeaseManager
	identity  store_sqlite.DedicatedBaseIdentity
	workspace string
}

func newDedicatedAdvanceFixture(t testing.TB) *dedicatedAdvanceFixture {
	t.Helper()
	b, request, claim := privateClaimedDedicatedFixture(t)
	return &dedicatedAdvanceFixture{
		builder: b, request: request,
		publisher: &dedicatedBasePublisher{runtime: &dedicatedBaseRuntime{store: b.Store}, authority: claim.Desire.Authority},
		leases:    graphview.NewLeaseManager(), identity: claim.Desire.Identity, workspace: request.WorkspaceID,
	}
}

func (f *dedicatedAdvanceFixture) git(t testing.TB, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = f.request.RootPath
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("private git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *dedicatedAdvanceFixture) observe(t testing.TB, ctx context.Context) (dedicatedBaseObservation, error) {
	t.Helper()
	g, found, err := f.builder.Store.Catalog().GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
	if err != nil {
		return dedicatedBaseObservation{}, err
	}
	if !found {
		return dedicatedBaseObservation{}, errors.New("private graph missing")
	}
	identity := f.identity
	identity.TreeOID = f.git(t, "rev-parse", "HEAD^{tree}")
	return dedicatedBaseObservation{
		Identity: identity, ExpectedActiveGenerationID: g.ActiveGenerationID,
		RootPath: f.request.RootPath, WorkspaceID: f.workspace, ProjectID: f.request.ProjectID,
		ProvenanceCommitOID: f.git(t, "rev-parse", "HEAD"), CreatedAt: 1, Builder: *f.builder,
	}, nil
}

func (f *dedicatedAdvanceFixture) ensure(t testing.TB, ctx context.Context) dedicatedBaseResult {
	t.Helper()
	out, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) { return f.observe(t, ctx) })
	if err != nil {
		t.Fatal(err)
	}
	if out.Claim.GenerationID <= 0 || out.Adoption.GenerationID != out.Claim.GenerationID {
		t.Fatalf("invalid publication: %+v", out)
	}
	return out
}

func (f *dedicatedAdvanceFixture) commitFile(t testing.TB, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.request.RootPath, "advance.go"), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(t, "add", "advance.go")
	f.git(t, "commit", "-qm", "private advancement")
}

func (f *dedicatedAdvanceFixture) view(t testing.TB, ctx context.Context, generation int64) *graphview.RepoView {
	t.Helper()
	m := graphview.Materializer{Store: f.builder.Store, Catalog: f.builder.Store.Catalog(), Leases: f.leases, Logger: f.builder.Logger}
	v, err := m.MaterializeRefView(ctx, f.publisher.authority.GraphID, generation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return v
}

func TestDedicatedBaseCurrentInitialDeltaAndReadyReplay(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	if initial.Claim.BaseGenerationID != 0 {
		t.Fatal("initial publication is not full")
	}
	oldView := f.view(t, ctx, initial.Claim.GenerationID)
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	if err := os.WriteFile(filepath.Join(f.request.RootPath, "dirty_only.go"), []byte("package dedicated\nfunc DirtyOnlyMarker() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	delta := f.ensure(t, ctx)
	if delta.Claim.BaseGenerationID != initial.Claim.GenerationID || delta.Report.Coalesced {
		t.Fatalf("did not physically advance sparsely: %+v", delta)
	}
	path := f.request.RepoPrefix + "/advance.go"
	if len(oldView.Reader.GetFileNodes(path)) != 0 {
		t.Fatal("old immutable route changed")
	}
	current := f.view(t, ctx, delta.Claim.GenerationID)
	if len(current.Reader.GetFileNodes(path)) == 0 {
		t.Fatal("new committed source missing")
	}
	if len(current.Reader.GetFileNodes(f.request.RepoPrefix+"/dirty_only.go")) != 0 {
		t.Fatal("committed delta read dirty source")
	}
	observation, err := f.observe(t, ctx)
	if err != nil {
		t.Fatal(err)
	}
	observation.RootPath = filepath.Join(t.TempDir(), "absent-git-root")
	check, err := installDedicatedWriteAudit(ctx, f.request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.publisher.ensureCurrent(ctx, f.leases, func(context.Context) (dedicatedBaseObservation, error) { return observation, nil })
	if err != nil || replay.Claim.GenerationID != delta.Claim.GenerationID || !replay.Report.Coalesced || !replay.Adoption.AlreadyAdopted {
		t.Fatalf("ready replay: %+v err=%v", replay, err)
	}
	if err := check(); err != nil {
		t.Fatalf("ready replay wrote logical rows: %v", err)
	}
}

func TestDedicatedBaseCurrentTwoAdvancesRetainAncestry(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	first := f.ensure(t, ctx)
	firstView := f.view(t, ctx, first.Claim.GenerationID)
	f.commitFile(t, "package dedicated\n// New committed documentation.\nfunc AdvancementMarker() int { return 2 }\n")
	second := f.ensure(t, ctx)
	if first.Claim.BaseGenerationID != initial.Claim.GenerationID || second.Claim.BaseGenerationID != first.Claim.GenerationID {
		t.Fatalf("ancestry lost: %+v / %+v", first.Claim, second.Claim)
	}
	secondView := f.view(t, ctx, second.Claim.GenerationID)
	if len(secondView.GenerationSources()) != 3 || len(firstView.GenerationSources()) != 2 {
		t.Fatal("view did not retain complete physical ancestry")
	}
	old := firstView.Reader.GetNode(f.request.RepoPrefix + "/advance.go::AdvancementMarker")
	newer := secondView.Reader.GetNode(f.request.RepoPrefix + "/advance.go::AdvancementMarker")
	if old == nil || newer == nil || old.StartLine == newer.StartLine {
		t.Fatalf("old/new coordinates not independent: %#v / %#v", old, newer)
	}
	if f.builder.Store.NodeCount() != 0 {
		t.Fatal("committed publication populated legacy generation zero")
	}
}

func TestDedicatedBaseCurrentPolicyReseedKeepsOldRoute(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	old := f.view(t, ctx, initial.Claim.GenerationID)
	f.workspace += "-changed"
	f.identity.ConfigHash += "-workspace-change"
	changed := f.ensure(t, ctx)
	if changed.Claim.GenerationID == initial.Claim.GenerationID || changed.Claim.BaseGenerationID != 0 || changed.Report.Coalesced {
		t.Fatalf("policy change did not build a fresh full root: %+v", changed)
	}
	current := f.view(t, ctx, changed.Claim.GenerationID)
	if len(old.Reader.AllNodes()) == 0 || len(current.Reader.AllNodes()) == 0 {
		t.Fatal("old/new ready roots unavailable")
	}
	for _, n := range current.Reader.AllNodes() {
		if n.WorkspaceID != f.workspace {
			t.Fatalf("new scope missing on %s: %q", n.ID, n.WorkspaceID)
		}
	}
	for _, n := range old.Reader.AllNodes() {
		if n.WorkspaceID == f.workspace {
			t.Fatalf("old scope mutated on %s", n.ID)
		}
	}
}

func TestDedicatedBaseCurrentStaleBuildCannotAdopt(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	out, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) {
		observation, err := f.observe(t, ctx)
		if err != nil {
			return observation, err
		}
		observation.PrePublish = func(ctx context.Context, _ int64) error {
			publication, found, err := f.builder.Store.Catalog().DedicatedBasePublication(ctx, f.publisher.authority.GraphID)
			if err != nil {
				return err
			}
			if !found {
				return errors.New("private publication missing")
			}
			newer := observation.Identity
			newer.ConfigHash += "-newer-observation"
			_, err = f.builder.Store.Catalog().RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: f.publisher.authority, ExpectedDesiredEpoch: publication.Desire.Epoch, Identity: newer})
			return err
		}
		return observation, nil
	})
	if !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("stale build adopted: %+v, %v", out, err)
	}
	graph, found, err := f.builder.Store.Catalog().GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || graph.ActiveGenerationID != initial.Claim.GenerationID {
		t.Fatalf("stale build replaced active root: %+v, %v", graph, err)
	}
}

func TestDedicatedBaseCurrentRejectsMissingSharedLeases(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	called := false
	_, err := f.publisher.ensureCurrent(context.Background(), nil, func(context.Context) (dedicatedBaseObservation, error) {
		called = true
		return dedicatedBaseObservation{}, nil
	})
	if !errors.Is(err, errDedicatedBaseRuntimeInput) || called {
		t.Fatalf("missing lease domain admitted: called=%v err=%v", called, err)
	}
}

func TestDedicatedBaseCurrentReconstructsPreAuthorityActive(t *testing.T) {
	for _, change := range []string{"same-identity", "new-tree", "new-policy"} {
		t.Run(change, func(t *testing.T) {
			b, request, git := privateDedicatedBuilderFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			catalog := b.Store.Catalog()
			// The physical builder fixture already registered this Git family and
			// its canonical owner. Reuse that binding rather than inventing a
			// second family with the same unique common directory.
			binding, found, err := catalog.GetDedicatedGraph(ctx, request.Identity.GraphID)
			if err != nil || !found {
				t.Fatalf("fixture graph: found=%v err=%v", found, err)
			}
			owner, found, err := catalog.GetCheckout(ctx, binding.OwnerCheckoutID)
			if err != nil || !found || owner.Incarnation == "" || owner.FamilyID != binding.FamilyID {
				t.Fatalf("fixture owner: found=%v owner=%+v err=%v", found, owner, err)
			}
			if owner.HeadTree != git("rev-parse", "HEAD^{tree}") || owner.HeadCommit != git("rev-parse", "HEAD") {
				t.Fatal("fixture owner does not name the committed Git input")
			}
			identity := store_sqlite.DedicatedBaseIdentity{TreeOID: owner.HeadTree, ConfigHash: request.Identity.ConfigHash, ExtractorVersions: request.Identity.ExtractorVersions, ResolverVersion: request.Identity.ResolverVersion}
			legacy, err := catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{OwnerKind: "dedicated_graph", GraphID: binding.GraphID, CheckoutID: owner.CheckoutID,
				GenerationKind: "dedicated", TreeOID: identity.TreeOID, ConfigHash: identity.ConfigHash, ExtractorVersions: identity.ExtractorVersions, ResolverVersion: identity.ResolverVersion, CreatedAt: 1, State: store_sqlite.ViewGenerationBuilding})
			if err != nil {
				t.Fatal(err)
			}
			legacyID := request.RepoPrefix + "/legacy_only.go::UntrustedDirtyLegacy"
			b.Store.AtGeneration(legacy).AddNode(&graph.Node{ID: legacyID, Name: "UntrustedDirtyLegacy", Kind: graph.KindFunction, FilePath: request.RepoPrefix + "/legacy_only.go", RepoPrefix: request.RepoPrefix, Language: "go"})
			if err := catalog.PublishViewGeneration(ctx, legacy, 2); err != nil {
				t.Fatal(err)
			}
			binding.ActiveGenerationID = legacy
			if err := catalog.UpsertDedicatedGraph(ctx, binding); err != nil {
				t.Fatal(err)
			}
			authority, err := catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{GraphID: binding.GraphID, Owner: store_sqlite.DedicatedBaseOwner{CheckoutID: owner.CheckoutID, Incarnation: owner.Incarnation}, Token: "new-publication-protocol"})
			if err != nil {
				t.Fatal(err)
			}
			if authority.GenerationFloor < legacy {
				t.Fatalf("legacy not fenced: %+v", authority)
			}
			f := &dedicatedAdvanceFixture{builder: b, request: request, publisher: &dedicatedBasePublisher{runtime: &dedicatedBaseRuntime{store: b.Store}, authority: authority}, leases: graphview.NewLeaseManager(), identity: identity, workspace: request.WorkspaceID}
			if change == "new-tree" {
				f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
			}
			if change == "new-policy" {
				f.identity.ConfigHash += "-new-policy"
				f.workspace += "-new-policy"
			}
			result := f.ensure(t, ctx)
			if result.Claim.GenerationID <= authority.GenerationFloor || result.Claim.BaseGenerationID != 0 || result.Claim.ExpectedActiveGenerationID != legacy || result.Report.Coalesced {
				t.Fatalf("legacy source was trusted/reused: %+v", result)
			}
			if f.view(t, ctx, result.Claim.GenerationID).Reader.GetNode(legacyID) != nil {
				t.Fatal("legacy dirty payload leaked into rebuilt committed root")
			}
			if b.Store.AtGeneration(legacy).GetNode(legacyID) == nil {
				t.Fatal("adoption unexpectedly retired old payload")
			}
		})
	}
}

// This is a metadata-policy test, not a claim that the synthetic empty layers
// were physically parsed or that a 32-layer runtime is cheap. Real historical
// cache dispatch is covered separately below.
func TestDedicatedBaseAdvanceParentDepthPolicy(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	catalog := f.builder.Store.Catalog()
	parent, found, err := catalog.GetViewGeneration(ctx, initial.Claim.GenerationID)
	if err != nil || !found {
		t.Fatalf("initial row: found=%v err=%v", found, err)
	}
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	observation, err := f.observe(t, ctx)
	if err != nil {
		t.Fatal(err)
	}
	for depth := 2; depth <= 65; depth++ {
		id, err := catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
			OwnerKind: "dedicated_graph", GraphID: f.publisher.authority.GraphID,
			CheckoutID: f.publisher.authority.Owner.CheckoutID, GenerationKind: "dedicated",
			TreeOID: parent.TreeOID, ConfigHash: parent.ConfigHash,
			ExtractorVersions: parent.ExtractorVersions, ResolverVersion: parent.ResolverVersion,
			BaseGenerationID:     parent.GenerationID,
			LayerID:              fmt.Sprintf("dedicated-delta:%d", parent.GenerationID),
			LowerViewFingerprint: fmt.Sprintf("dedicated:%s:%d", parent.GraphID, parent.GenerationID),
			CreatedAt:            int64(depth), State: store_sqlite.ViewGenerationBuilding,
		})
		if err != nil {
			t.Fatalf("create metadata layer depth %d: %v", depth, err)
		}
		if err := catalog.PublishViewGeneration(ctx, id, int64(depth+1)); err != nil {
			t.Fatalf("publish metadata layer depth %d: %v", depth, err)
		}
		parent, found, err = catalog.GetViewGeneration(ctx, id)
		if err != nil || !found {
			t.Fatalf("read metadata layer depth %d: found=%v err=%v", depth, found, err)
		}
		if depth == 2 {
			floorAuthority := f.publisher.authority
			floorAuthority.GenerationFloor = initial.Claim.GenerationID
			if _, err := dedicatedBaseParentForAdvance(ctx, catalog, floorAuthority, parent, observation.Identity); !errors.Is(err, store_sqlite.ErrDedicatedBaseCandidate) {
				t.Fatalf("above-floor child inherited a below-floor ancestor: %v", err)
			}
		}
		if depth != 31 && depth != 32 && depth != 64 && depth != 65 {
			continue
		}
		selected, err := dedicatedBaseParentForAdvance(ctx, catalog, f.publisher.authority, parent, observation.Identity)
		if depth == 65 {
			if !errors.Is(err, store_sqlite.ErrDedicatedBaseCandidate) {
				t.Fatalf("excessive ancestry admitted: selected=%d err=%v", selected, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("valid ancestry depth %d: %v", depth, err)
		}
		want := int64(0)
		if depth == 31 {
			want = parent.GenerationID
		}
		if selected != want {
			t.Fatalf("depth %d selected=%d want=%d", depth, selected, want)
		}
	}
}

func TestDedicatedBaseCurrentHistoricalDeltaUsesReturnedParent(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	first := f.ensure(t, ctx)
	firstCommit := f.git(t, "rev-parse", "HEAD")
	firstView := f.view(t, ctx, first.Claim.GenerationID)
	f.commitFile(t, "package dedicated\n// Later committed shape.\nfunc AdvancementMarker() int { return 2 }\n")
	second := f.ensure(t, ctx)
	secondView := f.view(t, ctx, second.Claim.GenerationID)
	if first.Claim.BaseGenerationID != initial.Claim.GenerationID || second.Claim.BaseGenerationID != first.Claim.GenerationID {
		t.Fatalf("fixture did not make a genuine chain: %+v / %+v", first.Claim, second.Claim)
	}
	// The observed active is second, so the planner proposes second as lower.
	// The catalog must instead return the ready historical first, whose actual
	// lower is initial. The missing root proves this path does not reopen Git.
	f.git(t, "checkout", "--detach", firstCommit)
	observation, err := f.observe(t, ctx)
	if err != nil {
		t.Fatal(err)
	}
	observation.RootPath = filepath.Join(t.TempDir(), "missing-history-git-root")
	physicalCallbacks := 0
	observation.PrePublish = func(context.Context, int64) error {
		physicalCallbacks++
		return errors.New("historical ready path attempted physical publication")
	}
	reused, err := f.publisher.ensureCurrent(ctx, f.leases, func(context.Context) (dedicatedBaseObservation, error) { return observation, nil })
	if err != nil {
		t.Fatal(err)
	}
	if reused.Claim.GenerationID != first.Claim.GenerationID || reused.Claim.BaseGenerationID != initial.Claim.GenerationID || reused.Claim.ExpectedActiveGenerationID != second.Claim.GenerationID || !reused.Report.Coalesced || reused.Adoption.AlreadyAdopted || physicalCallbacks != 0 {
		t.Fatalf("historical dispatch/adoption mismatch: %+v callbacks=%d", reused, physicalCallbacks)
	}
	graphRow, found, err := f.builder.Store.Catalog().GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || graphRow.ActiveGenerationID != first.Claim.GenerationID {
		t.Fatalf("historical candidate was not adopted: %+v found=%v err=%v", graphRow, found, err)
	}
	current := f.view(t, ctx, reused.Adoption.GenerationID)
	id := f.request.RepoPrefix + "/advance.go::AdvancementMarker"
	before, after, later := firstView.Reader.GetNode(id), current.Reader.GetNode(id), secondView.Reader.GetNode(id)
	if before == nil || after == nil || later == nil || before.StartLine != after.StartLine || later.StartLine == after.StartLine {
		t.Fatalf("cached/current/later views lost their own source: %#v / %#v / %#v", before, after, later)
	}
}

func TestDedicatedBaseCurrentCanceledDeltaFollowerKeepsLeader(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	started := make(chan int64, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	type outcome struct {
		result dedicatedBaseResult
		err    error
	}
	leaderResult := make(chan outcome, 1)
	leaderDone := make(chan struct{})
	go func() {
		finished := outcome{err: errors.New("leader exited before returning a result")}
		defer func() { leaderResult <- finished; close(leaderDone) }()
		result, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) {
			observation, err := f.observe(t, ctx)
			if err != nil {
				return observation, err
			}
			observation.PrePublish = func(ctx context.Context, id int64) error {
				started <- id
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return observation, nil
		})
		finished = outcome{result: result, err: err}
	}()
	// This cleanup is newer than the fixture's Store.Close cleanup: join the
	// physical owner before closing the shared temporary store, including fatal
	// assertions in the caller. Its request is bounded by ctx.
	t.Cleanup(func() { unblock(); cancel(); <-leaderDone })
	var generation int64
	select {
	case generation = <-started:
	case <-leaderDone:
		finished := <-leaderResult
		t.Fatalf("leader exited before the publication barrier: %v", finished.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	followerCtx, followerCancel := context.WithTimeout(ctx, 2*time.Second)
	follower, err := f.publisher.ensureCurrent(followerCtx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) { return f.observe(t, ctx) })
	followerCancel()
	if !errors.Is(err, context.DeadlineExceeded) || follower.Claim.GenerationID != generation || follower.Claim.Status == "allocated" {
		t.Fatalf("follower did not join then cancel: %+v err=%v", follower, err)
	}
	if !f.builder.Store.PayloadBuildFlightActive(generation) || !f.leases.InUse(initial.Claim.GenerationID) {
		t.Fatal("canceled follower removed the leader's flight or parent lease")
	}
	claim := follower.Claim
	validated, err := f.builder.Store.Catalog().ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		ExistingGenerationID: claim.GenerationID, Desire: claim.Desire,
		ExpectedActiveGenerationID: claim.ExpectedActiveGenerationID, AttemptToken: claim.AttemptToken,
		BaseGenerationID: claim.BaseGenerationID, LayerID: claim.LayerID, LowerViewFingerprint: claim.LowerViewFingerprint,
		CreatedAt: 1, ProvenanceCommitOID: f.git(t, "rev-parse", "HEAD"),
	})
	if err != nil || validated.GenerationID != generation {
		t.Fatalf("follower cancellation failed/replaced the current claim: %+v err=%v", validated, err)
	}
	unblock()
	<-leaderDone
	leader := <-leaderResult
	if leader.err != nil || leader.result.Adoption.GenerationID != generation {
		t.Fatalf("leader could not finish after follower cancellation: %+v err=%v", leader.result, leader.err)
	}
	if f.builder.Store.PayloadBuildFlightActive(generation) || f.leases.InUse(initial.Claim.GenerationID) {
		t.Fatal("completed delta retained its flight or private lower lease")
	}
	replay := f.ensure(t, ctx)
	if replay.Claim.GenerationID != generation || !replay.Report.Coalesced || !replay.Adoption.AlreadyAdopted {
		t.Fatalf("post-cancellation ready replay: %+v", replay)
	}
}

func TestDedicatedBaseCurrentPhysicalFailurePreservesActiveAndCharacterizesRetry(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	initial := f.ensure(t, ctx)
	f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	physicalFailure := errors.New("private physical prepublication failure")
	failed, err := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) {
		observation, err := f.observe(t, ctx)
		if err != nil {
			return observation, err
		}
		observation.PrePublish = func(context.Context, int64) error { return physicalFailure }
		return observation, nil
	})
	if !errors.Is(err, physicalFailure) || failed.Claim.GenerationID <= 0 || failed.Adoption.GenerationID != 0 {
		t.Fatalf("physical failure returned success/partial adoption: %+v err=%v", failed, err)
	}
	catalog := f.builder.Store.Catalog()
	beforeRetry, found, err := catalog.GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || beforeRetry.ActiveGenerationID != initial.Claim.GenerationID {
		t.Fatalf("physical failure changed active: %+v found=%v err=%v", beforeRetry, found, err)
	}
	if f.builder.Store.PayloadBuildFlightActive(failed.Claim.GenerationID) || f.leases.InUse(initial.Claim.GenerationID) {
		t.Fatal("failed physical owner retained its flight or private parent lease")
	}
	row, rowFound, rowErr := catalog.GetViewGeneration(ctx, failed.Claim.GenerationID)
	publication, pubFound, pubErr := catalog.DedicatedBasePublication(ctx, f.publisher.authority.GraphID)
	if rowErr != nil || pubErr != nil {
		t.Fatalf("failure metadata unavailable: row=%v publication=%v", rowErr, pubErr)
	}
	t.Logf("after physical failure: generationFound=%v row=%+v publicationFound=%v publication=%+v", rowFound, row, pubFound, publication)
	// Characterization only: the exact abandonment/current-attempt source flow
	// was unavailable. Do not assert automatic retry success or blanket-Fail a
	// claim here. The actual outcome is retained for the production owner.
	retry, retryErr := f.publisher.ensureCurrent(ctx, f.leases, func(ctx context.Context) (dedicatedBaseObservation, error) { return f.observe(t, ctx) })
	afterRetry, found, err := catalog.GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
	if err != nil || !found {
		t.Fatalf("retry active metadata unavailable: found=%v err=%v", found, err)
	}
	t.Logf("retry characterization: previousGeneration=%d result=%+v err=%v active=%d", failed.Claim.GenerationID, retry, retryErr, afterRetry.ActiveGenerationID)
	if retryErr != nil {
		if retry.Adoption.GenerationID != 0 || afterRetry.ActiveGenerationID != initial.Claim.GenerationID {
			t.Fatalf("rejected retry changed active/adopted: %+v err=%v active=%d", retry, retryErr, afterRetry.ActiveGenerationID)
		}
		return
	}
	if retry.Adoption.GenerationID <= 0 || retry.Adoption.GenerationID != retry.Claim.GenerationID || afterRetry.ActiveGenerationID != retry.Adoption.GenerationID {
		t.Fatalf("successful retry was not guarded adoption: %+v active=%d", retry, afterRetry.ActiveGenerationID)
	}
	if f.view(t, ctx, retry.Adoption.GenerationID).Reader.GetNode(f.request.RepoPrefix+"/advance.go::AdvancementMarker") == nil {
		t.Fatal("successful retry omitted the committed changed source")
	}
}

// TestDedicatedDeltaBuildTakesItsFactHintsFromTheWholeAncestry is the ref-fact
// hint scope at the third production site: the committed base's own advance.
//
// buildObservedClaim hands the delta build the reader for the parent
// generation it is stacking on. Scoped to that ONE generation the base answers
// the closure's durable-hint question from one layer of a stack the structural
// reads compose in full — and since no sparse generation writes reference
// facts (ancestryRefFacts documents the census), from a layer that holds none.
// The closure then adds nothing for the hint and a file the changed one
// references is left out of the generation: a NARROWER affected set, which is
// the one direction the closure is not allowed to be wrong in. It is the same
// blindness the coordinator's two sites had, in the file that advances the
// base every dependent is a delta against.
//
// The hint is the only route from the changed file to base.go: the committed
// file this advance adds references nothing, so the structural walk over its
// edges reaches no other file.
//
// Revert-red: scope the base's facts back to
// store.AtGeneration(claim.BaseGenerationID) and the published delta stops
// claiming base.go.
func TestDedicatedDeltaBuildTakesItsFactHintsFromTheWholeAncestry(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	initial := f.ensure(t, ctx)
	if initial.Claim.BaseGenerationID != 0 {
		t.Fatalf("the initial publication is not a full root: %+v", initial.Claim)
	}
	parent := initial.Claim.GenerationID
	prefix := f.request.RepoPrefix
	basePath := prefix + "/base.go"
	advancePath := prefix + "/advance.go"

	// The generation a single-handle scope would read holds no facts of its
	// own, which is the blindness this pins.
	parentFacts, err := f.builder.Store.AtGeneration(parent).LoadRefFactsByFiles(prefix, nil)
	if err != nil {
		t.Fatalf("read generation %d's facts: %v", parent, err)
	}
	if len(parentFacts) != 0 {
		t.Fatalf("generation %d holds %d facts of its own; the single-handle scope is no longer blind and this test needs rewriting",
			parent, len(parentFacts))
	}

	// The hint's target has to be a node the composed base can resolve to a
	// file, so it is read out of the parent rather than spelled by hand.
	var target string
	for _, node := range f.view(t, ctx, parent).Reader.GetFileNodes(basePath) {
		if node != nil && node.Name == "Committed" {
			target = node.ID
			break
		}
	}
	if target == "" {
		t.Fatalf("the committed base holds no Committed symbol in %s", basePath)
	}

	// A durable hint at generation zero, where every fact in the database is.
	hint := graph.RefFact{
		FromID: advancePath + "::AdvancementMarker", ToID: target,
		Kind: "calls", RefName: "Committed", Line: 2, Origin: "ast_resolved", Tier: "resolved",
		FilePath: advancePath, Lang: "go",
	}
	if err := f.builder.Store.AtGeneration(0).BulkSetRefFacts(prefix, []graph.RefFact{hint}); err != nil {
		t.Fatalf("seed the corpus hint: %v", err)
	}

	// The advance itself: one committed file that references nothing.
	f.commitFile(t, "package dedicated\n\nfunc AdvancementMarker() int { return 1 }\n")
	delta := f.ensure(t, ctx)
	if delta.Claim.BaseGenerationID != parent || delta.Report.Coalesced {
		t.Fatalf("the advance did not build a sparse delta over %d: %+v", parent, delta.Claim)
	}

	claimed := map[string]bool{}
	for _, path := range claimedPaths(t, f.builder.Store, delta.Claim.GenerationID) {
		claimed[path] = true
	}
	if !claimed[advancePath] {
		t.Fatalf("the delta does not claim the file the advance added (%s): %v", advancePath, claimed)
	}
	// The hint pulls base.go in as a file the pass had to READ to re-derive
	// the changed one, which the ownership split records as read-only context
	// rather than as a claim. Either way the closure reached it, and that is
	// the question the scope of the fact hints decides.
	reached := map[string]bool{}
	for _, path := range closurePaths(t, f.builder.Store, delta.Claim.GenerationID) {
		reached[path] = true
	}
	if !reached[basePath] {
		t.Fatalf("the delta left %s out of its closure: the build read its fact hints from one generation, not from the ancestry (reached %v)",
			basePath, reached)
	}
}
