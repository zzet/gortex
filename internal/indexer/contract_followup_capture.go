package indexer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
	// SuspectStuck, when set, receives the census paths a capture refused
	// because their receipt row cannot be in flight in this process: a
	// deleted row, or an unaccepted row no mutation of this process wrote.
	// Nothing of a live mutation will make such a row admissible.
	SuspectStuck   func(repo string, paths []string)
	selectedInputs *graphview.SelectedContractInputs
}

// ContractCaptureSource is what a capture takes from a request's selected
// view before the request returns: an independently owned handoff of the
// view's generations and the view facts the capture reads. A primary actor
// has no view and no source; its capture pins the base corpus itself.
type ContractCaptureSource struct {
	handoff      *graphview.ViewHandoff
	repo         string
	composesBase bool
	sources      []graphview.GenerationSource
	once         sync.Once
	// onRelease, set only by tests, observes the release.
	onRelease func()
}

// PrepareContractCaptureSource joins the view's lease and copies what the
// capture needs of the view, in time independent of the census: nothing is
// read from the store. A nil view prepares no source. A view whose lease has
// no live holder left cannot be captured.
func PrepareContractCaptureSource(view *graphview.RepoView) (*ContractCaptureSource, error) {
	if view == nil {
		return nil, nil
	}
	handoff := view.Handoff()
	if handoff == nil {
		return nil, graph.ErrContractProjectionStale
	}
	return &ContractCaptureSource{handoff: handoff, repo: view.ID.RepoPrefix, composesBase: view.ComposesBaseCorpus(), sources: view.GenerationSources()}, nil
}

// Release releases the source's handoff once. A capture that takes the
// source releases it itself, with its snapshot or on its failure; Release is
// for a source no capture took. A nil source releases nothing.
func (s *ContractCaptureSource) Release() {
	if s != nil {
		s.once.Do(func() {
			s.handoff.Close()
			if s.onRelease != nil {
				s.onRelease()
			}
		})
	}
}

// ContractFollowupSourceCapture is a capture from a prepared source: the
// census, receipts and witnesses, with no request view. It owns the source on
// every return.
type ContractFollowupSourceCapture func(context.Context, *ContractCaptureSource, *graphview.SelectedContractInputs, string, string) (ContractFollowupSnapshot, []ContractFollowupTarget, error)

// NewContractFollowupCapture captures only the supplied admitted dependency
// cohort. It acquires independent ownership before returning to the RPC.
func NewContractFollowupCapture(options ContractFollowupCaptureOptions) ContractFollowupCapture {
	capture := NewContractFollowupSourceCapture(options)
	return func(ctx context.Context, view *graphview.RepoView, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
		if inputs == nil || options.Store == nil || options.Materializer == nil {
			return ContractFollowupSnapshot{}, nil, graph.ErrContractInputVector
		}
		prepared, err := PrepareContractCaptureSource(view)
		if err != nil {
			return ContractFollowupSnapshot{}, nil, err
		}
		return capture(ctx, prepared, inputs, repo, checkout)
	}
}

