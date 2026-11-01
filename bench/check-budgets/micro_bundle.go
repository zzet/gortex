package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const testBuildReceiptName = "test-build.json"
const bundledBenchstatName = "benchstat"

func microTestName(pkg string) string {
	return "microbench-" + strings.ReplaceAll(pkg, "/", "-") + ".test"
}

type packagedExecutable struct {
	File     string                `json:"file"`
	Observed executableObservation `json:"observed"`
}

type mainMicroBundle struct {
	Version         int                           `json:"schema_version"`
	FixtureRevision string                        `json:"fixture_revision"`
	FixtureTree     string                        `json:"fixture_tree"`
	SourceTree      string                        `json:"source_tree"`
	Tests           map[string]packagedExecutable `json:"tests"`
	Benchstat       packagedExecutable            `json:"benchstat"`
}

type testBuildReceipt struct {
	Version  int                           `json:"schema_version"`
	Kind     string                        `json:"kind"`
	Revision string                        `json:"revision"`
	Tree     string                        `json:"tree"`
	Workflow workflowProvenance            `json:"workflow"`
	Started  time.Time                     `json:"started"`
	Finished time.Time                     `json:"finished"`
	Tests    map[string]packagedExecutable `json:"tests"`
	Complete bool                          `json:"complete"`
	Error    string                        `json:"error,omitempty"`
}

func currentBuildWorkflow(revision string) workflowProvenance {
	workflow := workflowProvenance{Event: os.Getenv("GITHUB_EVENT_NAME"), Ref: os.Getenv("GITHUB_REF"), Revision: revision, Job: os.Getenv("GITHUB_JOB")}
	workflow.Attempt, _ = strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if server, repo, id := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID"); server != "" && repo != "" && id != "" {
		workflow.RunURL = server + "/" + repo + "/actions/runs/" + id
	}
	return workflow
}

func validateTestObservation(observed executableObservation, pkg, revision, osName, arch string) error {
	if observed.Path != "github.com/zzet/gortex/"+pkg+".test" || observed.Module != "github.com/zzet/gortex" || observed.GoVersion != runtime.Version() || observed.Bytes <= 0 || !planPattern.MatchString(observed.SHA256) || observed.EmbeddedRevision == nil || *observed.EmbeddedRevision != revision || observed.EmbeddedModified == nil || *observed.EmbeddedModified != "false" || observed.Settings["vcs.revision"] != revision || observed.Settings["vcs.modified"] != "false" || observed.Settings["GOOS"] != osName || observed.Settings["GOARCH"] != arch || observed.Settings["CGO_ENABLED"] != "1" || observed.Settings["CGO_CFLAGS"] != "-O2 -g -fno-strict-aliasing" || observed.Settings["CGO_CXXFLAGS"] != "-O2 -g -fno-strict-aliasing" {
		return errors.New("benchmark executable lacks observed package/module/platform/toolchain/clean VCS/required build flags")
	}
	return nil
}

