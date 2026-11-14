package indexer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/indexer/source"
)

// CommitLayerGenerationKind is the generation kind a commit layer carries. It
// matches graphview.LayerCommit, which is what the materializer reads a routed
// generation back as.
const CommitLayerGenerationKind = "commit"

// ErrInvalidTreeOID reports an object id that is not a git object id. The diff
// and the tree source both take it straight to git, so it is validated here
// rather than handed to a subprocess as an option-looking argument.
var ErrInvalidTreeOID = errors.New("indexer: not a git object id")

// CommitLayerRequest is one commit-layer build: the two committed trees the
// layer spans, plus the identity and repository scoping the generation carries.
type CommitLayerRequest struct {
	// Identity names the generation. GenerationKind and TreeOID are stamped by
	// the builder from the request's own fields, so a caller cannot name one
	// tree and build another.
	Identity GenerationIdentity

	// Base is the reader for the layer beneath this one — the corpus at
	// BaseTreeOID. The affected closure is computed against it.
	Base LayerBase

	// RepoDir is the git repository (or worktree) the trees are read from.
	RepoDir string
	// BaseTreeOID and TargetTreeOID are the trees the layer spans.
	BaseTreeOID   string
	TargetTreeOID string

	// RootPath is the repository root paths are spelled against. Empty uses
	// RepoDir, which is what a plain checkout wants.
	RootPath string

	RepoPrefix  string
	WorkspaceID string
	ProjectID   string
}

// BuildCommitLayer builds the sparse generation that turns the corpus at one
// committed tree into the corpus at another.
//
// The change set comes from a two-tree name-status diff, and the content from
// the target tree's own objects — never from the working tree, which may be at
// a third state entirely. That is the whole reason a commit layer can be built
// for a branch nobody has checked out.
func (b *SparseGenerationBuilder) BuildCommitLayer(
	ctx context.Context,
	req CommitLayerRequest,
) (int64, BuildReport, error) {
	if err := b.validateCommitLayer(&req); err != nil {
		return 0, BuildReport{}, err
	}
	changes, err := diffTreeChanges(ctx, req.RepoDir, req.BaseTreeOID, req.TargetTreeOID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	if err := source.VerifyGitTreeObjectsLocal(ctx, req.RepoDir, req.TargetTreeOID); err != nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: verify tree %s: %w", req.TargetTreeOID, err)
	}
	target, err := source.NewGitTreeSource(ctx, req.RepoDir, req.TargetTreeOID)
	if err != nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: open tree %s: %w", req.TargetTreeOID, err)
	}
	defer target.Close() //nolint:errcheck // the source is read-only; a close failure cannot lose work

	identity := req.Identity
	identity.GenerationKind = CommitLayerGenerationKind
	identity.TreeOID = req.TargetTreeOID

	build := BuildRequest{
		Identity:    identity,
		Base:        req.Base,
		Target:      target,
		Changes:     changes,
		RootPath:    req.RootPath,
		RepoPrefix:  req.RepoPrefix,
		WorkspaceID: req.WorkspaceID,
		ProjectID:   req.ProjectID,
	}
	if enrichesWorkingCopy(identity) {
		build.committedTypecheck = &committedTypecheckStage{CheckoutID: identity.CheckoutID}
	}
	return b.Build(ctx, build)
}

func (b *SparseGenerationBuilder) validateCommitLayer(req *CommitLayerRequest) error {
	if req.RepoDir == "" {
		return errors.New("indexer: commit layer build needs a repository directory")
	}
	if !validGitOID(req.BaseTreeOID) {
		return fmt.Errorf("%w: base %q", ErrInvalidTreeOID, req.BaseTreeOID)
	}
	if !validGitOID(req.TargetTreeOID) {
		return fmt.Errorf("%w: target %q", ErrInvalidTreeOID, req.TargetTreeOID)
	}
	if req.RootPath == "" {
		req.RootPath = req.RepoDir
	}
	return nil
}

// validGitOID reports whether s is a full hexadecimal git object id. Both the
// 40-character SHA-1 and the 64-character SHA-256 forms are accepted;
// abbreviations are not, because an abbreviation is ambiguous and a caller
// naming a tree must name exactly one.
func validGitOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// diffTreeChanges reads the raw difference between two trees.
//
// -z is required rather than preferred: a path may contain spaces and
// newlines. --no-renames splits a rename into its delete and its add, because
// a generation's masks speak about paths and the two halves say everything a
// rename record would.
//
// Each record is classified by its two modes, not its status letter: a side
// holds the path when its mode is neither absent (000000) nor a gitlink
// (160000). Held on both sides is modified, only on the target side added,
// only on the base side deleted, and on neither side the record is skipped.
// Gitlinks are left out because the target is a GitTreeSource, which serves
// files and symlinks only — a submodule pointer named as present would fail
// the build's presence check (#1458) — so a pointer bump changes nothing, a
// file replaced by a submodule is a deletion and the reverse an addition.
func diffTreeChanges(ctx context.Context, repoDir, baseTree, targetTree string) ([]LayerPathChange, error) {
	out, err := gitcmd.RunNoLazy(ctx, repoDir,
		"diff", "--raw", "--no-renames", "--no-abbrev", "-z", baseTree, targetTree)
	if err != nil {
		return nil, fmt.Errorf("indexer: diff %s..%s in %s: %w", baseTree, targetTree, repoDir, err)
	}
	fields := strings.Split(string(out), "\x00")
	var changes []LayerPathChange
	for i := 0; i < len(fields); i++ {
		header := fields[i]
		if header == "" {
			continue
		}
		parts := strings.Fields(strings.TrimPrefix(header, ":"))
		if !strings.HasPrefix(header, ":") || len(parts) < 5 || i+1 >= len(fields) {
			return nil, fmt.Errorf("indexer: unexpected diff record %q", header)
		}
		i++
		inBase, inTarget := treeModeHoldsContent(parts[0]), treeModeHoldsContent(parts[1])
		var kind LayerChangeKind
		switch {
		case inBase && inTarget:
			kind = LayerPathModified
		case inTarget:
			kind = LayerPathAdded
		case inBase:
			kind = LayerPathDeleted
		default:
			continue
		}
		changes = append(changes, LayerPathChange{Path: path.Clean(fields[i]), Kind: kind})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// treeModeHoldsContent reports whether a raw-diff mode names content a tree
// source serves: not an absent side and not a submodule gitlink.
func treeModeHoldsContent(mode string) bool {
	return mode != "000000" && mode != "160000"
}