// NewContractFollowupSourceCapture captures the supplied admitted dependency
// cohort from a prepared source.
func NewContractFollowupSourceCapture(options ContractFollowupCaptureOptions) ContractFollowupSourceCapture {
	return func(ctx context.Context, prepared *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (snapshot ContractFollowupSnapshot, targets []ContractFollowupTarget, err error) {
		// The source is the capture's until its census takes it over.
		sourceOwned := true
		defer func() {
			if sourceOwned {
				prepared.Release()
			}
		}()
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
		// Mint selected publication ancestry while these sources are leased,
		// before census/extraction can overlap a content-equal background fold.
		// A later real input mutation still fails the physical publication CAS.
		receiver := options.Store.AtGeneration(0)
		if prepared != nil {
			if len(prepared.sources) == 0 {
				return snapshot, nil, graph.ErrContractInputVector
			}
			receiver = prepared.sources[len(prepared.sources)-1].Handle
		}
		publicationSources := make(map[string]*store_sqlite.Store, len(names))
		for _, name := range names {
			state, sourceErr := graph.ComposeContractInputState(name, checkout, inputs.Witnesses)
			if sourceErr != nil {
				return snapshot, nil, sourceErr
			}
			publicationSources[name], sourceErr = receiver.CaptureContractPublicationSource(ctx, state, inputs.Witnesses)
			if sourceErr != nil {
				return snapshot, nil, sourceErr
			}
		}
		captureOptions := options
		captureOptions.selectedInputs = inputs
		sourceOwned = false
		snapshot, err = captureAcceptedContractFiles(ctx, captureOptions, prepared, names)
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
			targets = append(targets, ContractFollowupTarget{Key: key, Work: work, Catalog: publicationSources[name]})
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
// are never substituted for a different historical accepted file. It owns
// prepared on every return: the snapshot releases it, or the failure does.
func captureAcceptedContractFiles(ctx context.Context, options ContractFollowupCaptureOptions, prepared *ContractCaptureSource, repos []string) (snapshot ContractFollowupSnapshot, err error) {
	if ctx == nil || options.Store == nil || options.Materializer == nil || options.Materializer.Leases == nil || options.Materializer.Catalog == nil || options.MultiIndexer == nil || options.Registry == nil {
		prepared.Release()
		return snapshot, fmt.Errorf("contract capture: incomplete selected owner")
	}
	if err = ctx.Err(); err != nil {
		prepared.Release()
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
	if prepared == nil {
		lease := options.Materializer.Leases.Acquire(0)
		release = append(release, lease.Release)
		snapshot.Core = options.Store.AtGeneration(0)
		handles = append(handles, options.Store.AtGeneration(0))
	} else {
		release = append(release, prepared.Release)
		snapshot.Core = prepared.handoff.Reader
		if prepared.composesBase {
			handles = append(handles, options.Store.AtGeneration(0))
		}
		for _, physical := range prepared.handoff.GenerationSources() {
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
	// No raw-source pin fences the capture or its job: a job proves each read
	// by the receipt stamps of the files it read (contract_stamp_scan.go), so
	// a save to a file it never read cannot fail it.
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
		if prepared != nil && prepared.repo == repo {
			physical := prepared.sources
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
	readDisk := func(ctx context.Context, file ContractFollowupFile) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		content := sources[file.RepoPrefix]
		if content == nil {
			return nil, graph.ErrContractProjectionIncomplete
		}
		rel := strings.TrimPrefix(file.Path, file.RepoPrefix+"/")
		if file.RepoPrefix == "" {
			rel = file.Path
		}
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			return nil, fmt.Errorf("contract capture: invalid source path")
		}
		rc, _, e := content.Open(rel)
		if e != nil {
			return nil, e
		}
		raw, e := io.ReadAll(rc)
		closeErr := rc.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return newTransformPipeline(configs[file.RepoPrefix].Transforms, options.Registry, options.Logger).run(rel, raw), ctx.Err()
	}
	read := func(ctx context.Context, file ContractFollowupFile) (ContractAcceptedSource, error) {
		accepted, err := readDisk(ctx, file)
		if err != nil {
			return ContractAcceptedSource{}, err
		}
		prior := metaByPath[file.Path]
		if contentHashForSource(accepted) != prior.ContentHash || len(accepted) != prior.Size {
			return ContractAcceptedSource{}, graph.ErrContractProjectionStale
		}
		return ContractAcceptedSource{Bytes: accepted, SourceFingerprint: contractInputHash(accepted), Policy: file.Policy}, nil
	}
	policies := make(map[[2]string]string)
	var policiesMu sync.Mutex
	// classify names one census path's language, extraction policy and
	// accepted scope, or reports that the path carries no contract language.
	classify := func(path, repo string) (ContractFollowupFile, bool, error) {
		language, ok := options.Registry.DetectLanguage(path)
		if !ok {
			if filepath.Base(path) != "go.mod" {
				return ContractFollowupFile{}, false, nil
			}
			language = "go"
		}
		policiesMu.Lock()
		policy, cached := policies[[2]string{repo, language}]
		policiesMu.Unlock()
		if !cached {
			idx := &Indexer{config: configs[repo], repoPrefix: repo, registry: options.Registry, logger: options.Logger}
			opts := extractionOptions[repo]
			idx.extractionOptions.Store(&opts)
			computed, e := contractFollowupPolicy(idx, language)
			if e != nil {
				return ContractFollowupFile{}, false, e
			}
			policy = computed
			policiesMu.Lock()
			policies[[2]string{repo, language}] = policy
			policiesMu.Unlock()
		}
		scope := scopes[repo]
		return ContractFollowupFile{RepoPrefix: repo, WorkspaceID: scope[0], ProjectID: scope[1], Path: path, Language: language, Policy: policy}, true, nil
	}
	paths := make([]string, 0, len(metaByPath))
	for path := range metaByPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	// Keep receipt payloads streaming and admission facts bounded to one
	// existing frontier chunk. Sparse upper layers are resolved by indexed keys,
	// rather than thousands of independently prepared point misses per layer.
	for start := 0; start < len(paths); start += contractFrontierReadBatchSize {
		chunk := paths[start:min(start+contractFrontierReadBatchSize, len(paths))]
		files := make(map[string]ContractFollowupFile, len(chunk))
		for _, path := range chunk {
			file, ok, e := classify(path, pathRepos[path])
			if e != nil {
				return ContractFollowupSnapshot{}, e
			}
			if ok {
				files[path] = file
			}
		}
		facts, e := contractCaptureReceiptFacts(ctx, options, handlesByRepo, files)
		if e != nil {
			return ContractFollowupSnapshot{}, e
		}
		for _, path := range chunk {
			file, ok := files[path]
			if !ok {
				continue
			}
			repo := file.RepoPrefix
			if fact, found := facts[path]; found {
				if fact.excluded {
					snapshot.Excluded = append(snapshot.Excluded, path)
					continue
				}
				file.SourceFingerprint = fact.source
				snapshot.opaqueContractInputs = snapshot.opaqueContractInputs || fact.opaque
				snapshot.Files = append(snapshot.Files, file)
				continue
			}
			language := file.Language
			idx := &Indexer{config: configs[repo], repoPrefix: repo, registry: options.Registry, logger: options.Logger}
			opts := extractionOptions[repo]
			idx.extractionOptions.Store(&opts)
			// Admission reads durable receipt identities, never every source file.
			// Missing legacy proof is usable only by background baseline rebuilding.
			repoHandles := handlesByRepo[repo]
			excluded := false
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
						if row.Accepted && contractReceiptIsFailureEnvelope(row) {
							// A failure envelope is not live: the file is excluded
							// from the census rather than certified or refused.
							excluded = true
							break
						}
						// An unaccepted row is admitted on the rule a job proves its
						// reads by (ContractReceiptStamp.quiescent): its mutation
						// settled it, or staged it in this process and has ended its
						// core writes (a watcher patch overtaken after its writes is
						// never accepted or settled), so the file's core rows are
						// complete. A row whose core-write window is open is
						// admitted in flight: the job's stamp scans see the window,
						// and its first read of the file waits for it to close.
						var state store_sqlite.ContractReceiptWriteState
						if !row.Deleted && !row.Accepted {
							state = repoHandles[i].ContractReceiptWriteStates(repo, actor, []string{path})[path]
						}
						switch contractCaptureRowAdmission(row, state, options.selectedInputs != nil) {
						case contractCaptureInFlight:
							snapshot.InFlight = append(snapshot.InFlight, path)
						case contractCaptureStale:
							return ContractFollowupSnapshot{}, graph.ErrContractProjectionStale
						case contractCaptureStuck:
							if options.SuspectStuck != nil {
								options.SuspectStuck(repo, []string{path})
							}
							return ContractFollowupSnapshot{}, graph.ErrContractProjectionStale
						}
						prior, known, e := contractCorePriorReceipt(row, true)
						if e != nil {
							return ContractFollowupSnapshot{}, e
						}
						policy := ""
						opaque := false
						if known && prior != nil {
							policy = prior.Policy
						} else if markerPolicy, overflow := contractReceiptOverflowPolicy(row); overflow && row.Accepted && row.RepoPrefix == repo && row.CheckoutID == actor && row.FilePath == path {
							policy, opaque = markerPolicy, true
						} else {
							return ContractFollowupSnapshot{}, graph.ErrContractProjectionIncomplete
						}
						if policy != file.Policy {
							projected, policyErr := idx.contractFollowupPolicyMode(language, policy)
							if policyErr != nil || !projected || !contractGeneratedFileHint(readers[repo].GetNode(path), path) {
								return ContractFollowupSnapshot{}, graph.ErrContractProjectionStale
							}
							// This is an admission hint from the exact accepted receipt
							// and selected file. The worker revalidates accepted bytes
							// and every public projection fact before omitting C parsing.
							file.Policy = policy
						}
						snapshot.opaqueContractInputs = snapshot.opaqueContractInputs || opaque
						file.SourceFingerprint = row.SourceFingerprint
						break
					}
				}
				if file.SourceFingerprint != "" || excluded {
					break
				}
			}
			if excluded {
				snapshot.Excluded = append(snapshot.Excluded, path)
				continue
			}
			snapshot.Files = append(snapshot.Files, file)
		}
	}

	if options.selectedInputs != nil {
		// The job proves its reads by receipt stamps of the selected chain
		// and re-reads the census at every full scan, instead of fencing the
		// whole inventory at publication.
		census := func(ctx context.Context) (map[string]bool, error) {
			live := make(map[string]bool)
			for _, repo := range repos {
				rows, e := captureAcceptedContractInventory(ctx, readers[repo], handlesByRepo[repo], repo)
				if e != nil {
					return nil, e
				}
				for path := range rows {
					if _, ok := options.Registry.DetectLanguage(path); ok || filepath.Base(path) == "go.mod" {
						live[path] = true
					}
				}
			}
			return live, ctx.Err()
		}
		scanner := newContractStampScanner(options.selectedInputs, handlesByRepo, census)
		snapshot.Scan = scanner.scan
		snapshot.ReceiptContents = scanner.contents
		snapshot.deltaReceipts = scanner.deltaReceipts
		for _, repo := range repos {
			if handles := handlesByRepo[repo]; len(handles) > 0 {
				// Every handle shares one store core, and with it the receipt
				// write sequence.
				snapshot.ReceiptEvents = handles[0].ContractReceiptEvents
				break
			}
		}
		snapshot.WindowLastCloses = func(paths []string, before time.Time) ([]store_sqlite.ContractCoreWindowClose, []string) {
			var closes []store_sqlite.ContractCoreWindowClose
			var missing []string
			for _, path := range paths {
				found := false
				for _, repo := range repos {
					handles := handlesByRepo[repo]
					if len(handles) == 0 || !contractPathInRepo(path, repo, repos) {
						continue
					}
					if close, ok := handles[0].ContractCoreWindowLastClose(repo, "", path, before); ok {
						closes = append(closes, close)
						found = true
					}
					break
				}
				if !found {
					missing = append(missing, path)
				}
			}
			return closes, missing
		}
		snapshot.ReceiptWindowClose = func(windows map[string]ContractReceiptStamp) (time.Time, bool) {
			var first time.Time
			for path, stamp := range windows {
				for _, repo := range repos {
					handles := handlesByRepo[repo]
					if len(handles) == 0 || !contractPathInRepo(path, repo, repos) {
						continue
					}
					if at, ok := handles[0].ContractCoreWindowClosedAt(repo, stamp.Actor, path, stamp.Seq); ok && (first.IsZero() || at.Before(first)) {
						first = at
					}
				}
			}
			return first, !first.IsZero()
		}
		snapshot.WindowGaps = func(paths []string, since time.Time) []time.Duration {
			var out []time.Duration
			for _, repo := range repos {
				handles := handlesByRepo[repo]
				if len(handles) == 0 {
					continue
				}
				var own []string
				for _, path := range paths {
					if contractPathInRepo(path, repo, repos) {
						own = append(own, path)
					}
				}
				for _, gap := range handles[0].ContractCoreWindowGaps(repo, own, since) {
					out = append(out, gap.Quiet)
				}
			}
			return out
		}
		if options.SuspectStuck != nil {
			snapshot.Suspect = func(paths []string) {
				for _, repo := range repos {
					var own []string
					for _, path := range paths {
						if contractPathInRepo(path, repo, repos) {
							own = append(own, path)
						}
					}
					if len(own) > 0 {
						options.SuspectStuck(repo, own)
					}
				}
			}
		}
		snapshot.CensusFile = func(ctx context.Context, path string) (ContractFollowupFile, bool, error) {
			for _, repo := range repos {
				if contractPathInRepo(path, repo, repos) {
					return classify(path, repo)
				}
			}
			return ContractFollowupFile{}, false, ctx.Err()
		}
		accepted := options.MultiIndexer.contractAcceptedSources()
		snapshot.ReadPinned = func(ctx context.Context, file ContractFollowupFile, sourceFingerprint string) ([]byte, error) {
			if cached, ok := accepted.get(sourceFingerprint); ok && contractInputHash(cached) == sourceFingerprint {
				return cached, nil
			}
			bytes, err := readDisk(ctx, file)
			if err != nil {
				return nil, err
			}
			return contractPinnedSourceAtFingerprint(bytes, sourceFingerprint)
		}
		snapshot.Seeds = func(ctx context.Context, repo string) ([]*graph.Node, []*graph.Edge, bool, error) {
			return readContractFollowupSeeds(ctx, readers[repo], handlesByRepo[repo], repo)
		}
	}
	snapshot.ReadAccepted = read
	snapshot.ReadCoreFile = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
		return readContractFollowupCoreFile(ctx, readers[file.RepoPrefix], file)
	}
	complete = true
	return snapshot, nil
}

// readContractFollowupSeeds keeps composed enrichment sparse without treating
// physical seed candidates as selected evidence. The existing checked repository
// projection completes DI ownership across edge-only layers and aliases. Spring
// and Java candidates are reselected through the same captured source reader;
// replacements and file/identity masks can remove or change a physical candidate.
func readContractFollowupSeeds(ctx context.Context, reader graph.Reader, handles []*store_sqlite.Store, repo string) ([]*graph.Node, []*graph.Edge, bool, error) {
	if len(handles) == 0 {
		return nil, nil, false, nil
	}
	if len(handles) == 1 && handles[0].ViewGeneration() == 0 {
		edges, err := handles[0].ContractSeedEdgesContext(ctx, repo)
		if err != nil {
			return nil, nil, false, err
		}
		nodes, err := handles[0].ContractSeedNodesContext(ctx, repo)
		if err != nil {
			return nil, nil, false, err
		}
		return nodes, edges, true, nil
	}
	projection, err := graph.CompleteContractRepoProjection(ctx, reader, repo)
	if err != nil {
		return nil, nil, false, err
	}
	var edges []*graph.Edge
	for _, row := range projection.OwnerRows {
		if row.Edge == nil {
			return nil, nil, false, graph.ErrContractProjectionIncomplete
		}
		if row.Edge.Kind != graph.EdgeProvides && row.Edge.Kind != graph.EdgeConsumes {
			continue
		}
		owner := projection.SourceNodes[row.Edge.From]
		if owner == nil {
			return nil, nil, false, graph.ErrContractProjectionIncomplete
		}
		if owner.RepoPrefix == repo {
			edges = append(edges, row.Edge)
		}
	}
	ids := make(map[string]bool)
	for _, handle := range handles {
		nodes, e := handle.ContractSeedNodesContext(ctx, repo)
		if e != nil {
			return nil, nil, false, e
		}
		for _, node := range nodes {
			if node == nil {
				return nil, nil, false, graph.ErrContractProjectionIncomplete
			}
			ids[node.ID] = true
			if len(ids) > graph.ContractProjectionRowLimit {
				return nil, nil, false, graph.ErrContractProjectionLimit
			}
		}
	}
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	selected, err := graph.ContractSourceNodesContext(ctx, reader, keys)
	if err != nil {
		return nil, nil, false, err
	}
	var nodes []*graph.Node
	for _, id := range keys {
		node := selected[id]
		if node == nil || node.RepoPrefix != repo {
			continue
		}
		if node.Kind != graph.KindField && node.Kind != graph.KindType && node.Kind != graph.KindInterface && node.Kind != graph.KindMethod {
			continue
		}
		_, spring := node.Meta["spring_config_keys"]
		if spring || node.Kind == graph.KindMethod && node.Language == "java" {
			if len(nodes)+len(edges) >= graph.ContractProjectionRowLimit {
				return nil, nil, false, graph.ErrContractProjectionLimit
			}
			nodes = append(nodes, node)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	return nodes, edges, true, nil
}

// Core enrichment needs selected file nodes and their checked outgoing edges.
// Derived contract ownership is neither a seed nor consumed by this read.
func readContractFollowupCoreFile(ctx context.Context, reader graph.Reader, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
	if extraction, _ := ctx.Value(contractExtractionCoreContextKey{}).(bool); extraction {
		if batch, _ := ctx.Value(contractReplayCoreBatchKey{}).(*contractReplayCoreBatch); batch != nil {
			if rows, ok := batch.read(ctx, reader, file); ok {
				return rows, nil
			}
		}
	}
	projection, e := graph.ContractFileNodesContext(ctx, reader, file.RepoPrefix, []string{file.Path})
	if e != nil {
		return ContractFollowupCoreFile{}, e
	}
	nodes := projection[file.Path]
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	var rows map[string][]*graph.Edge
	var truncated bool
	if extraction, _ := ctx.Value(contractExtractionCoreContextKey{}).(bool); extraction {
		rows, truncated, e = graph.ContractExtractionEdgesContext(ctx, reader, ids, graph.ContractProjectionRowLimit)
	} else {
		rows, truncated, e = graph.GetOutEdgesByNodeIDsWithMetadataContext(ctx, reader, ids, graph.ContractProjectionRowLimit)
	}
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

// contractCaptureAdmission is what a capture does with one census path's
// receipt row.
type contractCaptureAdmission int

const (
	// contractCaptureAdmitted: the file's core rows are complete.
	contractCaptureAdmitted contractCaptureAdmission = iota
	// contractCaptureInFlight: a mutation of this process has the file's
	// core-write window open; the job proves its reads of the file once the
	// window closes.
	contractCaptureInFlight
	// contractCaptureStale: the capture refuses the row until it moves.
	contractCaptureStale
	// contractCaptureStuck: the capture refuses a row no live mutation of
	// this process will move, and reports it.
	contractCaptureStuck
)

// contractCaptureRowAdmission classifies a census path's selected receipt
// row: accepted, or unaccepted with complete core rows (state quiescent), is
// admitted. An unaccepted row whose window is open is admitted in flight only
// when the job will take stamp scans (scanned): a frozen snapshot reads the
// row as captured and could not wait for the window. An unaccepted row this
// process never wrote (seq 0) was left by a mutation of an earlier process,
// and a deleted row in the census contradicts it: both are stuck.
func contractCaptureRowAdmission(row *graph.ContractBoundaryReceipt, state store_sqlite.ContractReceiptWriteState, scanned bool) contractCaptureAdmission {
	switch {
	case row.Deleted:
		return contractCaptureStuck
	case row.Accepted || contractReceiptRowsComplete(state):
		return contractCaptureAdmitted
	case state.Writing && state.Seq > 0 && scanned:
		return contractCaptureInFlight
	case state.Seq == 0:
		return contractCaptureStuck
	default:
		return contractCaptureStale
	}
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

// Git text checkouts can carry CRLF while their accepted tree bytes are LF.
// Recover only byte-for-byte accepted content: the full receipt fingerprint,
// not a newline-equivalence assertion, remains mandatory. As with the accepted
// byte cache, the current receipt/source/policy fences still govern its use.
func contractPinnedSourceAtFingerprint(data []byte, fingerprint string) ([]byte, error) {
	if contractInputHash(data) == fingerprint {
		return data, nil
	}
	// Bound the additional reconstruction allocation by the existing accepted
	// source envelope. No other byte rewriting or weaker digest is permitted.
	if len(data) <= contractFollowupCompactLimit && bytes.Contains(data, []byte("\r\n")) {
		canonical := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		if contractInputHash(canonical) == fingerprint {
			return canonical, nil
		}
	}
	return nil, errContractSourceMoved
}
