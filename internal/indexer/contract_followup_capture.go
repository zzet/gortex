package indexer

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

type ContractFollowupCaptureOptions struct {
	Context             context.Context
	Store               *store_sqlite.Store
	Materializer        *graphview.Materializer
	MultiIndexer        *MultiIndexer
	Registry            *parser.Registry
	Config              config.IndexConfig
	Logger              *zap.Logger
	Yield               func(context.Context) error
	BaselineAdmissionMu *sync.Mutex
	BaselineJobs        *sync.WaitGroup
	selectedInputs      *graphview.SelectedContractInputs
}

// NewContractFollowupCapture captures only the supplied admitted dependency
// cohort. It acquires independent ownership before returning to the RPC.
func NewContractFollowupCapture(options ContractFollowupCaptureOptions) ContractFollowupCapture {
	return func(ctx context.Context, view *graphview.RepoView, inputs *graphview.SelectedContractInputs, repo, checkout string) (snapshot ContractFollowupSnapshot, targets []ContractFollowupTarget, err error) {
		if inputs == nil || options.Store == nil || options.Materializer == nil {
			return snapshot, nil, graph.ErrContractInputVector
		}
		if err = inputs.Validate(ctx); err != nil {
			return snapshot, nil, err
		}
		repos := make(map[string]bool)
		for _, w := range inputs.Witnesses {
			repos[w.State.RepoPrefix] = true
		}
		names := make([]string, 0, len(repos))
		for name := range repos {
			names = append(names, name)
		}
		sort.Strings(names)
		captureOptions := options
		captureOptions.selectedInputs = inputs
		snapshot, err = captureAcceptedContractFiles(ctx, captureOptions, view, names)
		if err != nil {
			return snapshot, nil, err
		}
		for _, file := range snapshot.Files {
			if file.SourceFingerprint == "" {
				snapshot.Release()
				return ContractFollowupSnapshot{}, nil, graph.ErrContractProjectionIncomplete
			}
		}
		releaseSnapshot := snapshot.Release
		owned := true
		defer func() {
			if owned {
				releaseSnapshot()
			}
		}()
		snapshot.Inputs = append([]graph.ContractInputWitness(nil), inputs.Witnesses...)
		snapshot.Key = graph.ContractAttachmentKey{RepoPrefix: repo, CheckoutID: checkout, InputVersion: inputs.State.InputVersion, InputFingerprint: inputs.State.InputFingerprint}
		receiver := options.Store.AtGeneration(0)
		if view != nil {
			sources := view.GenerationSources()
			if len(sources) == 0 {
				return ContractFollowupSnapshot{}, nil, graph.ErrContractInputVector
			}
			receiver = sources[len(sources)-1].Handle
		}
		for _, name := range names {
			state, e := graph.ComposeContractInputState(name, checkout, snapshot.Inputs)
			if e != nil || !state.Accepted {
				if e == nil {
					e = graph.ErrContractInputVector
				}
				return ContractFollowupSnapshot{}, nil, e
			}
			key := graph.ContractAttachmentKey{RepoPrefix: name, CheckoutID: checkout, InputVersion: state.InputVersion, InputFingerprint: state.InputFingerprint}
			existing, e := receiver.GetContractAttachmentContext(ctx, key)
			if e != nil {
				return ContractFollowupSnapshot{}, nil, e
			}
			if existing != nil {
				continue
			}
			var work []graph.ContractWork
			tokens := make(map[string]bool)
			for _, w := range snapshot.Inputs {
				if w.State.RepoPrefix != name {
					continue
				}
				rows, e := options.Store.AtGeneration(w.GenerationID).PendingContractWorkForScopeContext(ctx, name, w.State.CheckoutID)
				if e != nil {
					return ContractFollowupSnapshot{}, nil, e
				}
				for _, row := range rows {
					if !tokens[row.Token] {
						tokens[row.Token] = true
						work = append(work, row)
					}
				}
			}
			targets = append(targets, ContractFollowupTarget{Key: key, Work: work, Catalog: receiver})
		}
		if err = inputs.Validate(ctx); err != nil {
			return ContractFollowupSnapshot{}, nil, err
		}
		owned = false
		return snapshot, targets, nil
	}
}

