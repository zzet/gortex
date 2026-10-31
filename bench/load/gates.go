package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type readiness struct {
	Ready        bool        `json:"ready"`
	Enriched     bool        `json:"enriched"`
	TrackedRepos []probeRepo `json:"tracked_repos"`
	LoadedRoots  *[]string   `json:"loaded_roots"`
}

type probeRepo struct {
	Path   string `json:"path"`
	Prefix string `json:"prefix"`
	State  string `json:"state"`
	Reason string `json:"state_reason"`
}

func readyCoverage(state readiness, expected []privateRepo) (bool, error) {
	for _, repo := range expected {
		found := false
		for _, tracked := range state.TrackedRepos {
			if filepath.Clean(tracked.Path) != filepath.Clean(repo.Path) {
				continue
			}
			if tracked.State == "failed" || tracked.State == "missing" {
				return false, fmt.Errorf("repo %s is %s: %s", repo.Corpus.Label, tracked.State, tracked.Reason)
			}
			found = tracked.Prefix != "" && (tracked.State == "ready" || tracked.State == "")
		}
		if !found {
			return false, nil
		}
		if state.LoadedRoots != nil {
			loaded := false
			for _, root := range *state.LoadedRoots {
				loaded = loaded || filepath.Clean(root) == filepath.Clean(repo.Path)
			}
			if !loaded {
				return false, nil
			}
		}
	}
	return true, nil
}

func (p *privateRun) awaitSocket(ctx context.Context) error {
	for {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := control(probeCtx, p.Socket, "probe", nil)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-p.done:
			return fmt.Errorf("private daemon exited before handshake: %v", p.exitErr)
		default:
		}
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("private daemon socket: %w", err)
		}
	}
}

// Readiness and enrichment are separate measured endpoints. Neither proves
// analysis publication, which is checked over the public MCP surface below.
func (p *privateRun) awaitReady(ctx context.Context, start time.Time, expected []privateRepo) (map[string]json.RawMessage, map[string]Metric, error) {
	gate := make(map[string]json.RawMessage)
	metrics := map[string]Metric{"cold_queryable_observed_upper_bound_seconds": unavailable("s", "queryable endpoint not reached"), "cold_enrichment_observed_upper_bound_seconds": unavailable("s", "enrichment endpoint not reached"), "readiness_poll_interval_seconds": measured(.25, "s")}
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		data, err := control(probeCtx, p.Socket, "probe", nil)
		cancel()
		if err == nil {
			var state readiness
			if err := json.Unmarshal(data, &state); err != nil {
				return gate, metrics, err
			}
			gate["readiness"] = data
			covered, coverageErr := readyCoverage(state, expected)
			if coverageErr != nil {
				return gate, metrics, coverageErr
			}
			if state.Ready && covered {
				if metrics["cold_queryable_observed_upper_bound_seconds"].Value == nil {
					metrics["cold_queryable_observed_upper_bound_seconds"] = measured(time.Since(start).Seconds(), "s")
					gate["queryable_observation"] = data
				}
				if state.Enriched {
					metrics["cold_enrichment_observed_upper_bound_seconds"] = measured(time.Since(start).Seconds(), "s")
					return gate, metrics, nil
				}
			}
		}
		select {
		case <-p.done:
			return gate, metrics, fmt.Errorf("daemon exited during readiness gate: %v", p.exitErr)
		default:
		}
		if err := waitContext(ctx, 250*time.Millisecond); err != nil {
			return gate, metrics, fmt.Errorf("readiness/enrichment gate: %w", err)
		}
	}
}

