package indexer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type dedicatedBuilderFixtureRequest struct {
	Identity    GenerationIdentity
	RootPath    string
	RepoPrefix  string
	WorkspaceID string
	ProjectID   string
	StorePath   string
}

func privateDedicatedBuilderFixture(t testing.TB) (*SparseGenerationBuilder, dedicatedBuilderFixtureRequest, func(...string) string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for name, body := range map[string]string{
		"go.mod":  "module example.invalid/dedicated\n\ngo 1.24\n",
		"base.go": "package dedicated\n\nfunc Committed() string { return \"committed\" }\nfunc Caller() string { return Committed() }\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("add", "go.mod", "base.go")
	git("-c", "user.name=Private Test", "-c", "user.email=private@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "initial committed snapshot")
	tree, commit := git("rev-parse", "HEAD^{tree}"), git("rev-parse", "HEAD")
	storePath := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := store_sqlite.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	builder := &SparseGenerationBuilder{Store: store, Registry: registry, Config: config.Default().Index, Logger: zap.NewNop()}
	request := dedicatedBuilderFixtureRequest{
		Identity:  GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: "private-dedicated-graph", CheckoutID: "private-owner", GenerationKind: "dedicated", TreeOID: tree, ProvenanceCommitOID: commit, ConfigHash: "private-config-v1", ExtractorVersions: "private-extractor-v1", ResolverVersion: "private-resolver-v1"},
		StorePath: storePath, RootPath: root, RepoPrefix: "private-dedicated", WorkspaceID: "private-workspace", ProjectID: "private-project",
	}
	catalog := store.Catalog()
	family := store_sqlite.RepositoryFamily{FamilyID: "private-family", CommonDirIdentity: filepath.Join(root, ".git"), State: "active"}
	if err := catalog.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	owner := store_sqlite.Checkout{
		CheckoutID: request.Identity.CheckoutID, Incarnation: "private-incarnation", FamilyID: family.FamilyID,
		RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main", State: store_sqlite.CheckoutStateReady,
		DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadTree: tree, HeadCommit: commit,
	}
	if err := catalog.UpsertCheckout(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID: request.Identity.GraphID, OwnerCheckoutID: owner.CheckoutID, RepoPrefix: request.RepoPrefix,
		FamilyID: family.FamilyID, IsPrimaryBase: true, State: store_sqlite.DedicatedGraphReady,
	}); err != nil {
		t.Fatal(err)
	}
	return builder, request, git
}

func TestPrivateDedicatedSnapshotPlanParity(t *testing.T) {
	builder, request, _ := privateDedicatedBuilderFixture(t)
	ctx := context.Background()
	target, err := source.NewGitTreeSource(ctx, request.RootPath, request.Identity.TreeOID)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	full, report, err := planDedicatedSnapshot(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	changes := make([]LayerPathChange, 0, len(full.indexed))
	for _, path := range full.indexed {
		changes = append(changes, LayerPathChange{Path: path, Kind: LayerPathAdded})
	}
	legacy, legacyReport, err := builder.planFileSetContext(ctx, BuildRequest{
		Identity: request.Identity, Base: graph.New(), Target: target, Changes: changes,
		RootPath: request.RootPath, RepoPrefix: request.RepoPrefix,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full, legacy) || report.SourceBytes != legacyReport.SourceBytes || report.AddedFiles != legacyReport.AddedFiles {
		t.Fatalf("full inventory differs from ordinary all-added plan: full=%+v sparse=%+v", report, legacyReport)
	}
}

func BenchmarkPrivateDedicatedSnapshotPlan(b *testing.B) {
	for _, size := range []int{10, 100} {
		b.Run(fmt.Sprintf("files_%d", size), func(b *testing.B) {
			builder, request, git := privateDedicatedBuilderFixture(b)
			for i := range size {
				body := fmt.Sprintf("package dedicated\n\nfunc Symbol%d() {}\n", i)
				if err := os.WriteFile(filepath.Join(request.RootPath, fmt.Sprintf("file%04d.go", i)), []byte(body), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			git("add", ".")
			git("-c", "user.name=Private Test", "-c", "user.email=private@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "planning fixture")
			target, err := source.NewGitTreeSource(context.Background(), request.RootPath, git("rev-parse", "HEAD^{tree}"))
			if err != nil {
				b.Fatal(err)
			}
			defer target.Close()
			full, _, err := planDedicatedSnapshot(context.Background(), target)
			if err != nil {
				b.Fatal(err)
			}
			changes := make([]LayerPathChange, 0, len(full.indexed))
			for _, path := range full.indexed {
				changes = append(changes, LayerPathChange{Path: path, Kind: LayerPathAdded})
			}
			req := BuildRequest{Identity: request.Identity, Base: graph.New(), Target: target, Changes: changes, RootPath: request.RootPath, RepoPrefix: request.RepoPrefix}
			b.Run("full_inventory", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					plan, _, err := planDedicatedSnapshot(context.Background(), target)
					if err != nil || len(plan.indexed) != len(full.indexed) {
						b.Fatalf("invalid full plan: %v", err)
					}
				}
			})
			b.Run("sparse_closure_full_change_set", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					plan, _, err := builder.planFileSetContext(context.Background(), req)
					if err != nil || len(plan.indexed) != len(full.indexed) {
						b.Fatalf("invalid sparse plan: %v", err)
					}
				}
			})
		})
	}
}

func privateClaimedDedicatedFixture(t testing.TB) (*SparseGenerationBuilder, dedicatedBuilderFixtureRequest, store_sqlite.DedicatedBaseBuildClaim) {
	t.Helper()
	builder, request, _ := privateDedicatedBuilderFixture(t)
	ctx := context.Background()
	// Production Open must install the publication schema; do not create it in this fixture.
	catalog := builder.Store.Catalog()
	authority, err := catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: request.Identity.GraphID,
		Owner:   store_sqlite.DedicatedBaseOwner{CheckoutID: request.Identity.CheckoutID, Incarnation: "private-incarnation"},
		Token:   "private-authority-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: authority, Identity: store_sqlite.DedicatedBaseIdentity{
			TreeOID: request.Identity.TreeOID, ConfigHash: request.Identity.ConfigHash,
			ExtractorVersions: request.Identity.ExtractorVersions, ResolverVersion: request.Identity.ResolverVersion,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "private-attempt-one", ProvenanceCommitOID: request.Identity.ProvenanceCommitOID, CreatedAt: 1,
	})
	if err != nil || claim.GenerationID <= 0 || claim.Status != "allocated" {
		t.Fatalf("initial claim=%+v err=%v", claim, err)
	}
	return builder, request, claim
}

