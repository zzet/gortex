package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type smokeProvenance struct {
	RetainReadSourceFailures bool              `json:"retain_read_source_failures,omitempty"`
	EditACKMode              string            `json:"edit_ack_mode,omitempty"`
	Main                     artifactSelection `json:"main_selection"`
	MainBinary               mainArtifact      `json:"main_binary"`
	CandidateBinary          mainArtifact      `json:"candidate_binary"`
	RunnerEnvironment        string            `json:"runner_environment"`
	RunnerOS                 string            `json:"runner_os"`
	RunnerArch               string            `json:"runner_arch"`
}

func readStrictJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected exactly one JSON document")
	}
	return nil
}

func verifiedMainBundle(path string) (artifactSelection, mainArtifact, error) {
	var selection artifactSelection
	var metadata mainArtifact
	if err := readStrictJSON(filepath.Join(path, "selection.json"), &selection); err != nil {
		return selection, metadata, err
	}
	if err := readStrictJSON(filepath.Join(path, "main-artifact.json"), &metadata); err != nil {
		return selection, metadata, err
	}
	binary, err := os.ReadFile(filepath.Join(path, mainArtifactBinary))
	if err != nil {
		return selection, metadata, err
	}
	if err := verifyMainArtifact(selection, metadata, binary); err != nil {
		return selection, metadata, err
	}
	if metadata.Microbench != nil {
		files := map[string][]byte{}
		for _, pkg := range microPackages {
			data, err := os.ReadFile(filepath.Join(path, microTestName(pkg)))
			if err != nil {
				return selection, metadata, err
			}
			files[microTestName(pkg)] = data
		}
		data, err := os.ReadFile(filepath.Join(path, bundledBenchstatName))
		if err != nil {
			return selection, metadata, err
		}
		files[bundledBenchstatName] = data
		if err := validateMicroBundle(metadata, files); err != nil {
			return selection, metadata, err
		}
	}
	return selection, metadata, nil
}

func sameBuildSettings(main, candidate mainArtifact) bool {
	normalize := func(m mainArtifact) map[string]string {
		settings := map[string]string{}
		for key, value := range m.Settings {
			if key != "vcs.revision" && key != "vcs.time" {
				settings[key] = value
			}
		}
		return settings
	}
	return main.OS == candidate.OS && main.Arch == candidate.Arch && main.GoVersion == candidate.GoVersion && reflect.DeepEqual(normalize(main), normalize(candidate))
}

func prepareSmokeProvenance(c laneConfig, workflow workflowProvenance) (smokeProvenance, error) {
	var proof smokeProvenance
	if c.HostClass != hostedSmokeClass || c.MainArtifact == "" || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GITHUB_SHA") != workflow.Revision || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" || os.Getenv("RUNNER_OS") != "Linux" || os.Getenv("RUNNER_ARCH") != "ARM64" {
		return proof, errors.New("smoke needs a verified exact-main bundle and current GitHub job provenance")
	}
	selection, metadata, err := verifiedMainBundle(c.MainArtifact)
	if err != nil {
		return proof, err
	}
	expected, err := filepath.EvalSymlinks(filepath.Join(c.MainArtifact, mainArtifactBinary))
	if err != nil {
		return proof, err
	}
	actual, err := filepath.EvalSymlinks(c.Builds["main"].Binary)
	if err != nil {
		return proof, err
	}
	if actual != expected || c.Builds["main"].Revision != selection.Revision {
		return proof, errors.New("smoke main build is not the selected downloaded exact-main binary")
	}
	candidate, err := binaryArtifact(c.Builds["candidate"].Binary)
	if err != nil {
		return proof, err
	}
	if candidate.Revision != c.Builds["candidate"].Revision || !sameBuildSettings(metadata, candidate) {
		return proof, errors.New("candidate/main binary platform, pinned toolchain or build settings differ")
	}
	proof = smokeProvenance{RetainReadSourceFailures: c.RetainReadSourceFailures, Main: selection, MainBinary: metadata, CandidateBinary: candidate, RunnerEnvironment: os.Getenv("RUNNER_ENVIRONMENT"), RunnerOS: os.Getenv("RUNNER_OS"), RunnerArch: os.Getenv("RUNNER_ARCH")}
	if err := validateSmokeWorkflow(workflow); err != nil {
		return proof, err
	}
	return proof, validateSmokeDispatchBinding(workflow, proof)
}

