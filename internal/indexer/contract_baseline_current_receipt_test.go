package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

type currentReceiptParseSpy struct {
	parser.Extractor
	reads  atomic.Int64
	forbid atomic.Bool
}

func (s *currentReceiptParseSpy) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	return s.ExtractWithOptions(path, src, parser.ExtractionOptions{})
}

func (s *currentReceiptParseSpy) ExtractWithOptions(path string, src []byte, opts parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	s.reads.Add(1)
	if s.forbid.Load() {
		return nil, errors.New("current accepted receipt must not be extracted again")
	}
	return parser.Extract(s.Extractor, path, src, opts)
}

func currentReceiptBaselineFixture(t *testing.T, helpers []string) (ContractFollowupCaptureOptions, *currentReceiptParseSpy, *Indexer, *graph.ContractBoundaryReceipt) {
	t.Helper()
	ctx := t.Context()
	idx, store := newSQLiteIndexer(t)
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix("fixture")
	opts := parser.NewExtractionOptions(helpers)
	idx.extractionOptions.Store(&opts)
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	spy := &currentReceiptParseSpy{Extractor: ext}
	idx.registry.Register(spy)
	root := t.TempDir()
	idx.initializeExtractionOptions(root)
	idx.extractionOptions.Store(&opts)
	writeFile(t, filepath.Join(root, "routes.go"), "package fixture\nconst route = \"/reused-current\"\nfunc register(router *Router) { router.GET(route, serve) }\nfunc serve() {}\n")
	backend, err := newPrimaryContractCoreStorageBackend(ctx, store, "fixture")
	require.NoError(t, err)
	idx.contractCoreInputs, err = newContractCoreInputJournal(ctx, backend)
	require.NoError(t, err)
	result, err := idx.IndexCtx(ctx, root)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	original, known, err := store.ContractBoundaryReceiptContext(ctx, "fixture", "", "fixture/routes.go")
	require.NoError(t, err)
	require.True(t, known)
	require.True(t, original.Accepted)
	require.True(t, original.Scope.Unknown, "fresh current payload can carry historical unknown discovery scope")
	mi := NewMultiIndexer(store, idx.registry, search.NewNull(), nil, zap.NewNop())
	mi.repos["fixture"] = &RepoMetadata{RepoPrefix: "fixture", RootPath: root}
	mi.indexers["fixture"] = idx
	leases := graphview.NewLeaseManager()
	registration, err := leases.RegisterRawRepositoryOwnerPrepared(graphview.RawRepositoryOwner{RepoPrefix: "fixture", RootIdentity: root, Incarnation: t.Name()}, nil)
	require.NoError(t, err)
	_, err = leases.CaptureInitialRawRepositorySource(ctx, registration, "accepted-current-source")
	require.NoError(t, err)
	mi.SetOutputGenerationAuthority(NewOutputGenerationAuthority(leases))
	options := ContractFollowupCaptureOptions{Context: ctx, Store: store, Materializer: &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases}, MultiIndexer: mi, Registry: idx.registry, Config: idx.config, Logger: zap.NewNop(), Yield: func(ctx context.Context) error { return ctx.Err() }}
	spy.reads.Store(0)
	return options, spy, idx, original
}

