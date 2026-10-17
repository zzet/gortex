package indexer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestContractWindowScopeDistinguishesRepeatedInputAttempts(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	req := ContractFollowupRequest{Logger: zap.New(core), Snapshot: ContractFollowupSnapshot{Key: graph.ContractAttachmentKey{RepoPrefix: "repo", InputVersion: "same", InputFingerprint: "same"}}}
	for range 2 {
		c := newContractConvergence(t.Context(), req, nil, map[string]bool{"repo": true}, &ContractFollowupReport{})
		c.blockedSince = time.Now()
		c.noteWindowGaps()
	}
	lines := logs.FilterMessage("contract analysis window scope").All()
	require.Len(t, lines, 2)
	first, second := lines[0].ContextMap(), lines[1].ContextMap()
	require.NotEmpty(t, first["window_process"])
	require.Equal(t, first["window_process"], second["window_process"])
	require.NotEqual(t, first["window_job"], second["window_job"])
}

func TestContractWindowLeadUsesTheExactCloseOnBothWakePaths(t *testing.T) {
	for _, fast := range []bool{true, false} {
		t.Run(map[bool]string{true: "already_closed", false: "notification"}[fast], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			closedAt := time.Now().Add(-5 * time.Second)
			var closed atomic.Bool
			closed.Store(fast)
			changed, waiting := make(chan struct{}), make(chan struct{})
			stamp := ContractReceiptStamp{Seq: 7, Writing: true}
			c := &contractConvergence{ctx: ctx, waitOn: map[string]ContractReceiptStamp{"repo/a.go": stamp}}
			c.req.WaitPublished = func(ctx context.Context, _ uint64, _ time.Time) bool { close(waiting); <-ctx.Done(); return false }
			c.snap.ReceiptEvents = func() (uint64, <-chan struct{}) {
				if closed.Load() {
					return 1, changed
				}
				return 0, changed
			}
			c.snap.Scan = func(context.Context, []string) (ContractFollowupScan, error) {
				now := stamp
				if closed.Load() {
					now.Writing = false
				}
				return ContractFollowupScan{Settled: true, Stamps: map[string]ContractReceiptStamp{"repo/a.go": now}}, nil
			}
			c.snap.ReceiptWindowClose = func(windows map[string]ContractReceiptStamp) (time.Time, bool) {
				return closedAt, closed.Load() && windows["repo/a.go"].Seq == 7
			}
			done := make(chan bool, 1)
			go func() { done <- c.waitWake(contractWake{}, time.Now().Add(10*time.Second)) }()
			if !fast {
				select {
				case <-waiting:
				case <-ctx.Done():
					t.Fatal("wait never subscribed")
				}
				closed.Store(true)
				close(changed)
			}
			select {
			case woke := <-done:
				require.True(t, woke)
			case <-ctx.Done():
				t.Fatal("close never woke the wait")
			}
			require.Equal(t, closedAt, c.wokeAt, "resuming after the close must include dispatch delay in its lead")
			require.Equal(t, "window_close", c.wakeSource)
		})
	}
}

func TestContractWindowLeadNeverUsesAnUnmatchedEvent(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	c := &contractConvergence{logger: zap.New(core)}
	c.measureWindowWake(map[string]ContractReceiptStamp{"repo/a.go": {Seq: 9, Writing: true}}, "receipt_unmatched")
	c.snap.ReceiptWindowClose = func(map[string]ContractReceiptStamp) (time.Time, bool) { return time.Now().Add(-time.Hour), false }
	c.measureWindowWake(map[string]ContractReceiptStamp{"repo/a.go": {Seq: 9, Writing: true}}, "receipt_unmatched")
	require.True(t, c.wokeAt.IsZero())
	c.noteReask(0, false, time.Millisecond, false, c.wakeSource, c.wokeAt)
	fields := logs.All()[0].ContextMap()
	require.Equal(t, float64(-1), fields["lead_ms"])
	require.Equal(t, false, fields["lead_valid"])
	require.Equal(t, "receipt_unmatched", fields["lead_source"])
	require.Empty(t, c.leads)
}

func TestContractWindowScopeRecordsTheCompletePostExtractionInterval(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	since := time.Now().Add(-time.Second)
	c := &contractConvergence{logger: zap.New(core), snap: ContractFollowupSnapshot{Key: graph.ContractAttachmentKey{RepoPrefix: "repo", CheckoutID: "checkout"}}, repos: map[string]bool{"repo": true}, blocked: map[string]bool{"repo/z.go": true, "repo/a.go": true}, blockedSince: since}
	var queried time.Time
	c.snap.WindowLastCloses = func(paths []string, before time.Time) ([]store_sqlite.ContractCoreWindowClose, []string) {
		require.Equal(t, []string{"repo/a.go", "repo/z.go"}, paths)
		queried = before
		return []store_sqlite.ContractCoreWindowClose{{Repo: "repo", Path: "repo/a.go", OpenerSeq: 7, ClosedAtNS: since.Add(time.Millisecond).UnixNano()}}, []string{"repo/z.go"}
	}
	before := time.Now()
	c.noteWindowGaps()
	after := time.Now()
	lines := logs.FilterMessage("contract analysis window scope").All()
	require.Len(t, lines, 1)
	fields := lines[0].ContextMap()
	require.Equal(t, since.UnixNano(), fields["postextract_since_ns"])
	require.GreaterOrEqual(t, fields["measurement_end_ns"].(int64), before.UnixNano())
	require.LessOrEqual(t, fields["measurement_end_ns"].(int64), after.UnixNano())
	require.ElementsMatch(t, []string{"repo/a.go", "repo/z.go"}, fields["blocked_paths"])
	require.Equal(t, "", fields["checkout"])
	require.Equal(t, "checkout", fields["actor_checkout"])
	require.ElementsMatch(t, []string{"repo/z.go"}, fields["last_close_missing"])
	require.NotEmpty(t, fields["last_closes"])
	require.Equal(t, queried.UnixNano(), fields["measurement_end_ns"])
}

func TestContractRepairWorkerLogsItsStartOnce(t *testing.T) {
	f := newContractRepairFixture(t, nil)
	require.Eventually(t, func() bool { return len(f.logs.FilterMessage("contract repair started").All()) == 1 }, 10*time.Second, time.Millisecond)
	for range 3 {
		f.r.Scan([]string{"fixture"})
	}
	lines := f.logs.FilterMessage("contract repair started").All()
	require.Len(t, lines, 1)
	require.Equal(t, "fixture", lines[0].ContextMap()["repo"])
}
