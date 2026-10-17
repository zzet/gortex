package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// NewContractBaselineReconciler admits bounded contract-only reconstruction of
// accepted primary sources. Legacy sealed positive payloads require the existing
// core rebuild path; this constructor never edits their immutable rows.
func NewContractBaselineReconciler(options ContractFollowupCaptureOptions) func(context.Context, *graphview.RepoView, string, string) (bool, error) {
	var jobsMu sync.Mutex
	jobs := make(map[string]bool)
	slots := make(chan struct{}, 2)
	return func(ctx context.Context, view *graphview.RepoView, repo, checkout string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if options.Context == nil || options.BaselineAdmissionMu == nil || options.BaselineJobs == nil || options.Store == nil || options.Materializer == nil || options.Materializer.Leases == nil || options.MultiIndexer == nil || options.Yield == nil {
			return false, fmt.Errorf("contract baseline: incomplete runtime owner")
		}
		if view != nil || checkout != "" {
			return false, nil
		}
		// No raw-source pin gates admission: a never-mutated or interrupted
		// owner has no accepted witness, and refusing on that kept a cold
		// store's consumers waiting on a producer that never started. The job
		// runs its census and every write under the repository lanes and
		// fences on the pending input state instead.
		baseline, err := options.Store.AtGeneration(0).ContractBoundaryReceiptBaselineContext(ctx, repo, "")
		if err != nil {
			return false, err
		}
		if baseline != nil && baseline.Version == contractBoundaryReceiptVersion {
			countContractBaseline(viewmetrics.ContractBaselineCurrent)
			return false, nil
		}
		options.BaselineAdmissionMu.Lock()
		defer options.BaselineAdmissionMu.Unlock()
		if err := options.Context.Err(); err != nil {
			return false, err
		}
		jobsMu.Lock()
		if jobs[repo] {
			jobsMu.Unlock()
			countContractBaseline(viewmetrics.ContractBaselineCoalesced)
			return true, nil
		}
		select {
		case slots <- struct{}{}:
		default:
			jobsMu.Unlock()
			// The occupied slots have independently owned jobs that publish a
			// global change after releasing admission capacity.
			countContractBaseline(viewmetrics.ContractBaselineSaturated)
			return true, nil
		}
		jobs[repo] = true
		jobsMu.Unlock()
		countContractBaseline(viewmetrics.ContractBaselineAdmitted)
		lease := options.Materializer.Leases.Acquire(0)
		options.BaselineJobs.Add(1)
		go func() {
			defer options.BaselineJobs.Done()
			// Admission capacity must be available before waking a consumer
			// that may need to repair another namespace in the same cohort.
			defer func() {
				if hooks := options.MultiIndexer.contractCoreRuntime.Load(); hooks != nil && hooks.Published != nil {
					hooks.Published(options.Context, repo, "")
				}
			}()
			defer lease.Release()
			defer func() { <-slots; jobsMu.Lock(); delete(jobs, repo); jobsMu.Unlock() }()
			// A panicking job still releases its slot, lease and waiters above.
			defer func() {
				if recovered := recover(); recovered != nil {
					countContractBaseline(viewmetrics.ContractBaselinePanic)
					if options.Logger != nil {
						options.Logger.Error("contract baseline reconstruction panicked", zap.String("repo", repo), zap.Any("panic", recovered))
					}
				}
			}()
			err := reconcilePrimaryContractBaseline(options.Context, options, repo)
			if err != nil {
				countContractBaseline(viewmetrics.ContractBaselineFailed)
			} else {
				countContractBaseline(viewmetrics.ContractBaselinePublished)
			}
			if err != nil && options.Context.Err() == nil && options.Logger != nil {
				options.Logger.Warn("contract baseline reconstruction refused", zap.String("repo", repo), zap.Error(err))
			}
		}()
		return true, nil
	}
}

