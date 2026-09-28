package indexer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// DirtyLayerGenerationKind is the generation kind a working-tree layer
// carries. It matches graphview.LayerDirty.
const DirtyLayerGenerationKind = "dirty"

// ErrDirtySnapshotChanged reports that the checkout moved while its layer was
// being built. The generation that was in flight describes a state that no
// longer exists, so it is superseded rather than published, and the build is
// worth running again against the state the checkout has now.
//
// Re-running is the CALLER's decision, and deliberately so: a checkout under a
// stream of edits can invalidate every build, and a builder that retried on its
// own would spin. The caller knows whether one more attempt is worth it.
var ErrDirtySnapshotChanged = errors.New("indexer: the checkout changed while its layer was building")

// DirtySnapshotChangedError carries the two fingerprints that disagreed, so a
// caller can log what moved without re-sampling.
type DirtySnapshotChangedError struct {
	CheckoutRoot string
	GenerationID int64
	Before       string
	After        string
}

func (e *DirtySnapshotChangedError) Error() string {
	return fmt.Sprintf(
		"indexer: checkout %s changed while generation %d was building (%s -> %s): %v",
		e.CheckoutRoot, e.GenerationID, e.Before, e.After, ErrDirtySnapshotChanged)
}

// Unwrap exposes the sentinel so errors.Is reaches it.
func (e *DirtySnapshotChangedError) Unwrap() error { return ErrDirtySnapshotChanged }

// Retryable reports that one more build against the current state may succeed.
func (e *DirtySnapshotChangedError) Retryable() bool { return true }

// DirtyLayerRequest is one working-tree-layer build.
type DirtyLayerRequest struct {
	// Identity names the generation. GenerationKind, TreeOID,
	// ProvenanceCommitOID and LowerViewFingerprint are stamped by the builder
	// from the dirty sample, so two builds of the same working-tree state
	// coalesce onto one generation instead of racing.
	Identity GenerationIdentity

	// Base is the reader for the layer beneath — the checkout's commit
	// generation composed over the corpus. The affected closure is computed
	// against it, so a dependent of a dirty file is found in the committed
	// state the working tree diverged from.
	Base LayerBase

	// CheckoutRoot is the working tree the layer describes.
	CheckoutRoot string

	RepoPrefix  string
	WorkspaceID string
	ProjectID   string

	// buildBarrier is a test seam: it runs after the payload is written and
	// before the checkout is re-sampled, which is exactly the window the
	// fingerprint check exists to close. nil in production.
	buildBarrier func()

	// stamped, when non-nil, receives the identity this build actually
	// stamped from its OWN sample, on every attempt.
	//
	// It is not a seam: the coordinator files the built generation in its
	// working-tree reuse cache under the key this renders, and it has to be
	// the build's key rather than the one the coordinator's earlier sample
	// would render. The two samples are taken at different instants, and a
	// tree that moves between them would otherwise file the generation under
	// a key its own row does not render — an entry no lookup can ever hit,
	// occupying a slot that would have held a real one. The retry loop
	// overwrites it per attempt, so the value after a successful return is
	// the identity of the generation that was published.
	stamped *GenerationIdentity

	// Sampler, when set, is the checkout's own working-copy sampler. The build
	// samples through it instead of discovering the worktree and hashing every
	// dirty file afresh: it carries the resolved HEAD tree and the digest memo
	// of quiet files, so a sample costs its git status and the young files
	// only. nil samples with gitstate.SampleDirty.
	Sampler *gitstate.DirtySampler

	// before, when set, is the sample the build's change set and identity come
	// from: the caller's own sample of the same cycle, taken after every
	// ticket the build serves arrived. The pre-publish fence still takes a
	// fresh sample. nil samples first.
	before *gitstate.DirtySnapshot

	// parent, when > 0, is the published working-tree generation this build is
	// a delta over: Base reads its composed view and Identity.BaseGenerationID
	// names it. parentManifest is its chain's resolved input manifest and
	// parentDepth its chain depth. The build plans only the paths whose
	// manifest entry differs from the parent's, writes the delta manifest, and
	// refuses with a *DirtyChainFallbackError when the delta must not be used.
	parent         int64
	parentManifest resolvedManifest
	parentDepth    int

	// chainFallbackReason is the reason a chained attempt for this state was
	// refused, carried into the direct build that replaces it so the report
	// says why the build went direct.
	chainFallbackReason string

	// baseCensus, when non-nil, is the language census of the committed state
	// beneath the working tree; the semantic admission floor is then judged
	// against the checkout's whole language surface (EnrichmentStage.
	// BaseCensus) rather than this build's own files.
	baseCensus map[string]int
}

