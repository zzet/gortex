package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/platform"
)

func TestSharedOwnerBindingChildProcess(t *testing.T) {
	if os.Getenv("GORTEX_SHARED_OWNER_BINDING_CHILD") != "1" {
		return
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

// The child is real and owned; its startup record is a controlled fixture.
// This tests report-arm binding, not a live Server's publication membership.
func TestSharedOwnerBindingRequiresActualChildStartupAndArm(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	statePath := filepath.Join(cache, "gortex", "daemon.state.json")
	repo := filepath.Join(root, "repos", "primary")
	for _, path := range []string{filepath.Dir(statePath), repo} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(exe, "-test.run=^TestSharedOwnerBindingChildProcess$", "-test.timeout=15s")
	configureOwnedProcess(command)
	command.Env = append(privateEnv(root, filepath.Join(root, "unused.sock"), filepath.Join(root, "gitconfig")), "GORTEX_SHARED_OWNER_BINDING_CHILD=1")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var exitErr error
	go func() { exitErr = command.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = input.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("owned binding child did not stop after input close")
			_ = killOwnedProcess(command)
			<-done
		}
		if exitErr != nil {
			t.Errorf("owned binding child: %v %s", exitErr, output.String())
		}
	})
	identity, err := platform.CaptureProcessIdentity(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	birth, err := identity.Token()
	if err != nil {
		t.Fatal(err)
	}
	p := &privateRun{Root: root, Binary: exe, Store: filepath.Join(root, "owned.db"), process: command, done: done, startCount: 1, Repos: []privateRepo{{Path: repo, Prefix: "primary"}}}
	state := daemon.RuntimeState{PID: command.Process.Pid, ProcessBirthToken: birth, BackendPath: p.Store, StartupPhase: daemon.StartupServing}
	writeState := func(value daemon.RuntimeState) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeState(state)
	binding, err := captureEditProbeSharedBinding(t.Context(), p, "candidate")
	if err != nil {
		t.Fatalf("actual child with matching startup record rejected: %v", err)
	}
	if binding.identity.PID() != command.Process.Pid || binding.birth != birth || binding.root != repo || binding.prefix != "primary" {
		t.Fatalf("binding did not retain the actual child/checkout: %+v", binding)
	}
	for _, tc := range []struct {
		name   string
		change func(*daemon.RuntimeState)
	}{
		{"another PID", func(s *daemon.RuntimeState) { s.PID++ }},
		{"another birth", func(s *daemon.RuntimeState) { s.ProcessBirthToken += "-other" }},
		{"another store", func(s *daemon.RuntimeState) { s.BackendPath += "-other" }},
		{"unready child", func(s *daemon.RuntimeState) { s.StartupPhase = daemon.StartupMigrating }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := state
			tc.change(&changed)
			writeState(changed)
			if _, err := captureEditProbeSharedBinding(t.Context(), p, "candidate"); err == nil {
				t.Fatal("foreign/stale startup record supplied an owned daemon domain")
			}
			writeState(state)
		})
	}
	if _, err := captureEditProbeSharedBinding(t.Context(), p, "baseline"); err == nil {
		t.Fatal("a role label changed the actual candidate arm")
	}
	if _, err := captureEditProbeSharedBinding(t.Context(), nil, "candidate"); err == nil {
		t.Fatal("missing owned child acquired shared authority")
	}
	for _, tc := range []struct {
		name   string
		change func(*privateRun)
	}{
		{"new start generation", func(p *privateRun) { p.startCount++ }},
		{"different executable", func(p *privateRun) { p.Binary += "-other" }},
		{"different store", func(p *privateRun) { p.Store += "-other" }},
		{"different child lifetime", func(p *privateRun) { p.done = make(chan struct{}) }},
		{"lookalike command is not the owned command", func(p *privateRun) {
			p.process = &exec.Cmd{Path: p.process.Path, Process: p.process.Process, Env: p.process.Env}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := *p
			tc.change(p)
			if err := binding.validate(p, "candidate"); err == nil {
				t.Fatal("a changed run identity reused an older live binding")
			}
			*p = original
		})
	}
	if err := binding.validate(p, "candidate"); err != nil {
		t.Fatalf("negative controls changed the original binding: %v", err)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := binding.validate(p, "candidate"); err == nil {
		t.Fatal("retained metadata revived an exited owned child")
	}
}
