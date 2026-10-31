package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
)

type buildSpec struct{ label, binary, sha string }
type buildFlags []string

func (b *buildFlags) String() string         { return strings.Join(*b, ",") }
func (b *buildFlags) Set(value string) error { *b = append(*b, value); return nil }

func parsePaired(args []string) (runOptions, []buildSpec, error) {
	var o runOptions
	var raw buildFlags
	f := flag.NewFlagSet("load paired", flag.ContinueOnError)
	bindRunFlags(f, &o)
	f.Var(&raw, "build", "labeled native binary, e.g. tag=/private/gortex (repeat 2–3 times; first is ratio baseline)")
	if err := f.Parse(args); err != nil {
		return o, nil, err
	}
	if f.NArg() != 0 || o.Binary != "" || o.GitSHA != "" {
		return o, nil, errors.New("paired uses --build label=binary with embedded VCS revision; no positional --binary/--git-sha overrides")
	}
	var builds []buildSpec
	var labels []string
	for _, value := range raw {
		label, binary, ok := strings.Cut(value, "=")
		if !ok || binary == "" {
			return o, nil, fmt.Errorf("invalid --build %q", value)
		}
		builds = append(builds, buildSpec{label: label, binary: binary})
		labels = append(labels, label)
	}
	if err := validateBuildLabels(labels); err != nil {
		return o, nil, err
	}
	o.Binary = builds[0].binary
	if err := validateRunOptions(o); err != nil {
		return o, nil, err
	}
	if o.Host != "native" || o.Profile != "agent" || o.CPUProfile != 0 {
		return o, nil, errors.New("paired is coupled native agent-profile latency only; reference/floor/saturation/CPU profiles must run standalone")
	}
	switch o.Scenario {
	case "read", "edit", "many", "many-edit", "worktrees", "sweep":
	default:
		return o, nil, errors.New("paired supports workload scenarios; cold/restart/all use standalone runs")
	}
	return o, builds, nil
}

func pairedCommand(ctx context.Context, args []string) error {
	o, builds, err := parsePaired(args)
	if err != nil {
		return err
	}
	corpora, err := loadManifest(o.Manifest)
	if err != nil {
		return err
	}
	if err := validateOutputRoots(corpora, o.Scratch, o.Output); err != nil {
		return err
	}
	labels := make([]string, len(builds))
	for i := range builds {
		builds[i].sha, err = binaryRevision(builds[i].binary, "")
		if err != nil {
			return fmt.Errorf("build %s: %w", builds[i].label, err)
		}
		labels[i] = builds[i].label
	}
	if err := os.MkdirAll(o.Scratch, 0o700); err != nil {
		return err
	}
	host, group, err := inspectHost("native", o.Scratch)
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp(o.Scratch, "load-paired-")
	if err != nil {
		return err
	}
	if o.Output == "" {
		o.Output = filepath.Join(root, "paired.json")
	}
	coordinator, err := newPairedCoordinator(ctx, o, labels, group)
	if err != nil {
		return err
	}
	defer coordinator.cancel()
	report := pairedReport{SchemaVersion: 1, Kind: "coupled_native_latency_not_independent_capacity", RunID: filepath.Base(root), BuildOrder: labels, Builds: make(map[string]Report)}
	var mu sync.Mutex
	var workers sync.WaitGroup
	for i, build := range builds {
		workers.Go(func() {
			options := o
			options.Binary = build.binary
			options.pairedDriver = coordinator.driver(i)
			dir := filepath.Join(root, build.label)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				coordinator.abort(err)
				mu.Lock()
				report.Errors = append(report.Errors, build.label+": "+err.Error())
				mu.Unlock()
				return
			}
			individual, err := newRunReport(options, corpora, build.sha, host, dir)
			if err == nil {
				phase, sweep, artifacts, crashes, version, phaseErr := runPhase(coordinator.ctx, options, o.Scenario, corpora, "", dir)
				addPhase(&individual, phase, sweep, artifacts, crashes, version, phaseErr)
				if phaseErr != nil {
					coordinator.abort(phaseErr)
				}
				err = finishReport(&individual, 1)
			}
			if err != nil {
				coordinator.abort(err)
				individual.Errors = append(individual.Errors, err.Error())
				individual.Complete = false
			}
			mu.Lock()
			report.Builds[build.label] = individual
			mu.Unlock()
		})
	}
	workers.Wait()
	coordinator.mu.Lock()
	report.Window, report.Records, report.Metrics = coordinator.window, coordinator.records, coordinator.metrics
	groupErr := coordinator.err
	coordinator.mu.Unlock()
	if groupErr != nil {
		report.Errors = append(report.Errors, groupErr.Error())
	}
	report.Ratios = matchedLatencyRatios(labels, report.Records)
	report.Complete = len(report.Errors) == 0 && len(report.Builds) == len(labels) && !report.Window.Started.IsZero()
	for label, individual := range report.Builds {
		if !individual.Complete {
			report.Complete = false
			report.Errors = append(report.Errors, label+": incomplete run; inspect retained build errors")
		}
	}
	matched := 0
	for _, categories := range report.Ratios {
		for _, row := range categories {
			matched += row.Matched
		}
	}
	if matched == 0 {
		report.Complete = false
		report.Errors = append(report.Errors, "no successful identical matched requests")
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.Output, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := printPairedRatios(os.Stdout, report); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "coupled native report:", o.Output)
	if !report.Complete {
		return fmt.Errorf("incomplete paired run; errors and censored samples retained in %s", o.Output)
	}
	return nil
}

func printPairedRatios(w io.Writer, report pairedReport) error {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "coupled native comparison\tcategory\tmedian matched latency ratio\tmatched\texcluded")
	var comparisons []string
	for comparison := range report.Ratios {
		comparisons = append(comparisons, comparison)
	}
	slices.Sort(comparisons)
	for _, comparison := range comparisons {
		var categories []string
		for category := range report.Ratios[comparison] {
			categories = append(categories, category)
		}
		slices.Sort(categories)
		for _, category := range categories {
			row := report.Ratios[comparison][category]
			value := "unavailable"
			if row.Median.Value != nil {
				value = fmt.Sprintf("%.4f", *row.Median.Value)
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%d\n", comparison, category, value, row.Matched, row.Excluded)
		}
	}
	if err := table.Flush(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "Coupled request ratios do not establish independent throughput or saturation capacity.")
	return err
}