// DirtyChainFallbackError reports that a working-tree build over a
// working-tree parent refused to use the delta: the state has to be built
// direct over the commit generation instead. Nothing was written. Reason is
// one of the fallback reason codes (dirty_chain_manifest.go).
type DirtyChainFallbackError struct {
	Parent int64
	Reason string
}

func (e *DirtyChainFallbackError) Error() string {
	return fmt.Sprintf("indexer: working-tree build over generation %d falls back direct: %s", e.Parent, e.Reason)
}

// StampDirtyLayerIdentity fills in the four fields of a working-tree layer's
// identity that are a function of the sample it is built from, and nothing
// else.
//
// It exists because two callers have to agree on them exactly. BuildDirtyLayer
// stamps them from its OWN sample, so a caller cannot name one working-tree
// state and build another; the coordinator's reuse cache has to render the
// same identity from the sample it took to decide whether to build at all. Two
// separate copies of "which fields the builder stamps" would drift, and the
// drift would show up as a reuse cache that silently never hits — or, worse,
// as a key that claims two different working trees are the same build. There
// is one definition, and it is here, beside the builder that owns it.
//
// The content fingerprint is the lower view: a dirty layer's lower view IS the
// working tree it was read from, and the fingerprint is what identifies it.
func StampDirtyLayerIdentity(identity GenerationIdentity, snap gitstate.DirtySnapshot) GenerationIdentity {
	identity.GenerationKind = DirtyLayerGenerationKind
	identity.TreeOID = snap.HeadTree
	identity.ProvenanceCommitOID = snap.HeadCommit
	identity.LowerViewFingerprint = snap.Fingerprint
	return identity
}

