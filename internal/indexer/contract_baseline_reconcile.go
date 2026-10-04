package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
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
		options.BaselineAdmissionMu.Lock()
		defer options.BaselineAdmissionMu.Unlock()
		if err := options.Context.Err(); err != nil {
			return false, err
		}
		jobsMu.Lock()
		if jobs[repo] {
			jobsMu.Unlock()
			return true, nil
		}
		select {
		case slots <- struct{}{}:
		default:
			jobsMu.Unlock()
			return false, nil
		}
		jobs[repo] = true
		jobsMu.Unlock()
		lease := options.Materializer.Leases.Acquire(0)
		options.BaselineJobs.Add(1)
		go func() {
			defer options.BaselineJobs.Done()
			defer lease.Release()
			defer func() { <-slots; jobsMu.Lock(); delete(jobs, repo); jobsMu.Unlock() }()
			err := reconcilePrimaryContractBaseline(options.Context, options, repo)
			if err != nil && options.Context.Err() == nil && options.Logger != nil {
				options.Logger.Warn("contract baseline reconstruction refused", zap.String("repo", repo), zap.Error(err))
			}
			if hooks := options.MultiIndexer.contractCoreRuntime.Load(); hooks != nil && hooks.Published != nil {
				hooks.Published(options.Context, repo, "")
			}
		}()
		return true, nil
	}
}

func reconcilePrimaryContractBaseline(ctx context.Context, options ContractFollowupCaptureOptions, repo string) error {
	snapshot, err := captureAcceptedContractFiles(ctx, options, nil, []string{repo})
	if err != nil {
		return err
	}
	defer snapshot.Release()
	if snapshot.ValidateAccepted == nil {
		return fmt.Errorf("contract baseline: accepted census fence unavailable")
	}
	store := options.Store.AtGeneration(0)
	var pending graph.ContractInputState
	err = options.MultiIndexer.withRepositoryMutationLanes(ctx, []string{repo}, func() error {
		if err := snapshot.ValidateAccepted(ctx); err != nil {
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
		for _, file := range files {
			if err := yield(ctx); err != nil {
				return err
			}
			accepted, err := snapshot.ReadAccepted(ctx, file)
			if err != nil {
				return err
			}
			evidence, err := snapshot.ReadCoreFile(ctx, file)
			if err != nil {
				return err
			}
			ids := make([]string, 0, len(evidence.Nodes))
			for _, node := range evidence.Nodes {
				if node != nil {
					ids = append(ids, node.ID)
				}
			}
			values, err := graph.ConstantValuesByNodeIDsContext(ctx, snapshot.Core, ids)
			if err != nil {
				return err
			}
			sort.Strings(ids)
			constants := make([]parser.ConstValue, 0, len(values))
			for _, id := range ids {
				if value, found := values[id]; found {
					constants = append(constants, parser.ConstValue{NodeID: id, FilePath: file.Path, Value: value})
				}
			}
			cfg, ok := snapshot.RepoConfigs[repo]
			if !ok {
				return fmt.Errorf("contract baseline: accepted configuration unavailable")
			}
			idx := &Indexer{config: cfg, registry: options.Registry, repoPrefix: repo, workspaceID: file.WorkspaceID, projectID: file.ProjectID, logger: options.Logger}
			tree := contracts.ParseTreeForLang(file.Language, accepted.Bytes)
			receipt, collectErr := idx.collectContractBoundaryReceipt(ctx, file.Path, file.Language, accepted.Bytes, &parser.ExtractionResult{Nodes: evidence.Nodes, Edges: evidence.Edges, ConstValues: constants, Tree: tree})
			if tree != nil {
				tree.Release()
			}
			if collectErr != nil {
				return collectErr
			}
			row, err := contractCoreStoredReceipt(repo, "", contractCoreInputChange{FilePath: file.Path, Current: &receipt, SourceFingerprint: accepted.SourceFingerprint})
			if err != nil {
				return err
			}
			if row.Scope.Unknown {
				return fmt.Errorf("contract baseline: incomplete receipt %q", file.Path)
			}
			rows = append(rows, row)
			receipt.Source = "" // Source bytes certify eligibility, not contract identity.
			encoded, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			_, _ = digest.Write(encoded)
			_, _ = digest.Write([]byte{0})
		}
		err = options.MultiIndexer.withRepositoryMutationLanes(ctx, []string{repo}, func() error {
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
			if !reflect.DeepEqual(actual, prior) {
				return graph.ErrContractProjectionStale
			}
			if err := store.SetContractBoundaryReceiptsContext(ctx, rows); err != nil {
				return err
			}
			return store.AcceptContractBoundaryReceiptsContext(ctx, rows)
		})
		if err != nil {
			return err
		}
	}
	return options.MultiIndexer.withRepositoryMutationLanes(ctx, []string{repo}, func() error {
		if err := snapshot.ValidateAccepted(ctx); err != nil {
			return err
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
}
