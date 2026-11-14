package mcp

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

// The proof unit test forwards checked target identities from the real leased
// SQLite view. Whole-file hydration is refused rather than secretly supplying
// the target through an errorless reader. Registered tests use the real reader.
type sourceTargetCheckedReader struct {
	graph.Reader
	afterTarget                func()
	targetError                error
	metadataError              error
	override                   func(*graph.Node) *graph.Node
	unknownRevision            bool
	targetCalls, fullFileCalls int
}

func (r *sourceTargetCheckedReader) ReadConstantValueProjectionContext(ctx context.Context, ids []string, keys []graph.ConstantFileKey) (graph.ConstantValueProjection, error) {
	if r.metadataError != nil {
		return graph.ConstantValueProjection{}, r.metadataError
	}
	return r.Reader.(graph.ConstantValueProjectionReader).ReadConstantValueProjectionContext(ctx, ids, keys)
}
func (r *sourceTargetCheckedReader) SearchMutationRevision() (uint64, bool) {
	if r.unknownRevision {
		return 0, false
	}
	return r.Reader.(interface{ SearchMutationRevision() (uint64, bool) }).SearchMutationRevision()
}
func (r *sourceTargetCheckedReader) LayerContractFileProjectionContext(context.Context, string, []string) (graph.ContractFileProjection, error) {
	r.fullFileCalls++
	return graph.ContractFileProjection{}, graph.ErrContractProjectionUnsupported
}
func (r *sourceTargetCheckedReader) LayerContractFileNodesContext(context.Context, string, []string) (map[string][]*graph.Node, error) {
	r.fullFileCalls++
	return nil, graph.ErrContractProjectionUnsupported
}
func (r *sourceTargetCheckedReader) LayerContractIDProjectionContext(ctx context.Context, ids []string) (graph.ContractFileProjection, error) {
	r.targetCalls++
	nodes, err := graph.ContractSourceNodesContext(ctx, r.Reader, ids)
	if err != nil {
		return graph.ContractFileProjection{}, err
	}
	if r.override != nil {
		for id, n := range nodes {
			nodes[id] = r.override(n)
		}
	}
	if r.afterTarget != nil {
		r.afterTarget()
	}
	return graph.ContractFileProjection{SourceNodes: nodes}, r.targetError
}

func TestIndexedSymbolSourceProofUsesOnlyCheckedTarget(t *testing.T) {
	selectedProjectionRegistry(t)
	stack := newViewStack(t)
	path := filepath.Join(stack.worktreeRoot, "unowned.go")
	content := []byte("package repo\n\nfunc Current() {\n}\n")
	require.NoError(t, os.WriteFile(path, content, 0600))
	node := viewRepoNode("repo/unowned.go::Current", "Current", graph.KindFunction, "repo/unowned.go", 3)
	base := stack.store.AtGeneration(0)
	base.AddNode(node)
	hash := blake3.Sum256(content)
	require.NoError(t, base.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: node.FilePath, ContentHash: hex.EncodeToString(hash[:]), Size: len(content), NodeCount: 1}}))
	ctx, view := routedEnrichmentView(t, stack)
	reader := &sourceTargetCheckedReader{Reader: view.reader}
	view.reader = reader
	got, ok := stack.srv.readIndexedSymbolSource(ctx, path, node.FilePath, content, node.ID, node)
	t.Logf("ACTUAL_TARGET_SOURCE_PROOF matched=%v target_calls=%d full_file_calls=%d", ok, reader.targetCalls, reader.fullFileCalls)
	require.True(t, ok, "a checked exact target does not need whole-file node hydration")
	require.Equal(t, node.ID, got.ID)
	require.Equal(t, node.Kind, got.Kind)
	require.Equal(t, node.StartLine, got.StartLine)
	require.Equal(t, node.EndLine, got.EndLine)
	require.Equal(t, 1, reader.targetCalls)
	require.Zero(t, reader.fullFileCalls)
	require.Empty(t, pendingFileProjections.snap.Load().entries, "read proof must not publish an owner")
}