func TestContractBaselineReusesCurrentAcceptedReceiptThroughAttachment(t *testing.T) {
	for _, helpers := range [][]string{nil, {" corpEnv ", "corpEnv", "otherEnv"}} {
		name := "default"
		if len(helpers) != 0 {
			name = "parser_options"
		}
		t.Run(name, func(t *testing.T) {
			options, spy, idx, original := currentReceiptBaselineFixture(t, helpers)
			spy.forbid.Store(true)
			require.NoError(t, reconcilePrimaryContractBaseline(t.Context(), options, "fixture"), "valid current receipts should not be reparsed")
			require.Zero(t, spy.reads.Load())
			current, _, err := options.Store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", original.FilePath)
			require.NoError(t, err)
			require.JSONEq(t, string(original.Payload), string(current.Payload), "all local records and public dependencies must survive reuse")
			inputs, err := options.Materializer.CaptureContractInputs(t.Context(), nil, "fixture", "")
			require.NoError(t, err)
			require.True(t, inputs.State.Accepted)
			snapshot, targets, err := NewContractFollowupCapture(options)(t.Context(), nil, inputs, "fixture", "")
			require.NoError(t, err)
			defer snapshot.Release()
			require.Len(t, targets, 1)
			require.Equal(t, "fixture", targets[0].Key.RepoPrefix)
			require.Equal(t, parser.NewExtractionOptions(helpers).TemporalEnvHelpers(), snapshot.RepoExtractionOptions["fixture"].TemporalEnvHelpers())
			policy, err := contractBoundaryPolicy(idx, "go", 0)
			require.NoError(t, err)
			require.Equal(t, policy, snapshot.Files[0].Policy)
			snapshot.Release()
			var admission sync.Mutex
			var jobs sync.WaitGroup
			options.BaselineAdmissionMu, options.BaselineJobs = &admission, &jobs
			coordinator, err := NewContractAnalysisCoordinator(ContractAnalysisCoordinatorOptions{Store: options.Store, Leases: options.Materializer.Leases, Registry: options.Registry, Config: options.Config, Capture: NewContractFollowupCapture(options), ReconcileBaseline: NewContractBaselineReconciler(options), Yield: options.Yield})
			require.NoError(t, err)
			defer coordinator.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			admitted, err := coordinator.Request(ctx, nil, inputs, "fixture", "")
			require.NoError(t, err)
			require.True(t, admitted)
			key := graph.ContractAttachmentKey{RepoPrefix: "fixture", InputVersion: inputs.State.InputVersion, InputFingerprint: inputs.State.InputFingerprint}
			for {
				analysis, err := options.Materializer.OpenContractAnalysisForInputs(ctx, nil, inputs)
				if err == nil {
					nodes, err := analysis.NodesByKindsContext(ctx, []graph.NodeKind{graph.KindContract})
					require.NoError(t, err)
					var ids []string
					for _, node := range nodes {
						ids = append(ids, node.ID)
					}
					require.Contains(t, ids, "http::GET::/reused-current")
					require.NoError(t, analysis.Validate(ctx))
					analysis.Close()
					break
				}
				require.Equal(t, graphview.CodeRequiredCapabilityIncomplete, graphview.CodeOf(err))
				require.NoError(t, coordinator.WaitChange(ctx, key))
			}
			require.Zero(t, spy.reads.Load(), "worker must use accepted core evidence and the frozen helper policy")
		})
	}
}

func TestContractBaselineCurrentReceiptRefusesUnprovedEnvelope(t *testing.T) {
	options, _, idx, original := currentReceiptBaselineFixture(t, nil)
	snapshot, err := captureAcceptedContractFiles(t.Context(), options, nil, []string{"fixture"})
	require.NoError(t, err)
	defer snapshot.Release()
	file := snapshot.Files[0]
	accepted, err := snapshot.ReadAccepted(t.Context(), file)
	require.NoError(t, err)
	for _, mode := range []string{"missing", "pending_previous", "deleted", "repo", "checkout", "path", "version", "fingerprint", "source", "payload_version", "payload_source", "language", "policy", "parser_options", "unknown_envelope", "record_proof", "memberships"} {
		t.Run(mode, func(t *testing.T) {
			encoded, err := json.Marshal(original)
			require.NoError(t, err)
			var row *graph.ContractBoundaryReceipt
			require.NoError(t, json.Unmarshal(encoded, &row))
			var payload contractBoundaryReceipt
			require.NoError(t, json.Unmarshal(row.Payload, &payload))
			switch mode {
			case "missing":
				row = nil
			case "pending_previous":
				row.Accepted = false
				row.Previous = original
			case "deleted":
				row.Deleted = true
			case "repo":
				row.RepoPrefix = "other"
			case "checkout":
				row.CheckoutID = "other"
			case "path":
				row.FilePath = "fixture/other.go"
			case "version":
				row.Version = "old"
			case "fingerprint":
				row.Fingerprint = "wrong"
			case "source":
				row.SourceFingerprint = "old"
			case "payload_version":
				payload.Version = "old"
			case "payload_source":
				payload.Source = "old"
			case "language":
				payload.Language = "python"
			case "policy":
				payload.Policy = "old"
			case "parser_options":
				opts := parser.NewExtractionOptions([]string{"corpEnv"})
				idx.extractionOptions.Store(&opts)
				defer func() { opts := parser.ExtractionOptions{}; idx.extractionOptions.Store(&opts) }()
			case "unknown_envelope":
				payload = contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: file.Path}
				row.Scope.Unknown = true
			case "record_proof":
				payload.Records.Full = ""
			case "memberships":
				row.LookupKeys = []string{"extra"}
			}
			if row != nil && (mode == "payload_version" || mode == "payload_source" || mode == "language" || mode == "policy" || mode == "unknown_envelope" || mode == "record_proof") {
				row.Payload, err = json.Marshal(payload)
				require.NoError(t, err)
				row.Fingerprint = contractInputHash(row.Payload)
			}
			_, reused := reuseContractBaselineCurrentReceipt(idx, file, accepted, row)
			require.False(t, reused)
		})
	}
	receipt, reused := reuseContractBaselineCurrentReceipt(idx, file, accepted, original)
	require.True(t, reused, "valid current historical Unknown scope is not an uncertainty envelope")
	var want contractBoundaryReceipt
	require.NoError(t, json.Unmarshal(original.Payload, &want))
	require.Equal(t, want, receipt)
	changed := accepted
	changed.Bytes = append(append([]byte(nil), accepted.Bytes...), '\n')
	_, reused = reuseContractBaselineCurrentReceipt(idx, file, changed, original)
	require.False(t, reused, "unchanged envelope cannot certify changed accepted bytes")
}

