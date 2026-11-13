package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

// The status consumer needs a present numeric zero before the first real hold,
// without turning a disabled or failed producer into a working one on paper.
func TestDaemonContractIdleCounterAvailability(t *testing.T) {
	const key = "views_contract_linked_jobs_total{policy=idle,event=deferred}"
	for _, name := range []string{"idle", "disabled", "failed", "eager", "no_demand", "no_linked_inputs"} {
		t.Run(name, func(t *testing.T) {
			viewmetrics.Reset()
			t.Cleanup(viewmetrics.Reset)
			startupPublicationEnv(t)
			t.Setenv("GORTEX_ASYNC_CONTRACTS", "")
			t.Setenv(indexer.ContractLinkedPolicyEnv, "")
			state, err := buildDaemonState(zap.NewNop())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, state.shared.Close()) })
			if name == "disabled" {
				t.Setenv("GORTEX_ASYNC_CONTRACTS", "0")
			}
			if name == "eager" {
				state.contractAnalysis.LinkedPolicy = "eager"
			}
			runtime, err := installDaemonContractAnalysisWith(state, zap.NewNop(), func(options *indexer.ContractAnalysisCoordinatorOptions) {
				options.Demand = func() (bool, uint64) { return true, 1 }
				if name == "failed" {
					options.QueueCapacity = -1
				}
				if name == "no_demand" {
					options.Demand = nil
				}
				if name == "no_linked_inputs" {
					options.LinkedInputs = nil
				}
			})
			if name == "failed" {
				require.Error(t, err)
				require.Nil(t, runtime)
			} else {
				require.NoError(t, err)
			}
			if runtime != nil {
				t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
			}
			controller := &realController{lifecycle: state.lifecycle}
			poll := func() map[string]json.RawMessage {
				t.Helper()
				status := controller.collectViewsStatus(t.Context())
				require.NotNil(t, status)
				wire, err := json.Marshal(status)
				require.NoError(t, err)
				var decoded struct {
					Counters map[string]json.RawMessage `json:"counters"`
				}
				require.NoError(t, json.Unmarshal(wire, &decoded))
				return decoded.Counters
			}
			if name != "idle" {
				require.NotContains(t, poll(), key, "an unavailable idle producer must not claim zero")
				return
			}
			require.Contains(t, poll(), key, "configured idle producer must export zero before its first hold")
			require.Equal(t, json.RawMessage("0"), poll()[key], "wire zero must be numeric, not null")
			// Existing public scheduling admission, with demand held active so no
			// capture or job body can run. One real held actor counts once.
			inputs := graph.ContractInputState{RepoPrefix: "tracked", CheckoutID: "linked", InputVersion: "v", InputFingerprint: "accepted", Accepted: true}
			_, err = runtime.coordinator.Schedule(t.Context(), indexer.ContractScheduleRequest{
				Actor:  indexer.NewContractActor("linked", []string{"tracked"}),
				Key:    graph.ContractAttachmentKey{RepoPrefix: "tracked", CheckoutID: "linked"},
				Inputs: &graphview.SelectedContractInputs{State: inputs, Witnesses: []graph.ContractInputWitness{{State: inputs, Found: true}}},
				Source: &indexer.ContractCaptureSource{}, Epoch: runtime.coordinator.InputEpoch(),
			})
			require.NoError(t, err)
			require.Eventually(t, func() bool { return viewmetrics.Read().Counters[key] == 1 }, 5*time.Second, time.Millisecond)
			for range 3 {
				require.Contains(t, poll(), key)
				require.Equal(t, json.RawMessage("1"), poll()[key], "status polling cannot increment or reset the counter")
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			require.Nil(t, controller.collectViewsStatus(canceled))
			require.Nil(t, (&realController{}).collectViewsStatus(t.Context()))
			require.Nil(t, (&realController{lifecycle: &indexer.CheckoutLifecycle{}}).collectViewsStatus(t.Context()))
		})
	}
}
