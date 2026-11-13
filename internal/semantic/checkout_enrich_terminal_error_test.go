package semantic

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

type terminalErrorCheckoutProvider struct {
	*rootRecordingProvider
	started, release chan struct{}
	waitForCancel    bool
	independent      error
	drained          atomic.Bool
}

type terminalErrorLegacyCheckoutProvider struct {
	control *terminalErrorCheckoutProvider
}

func (p *terminalErrorLegacyCheckoutProvider) Name() string        { return p.control.Name() }
func (p *terminalErrorLegacyCheckoutProvider) Languages() []string { return p.control.Languages() }
func (*terminalErrorLegacyCheckoutProvider) Available() bool       { return true }
func (*terminalErrorLegacyCheckoutProvider) Close() error          { return nil }
func (*terminalErrorLegacyCheckoutProvider) EnrichFile(graph.Store, string, string) (*EnrichResult, error) {
	return nil, nil
}
func (p *terminalErrorLegacyCheckoutProvider) Enrich(graph.Store, string) (*EnrichResult, error) {
	close(p.control.started)
	<-p.control.release
	p.control.drained.Store(true)
	return nil, errors.Join(context.Canceled, p.control.independent)
}

func (p *terminalErrorCheckoutProvider) EnrichRepoContext(ctx context.Context, _ graph.Store, _, _ string, _ EnrichDeadlinePolicy) (*EnrichResult, error) {
	close(p.started)
	if p.waitForCancel {
		<-ctx.Done()
	}
	<-p.release
	p.drained.Store(true)
	if p.independent != nil {
		return nil, errors.Join(ctx.Err(), p.independent)
	}
	return nil, errors.Join(ctx.Err(), fmt.Errorf("provider: %w", ctx.Err()))
}

func TestCheckoutCanceledPassPreservesDrainedProviderError(t *testing.T) {
	for _, mode := range []string{"mixed_cancellation", "mixed_after_grace", "mixed_legacy", "pure_joined_cancellation", "live_optional_failure"} {
		t.Run(mode, func(t *testing.T) {
			oldGrace := enrichCancelGrace
			enrichCancelGrace = 20 * time.Millisecond
			t.Cleanup(func() { enrichCancelGrace = oldGrace })
			sentinel := errors.New("independent checkout provider failure")
			p := &terminalErrorCheckoutProvider{rootRecordingProvider: &rootRecordingProvider{name: "terminal-go", langs: []string{"go"}},
				started: make(chan struct{}), release: make(chan struct{}), waitForCancel: mode != "live_optional_failure"}
			if mode != "pure_joined_cancellation" {
				p.independent = sentinel
			}
			mgr := NewManager(Config{Enabled: true, CheckoutLSPMaxWorkspaces: 2,
				Providers: []ProviderConfig{{Name: p.Name(), Languages: p.Languages(), Priority: 1, Enabled: true}}}, zap.NewNop())
			if mode == "mixed_legacy" {
				mgr.RegisterProvider(&terminalErrorLegacyCheckoutProvider{control: p})
			} else {
				mgr.RegisterProvider(p)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			store := checkoutStore(t)
			type outcome struct {
				report CheckoutEnrichReport
				err    error
			}
			done := make(chan outcome, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				report, err := mgr.EnrichCheckoutContext(ctx, store, CheckoutEnrichRequest{RepoPrefix: checkoutRepo,
					CheckoutID: "checkout-a", Root: "/family/a", Fingerprint: "fp"})
				done <- outcome{report, err}
			}()
			released := false
			t.Cleanup(func() {
				cancel()
				if !released {
					close(p.release)
				}
				_ = mgr.Close()
				select {
				case <-finished:
				case <-time.After(2 * time.Second):
					t.Error("owned checkout pass did not drain")
				}
			})
			select {
			case <-p.started:
			case <-time.After(time.Second):
				t.Fatal("provider did not start")
			}
			if p.waitForCancel {
				cancel()
				wait := time.Millisecond
				if mode == "mixed_after_grace" || mode == "mixed_legacy" {
					wait = enrichCancelGrace + 20*time.Millisecond
				}
				select {
				case <-done:
					t.Fatal("checkout pass returned before its provider drained")
				case <-time.After(wait):
				}
			}
			close(p.release)
			released = true
			select {
			case got := <-done:
				require.True(t, p.drained.Load())
				if mode == "live_optional_failure" {
					require.NoError(t, got.err, "live optional provider failures remain partial")
					require.True(t, got.report.Partial)
					require.ErrorIs(t, got.report.ProviderTerminalError(), sentinel, "raw terminal identity must outlive the live-context return check")
				} else {
					require.ErrorIs(t, got.err, context.Canceled)
					if mode != "pure_joined_cancellation" {
						require.ErrorIs(t, got.err, sentinel)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("drained checkout pass did not finish")
			}
		})
	}
}
