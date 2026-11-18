package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/progress"
)

// exitTempFail is EX_TEMPFAIL from sysexits.h: the daemon is still loading the
// repository, and the same command will work if run again later. Hooks and
// scripts branch on it instead of parsing the message.
const exitTempFail = 75

// defaultLoadingWait is how long an interactive CLI call waits for a
// repository that is still loading before it gives up for now.
const defaultLoadingWait = 30 * time.Second

// loadingWaitPoll is how often a waiting call asks again.
const loadingWaitPoll = time.Second

var loadingWait time.Duration

func init() {
	rootCmd.PersistentFlags().DurationVar(&loadingWait, "loading-wait", defaultLoadingWait,
		"how long a graph query waits for a repository the daemon is still loading after a restart "+
			"(default 30s when stdout is a terminal, 0 otherwise; 0 fails fast with exit code 75)")
}

// repoLoadingError is the typed form of the daemon's repo_loading refusal: a
// configured repository the daemon has not finished loading since it started.
// It carries exit code 75, so a script or hook that runs the CLI can retry
// later without parsing the message.
type repoLoadingError struct {
	admissionReason string
	repo            string
	phase           string
	percent         int
	// waited is how long the CLI already waited before giving up for now.
	waited time.Duration
}

func (e *repoLoadingError) Error() string {
	msg := fmt.Sprintf("the gortex daemon is still loading %s after a restart (%s, %d%%)", e.repo, e.phase, e.percent)
	if e.waited > 0 {
		msg += fmt.Sprintf("; still loading after waiting %s", e.waited.Round(time.Second))
	}
	if e.admissionReason != "" {
		msg = fmt.Sprintf("the gortex daemon is still resolving graph admission (repo_loading, %s)", e.admissionReason)
		if e.waited > 0 {
			msg += fmt.Sprintf("; still loading after waiting %s", e.waited.Round(time.Second))
		}
		return msg + " — graph admission for this path is not yet established; retry shortly (or pass --loading-wait to wait for it)"
	}
	return msg + " — the repository is already tracked; retry shortly (or pass --loading-wait to wait for it)"
}

// ExitCode implements the process-exit contract execute honours.
func (e *repoLoadingError) ExitCode() int { return exitTempFail }

// effectiveLoadingWait is how long this invocation waits for a loading
// repository: the explicit --loading-wait when given, otherwise the default on
// a terminal and zero for a hook or a script, which must not stall.
func effectiveLoadingWait() time.Duration {
	if flag := rootCmd.PersistentFlags().Lookup("loading-wait"); flag != nil && flag.Changed {
		return max(loadingWait, 0)
	}
	if progress.IsTTY(os.Stdout) {
		return defaultLoadingWait
	}
	return 0
}

// callToolWaitingForLoad runs one tool call and, when the daemon answers that
// the repository is still loading, retries it until it answers or the wait
// runs out. Running out is reported as "still loading", never as a failure of
// the work.
func callToolWaitingForLoad(exec cliExecutor, tool string, args map[string]any, wait time.Duration) (json.RawMessage, error) {
	out, err := exec.CallTool(context.Background(), tool, args)
	var loading *repoLoadingError
	if !errors.As(err, &loading) || wait <= 0 {
		return out, err
	}
	fmt.Fprintf(os.Stderr, "[gortex] the daemon is still loading %s (%s, %d%%); waiting up to %s\n",
		loading.repo, loading.phase, loading.percent, wait.Round(time.Second))
	start := time.Now()
	deadline := start.Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(min(loadingWaitPoll, time.Until(deadline)))
		out, err = exec.CallTool(context.Background(), tool, args)
		var still *repoLoadingError
		if !errors.As(err, &still) {
			return out, err
		}
		loading = still
	}
	loading.waited = time.Since(start)
	return nil, loading
}

// trackedRepoLoading reports whether the tracked repository at path is still
// loading after a daemon start, as the probe reported it.
func trackedRepoLoading(st daemon.StatusResponse, path string) bool {
	for _, repo := range st.TrackedRepos {
		if repo.Loading && pathkey.EqualPaths(repo.Path, path) {
			return true
		}
	}
	return false
}
