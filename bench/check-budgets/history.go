package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

type nightlyEvidence struct {
	receipt laneReceipt
	budgets budgetEvidence
	day     string
}

// The index contains artifact directories downloaded from reviewed GitHub runs.
// This verifies their contents, not the authenticity of arbitrary local files.
func loadNightlyHistory(index string, p policy) ([]nightlyEvidence, error) {
	data, err := os.ReadFile(index)
	if err != nil {
		return nil, err
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, err
	}
	var evidence []nightlyEvidence
	seen := map[string]bool{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(index), path)
		}
		var item nightlyEvidence
		data, err := os.ReadFile(filepath.Join(path, "lane.json"))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &item.receipt); err != nil {
			return nil, err
		}
		r := item.receipt
		if r.Version != 1 || r.Plan.Lane != "nightly" || r.Plan.PolicySHA256 != policyDigest(p) || r.Started.IsZero() || r.Finished.Before(r.Started) || !revisionPattern.MatchString(r.Workflow.Revision) || r.Workflow.Ref != "refs/heads/main" || r.Workflow.Attempt < 1 || !slices.Contains([]string{"schedule", "workflow_dispatch"}, r.Workflow.Event) || !strings.HasPrefix(r.Workflow.RunURL, "https://github.com/") || !strings.Contains(r.Workflow.RunURL, "/actions/runs/") {
			return nil, errors.New("nightly history requires complete scheduled or manual main workflow provenance; PR and preflight evidence cannot qualify")
		}
		item.day = r.Started.UTC().Format(time.DateOnly)
		key := fmt.Sprintf("%s/%d/%s", r.Workflow.RunURL, r.Workflow.Attempt, r.Plan.HostClass)
		if seen[key] {
			return nil, errors.New("duplicate nightly workflow-run attempt/host artifact")
		}
		seen[key] = true
		data, err = os.ReadFile(filepath.Join(path, "budget-results.json"))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &item.budgets); err != nil {
			return nil, err
		}
		if item.budgets.Lane != "nightly" || len(item.budgets.Rows) == 0 {
			return nil, errors.New("nightly budget table missing")
		}
		keys := map[string]bool{}
		expected := map[string]budget{}
		for _, row := range p.Budgets {
			if row.HostClass == r.Plan.HostClass && slices.Contains(row.Lane, "nightly") {
				expected[row.HostClass+" / "+row.Scenario+" / "+row.Metric] = row
			}
		}
		if len(expected) == 0 || len(expected) != len(item.budgets.Rows) {
			return nil, errors.New("nightly budget table does not contain the complete pinned policy")
		}
		allPass := true
		for _, row := range item.budgets.Rows {
			key := rowKey(row)
			target, exists := expected[key]
			if keys[key] || !exists || row.Mode != target.Mode || row.Unit != target.Unit {
				return nil, errors.New("invalid or duplicate nightly budget row")
			}
			keys[key] = true
			allPass = allPass && row.Pass
		}
		if item.budgets.Pass != allPass || r.BudgetPass != allPass {
			return nil, errors.New("nightly pass flags disagree with budget table")
		}
		if allPass {
			// A table alone cannot prove complete evidence. Re-evaluate the retained
			// raw reports through the same strict checker before counting a qualifying main run.
			dataset, err := loadInputs(filepath.Join(path, "inputs.json"))
			if err != nil {
				return nil, err
			}
			if dataset.config.CandidateRevision != r.Workflow.Revision {
				return nil, errors.New("nightly workflow and raw evidence revisions differ")
			}
			rows, err := check(p, dataset, "nightly")
			if err != nil {
				return nil, err
			}
			if len(rows) != len(item.budgets.Rows) {
				return nil, errors.New("nightly replay selected a different matrix")
			}
			stored := make(map[string]result, len(item.budgets.Rows))
			for _, row := range item.budgets.Rows {
				stored[rowKey(row)] = row
			}
			for _, row := range rows {
				if !row.Pass || !reflect.DeepEqual(row, stored[rowKey(row)]) {
					return nil, errors.New("nightly raw evidence does not reproduce the exact stored values, limits and verdicts")
				}
			}
		}
		evidence = append(evidence, item)
	}
	slices.SortFunc(evidence, func(a, b nightlyEvidence) int {
		return strings.Compare(a.day+"/"+a.receipt.Plan.HostClass, b.day+"/"+b.receipt.Plan.HostClass)
	})
	return evidence, nil
}

func rowKey(row result) string { return row.HostClass + " / " + row.Scenario + " / " + row.Metric }