// captureAcceptedContractFiles gives background work a complete checked census
// and independently owned sources. Bytes are read one file at a time and must
// reproduce the accepted transformed BLAKE3+size inventory; current disk bytes
// are never substituted for a different historical accepted file.
func captureAcceptedContractFiles(ctx context.Context, options ContractFollowupCaptureOptions, view *graphview.RepoView, repos []string) (snapshot ContractFollowupSnapshot, err error) {
	if ctx == nil || options.Store == nil || options.Materializer == nil || options.Materializer.Leases == nil || options.Materializer.Catalog == nil || options.MultiIndexer == nil || options.Registry == nil {
		return snapshot, fmt.Errorf("contract capture: incomplete selected owner")
	}
	if err = ctx.Err(); err != nil {
		return snapshot, err
	}
	var release []func()
	var once sync.Once
	releaseSnapshot := func() {
		once.Do(func() {
			for i := len(release) - 1; i >= 0; i-- {
				release[i]()
			}
		})
	}
	snapshot.Release = releaseSnapshot
	complete := false
	defer func() {
		if !complete {
			releaseSnapshot()
		}
	}()
	handles := []*store_sqlite.Store{}
	if view == nil {
		lease := options.Materializer.Leases.Acquire(0)
		release = append(release, lease.Release)
		snapshot.Core = options.Store.AtGeneration(0)
		handles = append(handles, options.Store.AtGeneration(0))
	} else {
		handoff := view.Handoff()
		if handoff == nil {
			return ContractFollowupSnapshot{}, graph.ErrContractProjectionStale
		}
		release = append(release, handoff.Close)
		snapshot.Core = handoff.Reader
		if view.ComposesBaseCorpus() {
			handles = append(handles, options.Store.AtGeneration(0))
		}
		for _, physical := range handoff.GenerationSources() {
			handles = append(handles, physical.Handle)
		}
	}
	readers := make(map[string]graph.Reader)
	handlesByRepo := make(map[string][]*store_sqlite.Store)
	if options.selectedInputs != nil {
		readers = options.selectedInputs.SourceReaders()
		ids := []int64{}
		seen := make(map[int64]bool)
		for _, w := range options.selectedInputs.Witnesses {
			if !seen[w.GenerationID] {
				seen[w.GenerationID] = true
				ids = append(ids, w.GenerationID)
			}
			repoHandles := handlesByRepo[w.State.RepoPrefix]
			duplicate := false
			for _, h := range repoHandles {
				duplicate = duplicate || h.ViewGeneration() == w.GenerationID
			}
			if !duplicate {
				handlesByRepo[w.State.RepoPrefix] = append(repoHandles, options.Store.AtGeneration(w.GenerationID))
			}
		}
		lease := options.Materializer.Leases.Acquire(ids...)
		release = append(release, lease.Release)
		snapshot.Core = &contractCapturedCore{Reader: snapshot.Core, readers: readers}
	} else {
		for _, repo := range repos {
			readers[repo] = snapshot.Core
			handlesByRepo[repo] = handles
		}
	}
	var basePins []*graphview.BasePin
	for _, repo := range repos {
		needsBase := false
		for _, handle := range handlesByRepo[repo] {
			needsBase = needsBase || handle.ViewGeneration() == 0
		}
		if needsBase {
			pin := options.Materializer.Leases.AcquireBaseCorpus(repo)
			release = append(release, pin.Release)
			if e := pin.ValidateAcceptedCurrent(); e != nil {
				return ContractFollowupSnapshot{}, e
			}
			basePins = append(basePins, pin)
		}
	}
	metaByPath := make(map[string]graph.FileMetaRow)
	sources := make(map[string]source.ContentSource)
	configs := make(map[string]config.IndexConfig)
	extractionOptions := make(map[string]parser.ExtractionOptions)
	pathRepos := make(map[string]string)
	scopes := make(map[string][2]string)
	for _, repo := range repos {
		root, ok := options.MultiIndexer.RepoRoot(repo)
		if !ok {
			return ContractFollowupSnapshot{}, fmt.Errorf("contract capture: source root unavailable for %q", repo)
		}
		options.MultiIndexer.mu.RLock()
		cfg := options.Config
		if options.MultiIndexer.configMgr != nil {
			cfg = options.MultiIndexer.configMgr.GetRepoConfig(repo).Index
		}
		if idx := options.MultiIndexer.indexers[repo]; idx != nil {
			scopes[repo] = [2]string{idx.workspaceID, idx.projectID}
			extractionOptions[repo] = parser.NewExtractionOptions(idx.extractionOptionsValue().TemporalEnvHelpers())
		}
		options.MultiIndexer.mu.RUnlock()
		scope := scopes[repo]
		owned, _, err := snapshotDedicatedBaseConfig(cfg, repo, scope[0], scope[1])
		if err != nil {
			return ContractFollowupSnapshot{}, err
		}
		configs[repo] = owned
		// A selected linked actor's own source lives at its catalog root. The
		// accepted hash still fences historical and dirty-file movement.
		if view != nil && view.ID.RepoPrefix == repo {
			physical := view.GenerationSources()
			if len(physical) > 0 && physical[len(physical)-1].CheckoutID != "" {
				checkout, found, e := options.Materializer.Catalog.GetCheckout(ctx, physical[len(physical)-1].CheckoutID)
				if e != nil {
					return ContractFollowupSnapshot{}, e
				}
				if !found {
					return ContractFollowupSnapshot{}, graph.ErrContractProjectionStale
				}
				root = checkout.RootPath
			}
		}
		content, e := source.NewFilesystemSource(root)
		if e != nil {
			return ContractFollowupSnapshot{}, e
		}
		sources[repo] = content
		release = append(release, func() { _ = content.Close() })
		if readers[repo] == nil || len(handlesByRepo[repo]) == 0 {
			return ContractFollowupSnapshot{}, graph.ErrContractInputVector
		}
		selected, e := captureAcceptedContractInventory(ctx, readers[repo], handlesByRepo[repo], repo)
		if e != nil {
			return ContractFollowupSnapshot{}, e
		}
		for path, row := range selected {
			metaByPath[path] = row
			pathRepos[path] = repo
		}

	}
	snapshot.RepoConfigs = configs
	snapshot.RepoExtractionOptions = extractionOptions
	snapshot.TrackedRepoModules = make(map[string]string)
	options.MultiIndexer.mu.RLock()
	for _, idx := range options.MultiIndexer.indexers {
		if idx != nil {
			for name, module := range idx.trackedRepoModules {
				snapshot.TrackedRepoModules[name] = module
			}
		}
	}
	options.MultiIndexer.mu.RUnlock()
	read := func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
		if err := ctx.Err(); err != nil {
			return ContractAcceptedSource{}, err
		}
		content := sources[file.RepoPrefix]
		if content == nil {
			return ContractAcceptedSource{}, graph.ErrContractProjectionIncomplete
		}
		rel := strings.TrimPrefix(file.Path, file.RepoPrefix+"/")
		if file.RepoPrefix == "" {
			rel = file.Path
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return ContractAcceptedSource{}, fmt.Errorf("contract capture: invalid source path")
		}
		rc, _, e := content.Open(rel)
		if e != nil {
			return ContractAcceptedSource{}, e
		}
		raw, e := io.ReadAll(rc)
		closeErr := rc.Close()
		if e != nil {
			return ContractAcceptedSource{}, e
		}
		if closeErr != nil {
			return ContractAcceptedSource{}, closeErr
		}
		accepted := newTransformPipeline(configs[file.RepoPrefix].Transforms, options.Registry, options.Logger).run(rel, raw)
		prior := metaByPath[file.Path]
		if contentHashForSource(accepted) != prior.ContentHash || len(accepted) != prior.Size {
			return ContractAcceptedSource{}, graph.ErrContractProjectionStale
		}
		if err := ctx.Err(); err != nil {
			return ContractAcceptedSource{}, err
		}
		return ContractAcceptedSource{Bytes: accepted, SourceFingerprint: contractInputHash(accepted), Policy: file.Policy}, nil
	}
	paths := make([]string, 0, len(metaByPath))
	for path := range metaByPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		repo := pathRepos[path]
		language, ok := options.Registry.DetectLanguage(path)
		if !ok {
			if filepath.Base(path) == "go.mod" {
				language = "go"
			} else {
				continue
			}
		}
		idx := &Indexer{config: configs[repo], repoPrefix: repo, registry: options.Registry, logger: options.Logger}
		policy, e := contractFollowupPolicy(idx, language)
		if e != nil {
			return ContractFollowupSnapshot{}, e
		}
		scope := scopes[repo]
		file := ContractFollowupFile{RepoPrefix: repo, WorkspaceID: scope[0], ProjectID: scope[1], Path: path, Language: language, Policy: policy}
		// Admission reads durable receipt identities, never every source file.
		// Missing legacy proof is usable only by background baseline rebuilding.
		repoHandles := handlesByRepo[repo]
		if options.selectedInputs == nil {
			// Baseline repair derives its source proof from accepted core metadata,
			// even when old optional analysis receipts are incomplete or obsolete.
			repoHandles = nil
		}
		for i := len(repoHandles) - 1; i >= 0; i-- {
			actors := []string{""}
			if options.selectedInputs != nil {
				actors = nil
				for _, w := range options.selectedInputs.Witnesses {
					if w.GenerationID == repoHandles[i].ViewGeneration() && w.State.RepoPrefix == repo {
						actors = append(actors, w.State.CheckoutID)
					}
				}
			}
			for _, actor := range actors {
				row, _, e := repoHandles[i].ContractBoundaryReceiptForVersionContext(ctx, repo, actor, path, contractBoundaryReceiptVersion)
				if e != nil {
					return ContractFollowupSnapshot{}, e
				}
				if row != nil {
					if !row.Accepted || row.Deleted {
						return ContractFollowupSnapshot{}, graph.ErrContractProjectionStale
					}
					prior, known, e := contractCorePriorReceipt(row, true)
					if e != nil {
						return ContractFollowupSnapshot{}, e
					}
					if !known || prior == nil {
						return ContractFollowupSnapshot{}, graph.ErrContractProjectionIncomplete
					}
					if prior.Policy != file.Policy {
						return ContractFollowupSnapshot{}, graph.ErrContractProjectionStale
					}
					file.SourceFingerprint = row.SourceFingerprint
					break
				}
			}
			if file.SourceFingerprint != "" {
				break
			}
		}
		snapshot.Files = append(snapshot.Files, file)
	}
	snapshot.ValidateAccepted = func(ctx context.Context) error {
		for _, pin := range basePins {
			if e := pin.ValidateAcceptedCurrent(); e != nil {
				return e
			}
		}
		current := make(map[string]graph.FileMetaRow)
		for _, repo := range repos {
			rows, e := captureAcceptedContractInventory(ctx, readers[repo], handlesByRepo[repo], repo)
			if e != nil {
				return e
			}
			for path, row := range rows {
				current[path] = row
			}
		}
		if !reflect.DeepEqual(current, metaByPath) {
			return graph.ErrContractProjectionStale
		}
		for _, pin := range basePins {
			if e := pin.ValidateAcceptedCurrent(); e != nil {
				return e
			}
		}
		return ctx.Err()
	}
	snapshot.ReadAccepted = read
	snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		projection, e := graph.CompleteContractFileProjection(ctx, readers[file.RepoPrefix], file.RepoPrefix, []string{file.Path})
		if e != nil {
			return ContractFollowupCoreFile{}, e
		}
		nodes := projection.FileNodes[file.Path]
		ids := make([]string, 0, len(nodes))
		for _, node := range nodes {
			ids = append(ids, node.ID)
		}
		rows, truncated, e := graph.GetOutEdgesByNodeIDsWithMetadataContext(ctx, readers[file.RepoPrefix], ids, graph.ContractProjectionRowLimit)
		if e != nil {
			return ContractFollowupCoreFile{}, e
		}
		if truncated {
			return ContractFollowupCoreFile{}, graph.ErrContractProjectionLimit
		}
		result := ContractFollowupCoreFile{Nodes: nodes}
		for _, id := range ids {
			result.Edges = append(result.Edges, rows[id]...)
		}
		return result, nil
	}
	complete = true
	return snapshot, nil
}

