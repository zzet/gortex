//go:build linux || darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func lapsedTopologyFixture(t *testing.T) (discoveryTopology, []Corpus) {
	t.Helper()
	var corpora []Corpus
	topology := discoveryTopology{Mode: "literal_lapsed_v1"}
	for i, kind := range []string{"gortex", "pdf", "sql", "go", "go", "pdf", "sql"} {
		corpus := gitFixture(t)
		corpus.Label = fmt.Sprintf("fixture-%d", i)
		corpora = append(corpora, corpus)
		role := "primary"
		if i >= 3 {
			role = "dedicated"
		}
		topology.Roots = append(topology.Roots, discoveryTopologyRoot{ID: corpus.Label, Role: role, Kind: kind, CorpusLabel: corpus.Label, Family: corpus.Label})
	}
	for i := range 22 {
		role := "linked"
		if i >= 20 {
			role = "stale"
		}
		topology.Roots = append(topology.Roots, discoveryTopologyRoot{ID: fmt.Sprintf("linked-%02d", i), Role: role, Kind: "gortex", Family: corpora[0].Label, Nested: i < 5})
	}
	return topology, corpora
}

func TestLapsedDiscoveryDeclarationRefusesIncompleteAndAmbiguousRoles(t *testing.T) {
	topology, corpora := lapsedTopologyFixture(t)
	require.NoError(t, topology.validate(corpora))
	for _, name := range []string{"no_opt_in", "duplicate", "missing", "escape", "foreign_family", "stale_replaces_live", "unused_corpus", "support_alias"} {
		t.Run(name, func(t *testing.T) {
			bad := topology
			bad.Roots = append([]discoveryTopologyRoot(nil), topology.Roots...)
			switch name {
			case "no_opt_in":
				bad.Mode = ""
			case "duplicate":
				bad.Roots[8].ID = bad.Roots[7].ID
			case "missing":
				bad.Roots = bad.Roots[:28]
			case "escape":
				bad.Roots[7].ID = "../escape"
			case "foreign_family":
				bad.Roots[7].Family = "absent"
			case "stale_replaces_live":
				bad.Roots[7].Role = "stale"
			case "unused_corpus":
				bad.Roots[3].CorpusLabel = corpora[0].Label
			case "support_alias":
				bad.Roots[7].ID = "support-" + bad.Roots[3].ID
			}
			require.Error(t, bad.validate(corpora))
		})
	}
}

func TestLapsedDiscoveryRejectsImplicitBroaderLegacyPreparation(t *testing.T) {
	_, corpora := lapsedTopologyFixture(t)
	scratch := t.TempDir()
	dest := filepath.Join(scratch, "prepared.json")
	err := prepareDiscoveryState(t.Context(), runOptions{Binary: "unused", Scratch: scratch, Discovery: discoveryOptions{Prepare: dest}}, corpora)
	require.Error(t, err, "a broader manifest must never silently prepare only its first two corpora")
	require.NoFileExists(t, dest)
}

func TestLapsedDiscoveryProtectsEveryDedicatedSourceRoot(t *testing.T) {
	topology, corpora := lapsedTopologyFixture(t)
	output := t.TempDir()
	declaration := filepath.Join(output, "topology.json")
	require.NoError(t, discoveryWriteJSON(declaration, topology))
	scratch := filepath.Join(corpora[3].Source, "must-remain-absent")
	err := prepareDiscoveryState(t.Context(), runOptions{Binary: "unused", Scratch: scratch, Discovery: discoveryOptions{Prepare: filepath.Join(output, "prepared.json"), Topology: declaration}}, corpora)
	require.Error(t, err)
	require.NoDirExists(t, scratch)
}

