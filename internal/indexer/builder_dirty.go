package indexer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"

	"github.com/zzet/gortex/internal/gitstate"
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
	before, err := gitstate.SampleDirty(ctx, req.CheckoutRoot)
	if err != nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: sample %s: %w", req.CheckoutRoot, err)
	}
	target, err := source.NewFilesystemSource(req.CheckoutRoot)
	if err != nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: open checkout %s: %w", req.CheckoutRoot, err)
	}
	defer target.Close() //nolint:errcheck // the source is read-only; a close failure cannot lose work

	identity := req.Identity
	identity.GenerationKind = DirtyLayerGenerationKind
	identity.TreeOID = before.HeadTree
	identity.ProvenanceCommitOID = before.HeadCommit
	identity.LowerViewFingerprint = before.Fingerprint

	changes, err := dirtyLayerChangesContext(ctx, before)
	if err != nil {
		return 0, BuildReport{}, err
	}
	changes, err = dirtyLayerDiskTruthContext(ctx, changes, target)
	if err != nil {
		return 0, BuildReport{}, err
	}

	return b.Build(ctx, BuildRequest{
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
		},
		PrePublish: func(ctx context.Context, generationID int64) error {
			if req.buildBarrier != nil {
				req.buildBarrier()
			}
			return b.confirmDirtySnapshot(ctx, req.CheckoutRoot, generationID, before.Fingerprint)
		},
	})
}

// confirmDirtySnapshot re-samples the checkout and refuses the publish when the
// state moved. A sample that cannot be taken at all is refused too: an
// unavailable snapshot carries no information, and reading its empty entry list
// as "nothing changed" would publish exactly the torn generation the check
// exists to stop.
func (b *SparseGenerationBuilder) confirmDirtySnapshot(
	ctx context.Context,
	root string,
	generationID int64,
	before string,
) error {
	after, err := gitstate.SampleDirty(ctx, root)
	if err != nil {
		if superseded := b.supersede(ctx, generationID); superseded != nil {
			return fmt.Errorf("indexer: re-sample %s: %w (supersede: %v)", root, err, superseded)
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
	if superseded := b.supersede(ctx, generationID); superseded != nil {
		return fmt.Errorf("%w (supersede: %v)", changed, superseded)
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
