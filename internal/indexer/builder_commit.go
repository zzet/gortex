package indexer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"

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

	return b.Build(ctx, BuildRequest{
		Identity:    identity,
		Base:        req.Base,
		Target:      target,
		Changes:     changes,
		RootPath:    req.RootPath,
		RepoPrefix:  req.RepoPrefix,
		WorkspaceID: req.WorkspaceID,
		ProjectID:   req.ProjectID,
	})
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

// diffTreeChanges reads the name-status difference between two trees.
//
// -z is required rather than preferred: a path may contain spaces and
// newlines, and a rename record spells its second path in its own
// NUL-terminated chunk. The parser is the one the git watcher already runs on
// exactly this output shape.
//
// The status letters map onto the three change kinds a layer speaks:
//
//	A          added
//	M, T       modified — content, or the type of the entry
//	D          deleted
//	R<score>   deleted at the source path plus added at the destination
//	C<score>   added at the destination; the source is untouched
//	U          modified — an unmerged path still has content on both sides
//
// A rename decomposes deliberately. The two states differ by a path that is
// gone and a path that is new, and a generation's masks speak about paths;
// carrying the rename as one record would say nothing the two halves do not.
func diffTreeChanges(ctx context.Context, repoDir, baseTree, targetTree string) ([]LayerPathChange, error) {
	out, err := gitcmd.RunNoLazy(ctx, repoDir,
		"diff", "--name-status", "--no-renames", "-z", baseTree, targetTree)
	if err != nil {
		return nil, fmt.Errorf("indexer: diff %s..%s in %s: %w", baseTree, targetTree, repoDir, err)
	}
	byPath := make(map[string]LayerChangeKind)
	for _, change := range parseDiffNameStatus(out) {
		switch change.Status {
		case 'A', 'C':
			byPath[path.Clean(change.Path)] = LayerPathAdded
		case 'M', 'T', 'U':
			byPath[path.Clean(change.Path)] = LayerPathModified
		case 'D':
			byPath[path.Clean(change.Path)] = LayerPathDeleted
		case 'R':
			byPath[path.Clean(change.Path)] = LayerPathAdded
			if change.OldPath != "" {
				old := path.Clean(change.OldPath)
				if _, present := byPath[old]; !present {
					byPath[old] = LayerPathDeleted
				}
			}
		}
	}
	changes := make([]LayerPathChange, 0, len(byPath))
	for p, kind := range byPath {
		changes = append(changes, LayerPathChange{Path: p, Kind: kind})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}
