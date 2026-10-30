package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const mainArtifactWorkflow = ".github/workflows/perf-main.yml"
const mainArtifactBinary = "gortex"
const maxArtifactBytes = 1024 * 1024 * 1024

type mainArtifact struct {
	Version      int               `json:"schema_version"`
	Repository   string            `json:"repository"`
	Revision     string            `json:"revision"`
	Workflow     string            `json:"workflow"`
	RunID        int64             `json:"run_id"`
	Attempt      int               `json:"run_attempt"`
	Event        string            `json:"event"`
	Ref          string            `json:"ref"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	GoVersion    string            `json:"go_version"`
	BinarySHA256 string            `json:"binary_sha256"`
	Settings     map[string]string `json:"build_settings"`
}

type githubRun struct {
	ID         int64  `json:"id"`
	Attempt    int    `json:"run_attempt"`
	SHA        string `json:"head_sha"`
	Branch     string `json:"head_branch"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Path       string `json:"path"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

type githubArtifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Expired bool   `json:"expired"`
	Digest  string `json:"digest"`
	Run     struct {
		ID     int64  `json:"id"`
		SHA    string `json:"head_sha"`
		Branch string `json:"head_branch"`
	} `json:"workflow_run"`
}

type artifactSelection struct {
	Version    int            `json:"schema_version"`
	Repository string         `json:"repository"`
	Revision   string         `json:"revision"`
	OS         string         `json:"os"`
	Arch       string         `json:"arch"`
	Run        githubRun      `json:"run"`
	Artifact   githubArtifact `json:"artifact"`
}

type githubClient struct {
	base   string
	token  string
	client *http.Client
}

func newGithubClient(token string) githubClient {
	return githubClient{base: "https://api.github.com", token: token, client: &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return errors.New("too many artifact redirects")
		}
		if req.URL.Scheme != "https" {
			return errors.New("artifact redirect must remain HTTPS")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}}}
}

func (g githubClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	response, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub %s returned HTTP %d", path, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxArtifactBytes {
		return nil, errors.New("artifact response exceeds one GiB")
	}
	return data, nil
}

func artifactName(revision, osName, arch string) string {
	return "perf-main-" + osName + "-" + arch + "-" + revision
}

func validateMainRun(run githubRun, repo, revision string) error {
	if run.ID <= 0 || run.Attempt < 1 || run.SHA != revision || run.Branch != "main" || run.Event != "push" || run.Status != "completed" || run.Conclusion != "success" || run.Path != mainArtifactWorkflow || run.Repository.FullName != repo || run.HeadRepository.FullName != repo {
		return errors.New("artifact run is not a successful exact-revision trusted-main push from the dedicated producer")
	}
	return nil
}

func selectMainArtifact(repo, revision, osName, arch string, runs []githubRun, artifacts []githubArtifact) (artifactSelection, error) {
	selection := artifactSelection{Version: 1, Repository: repo, Revision: revision, OS: osName, Arch: arch}
	// No older-green fallback and no arbitrary run-name match. An exact SHA may
	// have reruns: the greatest successful attempt/run must have exactly one bundle.
	for _, run := range runs {
		if validateMainRun(run, repo, revision) == nil && (run.ID > selection.Run.ID || run.ID == selection.Run.ID && run.Attempt > selection.Run.Attempt) {
			selection.Run = run
		}
	}
	if selection.Run.ID == 0 {
		return selection, errors.New("no successful exact-main artifact producer run; do not fall back to another revision")
	}
	found := 0
	for _, artifact := range artifacts {
		if artifact.Run.ID != selection.Run.ID || artifact.Name != artifactName(revision, osName, arch) {
			continue
		}
		if artifact.Expired || artifact.Run.SHA != revision || artifact.Run.Branch != "main" || !validDigest(artifact.Digest) {
			return selection, errors.New("main artifact expired or lacks exact-run/revision/digest provenance")
		}
		selection.Artifact = artifact
		found++
	}
	if found != 1 {
		return selection, errors.New("need exactly one nonexpired exact-main platform bundle")
	}
	return selection, nil
}

func validDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && planPattern.MatchString(strings.TrimPrefix(value, "sha256:"))
}

func (g githubClient) lookupMain(ctx context.Context, repo, revision, osName, arch string) (artifactSelection, error) {
	var runs []githubRun
	for page := 1; ; page++ {
		path := fmt.Sprintf("/repos/%s/actions/workflows/perf-main.yml/runs?branch=main&event=push&head_sha=%s&per_page=100&page=%d", repo, revision, page)
		data, err := g.get(ctx, path)
		if err != nil {
			return artifactSelection{}, err
		}
		var response struct {
			Runs []githubRun `json:"workflow_runs"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return artifactSelection{}, err
		}
		runs = append(runs, response.Runs...)
		if len(response.Runs) < 100 {
			break
		}
	}
	chosen, err := selectMainArtifact(repo, revision, osName, arch, runs, nil)
	// Selection without artifact inventory must still identify the newest trusted run.
	if chosen.Run.ID == 0 {
		return chosen, err
	}
	var artifacts []githubArtifact
	for page := 1; ; page++ {
		data, err := g.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d/artifacts?per_page=100&page=%d", repo, chosen.Run.ID, page))
		if err != nil {
			return chosen, err
		}
		var response struct {
			Artifacts []githubArtifact `json:"artifacts"`
		}
		if err := json.Unmarshal(data, &response); err != nil {
			return chosen, err
		}
		artifacts = append(artifacts, response.Artifacts...)
		if len(response.Artifacts) < 100 {
			break
		}
	}
	return selectMainArtifact(repo, revision, osName, arch, runs, artifacts)
}