func (p *privateRun) writeBootRepos(worktrees bool) ([]privateRepo, error) {
	configured := p.Repos
	if worktrees {
		configured = configured[:1]
	}
	entries := make([]map[string]string, 0, len(configured))
	for _, repo := range configured {
		entries = append(entries, map[string]string{"path": repo.Path, "name": repo.Corpus.Label})
	}
	data, err := json.Marshal(map[string]any{"repos": entries})
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(p.Root, "config", "gortex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return configured, os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0o600)
}

func preflightRoot(ctx context.Context, p *privateRun, client *socketClient, repo privateRepo, linked bool, deadline time.Duration, phase *Phase) error {
	if linked {
		for retry := 0; ; retry++ {
			data, err := control(ctx, p.Socket, "search_symbols", map[string]any{"path": repo.Path, "query": repo.Corpus.Query, "limit": 1})
			if err == nil {
				var answer struct {
					View *struct {
						Kind   string `json:"kind"`
						Exact  bool   `json:"exact"`
						Prefix string `json:"repo_prefix"`
						Reason string `json:"fallback_reason"`
					} `json:"view"`
				}
				if err := json.Unmarshal(data, &answer); err != nil {
					return err
				}
				phase.Gate[repo.Corpus.Label+".view"] = data
				if answer.View == nil {
					return errors.New("linked checkout has no public view receipt")
				}
				if answer.View.Exact && answer.View.Kind == "worktree" && answer.View.Prefix == repo.Prefix {
					break
				}
				if answer.View.Kind != "unrouted" && answer.View.Reason != "view_building" {
					return fmt.Errorf("linked checkout served fallback/base view: %s", data)
				}
			}
			if err := waitContext(ctx, busyBackoff(retry)); err != nil {
				return fmt.Errorf("linked view preflight: %w", err)
			}
		}
	}
	r := request{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": repo.Corpus.Symbol}}}
	result, rows, err := callTool(ctx, client, r, dispatchArgs(r, repo), "probe", 0, 0, 0, deadline, waitContext)
	phase.Attempts = append(phase.Attempts, rows...)
	phase.Gate[repo.Corpus.Label+".source_preflight"] = result
	if err != nil {
		return fmt.Errorf("repo %s source-backed target preflight: %w", repo.Corpus.Label, err)
	}
	return nil
}

func warmAnalysis(ctx context.Context, client *socketClient, deadline time.Duration) (map[string]json.RawMessage, []Attempt, error) {
	gate := make(map[string]json.RawMessage)
	var attempts []Attempt
	r := request{Tool: "analyze", Operation: "hotspots", Arguments: map[string]any{"kind": "hotspots", "output": map[string]any{"format": "json"}}}
	for {
		result, rows, err := callTool(ctx, client, r, r.Arguments, "probe", 0, 0, 0, deadline, waitContext)
		attempts = append(attempts, rows...)
		if err != nil {
			return gate, attempts, fmt.Errorf("analysis consumer gate: %w", err)
		}
		payload, err := textPayload(result, "content")
		if err != nil {
			return gate, attempts, fmt.Errorf("analysis consumer did not return JSON: %w", err)
		}
		gate["analysis_consumer"] = payload
		var state struct {
			Status   string          `json:"status"`
			Hotspots json.RawMessage `json:"hotspots"`
		}
		if err := json.Unmarshal(payload, &state); err != nil {
			return gate, attempts, err
		}
		if state.Status == "analysis_pending" {
			if err := waitContext(ctx, time.Second); err != nil {
				return gate, attempts, err
			}
			continue
		}
		if state.Hotspots == nil || state.Status != "" {
			return gate, attempts, errors.New("analysis consumer supplied no current-snapshot answer")
		}
		resourceCtx, cancel := context.WithTimeout(ctx, deadline)
		result, err = client.call(resourceCtx, "resources/read", map[string]any{"uri": "gortex://communities"})
		cancel()
		if err != nil {
			return gate, attempts, fmt.Errorf("analysis publication resource: %w", err)
		}
		payload, err = textPayload(result, "contents")
		if err != nil {
			return gate, attempts, err
		}
		gate["analysis_publication"] = payload
		var receipt struct {
			Returned    *int            `json:"returned"`
			Total       *int            `json:"total"`
			Communities json.RawMessage `json:"communities"`
		}
		if err := json.Unmarshal(payload, &receipt); err != nil {
			return gate, attempts, err
		}
		if receipt.Returned == nil || receipt.Total == nil || receipt.Communities == nil {
			return gate, attempts, errors.New("current analysis has no public durable-generation receipt; phase not measured")
		}
		return gate, attempts, nil
	}
}

// Notification support is a recorded capability, not a substitute for a public
// readiness/publication receipt. Older builds can explicitly report unavailable.
func subscribeInvalidations(ctx context.Context, client *socketClient, label string, deadline time.Duration, phase *Phase) {
	r := request{Tool: "session", Operation: "subscribe", Arguments: map[string]any{"operation": "subscribe", "channel": "graph_invalidated"}}
	data, rows, err := callTool(ctx, client, r, r.Arguments, "probe", 0, -1, 0, deadline, waitContext)
	phase.Attempts = append(phase.Attempts, rows...)
	receipt := struct {
		Available bool            `json:"available"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error"`
	}{Available: err == nil, Response: data}
	if err != nil {
		receipt.Error = err.Error()
	}
	encoded, _ := json.Marshal(receipt)
	phase.Gate[label+".graph_invalidated_subscription"] = encoded
}
