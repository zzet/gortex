package citest

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

func WorkflowRows(text string, p Policy) ([]MatrixRow, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.Contains(text, "matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}") {
		return p.FullMatrix().Include, nil
	}
	if regexp.MustCompile(`(?m)^\s+exclude:`).MatchString(text) {
		return nil, fmt.Errorf("test matrix must not use exclude")
	}
	values := func(key string) []string {
		match := regexp.MustCompile(`(?m)^\s+` + key + `:\s*\[([^\]]*)\]`).FindStringSubmatch(text)
		if len(match) == 0 {
			return nil
		}
		var out []string
		for _, value := range strings.Split(match[1], ",") {
			out = append(out, strings.Trim(strings.TrimSpace(value), "\"'"))
		}
		return out
	}
	rows := []MatrixRow{}
	for _, os := range values("os") {
		for _, part := range values("partition") {
			rows = append(rows, MatrixRow{os, part})
		}
	}
	for _, body := range regexp.MustCompile(`\{([^{}]*\bpartition:[^{}]*)\}`).FindAllStringSubmatch(text, -1) {
		fields := map[string]string{}
		for _, part := range strings.Split(body[1], ",") {
			key, value, ok := strings.Cut(part, ":")
			if ok {
				fields[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
			}
		}
		if fields["os"] == "" {
			return nil, fmt.Errorf("matrix include has no OS")
		}
		rows = append(rows, MatrixRow{fields["os"], fields["partition"]})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no test matrix rows found")
	}
	return rows, nil
}
func runnerOptions(root string, env []string) (RunOptions, error) {
	windows := envValue(env, "TEST_WINDOWS")
	if windows == "" {
		windows = strconv.FormatBool(runtime.GOOS == "windows")
	}
	isWindows := windows == "true"
	race := envValue(env, "TEST_RACE")
	if race == "" {
		race = strconv.FormatBool(!isWindows)
	}
	if race != "true" && race != "false" || isWindows && race == "true" {
		return RunOptions{}, fmt.Errorf("TEST_RACE must be true or false, and false on Windows")
	}
	workers := runtime.NumCPU()
	if value := envValue(env, "TEST_WORKERS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return RunOptions{}, fmt.Errorf("TEST_WORKERS must be a positive integer")
		}
		workers = n
	}
	return RunOptions{Root: root, Env: env, Race: race == "true", Windows: isWindows, Workers: workers, Partition: envValue(env, "TEST_PARTITION"), Target: envValue(env, "TEST_TARGET")}, nil
}
func RunTests(ctx context.Context, root string, args, env []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ci-test", flag.ContinueOnError)
	flags.SetOutput(stderr)
	part := flags.String("partition", envValue(env, "TEST_PARTITION"), "group or group:i/n")
	selection := flags.String("selection", "", "validated manifest")
	target := flags.String("target", envValue(env, "TEST_TARGET"), "native target")
	workflow := flags.String("check-workflow", "", "validate full matrix and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *workflow != "" {
		data, err := os.ReadFile(*workflow)
		if err != nil {
			return err
		}
		p, err := LoadPolicy()
		if err != nil {
			return err
		}
		rows, err := WorkflowRows(string(data), p)
		if err != nil {
			return err
		}
		if err := CheckLayout(rows, p); err != nil {
			return err
		}
		oses := set{}
		for _, row := range rows {
			oses.add(row.OS)
		}
		fmt.Fprintf(stdout, "%s: test layout covers every group on %v\n", *workflow, oses.sorted())
		return nil
	}
	if *part == "" {
		return fmt.Errorf("set --partition or TEST_PARTITION")
	}
	opts, err := runnerOptions(root, env)
	if err != nil {
		return err
	}
	opts.Partition = *part
	opts.SelectionFile = *selection
	opts.Target = *target
	opts.Output = stdout
	_, err = RunPartition(ctx, opts)
	return err
}
