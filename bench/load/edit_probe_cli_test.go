package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a scripted `gortex call` process. It records its
// argv and answers from the environment, or hangs until it is killed.
func init() {
	switch os.Getenv("LOAD_EDIT_PROBE_CLI_FIXTURE") {
	case "":
		return
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(9)
	}
	argv, _ := json.Marshal(os.Args[1:])
	if os.WriteFile(os.Getenv("LOAD_EDIT_PROBE_CLI_ARGV"), argv, 0o600) != nil {
		os.Exit(7)
	}
	if os.Getenv("LOAD_EDIT_PROBE_CLI_FIXTURE") == "large" {
		fmt.Fprint(os.Stdout, strings.Repeat("x", (8<<20)+1))
	} else {
		fmt.Fprint(os.Stdout, os.Getenv("LOAD_EDIT_PROBE_CLI_STDOUT"))
	}
	fmt.Fprint(os.Stderr, "fixture stderr")
	if os.Getenv("LOAD_EDIT_PROBE_CLI_EXIT") != "" {
		os.Exit(1)
	}
	os.Exit(0)
}

func editProbeCLIFixture(t *testing.T, deadline time.Duration, env ...string) (*editProbeRuntime, *editProbeState, editProbeOracle) {
	t.Helper()
	dir := t.TempDir()
	corpus := editProbeCorpus()
	corpus.EditOutput = "cli"
	plan, err := makeEditProbePlan(corpus, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	sink := newEditProbeHeldSink(t)
	sink.unblock()
	state := startHeldEditProbeSpool(t, sink)
	state.step, state.record = plan.Steps[0], newEditProbeRecord(corpus, plan.Steps[0])
	origin := time.Now()
	state.started, state.acknowledged = origin.Add(time.Millisecond), origin.Add(2*time.Millisecond)
	p := &privateRun{Root: dir, Config: filepath.Join(dir, "config.yaml"), Binary: os.Args[0], Env: append(os.Environ(), env...), Repos: []privateRepo{{Corpus: corpus, Prefix: "owned", Path: filepath.Join(dir, "clone")}}}
	run := &editProbeRuntime{options: runOptions{Deadline: deadline}, origin: origin, plan: plan, p: p, phase: &Phase{}}
	return run, state, editProbeOracles(corpus, plan.Steps[0])[1]
}

func TestEditProbeCLIProbeOwnsProcessAndDecodesStdout(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv.json")
	marker := "GortexEditProbe0000000000000001N000"
	answer := `{"nodes":[{"id":"owned/sample.go::` + marker + `"}],"truncated":false,"freshness":"fresh"}`
	run, state, oracle := editProbeCLIFixture(t, 10*time.Second, "LOAD_EDIT_PROBE_CLI_FIXTURE=reply", "LOAD_EDIT_PROBE_CLI_ARGV="+argvFile, "LOAD_EDIT_PROBE_CLI_STDOUT="+answer)
	if oracle.name != "summary" || state.step.Marker != marker {
		t.Fatal(oracle.name, state.step.Marker)
	}
	data, identity, err := run.performCLI(t.Context(), state, oracle.call, "oracle:summary", 11)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.observeProbe(state, oracle, identity.WriteStarted, data, identity, err, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		t.Fatal(err)
	}
	if len(argv) != 11 {
		t.Fatalf("CLI argv %q", argv)
	}
	want := []string{"--config", run.p.Config, "call", "read", "--json", argv[5], "--format", "json", "--index", run.p.Repos[0].Path, "--quiet"}
	lowered, err := json.Marshal(dispatchArgs(oracle.call, run.p.Repos[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(argv, want) || argv[5] != string(lowered) {
		t.Fatalf("CLI argv %q, want the lowered facade request %v", argv, dispatchArgs(oracle.call, run.p.Repos[0]))
	}
	if state.record.FirstCorrect["summary"].Value == nil || state.firstProbeWrite.IsZero() || len(run.phase.Attempts) != 1 || run.phase.Attempts[0].Tool != "cli:read.summary" || run.phase.Attempts[0].Role != "probe" || run.phase.Attempts[0].Outcome != "success" {
		t.Fatalf("CLI answer not owned or credited: %+v %+v", state.record.FirstCorrect, run.phase.Attempts)
	}
	coverage := editProbeOutputCoverage(run.plan.Mode, run.modeCounts)
	if cli := coverage["cli"]; cli.State != "selected" || cli.ProbeAnswers != 1 || cli.ClaimsFresh != 1 || cli.StaleClaimedFresh.Value == nil || *cli.StaleClaimedFresh.Value != 0 || coverage["json"].ProbeAnswers != 0 {
		t.Fatalf("CLI answers counted as another mode: %+v", coverage)
	}
	// A tool refusal is only a nonzero exit and stderr text: a probe error.
	run.p.Env = append(run.p.Env, "LOAD_EDIT_PROBE_CLI_EXIT=1")
	data, identity, err = run.performCLI(t.Context(), state, oracle.call, "oracle:summary", 11)
	if err == nil || !strings.Contains(err.Error(), "fixture stderr") || run.phase.Attempts[1].Outcome != "error" {
		t.Fatalf("failed CLI call: %v %+v", err, run.phase.Attempts)
	}
	if err := run.observeProbe(state, oracle, identity.WriteStarted, data, identity, err, false); err != nil {
		t.Fatal(err)
	}
	if state.record.ProbeErrors != 1 {
		t.Fatalf("CLI exit not a probe error: %+v", state.record)
	}
}

func TestEditProbeCLIRefusesProseWrapperAndUnsafeNumbers(t *testing.T) {
	if body, err := decodeEditProbeOutput([]byte("{\n  \"total\": 9007199254740991,\n  \"score\": 0.5\n}\n"), "cli", "read.summary"); err != nil || body["total"] != json.Number("9007199254740991") {
		t.Fatalf("exact CLI JSON refused: %v %v", body, err)
	}
	for name, stdout := range map[string]string{
		"prose wrapper":     `{"tool":"explore","format":"text","text":"# Answer"}`,
		"unsafe integer":    `{"total":9007199254740993}`,
		"unsafe negative":   `{"total":-9007199254740992}`,
		"out of range":      `{"total":1e400}`,
		"nested unsafe":     `{"nodes":[{"line":18014398509481984}]}`,
		"array":             `[1]`,
		"two values":        `{} {}`,
		"empty":             ``,
		"TOON passed as -":  "nodes[1]{id}:\n  a\n",
		"MCP envelope text": `{"content":[{"type":"text","text":"{}"}]}x`,
	} {
		if _, err := decodeEditProbeOutput([]byte(stdout), "cli", "read.summary"); err == nil {
			t.Fatal("accepted", name)
		}
	}
}

func TestEditProbeCLIStdoutKeepsEightMiBLimit(t *testing.T) {
	argvFile := filepath.Join(t.TempDir(), "argv.json")
	run, state, oracle := editProbeCLIFixture(t, 10*time.Second, "LOAD_EDIT_PROBE_CLI_FIXTURE=large", "LOAD_EDIT_PROBE_CLI_ARGV="+argvFile)
	_, _, err := run.performCLI(t.Context(), state, oracle.call, "oracle:summary", 11)
	if !errors.Is(err, errEditProbeEvidenceIncomplete) || !strings.Contains(err.Error(), "CLI stdout exceeded") || run.phase.Attempts[0].Outcome != "error" {
		t.Fatalf("oversized CLI stdout was not refused: %v %+v", err, run.phase.Attempts)
	}
	state.finishRaw()
	if state.sinkErr != nil {
		t.Fatalf("bounded CLI refusal evidence was lost: %v", state.sinkErr)
	}
	sink := state.sink.(*editProbeHeldSink)
	var row struct {
		Stdout string `json:"stdout"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(sink.data.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if len(row.Stdout) != 8<<20 || row.Error == "" {
		t.Fatalf("CLI stdout allowance changed or refusal omitted: retained=%d error=%q", len(row.Stdout), row.Error)
	}
}

func TestEditProbeCLIDeadlineKillsProcessGroupAndCensors(t *testing.T) {
	run, state, oracle := editProbeCLIFixture(t, 300*time.Millisecond, "LOAD_EDIT_PROBE_CLI_FIXTURE=hang")
	started := time.Now()
	_, _, err := run.performCLI(t.Context(), state, oracle.call, "oracle:summary", 11)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 10*time.Second || run.phase.Attempts[0].Outcome != "product_deadline" {
		t.Fatalf("hung CLI probe not killed at its deadline: %v after %s %+v", err, time.Since(started), run.phase.Attempts)
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	run.options.Deadline = time.Minute
	_, _, err = run.performCLI(ctx, state, oracle.call, "oracle:summary", 11)
	if !errors.Is(err, context.Canceled) || run.phase.Attempts[1].Outcome != "phase_censored" {
		t.Fatalf("phase end did not censor the CLI probe: %v %+v", err, run.phase.Attempts)
	}
}

func TestEditProbeCLIModeRefusesAbsenceKinds(t *testing.T) {
	deleted := editProbeRenameCorpus(t, "go", 100)
	deleted.EditKind = "delete"
	for _, c := range []Corpus{editProbeRenameCorpus(t, "go", 100), deleted, moveProbeCorpus(42)} {
		c.EditOutput = "cli"
		if _, err := makeEditProbePlan(c, 42, 100); err == nil || !strings.Contains(err.Error(), "CLI probes cannot check "+c.EditKind) {
			t.Fatalf("CLI admitted %s: %v", c.EditKind, err)
		}
	}
	for _, c := range []Corpus{editProbeCorpus(), editProbeBodyCorpus(t, "go", "modify_body", 100)} {
		baseline, err := makeEditProbePlan(c, 42, 100)
		if err != nil {
			t.Fatal(err)
		}
		operations := editProbeOperations(baseline, c)
		c.EditOutput = "cli"
		plan, err := makeEditProbePlan(c, 42, 100)
		if err != nil {
			t.Fatal(err)
		}
		// The edit, its readbacks and every oracle request stay the JSON plan's.
		if !reflect.DeepEqual(plan.Steps, baseline.Steps) || !reflect.DeepEqual(editProbeOperations(plan, c), operations) {
			t.Fatal("CLI mode changed the offered requests")
		}
		if !strings.Contains(strings.Join(newEditProbeReport(plan, c, true).Limitations, "\n"), "CLI probe cadence is bounded by process start") {
			t.Fatal("CLI limitations missing")
		}
	}
}

func TestEditGateCLIUsesItsAssignedCheckoutAndRejectsUnknownEditor(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			argvFile := filepath.Join(t.TempDir(), "argv.json")
			run, state, oracle := editProbeCLIFixture(t, 10*time.Second, "LOAD_EDIT_PROBE_CLI_FIXTURE=reply", "LOAD_EDIT_PROBE_CLI_ARGV="+argvFile, "LOAD_EDIT_PROBE_CLI_STDOUT={}")
			defer state.finishRaw()
			second := run.p.Repos[0]
			second.Path, second.Prefix, second.Corpus.Label = filepath.Join(run.p.Root, "second-clone"), "second", "second"
			run.p.Repos = append(run.p.Repos, second)
			run.editors = []*socketClient{{cwd: second.Path}}
			if invalid {
				state.step.Editor = 7
			}
			_, _, err := run.performCLI(t.Context(), state, oracle.call, "oracle:summary", 11)
			if invalid {
				if err == nil {
					t.Error("unknown route started a CLI process against the first repository")
				}
				if _, err := os.Stat(argvFile); !errors.Is(err, os.ErrNotExist) {
					t.Error("foreign editor launched a process")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatal(err)
			}
			var argv []string
			if err := json.Unmarshal(raw, &argv); err != nil {
				t.Fatal(err)
			}
			lowered, err := json.Marshal(dispatchArgs(oracle.call, second))
			if err != nil {
				t.Fatal(err)
			}
			if len(argv) != 11 || argv[9] != second.Path || argv[5] != string(lowered) {
				t.Errorf("CLI borrowed another checkout's index/arguments: %q", argv)
			}
		})
	}
}