func BenchmarkPrivateClaimedDedicatedReadyCycle(b *testing.B) {
	builder, request, claim := privateClaimedDedicatedFixture(b)
	ctx := context.Background()
	catalog := builder.Store.Catalog()
	id, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		b.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: claim.Desire.Authority, ExpectedDesiredEpoch: claim.Desire.Epoch, Identity: claim.Desire.Identity})
		if err != nil {
			b.Fatal(err)
		}
		ready, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: desire, ExpectedActiveGenerationID: id, AttemptToken: "not-used"})
		if err != nil {
			b.Fatal(err)
		}
		got, report, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: ready})
		if err != nil || got != id || !report.Coalesced {
			b.Fatalf("ready cycle: id=%d report=%+v err=%v", got, report, err)
		}
		adoption, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: ready})
		if err != nil || !adoption.AlreadyAdopted {
			b.Fatalf("repeat adoption: %+v %v", adoption, err)
		}
	}
	b.StopTimer()
	if err := check(); err != nil {
		b.Fatal(err)
	}
}

func TestPrivateClaimedDedicatedBaseLifecycle(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	catalog := builder.Store.Catalog()
	joined, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: claim.Desire, AttemptToken: "different-caller-token",
	})
	if err != nil || joined.GenerationID != claim.GenerationID || joined.AttemptToken != claim.AttemptToken || joined.Status != "building" {
		t.Fatalf("join=%+v err=%v", joined, err)
	}
	const dirty = "package dedicated\n\nfunc DirtyOnly() string { return \"dirty\" }\n"
	path := filepath.Join(request.RootPath, "base.go")
	if err := os.WriteFile(path, []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}
	build := ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID}
	id, report, err := builder.BuildClaimedDedicatedBase(ctx, build)
	if err != nil || id != claim.GenerationID || report.Coalesced || report.NodeCount == 0 {
		t.Fatalf("reserved build: id=%d report=%+v err=%v", id, report, err)
	}
	handle := builder.Store.AtGeneration(id)
	if handle.GetNode(request.RepoPrefix+"/base.go::Committed") == nil || handle.GetNode(request.RepoPrefix+"/base.go::DirtyOnly") != nil {
		t.Fatal("candidate did not retain committed source isolation")
	}
	dedicated, found, err := catalog.GetDedicatedGraph(ctx, request.Identity.GraphID)
	if err != nil || !found || dedicated.ActiveGenerationID != 0 {
		t.Fatalf("builder adopted candidate: %+v %v", dedicated, err)
	}
	adoption, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim})
	if err != nil || adoption.GenerationID != id || adoption.AlreadyAdopted {
		t.Fatalf("adopt=%+v err=%v", adoption, err)
	}
	ready, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: claim.Desire, ExpectedActiveGenerationID: id, AttemptToken: "unused-ready-token",
	})
	if err != nil || ready.GenerationID != id || ready.Status != "ready" || !ready.AlreadyAdopted {
		t.Fatalf("ready claim=%+v err=%v", ready, err)
	}
	// A ready build must use immutable payload, not start Git in this empty path.
	build.Claim, build.RootPath = ready, t.TempDir()
	readyID, readyReport, err := builder.BuildClaimedDedicatedBase(ctx, build)
	if err != nil || readyID != id || !readyReport.Coalesced {
		t.Fatalf("ready build: id=%d report=%+v err=%v", readyID, readyReport, err)
	}
	again, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: ready})
	if err != nil || !again.AlreadyAdopted {
		t.Fatalf("idempotent adopt=%+v err=%v", again, err)
	}
	if err := handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: request.RepoPrefix, FilePath: request.RepoPrefix + "/base.go", Mode: store_sqlite.OwnershipDelete}}); !errors.Is(err, store_sqlite.ErrPayloadGenerationSealed) {
		t.Fatalf("ready reuse reopened immutable payload: %v", err)
	}
	if len(builder.Store.AllNodes()) != 0 {
		t.Fatal("claimed candidate leaked into mutable generation zero")
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != dirty {
		t.Fatalf("dirty working file changed: %q %v", body, err)
	}
}

