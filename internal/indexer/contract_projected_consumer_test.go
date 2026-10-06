package indexer

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestContractProjectedConsumerPublishesTheMixedAcceptedAttachment(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	idx, store := newSQLiteIndexer(t)
	idx.SetRepoPrefix("fixture")
	root := t.TempDir()
	src := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))
	writeFile(t, filepath.Join(root, "src", "parser.c"), string(src))
	writeFile(t, filepath.Join(root, "routes.go"), "package fixture\nfunc register(router *Router) { router.GET(\"/with-generated-parser\", serve) }\nfunc serve() {}\n")
	backend, err := newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
	require.NoError(t, err)
	idx.contractCoreInputs, err = newContractCoreInputJournal(ctx, backend)
	require.NoError(t, err)
	result, err := idx.IndexCtx(ctx, root)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	file := store.GetNode("fixture/src/parser.c")
	require.NotNil(t, file)
	require.Equal(t, true, file.Meta[generatedParserProjectionMetaKey])
	mi := NewMultiIndexer(store, idx.registry, search.NewNull(), nil, zap.NewNop())
	mi.repos["fixture"] = &RepoMetadata{RepoPrefix: "fixture", RootPath: root}
	mi.indexers["fixture"] = idx
	leases := graphview.NewLeaseManager()
	registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "fixture", RootIdentity: root, Incarnation: t.Name()}, nil)
	require.NoError(t, err)
	_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "accepted-projected-core")
	require.NoError(t, err)
	mi.SetOutputGenerationAuthority(NewOutputGenerationAuthority(leases))
	materializer := &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases}
	var admission sync.Mutex
	var jobs sync.WaitGroup
	options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: materializer, MultiIndexer: mi, Registry: idx.registry, Config: idx.config, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }, BaselineAdmissionMu: &admission, BaselineJobs: &jobs}
	coordinator, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: store, Leases: leases, Registry: idx.registry, Config: idx.config, Capture: NewContractFollowupCapture(options), ReconcileBaseline: NewContractBaselineReconciler(options), Yield: options.Yield})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, coordinator.Close()); jobs.Wait() })
	mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{Published: coordinator.Published})
	pending, err := materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.False(t, pending.State.Accepted)
	admitted, err := coordinator.Request(ctx, nil, pending, "fixture", "")
	require.NoError(t, err)
	require.True(t, admitted)
	require.NoError(t, coordinator.WaitChange(ctx, graph.ContractAttachmentKey{RepoPrefix: "fixture", InputVersion: pending.State.InputVersion, InputFingerprint: pending.State.InputFingerprint}))
	jobs.Wait()
	accepted, err := materializer.CaptureContractInputs(ctx, nil, "fixture", "")
	require.NoError(t, err)
	require.True(t, accepted.State.Accepted)
	stored, known, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/src/parser.c")
	require.NoError(t, err)
	require.True(t, known)
	require.NotNil(t, stored)
	require.True(t, stored.Accepted)
	var receipt contractBoundaryReceipt
	require.NoError(t, json.Unmarshal(stored.Payload, &receipt))
	require.Equal(t, contractInputHash(src), receipt.Source)
	require.NotEmpty(t, receipt.ProducedInputs[contractBoundaryLookupKey("fixture", "symbol_name", "tree_sitter_demo")])
	require.Equal(t, true, store.GetNode("fixture/src/parser.c").Meta[generatedParserProjectionMetaKey], "repair must retain the actual accepted core projection")

	admitted, err = coordinator.Request(ctx, nil, accepted, "fixture", "")
	require.NoError(t, err, "capture must recognize the actual accepted projected receipt policy")
	require.True(t, admitted)
	key := graph.ContractAttachmentKey{RepoPrefix: "fixture", InputVersion: accepted.State.InputVersion, InputFingerprint: accepted.State.InputFingerprint}
	for {
		analysis, openErr := materializer.OpenContractAnalysisForInputs(ctx, nil, accepted)
		if openErr == nil {
			contracts, readErr := analysis.NodesByKindsContext(ctx, []graph.NodeKind{graph.KindContract})
			require.NoError(t, readErr)
			var ids []string
			for _, contract := range contracts {
				ids = append(ids, contract.ID)
			}
			require.Contains(t, ids, "http::GET::/with-generated-parser")
			require.NoError(t, analysis.Validate(ctx))
			analysis.Close()
			break
		}
		require.Equal(t, graphview.CodeRequiredCapabilityIncomplete, graphview.CodeOf(openErr))
		require.NoError(t, coordinator.WaitChange(ctx, key))
	}
	require.NotNil(t, store.GetNode("fixture/src/parser.c::tree_sitter_demo"), "selected public declaration evidence remains in the core")
}

