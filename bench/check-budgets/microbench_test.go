package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMicroCensusRejectsMissingMalformedOrChangedSamples(t *testing.T) {
	for _, data := range []string{"", "PASS\n", "BenchmarkX-2 0 10 ns/op\n", "BenchmarkX-2 1 NaN ns/op\n", "BenchmarkX-2 1 10\n", "BenchmarkX-2 1 10 ns/op\nBenchmarkX-2 1 11 ns/op\n"} {
		if _, err := microCensus([]byte(data)); err == nil {
			t.Fatalf("invalid/duplicate row accepted: %q", data)
		}
	}
	keys, err := microCensus([]byte("goos: linux\nBenchmarkX-2 1 10 ns/op 4 B/op 1 allocs/op\nPASS\n"))
	if err != nil || len(keys) != 3 {
		t.Fatalf("valid units lost: %v %v", keys, err)
	}
	data, err := os.ReadFile("../../.github/workflows/bench-arm.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "golang.org/x/perf/cmd/benchstat@"+benchstatVersion) {
		t.Fatal("benchstat version drifted from existing repository pin")
	}
}

func syntheticMicroConfig(t *testing.T) microConfig {
	t.Helper()
	fixture := filepath.Join(t.TempDir(), "fixture")
	if err := os.Mkdir(fixture, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, pkg := range microPackages {
		if err := os.MkdirAll(filepath.Join(fixture, pkg), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fixture, "fixture.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = fixture
		data, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("synthetic git fixture: %v: %s", err, data)
		}
		return strings.TrimSpace(string(data))
	}
	run("init", "--initial-branch=main")
	run("add", "fixture.go")
	run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "synthetic fixture")
	c := microConfig{Version: 1, MainRevision: strings.Repeat("a", 40), CandidateRevision: strings.Repeat("b", 40), Fixture: fixture, FixtureRevision: run("rev-parse", "HEAD"), Benchstat: "/synthetic/benchstat", Benchtime: "1x", Timeout: "1m", Repeats: 3, Binaries: map[string]microPair{}}
	for _, pkg := range microPackages {
		c.Binaries[pkg] = microPair{Main: "/synthetic/main-" + strings.ReplaceAll(pkg, "/", "-"), Candidate: "/synthetic/candidate-" + strings.ReplaceAll(pkg, "/", "-")}
	}
	return c
}

func TestMicrobenchRunsSameFixtureAndRetainsMissingSampleFailure(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "missing-candidate-sample"}[missing], func(t *testing.T) {
			c := syntheticMicroConfig(t)
			if err := validateMicroConfig(c); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "evidence")
			if err := validateMicroFixture(c, out); err != nil {
				t.Fatal(err)
			}
			calls, statCalls := 0, 0
			process := func(_ context.Context, binary string, args []string, cwd string, _ []string) ([]byte, []byte, error) {
				if binary == c.Benchstat {
					statCalls++
					return []byte("synthetic comparison table\n"), nil, nil
				}
				calls++
				if !strings.HasPrefix(cwd, c.Fixture+string(filepath.Separator)) || !strings.Contains(strings.Join(args, " "), "-test.benchtime=1x") {
					t.Fatal("benchmark did not use identical pinned cwd/profile")
				}
				sample := "BenchmarkSynthetic-2 1 100 ns/op 8 B/op 1 allocs/op\n"
				if missing && strings.Contains(binary, "candidate") {
					sample = "PASS\n"
				}
				return []byte(sample), nil, nil
			}
			err := collectMicrobench(context.Background(), c, out, workflowProvenance{}, nil, process)
			if missing && err == nil || !missing && err != nil {
				t.Fatalf("wrong result: %v", err)
			}
			if calls != 18 {
				t.Fatalf("matrix shrank after failed sample: %d", calls)
			}
			if missing && statCalls != 0 || !missing && statCalls != 1 {
				t.Fatal("benchstat used partial evidence or was omitted")
			}
			var receipt microReceipt
			data, err := os.ReadFile(filepath.Join(out, "microbench.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			if len(receipt.Runs) != 18 || receipt.FormalAcceptance || receipt.Complete == missing {
				t.Fatal("receipt lost failed repetitions or claimed formal acceptance")
			}
		})
	}
}

func TestMicrobenchRejectsFixtureChangedDuringRunsBeforeBenchstat(t *testing.T) {
	c := syntheticMicroConfig(t)
	out := filepath.Join(t.TempDir(), "evidence")
	calls, statCalls := 0, 0
	process := func(_ context.Context, binary string, _ []string, _ string, _ []string) ([]byte, []byte, error) {
		if binary == c.Benchstat {
			statCalls++
			return nil, nil, nil
		}
		calls++
		if calls == 1 {
			if err := os.WriteFile(filepath.Join(c.Fixture, "fixture.go"), []byte("package changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return []byte("BenchmarkSynthetic-2 1 100 ns/op\n"), nil, nil
	}
	if err := collectMicrobench(context.Background(), c, out, workflowProvenance{}, nil, process); err == nil {
		t.Fatal("dirty post-run fixture accepted")
	}
	if calls != 18 || statCalls != 0 {
		t.Fatal("dirty fixture produced a comparison or lost retained runs")
	}
}
