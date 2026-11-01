package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type gaugeFixtureSampler map[string]Metric

func (s gaugeFixtureSampler) sample(int) map[string]Metric { return s }

func TestHeapProfilesWithoutFootprintDoNotAdvertisePeak(t *testing.T) {
	for _, cell := range []struct {
		name      string
		available bool
		fail      bool
		keepPrior bool
	}{{"unavailable", false, false, false}, {"measured", true, false, false}, {"capture_failure", true, true, false}, {"later_capture_failure", true, true, true}} {
		t.Run(cell.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/debug/pprof/heap" {
					t.Errorf("unexpected profile endpoint: %s", r.URL)
				}
				if count := requests.Add(1); cell.fail && (!cell.keepPrior || count > 2) {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte("retained-heap-profile"))
			}))
			defer server.Close()
			status, _ := json.Marshal(map[string]any{"pprof_addr": strings.TrimPrefix(server.URL, "http://")})
			metrics := absentProcessMetrics("fixture has no process collector")
			if cell.available {
				metrics["rss_bytes"] = measured(4096, "bytes")
			}
			series := &gaugeSeries{client: &socketClient{}, sampler: gaugeFixtureSampler(metrics)}
			window := series.watchLoad(t.Context(), status, filepath.Join(t.TempDir(), "load"))
			if cell.keepPrior {
				window.observe(metrics)
				metrics["rss_bytes"] = measured(8192, "bytes")
			}
			phase := &Phase{Gate: make(map[string]json.RawMessage)}
			artifacts := map[string]string{}
			err := series.endLoadProfiles(window, phase, artifacts)
			if cell.fail {
				if err == nil || len(phase.Gate["heap_profile_load_peak_unavailable"]) != 0 {
					t.Fatalf("profile capture failure became unavailable telemetry: %v %s", err, phase.Gate)
				}
				peak, advertised := artifacts["heap_profile_load_peak"]
				if cell.keepPrior {
					if data, err := os.ReadFile(peak); !advertised || err != nil || string(data) != "retained-heap-profile" {
						t.Fatalf("later capture failure lost the actual earlier peak: %q %v", data, err)
					}
				} else if advertised {
					t.Fatalf("failed peak capture advertised a nonexistent artifact: %s", peak)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(artifacts["heap_profile_load_start"]); err != nil || string(data) != "retained-heap-profile" {
				t.Fatalf("load-start profile lost: %q %v", data, err)
			}
			peak, advertised := artifacts["heap_profile_load_peak"]
			if !cell.available {
				if advertised {
					t.Fatalf("unmeasured peak advertised as an artifact: %s", peak)
				}
				if _, err := os.Stat(window.peak); !os.IsNotExist(err) {
					t.Fatalf("unavailable footprint produced a peak file: %v", err)
				}
				var reason string
				if err := json.Unmarshal(phase.Gate["heap_profile_load_peak_unavailable"], &reason); err != nil || reason == "" {
					t.Fatalf("missing peak has no explicit unavailable reason: %q %v", reason, err)
				}
			} else if data, err := os.ReadFile(peak); !advertised || err != nil || string(data) != "retained-heap-profile" {
				t.Fatalf("measured peak profile lost: %q %v", data, err)
			}
		})
	}
}

func TestWorktreeStepAndGaugeOptions(t *testing.T) {
	base := []string{"--binary", "unused", "--manifest", "unused", "--scratch", t.TempDir()}
	options, err := parseRun(append(base, "--scenario", "worktrees", "--worktrees", "4", "--worktree-steps", "0,1,2,4"))
	if err != nil || !slices.Equal(options.WorktreeSteps, []int{0, 1, 2, 4}) {
		t.Fatalf("step mode: %+v %v", options.WorktreeSteps, err)
	}
	if options, err := parseRun(append(base, "--gauges")); err != nil || !options.Gauges || options.HeapProfiles {
		t.Fatalf("gauges: %+v %v", options, err)
	}
	if options, err := parseRun(append(base, "--gauges", "--heap-profiles")); err != nil || !options.Gauges || !options.HeapProfiles {
		t.Fatalf("gauges with heap profiles: %+v %v", options, err)
	}
	for _, extra := range [][]string{
		{"--heap-profiles"},
		{"--scenario", "read", "--worktree-steps", "0,1"},
		{"--scenario", "worktrees", "--worktrees", "4", "--worktree-steps", "0,2,1,4"},
		{"--scenario", "worktrees", "--worktrees", "4", "--worktree-steps", "0,1,1,4"},
		{"--scenario", "worktrees", "--worktrees", "4", "--worktree-steps", "0,1,2"},
		{"--scenario", "worktrees", "--worktrees", "2", "--worktree-steps", "-1,2"},
		{"--scenario", "worktrees", "--worktrees", "2", "--worktree-steps", "0,x"},
		{"--scenario", "worktrees", "--worktrees", "4", "--worktree-steps", "0,4", "--cpu-profile", "10s", "--duration", "20s"},
	} {
		if _, err := parseRun(append(base, extra...)); err == nil {
			t.Fatalf("invalid memory options accepted: %v", extra)
		}
	}
	// A coupled comparison measures latency between builds; memory evidence
	// is taken by standalone runs.
	paired := []string{"--build", "tag=/private/tag", "--build", "main=/private/main", "--manifest", "/private/manifest.json", "--scratch", t.TempDir()}
	for _, extra := range [][]string{{"--gauges"}, {"--scenario", "worktrees", "--worktrees", "2", "--worktree-steps", "0,2"}} {
		if _, _, err := parsePaired(append(paired, extra...)); err == nil {
			t.Fatalf("paired accepted memory diagnosis options: %v", extra)
		}
	}
}

