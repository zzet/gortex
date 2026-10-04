package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCheckoutMutationRefreshPreservesPendingPinnedCommit(t *testing.T) {
	fixture := newPinnedBaseMutationFixture(t)
	mutation, err := fixture.lifecycle.BeginCheckoutMutation(
		t.Context(), fixture.family.checkoutID, fixture.family.worktree, fixture.route.RouteEpoch,
	)
	if err != nil {
		t.Fatalf("admit mutation over routed pinned base: %v", err)
	}
	defer mutation.Close()
	if err := mutation.Prepare(t.Context()); err != nil {
		t.Fatalf("prepare mutation over routed pinned base: %v", err)
	}
	requireWithdrawnPinnedRoute(t, fixture)
	mutatePinnedBaseWorktree(t, fixture.family.worktree)

	cycle, err := mutation.Refresh(t.Context())
	if err != nil {
		t.Fatalf("refresh mutation over routed pinned base: %v (cycle=%+v)", err, cycle)
	}
	if cycle.CommitBuilt || cycle.Recomposed || !cycle.DirtyBuilt || !cycle.BasePinned {
		t.Fatalf("refresh rebuilt the pinned commit layer: %+v", cycle)
	}
	if cycle.CommitGenerationID != fixture.route.CommitGenerationID {
		t.Fatalf("commit generation changed: got %d want %d", cycle.CommitGenerationID, fixture.route.CommitGenerationID)
	}
	after := fixture.family.route()
	if after.State != store_sqlite.RouteActive ||
		after.CommitGenerationID != fixture.route.CommitGenerationID ||
		after.DirtyGenerationID == 0 || after.DirtyGenerationID == fixture.route.DirtyGenerationID {
		t.Fatalf("unexpected refreshed route: before=%+v after=%+v", fixture.route, after)
	}
}

func TestCheckoutMutationRefreshRejectsInvalidPendingPinnedBase(t *testing.T) {
	tests := []struct {
		name       string
		invalidate func(*testing.T, pinnedBaseMutationFixture)
	}{
		{
			name: "released",
			invalidate: func(t *testing.T, fixture pinnedBaseMutationFixture) {
				t.Helper()
				if !fixture.coordinator.RequestBaseRelease(fixture.baseGeneration, "test release") {
					t.Fatal("release request was not accepted")
				}
			},
		},
		{
			name: "failed retained commit",
			invalidate: func(t *testing.T, fixture pinnedBaseMutationFixture) {
				t.Helper()
				undo := doctorGenerationColumn(
					t, fixture.family.storePath, fixture.route.CommitGenerationID,
					"state", string(store_sqlite.ViewGenerationFailed),
				)
				t.Cleanup(undo)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newPinnedBaseMutationFixture(t)
			current, err := fixture.coordinator.primaryBase(t.Context())
			if err != nil {
				t.Fatalf("current primary base: %v", err)
			}
			mutation, err := fixture.lifecycle.BeginCheckoutMutation(
				t.Context(), fixture.family.checkoutID, fixture.family.worktree, fixture.route.RouteEpoch,
			)
			if err != nil {
				t.Fatalf("admit mutation over routed pinned base: %v", err)
			}
			defer mutation.Close()
			if err := mutation.Prepare(t.Context()); err != nil {
				t.Fatalf("prepare mutation over routed pinned base: %v", err)
			}
			requireWithdrawnPinnedRoute(t, fixture)
			tt.invalidate(t, fixture)
			mutatePinnedBaseWorktree(t, fixture.family.worktree)

			cycle, err := mutation.Refresh(t.Context())
			if err != nil {
				t.Fatalf("refresh after invalidating pin: %v (cycle=%+v)", err, cycle)
			}
			if !cycle.CommitBuilt || !cycle.DirtyBuilt || cycle.BasePinned {
				t.Fatalf("invalid pin did not rebuild over current base: %+v", cycle)
			}
			after := fixture.family.route()
			if after.State != store_sqlite.RouteActive ||
				after.CommitGenerationID == fixture.route.CommitGenerationID || after.DirtyGenerationID == 0 {
				t.Fatalf("invalid pin left an unexpected route: before=%+v after=%+v", fixture.route, after)
			}
			commit, found := fixture.family.generation(after.CommitGenerationID)
			if !found || commit.BaseGenerationID != current.generationID {
				t.Fatalf("rebuilt commit does not use current base %d: found=%v commit=%+v", current.generationID, found, commit)
			}
		})
	}
}

func requireWithdrawnPinnedRoute(t *testing.T, fixture pinnedBaseMutationFixture) {
	t.Helper()
	pending := fixture.family.route()
	if pending.State != store_sqlite.RoutePending ||
		pending.CommitGenerationID != fixture.route.CommitGenerationID || pending.DirtyGenerationID != 0 {
		t.Fatalf("Prepare did not leave Pending/COMMIT/0: before=%+v after=%+v", fixture.route, pending)
	}
}

func mutatePinnedBaseWorktree(t *testing.T, root string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "*.go"))
	if err != nil {
		t.Fatalf("find fixture Go file: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("fixture has no Go file to mutate")
	}
	target := files[0]
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read fixture Go file: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat fixture Go file: %v", err)
	}
	after := append(append([]byte(nil), before...), []byte("\n// pending pinned-base publication\n")...)
	if err := os.WriteFile(target, after, info.Mode().Perm()); err != nil {
		t.Fatalf("mutate fixture Go file: %v", err)
	}
}
