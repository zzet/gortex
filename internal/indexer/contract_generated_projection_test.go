package indexer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestContractGeneratedProjectionRetainsAcceptedPublicDependencies(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.SetRepoPrefix("fixture")
	idx.registry.Register(languages.NewCExtractor())
	t.Cleanup(idx.Close)
	_, configured := idx.buildPerFileContractExtractors()
	require.Empty(t, configured["c"], "the accepted inert-record proof requires no applicable configured extractor")
	src := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")
	for _, prefixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "prefixed"}[prefixed], func(t *testing.T) {
			result, recognized := generatedTreeSitterParserProjection("src/parser.c", "c", src)
			require.True(t, recognized)
			if prefixed {
				idx.applyRepoPrefix(result.Nodes, result.Edges)
			}
			receipt, err := idx.collectContractBoundaryReceipt(t.Context(), "fixture/src/parser.c", "c", src, result)
			require.NoError(t, err)
			require.Equal(t, contractInputHash(src), receipt.Source, "the complete accepted source remains fenced")
			require.Empty(t, receipt.Groups)
			require.Empty(t, receipt.HandlerInputs)
			require.NotEmpty(t, receipt.ProducedInputs[contractBoundaryLookupKey("fixture", "symbol_name", "tree_sitter_demo")])
			require.NotEmpty(t, receipt.ProducedInputs[contractBoundaryLookupKey("fixture", "symbol_id", "fixture/src/parser.c::tree_sitter_demo")])
			require.Equal(t, receipt.ProducedInputs[contractBoundaryLookupKey("fixture", "symbol_name", "tree_sitter_demo")], receipt.ProducedInputs[contractBoundaryLookupKey("*", "symbol_name", "tree_sitter_demo")], "foreign lookup dependencies remain observable")
			// The projection policy is an additional receipt fence. A full-parse
			// receipt with the same config must not share this policy identity.
			fullPolicy, err := json.Marshal(struct {
				Config                                    any
				EventBus                                  any
				Parser, PostExtraction                    int
				ContractPolicy, RecordPolicy, MatchPolicy string
			}{contractExtractionSettings(idx.config), idx.eventBusBoundaries(), extractorVersionForLang("c"), postExtractionPolicyVersion, contractExtractionPolicyVersion, contracts.RecordFingerprintVersion, contracts.MatchDependencyKeyVersion})
			require.NoError(t, err)
			require.NotEqual(t, contractInputHash(fullPolicy), receipt.Policy)
			changed := append(append([]byte(nil), src...), []byte("\n/* changed private table input */\n")...)
			file := ContractFollowupFile{RepoPrefix: "fixture", Path: "fixture/src/parser.c", Language: "c"}
			next, err := collectContractBaselineAcceptedReceipt(t.Context(), idx, file, ContractAcceptedSource{Bytes: changed, SourceFingerprint: contractInputHash(changed)})
			require.NoError(t, err)
			require.NotEqual(t, receipt.Source, next.Source)
			require.Equal(t, receipt.Policy, next.Policy)
			require.Equal(t, receipt.ProducedInputs, next.ProducedInputs, "private table changes do not invent public declaration changes")
			renamed := []byte(strings.ReplaceAll(string(src), "tree_sitter_demo", "tree_sitter_renamed"))
			next, err = collectContractBaselineAcceptedReceipt(t.Context(), idx, file, ContractAcceptedSource{Bytes: renamed, SourceFingerprint: contractInputHash(renamed)})
			require.NoError(t, err)
			require.NotContains(t, next.ProducedInputs, contractBoundaryLookupKey("fixture", "symbol_name", "tree_sitter_demo"))
			require.NotEmpty(t, next.ProducedInputs[contractBoundaryLookupKey("fixture", "symbol_name", "tree_sitter_renamed")])
			changedBody := []byte(strings.ReplaceAll(string(src), "return &language;", "return &other_language;"))
			next, err = collectContractBaselineAcceptedReceipt(t.Context(), idx, file, ContractAcceptedSource{Bytes: changedBody, SourceFingerprint: contractInputHash(changedBody)})
			require.NoError(t, err)
			require.NotEqual(t, receipt.ProducedInputs[contractBoundaryLookupKey("fixture", "symbol_id", "fixture/src/parser.c::tree_sitter_demo")], next.ProducedInputs[contractBoundaryLookupKey("fixture", "symbol_id", "fixture/src/parser.c::tree_sitter_demo")], "accepted public body changes remain dependency inputs")
		})
	}
}

func TestContractGeneratedProjectionRefusesUnprovedAcceptance(t *testing.T) {
	for _, mode := range []string{"malformed_source", "wrong_language", "stale_policy", "missing_public_entry", "extra_declaration", "changed_edge", "private_constants", "applicable_extractor", "full_parser_config", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			idx := newTestIndexer(graph.New())
			idx.SetRepoPrefix("fixture")
			t.Cleanup(idx.Close)
			src := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")
			result, recognized := generatedTreeSitterParserProjection("src/parser.c", "c", src)
			require.True(t, recognized)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			language := "c"
			switch mode {
			case "malformed_source":
				src = []byte(strings.ReplaceAll(string(src), "#define STATE_COUNT", "#define MISSING_MARKER"))
			case "wrong_language":
				language = "cpp"
			case "stale_policy":
				result.Nodes[0].Meta[generatedParserProjectionVersionMetaKey] = generatedParserProjectionPolicyVersion - 1
			case "missing_public_entry":
				result.Nodes = result.Nodes[:1]
			case "extra_declaration":
				result.Nodes = append(result.Nodes, &graph.Node{ID: "src/parser.c::private", Kind: graph.KindFunction, Name: "private", StartLine: 1, EndLine: 1})
			case "changed_edge":
				result.Edges[0].To = "unresolved::import::different.h"
			case "private_constants":
				result.ConstValues = []parser.ConstValue{{NodeID: "src/parser.c::private", Value: "omitted"}}
			case "applicable_extractor":
				_, supported := idx.contractGeneratedProjectionPolicy("fixture/src/parser.c", language, src, result, 1)
				require.False(t, supported, "any configured applicable extractor requires accepted complete syntax")
				return
			case "full_parser_config":
				idx.config.IndexGeneratedParsers = true
			case "canceled":
				cancel()
			}
			receipt, err := idx.collectContractBoundaryReceipt(ctx, "fixture/src/parser.c", language, src, result)
			require.Error(t, err)
			require.Empty(t, receipt.Source, "a refusal cannot yield partial receipt authority")
			if mode == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

func TestContractBaselineRepairsTheAcceptedGeneratedParserNamespace(t *testing.T) {
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
}
