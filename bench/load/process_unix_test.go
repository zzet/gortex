//go:build unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestOwnedProcessFixture(t *testing.T) {
	mode := os.Getenv("LOAD_PROCESS_FIXTURE")
	if mode == "" {
		return
	}
	path := os.Getenv("LOAD_HEARTBEAT")
	if mode == "child" {
		for i := 0; ; i++ {
			if err := os.WriteFile(path, fmt.Appendf(nil, "%d", i), 0o600); err != nil {
				os.Exit(3)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(4)
	}
	child := exec.Command(exe, "-test.run=^TestOwnedProcessFixture$")
	child.Env = append(os.Environ(), "LOAD_PROCESS_FIXTURE=child")
	if child.Start() != nil {
		os.Exit(5)
	}
	if mode == "exit" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestOwnedProcessGroupCleansDescendants(t *testing.T) {
	for _, mode := range []string{"blocked", "exit"} {
		t.Run(mode, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "heartbeat")
			cmd := exec.Command(exe, "-test.run=^TestOwnedProcessFixture$")
			cmd.Env = append(os.Environ(), "LOAD_PROCESS_FIXTURE="+mode, "LOAD_HEARTBEAT="+path)
			configureOwnedProcess(cmd)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				_ = killOwnedProcess(cmd)
				if !waited {
					_ = cmd.Wait()
				}
			})
			until := time.Now().Add(10 * time.Second)
			for {
				if data, _ := os.ReadFile(path); len(data) > 0 {
					break
				}
				if time.Now().After(until) {
					t.Fatal("owned child did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if mode == "exit" {
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
				waited = true
			}
			if err := killOwnedProcess(cmd); err != nil {
				t.Fatal(err)
			}
			if !waited {
				_ = cmd.Wait()
				waited = true
			}
			time.Sleep(50 * time.Millisecond)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatalf("owned descendant survived parent cleanup: %s -> %s %v", before, after, err)
			}
		})
	}
}
