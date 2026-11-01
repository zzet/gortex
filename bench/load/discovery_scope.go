package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const checkoutDiscoveryScenario = "worktree-discovery"

type checkoutDiscoveryObservation struct {
	Root         string          `json:"root"`
	FamilyRole   string          `json:"family_role"`
	Prefix       string          `json:"repo_prefix"`
	Query        string          `json:"query"`
	Added        time.Time       `json:"added"`
	FirstRequest time.Time       `json:"first_request"`
	ExactAt      time.Time       `json:"exact_at"`
	ExactViewLag Metric          `json:"exact_view_lag_ms"`
	View         json.RawMessage `json:"view,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type checkoutCWDCounts struct {
	Class        string       `json:"class"`
	ReadLatency  Distribution `json:"read_latency_ms"`
	ReadCensored int          `json:"read_censored"`
	Calls        int          `json:"calls"`
	Refusals     int          `json:"refusals"`
	Errors       int          `json:"errors"`
	Latency      Distribution `json:"latency_ms"`
}

type checkoutDiscoveryReport struct {
	ForeignCapacityRefusals int                            `json:"foreign_capacity_refusals"`
	PendingRefusals         int                            `json:"pending_refusals"`
	Checkouts               []checkoutDiscoveryObservation `json:"checkouts"`
	ByCWD                   map[string]checkoutCWDCounts   `json:"by_cwd"`
	OutsidePendingRefusals  int                            `json:"outside_pending_refusals"`
	OutsidePendingErrors    int                            `json:"outside_pending_errors"`
}

// A refusal belongs to a pending checkout only while its discovery is
// unresolved. Component boundaries keep a sibling path out of that scope.
func pendingCheckoutForAttempt(a Attempt, checkouts []checkoutDiscoveryObservation) bool {
	for _, checkout := range checkouts {
		if inside(checkout.Root, a.CWD) && !a.Started.Before(checkout.Added) &&
			(checkout.ExactAt.IsZero() || a.Started.Before(checkout.ExactAt)) {
			return true
		}
	}
	return false
}

func summarizeCheckoutDiscovery(attempts []Attempt, checkouts []checkoutDiscoveryObservation, primary, other string) checkoutDiscoveryReport {
	out := checkoutDiscoveryReport{Checkouts: checkouts, ByCWD: make(map[string]checkoutCWDCounts)}
	values := make(map[string][]float64)
	reads := make(map[string][]float64)
	for _, a := range attempts {
		row := out.ByCWD[a.CWD]
		row.Class = "outside_known_checkouts"
		if inside(primary, a.CWD) {
			row.Class = "primary"
		}
		if inside(other, a.CWD) {
			row.Class = "other_repository"
		}
		for _, checkout := range checkouts {
			if inside(checkout.Root, a.CWD) {
				row.Class = "new_worktree"
				if checkout.FamilyRole == "foreign" {
					row.Class = "foreign_new_worktree"
					if a.Code != 0 && (a.ErrorData["reason"] == "family_capacity" || a.ErrorData["reason"] == "stuck_capacity") {
						out.ForeignCapacityRefusals++
					}
				}
			}
		}
		if a.Role == "workload" && !strings.HasPrefix(a.Tool, "edit.") {
			if a.Outcome == "phase_censored" {
				row.ReadCensored++
			} else {
				reads[a.CWD] = append(reads[a.CWD], a.DurationMS)
			}
		}
		row.Calls++
		values[a.CWD] = append(values[a.CWD], a.DurationMS)
		refused := a.Code != 0
		failed := a.Outcome != "success" && a.Outcome != "phase_censored"
		if refused {
			row.Refusals++
		}
		if failed {
			row.Errors++
		}
		if pendingCheckoutForAttempt(a, checkouts) && refused {
			out.PendingRefusals++
		}
		if !pendingCheckoutForAttempt(a, checkouts) {
			if refused {
				out.OutsidePendingRefusals++
			}
			if failed {
				out.OutsidePendingErrors++
			}
		}
		out.ByCWD[a.CWD] = row
	}
	for cwd, row := range out.ByCWD {
		row.Latency = distribution(values[cwd], "ms")
		row.ReadLatency = distribution(reads[cwd], "ms")
		out.ByCWD[cwd] = row
	}
	return out
}

// The existing readers keep their original sessions and repository assignment
// throughout the additions. Discovery probes own separate root/subdirectory
// sessions; every refusal remains an attempt instead of being retried away.
func measureCheckoutDiscovery(parent context.Context, o runOptions, p *privateRun, clients []*socketClient, phase *Phase) (resultErr error) {
	phase.SessionModel = "per_agent_plus_new_worktree_root_and_subdirectory_probes"
	ctx, cancel := context.WithTimeout(parent, o.Duration)
	defer cancel()
	frozen := *p
	frozen.Repos = append([]privateRepo(nil), p.Repos...)
	load := Phase{}
	done := make(chan error, 1)
	go func() { done <- runAgents(ctx, o, &frozen, clients, o.Editors > 0, &load) }()
	first := len(phase.Attempts)
	var checkouts []checkoutDiscoveryObservation
	var probes []*socketClient
	defer func() {
		cancel()
		loadErr := <-done
		closeClients(probes)
		phase.Attempts = append(phase.Attempts, load.Attempts...)
		phase.Edits = append(phase.Edits, load.Edits...)
		finishEditMetrics(phase)
		if len(checkouts) != o.Worktrees {
			resultErr = errors.Join(resultErr, errors.New("offered window ended before all planned worktree additions"))
		}
		report := summarizeCheckoutDiscovery(phase.Attempts[first:], checkouts, frozen.Repos[0].Path, frozen.Repos[1].Path)
		phase.Gate["checkout_discovery"], _ = json.Marshal(report)
		if o.Discovery.StallFirst && report.PendingRefusals == 0 {
			resultErr = errors.Join(resultErr, errors.New("Git stall produced no pending refusal exposure"))
		}
		if report.OutsidePendingRefusals > 0 || report.OutsidePendingErrors > 0 || report.ForeignCapacityRefusals > 0 {
			resultErr = errors.Join(resultErr, fmt.Errorf("checkout discovery affected other cwds: %d refusals, %d errors, %d foreign capacity refusals", report.OutsidePendingRefusals, report.OutsidePendingErrors, report.ForeignCapacityRefusals))
		}
		if parent.Err() != nil {
			resultErr = errors.Join(resultErr, parent.Err(), loadErr)
		}
	}()
	// Additions are spread over the offered window, including a baseline with
	// --worktrees=0. Timing is recorded as observed, never inferred from count.
	interval := o.Duration / time.Duration(o.Worktrees+1)
	nextAdd := time.Now().Add(interval)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return resultErr
		case <-ticker.C:
		}
		if len(checkouts) < o.Worktrees && !time.Now().Before(nextAdd) {
			family := len(checkouts) % 2
			if err := p.addDiscoveryWorktree(ctx, family); err != nil {
				return err
			}
			repo := p.Repos[len(p.Repos)-1]
			role := "primary"
			if family == 1 {
				role = "foreign"
			}
			entry := checkoutDiscoveryObservation{Root: repo.Path, FamilyRole: role, Prefix: repo.Prefix, Query: repo.Corpus.Query, Added: time.Now(), ExactViewLag: unavailable("ms", "no exact worktree view observed before phase end")}
			checkouts = append(checkouts, entry)
			nextAdd = nextAdd.Add(interval)
			subdir := filepath.Join(repo.Path, ".discovery-probe")
			if err := os.Mkdir(subdir, 0o700); err != nil {
				return err
			}
			for _, cwd := range []string{repo.Path, subdir} {
				client, err := openMCP(ctx, p.Socket, cwd)
				if err != nil {
					return fmt.Errorf("open discovery session in %s: %w", cwd, err)
				}
				probes = append(probes, client)
			}
		}
		for i, client := range probes {
			entry := &checkouts[i/2]
			if entry.FirstRequest.IsZero() {
				entry.FirstRequest = time.Now()
			}
			r := request{Tool: "search", Operation: "symbols", Arguments: map[string]any{"operation": "symbols", "query": entry.Query}}
			_, rows, _ := callTool(ctx, client, r, r.Arguments, "probe", i, len(checkouts), 0, o.Deadline, waitContext)
			phase.Attempts = append(phase.Attempts, rows...)
			if i%2 != 0 || !entry.ExactAt.IsZero() || ctx.Err() != nil {
				continue
			}
			raw, err := control(ctx, p.Socket, "search_symbols", map[string]any{"path": entry.Root, "query": entry.Query, "limit": 1})
			entry.View = append(json.RawMessage(nil), raw...)
			if err != nil {
				entry.Error = err.Error()
				continue
			}
			var answer struct {
				View struct {
					Exact  bool   `json:"exact"`
					Kind   string `json:"kind"`
					Prefix string `json:"repo_prefix"`
				} `json:"view"`
			}
			if err := json.Unmarshal(raw, &answer); err != nil {
				entry.Error = err.Error()
				continue
			}
			if answer.View.Exact && answer.View.Kind == "worktree" && answer.View.Prefix == entry.Prefix {
				entry.ExactAt = time.Now()
				entry.ExactViewLag = measured(float64(entry.ExactAt.Sub(entry.FirstRequest))/float64(time.Millisecond), "ms")
				entry.Error = ""
			}
		}
	}
}