func TestContractProjectedConsumerProofPreservesSelectedEvidence(t *testing.T) {
	idx, store := newSQLiteIndexer(t)
	idx.SetRepoPrefix("fixture")
	src := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")
	result, recognized := generatedTreeSitterParserProjection("src/parser.c", "c", src)
	require.True(t, recognized)
	idx.applyRepoPrefix(result.Nodes, result.Edges)
	// Real persisted numeric metadata is decoded as JSON numbers. Keep the
	// selected enrichment and resolved external include, rather than deleting
	// these fields to make a raw-extraction differential pass.
	result.Nodes[1].Meta["selected_enrichment"] = "preserved"
	result.Edges[0].To = "external::tree_sitter/parser.h"
	result.Edges[0].Origin = graph.OriginASTResolved
	require.NoError(t, store.AddBatchChecked(result.Nodes, result.Edges))
	file := ContractFollowupFile{RepoPrefix: "fixture", Path: "fixture/src/parser.c", Language: "c"}
	projection, err := store.LayerContractFileProjectionContext(t.Context(), "fixture", []string{file.Path})
	require.NoError(t, err)
	nodes := projection.FileNodes[file.Path]
	var ids []string
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	outgoing, truncated, err := graph.GetOutEdgesByNodeIDsWithMetadataContext(t.Context(), store, ids, graph.ContractProjectionRowLimit)
	require.NoError(t, err)
	require.False(t, truncated)
	var edges []*graph.Edge
	for _, id := range ids {
		edges = append(edges, outgoing[id]...)
	}
	require.True(t, idx.contractGeneratedCoreProof(file, src, nodes, edges, store))
	for _, node := range nodes {
		if node.Kind == graph.KindFunction {
			require.Equal(t, "preserved", node.Meta["selected_enrichment"])
		}
	}
	// The ordinary hash remains byte-for-byte identical to the pre-projection
	// policy, while the projected hash travels through the existing Policy field.
	ordinary, err := contractFollowupPolicy(idx, "c")
	require.NoError(t, err)
	oldJSON, err := json.Marshal(struct {
		Config                                    any
		EventBus                                  any
		Parser, PostExtraction                    int
		ContractPolicy, RecordPolicy, MatchPolicy string
	}{contractExtractionSettings(idx.config), idx.eventBusBoundaries(), extractorVersionForLang("c"), postExtractionPolicyVersion, contractExtractionPolicyVersion, contracts.RecordFingerprintVersion, contracts.MatchDependencyKeyVersion})
	require.NoError(t, err)
	require.Equal(t, contractInputHash(oldJSON), ordinary)
	projected, err := contractBoundaryPolicy(idx, "c", generatedParserProjectionPolicyVersion)
	require.NoError(t, err)
	mode, err := idx.contractFollowupPolicyMode("c", projected)
	require.NoError(t, err)
	require.True(t, mode)
	mode, err = idx.contractFollowupPolicyMode("c", ordinary)
	require.NoError(t, err)
	require.False(t, mode)
	_, err = idx.contractFollowupPolicyMode("c", "unrecognized-policy")
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	for _, mode := range []string{"missing_public", "extra_public", "wrong_public", "wrong_span", "stale_version", "missing_marker", "wrong_signature", "missing_include", "wrong_include", "malformed_source", "full_parser_config"} {
		t.Run(mode, func(t *testing.T) {
			encoded, err := json.Marshal(nodes)
			require.NoError(t, err)
			var selected []*graph.Node
			require.NoError(t, json.Unmarshal(encoded, &selected))
			encoded, err = json.Marshal(edges)
			require.NoError(t, err)
			var selectedEdges []*graph.Edge
			require.NoError(t, json.Unmarshal(encoded, &selectedEdges))
			candidateSource := src
			var public *graph.Node
			for _, node := range selected {
				if node.Kind == graph.KindFunction {
					public = node
				}
			}
			require.NotNil(t, public)
			switch mode {
			case "missing_public":
				for i, node := range selected {
					if node.Kind == graph.KindFunction {
						selected = append(selected[:i], selected[i+1:]...)
						break
					}
				}
			case "extra_public":
				selected = append(selected, &graph.Node{ID: "fixture/src/parser.c::extra", Kind: graph.KindFunction, Name: "extra"})
			case "wrong_public":
				public.Name = "different"
			case "wrong_span":
				public.EndLine++
			case "stale_version":
				public.Meta[generatedParserProjectionVersionMetaKey] = generatedParserProjectionPolicyVersion - 1
			case "missing_marker":
				delete(public.Meta, generatedParserProjectionMetaKey)
			case "wrong_signature":
				public.Meta["signature"] = "different"
			case "missing_include":
				for i, edge := range selectedEdges {
					if edge.Kind == graph.EdgeImports {
						selectedEdges = append(selectedEdges[:i], selectedEdges[i+1:]...)
						break
					}
				}
			case "wrong_include":
				for _, edge := range selectedEdges {
					if edge.Kind == graph.EdgeImports {
						edge.To = "external::wrong.h"
					}
				}
			case "malformed_source":
				candidateSource = []byte(strings.ReplaceAll(string(src), "#define STATE_COUNT", "#define MISSING_MARKER"))
			case "full_parser_config":
				idx.config.IndexGeneratedParsers = true
				defer func() { idx.config.IndexGeneratedParsers = false }()
			}
			require.False(t, idx.contractGeneratedCoreProof(file, candidateSource, selected, selectedEdges, store), "unproved selected evidence cannot certify a projection")
		})
	}
}
