package mcp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

// Another write can change this checkout while its next publication is queued.
// Recompose before reading the edit target; never replay a prepared edit.
func TestEditAdmissionRecomposesDiskMotionBeforeReadingTarget(t *testing.T) {
	for _, changedTarget := range []bool{false, true} {
		name := "unrelated file"
		if changedTarget {
			name = "target guard remains authoritative"
		}
		t.Run(name, func(t *testing.T) {
			gate := indexer.NewViewBuildGate()
			gate.Open()
			f := newRealCheckoutMutationFixtureWithSetup(t, nil, func(lifecycle *indexer.CheckoutLifecycle, _ string) { lifecycle.SetBuildGate(gate) })
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			release, err := gate.Acquire(ctx, indexer.ViewBuildInteractive)
			require.NoError(t, err)
			release = sync.OnceFunc(release)
			t.Cleanup(release)
			stopped := make(chan struct{})
			joined := make(chan struct{})
			previous := mutationBeforeAdmission
			started := false
			t.Cleanup(func() {
				mutationBeforeAdmission = previous
				close(stopped)
				release()
				if started {
					<-joined
				}
			})
			injected := false
			mutationBeforeAdmission = func(hookCtx context.Context) {
				require.False(t, injected)
				injected = true
				path := filepath.Join(f.worktree, "concurrent.go")
				source := "package repo\nfunc Concurrent() {}\n"
				if changedTarget {
					path = filepath.Join(f.worktree, "edit.go")
					source = "package repo\nfunc ExternallyChanged() {}\n"
				}
				require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
				_, err := f.srv.lifecycle.RequestCheckoutRefresh(ctx, f.checkoutID, f.worktree)
				require.NoError(t, err)
				require.Eventually(t, func() bool { s := gate.Stats(); return s.InteractiveQueued+s.BackgroundQueued > 0 }, 10*time.Second, time.Millisecond, "real checkout publication must own its cycle and wait on the held build lane")
				intent, _ := hookCtx.Value(sourceMutationWriteIntentKey{}).(*sourceMutationWriteIntent)
				require.NotNil(t, intent)
				started = true
				go func() {
					defer close(joined)
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						intent.mu.Lock()
						waiting := intent.suspended > 0 || intent.closed
						intent.mu.Unlock()
						if waiting {
							release()
							return
						}
						select {
						case <-stopped:
							return
						case <-ctx.Done():
							return
						case <-ticker.C:
						}
					}
				}()
			}
			result := f.edit(t, f.worktree, map[string]any{"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func AfterConcurrentWrite() {}"})
			require.True(t, injected)
			data, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
			require.NoError(t, err)
			if changedTarget {
				require.True(t, result.IsError, viewResultText(t, result))
				require.Equal(t, "package repo\nfunc ExternallyChanged() {}\n", string(data))
			} else {
				require.False(t, result.IsError, viewResultText(t, result))
				require.Contains(t, string(data), "func AfterConcurrentWrite() {}")
				other, err := os.ReadFile(filepath.Join(f.worktree, "concurrent.go"))
				require.NoError(t, err)
				require.Equal(t, "package repo\nfunc Concurrent() {}\n", string(other))
				f.awaitMutation(t, f.worktree, result)
			}
		})
	}
}