// Inventory discovery uses physical indexed rows, while ownership is selected
// through the same checked file masks as constant sidecars. An accepted empty
// file has an inventory row; a tombstoned file does not.
func captureAcceptedContractInventory(ctx context.Context, core graph.Reader, handles []*store_sqlite.Store, repo string) (map[string]graph.FileMetaRow, error) {
	candidates := make(map[string]bool)
	for _, handle := range handles {
		rows, e := handle.FileMetasForRepoContext(ctx, repo)
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			candidates[row.FilePath] = true
		}
	}
	paths := make([]string, 0, len(candidates))
	for path := range candidates {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	checked, ok := core.(graph.ConstantValueProjectionReader)
	if !ok {
		return nil, graph.ErrConstantProjectionUnsupported
	}
	out := make(map[string]graph.FileMetaRow)
	for start := 0; start < len(paths); start += 64 {
		end := min(start+64, len(paths))
		keys := make([]graph.ConstantFileKey, 0, end-start)
		for _, path := range paths[start:end] {
			keys = append(keys, graph.ConstantFileKey{RepoPrefix: repo, FilePath: path})
		}
		selected, e := checked.ReadConstantValueProjectionContext(ctx, nil, keys)
		if e != nil {
			return nil, e
		}
		for _, key := range keys {
			row, present := selected.Files[key]
			if !present {
				continue
			}
			if row.ContentHash == "" || row.Size < 0 {
				return nil, graph.ErrConstantProjectionIncomplete
			}
			out[key.FilePath] = row
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
