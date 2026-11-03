package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

// Pause an already-delivered real OS event at the existing synchronous source
// callback. The normal invalidation callback and real native admission still
// run, unchanged, when released. No queue membership or sentinel is fabricated.
type registeredLateStormWatcher struct {
	*indexer.Watcher
	eventPath string
	hold      atomic.Bool
	entered   chan struct{}
	release   chan struct{}
	armed     atomic.Bool
	observe   func()
}

func (w *registeredLateStormWatcher) OnSourceEvent(cb func(string)) {
	w.Watcher.OnSourceEvent(w.sourceEventCallback(cb))
}

func (w *registeredLateStormWatcher) sourceEventCallback(cb func(string)) func(string) {
	return func(path string) {
		if path == w.eventPath && w.hold.CompareAndSwap(true, false) {
			close(w.entered)
			<-w.release
		}
		cb(path)
	}
}

func (w *registeredLateStormWatcher) TierPaths() map[string]indexer.MutationTierState {
	if w.armed.CompareAndSwap(true, false) {
		w.observe()
	}
	return w.Watcher.TierPaths()
}

// One real registered call, with native delivery prepared before the original
// read allowance starts. Only actual watcher admission is released after the
// leaf has selected its checked version and before response finalization.
func TestRegisteredSummaryLateNativeStormCannotEscapeAsGenericSuperseded(t *testing.T) {
	for _, scenario := range []string{"victim_storm", "other_path", "point_queue", "cancelled", "new_physical_owner", "caller_owned", "unchecked_eviction", "point_queue_startup_reconcile"} {
		name := scenario
		forceStartupReconcile := scenario == "point_queue_startup_reconcile"
		if forceStartupReconcile {
			scenario = "point_queue"
		}
		t.Run(name, func(t *testing.T) {
			selectedProjectionRegistry(t)
			var s *Server
			var path string
			var raw []byte
			var watcher *registeredLateStormWatcher
			var unblock, stopWatcher func()
			if scenario == "point_queue" {
				s, _, path = canonicalPromotionReadFixture(t)
				watcher, unblock, stopWatcher = startRegisteredLateStormFixtureWatcher(t, s, path, scenario, forceStartupReconcile)
				s, path, _, raw = acknowledgedCanonicalFileOnFixture(t, s, path)
				// Latest cleanup runs before the held-ACK receipt join; the early
				// once-protected registration also covers failures during setup.
				t.Cleanup(stopWatcher)
				t.Cleanup(unblock)
			} else {
				s, path, _, raw = acknowledgedCanonicalFile(t)
			}
			external := []byte(strings.Replace(string(raw), "func Target()", "// external version\n\nfunc Target()", 1))
			require.NoError(t, os.WriteFile(path+".external", external, 0600))
			require.NoError(t, os.Rename(path+".external", path))
			session := "late-storm-" + scenario
			require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(session) })
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := s.WithClientSession(WithSessionID(parent, session), session)
			require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"gortex-load","version":"1"}}}`)))
			s.NoteSessionToolPolicy(session, "all", "defer")
			// Reuse the existing held-ACK fixture for canonical publication; this real
			// registered read establishes a checked external read version before the
			// real native watcher starts. The upcoming target read uses its own facts.
			ready := preselectionRegisteredCall(t, s, ctx, 10, "get_symbol", map[string]any{"id": "main.go::Target", "detail": "full", "format": "json"})
			require.False(t, ready.IsError, resultText(ready))
			checked := liveEntry(pendingFileProjections, path)
			require.NotNil(t, checked)
			require.True(t, checked.readVersionChecked)
			require.Zero(t, checked.commitSeq)
			root := filepath.Dir(path)
			auxA, auxB, other := filepath.Join(root, "storm_a.go"), filepath.Join(root, "storm_b.go"), filepath.Join(root, "other.go")
			for _, file := range []string{auxA, auxB, other} {
				require.NoError(t, os.WriteFile(file, []byte("package probe\nfunc Auxiliary() int { return 0 }\n"), 0600))
			}
			if watcher == nil {
				watcher, unblock, _ = startRegisteredLateStormFixtureWatcher(t, s, path, scenario, false)
			} else {
				// The held-ACK helper installs its receipt watcher; restore the
				// already-started native watcher for the actual registered read.
				s.SetWatcher(watcher)
			}
			if scenario == "point_queue" {
				require.Equal(t, 5, s.graph.GetNode("main.go::Neighbor").StartLine, "point fixture requires the old canonical graph")
				require.False(t, s.indexer.IndexedContentMatches("main.go", external), "point read must borrow its checked owner rather than current canonical bytes")
			}
			if scenario != "point_queue" {
				require.NoError(t, os.WriteFile(auxA, []byte("package probe\nfunc AuxiliaryA() int { return 1 }\n"), 0600))
				require.NoError(t, os.WriteFile(auxB, []byte("package probe\nfunc AuxiliaryB() int { return 1 }\n"), 0600))
				require.Eventually(t, func() bool { return watcher.StormActiveForFile(auxA) || watcher.StormActiveForFile(auxB) }, 5*time.Second, time.Millisecond, "setup requires real other-path queued storm before the read")
			}
			require.False(t, watcher.StormActiveForFile(path))
			require.Same(t, checked, liveEntry(pendingFileProjections, path))
			require.False(t, checked.suspect.Load())
			// A real same-content save plus restored stamp makes this a pure late
			// source-event transition, not changed bytes or a synthetic stale error.
			eventBytes, err := os.ReadFile(watcher.eventPath)
			require.NoError(t, err)
			stamp, err := os.Stat(watcher.eventPath)
			require.NoError(t, err)
			watcher.holdNextSourceEvent()
			require.NoError(t, os.WriteFile(watcher.eventPath, eventBytes, 0600))
			require.NoError(t, os.Chtimes(watcher.eventPath, stamp.ModTime(), stamp.ModTime()))
			select {
			case <-watcher.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("real native event did not reach the controlled source callback")
			}
			disk, err := statDiskStamp(path)
			require.NoError(t, err)
			require.True(t, checked.disk.matches(disk))
			require.False(t, watcher.StormActiveForFile(path))

			var selected context.Context
			var selectedEntry *projectionEntry
			var observationErr error
			var calls, observations int
			var selectedValid bool
			var observedGeneration uint64
			var afterStorm bool
			var physicalSeq uint64
			var accepted atomic.Int32
			s.facades.mu.RLock()
			captured := s.facades.captured["get_file_summary"]
			s.facades.mu.RUnlock()
			leaf := captured.handler
			s.addTool(captured.tool, func(call context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				result, err := leaf(call, req)
				calls++
				if err == nil && result != nil && !result.IsError {
					afterFreshSymbolAcceptance(call, func() { accepted.Add(1) })
					if calls == 1 {
						selected = call
						facts, _ := call.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
						if facts != nil {
							facts.mu.RLock()
							for _, entry := range facts.entries {
								selectedEntry = entry
							}
							facts.mu.RUnlock()
						}
						selectedValid = validateRequestProjections(call) == nil && !watcher.StormActiveForFile(path)
						watcher.armed.Store(true)
					}
				}
				return result, err
			})
			allowance := &projectedSourceAllowance{tool: "get_file_summary"}
			deadline := time.Now().Add(projectedReadBudget)
			ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
			ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
			if scenario == "caller_owned" {
				ctx, err = retainRequestProjection(withRequestProjectionFacts(ctx), checked)
				require.NoError(t, err)
			}
			watcher.observe = func() {
				observations++
				unblock()
				// This is a queue-admission handshake, never a retried public read.
				// It stays inside the ORIGINAL allowance and never renews its deadline.
				for selected.Err() == nil && time.Now().Before(deadline) {
					state := watcher.TierState(watcher.eventPath)
					queued := watcher.StormActiveForFile(watcher.eventPath)
					if state.Generation != 0 && (scenario == "point_queue" || queued) {
						observedGeneration = state.Generation
						afterStorm = watcher.StormActiveForFile(path)
						break
					}
					runtime.Gosched()
				}
				if observedGeneration == 0 {
					observationErr = fmt.Errorf("native admission not observed inside original allowance, remaining=%s", time.Until(deadline))
					return
				}
				if scenario == "cancelled" {
					cancel()
				}
				if scenario == "unchecked_eviction" {
					pendingFileProjections.drop(selectedEntry)
				}
				if scenario == "new_physical_owner" {
					held, release, err := acquireMutationPath(t.Context(), path)
					if err != nil {
						observationErr = err
						return
					}
					_, observationErr = s.commitFileMutation(held, "write_file", "", "", "main.go", path, external, 0600)
					release()
					if current := liveEntry(pendingFileProjections, path); current != nil {
						physicalSeq = current.commitSeq
					}
				}
			}
			result, err := externalVersionRegisteredCall(s, ctx, 20, "get_file_summary", map[string]any{"path": "main.go", "format": "json"})
			after, physicalErr := os.ReadFile(path) // strictly after the one immediate call
			t.Logf("scenario=%s calls=%d observations=%d original_attempts=%d remaining=%s real_generation=%d victim_storm=%t physical_seq=%d result=%v error=%v observation=%v", scenario, calls, observations, allowance.attempts.Load(), time.Until(deadline), observedGeneration, afterStorm, physicalSeq, result, err, observationErr)
			require.NoError(t, physicalErr)
			require.Equal(t, external, after)
			require.NoError(t, observationErr)
			require.Equal(t, 1, observations)
			require.NotNil(t, selectedEntry)
			require.Same(t, checked, selectedEntry, "the actual checked cached version was selected before event admission")
			require.True(t, selectedEntry.readVersionChecked)
			require.Zero(t, selectedEntry.commitSeq)
			require.True(t, selectedValid, "the original checked selection precedes actual victim storm admission")
			require.Same(t, allowance, selected.Value(projectedSourceAllowanceKey{}))
			require.Equal(t, deadline, selected.Value(projectedSourceDeadlineKey{}))
			require.LessOrEqual(t, allowance.attempts.Load(), int32(2))
			require.LessOrEqual(t, calls, 2)
			if scenario == "other_path" {
				require.False(t, afterStorm)
				require.NoError(t, validateRequestProjections(context.WithoutCancel(selected)))
			} else {
				require.ErrorIs(t, validateRequestProjections(context.WithoutCancel(selected)), projectionSuperseded, "normal source invalidation must keep old facts invalid")
			}
			if scenario == "cancelled" {
				require.Error(t, err)
				require.ErrorIs(t, parent.Err(), context.Canceled)
				require.Zero(t, accepted.Load())
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			if scenario == "caller_owned" || scenario == "unchecked_eviction" {
				require.True(t, result.IsError, "caller-owned or unchecked selection cannot gain terminal storm permission")
				require.Zero(t, accepted.Load())
				return
			}
			if scenario == "new_physical_owner" {
				require.Positive(t, physicalSeq)
				require.True(t, result.IsError, "new physical ownership cannot borrow late-storm terminal permission")
				require.Zero(t, accepted.Load())
				return
			}
			if scenario == "victim_storm" {
				require.True(t, afterStorm, "the exact victim has a real live queued storm generation")
				require.False(t, result.IsError, "current-or-literal-pending response required; generic superseded does not satisfy the criterion: %s", resultText(result))
			}
			if result.IsError {
				require.Equal(t, "point_queue", scenario, "only the nonstorm point negative may retain its generic refusal")
				require.Zero(t, accepted.Load())
				return
			}
			body := extractTextResult(t, result)
			freshness, _ := body["freshness"].(map[string]any)
			if freshness["file_reason"] == "storm" {
				require.Equal(t, "victim_storm", scenario, "another path or ordinary point work cannot lend storm permission")
				require.Equal(t, "pending", freshness["graph_freshness"])
				require.NotEqual(t, true, freshness["readback_exact"])
				require.Equal(t, []any{"main.go"}, freshness["pending_files"])
				require.Equal(t, false, body["complete"])
				for _, key := range []string{"nodes", "defines", "source", "content"} {
					require.NotContains(t, body, key)
				}
				require.Zero(t, accepted.Load(), "a discarded data leaf cannot receive acceptance credit")
				proof := context.WithoutCancel(selected)
				cause := validateRequestProjections(proof)
				request := makeReq("get_file_summary", map[string]any{"path": "main.go", "format": "json"})
				before := allowance.attempts.Load()
				expired := context.WithValue(proof, projectedSourceDeadlineKey{}, time.Now().Add(-time.Nanosecond))
				terminal, terminalErr := s.lateStormTransitionPending(expired, request, cause)
				require.NoError(t, terminalErr)
				require.NotNil(t, terminal, "terminal formatting must not need unused private allowance")
				require.Equal(t, before, allowance.attempts.Load(), "formatting does not claim another attempt")
				require.ErrorIs(t, validateRequestProjections(proof), projectionSuperseded, "pending never revives old facts")
				for _, untyped := range []error{nil, errors.New(projectionSuperseded.Error())} {
					refused, refusedErr := s.lateStormTransitionPending(proof, request, untyped)
					require.NoError(t, refusedErr)
					require.Nil(t, refused, "nil/untyped same-text errors cannot gain storm authority")
				}
				wrong := makeReq("get_file_summary", map[string]any{"path": "other.go", "format": "json"})
				refused, refusedErr := s.lateStormTransitionPending(proof, wrong, cause)
				require.NoError(t, refusedErr)
				require.Nil(t, refused, "another valid owned path cannot borrow the selected victim")
				hard, cancelHard := context.WithDeadline(proof, time.Now().Add(-time.Nanosecond))
				defer cancelHard()
				refused, refusedErr = s.lateStormTransitionPending(hard, request, cause)
				require.ErrorIs(t, refusedErr, context.DeadlineExceeded)
				require.Nil(t, refused)
				foreign := filepath.Join(t.TempDir(), "foreign.go")
				require.NoError(t, os.WriteFile(foreign, external, 0600))
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.Symlink(foreign, path))
				_, _, resolveErr := s.resolveFilePath(proof, "main.go")
				require.Error(t, resolveErr)
				refused, refusedErr = s.lateStormTransitionPending(proof, request, cause)
				require.NoError(t, refusedErr)
				require.Nil(t, refused, "a rebound unresolved path cannot gain pending by string equality")
				require.NoError(t, os.Remove(path))
				require.NoError(t, os.WriteFile(path, external, 0600))
			} else {
				requireCanonicalDefinitionRows(t, "get_file_summary", result, external)
				require.Equal(t, int32(1), accepted.Load(), "only a fully revalidated current response receives credit")
			}
		})
	}
}

// Point startup must precede BOTH the held edit and external checked version.
// Its first real victim event is held before graph admission; no production
// replay/canonicalization is disabled and all original read caps remain intact.
func startRegisteredLateStormFixtureWatcher(t *testing.T, s *Server, path, scenario string, forceStartupReconcile bool) (*registeredLateStormWatcher, func(), func()) {
	t.Helper()
	threshold := 1
	if scenario == "point_queue" {
		threshold = 0
	}
	native, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 60000, StormThreshold: threshold, StormWindowMs: 5000, StormQuietPeriodMs: 60000}, zap.NewNop())
	require.NoError(t, err)
	watcher := &registeredLateStormWatcher{Watcher: native, eventPath: path, entered: make(chan struct{}), release: make(chan struct{})}
	if scenario == "other_path" {
		watcher.eventPath = filepath.Join(filepath.Dir(path), "other.go")
	}
	unblock := sync.OnceFunc(func() { close(watcher.release) })
	stop := sync.OnceFunc(func() { require.NoError(t, watcher.Stop()) })
	if scenario == "point_queue" {
		watcher.holdNextSourceEvent()
	}
	s.SetWatcher(watcher)
	require.NoError(t, watcher.Start([]string{filepath.Dir(path)}))
	t.Cleanup(stop)
	t.Cleanup(unblock)
	if forceStartupReconcile {
		// Controlled real publication at startup, not a claim about the hosted
		// event that was never captured. Before the edit this must retain old rows.
		require.NoError(t, s.indexer.IndexFile(path))
		require.Equal(t, 5, s.graph.GetNode("main.go::Neighbor").StartLine, "fixture startup must not publish the later external version")
	}
	return watcher, unblock, stop
}

// This same callback is installed by OnSourceEvent; repeated arm requests must
// not reuse the one-shot entered/release handshake for a later native event.
func (w *registeredLateStormWatcher) holdNextSourceEvent() { w.hold.Store(true) }

func TestRegisteredLateStormSourceCallbackArmsOnlyOnce(t *testing.T) {
	w := &registeredLateStormWatcher{eventPath: "victim.go", entered: make(chan struct{}), release: make(chan struct{})}
	var forwarded atomic.Int32
	callback := w.sourceEventCallback(func(string) { forwarded.Add(1) })
	release := sync.OnceFunc(func() { close(w.release) })
	t.Cleanup(release)
	w.holdNextSourceEvent()
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); callback("victim.go") }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("first callback never entered")
	}
	require.Zero(t, forwarded.Load(), "actual callback remains held before forwarding")
	w.holdNextSourceEvent() // common fixture setup after the early point arm
	release()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("held callback did not join")
	}
	require.Equal(t, int32(1), forwarded.Load())
	require.NotPanics(t, func() { callback("victim.go") }, "a second event must not close the consumed entered channel")
	require.Equal(t, int32(2), forwarded.Load(), "both actual callbacks forward exactly once")
}