func binaryArtifact(path string) (mainArtifact, error) {
	return inspectBinaryArtifact(path, "github.com/zzet/gortex/cmd/gortex")
}

func inspectBinaryArtifact(path, importPath string) (mainArtifact, error) {
	var metadata mainArtifact
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return metadata, err
	}
	if info.Path != importPath || info.GoVersion != runtime.Version() {
		return metadata, errors.New("artifact must be the Gortex daemon built with this pinned Go toolchain")
	}
	metadata.Version = 1
	metadata.GoVersion = info.GoVersion
	metadata.Settings = map[string]string{}
	for _, setting := range info.Settings {
		metadata.Settings[setting.Key] = setting.Value
	}
	metadata.Revision = metadata.Settings["vcs.revision"]
	metadata.OS = metadata.Settings["GOOS"]
	metadata.Arch = metadata.Settings["GOARCH"]
	if !revisionPattern.MatchString(metadata.Revision) || metadata.Settings["vcs.modified"] != "false" || metadata.Settings["CGO_ENABLED"] != "1" || metadata.Settings["CGO_CFLAGS"] != "-O2 -g -fno-strict-aliasing" || metadata.Settings["CGO_CXXFLAGS"] != "-O2 -g -fno-strict-aliasing" {
		return metadata, errors.New("main artifact requires clean VCS provenance and pinned C/C++ aliasing flags")
	}
	file, err := os.Open(path)
	if err != nil {
		return metadata, err
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	err = errors.Join(err, file.Close())
	if err != nil {
		return metadata, err
	}
	metadata.BinarySHA256 = fmt.Sprintf("%x", hash.Sum(nil))
	return metadata, nil
}