// The gauge series pairs every daemon_health push with an external process
// sample and lands in the report. Heap profiles from a load window's start
// and sampled peak are taken only when asked for, so a series-only run adds
// nothing but the health ticks to the load it measures. Step mode grows the
// linked worktrees on one daemon, loaded then idle at each count, and
// profiles every load and idle window.
//
// The agent profile thinks for at most 5 s before its first burst, so a 5.5 s
// window always carries workload calls. Saturation would leave calls in
// flight at every window's end, whose advisory cancellations can time out on
// a loaded test host and invalidate the shared stream. An idle window needs
// no calls, so the step idle windows last 300 ms.
func TestMemoryGaugesRecordSeriesAndHeapProfiles(t *testing.T) {
	t.Setenv("LOAD_DAEMON_FIXTURE", "serve")
	interval, idle := gaugeInterval, stepIdleOverride
	gaugeInterval, stepIdleOverride = 100*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { gaugeInterval, stepIdleOverride = interval, idle })
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const window = 5500 * time.Millisecond
	type healthRow struct {
		Coordinators int `json:"coordinators"`
	}
	readSeries := func(t *testing.T, phase Phase) []gaugeRow {
		t.Helper()
		var rows []gaugeRow
		if err := json.Unmarshal(phase.Gate["gauge_series"], &rows); err != nil || len(rows) < 2 {
			t.Fatalf("gauge series not retained: %s %v", phase.Gate["gauge_series"], err)
		}
		for _, row := range rows {
			if row.Received.IsZero() || len(row.Health) == 0 || row.Process["rss_bytes"].Unit != "bytes" {
				t.Fatalf("incomplete gauge row: %+v", row)
			}
		}
		return rows
	}
	// Load-window profiles are taken as of the daemon's last collection; an
	// idle-window profile forces one first.
	readProfile := func(t *testing.T, path, gc string) {
		t.Helper()
		if data, err := os.ReadFile(path); err != nil || string(data) != "fixture-heap-profile gc="+gc {
			t.Fatalf("heap profile %q not retained: %q %v", path, data, err)
		}
	}
	readPeak := func(t *testing.T, path, reason string, sampled, advertised bool) {
		t.Helper()
		if sampled {
			if reason != "" || !advertised {
				t.Fatalf("measured peak marked unavailable: %q %v", reason, advertised)
			}
			readProfile(t, path, "")
		} else if reason == "" || path != "" || advertised {
			t.Fatalf("unavailable peak advertised without a reason: %q %q %v", path, reason, advertised)
		}
	}

	t.Run("gauges", func(t *testing.T) {
		for _, profiles := range []bool{false, true} {
			options := runOptions{Binary: binary, Profile: "agent", Host: "native", Scenario: "read", Agents: 1, Gauges: true, HeapProfiles: profiles, Duration: window, Deadline: 2 * time.Second, WarmTimeout: 10 * time.Second, StopTimeout: 5 * time.Second}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			phase, _, artifacts, _, _, err := runPhase(ctx, options, "read", []Corpus{gitFixture(t)}, "", t.TempDir())
			cancel()
			if err != nil {
				t.Fatalf("gauged read phase (heap profiles %v): %v %+v", profiles, err, phase.Errors)
			}
			// The series starts once the daemon is ready, ahead of the warmup
			// gates, so it covers the level after enrichment before any load.
			rows := readSeries(t, phase)
			if err := gaugeWarmupOrderError(rows, phase.Attempts); err != nil {
				t.Fatal(err)
			}
			if profiles {
				readProfile(t, artifacts["heap_profile_load_start"], "")
				sampled := false
				for _, row := range rows {
					if _, ok := footprintOf(row.Process); ok {
						sampled = true
					}
				}
				var reason string
				if raw := phase.Gate["heap_profile_load_peak_unavailable"]; len(raw) != 0 {
					if err := json.Unmarshal(raw, &reason); err != nil {
						t.Fatal(err)
					}
				}
				peak, advertised := artifacts["heap_profile_load_peak"]
				readPeak(t, peak, reason, sampled, advertised)
				continue
			}
			taken, _ := filepath.Glob(filepath.Join(artifacts["private_root"], "*.pprof*"))
			if _, ok := artifacts["heap_profile_load_peak"]; ok || len(taken) > 0 {
				t.Fatalf("series-only run took heap profiles: %v %v", taken, artifacts)
			}
		}
	})

	t.Run("worktree steps", func(t *testing.T) {
		options := runOptions{Binary: binary, Profile: "agent", Host: "native", Scenario: "worktrees", Agents: 2, Worktrees: 2, WorktreeSteps: []int{0, 2}, Duration: window, Deadline: 2 * time.Second, WarmTimeout: 10 * time.Second, StopTimeout: 5 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		phase, _, artifacts, _, _, err := runPhase(ctx, options, "worktrees", []Corpus{gitFixture(t)}, "", t.TempDir())
		if err != nil {
			t.Fatalf("worktree steps: %v %+v", err, phase.Errors)
		}
		if phase.SessionModel != "per_agent_per_worktree_step:0,2" || len(phase.Tools) == 0 || phase.DurationSec <= 0 {
			t.Fatalf("step phase identity/workload: %q %d %v", phase.SessionModel, len(phase.Tools), phase.DurationSec)
		}
		var steps []worktreeStep
		if err := json.Unmarshal(phase.Gate["worktree_steps"], &steps); err != nil || len(steps) != len(options.WorktreeSteps) {
			t.Fatalf("steps not retained: %s %v", phase.Gate["worktree_steps"], err)
		}
		for i, step := range steps {
			if step.LinkedWorktrees != options.WorktreeSteps[i] || !step.LoadStarted.Before(step.LoadEnded) || step.IdleStarted.Before(step.LoadEnded) || step.IdleEnded.Sub(step.IdleStarted) < stepIdleOverride || len(step.Tools) == 0 {
				t.Fatalf("step %d: %+v", i, step)
			}
			if step.Load["peak_rss_bytes"].Unit != "bytes" {
				t.Fatalf("step %d has no load-window process peaks: %+v", i, step.Load)
			}
			readProfile(t, step.HeapLoadStart, "")
			_, sampled := footprintOf(map[string]Metric{"rss_bytes": step.Load["peak_rss_bytes"], "pss_bytes": step.Load["peak_pss_bytes"], "phys_footprint_bytes": step.Load["peak_phys_footprint_bytes"]})
			_, advertised := artifacts[stepArtifact(i, "heap_profile_load_peak")]
			readPeak(t, step.HeapLoadPeak, step.HeapLoadPeakUnavailable, sampled, advertised)
			readProfile(t, step.HeapIdle, "1")
			if artifacts[stepArtifact(i, "heap_profile_load_peak")] != step.HeapLoadPeak || artifacts[stepArtifact(i, "heap_profile_idle")] != step.HeapIdle {
				t.Fatalf("step %d profile is not a run artifact: %v", i, artifacts)
			}
		}
		// The series spans the steps: the daemon's coordinator count is seen
		// to grow with the linked worktrees.
		seen := make(map[int]bool)
		for _, row := range readSeries(t, phase) {
			var health healthRow
			if err := json.Unmarshal(row.Health, &health); err != nil {
				t.Fatal(err)
			}
			seen[health.Coordinators] = true
		}
		if !seen[0] || !seen[2] {
			t.Fatalf("series did not follow the steps: coordinators seen %v", seen)
		}
	})
}

// A step run is identified by the counts it walks: per-coordinator costs from
// 0,1,2,4 and from 0,2,4 are different measurements, so delta gives no verdict
// between them.
func TestDeltaRefusesStepRunsOverOtherWorktreeCounts(t *testing.T) {
	a, b := sampleReport(t), sampleReport(t)
	a.Phases[0].SessionModel = worktreeStepSessionModel([]int{0, 1, 2, 4})
	b.Phases[0].SessionModel = worktreeStepSessionModel([]int{0, 2, 4})
	if err := comparable(a, b); err == nil {
		t.Fatalf("step runs over different counts compared: %q vs %q", a.Phases[0].SessionModel, b.Phases[0].SessionModel)
	}
	b.Phases[0].SessionModel = worktreeStepSessionModel([]int{0, 1, 2, 4})
	if err := comparable(a, b); err != nil {
		t.Fatalf("step runs over the same counts: %v", err)
	}
}