func prepareTestBinaries(ctx context.Context, source, revision, out string, process microProcess) error {
	if !revisionPattern.MatchString(revision) {
		return errors.New("test builds require an explicit full source revision")
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	receipt := testBuildReceipt{Version: 1, Kind: "test_build_only", Revision: revision, Workflow: currentBuildWorkflow(revision), Started: time.Now().UTC(), Tests: map[string]packagedExecutable{}}
	var log []byte
	git := func(args ...string) (string, error) {
		stdout, stderr, err := process(ctx, "git", args, source, os.Environ())
		log = append(log, stdout...)
		log = append(log, stderr...)
		return strings.TrimSpace(string(stdout)), err
	}
	checkSource := func() error {
		head, err := git("rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if head != revision {
			return errors.New("source checkout differs from pinned test-build revision")
		}
		dirty, err := git("status", "--porcelain", "--untracked-files=normal")
		if err != nil {
			return err
		}
		if dirty != "" {
			return errors.New("test-build source is modified or untracked")
		}
		return nil
	}
	err := checkSource()
	if err == nil {
		receipt.Tree, err = git("rev-parse", "HEAD^{tree}")
		if err == nil && !revisionPattern.MatchString(receipt.Tree) {
			err = errors.New("test-build source tree identity unavailable")
		}
	}
	if err == nil && (os.Getenv("CGO_ENABLED") != "1" || os.Getenv("CGO_CFLAGS") != "-O2 -g -fno-strict-aliasing" || os.Getenv("CGO_CXXFLAGS") != "-O2 -g -fno-strict-aliasing") {
		err = errors.New("test build requires the prescribed C/C++ flags and enabled cgo")
	}
	if err == nil && os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("GITHUB_SHA") != revision {
		err = errors.New("test source revision differs from current workflow commit")
	}
	if err == nil {
		for _, pkg := range microPackages {
			file := microTestName(pkg)
			args := []string{"test", "-c", "-buildvcs=true", "-mod=readonly", "-p=2", "-o", filepath.Join(out, file), "./" + pkg}
			stdout, stderr, buildErr := process(ctx, "go", args, source, append(os.Environ(), "GOTOOLCHAIN=local"))
			log = append(log, []byte("go "+strings.Join(args, " ")+"\n")...)
			log = append(log, stdout...)
			log = append(log, stderr...)
			if buildErr != nil {
				err = buildErr
				break
			}
			observed, observeErr := observeExecutable(filepath.Join(out, file))
			if observeErr != nil {
				err = observeErr
				break
			}
			if observeErr = validateTestObservation(observed, pkg, revision, runtime.GOOS, runtime.GOARCH); observeErr != nil {
				err = fmt.Errorf("%s: %w", pkg, observeErr)
				break
			}
			receipt.Tests[pkg] = packagedExecutable{File: file, Observed: observed}
		}
	}
	if err == nil {
		err = checkSource()
	}
	receipt.Finished = time.Now().UTC()
	receipt.Complete = err == nil
	if err != nil {
		receipt.Error = err.Error()
	}
	return errors.Join(err, os.WriteFile(filepath.Join(out, "test-build.log"), log, 0o600), writeNewJSON(filepath.Join(out, testBuildReceiptName), receipt))
}

func verifyPackagedExecutable(record packagedExecutable, data []byte) error {
	if record.Observed.Bytes <= 0 || !planPattern.MatchString(record.Observed.SHA256) || record.Observed.Bytes != int64(len(data)) || fmt.Sprintf("%x", sha256.Sum256(data)) != record.Observed.SHA256 {
		return errors.New("bundled executable bytes differ from observed hash/size")
	}
	return nil
}

func validateMicroBundle(metadata mainArtifact, files map[string][]byte) error {
	bundle := metadata.Microbench
	if bundle == nil {
		return nil
	}
	if bundle.Version != 1 || !revisionPattern.MatchString(bundle.FixtureRevision) || !revisionPattern.MatchString(bundle.FixtureTree) || !revisionPattern.MatchString(bundle.SourceTree) || len(bundle.Tests) != len(microPackages) {
		return errors.New("full main microbundle lacks pinned fixture/source tree or complete test inventory")
	}
	for _, pkg := range microPackages {
		record, ok := bundle.Tests[pkg]
		if !ok || record.File != microTestName(pkg) {
			return errors.New("main microbundle missing exact package/file identity")
		}
		if err := validateTestObservation(record.Observed, pkg, metadata.Revision, metadata.OS, metadata.Arch); err != nil {
			return err
		}
		if err := verifyPackagedExecutable(record, files[record.File]); err != nil {
			return err
		}
	}
	tool := bundle.Benchstat
	if tool.File != bundledBenchstatName || tool.Observed.Path != "golang.org/x/perf/cmd/benchstat" || tool.Observed.Module != "golang.org/x/perf" || tool.Observed.Version != benchstatVersion || tool.Observed.GoVersion != metadata.GoVersion || tool.Observed.Settings["GOOS"] != metadata.OS || tool.Observed.Settings["GOARCH"] != metadata.Arch {
		return errors.New("bundled benchstat is not the pinned tool/platform")
	}
	return verifyPackagedExecutable(tool, files[tool.File])
}

type microPublication struct {
	Tests   string
	Tool    string
	Fixture string
}

func knownBundleFile(name string) bool {
	if name == mainArtifactBinary || name == "main-artifact.json" || name == bundledBenchstatName {
		return true
	}
	for _, pkg := range microPackages {
		if name == microTestName(pkg) {
			return true
		}
	}
	return false
}

func writeNewExecutable(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	return errors.Join(err, file.Close())
}

func loadPublicationMicro(metadata *mainArtifact, options *microPublication) (map[string][]byte, error) {
	if options == nil {
		return nil, nil
	}
	var tests testBuildReceipt
	if err := readStrictJSON(filepath.Join(options.Tests, testBuildReceiptName), &tests); err != nil {
		return nil, err
	}
	if tests.Version != 1 || tests.Kind != "test_build_only" || !tests.Complete || tests.Error != "" || tests.Revision != metadata.Revision || !revisionPattern.MatchString(tests.Tree) || tests.Workflow != currentBuildWorkflow(metadata.Revision) || len(tests.Tests) != len(microPackages) {
		return nil, errors.New("prepared main tests lack exact clean source/workflow build provenance")
	}
	var fixture fixtureSetup
	if err := readStrictJSON(filepath.Join(options.Fixture, "fixture.json"), &fixture); err != nil {
		return nil, err
	}
	if fixture.Version != 1 || fixture.Kind != "fixture_setup_only" || !fixture.Complete || fixture.Error != "" || fixture.Repository != fixtureRepository || !revisionPattern.MatchString(fixture.Revision) || !revisionPattern.MatchString(fixture.Tree) {
		return nil, errors.New("prepared fixture lacks fixed-repository commit/tree provenance")
	}
	bundle := &mainMicroBundle{Version: 1, FixtureRevision: fixture.Revision, FixtureTree: fixture.Tree, SourceTree: tests.Tree, Tests: tests.Tests}
	files := map[string][]byte{}
	for _, pkg := range microPackages {
		record, ok := bundle.Tests[pkg]
		if !ok || record.File != microTestName(pkg) {
			return nil, errors.New("prepared test package/file census differs")
		}
		data, err := os.ReadFile(filepath.Join(options.Tests, record.File))
		if err != nil {
			return nil, err
		}
		files[record.File] = data
	}
	if err := checkBenchstat(options.Tool); err != nil {
		return nil, err
	}
	observed, err := observeExecutable(options.Tool)
	if err != nil {
		return nil, err
	}
	bundle.Benchstat = packagedExecutable{File: bundledBenchstatName, Observed: observed}
	files[bundledBenchstatName], err = os.ReadFile(options.Tool)
	if err != nil {
		return nil, err
	}
	metadata.Microbench = bundle
	if err := validateMicroBundle(*metadata, files); err != nil {
		return nil, err
	}
	return files, nil
}

func sameTestSettings(a, b executableObservation) bool {
	main := mainArtifact{OS: a.Settings["GOOS"], Arch: a.Settings["GOARCH"], GoVersion: a.GoVersion, Settings: a.Settings}
	candidate := mainArtifact{OS: b.Settings["GOOS"], Arch: b.Settings["GOARCH"], GoVersion: b.GoVersion, Settings: b.Settings}
	return sameBuildSettings(main, candidate)
}

func sameExecutablePath(actual, expected string) bool {
	a, err := filepath.EvalSymlinks(actual)
	if err != nil {
		return false
	}
	b, err := filepath.EvalSymlinks(expected)
	return err == nil && a == b
}

func admitMicroBundle(c microConfig, workflow workflowProvenance) (map[string]map[string]executableObservation, error) {
	_, metadata, err := verifiedMainBundle(c.MainBundle)
	if err != nil {
		return nil, err
	}
	bundle := metadata.Microbench
	if bundle == nil || metadata.Revision != c.MainRevision || bundle.FixtureRevision != c.FixtureRevision {
		return nil, errors.New("microbench requires the full exact-main executable bundle and its pinned fixture")
	}
	var candidate testBuildReceipt
	if err := readStrictJSON(filepath.Join(c.CandidateBuilds, testBuildReceiptName), &candidate); err != nil {
		return nil, err
	}
	if candidate.Version != 1 || candidate.Kind != "test_build_only" || !candidate.Complete || candidate.Error != "" || candidate.Revision != c.CandidateRevision || !revisionPattern.MatchString(candidate.Tree) || candidate.Workflow != workflow || len(candidate.Tests) != len(microPackages) {
		return nil, errors.New("candidate test executables lack complete same-job source/build provenance")
	}
	if !sameExecutablePath(c.Benchstat, filepath.Join(c.MainBundle, bundledBenchstatName)) {
		return nil, errors.New("microbench tool is not the downloaded pinned benchstat")
	}
	tree, err := exec.Command("git", "-C", c.Fixture, "rev-parse", "HEAD^{tree}").Output()
	if err != nil || strings.TrimSpace(string(tree)) != bundle.FixtureTree {
		return nil, errors.New("runtime fixture tree differs from the trusted pinned tree")
	}
	builds := map[string]map[string]executableObservation{}
	for _, pkg := range microPackages {
		mainRecord := bundle.Tests[pkg]
		candidateRecord, ok := candidate.Tests[pkg]
		pair := c.Binaries[pkg]
		if !ok || candidateRecord.File != microTestName(pkg) || !sameExecutablePath(pair.Main, filepath.Join(c.MainBundle, microTestName(pkg))) || !sameExecutablePath(pair.Candidate, filepath.Join(c.CandidateBuilds, microTestName(pkg))) {
			return nil, errors.New("benchmark paths differ from the trusted main/current-job executable inventory")
		}
		mainObserved, err := observeExecutable(pair.Main)
		if err != nil {
			return nil, err
		}
		candidateObserved, err := observeExecutable(pair.Candidate)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(mainObserved, mainRecord.Observed) || !reflect.DeepEqual(candidateObserved, candidateRecord.Observed) {
			return nil, errors.New("benchmark executable changed after its observed build/download")
		}
		if err := validateTestObservation(candidateObserved, pkg, c.CandidateRevision, runtime.GOOS, runtime.GOARCH); err != nil {
			return nil, err
		}
		if !sameTestSettings(mainObserved, candidateObserved) {
			return nil, errors.New("main/candidate benchmark build settings differ")
		}
		builds[pkg] = map[string]executableObservation{"main": mainObserved, "candidate": candidateObserved}
	}
	return builds, nil
}