var pullRefPattern = regexp.MustCompile(`^refs/pull/[0-9]+/(head|merge)$`)

func validateSmokeWorkflow(workflow workflowProvenance) error {
	runID, err := strconv.ParseInt(strings.TrimPrefix(workflow.RunURL, "https://github.com/zzet/gortex/actions/runs/"), 10, 64)
	identity := workflow.Job != "" && workflow.Attempt >= 1 && revisionPattern.MatchString(workflow.Revision) && err == nil && runID > 0
	pr := workflow.Event == "pull_request" && pullRefPattern.MatchString(workflow.Ref) && workflow.SmokeDispatch == nil
	manual := workflow.Event == "workflow_dispatch" && validSmokeDispatchWorkflow(workflow)
	if !identity || !pr && !manual {
		return errors.New("smoke comparisons require one explicit PR workflow run/attempt/job identity or an explicitly pinned trusted-main manual smoke")
	}
	return nil
}

func validateSmokeJob(receipt laneReceipt, d dataset) error {
	if receipt.Plan.Lane != "smoke" || receipt.Smoke == nil || receipt.Started.IsZero() || receipt.Finished.Before(receipt.Started) {
		return errors.New("same-job smoke receipt/provenance is missing")
	}
	if err := validateSmokeWorkflow(receipt.Workflow); err != nil {
		return err
	}
	proof := receipt.Smoke
	if err := validateSmokeDispatchBinding(receipt.Workflow, *proof); err != nil {
		return err
	}
	for _, run := range d.runs {
		if (run.data.FailureCapture != nil) != proof.RetainReadSourceFailures || !validReadSourceFailureConfig(run.data.FailureCapture) {
			return errors.New("smoke failure-capture configuration differs from its selected bounded policy")
		}
	}
	if proof.EditACKMode != "" {
		if err := validateSmokeACKPolicy(d, proof.EditACKMode); err != nil {
			return err
		}
	}
	if receipt.Plan.HostClass != hostedSmokeClass || d.config.Hosts[hostedSmokeClass] == "" || proof.RunnerEnvironment != "github-hosted" || proof.RunnerOS != "Linux" || proof.RunnerArch != "ARM64" {
		return errors.New("smoke needs the scoped public hosted Linux ARM job profile, not reference capacity")
	}
	if proof.Main.Revision != d.config.MainRevision || proof.CandidateBinary.Revision != d.config.CandidateRevision || receipt.Workflow.Revision != d.config.CandidateRevision || !sameBuildSettings(proof.MainBinary, proof.CandidateBinary) {
		return errors.New("smoke binary/input/workflow identities differ")
	}
	if err := validateMainRun(proof.Main.Run, proof.Main.Repository, proof.Main.Revision); err != nil {
		return err
	}
	tasks := map[string]taskReceipt{}
	for _, task := range receipt.Tasks {
		if task.Error != "" || task.Unavailable != "" {
			return errors.New("smoke retains failed/unavailable task and cannot pass")
		}
		if task.Report == "" || !task.Finished.After(task.Started) || task.Started.Before(receipt.Started) || task.Finished.After(receipt.Finished) {
			return errors.New("smoke task lacks bounded same-job execution timestamps")
		}
		if _, exists := tasks[task.Report]; exists {
			return errors.New("smoke report reused between tasks")
		}
		tasks[task.Report] = task
	}
	if len(tasks) != len(d.runs) {
		return errors.New("smoke receipt does not cover every input report")
	}
	for _, run := range d.runs {
		task, exists := tasks[run.input.Report]
		if !exists || task.Role != run.input.Role || task.Repeat != run.input.Repeat || task.Scenario != run.input.Scenario || run.data.Started.IsZero() || run.data.Started.Before(task.Started) || run.data.Started.After(task.Finished) {
			return fmt.Errorf("%s/%d/%s is not a measured report from this same job", run.input.Scenario, run.input.Repeat, run.input.Role)
		}
	}
	retained := receipt.Plan.Execution == "two_owned_smoke_stores_v1"
	if receipt.Plan.Execution != "" && !retained {
		return errors.New("unknown smoke store-execution contract")
	}
	for _, run := range d.runs {
		if len(run.data.Phases[0].Gate["pr_smoke_store"]) > 0 && !retained {
			return errors.New("retained-store evidence cannot claim an ordinary standalone execution")
		}
	}
	if retained {
		if err := validateSmokeStores(d); err != nil {
			return err
		}
	}
	// Host/corpus/workload equality, all planned repeats, p95 medians and safety
	// counters are still evaluated by check. Artifact timing is never consumed.
	return nil
}

