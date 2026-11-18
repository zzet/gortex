package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

var (
	trackDaemonRunningFn = daemon.IsRunning
	trackFamilyProbeFn   = probeTrackFamilyRoots
	trackExplainViewFn   = explainTrackWorktree
)

// refuseAutomaticWorktree runs before config mutation or any track RPC. The
// only daemon contacts are best-effort read-only identity/explanation probes,
// and only when a daemon already runs. It never autostarts one.
func refuseAutomaticWorktree(w io.Writer, target string, gc *config.GlobalConfig) bool {
	info := indexer.ResolveWorktree(target)
	if trackAsWorktree || !info.IsWorktree {
		return false
	}
	roots := gc.ConfiguredRepoRoots()
	knownTarget := gc.FindRepoByPath(target) != nil
	otherRoots := func(roots []string) []string {
		var others []string
		for _, root := range roots {
			if !pathkey.SamePathIdentity(pathkey.CanonicalExistingRoot(root), target) {
				others = append(others, root)
			}
		}
		return others
	}
	_, family := agents.AutomaticCheckoutFamily(target, otherRoots(roots))
	running := trackDaemonRunningFn()
	if family == "" && running {
		// Registered-only families can exist after an external config edit.
		// Read identity, never counts or a graph, before choosing the remedy.
		if registered, err := trackFamilyProbeFn(); err == nil {
			for _, root := range registered {
				knownTarget = knownTarget || pathkey.SamePathIdentity(pathkey.CanonicalExistingRoot(root), target)
			}
			_, family = agents.AutomaticCheckoutFamily(target, otherRoots(agents.FamilyRepoRoots(registered, roots)))
		}
	}
	if family == "" {
		if !knownTarget {
			fmt.Fprintf(w, "%s will become this Git family's tracked primary checkout. Prefer tracking the default-branch checkout instead (main checkout: %s).\n", target, info.MainRepoPath)
		}
		return false
	}
	fmt.Fprintf(w, "%s is a linked worktree of %s; it is served automatically over that checkout and needs no track. Pass --as-worktree to build a separate graph anyway.\n", target, family)
	if running {
		if raw, err := trackExplainViewFn(target); err == nil {
			var view viewBindingPayload
			if json.Unmarshal(raw, &view) == nil {
				state := view.CheckoutState
				if state == "" {
					state = "unknown"
				}
				route := "none"
				if view.Route != nil {
					route = view.Route.State
				}
				fmt.Fprintf(w, "explain-view: checkout_state=%s, effective_mode=%s, route=%s, reason=%s\n", state, view.EffectiveMode, route, view.Reason)
			}
		} else {
			fmt.Fprintf(w, "explain-view is currently unavailable: %v\n", err)
		}
	}
	return true
}

func probeTrackFamilyRoots() ([]string, error) {
	client, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli-track-guidance"})
	if err != nil {
		return nil, err
	}
	defer client.Close()
	response, err := client.ControlWithTimeout(daemon.ControlProbe, nil, daemonRoutingProbeTimeout)
	if err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, fmt.Errorf("daemon identity probe unavailable: %s", response.ErrorCode)
	}
	var probe daemon.ProbeResponse
	if err := json.Unmarshal(response.Result, &probe); err != nil {
		return nil, err
	}
	var roots []string
	for _, repo := range probe.TrackedRepos {
		roots = append(roots, repo.Path)
	}
	return roots, nil
}

func explainTrackWorktree(path string) (json.RawMessage, error) {
	// An unbound CWD is deliberate for this metadata-only diagnostic, just as
	// checkoutsRelayPath avoids binding to the checkout being explained.
	client, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeMCP, ClientName: "cli-track-guidance", Tools: cliLegacyToolSurface, ToolsMode: cliLegacyToolMode})
	if err != nil {
		return nil, err
	}
	defer client.Close()
	if err := client.Conn.SetDeadline(time.Now().Add(daemonRoutingProbeTimeout)); err != nil {
		return nil, err
	}
	executor := &daemonExecutor{client: client, pinJSONDefault: true}
	return executor.CallTool(context.Background(), "explain_view", map[string]any{"path": path})
}
