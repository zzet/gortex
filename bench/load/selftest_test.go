package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("LOAD_DAEMON_FIXTURE"); mode != "" {
		os.Exit(fixtureDaemon(mode))
	}
	os.Exit(m.Run())
}

// This subprocess consumes the same owned global config and wire handshake as
// a real daemon. It proves the whole lifecycle/gate/artifact path without an
// application build, grammar bundle, network service, or existing daemon.
func fixtureDaemon(mode string) int {
	if mode == "crash" {
		fmt.Fprintln(os.Stderr, "panic: synthetic daemon crash")
		return 2
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "gortex", "config.yaml"))
	if err != nil {
		return 3
	}
	var config struct {
		Repos []struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"repos"`
	}
	if json.Unmarshal(data, &config) != nil || len(config.Repos) == 0 {
		return 4
	}
	listener, err := net.Listen("unix", os.Getenv("GORTEX_DAEMON_SOCKET"))
	if err != nil {
		return 5
	}
	defer listener.Close()
	var shutdown sync.Once
	for {
		conn, err := listener.Accept()
		if err != nil {
			return 0
		}
		go func() {
			defer conn.Close()
			decoder := json.NewDecoder(conn)
			encoder := json.NewEncoder(conn)
			var handshake map[string]any
			if decoder.Decode(&handshake) != nil {
				return
			}
			if encoder.Encode(map[string]any{"ok": true, "daemon_version": "fixture-v1"}) != nil {
				return
			}
			for {
				var frame stubFrame
				if decoder.Decode(&frame) != nil {
					return
				}
				if frame.Kind != "" {
					var result any
					switch frame.Kind {
					case "probe":
						var repos []any
						var roots []string
						for _, repo := range config.Repos {
							repos = append(repos, map[string]any{"path": repo.Path, "prefix": repo.Name, "state": "ready"})
							roots = append(roots, repo.Path)
						}
						result = map[string]any{"ready": true, "enriched": true, "tracked_repos": repos, "loaded_roots": roots}
					case "status":
						result = map[string]any{"ready": true, "enriched": true, "views": map[string]any{}, "storage": map[string]any{"wal_pending_frames": 0}}
					case "shutdown":
						result = map[string]any{"stopping": true}
					default:
						result = map[string]any{}
					}
					if encoder.Encode(map[string]any{"ok": true, "result": result}) != nil {
						return
					}
					if frame.Kind == "shutdown" {
						shutdown.Do(func() { _ = listener.Close() })
						return
					}
					continue
				}
				if frame.ID == 0 {
					continue
				}
				var result any = map[string]any{}
				switch frame.Method {
				case "resources/read":
					payload := `{"total_nodes":20,"total_edges":19,"runtime":{"heap_alloc_bytes":1024}}`
					if frame.Params["uri"] == "gortex://communities" {
						payload = `{"communities":[{"id":"fixture"}],"returned":1,"total":1}`
					}
					result = map[string]any{"contents": []any{map[string]any{"type": "text", "text": payload}}}
				case "tools/call":
					if frame.Params["name"] == "analyze" {
						result = stubContent(map[string]any{"hotspots": []any{}, "total": 0})
					} else {
						result = stubContent(map[string]any{"id": "sample/sample.go::Example", "source": "func Example() {}"})
					}
				}
				if encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": result}) != nil {
					return
				}
			}
		}()
	}
}

func TestPrivateDaemonLifecycleSelfTest(t *testing.T) {
	t.Setenv("LOAD_DAEMON_FIXTURE", "serve")
	corpus := gitFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	options := runOptions{Binary: binary, Profile: "agent", Host: "native", Agents: 1, Editors: 1, Duration: time.Second, Deadline: 2 * time.Second, WarmTimeout: 10 * time.Second, StopTimeout: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	phase, _, artifacts, crashes, version, err := runPhase(ctx, options, "cold", []Corpus{corpus}, "", t.TempDir())
	if err != nil {
		t.Fatalf("lifecycle self-test: %v %+v", err, phase.Errors)
	}
	if version != "fixture-v1" || phase.Gate["sample.analysis_publication"] == nil || phase.Gate["sample.source_preflight"] == nil || len(phase.Status) != 2 || len(phase.Stats) != 2 {
		t.Fatalf("missing public gates/receipts: %s %+v", version, phase)
	}
	for _, metric := range []string{"cold_queryable_observed_upper_bound_seconds", "cold_enrichment_observed_upper_bound_seconds", "stop_seconds", "force_killed"} {
		if phase.Metrics[metric].Value == nil {
			t.Fatalf("missing %s", metric)
		}
	}
	if *phase.Metrics["force_killed"].Value != 0 || crashes["panic_lines"] != 0 {
		t.Fatalf("unclean fixture stop: %v %v", phase.Metrics, crashes)
	}
	if _, err := os.Stat(filepath.Join(artifacts["private_root"], "repos")); !os.IsNotExist(err) {
		t.Fatalf("completed private clones retained: %v", err)
	}
	if _, err := os.Stat(artifacts["daemon-1.stderr.log"]); err != nil {
		t.Fatalf("stderr evidence missing: %v", err)
	}
	original, err := os.ReadFile(filepath.Join(corpus.Source, corpus.File))
	if err != nil || !strings.Contains(string(original), "func Example()") {
		t.Fatalf("source changed: %s %v", original, err)
	}
}

func TestFailedPrivateDaemonStillCollectsCrashEvidence(t *testing.T) {
	t.Setenv("LOAD_DAEMON_FIXTURE", "crash")
	corpus := gitFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	options := runOptions{Binary: binary, Profile: "agent", Host: "native", Agents: 1, Duration: time.Second, Deadline: time.Second, WarmTimeout: 5 * time.Second, StopTimeout: 3 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	phase, _, artifacts, crashes, _, err := runPhase(ctx, options, "cold", []Corpus{corpus}, "", t.TempDir())
	if err == nil || len(phase.Errors) == 0 || crashes["panic_lines"] != 1 {
		t.Fatalf("crash was not retained: %v %v %+v", err, crashes, phase.Errors)
	}
	if _, err := os.Stat(artifacts["daemon-1.stderr.log"]); err != nil {
		t.Fatal(err)
	}
}