func verifySmokeCommand(args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("verify-smoke", flag.ContinueOnError)
	f.SetOutput(stderr)
	evidence := f.String("evidence", "", "retained same-job collection directory")
	bundle := f.String("main-bundle", "", "verified downloaded main artifact directory")
	budgets := f.String("budgets", "bench/budgets.yaml", "strict policy")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *evidence == "" || *bundle == "" || f.NArg() != 0 {
		fmt.Fprintln(stderr, "--evidence and --main-bundle required")
		return 2
	}
	var receipt laneReceipt
	err := readStrictJSON(filepath.Join(*evidence, "lane.json"), &receipt)
	if err == nil {
		var selection artifactSelection
		var metadata mainArtifact
		selection, metadata, err = verifiedMainBundle(*bundle)
		if err == nil && (receipt.Smoke == nil || !reflect.DeepEqual(selection, receipt.Smoke.Main) || !reflect.DeepEqual(metadata, receipt.Smoke.MainBinary)) {
			err = errors.New("smoke receipt is not bound to retained downloaded-main bundle")
		}
	}
	var d dataset
	if err == nil {
		d, err = loadInputs(filepath.Join(*evidence, "inputs.json"))
	}
	if err == nil {
		err = validateSmokeJob(receipt, d)
	}
	var p policy
	if err == nil {
		var data []byte
		data, err = os.ReadFile(*budgets)
		if err == nil {
			p, err = loadPolicy(data)
		}
	}
	if err == nil && (receipt.Plan.PolicySHA256 != policyDigest(p) || !receipt.Plan.Ready || len(receipt.Plan.Prerequisites) != 0 || receipt.Error != "") {
		err = errors.New("smoke policy/readiness/collection did not complete successfully")
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	rows, err := check(p, d, "smoke")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !printRows(stdout, rows) {
		return 1
	}
	return 0
}

// Configured workload identity and actual wire ACKs are separate observations.
// Historical receipts without an explicit policy keep their original identity.
func validateSmokeACKPolicy(d dataset, expected string) error {
	if expected != "local" && expected != "full" {
		return errors.New("unknown explicit smoke ACK policy")
	}
	for _, run := range d.runs {
		var workload struct {
			EditACKMode string `json:"edit_ack_mode"`
		}
		if json.Unmarshal(run.data.Workload, &workload) != nil || workload.EditACKMode != expected {
			return errors.New("smoke workload differs from its explicitly configured ACK policy")
		}
		if run.input.Scenario != "loadedit-4" {
			continue
		}
		if len(run.data.Phases) != 1 || len(run.data.Phases[0].Edits) == 0 {
			return errors.New("smoke lacks actual edit ACK policy observations")
		}
		for _, edit := range run.data.Phases[0].Edits {
			if edit.Acknowledgement.StructuredContent.AckMode != expected {
				return errors.New("missing or mismatched effective smoke edit ACK policy")
			}
		}
	}
	return nil
}
