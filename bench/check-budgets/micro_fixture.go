package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const microFixtureTest = "TestBenchmarkFixtureTargets"
const microFixtureMarker = "GORTEX_BENCHMARK_FIXTURE_V1 "

type microFixtureCheck struct {
	Role                string           `json:"role"`
	Started             time.Time        `json:"started"`
	Finished            time.Time        `json:"finished"`
	Observations        map[string][]int `json:"nodes_edges,omitempty"`
	Complete            bool             `json:"complete"`
	Error               string           `json:"error,omitempty"`
	TimeoutCapture      string           `json:"timeout_capture,omitempty"`
	CaptureGraceSeconds int              `json:"timeout_capture_grace_seconds,omitempty"`
}

func parseMicroFixtureCheck(stdout []byte) (map[string][]int, error) {
	var data string
	passed := false
	for line := range strings.SplitSeq(string(stdout), "\n") {
		if strings.HasPrefix(line, "--- PASS: "+microFixtureTest+" (") {
			passed = true
		}
		if index := strings.Index(line, microFixtureMarker); index >= 0 {
			if data != "" {
				return nil, errors.New("query fixture preflight emitted duplicate result receipts")
			}
			data = line[index+len(microFixtureMarker):]
		}
	}
	var observations map[string][]int
	if !passed || data == "" {
		return nil, errors.New("query fixture preflight did not execute and pass with indexed results")
	}
	if err := json.Unmarshal([]byte(data), &observations); err != nil {
		return nil, err
	}
	names := []string{"SearchSymbols", "GetDependencies", "GetDependents", "GetCallers", "GetCallChain", "FindUsages", "GetCluster", "Stats"}
	if len(observations) != len(names) {
		return nil, errors.New("query fixture result census differs from the benchmark contract")
	}
	for _, name := range names {
		counts := observations[name]
		if len(counts) != 2 || name == "SearchSymbols" && (counts[0] < 1 || counts[1] != 0) || name != "SearchSymbols" && (counts[0] < 2 || counts[1] < 1) {
			return nil, fmt.Errorf("query fixture %s must return meaningful indexed results", name)
		}
	}
	return observations, nil
}

func checkMicroFixtureExecutables(ctx context.Context, c microConfig, out string, process microProcess) ([]microFixtureCheck, error) {
	var checks []microFixtureCheck
	var failures []error
	for _, role := range []string{"main", "candidate"} {
		check := microFixtureCheck{Role: role, Started: time.Now().UTC()}
		root := filepath.Join(out, "fixture-preflight-"+role)
		err := os.Mkdir(root, 0o700)
		if err == nil {
			binary := c.Binaries["internal/query"].Main
			if role == "candidate" {
				binary = c.Binaries["internal/query"].Candidate
			}
			duration, _ := time.ParseDuration(c.Timeout)
			runCtx, cancel := context.WithTimeout(ctx, duration)
			if runtime.GOOS == "linux" {
				runCtx = context.WithValue(runCtx, queryMicroDiagnosticsKey{}, true)
				check.TimeoutCapture = queryMicroCapturePolicy
				check.CaptureGraceSeconds = int(queryMicroCaptureGrace / time.Second)
			}
			env := append(os.Environ(), "GORTEX_BENCHMARK_FIXTURE_CHECK=1", "TMPDIR="+root, "TMP="+root, "TEMP="+root)
			cwd := filepath.Join(c.Fixture, "internal/query")
			stdout, stderr, listErr := process(runCtx, binary, []string{"-test.list=^" + microFixtureTest + "$"}, cwd, env)
			err = errors.Join(listErr, os.WriteFile(filepath.Join(root, "list.stdout.txt"), stdout, 0o600), os.WriteFile(filepath.Join(root, "list.stderr.txt"), stderr, 0o600))
			if err == nil && strings.TrimSpace(string(stdout)) != microFixtureTest {
				err = errors.New("query executable lacks required fixture preflight; trusted main must contain the new harness contract")
			}
			if err == nil {
				stdout, stderr, err = process(runCtx, binary, []string{"-test.run=^" + microFixtureTest + "$", "-test.v", "-test.count=1", "-test.timeout=" + c.Timeout}, cwd, env)
				err = errors.Join(err, os.WriteFile(filepath.Join(root, "check.stdout.txt"), stdout, 0o600), os.WriteFile(filepath.Join(root, "check.stderr.txt"), stderr, 0o600))
				if err == nil {
					check.Observations, err = parseMicroFixtureCheck(stdout)
				}
			}
			cancel()
		}
		check.Finished = time.Now().UTC()
		check.Complete = err == nil
		if err != nil {
			check.Error = err.Error()
			failures = append(failures, fmt.Errorf("%s fixture preflight: %w", role, err))
		}
		checks = append(checks, check)
	}
	return checks, errors.Join(failures...)
}
