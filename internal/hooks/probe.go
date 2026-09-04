package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

func unmarshalResult(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// controlRoundTrip dials the local daemon over its unix socket, runs one
// control RPC, and decodes the result into out. The whole exchange (dial +
// handshake + RPC) must fit inside timeout; otherwise errProbeTimeout is
// returned and the caller falls through to soft guidance.
//
// Returns errDaemonUnreachable when the daemon isn't running — the hook
// distinguishes "no signal" from "probed and missed" so telemetry stays
// clean.
func controlRoundTrip(kind string, params any, out any, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	done := make(chan error, 1)

	go func() {
		client, err := daemon.Dial(daemon.Handshake{
			Mode:       daemon.ModeControl,
			ClientName: "gortex-hook",
		})
		if err != nil {
			if errors.Is(err, daemon.ErrDaemonUnavailable) {
				done <- errDaemonUnreachable
				return
			}
			done <- fmt.Errorf("dial daemon: %w", err)
			return
		}
		defer client.Close()

		// Cap the round trip at the remaining budget so a stuck daemon
		// can't pin the goroutine past timeout. Passed explicitly rather
		// than left to Control's default: a hook runs on the agent's
		// critical path and its budget is far tighter than the default.
		//
		// The clamp matters: a non-positive budget means "no bound" to
		// ControlWithTimeout, so handing it a window the dial already
		// consumed would turn the tightest caller in the tree into the
		// only unbounded one.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			done <- fmt.Errorf("daemon probe budget exhausted before the %s rpc", kind)
			return
		}
		resp, err := client.ControlWithTimeout(kind, params, remaining)
		if err != nil {
			done <- fmt.Errorf("control rpc: %w", err)
			return
		}
		if !resp.OK {
			done <- fmt.Errorf("daemon rejected %s [%s]: %s", kind, resp.ErrorCode, resp.ErrorMsg)
			return
		}
		if err := unmarshalResult(resp.Result, out); err != nil {
			done <- fmt.Errorf("decode result: %w", err)
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(time.Until(deadline)):
		return errProbeTimeout
	}
}

// probeViaDaemon runs one search_symbols control RPC.
//
// scope is the file or directory the probe is about. The daemon answers out of
// the graph that path reads through, so a pattern probed from inside an
// automatic worktree is matched against that working copy's composed view
// rather than against the family primary's corpus. An empty scope keeps the
// base-corpus answer.
//
// The answer's view block is recorded, not acted on: a fallback answer still
// drives the deny the same way an exact one does. Recording it here matters as
// much as on the coverage verb — a grace-window deny whose evidence came from
// the family primary is a degradation, and one that left no record would be
// indistinguishable from a hit on the worktree's own view.
func probeViaDaemon(pattern, scope string, timeout time.Duration) ([]grepSymbolHit, error) {
	var result daemon.SearchSymbolsResult
	err := controlRoundTrip(daemon.ControlSearchSymbols, daemon.SearchSymbolsParams{
		Query: pattern,
		Limit: 10,
		Path:  scope,
	}, &result, timeout)
	if err != nil {
		return nil, err
	}
	logProbeViewFallback(daemon.ControlSearchSymbols, result.View)
	hits := make([]grepSymbolHit, 0, len(result.Hits))
	for _, h := range result.Hits {
		hits = append(hits, grepSymbolHit{
			Name:     h.Name,
			Kind:     h.Kind,
			FilePath: h.FilePath,
			Line:     h.Line,
		})
	}
	return hits, nil
}

// fileCoverageViaDaemon asks the daemon whether the graph serving path holds
// definition symbols for it.
//
// The path resolution is the daemon's, not the hook's: only the daemon can
// tell an automatic worktree from an ordinary tracked root, and only it knows
// whether a composed view exists for that working copy yet. A hook that
// resolved the path itself reported every worktree file as belonging to no
// tracked repository, which read as "not indexed" and let native tools
// through even where a routed view was serving.
func fileCoverageViaDaemon(path string, timeout time.Duration) (daemon.FileCoverageResult, bool) {
	var result daemon.FileCoverageResult
	if err := controlRoundTrip(daemon.ControlFileCoverage,
		daemon.FileCoverageParams{Path: path}, &result, timeout); err != nil {
		return daemon.FileCoverageResult{}, false
	}
	return result, true
}

// dirCoverageViaDaemon asks the daemon whether the graph serving path holds
// indexed source under it. The path is resolved daemon-side for the reason
// fileCoverageViaDaemon documents.
func dirCoverageViaDaemon(path string, timeout time.Duration) (daemon.DirCoverageResult, bool) {
	var result daemon.DirCoverageResult
	if err := controlRoundTrip(daemon.ControlDirCoverage,
		daemon.DirCoverageParams{Path: path}, &result, timeout); err != nil {
		return daemon.DirCoverageResult{}, false
	}
	return result, true
}
