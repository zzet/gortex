package store_sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
	sqlite "modernc.org/sqlite"
)

// Public Build plans before joining a flight. Only actual physical intake opens
// are blocked; ordinary planning opens pass through unchanged.
type privatePublicFullSource struct {
	source.ContentSource
	store        *store_sqlite.Store
	generationID int64
	ctx          context.Context
	entered      chan struct{}
	release      <-chan struct{}
	enterOnce    sync.Once
	closeOnce    sync.Once
	closeErr     error
	flightOpens  atomic.Int32
	planningOpen atomic.Int32
	closeCalls   atomic.Int32
}

func (s *privatePublicFullSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	if filepath.Base(path) == "base.go" {
		if s.store.PayloadBuildFlightActive(s.generationID) {
			s.flightOpens.Add(1)
			s.enterOnce.Do(func() { close(s.entered) })
			select {
			case <-s.release:
			case <-s.ctx.Done():
				return nil, source.FileMeta{}, s.ctx.Err()
			}
		} else {
			s.planningOpen.Add(1)
		}
	}
	return s.ContentSource.Open(path)
}

func (s *privatePublicFullSource) Close() error {
	s.closeCalls.Add(1)
	s.closeOnce.Do(func() { s.closeErr = s.ContentSource.Close() })
	return s.closeErr
}