// BuildDirtyLayer builds the sparse generation that turns a checkout's
// committed content into what is on disk right now.
//
// The checkout is sampled twice. The first sample supplies the change set, the
// content fingerprint that identifies the layer, and the commit the working
// tree diverged from. The second runs after the payload is complete and before
// it is published: if the fingerprints disagree, some part of the payload was
// read from a state the rest of it does not describe, and publishing it would
// make a torn read look like a coherent view of the checkout. Such a
// generation is superseded and the build reports a retryable error.
func (b *SparseGenerationBuilder) BuildDirtyLayer(
	ctx context.Context,
	req DirtyLayerRequest,
) (int64, BuildReport, error) {
	if req.CheckoutRoot == "" {
		return 0, BuildReport{}, errors.New("indexer: dirty layer build needs a checkout root")
	}
	if req.parent > 0 && req.Identity.BaseGenerationID != req.parent {
		return 0, BuildReport{}, fmt.Errorf(
			"indexer: working-tree build over parent %d names base generation %d", req.parent, req.Identity.BaseGenerationID)
	}
	var before gitstate.DirtySnapshot
	var err error
	switch {
	case req.before != nil:
		before = *req.before
	case req.Sampler != nil:
		before, err = req.Sampler.Sample(ctx)
	default:
		before, err = gitstate.SampleDirty(ctx, req.CheckoutRoot)
	}
	if err != nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: sample %s: %w", req.CheckoutRoot, err)
	}
	target, err := source.NewFilesystemSource(req.CheckoutRoot)
	if err != nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: open checkout %s: %w", req.CheckoutRoot, err)
	}
	defer target.Close() //nolint:errcheck // the source is read-only; a close failure cannot lose work

	identity := StampDirtyLayerIdentity(req.Identity, before)
	if req.stamped != nil {
		*req.stamped = identity
	}

	policy := b.dirtyManifestPolicyDigest(identity)
	var (
		changes  []LayerPathChange
		manifest *generationInputManifest
		reused   int
	)
	if req.parent > 0 {
		// A delta over the parent: plan only the paths whose admitted input
		// differs from the parent chain's resolved manifest, and record exactly
		// that difference as this generation's manifest.
		admit := chainManifestAdmitter(req.parentManifest, before, b.manifestAdmitter(req.CheckoutRoot, target))
		nextMeta, nextEntries := admittedManifest(before, admit, policy)
		next := resolvedFromFull(nextMeta, nextEntries)
		headHolds, err := dirtyChainHeadHolds(ctx, req.CheckoutRoot, before, req.parentManifest, next)
		if err != nil {
			return 0, BuildReport{}, err
		}
		delta, reason := planDelta(req.parentManifest, next, headHolds)
		if reason != "" {
			return 0, BuildReport{}, &DirtyChainFallbackError{Parent: req.parent, Reason: reason}
		}
		changes = delta
		entries := manifestDeltaEntries(req.parentManifest, next)
		manifest = &generationInputManifest{
			meta: store_sqlite.InputManifestMeta{
				ManifestVersion: store_sqlite.InputManifestVersion,
				IsFull:          false,
				EntryCount:      len(entries),
				PolicyDigest:    policy,
			},
			entries: entries,
		}
		reused = reusedParentPaths(req.parentManifest, delta)
	} else {
		changes, err = dirtyLayerChangesContext(ctx, before)
		if err != nil {
			return 0, BuildReport{}, err
		}
		// The generation records the full admitted-input manifest of the same
		// sample its change set and fingerprint come from, so a later build
		// can diff against it. It changes nothing this build plans or
		// publishes.
		meta, entries := admittedManifest(before, b.manifestAdmitter(req.CheckoutRoot, target), policy)
		manifest = &generationInputManifest{meta: meta, entries: entries}
	}
	changes, err = dirtyLayerDiskTruthContext(ctx, changes, target)
	if err != nil {
		return 0, BuildReport{}, err
	}
	// A delta over a parent is judged for semantic enrichment by the census of
	// the whole working-tree state, as a direct build of it would be; only
	// read when a semantic manager could use it.
	var chainCensus map[string]map[string]int
	var baseCensus map[string]int
	if b.Semantic != nil {
		if req.parent > 0 {
			chainCensus = dirtyChainLanguageCensus(ctx, b.Store, req.parent, req.RepoPrefix)
		}
		baseCensus = req.baseCensus
	}

	generationID, report, err := b.Build(ctx, BuildRequest{
		Identity:    identity,
		Base:        req.Base,
		Target:      target,
		Changes:     changes,
		RootPath:    req.CheckoutRoot,
		RepoPrefix:  req.RepoPrefix,
		WorkspaceID: req.WorkspaceID,
		ProjectID:   req.ProjectID,
		// The working-tree layer is the one generation whose root is a
		// directory a language server can be rooted at, and the one whose
		// content nothing else on disk holds. Whether the stage actually runs
		// is the enrichment manager's call — the build only says it has a
		// working copy to offer.
		Enrich: &EnrichmentStage{
			CheckoutID:  identity.CheckoutID,
			Fingerprint: before.Fingerprint,
			ChainCensus: chainCensus,
			BaseCensus:  baseCensus,
		},
		PrePublish: func(ctx context.Context, generationID int64) error {
			if req.buildBarrier != nil {
				req.buildBarrier()
			}
			return b.confirmDirtySnapshotWith(ctx, req.Sampler, req.CheckoutRoot, generationID, before.Fingerprint)
		},
		inputManifest: manifest,
	})
	report.ChainFallbackReason = req.chainFallbackReason
	report.ChainDepth = 1
	if req.parent > 0 {
		report.ParentGenerationID = req.parent
		report.ChainDepth = req.parentDepth + 1
	}
	if report.Work != nil {
		report.Work.ParentGenerationID = report.ParentGenerationID
		report.Work.ChainDepth = report.ChainDepth
		report.Work.ManifestEntriesWritten = report.ManifestEntriesWritten
		report.Work.ReusedPriorPayloadFiles = reused
		report.Work.noteChainFallback(req.chainFallbackReason)
	}
	return generationID, report, err
}

