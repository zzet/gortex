package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

// An old installation has no raw proof. Its first structural patch may finish
// while another save arrives: the next save must reuse that completed graph
// without requiring an accepted quiet structural warm-up.
func TestRawEdgeProofWatcherSupersessionBootstrapsMetadata(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "analysis", "adjacency.go"))
	require.NoError(t, err)
	files := map[string]string{"adjacency.go": string(raw), "routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f, reference := newContractJobFixture(t, files), newContractJobFixture(t, files)
	const owner = "fixture/adjacency.go"
	for _, fixture := range []*contractJobFixture{f, reference} {
		rawEdgeProofRemoveMarker(fixture, owner)
	}
	idx := f.mi.indexers["fixture"]
	w := rawEdgeCompletionWatcher(t, f)
	path := filepath.Join(f.root, "adjacency.go")
	first := files["adjacency.go"] + "\n// structural save overtaken after its tail\n"
	next := first + "// next EOF save\n"
	writeFile(t, path, first)
	firstMtime := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, firstMtime, firstMtime))
	var applied *IndexResult
	w.pointReindexRaw = func(p string, _ *PublicationStamps) (*IndexResult, error) {
		result, err := f.mi.incrementalPointRepoRaw("fixture", p)
		if err == nil {
			applied = result
			// This is the real executor's return boundary, after resolution and
			// derived work, but before the watcher checks its generation.
			writeFile(t, p, next)
			nextMtime := firstMtime.Add(time.Second)
			require.NoError(t, os.Chtimes(p, nextMtime, nextMtime))
			w.mu.Lock()
			w.pendingGeneration[p] = 8
			w.mu.Unlock()
		}
		return result, err
	}
	w.pendingGeneration[path] = 7
	require.ErrorIs(t, w.patchGraph(path, ChangeModified, 7), errMutationSuperseded)
	require.NotNil(t, applied)
	require.Zero(t, applied.DerivedInvalidation.MetadataOnlyFiles, "the missing marker required the real structural path")
	row, found, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, row.Accepted, "there was no quiet accepted warm patch")
	require.Equal(t, contractInputHash([]byte(first)), row.SourceFingerprint)
	state := f.store.ContractReceiptWriteStates("fixture", "", []string{owner})[owner]
	require.True(t, state.Seq > 0 && state.Settled && !state.Writing)
	require.True(t, idx.rawEdgesPriorCompleted(owner, f.store.GetFileNodes(owner)))

	w.pointReindexRaw = func(p string, _ *PublicationStamps) (*IndexResult, error) {
		result, err := f.mi.incrementalPointRepoRaw("fixture", p)
		applied = result
		return result, err
	}
	require.NoError(t, w.patchGraph(path, ChangeModified, 8))
	require.NotNil(t, applied)
	require.Equal(t, 1, applied.DerivedInvalidation.MetadataOnlyFiles)
	require.Equal(t, "metadata_only", w.History()[len(w.History())-1].Classification)
	row, found, err = f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, row.Accepted)
	require.Equal(t, contractInputHash([]byte(next)), row.SourceFingerprint)

	// The same completed structural history with ordinary acceptance is the
	// full-state reference; no row, metadata or reference-fact filtering.
	rawEdgeProofSave(t, reference, "adjacency.go", first)
	rawEdgeProofSave(t, reference, "adjacency.go", next)
	require.Equal(t, builderRenderNodes(reference.store.AllNodes()), builderRenderNodes(f.store.AllNodes()))
	require.Equal(t, builderRenderEdges(reference.store.AllEdges()), builderRenderEdges(f.store.AllEdges()))
	require.Equal(t, rawEdgeProofRepoFactRows(t, reference.store), rawEdgeProofRepoFactRows(t, f.store))
}

func rawEdgeCompletionWatcher(t *testing.T, f *contractJobFixture) *Watcher {
	t.Helper()
	mw, err := NewMultiWatcher(f.mi, map[string]config.WatchConfig{"fixture": {Enabled: true}}, zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, mw.watchers["fixture"])
	return mw.watchers["fixture"]
}

func rawEdgeCompletionOvertakenSave(t *testing.T, f *contractJobFixture, w *Watcher, source string) {
	t.Helper()
	path := filepath.Join(f.root, "consts.go")
	writeFile(t, path, source)
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(path, future, future))
	w.pointReindexRaw = func(p string, _ *PublicationStamps) (*IndexResult, error) {
		result, err := f.mi.incrementalPointRepoRaw("fixture", p)
		w.mu.Lock()
		w.pendingGeneration[p] = 8
		w.mu.Unlock()
		return result, err
	}
	w.pendingGeneration[path] = 7
	require.ErrorIs(t, w.patchGraph(path, ChangeModified, 7), errMutationSuperseded)
}

