package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type checkedRetirementNodeProbe struct {
	projectionStore
	afterNodes func()
}

func (s *checkedRetirementNodeProbe) GetFileNodes(path string) []*graph.Node {
	nodes := s.projectionStore.GetFileNodes(path)
	if s.afterNodes != nil {
		s.afterNodes()
	}
	return nodes
}

func TestCheckedRetirementHintRequiresExactUnchangedProof(t *testing.T) {
	for _, mode := range []string{"checked", "newer_owner", "event_then_legacy_lookup", "divergent_nodes"} {
		t.Run(mode, func(t *testing.T) {
			selectedProjectionRegistry(t)
			fixture := newProjectionFixture(t)
			entry := fixture.edit("a.go", "package p\n\nfunc Published() {}\n")
			require.NoError(t, pendingFileProjections.publish(entry))
			require.NoError(t, fixture.idx.IndexFile(filepath.Join(fixture.dir, "a.go")))
			probe := &checkedRetirementNodeProbe{projectionStore: fixture.g}
			var newer *projectionEntry
			switch mode {
			case "newer_owner":
				newer = entry.committedCopy(entry.commitSeq+1, entry.disk)
				probe.afterNodes = func() { require.NoError(t, pendingFileProjections.publish(newer)) }
			case "event_then_legacy_lookup":
				probe.afterNodes = func() {
					pendingFileProjections.markSuspect(entry.absPath)
					// The real legacy rehash clears suspect for identical bytes.
					// Its success cannot erase the checked retirement's event fence.
					found, err := pendingFileProjections.lookup(entry.absPath)
					require.NoError(t, err)
					require.Same(t, entry, found)
					require.False(t, entry.suspect.Load())
				}
			case "divergent_nodes":
				node := *fixture.g.GetNode("a.go::Published")
				node.StartLine += 100
				fixture.g.AddNode(&node)
			}
			retired := pendingFileProjections.retirePaths(probe, []string{entry.absPath})
			if mode == "newer_owner" {
				require.Zero(t, retired)
				require.Same(t, newer, liveEntry(pendingFileProjections, entry.absPath))
				require.False(t, newer.checkedRetired.Load())
			} else {
				require.Equal(t, 1, retired, "existing retirement/drop behavior remains unchanged")
				require.Nil(t, liveEntry(pendingFileProjections, entry.absPath))
			}
			require.Equal(t, mode == "checked", entry.checkedRetired.Load())
		})
	}
}

func TestCheckedRetirementHintDoesNotFollowNewOwnerOrPermitRepublish(t *testing.T) {
	selectedProjectionRegistry(t)
	fixture := newProjectionFixture(t)
	entry := fixture.edit("a.go", "package p\n\nfunc Published() {}\n")
	require.NoError(t, pendingFileProjections.publish(entry))
	require.NoError(t, fixture.idx.IndexFile(entry.absPath))
	require.Equal(t, 1, pendingFileProjections.retirePaths(fixture.g, []string{entry.absPath}))
	require.True(t, entry.checkedRetired.Load())
	require.ErrorIs(t, pendingFileProjections.publish(entry), projectionSuperseded)
	require.ErrorIs(t, pendingFileProjections.publishReadExpected(entry, projectionReadExpectation{}), projectionSuperseded)
	require.ErrorIs(t, pendingFileProjections.publishCohort([]projectionCohortMember{{Entry: entry}}), projectionSuperseded)
	newer := entry.committedCopy(entry.commitSeq+1, entry.disk)
	require.False(t, newer.checkedRetired.Load(), "the hint belongs only to the checked historical owner")
	require.Equal(t, entry.size, newer.size)
	require.NoError(t, pendingFileProjections.publish(newer))
	require.Same(t, newer, liveEntry(pendingFileProjections, entry.absPath))
	require.True(t, entry.checkedRetired.Load(), "republishing a new owner must not mutate the historical hint")
}
