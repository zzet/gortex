package main

import (
	"fmt"
	"strings"
	"time"
)

// graphSurfaces answer from the repository graph. Their first success after
// the primary registers is how long an agent waits for a working tool.
var graphSurfaces = []string{"search_symbols", "index_health_resource", "cli_pipe", "cli_tty"}

var probeRoles = []string{"primary", "worktree", "router"}

type firstOKTiming struct {
	FromStartMS Metric `json:"from_start_ms"`
	FromReadyMS Metric `json:"from_ready_ms"`
}

type bootSummary struct {
	bootTiming
	TimeToReadyMS Metric                   `json:"time_to_ready_ms"`
	FirstOK       map[string]firstOKTiming `json:"first_ok"`
}

// startupSummary is the startup probe's gate receipt. Answers are keyed
// "surface/cwd" and split into windows by what the readiness watcher saw:
// no_socket before the socket answered, loading until the primary was ready,
// and ready after.
type startupSummary struct {
	PrimaryRepo     string        `json:"primary_repo"`
	LinkedWorktrees int           `json:"linked_worktrees"`
	WatchIntervalMS float64       `json:"watch_interval_ms"`
	CLITTY          string        `json:"cli_tty"`
	Boots           []bootSummary `json:"boots"`
	// Outcomes counts each answer as ok, ok:<status>, an error code (with
	// repo_loading(incomplete) for a loading refusal missing retryable=true,
	// phase or percent), an initialize instructions variant, or a hook
	// decision.
	Outcomes              map[string]map[string]map[string]int `json:"outcomes"`
	TrackAdvice           map[string]map[string]int            `json:"track_advice"`
	RestartReadyToFirstOK map[string]Distribution              `json:"restart_ready_to_first_ok_ms"`
	BetweenBoots          int                                  `json:"samples_between_boots"`
}

// firstOK is when the earliest successful sample for surface and cwd that
// started in [from, until) completed; a zero until is open. Measured from a
// boot's start it is time to first OK; measured from readiness it is ready
// to first OK, for any warm or cold time-to-ready comparison.
func firstOK(samples []probeSample, surface, cwd string, from, until time.Time) (time.Time, bool) {
	var first time.Time
	for _, s := range samples {
		if !s.OK || s.Surface != surface || s.CWD != cwd || s.Started.Before(from) || (!until.IsZero() && !s.Started.Before(until)) {
			continue
		}
		if done := s.completed(); first.IsZero() || done.Before(first) {
			first = done
		}
	}
	return first, !first.IsZero()
}

// bootIndex places a sample in the boot it observed. The probe starts just
// before the first daemon does, so earlier samples belong to the first boot;
// samples between a stop request and the next start belong to none.
func bootIndex(boots []bootTiming, at time.Time) int {
	for i, boot := range boots {
		if (i == 0 || !at.Before(boot.Started)) && (boot.Stopping.IsZero() || at.Before(boot.Stopping)) {
			return i
		}
	}
	return -1
}

func startupWindow(boot bootTiming, primary string, at time.Time) string {
	if boot.SocketAt.IsZero() || at.Before(boot.SocketAt) {
		return "no_socket"
	}
	if ready, ok := boot.ReadyAt[primary]; !ok || at.Before(ready) {
		return "loading"
	}
	return "ready"
}

func outcomeKey(s probeSample) string {
	switch {
	case s.Variant != "":
		return s.Variant
	case s.OK && s.Status != "":
		return "ok:" + s.Status
	case s.OK:
		return "ok"
	case s.Code == "repo_loading" && !strings.HasPrefix(s.Surface, "cli_") && (s.Retryable == nil || !*s.Retryable || s.Phase == "" || s.Percent == nil):
		return "repo_loading(incomplete)"
	default:
		return s.Code
	}
}

func summarizeStartup(samples []probeSample, boots []bootTiming, primary string, linked int) startupSummary {
	sum := startupSummary{PrimaryRepo: primary, LinkedWorktrees: linked, WatchIntervalMS: durationMS(startupWatchInterval), Outcomes: make(map[string]map[string]map[string]int), TrackAdvice: make(map[string]map[string]int), RestartReadyToFirstOK: make(map[string]Distribution)}
	for _, s := range samples {
		index := bootIndex(boots, s.Started)
		if index < 0 {
			sum.BetweenBoots++
			continue
		}
		key, window := s.Surface+"/"+s.CWD, startupWindow(boots[index], primary, s.Started)
		if sum.Outcomes[key] == nil {
			sum.Outcomes[key] = make(map[string]map[string]int)
		}
		if sum.Outcomes[key][window] == nil {
			sum.Outcomes[key][window] = make(map[string]int)
		}
		sum.Outcomes[key][window][outcomeKey(s)]++
		if s.TrackAdvice {
			if sum.TrackAdvice[key] == nil {
				sum.TrackAdvice[key] = make(map[string]int)
			}
			sum.TrackAdvice[key][window]++
		}
	}
	restarts := make(map[string][]float64)
	for i, boot := range boots {
		row := bootSummary{bootTiming: boot, TimeToReadyMS: unavailable("ms", "primary repository never ready"), FirstOK: make(map[string]firstOKTiming)}
		ready, registered := boot.ReadyAt[primary]
		if registered {
			row.TimeToReadyMS = measured(durationMS(ready.Sub(boot.Started)), "ms")
		}
		for _, surface := range graphSurfaces {
			for _, cwd := range probeRoles {
				first, ok := firstOK(samples, surface, cwd, boot.Started, boot.Stopping)
				if !ok {
					continue
				}
				key := surface + "/" + cwd
				timing := firstOKTiming{FromStartMS: measured(durationMS(first.Sub(boot.Started)), "ms"), FromReadyMS: unavailable("ms", "primary repository never ready")}
				if registered {
					// The watcher stamps readiness up to one poll late, so a
					// success can precede it; that wait is zero.
					fromReady := max(0, durationMS(first.Sub(ready)))
					timing.FromReadyMS = measured(fromReady, "ms")
					if i > 0 {
						restarts[key] = append(restarts[key], fromReady)
					}
				}
				row.FirstOK[key] = timing
			}
		}
		sum.Boots = append(sum.Boots, row)
	}
	for key, values := range restarts {
		sum.RestartReadyToFirstOK[key] = distribution(values, "ms")
	}
	return sum
}