func historyVerdict(evidence []nightlyEvidence, through time.Time, hosts []string, revision string, out io.Writer) bool {
	// Required hosts must come from one actual workflow run/attempt at the
	// reviewed main revision, not a calendar assembled from unrelated runs.
	cohorts := map[string]map[string]nightlyEvidence{}
	cutoff := through.Format(time.DateOnly)
	for _, item := range evidence {
		w := item.receipt.Workflow
		if item.day > cutoff || w.Revision != revision {
			continue
		}
		key := fmt.Sprintf("%s/%d/%s", w.RunURL, w.Attempt, w.Event)
		if cohorts[key] == nil {
			cohorts[key] = map[string]nightlyEvidence{}
		}
		cohorts[key][item.receipt.Plan.HostClass] = item
	}
	keys := make([]string, 0, len(cohorts))
	for key := range cohorts {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	pass := false
	for _, key := range keys {
		complete := true
		for _, host := range hosts {
			item, ok := cohorts[key][host]
			good := ok && item.receipt.Plan.NightlySeed == nil && item.receipt.BudgetPass && item.receipt.Error == "" && item.receipt.Plan.Ready && len(item.receipt.Plan.Prerequisites) == 0
			fmt.Fprintf(out, "%s / %s: complete main nightly-equivalent = %t\n", host, key, good)
			complete = complete && good
		}
		pass = pass || complete
	}
	// Keep nightly trend monitoring independent of the one-run release proof.
	for _, host := range hosts {
		byDay := map[string]nightlyEvidence{}
		for _, item := range evidence {
			if item.receipt.Plan.HostClass == host && item.day <= cutoff && item.receipt.Workflow.Event == "schedule" {
				// Preserve every scheduled miss. A manual pass or same-day retry
				// cannot erase a failed nightly row; this is not metric pooling.
				day := byDay[item.day]
				day.day = item.day
				day.budgets.Rows = append(day.budgets.Rows, item.budgets.Rows...)
				byDay[item.day] = day
			}
		}
		// Check adjacent real dates, not just adjacent available artifacts.
		for day, current := range byDay {
			date, _ := time.Parse(time.DateOnly, day)
			previous, ok := byDay[date.AddDate(0, 0, -1).Format(time.DateOnly)]
			if !ok {
				continue
			}
			failed := map[string]result{}
			for _, row := range previous.budgets.Rows {
				if !row.Pass {
					failed[rowKey(row)] = row
				}
			}
			for _, row := range current.budgets.Rows {
				prior, exists := failed[rowKey(row)]
				if row.Pass || !exists || prior.Mode != row.Mode {
					continue
				}
				kind := "BLOCKER"
				if row.Mode == "guard" && slices.Contains([]string{"disk", "memory"}, metricSpecs[row.Metric].axis) {
					kind = "GUARD REVIEW"
				}
				fmt.Fprintf(out, "%s: %s missed %s and %s\n", kind, rowKey(row), previous.day, current.day)
			}
		}
	}
	return pass
}

func historyCommand(args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("history", flag.ContinueOnError)
	f.SetOutput(stderr)
	index := f.String("inputs", "", "JSON array of downloaded nightly artifact directories")
	through := f.String("through", "", "review cutoff UTC date (YYYY-MM-DD), not a waiting period")
	revision := f.String("revision", "", "reviewed main revision required for the complete run")
	hosts := f.String("hosts", "reference_linux", "comma-separated reviewed required host classes")
	budgets := f.String("budgets", "bench/budgets.yaml", "pinned budget policy for history coverage/replay")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	date, err := time.Parse(time.DateOnly, *through)
	classes := strings.Split(*hosts, ",")
	if *index == "" || err != nil || !revisionPattern.MatchString(*revision) || f.NArg() != 0 {
		fmt.Fprintln(stderr, "history requires --inputs, reviewed --revision and explicit --through review cutoff")
		return 2
	}
	for _, host := range classes {
		if !slices.Contains([]string{"reference_linux", "reference_mac", "large_mac"}, host) {
			fmt.Fprintln(stderr, "unknown host class")
			return 2
		}
	}
	data, err := os.ReadFile(*budgets)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	p, err := loadPolicy(data)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	evidence, err := loadNightlyHistory(*index, p)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !historyVerdict(evidence, date, classes, *revision, stdout) {
		fmt.Fprintln(stderr, "one complete qualifying main nightly-equivalent run is not available for every required host")
		return 1
	}
	return 0
}