func TestPrivatePublicBuilderSQLiteFull(t *testing.T) {
	for _, mode := range []string{"healthy", "sqlite_full"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			root := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("private git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			var body strings.Builder
			body.WriteString("package dedicated\n\n")
			firstName := ""
			for i := range 256 {
				name := fmt.Sprintf("Long%04d%s", i, strings.Repeat("x", 1024))
				if i == 0 {
					firstName = name
				}
				fmt.Fprintf(&body, "func %s() int { return %d }\n", name, i)
			}
			if body.Len() > 1024*1024 {
				t.Fatal("private corpus exceeded 1 MiB cap")
			}
			for name, contents := range map[string]string{
				"go.mod":  "module example.invalid/dedicated\n\ngo 1.24\n",
				"base.go": body.String(),
			} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			git("init", "-q")
			git("add", "go.mod", "base.go")
			git("-c", "user.name=Private Test", "-c", "user.email=private@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "bounded public FULL fixture")
			tree, commit := git("rev-parse", "HEAD^{tree}"), git("rev-parse", "HEAD")
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			const repoPrefix = "private-public-full"
			identity := indexer.GenerationIdentity{
				OwnerKind: "dedicated_graph", GraphID: "private-public-full-graph", CheckoutID: "private-public-full-owner",
				GenerationKind: "commit", LayerID: "private-public-full-layer", TreeOID: tree,
				ProvenanceCommitOID: commit, ConfigHash: "private-config-v1", ExtractorVersions: "private-extractor-v1",
				ResolverVersion: "private-resolver-v1", CreatedAt: 1,
			}
			catalog := store.Catalog()
			family := store_sqlite.RepositoryFamily{FamilyID: "private-public-full-family", CommonDirIdentity: filepath.Join(root, ".git"), State: "active"}
			if err := catalog.UpsertRepositoryFamily(ctx, family); err != nil {
				t.Fatal(err)
			}
			owner := store_sqlite.Checkout{
				CheckoutID: identity.CheckoutID, Incarnation: "private-incarnation", FamilyID: family.FamilyID,
				RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main", State: store_sqlite.CheckoutStateReady,
				DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated,
				HeadTree: tree, HeadCommit: commit,
			}
			if err := catalog.UpsertCheckout(ctx, owner); err != nil {
				t.Fatal(err)
			}
			if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
				GraphID: identity.GraphID, OwnerCheckoutID: owner.CheckoutID, RepoPrefix: repoPrefix,
				FamilyID: family.FamilyID, IsPrimaryBase: true, State: "ready",
			}); err != nil {
				t.Fatal(err)
			}
			// Match every field forwarded by the actual public Build allocation.
			id, _, adopted, err := store.BeginPayloadGenerationWithStatus(ctx, store_sqlite.PayloadGenerationRequest{
				OwnerKind: identity.OwnerKind, GraphID: identity.GraphID, LayerID: identity.LayerID,
				CheckoutID: identity.CheckoutID, GenerationKind: identity.GenerationKind, BaseGenerationID: identity.BaseGenerationID,
				LowerViewFingerprint: identity.LowerViewFingerprint, TreeOID: identity.TreeOID,
				ProvenanceCommitOID: identity.ProvenanceCommitOID, ConfigHash: identity.ConfigHash,
				ExtractorVersions: identity.ExtractorVersions, ResolverVersion: identity.ResolverVersion, CreatedAt: identity.CreatedAt,
			})
			if err != nil || adopted || id <= 0 {
				t.Fatalf("ordinary preallocation: id=%d adopted=%v err=%v", id, adopted, err)
			}
			if store.PayloadBuildFlightActive(id) {
				t.Fatal("preallocation unexpectedly owns a physical flight")
			}
			baseID := repoPrefix + "/base-sentinel.go::BaseSentinel"
			store.AddBatch([]*graph.Node{{ID: baseID, Name: "BaseSentinel", Kind: graph.KindFunction,
				FilePath: repoPrefix + "/base-sentinel.go", RepoPrefix: repoPrefix}}, nil)
			target, err := source.NewGitTreeSource(ctx, root, tree)
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			wrapped := &privatePublicFullSource{ContentSource: target, store: store, generationID: id,
				ctx: ctx, entered: entered, release: release}
			var releaseOnce, ownedCloseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			closeOwned := func() {
				ownedCloseOnce.Do(func() {
					if err := wrapped.Close(); err != nil {
						t.Errorf("caller-owned source close: %v", err)
					}
				})
			}
			var work sync.WaitGroup
			var oldLimit int64
			restore := func() error {
				if oldLimit <= 0 {
					return nil
				}
				restoreCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				_, err := store_sqlite.PrivateSQLiteFullWriterStateForTest(restoreCtx, store, oldLimit)
				return err
			}
			// LIFO cleanup joins all owners and restores capacity before Store.Close.
			t.Cleanup(func() {
				unblock()
				cancel()
				work.Wait()
				if err := restore(); err != nil {
					t.Errorf("private ceiling cleanup restore: %v", err)
				}
				closeOwned()
			})
			registry := parser.NewRegistry()
			languages.RegisterAll(registry)
			builder := &indexer.SparseGenerationBuilder{Store: store, Registry: registry, Config: config.Default().Index, Logger: zap.NewNop()}
			req := indexer.BuildRequest{
				Identity: identity, Base: graph.New(), Target: wrapped, RootPath: root, RepoPrefix: repoPrefix,
				WorkspaceID: "private-workspace", ProjectID: "private-project",
				Changes: []indexer.LayerPathChange{{Path: "go.mod", Kind: indexer.LayerPathAdded}, {Path: "base.go", Kind: indexer.LayerPathAdded}},
			}
			type outcome struct {
				id     int64
				report indexer.BuildReport
				err    error
				panic  any
			}
			var leader outcome
			leaderDone := make(chan struct{})
			work.Add(1)
			go func() {
				defer work.Done()
				defer close(leaderDone)
				defer func() { leader.panic = recover() }()
				leader.id, leader.report, leader.err = builder.Build(ctx, req)
			}()
			select {
			case <-entered:
			case <-leaderDone:
				t.Fatalf("public Build exited before active-flight intake barrier: %+v", leader)
			case <-ctx.Done():
				t.Fatal("public Build intake barrier deadline (setup failure)")
			}
			if !store.PayloadBuildFlightActive(id) || wrapped.flightOpens.Load() == 0 {
				t.Fatal("barrier did not prove a real public physical flight")
			}
			followerDone := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
			followerErrors := [2]error{}
			for i := range followerDone {
				flight, isLeader, ready, err := store.JoinPayloadBuildFlight(ctx, id, true)
				if err != nil || flight == nil || isLeader || ready {
					t.Fatalf("real follower %d: leader=%v ready=%v err=%v", i, isLeader, ready, err)
				}
				work.Add(1)
				go func(i int) {
					defer work.Done()
					defer close(followerDone[i])
					followerErrors[i] = flight.Wait(ctx)
				}(i)
			}
			before, err := store_sqlite.PrivateSQLiteFullWriterStateForTest(ctx, store, 0)
			if err != nil {
				t.Fatal(err)
			}
			oldLimit = before.MaxPageCount
			if before.PageCount <= 0 || before.PageSize <= 0 || before.FreePages < 0 ||
				before.PageCount*before.PageSize > 16*1024*1024 || before.FreePages*before.PageSize > int64(body.Len()) {
				t.Fatalf("private FULL sizing precondition: %+v corpus=%d", before, body.Len())
			}
			if mode == "sqlite_full" {
				capped, err := store_sqlite.PrivateSQLiteFullWriterStateForTest(ctx, store, before.PageCount)
				if err != nil || capped.MaxPageCount != before.PageCount {
					t.Fatalf("real writer page ceiling: %+v err=%v", capped, err)
				}
			}
			unblock()
			select {
			case <-leaderDone:
			case <-ctx.Done():
				t.Fatal("public leader did not drain")
			}
			for i := range followerDone {
				select {
				case <-followerDone[i]:
				case <-ctx.Done():
					t.Fatalf("public leader's follower %d did not drain", i)
				}
			}
			row, found, stateErr := catalog.GetViewGeneration(ctx, id)
			t.Logf("mode=%s id=%d corpus_bytes=%d before=%+v leader_id=%d leader_err_type=%T leader_err=%v panic_type=%T panic=%v state_before_restore_found=%v state=%s state_err=%v planning_opens=%d physical_opens=%d",
				mode, id, body.Len(), before, leader.id, leader.err, leader.err, leader.panic, leader.panic, found, row.State, stateErr, wrapped.planningOpen.Load(), wrapped.flightOpens.Load())
			if err := restore(); err != nil {
				t.Fatal(err)
			}
			firstID := repoPrefix + "/base.go::" + firstName
			if mode == "healthy" {
				if leader.panic != nil || leader.err != nil || leader.id != id || leader.report.GenerationID != id || leader.report.Coalesced || leader.report.NodeCount == 0 {
					t.Errorf("healthy public Build did not own preallocated generation: %+v", leader)
				}
				if store.AtGeneration(id).GetNode(firstID) == nil {
					t.Error("healthy real public parser did not persist expected function")
				}
				if !found || stateErr != nil || row.State != store_sqlite.ViewGenerationReady {
					t.Errorf("healthy public generation not ready: found=%v state=%s err=%v", found, row.State, stateErr)
				}
				for i, err := range followerErrors {
					if err != nil {
						t.Errorf("healthy follower %d: %v", i, err)
					}
				}
			} else {
				actualErr := leader.err
				if panicErr, ok := leader.panic.(error); ok {
					actualErr = panicErr
				}
				var driverErr *sqlite.Error
				if !errors.As(actualErr, &driverErr) || driverErr == nil || driverErr.Code()&0xff != 13 {
					t.Fatalf("fixture did not produce concrete SQLite FULL: error=%T %v panic=%T %v", actualErr, actualErr, leader.panic, leader.panic)
				}
				t.Logf("actual_driver_type=%T actual_driver_code=%d", driverErr, driverErr.Code())
				if leader.panic != nil {
					t.Errorf("actual SQLITE_FULL escaped public Build recovery: %T %v", leader.panic, leader.panic)
				}
				if leader.id != id || leader.report.GenerationID != id {
					t.Errorf("public FULL return lost generation identity: id=%d report=%d want=%d", leader.id, leader.report.GenerationID, id)
				}
				driverErr = nil
				if !errors.As(leader.err, &driverErr) || driverErr == nil || driverErr.Code()&0xff != 13 {
					t.Errorf("public leader lost actual FULL cause: %T %v", leader.err, leader.err)
				}
				for i, err := range followerErrors {
					driverErr = nil
					t.Logf("follower=%d error_type=%T error=%v", i, err, err)
					if !errors.As(err, &driverErr) || driverErr == nil || driverErr.Code()&0xff != 13 {
						t.Errorf("public follower %d lost actual FULL cause: %T %v", i, err, err)
					}
				}
			}
			if store.PayloadBuildFlightActive(id) || wrapped.closeCalls.Load() != 0 {
				t.Errorf("public borrowed-source/flight ownership: active=%v closes_before_owner=%d", store.PayloadBuildFlightActive(id), wrapped.closeCalls.Load())
			}
			if store.GetNode(baseID) == nil || store.GetNode(firstID) != nil {
				t.Error("public candidate damaged or contaminated generation zero")
			}
			closeOwned()
			if wrapped.closeCalls.Load() != 1 {
				t.Errorf("borrowed source close calls=%d want exactly one fixture-owned close", wrapped.closeCalls.Load())
			}
			if err := store.RetirePayloadGeneration(ctx, id, store.PayloadBuildFlightActive); err != nil {
				t.Errorf("post-capacity post-flight cleanup: %v", err)
			}
			t.Logf("ownership_drained=true source_owner_closes=%d", wrapped.closeCalls.Load())
		})
	}
}