func TestPrivateClaimedDedicatedBaseReadyCyclesDoNotWrite(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	catalog := builder.Store.Catalog()
	id, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		t.Fatal(err)
	}
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: claim.Desire.Authority, ExpectedDesiredEpoch: claim.Desire.Epoch, Identity: claim.Desire.Identity})
		if err != nil {
			t.Fatal(err)
		}
		ready, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: desire, ExpectedActiveGenerationID: id, AttemptToken: "not-used"})
		if err != nil {
			t.Fatal(err)
		}
		// No root: ready immutable payload cannot require a filesystem checkout.
		got, report, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: ready})
		if err != nil || got != id || !report.Coalesced {
			t.Fatalf("ready cycle: id=%d report=%+v err=%v", got, report, err)
		}
		adoption, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: ready})
		if err != nil || !adoption.AlreadyAdopted {
			t.Fatalf("repeat adoption: %+v %v", adoption, err)
		}
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateReservedGenerationPreparationFailuresDrainFlight(t *testing.T) {
	for _, mode := range []string{"prepare_error", "nil_source", "publish_fence"} {
		t.Run(mode, func(t *testing.T) {
			builder, request, claim := privateClaimedDedicatedFixture(t)
			ctx := context.Background()
			var preparations, closes atomic.Int32
			failure := errors.New("private preparation fence")
			prepare := func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
				preparations.Add(1)
				if mode == "nil_source" {
					return nil, buildPlan{}, BuildReport{}, nil
				}
				target, err := source.NewGitTreeSource(ctx, request.RootPath, request.Identity.TreeOID)
				if err != nil {
					return nil, buildPlan{}, BuildReport{}, err
				}
				owned := &privatePreparedSource{ContentSource: target, closes: &closes}
				if mode == "prepare_error" {
					return owned, buildPlan{}, BuildReport{}, failure
				}
				plan, report, err := planDedicatedSnapshot(ctx, target)
				return owned, plan, report, err
			}
			req := BuildRequest{Identity: request.Identity, Base: graph.New(), RootPath: request.RootPath, RepoPrefix: request.RepoPrefix}
			if mode == "publish_fence" {
				req.PrePublish = func(context.Context, int64) error { return failure }
			}
			id, _, err := builder.buildReservedGenerationWithPreparation(ctx, req, buildPlan{}, BuildReport{}, time.Now(), claim.GenerationID, builder.Store.AtGeneration(claim.GenerationID), false, prepare)
			if err == nil || id != claim.GenerationID {
				t.Fatalf("failure was lost: id=%d err=%v", id, err)
			}
			if mode != "nil_source" && !errors.Is(err, failure) {
				t.Fatalf("wrong failure: %v", err)
			}
			wantCloses := int32(1)
			if mode == "nil_source" {
				wantCloses = 0
			}
			if preparations.Load() != 1 || closes.Load() != wantCloses {
				t.Fatalf("source lifetime: prepares=%d closes=%d", preparations.Load(), closes.Load())
			}
			row, found, err := builder.Store.Catalog().GetViewGeneration(ctx, id)
			if err != nil || !found || row.State != "failed" {
				t.Fatalf("abandoned candidate: %+v %v", row, err)
			}
			// A caller joining the terminal flight must not hang or open another source.
			waitCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			_, _, retryErr := builder.buildReservedGenerationWithPreparation(waitCtx, req, buildPlan{}, BuildReport{}, time.Now(), claim.GenerationID, builder.Store.AtGeneration(claim.GenerationID), true, prepare)
			if retryErr == nil || errors.Is(retryErr, context.DeadlineExceeded) || preparations.Load() != 1 {
				t.Fatalf("terminal flight retry err=%v preparations=%d", retryErr, preparations.Load())
			}
		})
	}
}