// dirtyChainLanguageCensus is the per-file language census of a working-tree
// chain, top first: each generation's own node counts (a generation-scoped
// grouped projection, no node decoding), the newest generation winning per
// file. It walks at most maxDirtyChainDepth working-tree generations and stops
// at the first row that is not one.
func dirtyChainLanguageCensus(ctx context.Context, store *store_sqlite.Store, top int64, repoPrefix string) map[string]map[string]int {
	census := map[string]map[string]int{}
	if store == nil {
		return census
	}
	catalog := store.Catalog()
	id := top
	for depth := 0; id > 0 && depth < maxDirtyChainDepth; depth++ {
		row, found, err := catalog.GetViewGeneration(ctx, id)
		if err != nil || !found || row.GenerationKind != DirtyLayerGenerationKind {
			break
		}
		generation := map[string]map[string]int{}
		for _, count := range graph.ReadRepoLanguageFileCounts(store.AtGeneration(id), []string{repoPrefix}) {
			if count.Language == "" || count.Count <= 0 {
				continue
			}
			if generation[count.FilePath] == nil {
				generation[count.FilePath] = map[string]int{}
			}
			generation[count.FilePath][count.Language] += count.Count
		}
		for file, languages := range generation {
			if _, newer := census[file]; !newer {
				census[file] = languages
			}
		}
		id = row.BaseGenerationID
	}
	return census
}

// reusedParentPaths counts the parent chain's working-tree paths a delta
// leaves alone: their payload is the parent's, read through the composed view
// and never parsed or written again.
func reusedParentPaths(parent resolvedManifest, delta []LayerPathChange) int {
	touched := 0
	for _, change := range delta {
		if _, dirty := parent.entry(change.Path); dirty {
			touched++
		}
	}
	return parent.dirtyCount() - touched
}

// chainManifestAdmitter reuses the parent's recorded admission for a path whose
// bytes and mode are unchanged — same bytes under the same policy digest get
// the same verdict — and asks the builder's own admission for anything else.
// It keeps the admission work of a delta build proportional to what changed.
func chainManifestAdmitter(parent resolvedManifest, snap gitstate.DirtySnapshot, admit manifestAdmitFunc) manifestAdmitFunc {
	contents := make(map[string]gitstate.DirtyContent, len(snap.Contents))
	for _, content := range snap.Contents {
		contents[path.Clean(content.Path)] = content
	}
	return func(p string) store_sqlite.InputManifestAdmission {
		if prior, ok := parent.entry(p); ok && prior.State == store_sqlite.InputManifestPresent {
			if content, sampled := contents[p]; sampled && prior.Mode == content.Mode && prior.ContentSHA256 == content.SHA256 {
				return prior.Admission
			}
		}
		if admit == nil {
			return store_sqlite.InputManifestAdmitted
		}
		return admit(p)
	}
}

