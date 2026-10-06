package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestContractFollowupPublishesCanonicalColdFixtureFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	idx, store := newSQLiteIndexer(t)
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix("fixture")
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "testdata"), 0o755))
	writeFile(t, filepath.Join(root, "testdata", "data.json"), `{"fixture_key":"accepted-json"}`)
	writeFile(t, filepath.Join(root, "testdata", "endpoint.go"), "package fixture\nfunc register(router *Router) { router.GET(\"/fixture-owner\", serve) }\nfunc serve() {}\n")
	backend, err := newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
	require.NoError(t, err)
	idx.contractCoreInputs, err = newContractCoreInputJournal(ctx, backend)
	require.NoError(t, err)
	result, err := idx.IndexCtx(ctx, root)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	for _, path := range []string{"fixture/testdata/data.json", "fixture/testdata/endpoint.go"} {
		node := store.GetNode(path)
		require.NotNil(t, node)
		require.Equal(t, graph.KindFixture, node.Kind, "real cold classification replaces the file kind without creating another owner")
		require.Equal(t, path, node.ID)
		require.Equal(t, path, node.FilePath)
		require.Equal(t, true, node.Meta["fixture"])
	}
	mi := NewMultiIndexer(store, idx.registry, search.NewNull(), nil, zap.NewNop())
	mi.repos["fixture"] = &RepoMetadata{RepoPrefix: "fixture", RootPath: root}
	mi.indexers["fixture"] = idx
	leases := graphview.NewLeaseManager()
	registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "fixture", RootIdentity: root, Incarnation: t.Name()}, nil)
	require.NoError(t, err)
	_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "accepted-fixture-core")
	require.NoError(t, err)
	mi.SetOutputGenerationAuthority(NewOutputGenerationAuthority(leases))
	materializer := &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases}
	options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: materializer, MultiIndexer: mi, Registry: idx.registry, Config: idx.config, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
	require.NoError(t, reconcilePrimaryContractBaseline(ctx, options, "fixture"))
	inputs, err := materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.True(t, inputs.State.Accepted)
	snapshot, targets, err := NewContractFollowupCapture(options)(ctx, nil, inputs, "fixture", "")
	require.NoError(t, err)
	defer snapshot.Release()
	require.Len(t, snapshot.Files, 2, "both JSON and Go fixtures stay in the exact accepted census")
	require.Len(t, targets, 1)
	snapshot.Work = targets[0].Work
	_, payload, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "fixture-contract", LayerID: "fixture-owner", GenerationKind: "contract_analysis", ConfigHash: "fixture-config", ExtractorVersions: `{"go":"1","json":"1"}`, ResolverVersion: "contract-only", CreatedAt: 1})
	require.NoError(t, err)
	request := ContractFollowupRequest{RebuildReason: ContractFollowupColdBaseline, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: idx.registry, Config: idx.config, RepoConfigs: snapshot.RepoConfigs, Leases: leases, Logger: zap.NewNop(), Yield: options.Yield}
	report, err := RunContractFollowup(ctx, request)
	require.NoError(t, err, "a real canonical fixture-file owner must serve complete contract evidence")
	require.True(t, report.Published)
	analysis, err := materializer.OpenContractAnalysisForInputs(ctx, nil, inputs)
	require.NoError(t, err)
	defer analysis.Close()
	contracts, err := analysis.NodesByKindsContext(ctx, []graph.NodeKind{graph.KindContract})
	require.NoError(t, err)
	var ids []string
	for _, node := range contracts {
		ids = append(ids, node.ID)
	}
	require.Contains(t, ids, "http::GET::/fixture-owner", "fixture source contracts must remain nonempty, not silently discarded")
	require.NoError(t, analysis.Validate(ctx))
	require.Equal(t, graph.KindFixture, store.GetNode("fixture/testdata/data.json").Kind, "analysis must not synthesize or rewrite an ordinary file twin")
}

func TestContractFollowupFixtureOwnerRefusesNoncanonicalEvidence(t *testing.T) {
	for _, mode := range []string{"canonical", "wrong_id", "missing_marker", "false_marker", "wrong_path", "wrong_repo", "nonfixture_path", "declaration", "absent"} {
		t.Run(mode, func(t *testing.T) {
			path := "fixture/testdata/data.json"
			if mode == "nonfixture_path" {
				path = "fixture/assets/data.json"
			}
			owner := &graph.Node{ID: path, Kind: graph.KindFixture, RepoPrefix: "fixture", FilePath: path, Meta: map[string]any{"fixture": true}}
			var rows ContractFollowupCoreFile
			switch mode {
			case "wrong_id":
				owner.ID += "::declaration"
			case "missing_marker":
				owner.Meta = nil
			case "false_marker":
				owner.Meta["fixture"] = false
			case "wrong_path":
				owner.FilePath = "fixture/testdata/other.json"
			case "wrong_repo":
				owner.RepoPrefix = "other"
			case "declaration":
				owner.Kind = graph.KindVariable
			}
			if mode != "absent" {
				rows.Nodes = []*graph.Node{owner}
			}
			scratch := newFTSStore(t)
			evidence := &contractFollowupEvidence{Store: scratch, scratch: scratch, ctx: t.Context(), allowedRepos: map[string]bool{"fixture": true}, files: map[string]ContractFollowupFile{path: {RepoPrefix: "fixture", Path: path}}, readCore: func(context.Context, ContractFollowupFile) (ContractFollowupCoreFile, error) { return rows, nil }}
			nodes, edges, err := evidence.loadFile(path)
			if mode == "canonical" {
				require.NoError(t, err)
				require.Len(t, nodes, 1)
				require.Equal(t, graph.KindFixture, nodes[0].Kind)
				require.Equal(t, path, nodes[0].ID)
				return
			}
			require.Error(t, err)
			require.Nil(t, nodes)
			require.Nil(t, edges)
			require.Nil(t, scratch.GetNode(owner.ID), "refused evidence cannot seed a partial analysis")
		})
	}
}