func TestPrivateClaimedDedicatedBaseStaleDesireRejectsReadyCandidate(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	build := ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath}
	if _, _, err := builder.BuildClaimedDedicatedBase(ctx, build); err != nil {
		t.Fatal(err)
	}
	changed := claim.Desire.Identity
	changed.ConfigHash += "-changed"
	if _, err := builder.Store.Catalog().RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: claim.Desire.Authority, ExpectedDesiredEpoch: claim.Desire.Epoch, Identity: changed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := builder.BuildClaimedDedicatedBase(ctx, build); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("stale ready build: %v", err)
	}
	if _, err := builder.Store.Catalog().AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); !errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		t.Fatalf("stale ready adoption: %v", err)
	}
}

func TestPrivateClaimedDedicatedBaseConcurrentOnePhysicalBuild(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	second, err := builder.Store.Catalog().ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: claim.Desire, AttemptToken: "second-caller"})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		id     int64
		report BuildReport
		err    error
	}
	results := make(chan result, 2)
	for _, c := range []store_sqlite.DedicatedBaseBuildClaim{claim, second} {
		go func() {
			id, report, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: c, RootPath: request.RootPath})
			results <- result{id, report, err}
		}()
	}
	physical := 0
	for range 2 {
		got := <-results
		if got.err != nil || got.id != claim.GenerationID {
			t.Errorf("concurrent build: id=%d report=%+v err=%v", got.id, got.report, got.err)
			continue
		}
		if !got.report.Coalesced {
			physical++
		}
	}
	if physical != 1 {
		t.Fatalf("expected one physical build, got %d", physical)
	}
}

type privatePreparedSource struct {
	source.ContentSource
	closes *atomic.Int32
}

type privatePanicWalkSource struct{ *privatePreparedSource }

func (s *privatePanicWalkSource) Walk(context.Context, func(source.FileMeta) error) error {
	panic("private inventory panic")
}

func TestPrivateReservedGenerationPreparationPanicClosesSource(t *testing.T) {
	for _, at := range []string{"validate", "inventory"} {
		t.Run(at, func(t *testing.T) {
			builder, request, claim := privateClaimedDedicatedFixture(t)
			ctx := context.Background()
			var closes atomic.Int32
			prepare := func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
				target, err := source.NewGitTreeSource(ctx, request.RootPath, request.Identity.TreeOID)
				if err != nil {
					return nil, buildPlan{}, BuildReport{}, err
				}
				owned := &privatePreparedSource{ContentSource: target, closes: &closes}
				var content source.ContentSource = owned
				if at == "inventory" {
					content = &privatePanicWalkSource{owned}
				}
				return prepareOwnedDedicatedSnapshot(ctx, content, func() error {
					if at == "validate" {
						panic("private validation panic")
					}
					return nil
				})
			}
			req := BuildRequest{Identity: request.Identity, Base: graph.New(), RootPath: request.RootPath, RepoPrefix: request.RepoPrefix}
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				_, _, _ = builder.buildReservedGenerationWithPreparation(ctx, req, buildPlan{}, BuildReport{}, time.Now(), claim.GenerationID, builder.Store.AtGeneration(claim.GenerationID), true, prepare)
			}()
			if recovered == nil || closes.Load() != 1 {
				t.Fatalf("panic cleanup: recovered=%v closes=%d", recovered, closes.Load())
			}
			row, found, err := builder.Store.Catalog().GetViewGeneration(ctx, claim.GenerationID)
			if err != nil || !found || row.State != "failed" {
				t.Fatalf("panic candidate: %+v %v", row, err)
			}
		})
	}
}