func TestLapsedDiscoveryPreparationRecordsRolesWithoutClaimingGraphIdentity(t *testing.T) {
	topology, corpora := lapsedTopologyFixture(t)
	scratch := t.TempDir()
	declaration := filepath.Join(scratch, "topology.json")
	require.NoError(t, discoveryWriteJSON(declaration, topology))
	dest := filepath.Join(scratch, "prepared.json")
	o := runOptions{Binary: "unused", Scratch: scratch, Discovery: discoveryOptions{Prepare: dest, Topology: declaration}}
	require.NoError(t, prepareDiscoveryState(t.Context(), o, corpora))
	var d discoverySeedDescriptor
	require.NoError(t, discoveryReadJSON(dest, &d))
	require.Equal(t, "PREPARED_ROOTS_NO_STORE", d.State)
	require.Equal(t, "literal_lapsed_v1", d.Topology)
	require.Equal(t, 27, d.LiveCount)
	require.Equal(t, 2, d.StaleCount)
	require.Equal(t, 4, d.DedicatedCount)
	require.Equal(t, 3, d.PrimaryCount)
	require.Len(t, d.Repos, 29)
	require.Len(t, d.SupportRoots, 4)
	encoded, err := os.ReadFile(dest)
	require.NoError(t, err)
	var wire struct {
		Repos []map[string]any `json:"repositories"`
	}
	require.NoError(t, json.Unmarshal(encoded, &wire))
	for _, row := range wire.Repos {
		require.Contains(t, row, "live", "the external owner needs explicit false for stale rows")
		require.Contains(t, row, "configured")
		require.Contains(t, row, "observed_graph_owner_checkout_id")
		require.Nil(t, row["observed_graph_owner_checkout_id"])
	}
	commons := map[string]bool{}
	for _, row := range d.Repos {
		require.NotEmpty(t, row.CommonDir)
		require.NotEmpty(t, row.Repo.Corpus.Commit)
		require.Nil(t, row.GraphOwnerCheckoutID, "preparation cannot observe a graph owner")
		if row.Role == "dedicated" {
			require.False(t, commons[row.CommonDir])
			commons[row.CommonDir] = true
			require.Equal(t, "base", row.ViewKind, "a configured dedicated graph owner serves its indexed base graph")
			require.Equal(t, "linked_worktree", row.CheckoutKind)
			require.True(t, row.Configured)
		}
	}
	for _, support := range d.SupportRoots {
		require.True(t, inside(d.Root, support.Path))
		require.True(t, commons[support.CommonDir])
		require.NoFileExists(t, filepath.Join(support.Path, corpora[0].File), "support clone must have no checkout")
	}
	require.NoFileExists(t, filepath.Join(d.Root, "store.db"))
	config, err := os.ReadFile(filepath.Join(d.Root, "config", "gortex", "config.yaml"))
	require.NoError(t, err)
	var boot struct {
		Repos []struct {
			Path string `json:"path"`
		} `json:"repos"`
	}
	require.NoError(t, json.Unmarshal(config, &boot))
	paths := map[string]bool{}
	for _, repo := range boot.Repos {
		paths[repo.Path] = true
	}
	require.Len(t, paths, 7)
	for _, row := range d.Repos {
		require.Equal(t, row.Configured, paths[row.Repo.Path])
	}
}

func TestLapsedDiscoveryFlagIsPreparationOnly(t *testing.T) {
	require.Error(t, (discoveryOptions{Topology: "topology.json"}).validate(checkoutDiscoveryScenario, 1))
	require.Error(t, (discoveryOptions{Topology: "topology.json", Prepare: "out", ExistingWorktrees: 20}).validate(checkoutDiscoveryScenario, 1))
	require.NoError(t, (discoveryOptions{Topology: "topology.json", Prepare: "out"}).validate(checkoutDiscoveryScenario, 1))
}

func TestDiscoveryLegacyDescriptorKeepsWireShape(t *testing.T) {
	encoded, err := json.Marshal(discoverySeedRepo{Role: "primary"})
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	require.NotContains(t, wire, "configured", "old strict consumers must still accept the default descriptor")
	require.NotContains(t, wire, "live")
	require.NotContains(t, wire, "observed_graph_owner_checkout_id")
}
