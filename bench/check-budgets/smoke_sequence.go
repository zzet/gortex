package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type smokeSequenceReceipt struct {
	EditACKMode   string         `json:"edit_ack_mode,omitempty"`
	Version       int            `json:"schema_version"`
	Started       time.Time      `json:"started"`
	Finished      time.Time      `json:"finished"`
	Tasks         []taskReceipt  `json:"tasks"`
	FreshStores   int            `json:"fresh_store_count"`
	ProcessStarts map[string]int `json:"process_starts"`
	Complete      bool           `json:"complete"`
	Error         string         `json:"error,omitempty"`
}

func collectSmokeSequence(ctx context.Context, c laneConfig, p policy, plan lanePlan, out string, stdout io.Writer, process laneProcess) error {
	warm, err := smokeWarmBudget(ctx)
	if err != nil {
		return err
	}
	c.WarmTimeout = warm.String()
	if err := validateLaneOutput(c.Manifest, out); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	receipt := laneReceipt{Version: 1, Plan: plan, Started: c.started, Tasks: []taskReceipt{}, Workflow: currentBuildWorkflow(c.Builds["candidate"].Revision)}
	proof, err := prepareSmokeProvenance(c, receipt.Workflow)
	if err != nil {
		receipt.Error = err.Error()
		receipt.Finished = time.Now().UTC()
		return errors.Join(err, writeNewJSON(filepath.Join(out, "lane.json"), receipt))
	}
	proof.EditACKMode = "local"
	receipt.Smoke = &proof
	root := filepath.Join(out, "runs")
	log, err := os.OpenFile(filepath.Join(out, "client.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	started := time.Now().UTC()
	args := smokeSequenceArgs(c, root)
	runErr := errors.Join(process(ctx, c.Load, args, io.MultiWriter(stdout, log)), log.Close())
	finished := time.Now().UTC()
	var failures []error
	if runErr != nil {
		failures = append(failures, runErr)
	}
	var sequence smokeSequenceReceipt
	err = readStrictJSON(filepath.Join(root, "sequence.json"), &sequence)
	if err == nil && sequence.EditACKMode != "local" {
		err = errors.New("smoke child did not retain its explicitly selected local ACK policy")
	}
	if err == nil {
		err = validateSmokeSequence(sequence, plan, started, finished)
	}
	if err != nil {
		failures = append(failures, err)
	}
	index := inputs{Version: 1, CandidateRevision: c.Builds["candidate"].Revision, MainRevision: c.Builds["main"].Revision, TagRevision: shippedRevision, Repeats: 3, ScenarioRepeats: map[string]int{}, Hosts: map[string]string{hostedSmokeClass: c.Machine}, Runs: []runInput{}}
	for _, task := range sequence.Tasks {
		// Even a failed sequence retains every observed task/report. The exact
		// census/timestamps above and the normal checker must both pass.
		path := filepath.Join(fmt.Sprintf("%s-%03d-%s", task.Scenario, task.Repeat, task.Role), "report.json")
		if !slices.Contains(smokeSteps, task.Scenario) || !slices.Contains([]string{"main", "candidate"}, task.Role) || task.Repeat < 1 || task.Repeat > 3 || task.Report != path {
			failures = append(failures, errors.New("refused unsafe/unplanned smoke report path; original sequence receipt retained"))
			continue
		}
		task.Report = filepath.Join("runs", path)
		receipt.Tasks = append(receipt.Tasks, task)
		index.Runs = append(index.Runs, runInput{Role: task.Role, HostClass: hostedSmokeClass, Scenario: task.Scenario, Repeat: task.Repeat, Report: task.Report})
	}
	return finishLaneEvidence(c, p, plan, out, stdout, receipt, index, failures)
}

// The PR entry owns the aggregate deadline. Each strict readiness gate may use
// its remaining budget; it must not introduce an unrelated cold-start cutoff.
func smokeWarmBudget(ctx context.Context) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, errors.New("smoke readiness requires its parent measurement deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, context.DeadlineExceeded
	}
	return min(remaining, smokeMeasurementWindow), nil
}

func validateSmokeSequence(s smokeSequenceReceipt, plan lanePlan, started, finished time.Time) error {
	if s.Version != 1 || !s.Complete || s.Error != "" || s.FreshStores != 2 || len(s.ProcessStarts) != 2 || s.ProcessStarts["main"] != 18 || s.ProcessStarts["candidate"] != 18 || len(s.Tasks) != len(plan.Tasks) || s.Started.Before(started) || s.Finished.After(finished) || !s.Finished.After(s.Started) {
		return errors.New("smoke sequence lacks complete two-store/36-start lifecycle evidence; failed attempts remain unaccepted")
	}
	previous := s.Started
	for i, task := range s.Tasks {
		want := plan.Tasks[i]
		path := filepath.Join(fmt.Sprintf("%s-%03d-%s", task.Scenario, task.Repeat, task.Role), "report.json")
		if task.Scenario != want.Scenario || task.Role != want.Role || task.Repeat != want.Repeat || task.Seed != want.Seed || task.Error != "" || task.Report != path || task.Started.Before(previous) || task.Finished.After(s.Finished) || !task.Finished.After(task.Started) {
			return errors.New("smoke sequence has missing/duplicated/forged task identity, path or execution interval")
		}
		previous = task.Finished
	}
	return nil
}

func validateSmokeStores(d dataset) error {
	roots := map[string]string{}
	for _, run := range d.runs {
		var store struct {
			Root    string `json:"owned_root"`
			Binary  string `json:"binary"`
			Starts  int    `json:"starts_before_phase"`
			Present bool   `json:"store_present"`
		}
		step := -1
		for i, name := range smokeSteps {
			if run.input.Scenario == name {
				step = i
			}
		}
		if step < 0 {
			return errors.New("retained smoke evidence contains an unknown step")
		}
		starts := (run.input.Repeat-1)*6 + step
		if step == 4 {
			starts++ // the restart step has two actual process starts
		}
		gate := run.data.Phases[0].Gate
		if json.Unmarshal(gate["pr_smoke_store"], &store) != nil || store.Root == "" || store.Binary == "" || store.Binary != run.data.Binary || store.Starts != starts || store.Present != (starts > 0) {
			return errors.New("smoke report lacks exact fresh/retained owned-store and process-start proof")
		}
		if root := roots[run.input.Role]; root != "" && root != store.Root {
			return errors.New("smoke changed owned stores between repetitions")
		}
		roots[run.input.Role] = store.Root
		if starts >= 2 {
			var restoration struct {
				Markers int  `json:"restored_markers"`
				Ready   bool `json:"ready_and_enriched"`
			}
			if json.Unmarshal(gate["pr_smoke_restoration"], &restoration) != nil || restoration.Markers < 1 || !restoration.Ready {
				return errors.New("smoke reused a store without a successful edit-restoration/publication fence")
			}
		}
	}
	if len(roots) != 2 || roots["main"] == "" || roots["candidate"] == "" || roots["main"] == roots["candidate"] {
		return errors.New("main and candidate require distinct owned stores")
	}
	return nil
}

// Keep the actual child command testable without launching an owned daemon.
func smokeSequenceArgs(c laneConfig, root string) []string {
	return []string{"smoke", "--main-binary", c.Builds["main"].Binary, "--main-revision", c.Builds["main"].Revision, "--candidate-binary", c.Builds["candidate"].Binary, "--candidate-revision", c.Builds["candidate"].Revision, "--manifest", c.Manifest, "--out", root, "--offer", c.Duration, "--deadline", c.Deadline, "--warm-timeout", c.WarmTimeout, "--stop-timeout", c.StopTimeout, "--sweep-duration", c.SweepDuration, "--edit-ack", "local"}
}