func TestPrivateReservedGenerationPreparationCancellationClosesSource(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var closes atomic.Int32
	prepare := func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
		target, err := source.NewGitTreeSource(ctx, request.RootPath, request.Identity.TreeOID)
		if err != nil {
			return nil, buildPlan{}, BuildReport{}, err
		}
		owned := &privatePreparedSource{ContentSource: target, closes: &closes}
		return prepareOwnedDedicatedSnapshot(ctx, owned, func() error { cancel(); return ctx.Err() })
	}
	req := BuildRequest{Identity: request.Identity, Base: graph.New(), RootPath: request.RootPath, RepoPrefix: request.RepoPrefix}
	_, _, err := builder.buildReservedGenerationWithPreparation(ctx, req, buildPlan{}, BuildReport{}, time.Now(), claim.GenerationID, builder.Store.AtGeneration(claim.GenerationID), true, prepare)
	if !errors.Is(err, context.Canceled) || closes.Load() != 1 {
		t.Fatalf("cancel cleanup: err=%v closes=%d", err, closes.Load())
	}
	row, found, err := builder.Store.Catalog().GetViewGeneration(context.Background(), claim.GenerationID)
	if err != nil || !found || row.State != "failed" {
		t.Fatalf("canceled candidate: %+v %v", row, err)
	}
}

func TestPrivateReservedGenerationRefreshedClaimSkipsReadyPayload(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	if _, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath}); err != nil {
		t.Fatal(err)
	}
	validated, err := builder.Store.Catalog().ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: claim.Desire, AttemptToken: claim.AttemptToken})
	if err != nil || validated.Status != "ready" {
		t.Fatalf("refreshed claim=%+v err=%v", validated, err)
	}
	var preparations atomic.Int32
	prepare := func(context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
		preparations.Add(1)
		return nil, buildPlan{}, BuildReport{}, errors.New("ready payload must not prepare again")
	}
	id, report, err := builder.buildReservedGenerationWithPreparation(ctx, BuildRequest{}, buildPlan{}, BuildReport{}, time.Now(), claim.GenerationID, builder.Store.AtGeneration(claim.GenerationID), validated.Status != "allocated", prepare)
	if err != nil || id != claim.GenerationID || !report.Coalesced || preparations.Load() != 0 {
		t.Fatalf("ready join: id=%d report=%+v err=%v preparations=%d", id, report, err, preparations.Load())
	}
}

func (s *privatePreparedSource) Close() error {
	s.closes.Add(1)
	return s.ContentSource.Close()
}

func TestPrivateReservedGenerationOnlyLeaderPreparesAndClosesSource(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	var preparations, closes atomic.Int32
	prepare := func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
		preparations.Add(1)
		target, err := source.NewGitTreeSource(ctx, request.RootPath, request.Identity.TreeOID)
		if err != nil {
			return nil, buildPlan{}, BuildReport{}, err
		}
		owned := &privatePreparedSource{ContentSource: target, closes: &closes}
		plan, report, err := planDedicatedSnapshot(ctx, target)
		return owned, plan, report, err
	}
	req := BuildRequest{Identity: request.Identity, Base: graph.New(), RootPath: request.RootPath,
		RepoPrefix: request.RepoPrefix, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID}
	type result struct {
		id     int64
		report BuildReport
		err    error
	}
	results := make(chan result, 2)
	for _, adopted := range []bool{false, true} {
		go func() {
			id, report, err := builder.buildReservedGenerationWithPreparation(ctx, req, buildPlan{}, BuildReport{}, time.Now(),
				claim.GenerationID, builder.Store.AtGeneration(claim.GenerationID), adopted, prepare)
			results <- result{id, report, err}
		}()
	}
	physical := 0
	for range 2 {
		got := <-results
		if got.err != nil || got.id != claim.GenerationID {
			t.Errorf("prepared flight: id=%d report=%+v err=%v", got.id, got.report, got.err)
			continue
		}
		if !got.report.Coalesced {
			physical++
		}
	}
	if physical != 1 || preparations.Load() != 1 || closes.Load() != 1 {
		t.Fatalf("duplicate preparation or source lifetime: physical=%d preparations=%d closes=%d", physical, preparations.Load(), closes.Load())
	}
}