func reconcilePrimaryContractBaseline(ctx context.Context, options ContractFollowupCaptureOptions, repo string) error {
	end := options.MultiIndexer.beginContractBaseline(repo)
	defer end()
	store := options.Store.AtGeneration(0)
	var pending graph.ContractInputState
	var snapshot ContractFollowupSnapshot
	defer func() {
		if snapshot.Release != nil {
			snapshot.Release()
		}
	}()
	// The census and the pending marker share one lane interval, so no core
	// write of this repository lands between the inventory and the marker.
	err := options.MultiIndexer.withRepositoryMutationLanes(ctx, []string{repo}, func() error {
		return options.MultiIndexer.withContractBaselineOutput(ctx, options.Store, repo, OutputEntryContractBaseline, func() error {
			var err error
			snapshot, err = captureAcceptedContractFiles(ctx, options, nil, []string{repo})
			if err != nil {
				return err
			}
			current, found, err := store.ContractInputStateContext(ctx, repo, "")
			if err != nil {
				return err
			}
			baseline, err := store.ContractBoundaryReceiptBaselineContext(ctx, repo, "")
			if err != nil {
				return err
			}
			if found && current.Accepted && baseline != nil && baseline.Version == contractBoundaryReceiptVersion {
				return nil
			}
			var previous *graph.ContractInputState
			if found {
				previous = &current
			}
			changes := []contractCoreInputChange{{FilePath: repo + "/", Delta: contractBoundaryDelta{Scope: graph.ContractWorkScope{Unknown: true, Causes: []string{"namespace_baseline_reconciliation"}}}}}
			pending, _, err = nextContractCoreInputState(previous, repo, "", changes)
			if err != nil {
				return err
			}
			work, err := contractCoreWorkForChanges(0, pending, changes)
			if err != nil {
				return err
			}
			return store.BeginContractInputMutationContext(ctx, previous, pending, work)
		})
	})
	if err != nil {
		return err
	}
	if pending.InputFingerprint == "" {
		return nil
	}
	digest := sha256.New()
	yield := options.Yield
	for start := 0; start < len(snapshot.Files); start += 64 {
		end := min(start+64, len(snapshot.Files))
		files := snapshot.Files[start:end]
		paths := make([]string, 0, len(files))
		for _, file := range files {
			paths = append(paths, file.Path)
		}
		prior, err := store.ContractBoundaryReceiptsForPathsContext(ctx, repo, "", paths)
		if err != nil {
			return err
		}
		rows := make([]graph.ContractBoundaryReceipt, 0, len(files))
		reconstructed := make(map[string][]byte, len(files))
		for _, file := range files {
			if err := yield(ctx); err != nil {
				return err
			}
			if row := prior[file.Path]; row != nil && row.Accepted && contractReceiptIsFailureEnvelope(row) {
				// An accepted failure envelope stays: the file is excluded, and
				// its envelope is folded as it stands below.
				continue
			}
			accepted, err := snapshot.ReadAccepted(ctx, file)
			if errors.Is(err, graph.ErrContractProjectionStale) {
				// The file moved after the census: its save owns its receipt.
				// The chunk folds whatever receipt stands for it at write time.
				continue
			}
			if err != nil {
				return err
			}
			cfg, ok := snapshot.RepoConfigs[repo]
			if !ok {
				return fmt.Errorf("contract baseline: accepted configuration unavailable")
			}
			opts, ok := snapshot.RepoExtractionOptions[repo]
			if !ok {
				return fmt.Errorf("contract baseline: accepted parser options unavailable")
			}
			idx := &Indexer{config: cfg, registry: options.Registry, repoPrefix: repo, workspaceID: file.WorkspaceID, projectID: file.ProjectID, logger: options.Logger}
			idx.extractionOptions.Store(&opts)
			if reuseContractBaselineOverflowReceipt(idx, file, accepted, prior[file.Path]) {
				idx.Close()
				continue // Fold the standing bounded Unknown row, not empty facts.
			}
			receipt, reused := reuseContractBaselineCurrentReceipt(idx, file, accepted, prior[file.Path])
			var collectErr error
			if !reused {
				receipt, collectErr = collectContractBaselineAcceptedReceipt(ctx, idx, file, accepted)
			}
			idx.Close()
			if collectErr != nil {
				return collectErr
			}
			row, err := contractCoreStoredReceipt(repo, "", contractCoreInputChange{FilePath: file.Path, Current: &receipt, SourceFingerprint: accepted.SourceFingerprint})
			if err != nil {
				return err
			}
			if row.Scope.Unknown {
				if _, overflow := contractReceiptOverflowPolicy(&row); !overflow {
					return fmt.Errorf("contract baseline: incomplete receipt %q", file.Path)
				}
				rows = append(rows, row)
				reconstructed[file.Path] = row.Payload
				continue // Fold the exact bounded source/policy marker.
			}
			rows = append(rows, row)
			receipt.Source = "" // Source bytes certify eligibility, not contract identity.
			encoded, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			reconstructed[file.Path] = encoded
		}
		err = options.MultiIndexer.withRepositoryMutationLanes(ctx, []string{repo}, func() error {
			return options.MultiIndexer.withContractBaselineOutput(ctx, options.Store, repo, OutputEntryContractBaseline, func() error {
				// The fence is the pending input state alone: a key-moving save
				// rewrote it, and only that supersedes the baseline.
				current, found, err := store.ContractInputStateContext(ctx, repo, "")
				if err != nil {
					return err
				}
				if !found || !reflect.DeepEqual(current, pending) {
					return graph.ErrContractProjectionStale
				}
				actual, err := store.ContractBoundaryReceiptsForPathsContext(ctx, repo, "", paths)
				if err != nil {
					return err
				}
				// Per-row CAS: a row a concurrent save rewrote since the chunk
				// read it is skipped and folded as it stands; the save's own
				// acceptance settles it. Only unchanged rows are certified.
				var write []graph.ContractBoundaryReceipt
				for _, row := range rows {
					if reflect.DeepEqual(actual[row.FilePath], prior[row.FilePath]) {
						write = append(write, row)
					}
				}
				for _, path := range paths {
					encoded, rebuilt := reconstructed[path]
					if rebuilt && reflect.DeepEqual(actual[path], prior[path]) {
						_, _ = digest.Write(encoded)
					} else if standing := actual[path]; standing != nil {
						_, _ = digest.Write([]byte("standing:" + standing.Fingerprint + ":" + standing.SourceFingerprint))
					} else {
						_, _ = digest.Write([]byte("absent:" + path))
					}
					_, _ = digest.Write([]byte{0})
				}
				if len(write) == 0 {
					return nil
				}
				if err := store.SetContractBoundaryReceiptsContext(ctx, write); err != nil {
					return err
				}
				return store.AcceptContractBoundaryReceiptsContext(ctx, write)
			})
		})
		if err != nil {
			return err
		}
	}
	return options.MultiIndexer.withRepositoryMutationLanes(ctx, []string{repo}, func() error {
		return options.MultiIndexer.withContractBaselineOutput(ctx, options.Store, repo, OutputEntryContractBaseline, func() error {
			current, found, err := store.ContractInputStateContext(ctx, repo, "")
			if err != nil {
				return err
			}
			if !found || !reflect.DeepEqual(current, pending) {
				return graph.ErrContractProjectionStale
			}
			next := pending
			next.InputFingerprint = contractInputHash([]byte(pending.InputFingerprint + ":" + hex.EncodeToString(digest.Sum(nil))))
			changes := []contractCoreInputChange{{FilePath: repo + "/", Delta: contractBoundaryDelta{Scope: graph.ContractWorkScope{Unknown: true, Causes: []string{"namespace_baseline_complete"}}}}}
			work, err := contractCoreWorkForChanges(0, next, changes)
			if err != nil {
				return err
			}
			if err := store.BeginContractInputMutationContext(ctx, &pending, next, work); err != nil {
				return err
			}
			return store.AcceptContractInputMutationWithBaselineContext(ctx, next, graph.ContractBoundaryReceiptBaseline{RepoPrefix: repo, Version: contractBoundaryReceiptVersion, Fingerprint: hex.EncodeToString(digest.Sum(nil))})
		})
	})
}

