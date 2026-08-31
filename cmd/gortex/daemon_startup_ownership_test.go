package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func TestStartupOwnershipPlanPartitionsEveryPhysicalRegistrationOnce(t *testing.T) {
	ctx := context.Background()
	repos := []config.RepoEntry{
		{Path: "/repo/catalog", Name: "catalog"},
		{Path: "/repo/seed-gap", Name: "seed-gap"},
		{Path: "/repo/legacy", Name: "legacy"},
	}
	plan := buildStartupOwnershipPlan(
		ctx,
		repos,
		func(_ context.Context, path string) bool { return path == "/repo/catalog" },
		func(_ context.Context, path string) bool { return path == "/repo/seed-gap" },
	)

	require.Equal(t, repos, plan.configured)
	require.Equal(t, []string{"/repo/catalog"}, plan.managedPaths)
	require.Equal(t, []config.RepoEntry{
		{Path: "/repo/seed-gap", Name: "seed-gap"},
		{Path: "/repo/legacy", Name: "legacy"},
	}, plan.legacy)
	require.Equal(t, []string{"/repo/seed-gap"}, plan.legacyGitFallbackPaths)

	owners := make(map[string]int, len(repos))
	for _, path := range plan.managedPaths {
		owners[path]++
	}
	for _, entry := range plan.legacy {
		owners[entry.Path]++
	}
	for _, entry := range repos {
		require.Equalf(t, 1, owners[entry.Path], "%s must have exactly one startup owner", entry.Path)
	}
}

func TestStartupOwnershipPlanRetainsCatalogOwnerAfterInventoryLoss(t *testing.T) {
	repos := []config.RepoEntry{{Path: "/repo/disappeared-after-seed"}}
	inventoryCalls := 0
	plan := buildStartupOwnershipPlan(
		context.Background(),
		repos,
		func(context.Context, string) bool { return true },
		func(context.Context, string) bool {
			inventoryCalls++
			return false
		},
	)
	require.Equal(t, []string{"/repo/disappeared-after-seed"}, plan.managedPaths)
	require.Empty(t, plan.legacy)
	require.Zero(t, inventoryCalls, "a published catalog owner must not be reclassified by a later filesystem race")
}

func TestStartupOwnershipPlanWithoutLifecycleKeepsLegacyOwner(t *testing.T) {
	repos := []config.RepoEntry{{Path: "/repo/plain"}, {Path: "/repo/git"}}
	plan := buildStartupOwnershipPlan(context.Background(), repos, nil, nil)
	require.Equal(t, repos, plan.legacy)
	require.Empty(t, plan.managedPaths)
}

func TestStartupOwnershipSeedGapKeepsExactlyOneLegacyOwner(t *testing.T) {
	repos := []config.RepoEntry{{Path: "/repo/seed-gap"}}
	plan := buildStartupOwnershipPlan(
		context.Background(), repos,
		func(context.Context, string) bool { return false },
		func(context.Context, string) bool { return true },
	)
	require.Empty(t, plan.managedPaths)
	require.Equal(t, repos, plan.legacy,
		"inventory proves Git but does not prove that Seed admitted a coordinator")
	require.Equal(t, []string{"/repo/seed-gap"}, plan.legacyGitFallbackPaths)
}

func BenchmarkStartupOwnershipPlan256(b *testing.B) {
	repos := make([]config.RepoEntry, 256)
	for i := range repos {
		repos[i] = config.RepoEntry{Path: fmt.Sprintf("/repo/%03d", i)}
	}
	catalogOwns := func(_ context.Context, path string) bool {
		return path[len(path)-1]%2 == 0
	}
	inventory := func(_ context.Context, path string) bool {
		return path[len(path)-1]%3 == 0
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		plan := buildStartupOwnershipPlan(ctx, repos, catalogOwns, inventory)
		if len(plan.managedPaths)+len(plan.legacy) != len(repos) {
			b.Fatalf("partition size = %d, want %d", len(plan.managedPaths)+len(plan.legacy), len(repos))
		}
	}
}
