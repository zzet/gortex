//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var readbackProjectionEvidence = flag.Bool("readback-reference-projection-evidence", false, "opt in to owned-daemon lifetime RSS and index-health bookends outside offered probes; repin affected cells")

type projectionProcessEvidence struct {
	PID        int    `json:"pid"`
	BirthTicks string `json:"birth_ticks"`
	BootID     string `json:"boot_id"`
	StatBefore string `json:"stat_before"`
	StatAfter  string `json:"stat_after"`
	Status     string `json:"status"`
	Peak       Metric `json:"lifetime_rss_peak_bytes"`
	Scope      string `json:"scope"`
}

type projectionRuntimeBookend struct {
	Version    int                       `json:"version"`
	Phase      string                    `json:"phase"`
	ObservedAt time.Time                 `json:"observed_at"`
	Process    projectionProcessEvidence `json:"process"`
	Counter    Metric                    `json:"projection_divergence"`
	RPC        readbackReferenceRPC      `json:"index_health_rpc"`
	Reasons    []string                  `json:"unavailable_reasons"`
	Connection string                    `json:"connection_identity"`
}

func projectionBirth(stat string, pid int) (string, error) {
	open, close := strings.IndexByte(stat, '('), strings.LastIndexByte(stat, ')')
	if open <= 0 || close < open || strings.TrimSpace(stat[:open]) != strconv.Itoa(pid) {
		return "", errors.New("proc stat PID/comm is unavailable")
	}
	fields := strings.Fields(stat[close+1:])
	if len(fields) <= 19 {
		return "", errors.New("proc stat starttime field is missing")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || start == 0 {
		return "", errors.New("proc stat starttime is invalid")
	}
	return fields[19], nil
}

func projectionHWM(status string) (float64, error) {
	var value uint64
	found := false
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "VmHWM:" {
			continue
		}
		if found || len(fields) != 3 || fields[2] != "kB" {
			return 0, errors.New("kernel VmHWM requires one kB field")
		}
		var err error
		value, err = strconv.ParseUint(fields[1], 10, 64)
		if err != nil || value > (1<<53-1)/1024 {
			return 0, errors.New("kernel VmHWM bytes cannot be represented exactly")
		}
		found = true
	}
	if !found {
		return 0, errors.New("kernel VmHWM is missing, not zero")
	}
	return float64(value * 1024), nil
}

func projectionProcessSnapshot(pid int, read func(string) ([]byte, error)) projectionProcessEvidence {
	out := projectionProcessEvidence{PID: pid, Scope: "owned_daemon_pid_only_since_process_start", Peak: unavailable("bytes", "owned Linux process identity unavailable")}
	if pid <= 0 {
		return out
	}
	base := filepath.Join("/proc", strconv.Itoa(pid))
	before, e1 := read(filepath.Join(base, "stat"))
	status, e2 := read(filepath.Join(base, "status"))
	after, e3 := read(filepath.Join(base, "stat"))
	boot, e4 := read("/proc/sys/kernel/random/boot_id")
	out.StatBefore, out.StatAfter, out.Status = string(before), string(after), string(status)
	out.BootID = strings.TrimSpace(string(boot))
	birth, b1 := projectionBirth(out.StatBefore, pid)
	endBirth, b2 := projectionBirth(out.StatAfter, pid)
	value, hwm := projectionHWM(out.Status)
	if errors.Join(e1, e2, e3, e4, b1, b2, hwm) != nil || birth != endBirth || out.BootID == "" {
		out.Peak = unavailable("bytes", "missing/reused PID, birth, boot ID or VmHWM")
		return out
	}
	out.BirthTicks, out.Peak = birth, measured(value, "bytes")
	return out
}

