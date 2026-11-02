package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProjectionCohortRefusalLeavesEveryOldPointerAndBudget(t *testing.T) {
	for _, failure := range []string{"bytes", "files", "stale pointer", "older sequence", "duplicate"} {
		t.Run(failure, func(t *testing.T) {
			registry := newPendingProjections()
			registry.enabled = true
			oldA := &projectionEntry{key: "a", graphPath: "a.go", commitSeq: 1, size: 10}
			oldB := &projectionEntry{key: "b", graphPath: "b.go", commitSeq: 1, size: 10}
			require.NoError(t, registry.publish(oldA))
			require.NoError(t, registry.publish(oldB))
			if failure == "files" {
				for i := 2; i < projectionMaxFiles; i++ {
					key := fmt.Sprintf("f%d", i)
					require.NoError(t, registry.publish(&projectionEntry{key: key, graphPath: key, size: 1}))
				}
			}
			first := &projectionEntry{key: "a", graphPath: "a.go", commitSeq: 2, size: 20}
			second := &projectionEntry{key: "b", graphPath: "b.go", commitSeq: 2, size: 20}
			members := []projectionCohortMember{{Entry: first, Expected: oldA}, {Entry: second, Expected: oldB}}
			switch failure {
			case "bytes":
				second.size = projectionMaxBytes
			case "files":
				members = append(members, projectionCohortMember{Entry: &projectionEntry{key: "new", graphPath: "new.go", commitSeq: 2, size: 1}})
			case "stale pointer":
				members[1].Expected = &projectionEntry{}
			case "older sequence":
				second.commitSeq = 0
			case "duplicate":
				second.key = "a"
			}
			before := registry.snap.Load()
			require.Error(t, registry.publishCohort(members))
			require.Same(t, before, registry.snap.Load(), "no member or byte accounting can advance on a cohort refusal")
			require.Same(t, oldA, registry.snap.Load().entries["a"])
			require.Same(t, oldB, registry.snap.Load().entries["b"])
		})
	}
}

func TestProjectionCohortPublishesOneCompleteSnapshotAndPreservesSuspect(t *testing.T) {
	registry := newPendingProjections()
	registry.enabled = true
	old := &projectionEntry{key: "a", graphPath: "a.go", commitSeq: 1, size: 10}
	old.suspect.Store(true)
	require.NoError(t, registry.publish(old))
	before := registry.snap.Load()
	first := &projectionEntry{key: "a", graphPath: "a.go", commitSeq: 2, size: 20}
	second := &projectionEntry{key: "b", graphPath: "b.go", commitSeq: 2, size: 30}
	require.NoError(t, registry.publishCohort([]projectionCohortMember{{Entry: first, Expected: old}, {Entry: second}}))
	after := registry.snap.Load()
	require.NotSame(t, before, after)
	require.EqualValues(t, 50, after.bytes)
	require.Same(t, first, after.entries["a"])
	require.Same(t, second, after.entries["b"])
	require.True(t, first.suspect.Load())
	require.Len(t, before.entries, 1)
	require.Same(t, old, before.entries["a"])
}

func TestProjectionCohortBudgetUsesFinalCombinedReplacement(t *testing.T) {
	registry := newPendingProjections()
	registry.enabled = true
	oldA := &projectionEntry{key: "a", graphPath: "a.go", commitSeq: 1, size: projectionMaxBytes - 20}
	oldB := &projectionEntry{key: "b", graphPath: "b.go", commitSeq: 1, size: 20}
	require.NoError(t, registry.publish(oldA))
	require.NoError(t, registry.publish(oldB))
	nextA := &projectionEntry{key: "a", graphPath: "a.go", commitSeq: 2, size: projectionMaxBytes - 10}
	nextB := &projectionEntry{key: "b", graphPath: "b.go", commitSeq: 2, size: 10}
	require.NoError(t, registry.publishCohort([]projectionCohortMember{{Entry: nextA, Expected: oldA}, {Entry: nextB, Expected: oldB}}))
	require.EqualValues(t, projectionMaxBytes, registry.snap.Load().bytes)
}
