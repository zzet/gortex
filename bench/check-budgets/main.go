// Command check-budgets compares repeated #996 report-v1 measurements with the
// release policy. The default command consumes evidence; collect invokes the
// signal-aware load client for private owned daemons. Plan starts no workload.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "plan", "collect":
			return laneCommand(args[0], args[1:], stdout, stderr)
		case "history":
			return historyCommand(args[1:], stdout, stderr)
		case "main-artifact", "fetch-main":
			return artifactCommand(args[0], args[1:], stdout, stderr)
		case "verify-smoke":
			return verifySmokeCommand(args[1:], stdout, stderr)
		case "microbench":
			return microbenchCommand(args[1:], stdout, stderr)
		}
	}
	f := flag.NewFlagSet("check-budgets", flag.ContinueOnError)
	f.SetOutput(stderr)
	policyPath := f.String("budgets", "bench/budgets.yaml", "strict budget policy")
	inputPath := f.String("inputs", "", "JSON manifest of pinned repeated harness report paths")
	lane := f.String("lane", "", "smoke, nightly or release")
	output := f.String("out", "", "optional NEW JSON evidence output (never overwritten)")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if f.NArg() != 0 || *inputPath == "" || *lane == "" {
		fmt.Fprintln(stderr, "--inputs and --lane are required; no positional arguments")
		return 2
	}
	rows, err := execute(*policyPath, *inputPath, *lane)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	pass := printRows(stdout, rows)
	if *output != "" {
		data, err := json.MarshalIndent(struct {
			Lane string   `json:"lane"`
			Pass bool     `json:"pass"`
			Rows []result `json:"rows"`
		}{*lane, pass, rows}, "", "  ")
		if err == nil {
			var file *os.File
			file, err = os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err == nil {
				_, err = file.Write(append(data, '\n'))
				err = errors.Join(err, file.Close())
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "cannot retain budget evidence:", err)
			return 1
		}
	}
	if !pass {
		return 1
	}
	return 0
}

func execute(policyPath, inputPath, lane string) ([]result, error) {
	data, err := os.ReadFile(policyPath)
	if err != nil {
		return nil, err
	}
	p, err := loadPolicy(data)
	if err != nil {
		return nil, err
	}
	d, err := loadInputs(inputPath)
	if err != nil {
		return nil, err
	}
	return check(p, d, lane)
}

func printRows(w io.Writer, rows []result) bool {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "HOST / SCENARIO / METRIC\tBEFORE -> AFTER\tLIMIT\tADDED HEADROOM\tRESULT / BASIS")
	value := func(v *float64) string {
		if v == nil {
			return "unavailable"
		}
		return fmt.Sprintf("%.6g", *v)
	}
	pass := true
	for _, row := range rows {
		status := "PASS"
		if !row.Pass {
			status, pass = "FAIL", false
		}
		detail := strings.Join(append([]string{row.Baseline, row.Reason}, row.Allowances...), "; ")
		if row.Worst != nil {
			detail += "; worst hard repeat=" + value(row.Worst)
		}
		fmt.Fprintf(table, "%s / %s / %s\t%s -> %s %s\t%s\t%s\t%s / %s\n", row.HostClass, row.Scenario, row.Metric, value(row.Before), value(row.After), row.Unit, value(row.Limit), value(row.EffectiveHeadroom), status, detail)
	}
	if err := table.Flush(); err != nil {
		return false
	}
	return pass
}