func publishMainArtifact(binary, out string) error {
	metadata, err := binaryArtifact(binary)
	if err != nil {
		return err
	}
	metadata.Repository = os.Getenv("GITHUB_REPOSITORY")
	metadata.Event = os.Getenv("GITHUB_EVENT_NAME")
	metadata.Ref = os.Getenv("GITHUB_REF")
	metadata.Workflow = mainArtifactWorkflow
	metadata.RunID, _ = strconv.ParseInt(os.Getenv("GITHUB_RUN_ID"), 10, 64)
	metadata.Attempt, _ = strconv.Atoi(os.Getenv("GITHUB_RUN_ATTEMPT"))
	if metadata.Repository != "zzet/gortex" || metadata.Event != "push" || metadata.Ref != "refs/heads/main" || metadata.RunID < 1 || metadata.Attempt < 1 || os.Getenv("GITHUB_SHA") != metadata.Revision || os.Getenv("GITHUB_WORKFLOW_SHA") != metadata.Revision || os.Getenv("GITHUB_WORKFLOW_REF") != metadata.Repository+"/"+mainArtifactWorkflow+"@refs/heads/main" {
		return errors.New("publication requires dedicated exact-revision trusted-main push workflow provenance")
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	source, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(filepath.Join(out, mainArtifactBinary), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	_, err = io.Copy(target, source)
	err = errors.Join(err, target.Close())
	if err != nil {
		return err
	}
	return writeNewJSON(filepath.Join(out, "main-artifact.json"), metadata)
}

func extractMainArtifact(selection artifactSelection, archive []byte, out string) error {
	if !validDigest(selection.Artifact.Digest) || fmt.Sprintf("sha256:%x", sha256.Sum256(archive)) != selection.Artifact.Digest {
		return errors.New("downloaded archive does not match GitHub artifact SHA256")
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return err
	}
	files := map[string][]byte{}
	for _, entry := range reader.File {
		if (entry.Name != "main-artifact.json" && entry.Name != mainArtifactBinary) || entry.Mode()&os.ModeType != 0 || entry.UncompressedSize64 > maxArtifactBytes || files[entry.Name] != nil {
			return errors.New("bundle has duplicate, nonregular, oversized or unexpected file")
		}
		file, err := entry.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
		if len(data) > maxArtifactBytes {
			return errors.New("expanded artifact exceeds limit")
		}
		files[entry.Name] = data
	}
	if len(files) != 2 {
		return errors.New("incomplete daemon artifact bundle")
	}
	var metadata mainArtifact
	d := json.NewDecoder(bytes.NewReader(files["main-artifact.json"]))
	d.DisallowUnknownFields()
	if err := d.Decode(&metadata); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("artifact metadata contains trailing JSON")
	}
	if err := verifyMainArtifact(selection, metadata, files[mainArtifactBinary]); err != nil {
		return err
	}
	if err := os.Mkdir(out, 0o700); err != nil {
		return err
	}
	for name, data := range files {
		mode := os.FileMode(0o600)
		if name == mainArtifactBinary {
			mode = 0o700
		}
		if err := os.WriteFile(filepath.Join(out, name), data, mode); err != nil {
			return err
		}
	}
	return writeNewJSON(filepath.Join(out, "selection.json"), selection)
}

func verifyMainArtifact(s artifactSelection, m mainArtifact, binary []byte) error {
	if err := validateMainRun(s.Run, s.Repository, s.Revision); err != nil {
		return err
	}
	if s.Version != 1 || s.Artifact.ID < 1 || s.Artifact.Expired || s.Artifact.Name != artifactName(s.Revision, s.OS, s.Arch) || s.Artifact.Run.ID != s.Run.ID || s.Artifact.Run.SHA != s.Revision || s.Artifact.Run.Branch != "main" || !validDigest(s.Artifact.Digest) || m.Version != 1 || m.Repository != s.Repository || m.Revision != s.Revision || m.RunID != s.Run.ID || m.Attempt != s.Run.Attempt || m.Event != "push" || m.Ref != "refs/heads/main" || m.Workflow != mainArtifactWorkflow || m.OS != s.OS || m.Arch != s.Arch || m.GoVersion != runtime.Version() || m.Settings["GOOS"] != m.OS || m.Settings["GOARCH"] != m.Arch || m.Settings["vcs.revision"] != m.Revision || m.Settings["vcs.modified"] != "false" || m.Settings["CGO_ENABLED"] != "1" || m.Settings["CGO_CFLAGS"] != "-O2 -g -fno-strict-aliasing" || m.Settings["CGO_CXXFLAGS"] != "-O2 -g -fno-strict-aliasing" || fmt.Sprintf("%x", sha256.Sum256(binary)) != m.BinarySHA256 {
		return errors.New("downloaded bundle provenance/platform/toolchain/binary hash differs from selected trusted main run")
	}
	return nil
}

func artifactCommand(command string, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(stderr)
	binary := f.String("binary", "", "clean exact-main daemon binary for publication")
	out := f.String("out", "", "NEW external bundle directory")
	revision := f.String("main-revision", "", "exact main SHA pinned at PR planning time; no older-green fallback")
	repo := f.String("repository", "zzet/gortex", "trusted repository")
	osName := f.String("os", "linux", "required binary OS")
	arch := f.String("arch", "arm64", "required binary architecture")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *out == "" || f.NArg() != 0 || command == "main-artifact" && *binary == "" || command == "fetch-main" && (!revisionPattern.MatchString(*revision) || *repo != "zzet/gortex" || *osName != "linux" || *arch != "arm64") {
		fmt.Fprintln(stderr, "publication needs --binary/--out; fetch needs exact --main-revision/--out and reviewed linux/arm64 repository")
		return 2
	}
	var err error
	if command == "main-artifact" {
		err = publishMainArtifact(*binary, *out)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		client := newGithubClient(os.Getenv("GH_TOKEN"))
		var selection artifactSelection
		selection, err = client.lookupMain(ctx, *repo, *revision, *osName, *arch)
		if err == nil {
			var archive []byte
			archive, err = client.get(ctx, fmt.Sprintf("/repos/%s/actions/artifacts/%d/zip", *repo, selection.Artifact.ID))
			if err == nil {
				err = extractMainArtifact(selection, archive, *out)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "retained exact-main bundle:", *out)
	return 0
}
