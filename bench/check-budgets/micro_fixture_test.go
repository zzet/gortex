package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMicroFixturePreflightRefusesAbsentSkippedEmptyOrChangedTargets(t *testing.T) {
	for _, mutation := range []string{"", "absent-main-test", "skipped-test", "empty-callchain", "missing-case", "duplicate-receipt"} {
		t.Run(map[string]string{"": "complete"}[mutation]+mutation, func(t *testing.T) {
			c := syntheticMicroConfig(t)
			out := filepath.Join(t.TempDir(), "evidence")
			timed := 0
			process := func(_ context.Context, binary string, args []string, cwd string, env []string) ([]byte, []byte, error) {
				data, preflight := syntheticMicroFixtureResponse(args)
				if !preflight {
					timed++
					return []byte("BenchmarkSynthetic-2 1 100 ns/op\n"), nil, nil
				}
				if cwd != filepath.Join(c.Fixture, "internal/query") || !slices.Contains(env, "GORTEX_BENCHMARK_FIXTURE_CHECK=1") {
					t.Fatal("preflight escaped the pinned query fixture or omitted explicit opt-in")
				}
				if mutation == "absent-main-test" && strings.Contains(binary, "main") {
					return nil, nil, nil
				}
				if slices.Contains(args, "-test.run=^"+microFixtureTest+"$") {
					switch mutation {
					case "skipped-test":
						data = []byte("--- SKIP: " + microFixtureTest + " (0.00s)\nPASS\n")
					case "empty-callchain":
						data = []byte(strings.ReplaceAll(string(data), `"GetCallChain":[2,1]`, `"GetCallChain":[1,0]`))
					case "missing-case":
						data = []byte(strings.ReplaceAll(string(data), `"GetCallChain":[2,1],`, ""))
					case "duplicate-receipt":
						data = append(data, data...)
					}
				}
				return data, nil, nil
			}
			err := collectMicrobench(context.Background(), c, out, workflowProvenance{}, nil, process)
			if mutation == "" && err != nil || mutation != "" && err == nil {
				t.Fatalf("wrong executable fixture admission: %v", err)
			}
			if mutation != "" && timed != 0 {
				t.Fatal("timing or benchstat started after rejected fixture preflight")
			}
			var receipt microReceipt
			data, readErr := os.ReadFile(filepath.Join(out, "microbench.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			if len(receipt.FixtureChecks) != 2 || receipt.Complete != (mutation == "") {
				t.Fatal("both binary preflight receipts and failure state must be retained")
			}
			if mutation == "absent-main-test" && !strings.Contains(receipt.Error, "trusted main must contain the new harness contract") {
				t.Fatal("old main's absent executable test must fail explicitly")
			}
		})
	}
}
