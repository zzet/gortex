package main

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const fixtureRepository = "https://github.com/zzet/gortex.git"

type fixtureSetup struct {
	Version    int       `json:"schema_version"`
	Kind       string    `json:"kind"`
	Repository string    `json:"repository"`
	Revision   string    `json:"revision"`
	Tree       string    `json:"tree"`
	GitVersion string    `json:"git_version"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished"`
	Complete   bool      `json:"complete"`
	Error      string    `json:"error,omitempty"`
}

type executableObservation struct {
	Path      string            `json:"go_package_path"`
	Module    string            `json:"go_module_path"`
	Version   string            `json:"go_module_version"`
	GoVersion string            `json:"go_version"`
	SHA256    string            `json:"sha256"`
	Bytes     int64             `json:"bytes"`
	Settings  map[string]string `json:"build_settings"`
	// No daemon-derived defaults: absence remains an explicit observation.
	EmbeddedRevision *string `json:"embedded_vcs_revision"`
	EmbeddedModified *string `json:"embedded_vcs_modified"`
}

func observeExecutable(path string) (executableObservation, error) {
	var observed executableObservation
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return observed, err
	}
	observed.Path, observed.Module, observed.Version, observed.GoVersion = info.Path, info.Main.Path, info.Main.Version, info.GoVersion
	observed.Settings = map[string]string{}
	for _, setting := range info.Settings {
		observed.Settings[setting.Key] = setting.Value
	}
	if value, present := observed.Settings["vcs.revision"]; present {
		observed.EmbeddedRevision = &value
	}
	if value, present := observed.Settings["vcs.modified"]; present {
		observed.EmbeddedModified = &value
	}
	file, err := os.Open(path)
	if err != nil {
		return observed, err
	}
	hash := sha256.New()
	observed.Bytes, err = io.Copy(hash, file)
	err = errors.Join(err, file.Close())
	if err != nil {
		return observed, err
	}
	observed.SHA256 = fmt.Sprintf("%x", hash.Sum(nil))
	return observed, nil
}

func prepareFixture(ctx context.Context, revision, out string, process microProcess) error {
	if !revisionPattern.MatchString(revision) {
		return errors.New("fixture needs an explicit full reviewed revision; no moving-branch default")
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	receipt := fixtureSetup{Version: 1, Kind: "fixture_setup_only", Repository: fixtureRepository, Revision: revision, Started: time.Now().UTC()}
	repo := filepath.Join(out, "repo")
	err := os.Mkdir(repo, 0o700)
	var log []byte
	run := func(args ...string) (string, error) {
		stdout, stderr, err := process(ctx, "git", args, repo, os.Environ())
		log = append(log, []byte("git "+strings.Join(args, " ")+"\n")...)
		log = append(log, stdout...)
		log = append(log, stderr...)
		return strings.TrimSpace(string(stdout)), err
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"remote", "add", "origin", fixtureRepository}, {"fetch", "--quiet", "--depth=1", "--no-tags", "origin", revision}, {"checkout", "--quiet", "--detach", "FETCH_HEAD"}} {
		if err != nil {
			break
		}
		_, err = run(args...)
	}
	if err == nil {
		var actual string
		actual, err = run("rev-parse", "HEAD")
		if err == nil && actual != revision {
			err = errors.New("fetched fixture HEAD differs from reviewed revision")
		}
	}
	if err == nil {
		receipt.Tree, err = run("rev-parse", "HEAD^{tree}")
		if err == nil && !revisionPattern.MatchString(receipt.Tree) {
			err = errors.New("fixture tree identity missing")
		}
	}
	if err == nil {
		var dirty string
		dirty, err = run("status", "--porcelain", "--untracked-files=normal")
		if err == nil && dirty != "" {
			err = errors.New("new fixture is modified or untracked")
		}
	}
	if err == nil {
		receipt.GitVersion, err = run("--version")
	}
	receipt.Finished = time.Now().UTC()
	receipt.Complete = err == nil
	if err != nil {
		receipt.Error = err.Error()
	}
	return errors.Join(err, os.WriteFile(filepath.Join(out, "fixture-setup.log"), log, 0o600), writeNewJSON(filepath.Join(out, "fixture.json"), receipt))
}

type pinnedToolSetup struct {
	Version    int                   `json:"schema_version"`
	Kind       string                `json:"kind"`
	ModulePin  string                `json:"module_pin"`
	Executable executableObservation `json:"executable"`
	Complete   bool                  `json:"complete"`
	Error      string                `json:"error,omitempty"`
}

func prepareBenchstat(ctx context.Context, out string, process microProcess) error {
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	receipt := pinnedToolSetup{Version: 1, Kind: "tool_setup_only", ModulePin: "golang.org/x/perf/cmd/benchstat@" + benchstatVersion}
	env := append(os.Environ(), "GOBIN="+out, "GOTOOLCHAIN=local")
	stdout, stderr, err := process(ctx, "go", []string{"install", receipt.ModulePin}, out, env)
	binary := filepath.Join(out, "benchstat")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if err == nil {
		err = checkBenchstat(binary)
	}
	if err == nil {
		receipt.Executable, err = observeExecutable(binary)
	}
	receipt.Complete = err == nil
	if err != nil {
		receipt.Error = err.Error()
	}
	return errors.Join(err, os.WriteFile(filepath.Join(out, "tool.stdout.txt"), stdout, 0o600), os.WriteFile(filepath.Join(out, "tool.stderr.txt"), stderr, 0o600), writeNewJSON(filepath.Join(out, "tool.json"), receipt))
}

func setupMicroCommand(command string, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(stderr)
	out := f.String("out", "", "NEW owned external setup directory")
	revision := f.String("revision", "", "full reviewed fixture revision")
	source := f.String("source", ".", "clean source checkout for prepared test executables")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *out == "" || f.NArg() != 0 || (command == "prepare-fixture" || command == "prepare-tests") && !revisionPattern.MatchString(*revision) {
		fmt.Fprintln(stderr, "setup requires --out; fixture also requires explicit full --revision")
		return 2
	}
	signals, cancelSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelSignals()
	ctx, cancel := context.WithTimeout(signals, 5*time.Minute)
	defer cancel()
	output := pathAbsolute(*out)
	var err error
	if command == "prepare-fixture" {
		err = prepareFixture(ctx, *revision, output, executeMicroProcess)
	} else if command == "prepare-tests" {
		err = prepareTestBinaries(ctx, pathAbsolute(*source), *revision, output, executeMicroProcess)
	} else {
		err = prepareBenchstat(ctx, output, executeMicroProcess)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "owned setup retained; no workload acceptance claimed:", output)
	return 0
}
