package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func parseForeignImpact(args []string) (runOptions, int, error) {
	var o runOptions
	f := flag.NewFlagSet("load foreign-impact", flag.ContinueOnError)
	bindRunFlags(f, &o)
	edited := f.Int("edited-repo", 0, "zero-based edited repository in the pinned six-root manifest")
	if err := f.Parse(args); err != nil {
		return o, *edited, err
	}
	if f.NArg() != 0 {
		return o, *edited, errors.New("unexpected foreign-impact positional arguments")
	}
	if err := validateRunOptions(o); err != nil {
		return o, *edited, err
	}
	if o.Scenario != "many" || o.Agents != 6 || o.Editors != 1 || o.Worktrees != 0 || o.EditRepos != 1 || o.EditCount != 100 || o.Seed != 42 || o.Profile != "agent" || o.CPUProfile != 0 || o.PRSmokeDrain != 0 || o.Duration != foreignImpactDuration || o.Deadline != 120*time.Second || o.WarmTimeout != 30*time.Minute || o.StopTimeout != 30*time.Second || *edited < 0 || *edited >= 6 {
		return o, *edited, errors.New("foreign-impact requires many/6 sessions/1 editor/0 worktrees, seed42,100 edits, agent profile, duration10m, deadline120s, warm30m, stop30s and edited-repo0..5")
	}
	return o, *edited, nil
}

// The separate entry retains the ordinary run lifecycle and its host checks.
// It never lengthens or replaces the main six-editor 100-offer experiment.
func foreignImpactCommand(ctx context.Context, args []string) error {
	o, edited, err := parseForeignImpact(args)
	if err != nil {
		return err
	}
	corpora, err := loadManifest(o.Manifest)
	if err != nil {
		return err
	}
	if len(corpora) != 6 {
		return errors.New("foreign-impact requires six distinct pinned repositories")
	}
	if err := validateOutputRoots(corpora, o.Scratch, o.Output); err != nil {
		return err
	}
	sha, err := binaryRevision(o.Binary, o.GitSHA)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.Scratch, 0o700); err != nil {
		return err
	}
	host, group, err := inspectHost(o.Host, o.Scratch)
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp(o.Scratch, "foreign-impact-report-")
	if err != nil {
		return err
	}
	if o.Output == "" {
		o.Output = filepath.Join(root, "run.json")
	}
	if err := validateEditProbeSourceRepos(ctx, corpora, root); err != nil {
		return err
	}
	report, err := newRunReport(o, corpora, sha, host, root)
	if err != nil {
		return err
	}
	foreignPath := filepath.Join(root, "foreign-impact-summary.json")
	o.pairedDriver = func(ctx context.Context, p *privateRun, clients, _ []*socketClient, phase *Phase) error {
		cell, runErr := runForeignImpactWithEdits(ctx, o, p, clients, phase, edited)
		data, encodeErr := json.MarshalIndent(cell, "", "  ")
		if encodeErr == nil {
			encodeErr = os.WriteFile(foreignPath, append(data, '\n'), 0o600)
		}
		return errors.Join(runErr, encodeErr)
	}
	phase, sweep, artifacts, crashes, version, runErr := runPhase(ctx, o, "many", corpora, group, root)
	artifacts["foreign_impact_summary"] = foreignPath
	addPhase(&report, phase, sweep, artifacts, crashes, version, runErr)
	if err := finishReport(&report, 1); err != nil {
		return err
	}
	if err := writeReport(o.Output, report); err != nil {
		return err
	}
	if !report.Complete {
		return fmt.Errorf("incomplete foreign-impact run; evidence retained in %s", o.Output)
	}
	return nil
}