// beginContractBaseline records a live reconstruction before it asks for a
// repository lane. Overlapping reconstructions each retain their own claim.
func (mi *MultiIndexer) beginContractBaseline(repo string) func() {
	mi.contractBaselineMu.Lock()
	if mi.contractBaselines == nil {
		mi.contractBaselines = make(map[string]int)
	}
	mi.contractBaselines[repo]++
	mi.contractBaselineMu.Unlock()
	return func() {
		mi.contractBaselineMu.Lock()
		defer mi.contractBaselineMu.Unlock()
		mi.contractBaselines[repo]--
		if mi.contractBaselines[repo] == 0 {
			delete(mi.contractBaselines, repo)
		}
	}
}

func (mi *MultiIndexer) contractBaselineActive(repo string) bool {
	mi.contractBaselineMu.Lock()
	defer mi.contractBaselineMu.Unlock()
	return mi.contractBaselines[repo] > 0
}

// Durable core rows include resolver and enrichment output and may be ordered
// differently from the accepted parser result. Reconstruct parser-owned facts
// from the already verified extractor bytes, without indexing or resolving.
func collectContractBaselineAcceptedReceipt(ctx context.Context, idx *Indexer, file ContractFollowupFile, accepted ContractAcceptedSource) (contractBoundaryReceipt, error) {
	if err := ctx.Err(); err != nil {
		return contractBoundaryReceipt{}, err
	}
	ext, ok := idx.registry.GetByLanguage(file.Language)
	if !ok || ext == nil {
		return contractBoundaryReceipt{}, fmt.Errorf("contract baseline: accepted extractor unavailable for %s", file.Language)
	}
	rel := strings.TrimPrefix(file.Path, file.RepoPrefix+"/")
	result, skipped, err := idx.extractFileCtxWithRawLease(ctx, nil, nil, nil, nil, file.Path, rel, file.Language, ext, accepted.Bytes)
	if result != nil {
		defer result.ReleaseTree()
	}
	if err != nil {
		return contractBoundaryReceipt{}, err
	}
	if skipped || result == nil {
		return contractBoundaryReceipt{}, fmt.Errorf("contract baseline: accepted extraction incomplete for %s", file.Path)
	}
	return idx.collectContractBoundaryReceipt(ctx, file.Path, file.Language, accepted.Bytes, result)
}