func TestClaimedDedicatedBaseRejectsInvalidRequestsWithoutWrites(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	check, err := installDedicatedWriteAudit(ctx, request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(*ClaimedDedicatedBaseRequest)
	}{
		{"nonpositive_generation", func(r *ClaimedDedicatedBaseRequest) { r.Claim.GenerationID = 0 }},
		{"missing_attempt", func(r *ClaimedDedicatedBaseRequest) { r.Claim.AttemptToken = "" }},
		{"incremental_parent", func(r *ClaimedDedicatedBaseRequest) { r.Claim.BaseGenerationID = 1 }},
		{"checkout_layer", func(r *ClaimedDedicatedBaseRequest) { r.Claim.LayerID = "dirty-layer" }},
		{"lower_fingerprint", func(r *ClaimedDedicatedBaseRequest) { r.Claim.LowerViewFingerprint = "foreign-lower" }},
		{"replaced_attempt", func(r *ClaimedDedicatedBaseRequest) { r.Claim.AttemptToken += "-stale" }},
		{"different_owner", func(r *ClaimedDedicatedBaseRequest) { r.Claim.Desire.Authority.Owner.CheckoutID += "-foreign" }},
		{"different_incarnation", func(r *ClaimedDedicatedBaseRequest) { r.Claim.Desire.Authority.Owner.Incarnation += "-stale" }},
		{"different_config", func(r *ClaimedDedicatedBaseRequest) { r.Claim.Desire.Identity.ConfigHash += "-different" }},
		{"different_active_guard", func(r *ClaimedDedicatedBaseRequest) { r.Claim.ExpectedActiveGenerationID++ }},
		{"missing_root_while_building", func(r *ClaimedDedicatedBaseRequest) { r.RootPath = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath}
			test.edit(&input)
			id, _, err := builder.BuildClaimedDedicatedBase(ctx, input)
			if err == nil || id != 0 {
				t.Fatalf("invalid reservation accepted: id=%d err=%v", id, err)
			}
		})
	}
	input := ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath}
	if id, _, err := builder.BuildClaimedDedicatedBase(nil, input); err == nil || id != 0 { //nolint:staticcheck // Deliberately pass nil to verify rejection before persistence.
		t.Fatalf("nil context accepted: id=%d err=%v", id, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if id, _, err := builder.BuildClaimedDedicatedBase(canceled, input); id != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: id=%d err=%v", id, err)
	}
	var absent *SparseGenerationBuilder
	if id, _, err := absent.BuildClaimedDedicatedBase(ctx, input); err == nil || id != 0 {
		t.Fatalf("nil builder accepted: id=%d err=%v", id, err)
	}
	if err := check(); err != nil {
		t.Fatal(err)
	}
	row, found, err := builder.Store.Catalog().GetViewGeneration(ctx, claim.GenerationID)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationBuilding {
		t.Fatalf("invalid requests changed reserved generation: row=%+v found=%v err=%v", row, found, err)
	}
}

