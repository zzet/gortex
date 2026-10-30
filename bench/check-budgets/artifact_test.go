package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func syntheticMainArtifact() (artifactSelection, mainArtifact, []byte) {
	revision := strings.Repeat("a", 40)
	run := githubRun{ID: 123, Attempt: 2, SHA: revision, Branch: "main", Event: "push", Status: "completed", Conclusion: "success", Path: mainArtifactWorkflow}
	run.Repository.FullName = "zzet/gortex"
	run.HeadRepository.FullName = "zzet/gortex"
	artifact := githubArtifact{ID: 456, Name: artifactName(revision, "linux", "arm64"), Digest: "sha256:" + strings.Repeat("b", 64)}
	artifact.Run.ID = run.ID
	artifact.Run.SHA = revision
	artifact.Run.Branch = "main"
	selection := artifactSelection{Version: 1, Repository: "zzet/gortex", Revision: revision, OS: "linux", Arch: "arm64", Run: run, Artifact: artifact}
	binary := []byte("synthetic daemon bytes: not real benchmark evidence")
	metadata := mainArtifact{Version: 1, Repository: selection.Repository, Revision: revision, Workflow: mainArtifactWorkflow, RunID: run.ID, Attempt: run.Attempt, Event: "push", Ref: "refs/heads/main", OS: "linux", Arch: "arm64", GoVersion: runtime.Version(), BinarySHA256: fmt.Sprintf("%x", sha256.Sum256(binary)), Settings: map[string]string{"vcs.revision": revision, "vcs.modified": "false", "GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "1", "CGO_CFLAGS": "-O2 -g -fno-strict-aliasing", "CGO_CXXFLAGS": "-O2 -g -fno-strict-aliasing"}}
	return selection, metadata, binary
}

func TestMainArtifactSelectionRefusesUntrustedOrOlderEvidence(t *testing.T) {
	selection, _, _ := syntheticMainArtifact()
	if _, err := selectMainArtifact(selection.Repository, selection.Revision, "linux", "arm64", []githubRun{selection.Run}, []githubArtifact{selection.Artifact}); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"older-main", "pull-request", "fork", "wrong-workflow", "incomplete", "failed", "expired", "wrong-run", "wrong-arch", "missing-digest", "duplicate"} {
		t.Run(mutation, func(t *testing.T) {
			s, _, _ := syntheticMainArtifact()
			artifacts := []githubArtifact{s.Artifact}
			switch mutation {
			case "older-main":
				s.Run.SHA = strings.Repeat("c", 40)
			case "pull-request":
				s.Run.Event = "pull_request"
			case "fork":
				s.Run.HeadRepository.FullName = "example/fork"
			case "wrong-workflow":
				s.Run.Path = ".github/workflows/ci.yml"
			case "incomplete":
				s.Run.Status = "in_progress"
			case "failed":
				s.Run.Conclusion = "failure"
			case "expired":
				artifacts[0].Expired = true
			case "wrong-run":
				artifacts[0].Run.ID++
			case "wrong-arch":
				artifacts[0].Name = artifactName(s.Revision, "linux", "amd64")
			case "missing-digest":
				artifacts[0].Digest = ""
			case "duplicate":
				artifacts = append(artifacts, s.Artifact)
			}
			if _, err := selectMainArtifact(s.Repository, s.Revision, "linux", "arm64", []githubRun{s.Run}, artifacts); err == nil {
				t.Fatal("incomparable/untrusted artifact selected")
			}
		})
	}
	// A missing bundle on a newer successful producer must not cause fallback.
	newer := selection.Run
	newer.ID++
	if _, err := selectMainArtifact(selection.Repository, selection.Revision, "linux", "arm64", []githubRun{selection.Run, newer}, []githubArtifact{selection.Artifact}); err == nil {
		t.Fatal("fell back to older run without explicitly matching newest bundle")
	}
}

func TestMainArtifactExtractionChecksDigestIdentityAndSafeFiles(t *testing.T) {
	makeArchive := func(t *testing.T, metadata mainArtifact, binary []byte, extra string) []byte {
		t.Helper()
		var buffer bytes.Buffer
		writer := zip.NewWriter(&buffer)
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		for name, contents := range map[string][]byte{"main-artifact.json": data, mainArtifactBinary: binary} {
			file, err := writer.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(contents); err != nil {
				t.Fatal(err)
			}
		}
		if extra != "" {
			if _, err := writer.Create(extra); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	for _, mutation := range []string{"", "bad-archive-hash", "binary-tamper", "wrong-attempt", "wrong-platform", "dirty", "unapproved-flags", "zip-traversal", "extra-file"} {
		t.Run(mutation, func(t *testing.T) {
			selection, metadata, binary := syntheticMainArtifact()
			extra := ""
			switch mutation {
			case "binary-tamper":
				binary = append(binary, 'x')
			case "wrong-attempt":
				metadata.Attempt++
			case "wrong-platform":
				metadata.Arch = "amd64"
			case "dirty":
				metadata.Settings["vcs.modified"] = "true"
			case "unapproved-flags":
				metadata.Settings["CGO_CFLAGS"] = "-O2"
			case "zip-traversal":
				extra = "../escape"
			case "extra-file":
				extra = "unexpected"
			}
			archive := makeArchive(t, metadata, binary, extra)
			selection.Artifact.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
			if mutation == "bad-archive-hash" {
				selection.Artifact.Digest = "sha256:" + strings.Repeat("0", 64)
			}
			out := filepath.Join(t.TempDir(), "bundle")
			err := extractMainArtifact(selection, archive, out)
			if mutation != "" {
				if err == nil {
					t.Fatal("tampered or unsafe bundle accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(out, mainArtifactBinary))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o700 {
				t.Fatal("download lost executable/owned-only mode")
			}
			if err := extractMainArtifact(selection, archive, out); err == nil {
				t.Fatal("existing downloaded bundle overwritten")
			}
		})
	}
}