// metrics are the headline counters. Counts cover every boot; restart
// figures cover boots after the first and are unavailable unless every
// restart produced the observation.
func (sum startupSummary) metrics() map[string]Metric {
	count := func(match func(surface, window, outcome string) bool) Metric {
		total := 0
		for key, windows := range sum.Outcomes {
			surface, _, _ := strings.Cut(key, "/")
			for window, outcomes := range windows {
				for outcome, n := range outcomes {
					if match(surface, window, outcome) {
						total += n
					}
				}
			}
		}
		return measured(float64(total), "count")
	}
	advice := func(window string) Metric {
		total := 0
		for _, windows := range sum.TrackAdvice {
			total += windows[window]
		}
		return measured(float64(total), "count")
	}
	m := map[string]Metric{
		"startup_boots":            measured(float64(len(sum.Boots)), "count"),
		"startup_linked_worktrees": measured(float64(sum.LinkedWorktrees), "count"),
		// The untracked initialize variant is the same answer at handshake time.
		"startup_repo_not_tracked_before_ready": count(func(_, window, outcome string) bool {
			return window != "ready" && (outcome == "repo_not_tracked" || outcome == "untracked")
		}),
		"startup_nonconforming_loading_refusals": count(func(surface, window, outcome string) bool {
			return window == "loading" && surface != "hook_read" && !strings.HasPrefix(surface, "initialize") && outcome != "repo_loading" && !strings.HasPrefix(outcome, "ok")
		}),
		"startup_hook_denies_while_loading": count(func(surface, window, outcome string) bool {
			return surface == "hook_read" && window != "ready" && outcome == "deny"
		}),
		"startup_hook_denies_after_ready": count(func(surface, window, outcome string) bool {
			return surface == "hook_read" && window == "ready" && outcome == "deny"
		}),
		"startup_track_advice_while_loading":  advice("loading"),
		"startup_track_advice_without_socket": advice("no_socket"),
		"startup_initial_time_to_ready_ms":    unavailable("ms", "no boot"),
		"startup_initial_first_ok_ms":         unavailable("ms", "no successful search_symbols from the primary"),
	}
	if len(sum.Boots) > 0 {
		m["startup_initial_time_to_ready_ms"] = sum.Boots[0].TimeToReadyMS
		if first, ok := sum.Boots[0].FirstOK["search_symbols/primary"]; ok {
			m["startup_initial_first_ok_ms"] = first.FromStartMS
		}
	}
	restarts := max(0, len(sum.Boots)-1)
	var toReady []float64
	for _, boot := range sum.Boots[min(1, len(sum.Boots)):] {
		if boot.TimeToReadyMS.Value != nil {
			toReady = append(toReady, *boot.TimeToReadyMS.Value)
		}
	}
	m["startup_restart_time_to_ready_p95_ms"] = restartP95(distribution(toReady, "ms"), restarts, "reached ready")
	m["startup_restart_ready_to_first_ok_p95_ms"] = restartP95(sum.RestartReadyToFirstOK["search_symbols/primary"], restarts, "had a successful search_symbols from the primary")
	// The census is per boot; with no restart the initial boot is all there is.
	census := sum.Boots[min(1, len(sum.Boots)):]
	if len(sum.Boots) == 1 {
		census = sum.Boots
	}
	m["startup_coordinators_within_2s_min"] = unavailable("count", "no boot")
	for i, boot := range census {
		if boot.Coordinators == nil {
			m["startup_coordinators_within_2s_min"] = unavailable("count", fmt.Sprintf("boot %d has no coordinator census", boot.Boot))
			break
		}
		if value := float64(*boot.Coordinators); i == 0 || value < *m["startup_coordinators_within_2s_min"].Value {
			m["startup_coordinators_within_2s_min"] = measured(value, "count")
		}
	}
	return m
}

func restartP95(d Distribution, restarts int, observation string) Metric {
	if restarts == 0 {
		return unavailable("ms", "no restarts")
	}
	if d.Count < restarts {
		return unavailable("ms", fmt.Sprintf("%d of %d restarts %s", d.Count, restarts, observation))
	}
	return d.P95
}

func durationMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