// installDedicatedWriteAudit instruments only a test-owned database. Keeping
// this in the indexer test package avoids exporting production-only test hooks
// from store_sqlite. All DDL commits before the measured interval starts.
func installDedicatedWriteAudit(ctx context.Context, path string) (func() error, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE dedicated_write_audit(n INTEGER NOT NULL); INSERT INTO dedicated_write_audit VALUES(0)`); err != nil {
		return nil, err
	}
	for _, table := range []string{"dedicated_base_publications", "dedicated_graphs", "view_generations", "nodes", "edges"} {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			query := fmt.Sprintf(`CREATE TRIGGER dedicated_audit_%s_%s AFTER %s ON %s BEGIN UPDATE dedicated_write_audit SET n=n+1; END`, table, op, op, table)
			if _, err := tx.ExecContext(ctx, query); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Exclude instrumentation connection cleanup from the measured interval.
	if err := db.Close(); err != nil {
		return nil, err
	}
	before, err := os.Stat(path + "-wal")
	if err != nil {
		return nil, err
	}
	return func() error {
		uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
		probe, err := sql.Open("sqlite", uri)
		if err != nil {
			return err
		}
		defer probe.Close()
		var writes int
		if err := probe.QueryRowContext(ctx, `SELECT n FROM dedicated_write_audit`).Scan(&writes); err != nil {
			return err
		}
		after, err := os.Stat(path + "-wal")
		if err != nil {
			return err
		}
		if writes != 0 || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
			return fmt.Errorf("ready reuse wrote store: logical writes=%d WAL=%d->%d mtime=%v->%v", writes, before.Size(), after.Size(), before.ModTime(), after.ModTime())
		}
		return nil
	}, nil
}

// This covers the builder -> adoption -> real public materializer boundary.
// It deliberately does not pretend to exercise daemon discovery or startup.
func TestClaimedDedicatedBaseMaterializesWithoutMutableRoot(t *testing.T) {
	builder, request, claim := privateClaimedDedicatedFixture(t)
	ctx := context.Background()
	file := request.RepoPrefix + "/base.go"
	const dirty = "package dedicated\n\nfunc DirtyOnly() {}\n"
	if err := os.WriteFile(filepath.Join(request.RootPath, "base.go"), []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}
	// Seed generation zero with both a conflicting identity and a unique file.
	// Neither may participate in an immutable full snapshot's effective view.
	noiseFile := request.RepoPrefix + "/dirty-only.go"
	builder.Store.AddBatch([]*graph.Node{
		{ID: file + "::Committed", Name: "DirtyCollision", Kind: graph.KindFunction, FilePath: file, RepoPrefix: request.RepoPrefix},
		{ID: noiseFile, Name: "dirty-only.go", Kind: graph.KindFile, FilePath: noiseFile, RepoPrefix: request.RepoPrefix},
		{ID: noiseFile + "::DirtyOnly", Name: "DirtyOnly", Kind: graph.KindFunction, FilePath: noiseFile, RepoPrefix: request.RepoPrefix},
	}, nil)
	id, _, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Store.Catalog().AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		t.Fatal(err)
	}
	materializer := &graphview.Materializer{
		Store: builder.Store, Catalog: builder.Store.Catalog(), Leases: graphview.NewLeaseManager(),
	}
	view, err := materializer.MaterializeRefView(ctx, request.Identity.GraphID, id)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if node := view.Reader.GetNode(file + "::Committed"); node == nil || node.Name != "Committed" {
		t.Fatalf("committed snapshot lost to mutable collision: %+v", node)
	}
	for _, key := range []string{noiseFile, noiseFile + "::DirtyOnly", file + "::DirtyOnly"} {
		if node := view.Reader.GetNode(key); node != nil {
			t.Errorf("mutable content leaked into immutable snapshot: %s: %+v", key, node)
		}
	}
	if got := view.Generations(); len(got) != 1 || got[0] != id {
		t.Fatalf("wrong published ancestry: %v", got)
	}
	if sources := view.GenerationSources(); len(sources) != 1 {
		t.Fatalf("wrong snapshot source count: %d", len(sources))
	}
	if node := builder.Store.GetNode(noiseFile); node == nil {
		t.Fatal("isolation was achieved by deleting the mutable graph")
	}
	if body, err := os.ReadFile(filepath.Join(request.RootPath, "base.go")); err != nil || string(body) != dirty {
		t.Fatalf("working copy changed: %q: %v", body, err)
	}
}

// This is a real root commit whose tree has no entries, not an unborn HEAD.
// The fixture is wholly private and does not reuse a preexisting checkout.
func TestPrivateEmptyCommittedDedicatedBaseAcceptance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("-c", "user.name=Private Test", "-c", "user.email=private@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "empty initial committed snapshot")
	commit := git("rev-parse", "--verify", "HEAD^{commit}")
	tree := git("rev-parse", "--verify", "HEAD^{tree}")
	if got := git("rev-list", "--parents", "-n", "1", "HEAD"); got != commit {
		t.Fatalf("fixture commit is not an initial root commit: %q", got)
	}
	if got := git("ls-tree", "-r", "--name-only", tree); got != "" {
		t.Fatalf("fixture tree is not empty: %q", got)
	}
	storePath := filepath.Join(t.TempDir(), "store.sqlite")
	store, err := store_sqlite.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	builder := &SparseGenerationBuilder{Store: store, Registry: registry, Config: config.Default().Index, Logger: zap.NewNop()}
	catalog := store.Catalog()
	const prefix = "private-empty"
	const graphID = "private-empty-graph"
	const checkoutID = "private-empty-owner"
	const incarnation = "private-empty-incarnation"
	family := store_sqlite.RepositoryFamily{FamilyID: "private-empty-family", CommonDirIdentity: filepath.Join(root, ".git"), State: "active"}
	if err := catalog.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID: checkoutID, Incarnation: incarnation, FamilyID: family.FamilyID,
		RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main", State: store_sqlite.CheckoutStateReady,
		DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated,
		HeadTree: tree, HeadCommit: commit,
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID: graphID, OwnerCheckoutID: checkoutID, RepoPrefix: prefix,
		FamilyID: family.FamilyID, IsPrimaryBase: true, State: store_sqlite.DedicatedGraphReady,
	}); err != nil {
		t.Fatal(err)
	}
	authority, err := catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: graphID, Owner: store_sqlite.DedicatedBaseOwner{CheckoutID: checkoutID, Incarnation: incarnation}, Token: "private-empty-authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: authority, Identity: store_sqlite.DedicatedBaseIdentity{
			TreeOID: tree, ConfigHash: "private-empty-config", ExtractorVersions: "private-empty-extractor", ResolverVersion: "private-empty-resolver",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "private-empty-attempt", ProvenanceCommitOID: commit, CreatedAt: 1,
	})
	if err != nil || claim.GenerationID <= 0 || claim.Status != "allocated" {
		t.Fatalf("initial claim=%+v err=%v", claim, err)
	}
	file := prefix + "/dirty-only.go"
	symbol := file + "::DirtyOnly"
	store.AddBatch([]*graph.Node{
		{ID: file, Name: "dirty-only.go", Kind: graph.KindFile, FilePath: file, RepoPrefix: prefix},
		{ID: symbol, Name: "DirtyOnly", Kind: graph.KindFunction, FilePath: file, RepoPrefix: prefix},
	}, nil)
	if store.GetNode(file) == nil || store.GetNode(symbol) == nil {
		t.Fatal("generation-zero sentinel setup failed")
	}
	publicationChecks := 0
	id, report, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
		Claim: claim, RootPath: root,
		PrePublish: func(context.Context, int64) error { publicationChecks++; return nil },
	})
	if err != nil || id != claim.GenerationID || report.Coalesced {
		t.Fatalf("empty committed build: id=%d report=%+v err=%v", id, report, err)
	}
	if report.AddedFiles != 0 || len(report.IndexedPaths) != 0 || report.SourceBytes != 0 || report.EdgeCount != 0 {
		t.Fatalf("empty committed tree produced source/edge work: %+v", report)
	}
	if publicationChecks != 1 {
		t.Fatalf("physical publication checks=%d, want one", publicationChecks)
	}
	row, found, err := catalog.GetViewGeneration(ctx, id)
	if err != nil || !found || row.State != store_sqlite.ViewGenerationReady || row.TreeOID != tree || row.BaseGenerationID != 0 {
		t.Fatalf("empty candidate not ready with exact identity: %+v found=%v err=%v", row, found, err)
	}
	before, found, err := catalog.GetDedicatedGraph(ctx, graphID)
	if err != nil || !found || before.ActiveGenerationID != 0 {
		t.Fatalf("builder adopted before explicit guard: %+v found=%v err=%v", before, found, err)
	}
	adoption, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: claim})
	if err != nil || adoption.GenerationID != id || adoption.AlreadyAdopted {
		t.Fatalf("empty candidate adoption=%+v err=%v", adoption, err)
	}
	materializer := &graphview.Materializer{Store: store, Catalog: catalog, Leases: graphview.NewLeaseManager()}
	view, err := materializer.MaterializeRefView(ctx, graphID, id)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if got := view.Generations(); len(got) != 1 || got[0] != id {
		t.Fatalf("wrong exact empty ancestry: %v", got)
	}
	if got := len(view.GenerationSources()); got != 1 {
		t.Fatalf("empty committed source count=%d, want one", got)
	}
	for _, key := range []string{file, symbol} {
		if node := view.Reader.GetNode(key); node != nil {
			t.Fatalf("generation-zero sentinel leaked into empty committed view: %s: %+v", key, node)
		}
		if store.GetNode(key) == nil {
			t.Fatalf("empty view achieved by deleting generation-zero sentinel: %s", key)
		}
	}
	nodes := view.Reader.AllNodes()
	for _, node := range nodes {
		if node.Kind == graph.KindFile || node.Kind == graph.KindFunction {
			t.Fatalf("empty committed view contains source node: %+v", node)
		}
		t.Logf("empty-view metadata node (no root-node policy assumed): %+v", node)
	}
	t.Logf("empty committed acceptance: commit=%s tree=%s generation=%d report_nodes=%d view_nodes=%d mutable_nodes=%d", commit, tree, id, report.NodeCount, len(nodes), len(store.AllNodes()))
	checkWrites, err := installDedicatedWriteAudit(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	for cycle := range 3 {
		unchanged, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
			Authority: authority, ExpectedDesiredEpoch: desire.Epoch, Identity: desire.Identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		ready, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
			Desire: unchanged, ExpectedActiveGenerationID: id, AttemptToken: fmt.Sprintf("unused-empty-%d", cycle),
		})
		if err != nil || ready.Status != "ready" || !ready.AlreadyAdopted {
			t.Fatalf("empty ready claim=%+v err=%v", ready, err)
		}
		// This directory has no .git, so reuse cannot require opening the tree.
		got, reused, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
			Claim: ready, RootPath: t.TempDir(),
			PrePublish: func(context.Context, int64) error {
				publicationChecks++
				return fmt.Errorf("ready reuse entered physical publication")
			},
		})
		if err != nil || got != id || !reused.Coalesced || publicationChecks != 1 {
			t.Fatalf("empty ready reuse: id=%d report=%+v physical_checks=%d err=%v", got, reused, publicationChecks, err)
		}
		again, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: ready})
		if err != nil || !again.AlreadyAdopted {
			t.Fatalf("empty ready adoption=%+v err=%v", again, err)
		}
	}
	if err := checkWrites(); err != nil {
		t.Fatal(err)
	}
}
