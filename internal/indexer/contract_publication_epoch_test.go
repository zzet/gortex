package indexer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/viewmetrics"
)

func TestContractSchedulerEqualAcceptanceTimesKeepPublicationOrderAndFences(t *testing.T) {
	for _, mode := range []string{"latest-repository", "relevant-input-fences"} {
		t.Run(mode, func(t *testing.T) {
			p := newSchedulerProbe(t, nil)
			// Model a clock tick shared by distinct real notifications. Do not
			// sleep or require the host clock to advance between publications.
			stamp := time.Unix(1700000000, 0)
			publish := func(repo string) {
				p.c.Published(t.Context(), repo, "")
				p.c.mu.Lock()
				p.c.acceptedAt[repo] = stamp
				p.c.mu.Unlock()
			}
			publish("repo-b")
			publish("repo-a")
			var validations atomic.Int64
			var reject atomic.Bool
			capture := p.c.options.Capture
			p.c.options.Capture = func(ctx context.Context, source *ContractCaptureSource, inputs *graphview.SelectedContractInputs, repo, checkout string) (ContractFollowupSnapshot, []ContractFollowupTarget, error) {
				snapshot, targets, err := capture(ctx, source, inputs, repo, checkout)
				snapshot.inputsCurrent = func(context.Context) error {
					validations.Add(1)
					if reject.Load() {
						return graph.ErrContractProjectionStale
					}
					return nil
				}
				return snapshot, targets, err
			}
			jobs := make(chan *contractAnalysisJob, 1)
			resume := make(chan struct{})
			defer close(resume)
			p.c.run = func(job *contractAnalysisJob) string {
				defer job.snapshot.Release()
				select {
				case jobs <- job:
				case <-p.c.ctx.Done():
					return viewmetrics.ContractJobPreempted
				}
				select {
				case <-resume:
				case <-p.c.ctx.Done():
				}
				return viewmetrics.ContractJobPublished
			}
			repos := []string{"repo-a", "repo-b"}
			actor := NewContractActor("", repos)
			_, err := p.c.Schedule(t.Context(), ContractScheduleRequest{Actor: actor, Key: graph.ContractAttachmentKey{RepoPrefix: "repo-b"}, Inputs: schedulerInputs(repos, "same"), Epoch: p.c.InputEpoch()})
			require.NoError(t, err)
			var job *contractAnalysisJob
			select {
			case job = <-jobs:
			case <-time.After(time.Minute):
				t.Fatal("scheduler never dispatched")
			}
			if mode == "latest-repository" {
				require.Equal(t, []string{"repo-a"}, job.priorityRepos, "publication order wins over equal timestamps and the selected repository")
				return
			}
			require.NoError(t, job.backgroundCurrent(t.Context()))
			publish("outside-cohort")
			require.NoError(t, job.backgroundCurrent(t.Context()))
			require.Zero(t, validations.Load(), "unchanged and unrelated notifications do not reread the cohort")
			for expected := int64(1); expected <= 2; expected++ {
				publish("repo-a")
				require.NoError(t, job.backgroundCurrent(t.Context()))
				require.Equal(t, expected, validations.Load(), "each relevant notification is observed despite an identical timestamp")
				require.NoError(t, job.backgroundCurrent(t.Context()))
				require.Equal(t, expected, validations.Load(), "a validated epoch is not reread")
			}
			reject.Store(true)
			publish("repo-b")
			require.ErrorIs(t, job.backgroundCurrent(t.Context()), errContractJobKeyMoved, "the full input fence still refuses a real stale input")
			require.EqualValues(t, 3, validations.Load())
		})
	}
}