// withContractBaselineOutput names the contract-only output after its repository
// lane is held. It deliberately avoids core-input journal/source mutation entry:
// the reconstruction's own accepted-source and sidecar CAS fences remain in fn.
// Callers pass the actual runtime Store used for these writes, not a temporary
// AtGeneration handle, and already own the repository mutation lane.
func (mi *MultiIndexer) withContractBaselineOutput(ctx context.Context, output *store_sqlite.Store, repo string, entry OutputMutationEntry, fn func() error) error {
	if fn == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if mi == nil || output == nil || output.SymbolSearchViewGeneration() != 0 || repo == "" {
		return fmt.Errorf("%w: contract baseline requires its generation-zero store and repository root", ErrOutputMutationTargetInvalid)
	}
	root := mi.repoRootPath(repo)
	if root == "" {
		return fmt.Errorf("%w: contract baseline requires its repository root", ErrOutputMutationTargetInvalid)
	}
	if entry != OutputEntryContractBaseline {
		return fmt.Errorf("%w: contract baseline entry %q", ErrOutputMutationEntryUnregistered, entry)
	}
	target := legacyOutputTargetFor(outputStoreIdentity(output), repo, root, "")
	target.OwnerKey += "|producer:contract-baseline"
	receipt, err := mi.outputGenerationAuthority().Begin(ctx, entry, target)
	if err != nil {
		return err
	}
	defer receipt.Abandon() // Also settles a panic; idempotent after Complete.
	return runUnderOutputReceipt(receipt, fn)
}