func projectionCounter(raw json.RawMessage) Metric {
	payload, err := textPayload(raw, "contents")
	if err != nil {
		return unavailable("count", "index-health resource payload unavailable")
	}
	var body struct {
		Pending json.RawMessage `json:"pending_projection"`
	}
	if json.Unmarshal(payload, &body) != nil || len(body.Pending) == 0 || string(body.Pending) == "null" {
		return unavailable("count", "pending_projection section absent; never inferred zero")
	}
	var section map[string]json.RawMessage
	if json.Unmarshal(body.Pending, &section) != nil {
		return unavailable("count", "pending_projection section malformed")
	}
	var count json.Number
	value := section["projection_divergence"]
	if len(value) == 0 || value[0] < '0' || value[0] > '9' || json.Unmarshal(value, &count) != nil {
		return unavailable("count", "projection_divergence absent or malformed")
	}
	n, err := count.Int64()
	if err != nil || n < 0 || n > 1<<53-1 {
		return unavailable("count", "projection_divergence is not an exact nonnegative integer")
	}
	return measured(float64(n), "count")
}

func collectProjectionBookend(ctx context.Context, caller readbackReferenceCaller, pid int, phase string, prior *projectionRuntimeBookend) projectionRuntimeBookend {
	out := projectionRuntimeBookend{Version: 1, Phase: phase, Connection: "fixed-editor-0", ObservedAt: time.Now(), Counter: unavailable("count", "bookend not observed"), Reasons: []string{}}
	if runtime.GOOS == "linux" {
		out.Process = projectionProcessSnapshot(pid, os.ReadFile)
	} else {
		out.Process = projectionProcessEvidence{PID: pid, Scope: "owned_daemon_pid_only_since_process_start", Peak: unavailable("bytes", "Linux VmHWM required")}
	}
	// This is the existing resource URI, on the same producer connection.
	// There is exactly one attempt, outside ACK -> immediate read bursts.
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, id, err := caller.callWithIdentity(callCtx, "resources/read", map[string]any{"uri": "gortex://index-health"})
	out.RPC = readbackReferenceRPCObservation(raw, id, err)
	out.RPC.Request, _ = json.Marshal(map[string]any{"uri": "gortex://index-health"})
	if err == nil && id.ID != 0 && !id.Started.IsZero() && !id.WriteStarted.IsZero() && !id.WriteFinished.IsZero() && !id.ReplyDelivered.IsZero() && !id.Finished.Before(id.Started) {
		out.Counter = projectionCounter(raw)
	}
	if runtime.GOOS == "linux" {
		stat, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		birth, birthErr := projectionBirth(string(stat), pid)
		if readErr != nil || birthErr != nil || birth != out.Process.BirthTicks {
			out.Counter = unavailable("count", "producer identity changed during index-health RPC")
			out.Process.Peak = unavailable("bytes", "producer identity changed during index-health RPC")
		}
	}
	if prior != nil && (out.Process.PID != prior.Process.PID || out.Process.BirthTicks == "" || out.Process.BirthTicks != prior.Process.BirthTicks || out.Process.BootID != prior.Process.BootID) {
		out.Counter = unavailable("count", "producer PID/birth epoch changed or unavailable")
		out.Process.Peak = unavailable("bytes", "producer PID/birth epoch changed or unavailable")
	}
	if out.Process.BirthTicks == "" {
		out.Counter = unavailable("count", "owned producer PID/birth proof unavailable")
	}
	if out.Process.Peak.Value == nil {
		out.Reasons = append(out.Reasons, out.Process.Peak.Reason)
	}
	if out.Counter.Value == nil {
		out.Reasons = append(out.Reasons, out.Counter.Reason)
	}
	return out
}

func retainProjectionBookend(report *readbackReferenceCaseReport, value projectionRuntimeBookend) {
	raw, err := json.Marshal(value)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("projection evidence encoding: %v", err))
		return
	}
	if report.Readiness == nil {
		report.Readiness = make(map[string]json.RawMessage)
	}
	report.Readiness["projection_evidence."+value.Phase] = raw
}
