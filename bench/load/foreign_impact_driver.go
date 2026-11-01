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

type foreignImpactRunReport struct {
	EditedRepo     string               `json:"edited_repo"`
	TargetRepos    []string             `json:"target_repos"`
	PlanHash       string               `json:"edit_plan_sha256"`
	EditOffers     int                  `json:"edit_offers"`
	EditMeanNS     int64                `json:"edit_mean_ns"`
	FirstDueNS     int64                `json:"first_edit_due_ns"`
	LastDueNS      int64                `json:"last_edit_due_ns"`
	FirstEditWrite time.Time            `json:"first_edit_write"`
	Window         foreignImpactWindow  `json:"window"`
	ProbePeriodNS  int64                `json:"probe_period_ns"`
	ProbeStarted   time.Time            `json:"probe_started"`
	ProbeFinished  time.Time            `json:"probe_finished"`
	Summary        foreignImpactSummary `json:"summary"`
	RawFile        string               `json:"raw_file"`
	Complete       bool                 `json:"complete"`
}

// The foreign cell keeps the accepted one-editor Poisson schedule. Unlike the
// six-editor 100-global-offer cell, this stream must span the full ten minutes.
func foreignImpactEditPlan(c Corpus, seed int64) (editProbePlan, error) {
	plan, err := planEditProbe(c, runOptions{Seed: seed, EditCount: 100, Editors: 1})
	if err != nil {
		return plan, err
	}
	if len(plan.Steps) != 100 || plan.MeanNS != int64(editProbeMean) || plan.Steps[len(plan.Steps)-1].DueNS-plan.Steps[0].DueNS < int64(foreignImpactDuration) {
		return plan, errors.New("fixed foreign edit plan does not cover ten active minutes; never fill the remainder with idle queries")
	}
	return plan, nil
}

func foreignImpactQueryTargets(repos []privateRepo, clients []*socketClient, edited int) ([]foreignImpactTarget, error) {
	if len(repos) != 6 || edited < 0 || edited >= len(repos) {
		return nil, errors.New("foreign impact requires the six pinned repositories and one selected editor")
	}
	var targets []foreignImpactTarget
	for i, repo := range repos {
		if i == edited {
			continue
		}
		var owner *socketClient
		for _, client := range clients {
			if client != nil && filepath.Clean(client.cwd) == filepath.Clean(repo.Path) {
				if owner != nil {
					return nil, errors.New("ambiguous target query session")
				}
				owner = client
			}
		}
		if owner == nil {
			return nil, fmt.Errorf("no own query session for %s", repo.Corpus.Label)
		}
		targets = append(targets, foreignImpactTarget{Client: owner, Repo: repo})
	}
	return targets, nil
}