// dirtyChainHeadHolds answers planDelta's HEAD-membership question for the
// paths whose entry differs between the parent's resolved manifest and next.
//
// Most answers come from the sample itself: a path sampled absent differs from
// HEAD, so HEAD holds it; a path sampled equal to HEAD holds it exactly when
// it has bytes; a path git reports only as modified is in HEAD, and one it
// reports only as added or untracked is not. The rest — a path that left the
// sample (it is back at its committed state, whether or not HEAD holds it)
// and a path with conflicting status records — are asked of the HEAD tree in
// one git call. Nothing is inferred: a wrong answer here would turn an undo
// into a stale payload or a missing file.
func dirtyChainHeadHolds(
	ctx context.Context, root string, snap gitstate.DirtySnapshot, parent, next resolvedManifest,
) (func(string) bool, error) {
	known := make(map[string]bool)
	kinds := make(map[string][]gitstate.DirtyKind)
	for _, entry := range snap.Entries {
		clean := path.Clean(entry.Path)
		kinds[clean] = append(kinds[clean], entry.Kind)
	}
	for _, content := range snap.Contents {
		clean := path.Clean(content.Path)
		switch {
		case content.State == gitstate.DirtyContentAbsent && !content.HeadEqual:
			known[clean] = true
		case content.HeadEqual:
			known[clean] = content.State == gitstate.DirtyContentPresent
		case content.State == gitstate.DirtyContentPresent:
			if held, ok := headHoldsFromKinds(kinds[clean]); ok {
				known[clean] = held
			}
		}
	}
	var ask []string
	consider := func(p string) {
		if _, ok := known[p]; ok {
			return
		}
		pe, pDirty := parent.entry(p)
		ne, nDirty := next.entry(p)
		if pDirty && nDirty && manifestEntriesEqual(pe, ne) {
			return
		}
		known[p] = false
		ask = append(ask, p)
	}
	for p := range parent.entries {
		consider(p)
	}
	for p := range next.entries {
		consider(p)
	}
	if len(ask) > 0 {
		sort.Strings(ask)
		held, err := gitstate.TreeHoldsPaths(ctx, root, snap.HeadTree, ask)
		if err != nil {
			return nil, fmt.Errorf("indexer: read HEAD membership for a working-tree delta: %w", err)
		}
		for _, p := range ask {
			known[p] = held[p]
		}
	}
	return func(p string) bool { return known[p] }, nil
}

// headHoldsFromKinds reads HEAD membership off git's status records for one
// present path when they agree: modified-family records mean HEAD holds it,
// added or untracked records mean it does not. Mixed records (a staged delete
// beside an untracked copy, say) are not answered.
func headHoldsFromKinds(kinds []gitstate.DirtyKind) (bool, bool) {
	if len(kinds) == 0 {
		return false, false
	}
	held, lacks := 0, 0
	for _, kind := range kinds {
		switch kind {
		case gitstate.DirtyModified, gitstate.DirtyModeChanged, gitstate.DirtySymlinkChanged:
			held++
		case gitstate.DirtyAdded, gitstate.DirtyUntracked:
			lacks++
		default:
			return false, false
		}
	}
	switch {
	case held > 0 && lacks == 0:
		return true, true
	case lacks > 0 && held == 0:
		return false, true
	default:
		return false, false
	}
}

// confirmDirtySnapshotWith re-samples the checkout — through the checkout's
// own sampler when one is given — and refuses the publish when the state
// moved. The sample is always a new one: it is the fence that proves the
// payload describes a state that still exists after it was written. A sample
// that cannot be taken at all is refused too: an unavailable snapshot carries
// no information, and reading its empty entry list as "nothing changed" would
// publish exactly the torn generation the check exists to stop.
func (b *SparseGenerationBuilder) confirmDirtySnapshotWith(
	ctx context.Context,
	sampler *gitstate.DirtySampler,
	root string,
	generationID int64,
	before string,
) error {
	var after gitstate.DirtySnapshot
	var err error
	if sampler != nil {
		after, err = sampler.Sample(ctx)
	} else {
		after, err = gitstate.SampleDirty(ctx, root)
	}
	if err != nil {
		if torn := b.tear(ctx, generationID); torn != nil {
			return fmt.Errorf("indexer: re-sample %s: %w (tear: %v)", root, err, torn)
		}
		return fmt.Errorf("indexer: re-sample %s: %w", root, err)
	}
	if after.Fingerprint == before {
		return nil
	}
	changed := &DirtySnapshotChangedError{
		CheckoutRoot: root,
		GenerationID: generationID,
		Before:       before,
		After:        after.Fingerprint,
	}
	if torn := b.tear(ctx, generationID); torn != nil {
		return fmt.Errorf("%w (tear: %v)", changed, torn)
	}
	return changed
}