func TestIndexedSymbolSourceProofRefusesUnprovedTargetAndVersion(t *testing.T) {
	for _, mode := range []string{"wrong_target", "missing_target", "masked_file", "masked_target", "wrong_path", "empty_target", "metadata_error", "unsupported_reader", "deadline", "wrong_id", "wrong_repo", "wrong_file", "wrong_kind", "wrong_start", "wrong_end", "invalid_range", "partial_error", "canceled", "cancel_during_target", "hash_mismatch", "size_mismatch", "unknown_revision", "gen0_aba", "layer_administration", "pending_owner", "owner_aba"} {
		t.Run(mode, func(t *testing.T) {
			selectedProjectionRegistry(t)
			stack := newViewStack(t)
			path := filepath.Join(stack.worktreeRoot, "unowned.go")
			content := []byte("package repo\n\nfunc Current() {\n}\n")
			require.NoError(t, os.WriteFile(path, content, 0600))
			node := viewRepoNode("repo/unowned.go::Current", "Current", graph.KindFunction, "repo/unowned.go", 3)
			base := stack.store.AtGeneration(0)
			base.AddNode(node)
			hash := blake3.Sum256(content)
			require.NoError(t, base.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: node.FilePath, ContentHash: hex.EncodeToString(hash[:]), Size: len(content), NodeCount: 1}}))
			ctx, view := routedEnrichmentView(t, stack)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			reader := &sourceTargetCheckedReader{Reader: view.reader}
			id := node.ID
			actualPath := path
			switch mode {
			case "wrong_path":
				actualPath = filepath.Join(filepath.Dir(stack.worktreeRoot), "sibling", "unowned.go")
			case "empty_target":
				id = ""
			case "metadata_error":
				reader.metadataError = errors.New("metadata refused")
			case "deadline":
				expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
				ctx = expired
			case "masked_target":
				layer := graph.NewOverlayLayer()
				layer.MarkRemoved(node.Name, node.ID)
				reader.Reader = graph.NewOverlaidView(reader.Reader, layer)
			case "wrong_target":
				id = "repo/other.go::Current"
			case "missing_target":
				id = "repo/unowned.go::Missing"
			case "masked_file":
				layer := graph.NewOverlayLayer()
				layer.MarkFile(node.FilePath, true)
				reader.Reader = graph.NewOverlaidView(reader.Reader, layer)
			case "wrong_id", "wrong_repo", "wrong_file", "wrong_kind", "wrong_start", "wrong_end", "invalid_range":
				reader.override = func(actual *graph.Node) *graph.Node {
					changed := *actual
					switch mode {
					case "wrong_id":
						changed.ID = "repo/unowned.go::Other"
					case "wrong_repo":
						changed.RepoPrefix = "foreign"
					case "wrong_file":
						changed.FilePath = "repo/other.go"
					case "wrong_kind":
						changed.Kind = graph.KindType
					case "wrong_start":
						changed.StartLine++
					case "wrong_end":
						changed.EndLine--
					case "invalid_range":
						changed.EndLine = 1000
					}
					return &changed
				}
			case "partial_error":
				reader.targetError = errors.New("checked target failed")
			case "canceled":
				cancel()
			case "cancel_during_target":
				reader.afterTarget = cancel
			case "hash_mismatch":
				content = append([]byte(nil), content...)
				content[0] = 'P'
			case "size_mismatch":
				content = append(content, '\n')
			case "unknown_revision":
				reader.unknownRevision = true
			case "gen0_aba":
				reader.afterTarget = func() {
					before := base.MutationRevision()
					changed := *node
					changed.StartLine++
					base.AddNode(&changed)
					base.AddNode(node)
					require.Greater(t, base.MutationRevision(), before)
				}
			case "layer_administration":
				reader.afterTarget = func() {
					sources := view.materialized.GenerationSources()
					require.NotEmpty(t, sources)
					handle := sources[0].Handle
					before, err := handle.PayloadInputRevisionContext(ctx)
					require.NoError(t, err)
					// Published payload writes correctly refuse. Real repo rekey is
					// an administrative operation across owned generations and
					// invalidates the actual selected input witness.
					require.NoError(t, stack.store.RekeyRepoPrefix("repo", "renamed"))
					after, err := handle.PayloadInputRevisionContext(ctx)
					require.NoError(t, err)
					require.NotEqual(t, before, after)
				}
			case "pending_owner", "owner_aba":
				publish := func() {
					before := pendingFileProjections.snap.Load()
					stamp, err := statDiskStamp(path)
					require.NoError(t, err)
					entry := syntheticProjectionEntry(path, node.FilePath, "new-owner", 1, stamp, content)
					require.NoError(t, pendingFileProjections.publish(entry))
					if mode == "owner_aba" {
						pendingFileProjections.drop(entry)
						require.Nil(t, pendingFileProjections.snap.Load().entries[entry.key])
						require.NotSame(t, before, pendingFileProjections.snap.Load())
					}
				}
				if mode == "pending_owner" {
					publish()
				} else {
					reader.afterTarget = publish
				}
			}
			view.reader = reader
			if mode == "unsupported_reader" {
				view.reader = struct{ graph.Reader }{reader.Reader}
			}
			got, ok := stack.srv.readIndexedSymbolSource(ctx, actualPath, node.FilePath, content, id, node)
			require.False(t, ok, "unproved target/version must retain the ordinary fallback")
			require.Nil(t, got)
			require.Zero(t, reader.fullFileCalls)
			if mode == "gen0_aba" || mode == "layer_administration" || mode == "cancel_during_target" || mode == "owner_aba" {
				require.Equal(t, 1, reader.targetCalls, "the actual checked read must reach the interleaving")
			}
		})
	}
}