// runForeignImpactWithEdits is called from the existing owned runPhase driver
// hook after all six roots pass readiness. It creates no process, store, source
// clone, or new traffic scheduler. The normal lifecycle still owns shutdown.
func runForeignImpactWithEdits(ctx context.Context, o runOptions, p *privateRun, clients []*socketClient, phase *Phase, editedOrdinal int) (report foreignImpactRunReport, resultErr error) {
	targets, err := foreignImpactQueryTargets(p.Repos, clients, editedOrdinal)
	if err != nil {
		return report, err
	}
	edited := p.Repos[editedOrdinal]
	plan, err := foreignImpactEditPlan(edited.Corpus, o.Seed)
	if err != nil {
		return report, err
	}
	if phase.editProbeRuntime != nil {
		return report, errors.New("foreign driver cannot replace an admitted edit plan")
	}
	report.EditedRepo = edited.Corpus.Label
	report.EditOffers = len(plan.Steps)
	report.EditMeanNS = plan.MeanNS
	report.FirstDueNS = plan.Steps[0].DueNS
	report.LastDueNS = plan.Steps[len(plan.Steps)-1].DueNS
	report.ProbePeriodNS = int64(editProbePeriod)
	report.PlanHash, _ = hashJSON(plan)
	for _, target := range targets {
		report.TargetRepos = append(report.TargetRepos, target.Repo.Corpus.Label)
	}
	editor, err := openMCP(ctx, p.Socket, edited.Path)
	if err != nil {
		return report, err
	}
	defer editor.Close()
	// All roots stay registered in the daemon. Only B owns offered edits.
	one := &privateRun{Root: p.Root, SocketDir: p.SocketDir, Socket: p.Socket, Store: p.Store, Config: p.Config, Env: p.Env, Repos: []privateRepo{edited}, Binary: p.Binary}
	options := o
	options.Agents = 0
	options.Editors = 1
	options.EditRepos = 1
	options.Worktrees = 0
	options.EditCount = 100
	options.pairedDriver = nil
	runtime, probers, err := prepareEditProbe(ctx, options, one, nil, []*socketClient{editor}, phase)
	defer func() {
		for _, client := range probers {
			_ = client.Close()
		}
		if runtime != nil {
			resultErr = errors.Join(resultErr, runtime.close())
		}
		phase.editProbeRuntime = nil
	}()
	if err != nil {
		return report, err
	}
	phase.SessionModel = "one_B_editor_and_five_independent_rotating_foreign_queries"
	phase.Metrics["mcp_sessions"] = measured(float64(len(clients)+len(probers)+1), "sessions")
	rawPath := filepath.Join(p.Root, "foreign-impact.jsonl")
	raw, err := os.OpenFile(rawPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, raw.Close()) }()
	report.RawFile = rawPath
	budget, err := plan.budget(o.Deadline, 1)
	if err != nil {
		return report, err
	}
	measuredCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	captureReceipt(ctx, one, editor, phase)
	phase.EditProbe.StatsBefore, err = runtime.stats(ctx)
	if err != nil {
		return report, err
	}
	phase.Started = time.Now()
	runtime.origin = phase.Started
	done := make(chan error, 1)
	go func() {
		scheduleErr := runEditProbeArrivals(measuredCtx, runtime.plan, runtime.origin, runtime.activate, runtime.doEdit, waitContext)
		done <- errors.Join(scheduleErr, runtime.finish())
	}()
	// The first actual edit write is already retained by the existing RPC
	// observer. Wait for that evidence, rather than assuming its planned due time.
	var editErr error
	editsJoined := false
	ticker := time.NewTicker(editProbePeriod)
awaitWrite:
	for report.FirstEditWrite.IsZero() && measuredCtx.Err() == nil {
		first := runtime.states[0]
		first.mu.Lock()
		report.FirstEditWrite = first.editWrite
		first.mu.Unlock()
		if !report.FirstEditWrite.IsZero() {
			break
		}
		select {
		case <-ticker.C:
		case <-measuredCtx.Done():
		case editErr = <-done:
			editsJoined = true
			break awaitWrite
		}
	}
	ticker.Stop()
	if report.FirstEditWrite.IsZero() {
		cancel()
		resultErr = errors.New("no actual B edit write was observed")
	} else {
		report.Window = foreignImpactWindow{ID: "foreign-" + edited.Corpus.Label, EditedRepo: edited.Corpus.Label, Start: time.Now()}
		report.Window.End = report.Window.Start.Add(foreignImpactDuration)
		if runtime.origin.Add(time.Duration(report.LastDueNS)).Before(report.Window.End) {
			cancel()
			resultErr = errors.New("actual edit start was too late to cover the full foreign window")
		} else {
			report.ProbeStarted = time.Now()
			report.Summary, err = runForeignImpactStream(measuredCtx, targets, edited, report.Window, func(row foreignImpactSample) error { return json.NewEncoder(raw).Encode(row) })
			report.ProbeFinished = time.Now()
			resultErr = errors.Join(resultErr, err)
			if err != nil {
				cancel()
			}
		}
	}
	if !editsJoined {
		editErr = <-done
	}
	resultErr = errors.Join(resultErr, editErr)
	phase.EditProbe.StatsAfter, err = runtime.stats(ctx)
	resultErr = errors.Join(resultErr, err)
	if ctx.Err() == nil {
		restoreCtx, stop := context.WithTimeout(ctx, o.Deadline)
		resultErr = errors.Join(resultErr, runtime.restore(restoreCtx))
		stop()
	} else {
		resultErr = errors.Join(resultErr, errors.New("foreign-cell original restore unavailable after parent cancellation"))
	}
	report.Complete = resultErr == nil && !report.ProbeFinished.Before(report.Window.End) && report.Summary.Offered > 0
	phase.Metrics["foreign_impact_p95_ms"] = report.Summary.Latency.P95
	phase.Metrics["foreign_impact_refusals"] = measured(float64(report.Summary.Refusals), "calls")
	phase.Metrics["foreign_impact_errors"] = measured(float64(report.Summary.Errors), "calls")
	return report, resultErr
}