// dirtyLayerChanges maps a dirty sample onto the layer's change vocabulary.
//
// An untracked path is an add: the layer's job is to describe what a reader
// sees on disk, and git's distinction between "staged but new" and "not staged
// at all" is about the index, not about the content. A rename destination is an
// add for the same reason, and its vanished source arrives as its own delete
// entry, so nothing has to read OldPath. Mode and symlink flips are content
// changes to a path present on both sides — modified. Submodule entries are
// skipped: a content source serves files and symlinks only, and a submodule
// pointer is neither.
//
// The mapping reads git's vocabulary alone and can therefore call a path
// present that the working tree no longer holds — dirtyLayerDiskTruth settles
// those against the checkout afterwards.
func dirtyLayerChanges(snap gitstate.DirtySnapshot) []LayerPathChange {
	changes, _ := dirtyLayerChangesContext(context.Background(), snap)
	return changes
}

func dirtyLayerChangesContext(ctx context.Context, snap gitstate.DirtySnapshot) ([]LayerPathChange, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	byPath := make(map[string]LayerChangeKind, len(snap.Entries))
	for _, entry := range snap.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Submodule || entry.Path == "" {
			continue
		}
		clean := path.Clean(entry.Path)
		var kind LayerChangeKind
		switch entry.Kind {
		case gitstate.DirtyAdded, gitstate.DirtyUntracked, gitstate.DirtyRenamedFrom:
			kind = LayerPathAdded
		case gitstate.DirtyDeleted:
			kind = LayerPathDeleted
		case gitstate.DirtyModified, gitstate.DirtyModeChanged, gitstate.DirtySymlinkChanged:
			kind = LayerPathModified
		default:
			continue
		}
		// One path can carry more than one entry — staged and unstaged halves
		// of the same change, or a delete followed by a rename onto the same
		// name. A present claim wins over a deletion, because the target
		// source is the working tree and it is what the reader will see.
		if existing, seen := byPath[clean]; seen && existing != LayerPathDeleted {
			continue
		}
		byPath[clean] = kind
	}
	changes := make([]LayerPathChange, 0, len(byPath))
	for p, kind := range byPath {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		changes = append(changes, LayerPathChange{Path: p, Kind: kind})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return changes, nil
}

// dirtyLayerDiskTruth rewrites a present claim into a deletion when the
// checkout does not hold the path.
//
// git's two status columns can call one path present and gone at the same
// time. `git add f` followed by `rm f` reports "AD"; a staged modification
// whose file was then removed reports "MD"; a staged rename whose destination
// was removed reports the destination as a rename. gitstate emits one entry
// per record and lets the staged column decide, so all three arrive here as a
// present claim for a path that is not on disk.
//
// The disk wins, because the layer describes what a reader sees there and what
// a reader sees at such a path is nothing. Passing the claim through instead
// would refuse the whole build — planFileSet reads a present claim the target
// cannot serve as a caller whose diff contradicts its own content — and the
// refusal would repeat on every retry until the user staged the deletion.
// `git add f && rm f` is a legal state an agent reaches routinely; it is not
// a contradiction for the builder to report.
//
// Only "the target does not hold it" demotes. Any other stat failure is left
// for planFileSet to refuse: an unreadable path is a broken read rather than
// an absent file, and turning one into a delete mask would hide the layer
// below behind a permissions error.
func dirtyLayerDiskTruth(changes []LayerPathChange, target source.ContentSource) []LayerPathChange {
	changes, _ = dirtyLayerDiskTruthContext(context.Background(), changes, target)
	return changes
}

func dirtyLayerDiskTruthContext(
	ctx context.Context,
	changes []LayerPathChange,
	target source.ContentSource,
) ([]LayerPathChange, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i := range changes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if changes[i].Kind == LayerPathDeleted {
			continue
		}
		_, statErr := target.Stat(changes[i].Path)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if errors.Is(statErr, source.ErrNotInSource) {
			changes[i].Kind = LayerPathDeleted
		}
	}
	return changes, nil
}