func TestContractBaselineCurrentReceiptFallbackReconstructsAcceptedFacts(t *testing.T) {
	for _, mode := range []string{"pending_previous", "policy", "parser_options", "unknown_envelope", "corrupt_hash"} {
		t.Run(mode, func(t *testing.T) {
			options, spy, idx, original := currentReceiptBaselineFixture(t, nil)
			row := *original
			row.Accepted = false
			var payload contractBoundaryReceipt
			require.NoError(t, json.Unmarshal(row.Payload, &payload))
			switch mode {
			case "pending_previous":
				row.Accepted = false
				row.Previous = original
			case "policy":
				payload.Policy = "old-policy"
			case "parser_options":
				opts := parser.NewExtractionOptions([]string{"corpEnv"})
				idx.extractionOptions.Store(&opts)
			case "unknown_envelope":
				payload = contractBoundaryReceipt{Version: contractBoundaryReceiptVersion, FilePath: row.FilePath}
				row.Scope.Unknown = true
			case "corrupt_hash":
				row.Fingerprint = "wrong"
			}
			if mode == "policy" || mode == "unknown_envelope" {
				var err error
				row.Payload, err = json.Marshal(payload)
				require.NoError(t, err)
				row.Fingerprint = contractInputHash(row.Payload)
			}
			require.NoError(t, options.Store.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
			if mode != "pending_previous" {
				require.NoError(t, options.Store.AcceptContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
			}
			require.NoError(t, reconcilePrimaryContractBaseline(t.Context(), options, "fixture"))
			require.EqualValues(t, 1, spy.reads.Load(), "unproved current must fall back to one actual accepted extraction")
			repaired, _, err := options.Store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", row.FilePath)
			require.NoError(t, err)
			require.True(t, repaired.Accepted)
			var got contractBoundaryReceipt
			require.NoError(t, json.Unmarshal(repaired.Payload, &got))
			policy, err := contractBoundaryPolicy(idx, "go", 0)
			require.NoError(t, err)
			require.Equal(t, policy, got.Policy)
			var want contractBoundaryReceipt
			require.NoError(t, json.Unmarshal(original.Payload, &want))
			want.Policy = got.Policy
			require.Equal(t, want, got, "fallback preserves all accepted local and public dependency facts")
		})
	}
}

func TestContractBaselineCurrentReceiptRetainsCancellationAndCensusCAS(t *testing.T) {
	for _, mode := range []string{"canceled", "receipt_changed", "census_changed", "source_changed"} {
		t.Run(mode, func(t *testing.T) {
			options, spy, _, original := currentReceiptBaselineFixture(t, nil)
			spy.forbid.Store(true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			changed := false
			options.Yield = func(ctx context.Context) error {
				if !changed {
					changed = true
					switch mode {
					case "canceled":
						cancel()
					case "receipt_changed":
						row := *original
						row.Accepted = false
						row.Scope.Causes = []string{"newer-receipt"}
						if err := options.Store.SetContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row}); err != nil {
							return err
						}
						if err := options.Store.AcceptContractBoundaryReceiptsContext(ctx, []graph.ContractBoundaryReceipt{row}); err != nil {
							return err
						}
					case "census_changed":
						rows, err := options.Store.FileMetasByPaths("fixture", []string{original.FilePath})
						if err != nil {
							return err
						}
						row, exists := rows[original.FilePath]
						require.True(t, exists)
						row.Size++
						if err := options.Store.SetFileMetas("fixture", []graph.FileMetaRow{row}); err != nil {
							return err
						}
					case "source_changed":
						lease, err := options.Materializer.Leases.AcquireBaseCorpusMutation(ctx, "fixture", options.MultiIndexer.repoRootPath("fixture"))
						if err != nil {
							return err
						}
						err = lease.Complete("newer-accepted-source")
						lease.Release()
						if err != nil {
							return err
						}
					}
				}
				return ctx.Err()
			}
			err := reconcilePrimaryContractBaseline(ctx, options, "fixture")
			switch mode {
			case "canceled":
				require.ErrorIs(t, err, context.Canceled)
			case "source_changed":
				require.ErrorIs(t, err, graphview.ErrBaseCorpusChanged)
			default:
				require.ErrorIs(t, err, graph.ErrContractProjectionStale)
			}
			require.True(t, changed)
			require.Zero(t, spy.reads.Load())
			baseline, err := options.Store.ContractBoundaryReceiptBaselineContext(t.Context(), "fixture", "")
			require.NoError(t, err)
			require.Nil(t, baseline, "reuse must never bypass cancellation, receipt CAS, or accepted census/source authority")
		})
	}
}

func TestContractBaselineCurrentProjectionReceiptRequiresCurrentRecognition(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.SetRepoPrefix("fixture")
	idx.registry.Register(languages.NewCExtractor())
	t.Cleanup(idx.Close)
	file := ContractFollowupFile{RepoPrefix: "fixture", Path: "fixture/src/parser.c", Language: "c"}
	src := generatedParserTestSource("\nTS_PUBLIC const TSLanguage *tree_sitter_demo(void) { return &language; }\n")
	result, recognized := generatedTreeSitterParserProjection("src/parser.c", "c", src)
	require.True(t, recognized)
	receipt, err := idx.collectContractBoundaryReceipt(t.Context(), file.Path, file.Language, src, result)
	require.NoError(t, err)
	row, err := contractCoreStoredReceipt("fixture", "", contractCoreInputChange{FilePath: file.Path, Current: &receipt, SourceFingerprint: contractInputHash(src)})
	require.NoError(t, err)
	row.Accepted = true
	accepted := ContractAcceptedSource{Bytes: src, SourceFingerprint: row.SourceFingerprint}
	got, reused := reuseContractBaselineCurrentReceipt(idx, file, accepted, &row)
	require.True(t, reused)
	var stored contractBoundaryReceipt
	require.NoError(t, json.Unmarshal(row.Payload, &stored))
	require.Equal(t, stored, got, "reuse preserves the entire durable payload, including omitted empty maps")
	idx.config.IndexGeneratedParsers = true
	_, reused = reuseContractBaselineCurrentReceipt(idx, file, accepted, &row)
	require.False(t, reused, "full-parser opt-in cannot reuse a projection policy")
	idx.config.IndexGeneratedParsers = false
	malformed := []byte("int ordinary_c_source(void) { return 1; }\n")
	receipt.Source = contractInputHash(malformed)
	row, err = contractCoreStoredReceipt("fixture", "", contractCoreInputChange{FilePath: file.Path, Current: &receipt, SourceFingerprint: receipt.Source})
	require.NoError(t, err)
	row.Accepted = true
	_, reused = reuseContractBaselineCurrentReceipt(idx, file, ContractAcceptedSource{Bytes: malformed, SourceFingerprint: receipt.Source}, &row)
	require.False(t, reused, "even internally matching source envelopes need the current strict projection recognizer")
}

func TestContractReceiptPolicyIncludesNormalizedFrozenParserOptions(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	defaultPolicy, err := contractBoundaryPolicy(idx, "go", 0)
	require.NoError(t, err)
	opts := parser.NewExtractionOptions([]string{" corpEnv ", "corpEnv", "otherEnv"})
	idx.extractionOptions.Store(&opts)
	configured, err := contractBoundaryPolicy(idx, "go", 0)
	require.NoError(t, err)
	require.NotEqual(t, defaultPolicy, configured, "old helper-agnostic policies cannot certify a configured parser")
	opts = parser.NewExtractionOptions([]string{"otherEnv", "corpEnv"})
	idx.extractionOptions.Store(&opts)
	normalized, err := contractBoundaryPolicy(idx, "go", 0)
	require.NoError(t, err)
	require.Equal(t, configured, normalized)
	opts = parser.NewExtractionOptions([]string{"differentEnv"})
	idx.extractionOptions.Store(&opts)
	changed, err := contractBoundaryPolicy(idx, "go", 0)
	require.NoError(t, err)
	require.NotEqual(t, configured, changed)
	opts = parser.ExtractionOptions{}
	idx.extractionOptions.Store(&opts)
	restored, err := contractBoundaryPolicy(idx, "go", 0)
	require.NoError(t, err)
	require.Equal(t, defaultPolicy, restored)
}