func TestRawEdgeProofSettledSourceAndSequenceGuards(t *testing.T) {
	const owner = "fixture/consts.go"
	for _, change := range []string{"source", "newer_sequence", "restart"} {
		t.Run(change, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			w := rawEdgeCompletionWatcher(t, f)
			rawEdgeCompletionOvertakenSave(t, f, w, jobConsts+"// complete but overtaken\n")
			idx := f.mi.indexers["fixture"]
			prior := f.store.GetFileNodes(owner)
			require.True(t, idx.rawEdgesPriorCompleted(owner, prior))
			row, found, err := f.store.ContractBoundaryReceiptContext(t.Context(), "fixture", "", owner)
			require.NoError(t, err)
			require.True(t, found)
			require.False(t, row.Accepted)
			switch change {
			case "source":
				// The exact completed sequence must not certify another raw
				// source, even if that source carries identical edge proof.
				for i, n := range prior {
					if n.Kind != graph.KindFile {
						continue
					}
					copyNode := *n
					copyNode.Meta = make(map[string]any, len(n.Meta))
					for key, value := range n.Meta {
						copyNode.Meta[key] = value
					}
					var input contractDependencyInputs
					require.NoError(t, json.Unmarshal([]byte(n.Meta[contractDependencyInputMeta].(string)), &input))
					input.Source = "different"
					encoded, err := json.Marshal(input)
					require.NoError(t, err)
					copyNode.Meta[contractDependencyInputMeta] = string(encoded)
					prior[i] = &copyNode
				}
				_, known := storedContractDependencyInputs(prior)
				require.True(t, known, "valid full stamp with a different source")
			case "newer_sequence":
				stale := *row
				stale.Fingerprint = "refused-newer-receipt"
				err := f.store.AcceptContractBoundaryReceiptsContext(context.Background(), []graph.ContractBoundaryReceipt{stale})
				require.ErrorIs(t, err, store_sqlite.ErrCatalogStaleGuard)
				state := f.store.ContractReceiptWriteStates("fixture", "", []string{owner})[owner]
				require.False(t, state.Settled, "even a refused newer receipt invalidates completion")
			case "restart":
				f.store.ForgetContractReceiptWritesForTest("fixture", "", []string{owner})
				require.Zero(t, f.store.ContractReceiptWriteStates("fixture", "", []string{owner})[owner].Seq)
			}
			require.False(t, idx.rawEdgesPriorCompleted(owner, prior), "pending current cannot inherit completion from an old source/sequence/process")
		})
	}
}

func TestRawEdgeProofWatcherFailedTailDoesNotSettle(t *testing.T) {
	const owner = "fixture/consts.go"
	for _, failure := range []string{"early_superseded", "executor_error", "failed_files"} {
		t.Run(failure, func(t *testing.T) {
			f := newRouteConstantFixture(t)
			w := rawEdgeCompletionWatcher(t, f)
			path := filepath.Join(f.root, "consts.go")
			writeFile(t, path, jobConsts+"// unsuccessful watcher outcome\n")
			mtime := time.Now().Add(time.Minute)
			require.NoError(t, os.Chtimes(path, mtime, mtime))
			before := f.store.ContractReceiptWriteStates("fixture", "", []string{owner})[owner]
			called := false
			w.pointReindexRaw = func(p string, _ *PublicationStamps) (*IndexResult, error) {
				called = true
				result, err := f.mi.incrementalPointRepoRaw("fixture", p)
				if err != nil {
					return result, err
				}
				if failure == "executor_error" {
					return result, errors.New("executor tail failed")
				}
				result.FailedFiles = []string{p}
				return result, nil
			}
			w.pendingGeneration[path] = 7
			if failure == "early_superseded" {
				w.pendingGeneration[path] = 8
			}
			require.Error(t, w.patchGraph(path, ChangeModified, 7))
			after := f.store.ContractReceiptWriteStates("fixture", "", []string{owner})[owner]
			if failure == "early_superseded" {
				require.False(t, called)
				require.Equal(t, before, after, "refused entry wrote nothing")
			} else {
				require.True(t, called)
				require.False(t, after.Settled)
				require.True(t, after.Writing)
				require.False(t, f.mi.indexers["fixture"].rawEdgesPriorCompleted(owner, f.store.GetFileNodes(owner)))
			}
		})
	}
}
