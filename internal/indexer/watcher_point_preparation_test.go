package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

type pointPreparationBarrier struct {
	parser.Extractor
	armed   atomic.Bool
	calls   atomic.Uint64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	failed  bool
}

func (e *pointPreparationBarrier) extract(path string, raw []byte, options parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	if e.armed.Load() && filepath.Base(path) == "other.go" {
		e.calls.Add(1)
		e.once.Do(func() { close(e.entered) })
		<-e.release
		if e.failed {
			return nil, errors.New("real extractor refusal")
		}
	}
	return parser.Extract(e.Extractor, path, raw, options)
}
func (e *pointPreparationBarrier) Extract(path string, raw []byte) (*parser.ExtractionResult, error) {
	return e.extract(path, raw, parser.ExtractionOptions{})
}
func (e *pointPreparationBarrier) ExtractWithOptions(path string, raw []byte, options parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	return e.extract(path, raw, options)
}

type pointPreparationRemappedExtractor struct{ parser.Extractor }

func (*pointPreparationRemappedExtractor) Language() string     { return "remapped-go" }
func (*pointPreparationRemappedExtractor) Extensions() []string { return []string{".go"} }

// No accepted witness or mutation result is fabricated. A real SQLite index
// publishes the initial source; the exact production queue then enters a real
// read/extract barrier, before the extractor has returned a staged result.
func TestWatcherPointPreparationKeepsAcceptedPublicationUntilApply(t *testing.T) {
	for _, name := range []string{"prepare_only", "apply_invalidates", "parser_failure", "opaque_executor", "remapped_extractor"} {
		t.Run(name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			defer f.close()
			barrier := &pointPreparationBarrier{Extractor: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{}), failed: name == "parser_failure" || name == "remapped_extractor"}
			f.mi.registry.Register(barrier)
			authority := NewOutputGenerationAuthority(f.lc.ViewLeases())
			f.mi.SetOutputGenerationAuthority(authority)
			root := f.gitRepo("prepare-point")
			path := filepath.Join(root, "other.go")
			require.NoError(t, os.WriteFile(path, []byte("package a\nfunc Other() int { return 1 }\n"), 0600))
			tracked, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root}, TrackSourceCLI)
			require.NoError(t, err)
			require.NoError(t, tracked.CatalogErr)
			results, err := f.mi.IndexAll()
			require.NoError(t, err)
			require.Empty(t, results[tracked.Prefix].FailedFiles)
			idx := f.mi.GetIndexer(tracked.Prefix)
			require.NotNil(t, idx)
			require.NoError(t, idx.IndexFile(path))
			pin := f.lc.ViewLeases().AcquireBaseCorpus(tracked.Prefix)
			defer pin.Release()
			require.True(t, pin.OwnerPinned())
			require.True(t, pin.Witnessed())
			require.NoError(t, pin.ValidateAcceptedCurrent())
			beforeRevision := f.store.MutationRevision()
			beforeInput := f.store.PayloadInputRevision()
			beforeStats := authority.Stats()
			require.Zero(t, beforeStats.LiveOwners)
			if name == "remapped_extractor" {
				idx.registry.Register(&pointPreparationRemappedExtractor{Extractor: barrier})
				originalGo, known := idx.registry.GetByLanguage("go")
				require.True(t, known)
				require.Same(t, barrier, originalGo)
			}
			watcher, err := NewWatcher(idx, config.Default().Watch, zap.NewNop())
			require.NoError(t, err)
			// Queue/timer component only; no native-backend provenance is claimed.
			watcher.degradedNoFsnotify = true
			released := false
			release := func() {
				if !released {
					released = true
					close(barrier.release)
				}
			}
			stopped := false
			stop := func() {
				if !stopped {
					stopped = true
					require.NoError(t, watcher.Stop())
				}
			}
			defer func() { release(); stop() }()
			opaqueEntered := make(chan struct{})
			if name == "opaque_executor" {
				watcher.pointReindexRaw = func(string, *PublicationStamps) (*IndexResult, error) {
					close(opaqueEntered)
					<-barrier.release
					return nil, errors.New("opaque executor refusal")
				}
			}
			var wrote atomic.Bool
			var invalidBeforeWrite atomic.Bool
			restore := f.store.SetBatchWriteHookForTest(func(nodes, edges int) error {
				if nodes+edges > 0 {
					wrote.Store(true)
					invalidBeforeWrite.Store(pin.ValidateAcceptedCurrent() != nil)
				}
				return nil
			})
			defer restore()
			require.NoError(t, os.WriteFile(path, []byte("package a\nfunc Other() int { return 2 }\n"), 0600))
			require.False(t, idx.indexedFileReceiptMatches(path))
			barrier.armed.Store(true)
			ticket, err := watcher.EnqueueToolMutation(t.Context(), path)
			require.NoError(t, err)
			require.NotNil(t, ticket)
			require.NotZero(t, ticket.Generation)
			entered := barrier.entered
			if name == "opaque_executor" {
				entered = opaqueEntered
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatalf("real entry missing: %v", ctx.Err())
			}
			require.False(t, wrote.Load())
			require.Equal(t, beforeRevision, f.store.MutationRevision())
			require.Equal(t, beforeInput, f.store.PayloadInputRevision())
			observed := pin.ValidateAcceptedCurrent()
			t.Logf("ACTUAL_POINT_PREPARATION case=%s original_pin_initially_accepted=true entered=true canonical_write=false graph_input_equal=true pin_error=%v stats=%+v", name, observed, authority.Stats())
			if name == "prepare_only" {
				require.NoError(t, observed, "read/extract-only preparation must retain the real accepted publication before any canonical write")
				require.Equal(t, beforeStats.Issued, authority.Stats().Issued)
			}
			if name == "opaque_executor" || name == "remapped_extractor" {
				require.ErrorIs(t, observed, graphview.ErrBaseCorpusChanged, "opaque or remapped extraction must run only under original Begin")
			}
			release()
			select {
			case result, ok := <-ticket.Done:
				require.True(t, ok)
				if name == "parser_failure" || name == "opaque_executor" || name == "remapped_extractor" {
					require.Error(t, result.Err)
				} else {
					require.NoError(t, result.Err)
				}
			case <-ctx.Done():
				t.Fatalf("real completion missing: %v", ctx.Err())
			}
			stop()
			if name == "apply_invalidates" || name == "prepare_only" {
				require.True(t, wrote.Load())
				require.True(t, invalidBeforeWrite.Load(), "first real AddBatch must already be protected")
				require.ErrorIs(t, pin.ValidateAcceptedCurrent(), graphview.ErrBaseCorpusChanged)
				require.Equal(t, uint64(1), barrier.calls.Load(), "the prepared result must be consumed once, without another extraction/admission")
			} else {
				require.Error(t, pin.ValidateAcceptedCurrent(), "failed or opaque work must not manufacture acceptance")
			}
			require.Zero(t, authority.Stats().LiveOwners)
		})
	}
}
